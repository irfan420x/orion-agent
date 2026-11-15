// F-C2 tests: the scheduler must not serialise due jobs across
// chats, a hung scheduled turn must end at its deadline as a
// failure, overlapping ticks must never double-fire a job, turns
// in one chat must still serialise, and the missed-run check
// must use the time a job is actually processed.
package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// cronBlockingRunner is a test runner whose calls block until the
// test releases them, per chat. It records every call in entry
// order and the maximum number of overlapping calls per chat, so
// tests can prove cross-chat parallelism and same-chat
// serialisation without real time.
type cronBlockingRunner struct {
	mu sync.Mutex
	// release holds one channel per blocked chat; closing it
	// lets that chat's calls proceed. closed guards double close.
	release map[int64]chan struct{}
	closed  map[int64]bool
	// respectCtx, when true, makes a blocked call watch its
	// context and return ctx.Err() when it fires; when false
	// the call ignores its context entirely (a hung runner).
	respectCtx bool
	// hold, when set, is slept inside every call while the
	// per-chat concurrency counter is up (overlap detection).
	hold time.Duration
	err  error

	calls []CronJob
	cur   map[int64]int // calls currently inside, per chat
	max   map[int64]int // peak overlap per chat
}

func newCronBlockingRunner() *cronBlockingRunner {
	return &cronBlockingRunner{
		release: map[int64]chan struct{}{},
		closed:  map[int64]bool{},
		cur:     map[int64]int{},
		max:     map[int64]int{},
	}
}

// blockChat makes calls in chatID wait until releaseChat is
// called. It must be called before the run starts.
func (r *cronBlockingRunner) blockChat(chatID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.release[chatID]; !ok {
		r.release[chatID] = make(chan struct{})
	}
}

func (r *cronBlockingRunner) releaseChat(chatID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed[chatID] {
		return
	}
	r.closed[chatID] = true
	if ch, ok := r.release[chatID]; ok {
		close(ch)
	}
}

func (r *cronBlockingRunner) run(ctx context.Context, job CronJob) (string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, job)
	r.cur[job.ChatID]++
	if r.cur[job.ChatID] > r.max[job.ChatID] {
		r.max[job.ChatID] = r.cur[job.ChatID]
	}
	ch := r.release[job.ChatID]
	hold := r.hold
	r.mu.Unlock()
	if hold > 0 {
		time.Sleep(hold)
	}
	if ch != nil {
		if r.respectCtx {
			select {
			case <-ch:
			case <-ctx.Done():
				r.finish(job.ChatID)
				return "", ctx.Err()
			}
		} else {
			<-ch
		}
	}
	r.finish(job.ChatID)
	return "ran: " + job.Prompt, r.err
}

func (r *cronBlockingRunner) finish(chatID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cur[chatID]--
}

func (r *cronBlockingRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *cronBlockingRunner) callsFor(chatID int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if c.ChatID == chatID {
			n++
		}
	}
	return n
}

func (r *cronBlockingRunner) maxOverlap(chatID int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.max[chatID]
}

// waitForRunnerCalls waits until the runner has recorded n calls
// in chatID, or fails the test. It polls: a call is recorded on
// entry, before it blocks.
func waitForRunnerCalls(t *testing.T, r *cronBlockingRunner, chatID int64, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for r.callsFor(chatID) < n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d runner call(s) in chat %d (got %d)", n, chatID, r.callsFor(chatID))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForRunnerTotal waits until the runner has recorded n calls
// in total, or fails the test.
func waitForRunnerTotal(t *testing.T, r *cronBlockingRunner, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for r.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d runner call(s) in total (got %d)", n, r.count())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func waitForJobResult(t *testing.T, list *cronListTool, ctx context.Context, id, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := mustCronList(t, list, ctx)
		for _, j := range got.Jobs {
			if j.ID == id && j.LastResult == want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for job %s to record %q: %+v", id, want, got.Jobs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCronSchedulerSlowChatDoesNotBlockOtherChat(t *testing.T) {
	store := NewPlanningStore("")
	sched, list, _ := cronTestTools(store)
	ctxA := cronChatCtx(111, 111)
	ctxB := cronChatCtx(222, 222)
	now := time.Now().UTC().Truncate(time.Second)
	cronDueJob(t, store, ctxA, sched, "*/5 * * * *", "slow A", now.Add(-time.Minute))
	cronDueJob(t, store, ctxB, sched, "*/5 * * * *", "quick B", now.Add(-time.Minute))

	runner := newCronBlockingRunner()
	runner.blockChat(111)
	s := NewCronScheduler(store)
	s.SetRunner(runner.run)

	done := make(chan error, 1)
	go func() { done <- s.ProcessDue(ctxA, now) }()
	// Wait until A's turn is inside the runner (blocked).
	waitForRunnerCalls(t, runner, 111, 1)
	// B's job must complete while A is still blocked: its
	// outcome is applied by B's own worker, not after A.
	got := mustCronList(t, list, ctxB)
	idB := got.Jobs[0].ID
	waitForJobResult(t, list, ctxB, idB, "ok")
	if runner.callsFor(222) != 1 {
		runner.releaseChat(111)
		t.Fatalf("B runner calls = %d, want 1", runner.callsFor(222))
	}
	runner.releaseChat(111)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ProcessDue: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ProcessDue did not return after releasing A")
	}
	if runner.count() != 2 {
		t.Fatalf("runner calls = %d, want 2 (one per chat)", runner.count())
	}
}

func TestCronSchedulerHungTurnTimesOut(t *testing.T) {
	runCase := func(t *testing.T, respectCtx bool) {
		t.Helper()
		store := NewPlanningStore("")
		sched, list, _ := cronTestTools(store)
		ctx := cronChatCtx(111, 111)
		now := time.Now().UTC().Truncate(time.Second)
		cronDueJob(t, store, ctx, sched, "*/5 * * * *", "hung job", now.Add(-time.Minute))

		runner := newCronBlockingRunner()
		runner.respectCtx = respectCtx
		runner.blockChat(111)
		s := NewCronScheduler(store)
		s.SetRunner(runner.run)
		s.SetTurnTimeout(200 * time.Millisecond)

		start := time.Now()
		if err := s.ProcessDue(ctx, now); err != nil {
			t.Fatalf("ProcessDue: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("ProcessDue took %s with a hung runner (deadline 200ms): the scheduler waited for the hung turn", elapsed)
		}
		// The hung turn counts as a failure (backoff), not a
		// silent drop and not a re-fire.
		got := mustCronList(t, list, ctx)
		j := got.Jobs[0]
		if j.ConsecutiveFailures != 1 || j.Disabled {
			t.Fatalf("job after timeout = %+v, want 1 failure / enabled", j)
		}
		if !strings.Contains(j.LastResult, "timed out") {
			t.Fatalf("last result = %q, want a timeout failure", j.LastResult)
		}
		if runner.count() != 1 {
			t.Fatalf("runner calls = %d, want 1", runner.count())
		}
		// The backed-off next run must be after the timeout
		// instant (tick time + 200ms deadline), not merely
		// after the tick: a backoff anchored at the time the
		// job was classified would already be stale by the
		// time the turn timed out.
		next, err := time.Parse(time.RFC3339, j.NextRun)
		if err != nil {
			t.Fatalf("next run %q: %v", j.NextRun, err)
		}
		if !next.After(now.Add(200 * time.Millisecond)) {
			t.Fatalf("next run %s is not after the timeout instant %s (backoff not anchored at the turn's outcome)", j.NextRun, now.Add(200*time.Millisecond).Format(time.RFC3339))
		}
		// Re-tick with ADVANCED time — a real next tick, 30s
		// after the first one, while the hung runner is still
		// blocked: the timed-out job must NOT re-fire, its
		// backed-off next run is in the future. (The old
		// test re-ticked with the SAME time, which masked a
		// stale backoff: with the production 10-minute
		// deadline the next real tick lands ~10 minutes after
		// classification, long past a backoff anchored there.)
		if err := s.ProcessDue(ctx, now.Add(30*time.Second)); err != nil {
			t.Fatalf("second ProcessDue: %v", err)
		}
		if runner.count() != 1 {
			t.Fatalf("runner calls after second tick = %d, want 1 (no re-fire at the advanced tick)", runner.count())
		}
		// Releasing the hung runner must not apply a second
		// outcome: its late result is discarded.
		runner.releaseChat(111)
		time.Sleep(100 * time.Millisecond) // let the orphan exit
		got = mustCronList(t, list, ctx)
		if got.Jobs[0].ConsecutiveFailures != 1 {
			t.Fatalf("failures after release = %d, want 1 (late result discarded)", got.Jobs[0].ConsecutiveFailures)
		}
	}
	t.Run("ignores-context", func(t *testing.T) { runCase(t, false) })
	t.Run("respects-context", func(t *testing.T) { runCase(t, true) })
}

func TestCronSchedulerOverlappingTicksNeverDoubleFire(t *testing.T) {
	store := NewPlanningStore("")
	sched, _, _ := cronTestTools(store)
	ctxA := cronChatCtx(111, 111)
	ctxB := cronChatCtx(222, 222)
	now := time.Now().UTC().Truncate(time.Second)
	cronDueJob(t, store, ctxA, sched, "*/5 * * * *", "slow A", now.Add(-time.Minute))
	cronDueJob(t, store, ctxB, sched, "*/5 * * * *", "quick B", now.Add(-time.Minute))

	runner := newCronBlockingRunner()
	runner.blockChat(111)
	s := NewCronScheduler(store)
	s.SetRunner(runner.run)

	firstDone := make(chan error, 1)
	go func() { firstDone <- s.ProcessDue(ctxA, now) }()
	// Wait until A's turn is inside the runner (blocked).
	waitForRunnerCalls(t, runner, 111, 1)
	// A second, overlapping tick must return promptly, must not
	// run A's job a second time, and must not re-run B (whose
	// next run already advanced).
	secondDone := make(chan error, 1)
	go func() { secondDone <- s.ProcessDue(ctxA, now) }()
	select {
	case err := <-secondDone:
		if err != nil {
			runner.releaseChat(111)
			t.Fatalf("overlapping tick error = %v, want nil (skip)", err)
		}
	case <-time.After(5 * time.Second):
		runner.releaseChat(111)
		t.Fatal("overlapping tick did not return within 5s")
	}
	if runner.callsFor(111) != 1 || runner.callsFor(222) != 1 {
		runner.releaseChat(111)
		t.Fatalf("runner calls: A=%d B=%d, want 1 each (no double-fire)", runner.callsFor(111), runner.callsFor(222))
	}
	runner.releaseChat(111)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first tick error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first tick did not return after releasing A")
	}
	if runner.count() != 2 {
		t.Fatalf("runner calls = %d, want 2", runner.count())
	}
}

func TestCronSchedulerSameChatTurnsSerialise(t *testing.T) {
	store := NewPlanningStore("")
	sched, list, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	now := time.Now().UTC().Truncate(time.Second)
	id1 := cronDueJob(t, store, ctx, sched, "*/5 * * * *", "first job", now.Add(-time.Minute))
	id2 := cronDueJob(t, store, ctx, sched, "*/5 * * * *", "second job", now.Add(-time.Minute))

	// Each call holds the per-chat concurrency counter up for
	// 50ms: if the scheduler ever ran two turns for this chat
	// at once, the peak overlap would be 2.
	runner := newCronBlockingRunner()
	runner.hold = 50 * time.Millisecond
	s := NewCronScheduler(store)
	s.SetRunner(runner.run)
	if err := s.ProcessDue(ctx, now); err != nil {
		t.Fatalf("ProcessDue: %v", err)
	}
	if runner.count() != 2 {
		t.Fatalf("runner calls = %d, want 2", runner.count())
	}
	if runner.maxOverlap(111) != 1 {
		t.Fatalf("max overlap in chat 111 = %d, want 1 (turns must serialise)", runner.maxOverlap(111))
	}
	// Creation order is preserved: the first job runs first.
	if runner.calls[0].ID != id1 || runner.calls[1].ID != id2 {
		t.Fatalf("runner call order = %q, %q, want %q then %q", runner.calls[0].ID, runner.calls[1].ID, id1, id2)
	}
	got := mustCronList(t, list, ctx)
	for _, j := range got.Jobs {
		if j.LastResult != "ok" || j.ConsecutiveFailures != 0 {
			t.Fatalf("job after run = %+v, want ok/0 failures", j)
		}
	}
}

func TestCronSchedulerMissedCheckUsesProcessingTime(t *testing.T) {
	store := NewPlanningStore("")
	sched, list, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	now := time.Now().UTC().Truncate(time.Second)
	// Job 1: due 1 minute ago; its turn blocks ~3s.
	id1 := cronDueJob(t, store, ctx, sched, "*/5 * * * *", "slow first job", now.Add(-time.Minute))
	// Job 2: due 9m59s ago — inside the 10-minute window at
	// tick time, but past it by the time its turn would start:
	// same chat, so it waits behind job 1's turn. Under the
	// old tick-start check it would have run; judged at its
	// real processing time (tick+3s = 10m02s late) it must be
	// skipped.
	id2 := cronDueJob(t, store, ctx, sched, "*/5 * * * *", "queued job", now.Add(-9*time.Minute-59*time.Second))

	runner := newCronBlockingRunner()
	runner.blockChat(111)
	// Release job 1's turn after 3s.
	time.AfterFunc(3*time.Second, func() { runner.releaseChat(111) })
	s := NewCronScheduler(store)
	s.SetRunner(runner.run)
	if err := s.ProcessDue(ctx, now); err != nil {
		t.Fatalf("ProcessDue: %v", err)
	}
	if runner.count() != 1 || runner.calls[0].ID != id1 {
		t.Fatalf("runner calls = %+v, want only the first job (the queued job must be skipped, not run late)", runner.calls)
	}
	got := mustCronList(t, list, ctx)
	for _, j := range got.Jobs {
		switch j.ID {
		case id1:
			if j.LastResult != "ok" || j.ConsecutiveFailures != 0 {
				t.Fatalf("first job = %+v, want ok/0 failures", j)
			}
		case id2:
			if !strings.Contains(j.LastResult, "skipped") || j.ConsecutiveFailures != 0 || j.Disabled {
				t.Fatalf("queued job = %+v, want skipped/0 failures/enabled", j)
			}
		}
	}
}

func TestCronSchedulerWorkerPoolBoundsConcurrency(t *testing.T) {
	store := NewPlanningStore("")
	sched, _, _ := cronTestTools(store)
	ctxA := cronChatCtx(111, 111)
	ctxB := cronChatCtx(222, 222)
	now := time.Now().UTC().Truncate(time.Second)
	cronDueJob(t, store, ctxA, sched, "*/5 * * * *", "A job", now.Add(-time.Minute))
	cronDueJob(t, store, ctxB, sched, "*/5 * * * *", "B job", now.Add(-time.Minute))

	runner := newCronBlockingRunner()
	runner.blockChat(111)
	runner.blockChat(222)
	s := NewCronScheduler(store)
	s.SetRunner(runner.run)
	s.SetMaxWorkers(1)

	done := make(chan error, 1)
	go func() { done <- s.ProcessDue(ctxA, now) }()
	// One chat's turn enters the runner and blocks, holding
	// the only worker.
	waitForRunnerTotal(t, runner, 1)
	// Give the other chat a moment: with a pool of 1 its turn
	// must not start while the worker is held.
	time.Sleep(300 * time.Millisecond)
	if runner.count() != 1 {
		runner.releaseChat(111)
		runner.releaseChat(222)
		t.Fatalf("runner calls = %d while the only worker is held, want 1 (pool bound not enforced)", runner.count())
	}
	// Release whichever chat holds the worker; the other chat's
	// turn must start only now.
	runner.releaseChat(111)
	runner.releaseChat(222)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ProcessDue: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ProcessDue did not return after releasing both chats")
	}
	if runner.count() != 2 {
		t.Fatalf("runner calls = %d, want 2 (both chats ran, one after the other)", runner.count())
	}
}

func TestCronSchedulerLongFailingTurnBackoffAnchoredAtCompletion(t *testing.T) {
	// A turn that runs long (3s) and THEN fails: the failure
	// backoff must be anchored at the turn's outcome time, not
	// at the time the job was classified. With a classification
	// anchor the next run would be at+1min; with the outcome
	// anchor it is (at+3s)+1min. The 3s turn stands in for the
	// production 10-minute timeout, where the stale anchor
	// puts the next run ~9 minutes in the past and the job
	// re-fires on the next tick. `now` is pinned to a minute
	// with minute%5==4 (+30s) so the next `*/5` slot is
	// exactly now+30s and the arithmetic below has no boundary
	// ambiguity — at any other minute the next `*/5` slot is
	// up to now+4m30s, which swamps the 1-minute backoff and
	// the test would pass with the stale anchor too.
	store := NewPlanningStore("")
	sched, list, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	base := time.Now().UTC().Truncate(time.Minute)
	for base.Minute()%5 != 4 {
		base = base.Add(time.Minute)
	}
	now := base.Add(30 * time.Second)
	cronDueJob(t, store, ctx, sched, "*/5 * * * *", "slow then fails", now.Add(-time.Minute))

	var calls atomic.Int32
	runner := func(ctx context.Context, job CronJob) (string, error) {
		calls.Add(1)
		time.Sleep(3 * time.Second)
		return "", errors.New("provider exploded")
	}
	s := NewCronScheduler(store)
	s.SetRunner(runner)
	if err := s.ProcessDue(ctx, now); err != nil {
		t.Fatalf("ProcessDue: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("runner calls = %d, want 1", calls.Load())
	}
	got := mustCronList(t, list, ctx)
	j := got.Jobs[0]
	if j.ConsecutiveFailures != 1 || j.Disabled {
		t.Fatalf("job after long failure = %+v, want 1 failure / enabled", j)
	}
	next, err := time.Parse(time.RFC3339, j.NextRun)
	if err != nil {
		t.Fatalf("next run %q: %v", j.NextRun, err)
	}
	// Outcome-anchored: next run is at least 1 minute after
	// the turn ENDED (~now+63s). Classification-anchored it
	// would be ~now+60s. The 61s threshold separates the two
	// with a 2s margin on the fixed side and a 1s margin on
	// the stale side (the stored value is second precision).
	if !next.After(now.Add(61 * time.Second)) {
		t.Fatalf("next run %s is not after now+61s (want the 1m backoff anchored at the turn's outcome, ~now+63s)", j.NextRun)
	}
	// A real next tick at now+61s — past the stale next run,
	// before the outcome-anchored one — must NOT re-fire the
	// job: with the stale anchor the job would run again here,
	// 1s after its (already past) next run.
	if err := s.ProcessDue(ctx, now.Add(61*time.Second)); err != nil {
		t.Fatalf("second ProcessDue: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("runner calls after second tick = %d, want 1 (no re-fire: the backoff had not expired)", calls.Load())
	}
	got = mustCronList(t, list, ctx)
	if got.Jobs[0].ConsecutiveFailures != 1 {
		t.Fatalf("failures after second tick = %d, want 1", got.Jobs[0].ConsecutiveFailures)
	}
}

func TestCronSchedulerLongSuccessfulTurnKeepsSchedule(t *testing.T) {
	// The outcome anchoring applies to the FAILURE backoff
	// only. A long turn that SUCCEEDS keeps its schedule-based
	// next run (the next cron slot after it started): the
	// missed-window policy deliberately lets a run that is up
	// to 10 minutes late still fire, so a successful turn's
	// next run is the next slot, not completion+anything. This
	// pins the boundary so a future "fix" does not move
	// successful turns onto the outcome clock and skip slots.
	// `now` is mid-minute so the next slot is exactly now+30s.
	store := NewPlanningStore("")
	sched, list, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	now := time.Now().UTC().Truncate(time.Minute).Add(30 * time.Second)
	cronDueJob(t, store, ctx, sched, "*/5 * * * *", "slow then succeeds", now.Add(-time.Minute))

	s := NewCronScheduler(store)
	s.SetRunner(func(ctx context.Context, job CronJob) (string, error) {
		time.Sleep(3 * time.Second)
		return "ok", nil
	})
	if err := s.ProcessDue(ctx, now); err != nil {
		t.Fatalf("ProcessDue: %v", err)
	}
	got := mustCronList(t, list, ctx)
	j := got.Jobs[0]
	if j.LastResult != "ok" || j.ConsecutiveFailures != 0 {
		t.Fatalf("job after long success = %+v, want ok/0 failures", j)
	}
	next, err := time.Parse(time.RFC3339, j.NextRun)
	if err != nil {
		t.Fatalf("next run %q: %v", j.NextRun, err)
	}
	schedExpr, err := parseCronExpression("*/5 * * * *")
	if err != nil {
		t.Fatalf("parseCronExpression: %v", err)
	}
	wantNext, err := schedExpr.nextAfter(now)
	if err != nil {
		t.Fatalf("nextAfter: %v", err)
	}
	if !next.Equal(wantNext) {
		t.Fatalf("next run %s, want the next cron slot %s (success keeps the schedule, it is not outcome-anchored)", j.NextRun, wantNext.Format(time.RFC3339))
	}
}

func TestCronSchedulerWorkerPoolSharedAcrossOverlappingTicks(t *testing.T) {
	// The worker pool is scheduler-level, not per tick: two
	// overlapping ProcessDue calls share one pool of
	// maxWorkers, so the bound holds across ticks. With a
	// per-call pool the second tick would bring its own pool
	// of 1 and run its chat's turn concurrently with the
	// first tick's blocked turn (2 turns at once, bound 1).
	store := NewPlanningStore("")
	sched, _, _ := cronTestTools(store)
	ctxA := cronChatCtx(111, 111)
	ctxB := cronChatCtx(222, 222)
	now := time.Now().UTC().Truncate(time.Second)
	cronDueJob(t, store, ctxA, sched, "*/5 * * * *", "A job", now.Add(-time.Minute))

	runner := newCronBlockingRunner()
	runner.blockChat(111)
	runner.blockChat(222)
	s := NewCronScheduler(store)
	s.SetRunner(runner.run)
	s.SetMaxWorkers(1)

	firstDone := make(chan error, 1)
	go func() { firstDone <- s.ProcessDue(ctxA, now) }()
	// A's turn enters the runner and blocks, holding the only
	// worker.
	waitForRunnerCalls(t, runner, 111, 1)
	// A second chat's job appears only now, so the first tick
	// never saw it: the overlapping second tick has an
	// unclaimed chat with a due job.
	cronDueJob(t, store, ctxB, sched, "*/5 * * * *", "B job", now.Add(-time.Minute))
	secondDone := make(chan error, 1)
	go func() { secondDone <- s.ProcessDue(ctxA, now) }()
	// Give the second tick a moment: B's turn must not start
	// while A holds the only (shared) worker.
	time.Sleep(300 * time.Millisecond)
	if runner.count() != 1 {
		runner.releaseChat(111)
		runner.releaseChat(222)
		t.Fatalf("runner calls = %d while the only worker is held, want 1 (second tick ran B on its own pool)", runner.count())
	}
	runner.releaseChat(111)
	runner.releaseChat(222)
	for _, done := range []chan error{firstDone, secondDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("ProcessDue: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("ProcessDue did not return after releasing both chats")
		}
	}
	if runner.count() != 2 {
		t.Fatalf("runner calls = %d, want 2 (both chats ran, one after the other)", runner.count())
	}
}

func TestCronSchedulerDeadlineRaceUsesResult(t *testing.T) {
	// Best-effort race test: a turn that returns success well
	// before its deadline must be recorded as a success, not
	// a timeout. The scheduler's select can take the Done
	// branch at the exact instant the result lands; the fix
	// checks the result channel first in that branch. The race
	// itself cannot be forced deterministically from outside —
	// landing the result in the microsecond window where both
	// the result and the deadline are ready is a matter of
	// scheduling, and a sleep that ends only just before the
	// deadline is itself flaky on a loaded machine (a 90ms
	// sleep against a 100ms deadline failed here for that
	// reason, the runner simply being scheduled late) — so
	// the margin is deliberately wide (50ms turn, 200ms
	// deadline). What this pins is the near-deadline success
	// path, repeated: on the fixed code every iteration is a
	// success by construction, either because the result
	// branch wins, or because the Done branch finds the
	// already-sent result.
	for i := 0; i < 5; i++ {
		store := NewPlanningStore("")
		sched, list, _ := cronTestTools(store)
		ctx := cronChatCtx(111, 111)
		now := time.Now().UTC().Truncate(time.Second)
		cronDueJob(t, store, ctx, sched, "*/5 * * * *", "just in time", now.Add(-time.Minute))
		s := NewCronScheduler(store)
		s.SetRunner(func(ctx context.Context, job CronJob) (string, error) {
			time.Sleep(50 * time.Millisecond)
			return "ok", nil
		})
		s.SetTurnTimeout(200 * time.Millisecond)
		if err := s.ProcessDue(ctx, now); err != nil {
			t.Fatalf("iteration %d: ProcessDue: %v", i, err)
		}
		got := mustCronList(t, list, ctx)
		j := got.Jobs[0]
		if j.LastResult != "ok" || j.ConsecutiveFailures != 0 {
			t.Fatalf("iteration %d: job = %+v, want ok/0 failures (a turn that finished before its deadline is a success, not a timeout)", i, j)
		}
	}
}

// TestCronSchedulerCancelWaitsForDispatchedChat pins the dispatch
// contract: when the tick ctx is cancelled mid-dispatch, ProcessDue
// returns only once every dispatched chat's work is done — it must
// wait out a dispatched chat's shutdown outcome-apply instead of
// returning early and abandoning the chat's goroutine. (Found by
// the F-C2 fix-up verifier: the fixer had claimed this was
// "fix-verified-by-inspection only"; it is deterministically
// testable.)
func TestCronSchedulerCancelWaitsForDispatchedChat(t *testing.T) {
	store := NewPlanningStore("")
	sched, _, _ := cronTestTools(store)
	ctx111 := cronChatCtx(111, 111)
	now := time.Now().UTC().Truncate(time.Second)
	// Two due jobs in chat 111: job 1 runs (and cancels the tick
	// ctx from inside the runner), job 2 is never reached — the
	// per-job loop's ctx check then applies job 1's outcome with
	// a context that survives the cancel, which is the slow spot
	// the dispatch loop must wait for.
	cronDueJob(t, store, ctx111, sched, "*/5 * * * *", "first job", now.Add(-time.Minute))
	cronDueJob(t, store, ctx111, sched, "*/5 * * * *", "second job", now.Add(-time.Minute))
	// Many trailing buckets stretch the dispatch loop so the
	// runner's cancel lands while it is still dispatching.
	for id := int64(1000); id < 3000; id++ {
		store.bucket(fmt.Sprintf("chat:%d", id))
	}
	ctx, cancel := context.WithCancel(cronChatCtx(111, 111))
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var once sync.Once
	runner := func(rctx context.Context, job CronJob) (string, error) {
		once.Do(func() { cancel() })
		<-release // block, ignoring ctx; released by t.Cleanup
		return "", nil
	}
	s := NewCronScheduler(store)
	s.SetRunner(runner)
	var calls atomic.Int32
	s.saveJobs = func(st *PlanningStore, dir, key string, doc *cronJobsDoc) error {
		if calls.Add(1) == 2 { // the shutdown outcome apply (claim is call 1)
			time.Sleep(2 * time.Second)
		}
		return saveCronJobs(st, dir, key, doc)
	}
	start := time.Now()
	err := s.ProcessDue(ctx, now)
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ProcessDue err = %v, want context.Canceled", err)
	}
	if elapsed < 1500*time.Millisecond {
		t.Fatalf("ProcessDue returned after %v, want >=1.5s (must wait for the dispatched chat's outcome apply)", elapsed)
	}
}
