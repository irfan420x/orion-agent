package main

import (
	"testing"
	"time"

	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/tools"
)

// --- Budget-exceeded scheduled turns (F-C7) ---------------------------
//
// budgetNoticeAlreadySentToday is the throttle runScheduledTurn
// uses to send the budget notice at most once per job per day:
// the scheduler records a budget-exceeded run in the job's
// LastResult, so a stored "budget-exceeded" outcome from a run
// earlier today (UTC) means the notice already went out. The
// signal is the persisted LastResult/LastRun pair, so it
// survives a daemon restart.

func TestBudgetNoticeAlreadySentToday(t *testing.T) {
	// now is a fixed Saturday morning UTC; "today" is 2026-11-14.
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	job := func(lastResult, lastRun string) tools.CronJob {
		return tools.CronJob{ID: "job1", LastResult: lastResult, LastRun: lastRun}
	}
	cases := []struct {
		name string
		job  tools.CronJob
		want bool
	}{
		{
			"budget outcome earlier today: already sent",
			job("budget-exceeded: daily token budget reached; next run 2026-11-15T00:00:00Z (UTC)", "2026-11-14T09:00:00Z"),
			true,
		},
		{
			"budget outcome yesterday: not sent today",
			job("budget-exceeded: daily token budget reached; next run 2026-11-14T00:00:00Z (UTC)", "2026-11-13T15:30:00Z"),
			false,
		},
		{
			"budget outcome just before midnight yesterday, now just after: not sent today",
			job("budget-exceeded: daily token budget reached; next run 2026-11-14T00:00:00Z (UTC)", "2026-11-13T23:59:00Z"),
			false,
		},
		{
			"ok outcome today: not sent",
			job("ok", "2026-11-14T09:00:00Z"),
			false,
		},
		{
			"error outcome today: not sent",
			job("error: provider exploded", "2026-11-14T09:00:00Z"),
			false,
		},
		{
			"skipped outcome today: not sent",
			job("skipped: scheduled run at 2026-11-14T09:00:00Z was missed by 11m0s (daemon was down or busy); next run 2026-11-14T09:01:00Z (UTC)", "2026-11-14T09:00:00Z"),
			false,
		},
		{
			"budget outcome but no last run: not sent",
			job("budget-exceeded: daily token budget reached; next run 2026-11-15T00:00:00Z (UTC)", ""),
			false,
		},
		{
			"budget outcome but unparsable last run: not sent",
			job("budget-exceeded: daily token budget reached; next run 2026-11-15T00:00:00Z (UTC)", "not-a-time"),
			false,
		},
		{
			"bare budget prefix today: already sent",
			job("budget-exceeded", "2026-11-14T09:00:00Z"),
			true,
		},
		{
			"no outcome at all: not sent",
			job("", ""),
			false,
		},
	}
	for _, tc := range cases {
		if got := budgetNoticeAlreadySentToday(tc.job, now); got != tc.want {
			t.Errorf("%s: budgetNoticeAlreadySentToday = %v, want %v (job %+v)", tc.name, got, tc.want, tc.job)
		}
	}
	// A last run earlier today in a non-UTC offset still counts
	// when it is the same UTC day: LastRun is stored RFC3339
	// UTC, but the comparison is on the UTC calendar date, so
	// an offset form of the same instant must match.
	offset := job("budget-exceeded: daily token budget reached; next run 2026-11-15T00:00:00Z (UTC)", "2026-11-14T11:00:00+02:00")
	if !budgetNoticeAlreadySentToday(offset, now) {
		t.Errorf("offset last run on the same UTC day: budgetNoticeAlreadySentToday = false, want true")
	}
}

// TestBudgetExceededTurnIsNotAllDenied pins the F-C7/F-C8
// boundary: a budget refusal must never be counted as an
// all-denied turn. The pre-flight refusal never runs the agent,
// so it carries nil stats (zero tool calls) — and
// scheduledTurnAllDenied is false for that, exactly as for a
// plain-text answer. The other half of the boundary is structural
// (runScheduledTurn returns the budget error before it ever
// reaches the all-denied check), so a mid-turn abort that happens
// to deny every tool call it attempted still reports
// budget-exceeded, never all-denied.
func TestBudgetExceededTurnIsNotAllDenied(t *testing.T) {
	// Pre-flight refusal: the turn never ran, nil stats.
	if scheduledTurnAllDenied(nil) {
		t.Errorf("scheduledTurnAllDenied(nil) = true, want false: a pre-flight budget refusal has no tool calls and is not all-denied")
	}
	// Zero tool calls, explicitly.
	if scheduledTurnAllDenied(&core.TurnStats{ToolCalls: 0, GateDeniedCalls: 0}) {
		t.Errorf("scheduledTurnAllDenied(0 calls) = true, want false: a budget refusal with no tool calls is not all-denied")
	}
}
