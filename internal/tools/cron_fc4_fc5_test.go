// F-C4 + F-C5 tests: a due job's slot must be claimed in the
// store BEFORE its turn runs (a crash/restart mid-turn cannot
// re-fire it), an outcome-save failure must be surfaced and
// counted instead of silently re-firing, cron_cancel must stop a
// running turn, and a deliberate stop must not count as a failure.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"
)

// cronCtxRunner is a test runner that models runScheduledTurn's
// context behaviour: it blocks until its turn context is cancelled
// (StopJob) or the test releases it, and reports a cancellation as
// ErrCronTurnStopped — exactly what the daemon's runner returns
// for a stopped turn.
type cronCtxRunner struct {
	release chan struct{}
	calls   []CronJob
}

func (r *cronCtxRunner) run(ctx context.Context, job CronJob) (string, error) {
	r.calls = append(r.calls, job)
	select {
	case <-ctx.Done():
		return "", ErrCronTurnStopped
	case <-r.release:
		return "ran: " + job.Prompt, nil
	}
}

func (r *cronCtxRunner) count() int { return len(r.calls) }

func waitForCronRunnerCalls(t *testing.T, count func() int, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d runner call(s) (got %d)", n, count())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func cronJobNextRun(t *testing.T, list *cronListTool, ctx context.Context, id string) (nextRun, lastResult string, failures int) {
	t.Helper()
	got := mustCronList(t, list, ctx)
	for _, j := range got.Jobs {
		if j.ID == id {
			return j.NextRun, j.LastResult, j.ConsecutiveFailures
		}
	}
	t.Fatalf("job %s not in list %+v", id, got.Jobs)
	return "", "", 0
}

func TestCronSchedulerCrashMidTurnDoesNotRefire(t *testing.T) {
	dir := t.TempDir()
	ctx := cronChatCtx(111, 111)
	now := time.Now().UTC().Truncate(time.Second)

	// Schedule a due job on disk, and start a tick whose turn
	// blocks inside the runner.
	store1 := NewPlanningStore(dir)
	sched, list, _ := cronTestTools(store1)
	id := cronDueJob(t, store1, ctx, sched, "* * * * *", "crash mid-turn", now.Add(-time.Minute))
	release := make(chan struct{})
	runner := &cronFakeRunner{block: release}
	s := NewCronScheduler(store1)
	s.SetRunner(runner.run)
	done := make(chan error, 1)
	go func() { done <- s.ProcessDue(ctx, now) }()
	waitForCronRunnerCalls(t, runner.count, 1)

	// The claim is already on disk while the turn is still
	// running: a fresh store (a daemon restart) sees the slot
	// consumed.
	store2 := NewPlanningStore(dir)
	_, list2, _ := cronTestTools(store2)
	nextRun, _, _ := cronJobNextRun(t, list2, ctx, id)
	next, err := time.Parse(time.RFC3339, nextRun)
	if err != nil {
		close(release)
		t.Fatalf("claimed next run %q does not parse: %v", nextRun, err)
	}
	if !next.After(now) {
		close(release)
		t.Fatalf("claimed next run %s is not after the tick time %s (claim did not persist before the turn finished)", nextRun, now.Format(time.RFC3339))
	}

	// The restart re-scans at the same tick time, inside the
	// missed window of the original slot: the interrupted slot
	// must not fire a second time.
	runner2 := &cronFakeRunner{}
	s2 := NewCronScheduler(store2)
	s2.SetRunner(runner2.run)
	if err := s2.ProcessDue(ctx, now); err != nil {
		close(release)
		t.Fatalf("ProcessDue after restart: %v", err)
	}
	if runner2.count() != 0 {
		close(release)
		t.Fatalf("runner calls after restart = %d, want 0 (the interrupted slot re-fired: duplicate turn)", runner2.count())
	}

	// Let the interrupted turn finish: exactly one run happened,
	// recorded ok, with the next run advanced.
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("first ProcessDue: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first ProcessDue did not return after releasing the runner")
	}
	if runner.count() != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.count())
	}
	_, lastRes, failures := cronJobNextRun(t, list, ctx, id)
	if lastRes != "ok" || failures != 0 {
		t.Fatalf("job after run: last result %q failures %d, want ok/0", lastRes, failures)
	}
}

func TestCronSchedulerClaimSaveFailureSkipsRun(t *testing.T) {
	store := NewPlanningStore("")
	sched, list, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	now := time.Now().UTC().Truncate(time.Second)
	id := cronDueJob(t, store, ctx, sched, "* * * * *", "needs a claim", now.Add(-time.Minute))
	dueNext := now.Add(-time.Minute).Format(time.RFC3339)

	runner := &cronFakeRunner{}
	s := NewCronScheduler(store)
	s.SetRunner(runner.run)
	var logBuf bytes.Buffer
	s.SetLogger(log.New(&logBuf, "", 0))
	// The first persist is the claim: fail it. The turn must not
	// run without a persisted claim, whatever the disk does.
	calls := 0
	s.saveJobs = func(st *PlanningStore, dir, key string, jobs []*CronJob) error {
		calls++
		if calls == 1 {
			return errors.New("disk full (simulated)")
		}
		return saveCronJobs(st, dir, key, jobs)
	}
	if err := s.ProcessDue(ctx, now); err != nil {
		t.Fatalf("ProcessDue: %v (a claim failure is logged, not returned)", err)
	}
	if runner.count() != 0 {
		t.Fatalf("runner calls = %d, want 0 (no run without a persisted claim)", runner.count())
	}
	nextRun, lastRes, failures := cronJobNextRun(t, list, ctx, id)
	if nextRun != dueNext {
		t.Fatalf("next run = %s, want the still-due %s (the claim must not have advanced)", nextRun, dueNext)
	}
	if lastRes != "" || failures != 0 {
		t.Fatalf("job after skipped claim = %q/%d, want untouched", lastRes, failures)
	}
	if !strings.Contains(logBuf.String(), "claim failed") {
		t.Fatalf("scheduler log = %q, want a loud claim-failed line", logBuf.String())
	}
	// With the store healthy again the same tick fires normally.
	s.saveJobs = nil
	if err := s.ProcessDue(ctx, now); err != nil {
		t.Fatalf("ProcessDue after store recovers: %v", err)
	}
	if runner.count() != 1 {
		t.Fatalf("runner calls after recovery = %d, want 1", runner.count())
	}
}

func TestCronSchedulerOutcomeSaveFailureCountsAsFailure(t *testing.T) {
	store := NewPlanningStore("")
	sched, list, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	now := time.Now().UTC().Truncate(time.Second)
	id := cronDueJob(t, store, ctx, sched, "* * * * *", "runs fine, save fails", now.Add(-time.Minute))

	runner := &cronFakeRunner{}
	s := NewCronScheduler(store)
	s.SetRunner(runner.run)
	var logBuf bytes.Buffer
	s.SetLogger(log.New(&logBuf, "", 0))
	// Persist call 1 is the claim (succeeds); call 2 is the
	// outcome apply (fails); call 3 is the retry-as-failure
	// (succeeds).
	calls := 0
	s.saveJobs = func(st *PlanningStore, dir, key string, jobs []*CronJob) error {
		calls++
		if calls == 2 {
			return errors.New("disk full (simulated)")
		}
		return saveCronJobs(st, dir, key, jobs)
	}
	if err := s.ProcessDue(ctx, now); err != nil {
		t.Fatalf("ProcessDue: %v (the retry saves the failure, so the tick succeeds)", err)
	}
	if runner.count() != 1 {
		t.Fatalf("runner calls = %d, want 1 (the turn ran exactly once)", runner.count())
	}
	if calls != 3 {
		t.Fatalf("persist calls = %d, want 3 (claim, failed apply, retry)", calls)
	}
	nextRun, lastRes, failures := cronJobNextRun(t, list, ctx, id)
	if failures != 1 {
		t.Fatalf("failures = %d, want 1 (the outcome-save failure is counted)", failures)
	}
	if !strings.Contains(lastRes, "outcome save failed") {
		t.Fatalf("last result = %q, want it to name the outcome-save failure", lastRes)
	}
	next, err := time.Parse(time.RFC3339, nextRun)
	if err != nil {
		t.Fatalf("next run %q: %v", nextRun, err)
	}
	if next.Before(now.Add(time.Minute)) {
		t.Fatalf("next run %s is sooner than now+1m (no backoff after the counted failure)", nextRun)
	}
	if !strings.Contains(logBuf.String(), "outcome save failed") {
		t.Fatalf("scheduler log = %q, want a loud outcome-save-failed line", logBuf.String())
	}
	// The same slot does not re-fire: its next run is backed off.
	if err := s.ProcessDue(ctx, now); err != nil {
		t.Fatalf("second ProcessDue: %v", err)
	}
	if runner.count() != 1 {
		t.Fatalf("runner calls after second tick = %d, want 1 (no re-fire)", runner.count())
	}
}

func TestCronSchedulerOutcomeSaveFailureSurfacedWhenRetryFails(t *testing.T) {
	store := NewPlanningStore("")
	sched, list, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	now := time.Now().UTC().Truncate(time.Second)
	id := cronDueJob(t, store, ctx, sched, "* * * * *", "save always fails", now.Add(-time.Minute))

	runner := &cronFakeRunner{}
	s := NewCronScheduler(store)
	s.SetRunner(runner.run)
	var logBuf bytes.Buffer
	s.SetLogger(log.New(&logBuf, "", 0))
	// Only the claim persists; every later save fails (a disk
	// that stays full). The failure cannot be counted on disk —
	// but it must be loud, and the slot must still not re-fire.
	calls := 0
	s.saveJobs = func(st *PlanningStore, dir, key string, jobs []*CronJob) error {
		calls++
		if calls == 1 {
			return saveCronJobs(st, dir, key, jobs)
		}
		return errors.New("disk full (simulated)")
	}
	if err := s.ProcessDue(ctx, now); err != nil {
		t.Fatalf("ProcessDue: %v (a per-chat save failure is logged, not returned)", err)
	}
	if runner.count() != 1 {
		t.Fatalf("runner calls = %d, want 1 (the turn ran exactly once)", runner.count())
	}
	// The claim advanced NextRun, so a second tick at the same
	// time — and 30s later, inside the original slot's missed
	// window — fires nothing: no tight re-fire loop.
	for _, at := range []time.Time{now, now.Add(30 * time.Second)} {
		if err := s.ProcessDue(ctx, at); err != nil {
			t.Fatalf("ProcessDue at %s: %v", at.Format(time.RFC3339), err)
		}
		if runner.count() != 1 {
			t.Fatalf("runner calls after tick at %s = %d, want 1 (the unrecorded slot re-fired)", at.Format(time.RFC3339), runner.count())
		}
	}
	nextRun, _, failures := cronJobNextRun(t, list, ctx, id)
	next, err := time.Parse(time.RFC3339, nextRun)
	if err != nil {
		t.Fatalf("next run %q: %v", nextRun, err)
	}
	if !next.After(now) {
		t.Fatalf("next run %s is not after the tick time (the claim did not survive the save failures)", nextRun)
	}
	if failures != 0 {
		t.Fatalf("failures = %d, want 0 (nothing could be persisted — the loss is honest, not invented)", failures)
	}
	if !strings.Contains(logBuf.String(), "outcome save retry failed") {
		t.Fatalf("scheduler log = %q, want a loud outcome-save-retry-failed line", logBuf.String())
	}
}

func TestCronCancelStopsRunningJob(t *testing.T) {
	store := NewPlanningStore("")
	sched, list, cancel := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	now := time.Now().UTC().Truncate(time.Second)
	id := cronDueJob(t, store, ctx, sched, "* * * * *", "long running job", now.Add(-time.Minute))

	runner := &cronCtxRunner{release: make(chan struct{})}
	s := NewCronScheduler(store)
	s.SetRunner(runner.run)
	// Wire the cancel tool to this scheduler, as the daemon does.
	cancel.stopJob = s.StopJob

	done := make(chan error, 1)
	go func() { done <- s.ProcessDue(ctx, now) }()
	waitForCronRunnerCalls(t, runner.count, 1)

	// Cancel while the turn is running: the tool reports that it
	// stopped the live run, and the turn ends promptly.
	out, err := cancel.Execute(ctx, json.RawMessage(fmt.Sprintf(`{"job_id":%q}`, id)))
	if err != nil {
		close(runner.release)
		t.Fatalf("cron_cancel on a running job: %v", err)
	}
	var cancelled map[string]any
	if err := json.Unmarshal([]byte(out), &cancelled); err != nil {
		close(runner.release)
		t.Fatalf("cancel result is not JSON (%q): %v", out, err)
	}
	if cancelled["cancelled"] != true || cancelled["job_id"] != id {
		close(runner.release)
		t.Fatalf("cancel result = %s, want {cancelled:true, job_id}", out)
	}
	if cancelled["stopped_running"] != true {
		close(runner.release)
		t.Fatalf("cancel result = %s, want stopped_running:true (the live turn was not stopped)", out)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ProcessDue after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		close(runner.release)
		t.Fatal("ProcessDue did not return within 5s of the cancel (the running turn was not stopped)")
	}
	if runner.count() != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.count())
	}
	// The job is gone and its stopped outcome was dropped, not
	// resurrected — and nothing re-fires.
	if got := mustCronList(t, list, ctx); got.Count != 0 {
		t.Fatalf("list after cancel = %d jobs, want 0", got.Count)
	}
	if err := s.ProcessDue(ctx, now); err != nil {
		t.Fatalf("ProcessDue after cancel: %v", err)
	}
	if runner.count() != 1 {
		t.Fatalf("runner calls after a further tick = %d, want 1 (no re-fire)", runner.count())
	}
}

func TestCronSchedulerStoppedTurnDoesNotCountFailure(t *testing.T) {
	runCase := func(t *testing.T, name string, runErr error) {
		t.Helper()
		store := NewPlanningStore("")
		sched, list, _ := cronTestTools(store)
		ctx := cronChatCtx(111, 111)
		now := time.Now().UTC().Truncate(time.Second)
		// Start with one failure already, so a miscount would
		// be visible as 2 (and a stop-then-fail path as 1).
		id := cronDueJob(t, store, ctx, sched, "* * * * *", "stopped job", now.Add(-time.Minute))
		rewriteCronJob(t, store, ctx, id, func(j *CronJob) {
			j.ConsecutiveFailures = 1
		})

		runner := &cronFakeRunner{err: runErr}
		s := NewCronScheduler(store)
		s.SetRunner(runner.run)
		if err := s.ProcessDue(ctx, now); err != nil {
			t.Fatalf("%s: ProcessDue: %v", name, err)
		}
		if runner.count() != 1 {
			t.Fatalf("%s: runner calls = %d, want 1", name, runner.count())
		}
		nextRun, lastRes, failures := cronJobNextRun(t, list, ctx, id)
		if failures != 1 {
			t.Fatalf("%s: failures = %d, want 1 (a deliberate stop must not increment the count)", name, failures)
		}
		if !strings.Contains(lastRes, "stopped") {
			t.Fatalf("%s: last result = %q, want a stopped note", name, lastRes)
		}
		next, err := time.Parse(time.RFC3339, nextRun)
		if err != nil {
			t.Fatalf("%s: next run %q: %v", name, nextRun, err)
		}
		if !next.After(now) {
			t.Fatalf("%s: next run %s is not after the tick (the stopped slot was not consumed)", name, nextRun)
		}
		// The job is not disabled by the stop.
		got := mustCronList(t, list, ctx)
		if got.Jobs[0].Disabled {
			t.Fatalf("%s: job is disabled after a stop", name)
		}
	}
	// The sentinel runScheduledTurn returns for a /stop or a
	// cron_cancel stop.
	t.Run("sentinel", func(t *testing.T) {
		runCase(t, "sentinel", ErrCronTurnStopped)
	})
	// A runner that reports only the bare context cancellation
	// (e.g. an older runner shape) is a stop too, not a failure.
	t.Run("context-canceled", func(t *testing.T) {
		runCase(t, "context-canceled", fmt.Errorf("turn aborted: %w", context.Canceled))
	})
	// StopJob reports false when nothing is running, and the
	// cancel tool reports stopped_running:false for it.
	t.Run("stop-when-not-running", func(t *testing.T) {
		store := NewPlanningStore("")
		sched, _, cancel := cronTestTools(store)
		ctx := cronChatCtx(111, 111)
		created := mustCronSchedule(t, sched, ctx, "0 9 * * *", "not running")
		s := NewCronScheduler(store)
		if s.StopJob(created.Job.ID) {
			t.Fatalf("StopJob on a job that is not running returned true")
		}
		if s.StopJob("cron_no_such_job") {
			t.Fatalf("StopJob on an unknown job returned true")
		}
		cancel.stopJob = s.StopJob
		out, err := cancel.Execute(ctx, json.RawMessage(fmt.Sprintf(`{"job_id":%q}`, created.Job.ID)))
		if err != nil {
			t.Fatalf("cron_cancel: %v", err)
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(out), &parsed); err != nil {
			t.Fatalf("cancel result is not JSON: %v", err)
		}
		if parsed["stopped_running"] != false {
			t.Fatalf("cancel result = %s, want stopped_running:false (nothing was running)", out)
		}
	})
}

func TestCronCancelStopJobWiring(t *testing.T) {
	// Nil-safe, mirroring SetCronScheduleRuleStore: a nil registry
	// or fn is a no-op, never a panic.
	SetCronCancelStopJob(nil, func(string) bool { return true })
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	SetCronCancelStopJob(reg, nil)

	// Before wiring, the registry's cron_cancel cancels
	// future runs only: stopped_running is false.
	ctx := cronChatCtx(111, 111)
	out, err := reg.Execute(ctx, "cron_schedule", json.RawMessage(`{"expression":"0 9 * * *","prompt":"via registry"}`))
	if err != nil {
		t.Fatalf("registry cron_schedule: %v", err)
	}
	var created cronJobOut
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("registry schedule result: %v", err)
	}
	out, err = reg.Execute(ctx, "cron_cancel", json.RawMessage(fmt.Sprintf(`{"job_id":%q}`, created.Job.ID)))
	if err != nil {
		t.Fatalf("registry cron_cancel before wiring: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("cancel result is not JSON: %v", err)
	}
	if parsed["stopped_running"] != false {
		t.Fatalf("cancel result before wiring = %s, want stopped_running:false", out)
	}

	// After wiring, the installed hook is called with the job id.
	called := ""
	SetCronCancelStopJob(reg, func(id string) bool {
		called = id
		return true
	})
	out, err = reg.Execute(ctx, "cron_schedule", json.RawMessage(`{"expression":"0 9 * * *","prompt":"second job"}`))
	if err != nil {
		t.Fatalf("registry cron_schedule: %v", err)
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("registry schedule result: %v", err)
	}
	out, err = reg.Execute(ctx, "cron_cancel", json.RawMessage(fmt.Sprintf(`{"job_id":%q}`, created.Job.ID)))
	if err != nil {
		t.Fatalf("registry cron_cancel after wiring: %v", err)
	}
	if called != created.Job.ID {
		t.Fatalf("stop hook called with %q, want the cancelled job %q", called, created.Job.ID)
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("cancel result is not JSON: %v", err)
	}
	if parsed["stopped_running"] != true {
		t.Fatalf("cancel result after wiring = %s, want stopped_running:true", out)
	}
}
