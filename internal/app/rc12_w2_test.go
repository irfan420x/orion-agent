// rc12 W2 regression tests for Audit 7 finding F-A7-2: (a) late usage
// from an executor that ignores its context was discarded, and (b)
// the sweep's FinishedAt window could permanently lose a recorded
// task that finished mid-sweep.
package app

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/irfan/orion/internal/budget"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/subagent"
)

// TestSweepChargesLateUsageFromIgnoringExecutor is the end-to-end
// port of Audit 7's probe (F-A7-2a): an executor that ignores ctx,
// works 300 ms past a 20 ms deadline and returns 700 tokens must
// have those 700 tokens charged — by a later turn, once the usage
// has settled. A turn that runs BEFORE the late usage arrives
// charges only its own tokens and must NOT mark the task swept
// (charging zero and marking it would lose the 700 forever); the
// next turn charges them, exactly once.
func TestSweepChargesLateUsageFromIgnoringExecutor(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(100000)
	mgr := subagent.NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, subagent.Usage, error) {
		// Ignores ctx entirely: works 300 ms past the deadline.
		time.Sleep(320 * time.Millisecond)
		return "late", subagent.Usage{Tokens: 700}, nil
	})
	a.Subagents = mgr
	prov := &usageScriptProvider{chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		return finalUsageResponse("done", model.Usage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10}), nil
	}}
	a.Provider = prov

	id, err := mgr.SpawnForChat(111, "slow work", 20*time.Millisecond)
	if err != nil {
		t.Fatalf("SpawnForChat: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if st, _ := mgr.Status(id); st == subagent.StatusTimeout {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s never timed out", id)
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Turn 1 runs while the executor is still working past the
	// deadline: the task's usage is unsettled, so the sweep skips
	// it WITHOUT marking it swept, and the turn charges only its
	// own 10 tokens.
	if _, stats, err := a.HandleStreamForChat(context.Background(), 111, "anything?", nil); err != nil {
		t.Fatalf("turn 1: %v", err)
	} else if stats == nil || stats.Tokens != 10 || stats.SubagentTokens != 0 {
		t.Fatalf("turn 1 stats = %+v, want {Tokens:10 SubagentTokens:0} (unsettled late usage must not be charged as zero)", stats)
	}
	cs, err := a.chatSessionFor(111)
	if err != nil {
		t.Fatalf("chatSessionFor: %v", err)
	}
	a.chatsMu.Lock()
	marked := cs.sweptSubagents[id]
	a.chatsMu.Unlock()
	if marked {
		t.Fatalf("timed-out task %s was marked swept by turn 1 while its usage was still unsettled (the late 700 tokens would be lost)", id)
	}

	// The executor returns ~300 ms after the deadline; wait for the
	// Manager to record the late usage.
	deadline = time.Now().Add(5 * time.Second)
	for {
		if res, ok := mgr.GetResult(id); ok && res.UsageSettled && res.Usage.Tokens == 700 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("late usage for task %s never settled (want 700 tokens)", id)
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Turn 2 charges the late usage, exactly once.
	if _, stats, err := a.HandleStreamForChat(context.Background(), 111, "anything else?", nil); err != nil {
		t.Fatalf("turn 2: %v", err)
	} else if stats == nil || stats.Tokens != 710 || stats.SubagentTokens != 700 {
		t.Errorf("turn 2 stats = %+v, want {Tokens:710 SubagentTokens:700} (10 main + 700 late child)", stats)
	}
	if got := a.Budget.Usage(111); got != 720 {
		t.Errorf("Budget.Usage(111) = %d, want 720 (10 turn 1 + 710 turn 2)", got)
	}
	// Turn 3 charges nothing more.
	if _, stats, err := a.HandleStreamForChat(context.Background(), 111, "again?", nil); err != nil {
		t.Fatalf("turn 3: %v", err)
	} else if stats == nil || stats.SubagentTokens != 0 || stats.Tokens != 10 {
		t.Errorf("turn 3 stats = %+v, want {Tokens:10 SubagentTokens:0}", stats)
	}
	if got := a.Budget.Usage(111); got != 730 {
		t.Errorf("Budget.Usage(111) = %d, want 730 (720 + 10 turn 3)", got)
	}
}

// TestSweepChargesTaskFinishingDuringSweep is the port of Audit 7's
// probe TestAudit7SweepWindowLosesMidSweepFinisher, INVERTED into a
// positive test: a task that finishes mid-sweep — after the sweep's
// loop passed it but before the sweep finished — must still be
// charged once by a later sweep. Under the old FinishedAt window
// gate the second sweep charged 0 for it, forever.
//
// Constructing a mid-sweep finish needs a controlled interleaving:
// the sweep's loop is microseconds long, so the test stretches it
// with 50 instant filler tasks (whose marking the loop must work
// through) and blocks the sweep mid-loop on chatsMu while the
// blocked task finishes. Whether the interleaving landed is
// VERIFIED (the sweep was still running when the task finished,
// and the task was left unmarked) — an attempt that missed the
// window retries instead of asserting on a state it did not build,
// so the test fails on the old code (every constructed attempt
// charges 0 on the second sweep) and passes on the new code.
func TestSweepChargesTaskFinishingDuringSweep(t *testing.T) {
	const maxAttempts = 30
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if tryMidSweepFinish(t, attempt) {
			return
		}
	}
	t.Fatalf("could not construct a mid-sweep finish in %d attempts; the test cannot verify the fix", maxAttempts)
}

// tryMidSweepFinish runs one attempt to build the F-A7-2(b) state.
// It reports true when the state was constructed (and the
// assertions on it ran — via t.Errorf on failure, so a real
// failure is reported, not retried away), and false when the
// interleaving missed and the attempt should be retried. The app
// is closed explicitly on every path: a.Close runs a session-close
// sweep, so it must not run before the attempt's own sweeps.
func tryMidSweepFinish(t *testing.T, attempt int) bool {
	t.Helper()
	a := testApp(t)
	release := make(chan struct{})
	// Wide concurrency so all 50 fillers can be spawned without
	// hitting the at-capacity error while earlier ones drain.
	mgr := subagent.NewManagerWithExecutor(256, func(ctx context.Context, brief string) (string, subagent.Usage, error) {
		if brief == "blocked" {
			<-release
			return "blocked done", subagent.Usage{Tokens: 250}, nil
		}
		return "done", subagent.Usage{Tokens: 1}, nil
	})
	a.Subagents = mgr

	blockedID, err := mgr.SpawnForChat(111, "blocked", time.Minute)
	if err != nil {
		t.Fatalf("SpawnForChat blocked: %v", err)
	}
	// A gap in StartedAt so the blocked task is unambiguously
	// first in the sweep's List order.
	time.Sleep(2 * time.Millisecond)
	// 50 fillers: enough to stretch the sweep's loop (each
	// iteration takes a chatsMu round-trip) while staying well
	// under the Manager's maxEntries=100 registry cap — past
	// 100 finished entries the Manager evicts the oldest, so a
	// larger filler count could never all be visible to the sweep.
	const fillers = 50
	for i := 0; i < fillers; i++ {
		if _, err := mgr.SpawnForChat(111, "filler", time.Minute); err != nil {
			t.Fatalf("SpawnForChat filler %d: %v", i, err)
		}
	}
	// Wait for every filler to finish (the blocked task stays
	// running); the sweep's loop then has 200 settled tasks to
	// mark after it passes the blocked one.
	deadline := time.Now().Add(5 * time.Second)
	for {
		done := 0
		for _, ts := range mgr.List() {
			if ts.Status == subagent.StatusDone {
				done++
			}
		}
		if done == fillers {
			break
		}
		if time.Now().After(deadline) {
			_ = a.Close()
			t.Fatalf("attempt %d: fillers did not finish: %d/%d done", attempt, done, fillers)
		}
		time.Sleep(2 * time.Millisecond)
	}
	cs, err := a.chatSessionFor(111)
	if err != nil {
		_ = a.Close()
		t.Fatalf("chatSessionFor: %v", err)
	}

	// Hold chatsMu so the sweep blocks at its first lock, start
	// it, let it wake, yield so it enters its loop, then re-lock
	// to block it mid-loop — after it passed the blocked task and
	// while it is marking fillers.
	a.chatsMu.Lock()
	sweepDone := make(chan int, 1)
	go func() { sweepDone <- a.sweepFinishedSubagents(cs, 111) }()
	time.Sleep(20 * time.Millisecond)
	a.chatsMu.Unlock()
	runtime.Gosched()
	runtime.Gosched()
	a.chatsMu.Lock()
	close(release)
	// The blocked task finishes while the sweep is blocked
	// mid-loop. (Its completion path uses only the Manager's
	// mutex, so holding chatsMu here cannot deadlock it.)
	deadline = time.Now().Add(5 * time.Second)
	for {
		if st, _ := mgr.Status(blockedID); st == subagent.StatusDone {
			break
		}
		if time.Now().After(deadline) {
			a.chatsMu.Unlock()
			_ = a.Close()
			t.Fatalf("attempt %d: blocked task did not finish", attempt)
		}
		time.Sleep(1 * time.Millisecond)
	}
	// Verify the interleaving landed: the sweep must still have
	// been running when the blocked task finished. If it had
	// already returned, the task finished AFTER the sweep, which
	// is not the bug state — retry.
	select {
	case <-sweepDone:
		a.chatsMu.Unlock()
		_ = a.Close()
		return false
	default:
	}
	a.chatsMu.Unlock()
	swept1 := <-sweepDone
	// The sweep charged all fillers. The blocked task must have
	// been left unmarked: if the sweep charged it, it finished
	// before the loop reached it, and the state was not
	// constructed — retry. (rc13: under -race + full-suite load
	// the sweep goroutine is not always scheduled inside the
	// window, so it starts after the blocked task finished and
	// correctly charges it: swept1 = fillers + 250. That is a
	// missed interleaving, not a lost token — the task WAS
	// charged — so it retries like the sweep-already-returned
	// miss above, instead of asserting on a state it did not
	// build. Any other total is unexpected and stays a hard
	// failure.)
	if swept1 == fillers+250 {
		_ = a.Close()
		return false
	}
	if swept1 != fillers {
		_ = a.Close()
		t.Errorf("attempt %d: first sweep = %d, want %d (all fillers)", attempt, swept1, fillers)
		return true
	}
	a.chatsMu.Lock()
	marked := cs.sweptSubagents[blockedID]
	a.chatsMu.Unlock()
	if marked {
		_ = a.Close()
		return false
	}
	// Constructed: the task finished mid-sweep and was left
	// uncharged. A second sweep must charge exactly its 250
	// tokens, and a third must charge nothing.
	if got := a.sweepFinishedSubagents(cs, 111); got != 250 {
		t.Errorf("attempt %d: second sweep after a mid-sweep finish = %d, want 250 (F-A7-2(b): the task must be charged, not lost)", attempt, got)
	}
	if got := a.sweepFinishedSubagents(cs, 111); got != 0 {
		t.Errorf("attempt %d: third sweep = %d, want 0 (already charged)", attempt, got)
	}
	_ = a.Close()
	return true // state was constructed; report, do not retry
}

// TestSweepChargesTasksFinishingDuringSweepStress hammers the
// F-A7-2(b) fix concurrently: 60 tasks finish staggered over
// ~180 ms while a sweeper runs every millisecond. Every finished
// task must be charged exactly once in total — no loss (the old
// window dropped tasks that finished mid-sweep) and no
// double-charge (the swept set is the exactly-once guard under
// concurrent sweeps).
func TestSweepChargesTasksFinishingDuringSweepStress(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	var started atomic.Int64
	mgr := subagent.NewManagerWithExecutor(64, func(ctx context.Context, brief string) (string, subagent.Usage, error) {
		i := started.Add(1) - 1
		time.Sleep(time.Duration(i*3) * time.Millisecond)
		return "done", subagent.Usage{Tokens: 250}, nil
	})
	a.Subagents = mgr
	const tasks = 60
	for i := 0; i < tasks; i++ {
		if _, err := mgr.SpawnForChat(111, "work", time.Minute); err != nil {
			t.Fatalf("SpawnForChat %d: %v", i, err)
		}
	}
	cs, err := a.chatSessionFor(111)
	if err != nil {
		t.Fatalf("chatSessionFor: %v", err)
	}
	var total atomic.Int64
	stop := make(chan struct{})
	sweeperDone := make(chan struct{})
	go func() {
		defer close(sweeperDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			total.Add(int64(a.sweepFinishedSubagents(cs, 111)))
			time.Sleep(time.Millisecond)
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		done := 0
		for _, ts := range mgr.List() {
			if ts.Status == subagent.StatusDone {
				done++
			}
		}
		if done == tasks {
			break
		}
		if time.Now().After(deadline) {
			close(stop)
			<-sweeperDone
			t.Fatalf("tasks did not finish: %d/%d done", done, tasks)
		}
		time.Sleep(2 * time.Millisecond)
	}
	close(stop)
	<-sweeperDone
	// Final sweep catches anything the sweeper had not reached.
	total.Add(int64(a.sweepFinishedSubagents(cs, 111)))
	if got := total.Load(); got != tasks*250 {
		t.Errorf("total swept = %d, want %d (every finished task charged exactly once; a shortfall is a lost task, an excess is a double-charge)", got, tasks*250)
	}
	if got := a.sweepFinishedSubagents(cs, 111); got != 0 {
		t.Errorf("final sweep = %d, want 0 (already charged)", got)
	}
}
