package main

import (
	"testing"

	"github.com/irfan/orion/internal/core"
)

// --- Unattended scheduled turns (F-C3, F-C8) ---------------------------
//
// scheduledTurnAllDenied is the predicate runScheduledTurn uses to
// decide whether a finished scheduled turn counts toward the job's
// consecutive failures: only a turn that attempted tool calls and
// had EVERY one of them denied by the gate counts — a turn with no
// tool calls, or one where any call ran, records "ok" as before.

func TestScheduledTurnAllDenied(t *testing.T) {
	cases := []struct {
		name  string
		stats *core.TurnStats
		want  bool
	}{
		{"nil stats", nil, false},
		{"no tool calls", &core.TurnStats{ToolCalls: 0, GateDeniedCalls: 0}, false},
		{"one call denied", &core.TurnStats{ToolCalls: 1, GateDeniedCalls: 1}, true},
		{"all calls denied", &core.TurnStats{ToolCalls: 3, GateDeniedCalls: 3}, true},
		{"one of two denied", &core.TurnStats{ToolCalls: 2, GateDeniedCalls: 1}, false},
		{"no call denied", &core.TurnStats{ToolCalls: 2, GateDeniedCalls: 0}, false},
	}
	for _, tc := range cases {
		if got := scheduledTurnAllDenied(tc.stats); got != tc.want {
			t.Errorf("%s: scheduledTurnAllDenied = %v, want %v", tc.name, got, tc.want)
		}
	}
}
