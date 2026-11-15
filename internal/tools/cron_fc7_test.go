package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// --- Budget-exceeded scheduled turns (F-C7) ---------------------------
//
// A scheduled turn refused for the chat's daily token budget used
// to be recorded as "ok": the runner built the budget notice,
// sent it, and returned nil, so the scheduler reset
// ConsecutiveFailures to 0 and advanced NextRun to the next cron
// slot — and a minutely job spammed the chat with a budget notice
// every minute until the UTC-midnight reset. The runner now
// returns an error wrapping ErrBudgetExceeded, and the scheduler
// records a distinct budget-exceeded outcome: the failure count
// is left exactly where it was, the job is never disabled on
// this outcome, and the next run waits for the UTC-midnight
// reset instead of the next cron slot.
//
// The tests use a fixed tick time (mid-afternoon UTC) so the
// expected midnight is exact and no wall-clock midnight can
// straddle the run.

// cronBudgetRunner is a cronFakeRunner whose turns are always
// refused for the budget, wrapped the way the daemon's
// runScheduledTurn wraps it.
func cronBudgetRunner() *cronFakeRunner {
	return &cronFakeRunner{err: fmt.Errorf("turn refused: %w", ErrBudgetExceeded)}
}

// cronBudgetTick runs one budget-refused tick and returns the
// stored job list afterwards.
func cronBudgetTick(t *testing.T, s *CronScheduler, list *cronListTool, ctx context.Context, now time.Time) cronListOut {
	t.Helper()
	if err := s.ProcessDue(ctx, now); err != nil {
		t.Fatalf("ProcessDue: %v", err)
	}
	got := mustCronList(t, list, ctx)
	if got.Count != 1 {
		t.Fatalf("list = %d jobs, want 1", got.Count)
	}
	return got
}

func TestCronSchedulerBudgetExceededOutcome(t *testing.T) {
	store := NewPlanningStore("")
	sched, list, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	now := time.Date(2026, 11, 14, 15, 30, 0, 0, time.UTC)
	id := cronDueJob(t, store, ctx, sched, "* * * * *", "minutely brief", now.Add(-time.Minute))
	// A real failure streak must survive a budget refusal: it is
	// neither reset (as "ok" used to) nor grown.
	rewriteCronJob(t, store, ctx, id, func(j *CronJob) {
		j.ConsecutiveFailures = 2
	})

	runner := cronBudgetRunner()
	s := NewCronScheduler(store)
	s.SetRunner(runner.run)
	j := cronBudgetTick(t, s, list, ctx, now).Jobs[0]
	if runner.count() != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.count())
	}
	if j.ID != id {
		t.Fatalf("job id = %s, want %s", j.ID, id)
	}
	if !strings.HasPrefix(j.LastResult, CronBudgetExceededResult) {
		t.Fatalf("last result = %q, want the %q prefix (not \"ok\")", j.LastResult, CronBudgetExceededResult)
	}
	if j.ConsecutiveFailures != 2 {
		t.Fatalf("failures = %d, want 2 (a budget refusal neither resets nor grows the count)", j.ConsecutiveFailures)
	}
	if j.Disabled {
		t.Fatalf("job is disabled after a budget refusal: %+v", j)
	}
	next, err := time.Parse(time.RFC3339, j.NextRun)
	if err != nil {
		t.Fatalf("next run %q: %v", j.NextRun, err)
	}
	// Next run is the UTC-midnight reset, not the next minute.
	if want := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next run = %s, want the budget reset %s", j.NextRun, want.Format(time.RFC3339))
	}
}

func TestCronSchedulerBudgetExceededSecondFiringSameDay(t *testing.T) {
	store := NewPlanningStore("")
	sched, list, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	now := time.Date(2026, 11, 14, 15, 30, 0, 0, time.UTC)
	id := cronDueJob(t, store, ctx, sched, "* * * * *", "minutely brief", now.Add(-time.Minute))
	rewriteCronJob(t, store, ctx, id, func(j *CronJob) {
		j.ConsecutiveFailures = 2
	})

	runner := cronBudgetRunner()
	s := NewCronScheduler(store)
	s.SetRunner(runner.run)
	cronBudgetTick(t, s, list, ctx, now)

	// Force a second firing the same day (a rewritten NextRun, a
	// restart that re-fires): the scheduler must still record a
	// budget-exceeded outcome with the count untouched and the
	// next run at the reset — and, crucially, the job copy the
	// runner receives must carry the first run's
	// "budget-exceeded" LastResult, which is the persisted
	// signal the daemon reads to suppress the second chat
	// notice (see budgetNoticeAlreadySentToday in cmd/oriond).
	rewriteCronJob(t, store, ctx, id, func(j *CronJob) {
		j.NextRun = now.Add(-time.Minute).Format(time.RFC3339)
	})
	j := cronBudgetTick(t, s, list, ctx, now).Jobs[0]
	if runner.count() != 2 {
		t.Fatalf("runner calls = %d, want 2", runner.count())
	}
	if !strings.HasPrefix(runner.calls[1].LastResult, CronBudgetExceededResult) {
		t.Fatalf("second runner call saw last result = %q, want the %q prefix (the daemon suppresses the second notice on it)", runner.calls[1].LastResult, CronBudgetExceededResult)
	}
	if !strings.HasPrefix(j.LastResult, CronBudgetExceededResult) || j.ConsecutiveFailures != 2 || j.Disabled {
		t.Fatalf("job after second budget firing = %+v, want budget-exceeded / 2 failures / enabled", j)
	}
	next, err := time.Parse(time.RFC3339, j.NextRun)
	if err != nil {
		t.Fatalf("next run %q: %v", j.NextRun, err)
	}
	if want := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next run after second firing = %s, want the budget reset %s", j.NextRun, want.Format(time.RFC3339))
	}
}

func TestCronSchedulerBudgetExceededDailyJobKeepsSlot(t *testing.T) {
	store := NewPlanningStore("")
	sched, list, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	now := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	id := cronDueJob(t, store, ctx, sched, "0 9 * * *", "morning brief", now.Add(-time.Minute))

	runner := cronBudgetRunner()
	s := NewCronScheduler(store)
	s.SetRunner(runner.run)
	j := cronBudgetTick(t, s, list, ctx, now).Jobs[0]
	if j.ID != id {
		t.Fatalf("job id = %s, want %s", j.ID, id)
	}
	if !strings.HasPrefix(j.LastResult, CronBudgetExceededResult) {
		t.Fatalf("last result = %q, want the %q prefix", j.LastResult, CronBudgetExceededResult)
	}
	if j.ConsecutiveFailures != 0 || j.Disabled {
		t.Fatalf("job after budget refusal = %+v, want 0 failures / enabled", j)
	}
	next, err := time.Parse(time.RFC3339, j.NextRun)
	if err != nil {
		t.Fatalf("next run %q: %v", j.NextRun, err)
	}
	// The next daily slot (tomorrow 09:00) is already past the
	// midnight reset, so the job keeps its slot instead of
	// firing at midnight.
	if want := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next run = %s, want the next daily slot %s", j.NextRun, want.Format(time.RFC3339))
	}
}

func TestCronSchedulerBudgetExceededNeverDisables(t *testing.T) {
	store := NewPlanningStore("")
	sched, list, _ := cronTestTools(store)
	ctx := cronChatCtx(111, 111)
	now := time.Date(2026, 11, 14, 15, 30, 0, 0, time.UTC)
	id := cronDueJob(t, store, ctx, sched, "* * * * *", "always over budget", now.Add(-time.Minute))

	runner := cronBudgetRunner()
	s := NewCronScheduler(store)
	s.SetRunner(runner.run)

	// Five consecutive over-budget days: each tick fires at the
	// job's (reset) next run, and the job must never be
	// disabled — budget exhaustion is the chat being over its
	// allowance, not a defect in the job, and disabling it
	// would silently kill a healthy job after 5 over-budget
	// days. (Five is the generic disable threshold: a job that
	// genuinely failed 5 times in a row IS disabled — see
	// TestCronSchedulerBackoffAndDisable.)
	tickNow := now
	for day := 1; day <= cronMaxFailures; day++ {
		j := cronBudgetTick(t, s, list, ctx, tickNow).Jobs[0]
		if j.ID != id {
			t.Fatalf("day %d: job id = %s, want %s", day, j.ID, id)
		}
		if runner.count() != day {
			t.Fatalf("day %d: runner calls = %d, want %d", day, runner.count(), day)
		}
		if !strings.HasPrefix(j.LastResult, CronBudgetExceededResult) {
			t.Fatalf("day %d: last result = %q, want the %q prefix", day, j.LastResult, CronBudgetExceededResult)
		}
		if j.ConsecutiveFailures != 0 {
			t.Fatalf("day %d: failures = %d, want 0 (budget refusals never grow the count)", day, j.ConsecutiveFailures)
		}
		if j.Disabled {
			t.Fatalf("day %d: job disabled after %d budget refusals", day, day)
		}
		next, err := time.Parse(time.RFC3339, j.NextRun)
		if err != nil {
			t.Fatalf("day %d: next run %q: %v", day, j.NextRun, err)
		}
		if next.Hour() != 0 || next.Minute() != 0 || next.Second() != 0 {
			t.Fatalf("day %d: next run = %s, want a UTC midnight", day, j.NextRun)
		}
		tickNow = next.Add(30 * time.Second)
	}
}

func TestNextBudgetRun(t *testing.T) {
	midnight := func(y int, m time.Month, d int) time.Time {
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	cases := []struct {
		name     string
		nextCron time.Time
		now      time.Time
		want     time.Time
	}{
		{
			"minutely job waits for the reset",
			time.Date(2026, 11, 14, 15, 31, 0, 0, time.UTC),
			time.Date(2026, 11, 14, 15, 30, 0, 0, time.UTC),
			midnight(2026, 11, 15),
		},
		{
			"daily job keeps its next-day slot",
			time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC),
			time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC),
			time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC),
		},
		{
			"next slot exactly at the reset stays at the reset",
			midnight(2026, 11, 15),
			time.Date(2026, 11, 14, 15, 30, 0, 0, time.UTC),
			midnight(2026, 11, 15),
		},
		{
			"late-evening firing still waits for midnight",
			time.Date(2026, 11, 14, 23, 59, 0, 0, time.UTC),
			time.Date(2026, 11, 14, 23, 58, 0, 0, time.UTC),
			midnight(2026, 11, 15),
		},
	}
	for _, tc := range cases {
		if got := nextBudgetRun(tc.nextCron, tc.now); !got.Equal(tc.want) {
			t.Errorf("%s: nextBudgetRun(%s, %s) = %s, want %s", tc.name, tc.nextCron, tc.now, got, tc.want)
		}
	}
}

func TestNextUTCMidnight(t *testing.T) {
	cases := []struct {
		at   time.Time
		want time.Time
	}{
		{time.Date(2026, 11, 14, 15, 30, 0, 0, time.UTC), time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 11, 14, 23, 59, 59, 0, time.UTC), time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		if got := nextUTCMidnight(tc.at); !got.Equal(tc.want) {
			t.Errorf("nextUTCMidnight(%s) = %s, want %s", tc.at, got, tc.want)
		}
	}
}
