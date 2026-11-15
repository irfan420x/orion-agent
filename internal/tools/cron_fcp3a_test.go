// P3-A tests (F-C18, F-C19, F-C26, F-C28 + P2-A residual R2):
// a slow successful turn must not re-fire back-to-back, a daemon
// shutdown mid-turn must be recorded as a shutdown (not an owner
// stop, and never a failure strike), the tick's load must take
// the flock like every writer, the next-run search must return
// exactly what the old minute scan returned without scanning
// minute by minute, and the post-loop outcome apply must survive
// a shutdown with a contended lock.
package tools

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCronNextRunAfterOutcomeSkipsPastSlot(t *testing.T) {
	// Unit pin for the F-C18 rule, without wall time: the slot
	// computed from the turn's start is kept when the turn
	// finished before it, and replaced by the next occurrence
	// after the turn's outcome time when the turn outlasted it.
	sched, err := parseCronExpression("*/5 * * * *")
	if err != nil {
		t.Fatalf("parseCronExpression: %v", err)
	}
	now := time.Date(2026, 11, 15, 10, 4, 30, 0, time.UTC)
	nextAfter, err := sched.nextAfter(now)
	if err != nil {
		t.Fatalf("nextAfter: %v", err)
	}
	if !nextAfter.Equal(time.Date(2026, 11, 15, 10, 5, 0, 0, time.UTC)) {
		t.Fatalf("nextAfter(%s) = %s, want 10:05", now, nextAfter)
	}
	// Turn finished before the slot: the slot is kept exactly.
	if got := sched.nextRunAfterOutcome(nextAfter, now.Add(10*time.Second)); !got.Equal(nextAfter) {
		t.Fatalf("nextRunAfterOutcome(done before slot) = %s, want the slot %s", got, nextAfter)
	}
	// Turn finished after the slot: the next occurrence after
	// the outcome time, not the outlasted slot.
	doneAt := now.Add(90 * time.Second) // 10:06:00
	want, err := sched.nextAfter(doneAt)
	if err != nil {
		t.Fatalf("nextAfter(doneAt): %v", err)
	}
	if got := sched.nextRunAfterOutcome(nextAfter, doneAt); !got.Equal(want) {
		t.Fatalf("nextRunAfterOutcome(done after slot) = %s, want %s", got, want)
	}
	// Done exactly at the slot: the slot is not after the
	// outcome time, so it is skipped too.
	if got := sched.nextRunAfterOutcome(nextAfter, nextAfter); got.Equal(nextAfter) {
		t.Fatalf("nextRunAfterOutcome(done at the slot) = %s, want the following slot", got)
	}
}

func TestCronSchedulerSlowSuccessfulTurnDoesNotRefire(t *testing.T) {
	// F-C18: a successful turn that runs longer than the gap
	// to its next slot must not leave a next run in the past.
	// A 35s turn on a */5 job pinned so the next slot is
	// now+30s stands in for the production case (a 7-minute
	// turn on a 5-minute job, inside the 10-minute turn
	// timeout): the outlasted now+30s slot must be skipped,
	// and the next tick — past that slot, before the
	// outcome-anchored one — must NOT re-fire the job.
	// (The failure path already anchored its backoff at the
	// outcome time; the success path kept the start-anchored
	// slot, so a slow success re-fired on the next tick.)
	store := NewPlanningStore("")
	sched, list, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	base := time.Now().UTC().Truncate(time.Minute)
	for base.Minute()%5 != 4 {
		base = base.Add(time.Minute)
	}
	now := base.Add(30 * time.Second)
	cronDueJob(t, store, ctx, sched, "*/5 * * * *", "slow then succeeds", now.Add(-time.Minute))

	var calls atomic.Int32
	s := NewCronScheduler(store)
	s.SetRunner(func(ctx context.Context, job CronJob) (string, error) {
		calls.Add(1)
		time.Sleep(35 * time.Second)
		return "ok", nil
	})
	if err := s.ProcessDue(ctx, now); err != nil {
		t.Fatalf("ProcessDue: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("runner calls = %d, want 1", calls.Load())
	}
	got := mustCronList(t, list, ctx)
	j := got.Jobs[0]
	if j.LastResult != "ok" || j.ConsecutiveFailures != 0 {
		t.Fatalf("job after slow success = %+v, want ok/0 failures", j)
	}
	next, err := time.Parse(time.RFC3339, j.NextRun)
	if err != nil {
		t.Fatalf("next run %q: %v", j.NextRun, err)
	}
	// The turn ended at ~now+35s, past the now+30s slot, so
	// the next run must be the following */5 slot (~now+90s)
	// — certainly after now+50s, the second tick below.
	if !next.After(now.Add(50 * time.Second)) {
		t.Fatalf("next run %s is not after now+50s (want the next slot after the turn ended, ~now+90s, not the outlasted now+30s slot)", j.NextRun)
	}
	// A real next tick at now+50s — past the outlasted slot,
	// before the outcome-anchored one — must NOT re-fire.
	if err := s.ProcessDue(ctx, now.Add(50*time.Second)); err != nil {
		t.Fatalf("second ProcessDue: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("runner calls after second tick = %d, want 1 (no back-to-back re-fire: the outlasted slot was skipped)", calls.Load())
	}
}

func TestCronSchedulerShutdownMidTurnRecordsShutdownNotStop(t *testing.T) {
	// F-C19 (P2-A residual R1): a daemon shutdown mid-turn
	// must not cost a failure strike — and must not be
	// mislabelled as an owner stop. The runner blocks until
	// its turn context is cancelled (the daemon going down),
	// then reports the cancellation, exactly as the daemon's
	// runner does; the parent context is the daemon context,
	// so cancelling it is a shutdown, not a StopJob.
	store := NewPlanningStore("")
	sched, list, _ := cronTestTools(store)
	baseCtx := cronChatCtx(111, 111)
	now := time.Now().UTC().Truncate(time.Second)
	id := cronDueJob(t, store, baseCtx, sched, "*/5 * * * *", "shutdown job", now.Add(-time.Minute))
	// Start with one failure already, so a strike would be
	// visible as 2.
	rewriteCronJob(t, store, baseCtx, id, func(j *CronJob) {
		j.ConsecutiveFailures = 1
	})

	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()
	started := make(chan struct{})
	var once sync.Once
	runner := func(rctx context.Context, job CronJob) (string, error) {
		once.Do(func() { close(started) })
		<-rctx.Done()
		return "", rctx.Err()
	}
	s := NewCronScheduler(store)
	s.SetRunner(runner)
	done := make(chan error, 1)
	go func() { done <- s.ProcessDue(ctx, now) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("runner never started")
	}
	cancel() // daemon shutdown, mid-turn
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ProcessDue: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ProcessDue did not return after shutdown")
	}
	_, lastRes, failures := cronJobNextRun(t, list, baseCtx, id)
	if failures != 1 {
		t.Fatalf("failures = %d, want 1 (a shutdown is not the job's fault and must not cost a strike)", failures)
	}
	if !strings.Contains(lastRes, "shutdown") {
		t.Fatalf("last result = %q, want a shutdown note", lastRes)
	}
	if strings.Contains(lastRes, "stopped by the owner") {
		t.Fatalf("last result = %q, want no owner-stop wording for a daemon shutdown", lastRes)
	}
	nextRun, _, _ := cronJobNextRun(t, list, baseCtx, id)
	next, err := time.Parse(time.RFC3339, nextRun)
	if err != nil {
		t.Fatalf("next run %q: %v", nextRun, err)
	}
	if !next.After(now) {
		t.Fatalf("next run %s is not after the tick (the shutdown slot was not consumed)", nextRun)
	}
	got := mustCronList(t, list, baseCtx)
	if got.Jobs[0].Disabled {
		t.Fatalf("job is disabled after a shutdown")
	}
}

func TestCronSchedulerTickLoadTakesFlock(t *testing.T) {
	// F-C26: processChat loads under the flock, like every
	// writer. Hold the flock the way a writer would, then
	// tick with a short-lived context: the tick's load must
	// block on the flock and fail loudly ("load jobs"), never
	// decide on an unlocked read and run the job — and never
	// silently skip past the load to the claim. The control
	// (flock free) must fire normally, so a broken setup
	// cannot fake the assertion.
	dir := t.TempDir()
	store := NewPlanningStore(dir)
	sched, _, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	now := time.Now().UTC().Truncate(time.Second)
	cronDueJob(t, store, ctx, sched, "*/5 * * * *", "flocked load", now.Add(-time.Minute))

	chatDir := filepath.Join(dir, "chats", "111")
	release, err := store.lockState(context.Background(), chatDir, cronJobsFile)
	if err != nil {
		t.Fatalf("hold flock: %v", err)
	}
	var logBuf bytes.Buffer
	s := NewCronScheduler(store)
	s.SetLogger(log.New(&logBuf, "", 0))
	runner := &cronFakeRunner{}
	s.SetRunner(runner.run)
	tickCtx, tickCancel := context.WithTimeout(ctx, time.Second)
	defer tickCancel()
	if err := s.ProcessDue(tickCtx, now); err != nil {
		t.Fatalf("ProcessDue: %v", err)
	}
	if runner.count() != 0 {
		t.Fatalf("runner calls = %d, want 0 (the tick's load could not take the flock)", runner.count())
	}
	if !strings.Contains(logBuf.String(), "load jobs") {
		t.Fatalf("tick log = %q, want a load-jobs failure (the load must block on the flock, not skip past it to the claim)", logBuf.String())
	}
	// Positive control: with the flock free, the same tick fires.
	release()
	if err := s.ProcessDue(ctx, now); err != nil {
		t.Fatalf("ProcessDue (control): %v", err)
	}
	if runner.count() != 1 {
		t.Fatalf("control runner calls = %d, want 1", runner.count())
	}
}

// naiveNextAfter is the pre-F-C28 minute scan, kept as the
// equivalence oracle for the new field-wise search: the new
// computation must return exactly what it returned, for every
// expression and date below.
func naiveNextAfter(s *cronSchedule, after time.Time) (time.Time, error) {
	t := after.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(cronSearchYears, 0, 0)
	for !t.After(limit) {
		if s.matches(t) {
			return t, nil
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, fmt.Errorf("cron expression has no run in the next %d years", cronSearchYears)
}

func TestCronNextAfterMatchesNaiveScan(t *testing.T) {
	// F-C28: the field-wise nextAfter must compute exactly
	// the same next run as the old minute scan — same time,
	// same no-run-in-5-years error — over a broad matrix of
	// expressions (including the Feb 29 worst case, DOM/DOW
	// OR semantics, bare-number steps, and a never-firing
	// April 31) and start dates (month ends, leap days, the
	// 5-year cap edge).
	exprs := []string{
		"* * * * *",
		"*/15 * * * *",
		"2/10 * * * *",
		"5/15 * * * *",
		"0 9 * * *",
		"30 4 1,15 * *",
		"0 0 * * 0",
		"0 0 * * 1-5",
		"0 12 * * 1-5",
		"0 0 29 2 *",
		"0 0 29 2 1",
		"0 0 29 2 0",
		"0 0 31 * *",
		"0 0 31 4 *",
		"59 23 31 12 *",
		"0 0 1 1 *",
		"15 3 * * 6",
		"45 2 29 2 *",
		"0 0 * 2 *",
		"0 0 * * 7",
	}
	starts := []time.Time{
		time.Date(2026, 11, 15, 10, 30, 0, 0, time.UTC),
		time.Date(2026, 2, 28, 23, 59, 0, 0, time.UTC),
		time.Date(2027, 2, 28, 0, 0, 0, 0, time.UTC),
		time.Date(2028, 2, 28, 12, 0, 0, 0, time.UTC),
		time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 31, 23, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC),
		time.Date(2031, 11, 15, 10, 30, 0, 0, time.UTC),
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 15, 8, 45, 0, 0, time.UTC),
	}
	for _, expr := range exprs {
		sched, err := parseCronExpression(expr)
		if err != nil {
			t.Fatalf("parse %q: %v", expr, err)
		}
		for _, start := range starts {
			want, wantErr := naiveNextAfter(sched, start)
			got, gotErr := sched.nextAfter(start)
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("nextAfter(%q, %s): new err=%v, naive err=%v", expr, start.Format(time.RFC3339), gotErr, wantErr)
			}
			if wantErr == nil && !got.Equal(want) {
				t.Fatalf("nextAfter(%q, %s) = %s, want %s (naive minute scan)", expr, start.Format(time.RFC3339), got.Format(time.RFC3339), want.Format(time.RFC3339))
			}
		}
	}
}

func TestCronNextAfterFeb29IsFast(t *testing.T) {
	// F-C28: the old worst case — Feb 29 from just after one
	// — scanned ~2.6M minutes. The field-wise search must
	// answer it in well under 100ms (it takes microseconds:
	// a month jump, 28 day jumps, and the matching day).
	sched, err := parseCronExpression("0 0 29 2 *")
	if err != nil {
		t.Fatalf("parseCronExpression: %v", err)
	}
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	t0 := time.Now()
	got, err := sched.nextAfter(start)
	elapsed := time.Since(t0)
	if err != nil {
		t.Fatalf("nextAfter: %v", err)
	}
	if !got.Equal(time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("nextAfter = %s, want 2028-02-29", got.Format(time.RFC3339))
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("nextAfter Feb 29 took %s, want < 100ms (the minute scan took ~2.6M iterations)", elapsed)
	}
}

func BenchmarkCronNextAfterFeb29(b *testing.B) {
	sched, err := parseCronExpression("0 0 29 2 *")
	if err != nil {
		b.Fatalf("parseCronExpression: %v", err)
	}
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sched.nextAfter(start); err != nil {
			b.Fatalf("nextAfter: %v", err)
		}
	}
}

func BenchmarkCronNextAfterFeb29Naive(b *testing.B) {
	sched, err := parseCronExpression("0 0 29 2 *")
	if err != nil {
		b.Fatalf("parseCronExpression: %v", err)
	}
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := naiveNextAfter(sched, start); err != nil {
			b.Fatalf("naiveNextAfter: %v", err)
		}
	}
}

func TestCronSchedulerPostLoopOutcomeApplySurvivesShutdown(t *testing.T) {
	// P2-A residual R2: the post-loop outcome apply must use
	// context.WithoutCancel, like the mid-loop shutdown
	// branch. Hold the flock while the turn ends in a
	// shutdown (the test cancels the daemon context as the
	// turn returns): with a plain ctx the apply fails on the
	// contended lock and the "ok" outcome is lost (the retry
	// fails the same way); with WithoutCancel it waits the
	// flock out and the outcome lands.
	dir := t.TempDir()
	store := NewPlanningStore(dir)
	sched, list, _ := cronTestTools(store)
	baseCtx := cronChatCtx(111, 111)
	now := time.Now().UTC().Truncate(time.Second)
	id := cronDueJob(t, store, baseCtx, sched, "*/5 * * * *", "shutdown apply", now.Add(-time.Minute))

	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()
	started := make(chan struct{})
	releaseTurn := make(chan struct{})
	var calls atomic.Int32
	var once sync.Once
	runner := func(rctx context.Context, job CronJob) (string, error) {
		calls.Add(1)
		once.Do(func() { close(started) })
		<-releaseTurn
		cancel() // daemon shuts down just as the turn ends
		return "ok", nil
	}
	s := NewCronScheduler(store)
	s.SetRunner(runner)
	done := make(chan error, 1)
	go func() { done <- s.ProcessDue(ctx, now) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("runner never started")
	}
	// Take the flock before the turn returns: the claim is
	// done (the runner only starts after it), so this
	// succeeds at once, and the post-loop apply will find
	// the lock contended.
	chatDir := filepath.Join(dir, "chats", "111")
	flockRelease, err := store.lockState(context.Background(), chatDir, cronJobsFile)
	if err != nil {
		t.Fatalf("hold flock: %v", err)
	}
	close(releaseTurn)
	// Let the apply start and block on the flock, then free it.
	time.Sleep(time.Second)
	flockRelease()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ProcessDue: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("ProcessDue did not return after the flock was released")
	}
	if calls.Load() != 1 {
		t.Fatalf("runner calls = %d, want 1", calls.Load())
	}
	_, lastRes, failures := cronJobNextRun(t, list, baseCtx, id)
	if lastRes != "ok" {
		t.Fatalf("last result = %q, want \"ok\" (the post-loop outcome apply must survive the shutdown, not be lost to the cancelled context)", lastRes)
	}
	if failures != 0 {
		t.Fatalf("failures = %d, want 0", failures)
	}
}
