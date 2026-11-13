package messaging

// rc9 (Worker E) tests: the footer breaks out subagent tokens, and
// /usage shows the per-chat/per-day block (tokens + estimated USD +
// daily budget) when the daemon wires UsageForChat, and the old
// totals-only body when it does not.
import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/irfan/orion/internal/core"
)

func TestFormatFooterWithSubagentTokens(t *testing.T) {
	// 1,240 tokens this turn, 300 of them from subagent results the
	// parent consumed: the footer total includes them AND says so.
	stats := &core.TurnStats{Provider: "deepseek", Model: "deepseek-chat", Tokens: 1240, SubagentTokens: 300, Steps: 3, Duration: 4200 * time.Millisecond}
	if got, want := FormatFooter(stats), "— deepseek-chat · 1,240 tokens (incl. 300 subagent) · 3 steps · 4.2s"; got != want {
		t.Fatalf("FormatFooter = %q, want %q", got, want)
	}
	// The breakout composes with the memories segment.
	stats.MemoriesUsed = 2
	if got, want := FormatFooter(stats), "— deepseek-chat · 1,240 tokens (incl. 300 subagent) · 3 steps · 4.2s · 2 memories"; got != want {
		t.Fatalf("FormatFooter with memories = %q, want %q", got, want)
	}
	// Zero subagent tokens: the footer renders exactly as before —
	// no "(incl. 0 subagent)" noise on ordinary turns.
	plain := &core.TurnStats{Provider: "deepseek", Model: "deepseek-chat", Tokens: 1240, Steps: 3, Duration: 4200 * time.Millisecond}
	if got, want := FormatFooter(plain), "— deepseek-chat · 1,240 tokens · 3 steps · 4.2s"; got != want {
		t.Fatalf("FormatFooter without subagents = %q, want %q", got, want)
	}
}

func usageCostEnv(t *testing.T, usageForChat func(chatID int64) string, deadLetter func() (int, int)) *CommandEnv {
	t.Helper()
	return &CommandEnv{
		ChatID: 7,
		Session: func(chatID int64) *Session {
			return &Session{ChatID: chatID, TotalTokens: 5000, Turns: 3}
		},
		Usage:        func() (int, int) { return 9000, 5 },
		Limits:       func() (int, int) { return 20, 120 },
		UsageForChat: usageForChat,
		DeadLetter:   deadLetter,
	}
}

func TestUsageShowsPerChatDayBlock(t *testing.T) {
	env := usageCostEnv(t, func(chatID int64) string {
		if chatID != 7 {
			t.Errorf("UsageForChat called with chat %d, want 7 (the dispatching chat)", chatID)
		}
		return "Tokens today (this chat): 3,337 (3,000 prompt + 337 completion, incl. 0 subagent)\n" +
			"Est. cost today (this chat): $0.0010 (est., deepseek-chat, price table v2026-11-13 effective 2026-11-13)\n" +
			"Daily token budget: 100,000 tokens (96,663 remaining, resets 2026-11-14 00:00 UTC)"
	}, nil)
	r := DefaultRegistry(env)
	reply, handled, err := r.Dispatch(context.Background(), env, "/usage")
	if err != nil || !handled {
		t.Fatalf("Dispatch /usage = handled=%v err=%v, want handled with no error", handled, err)
	}
	for _, want := range []string{
		"Tokens today (this chat): 3,337",
		"Est. cost today (this chat): $0.0010 (est., deepseek-chat",
		"Daily token budget: 100,000 tokens (96,663 remaining",
		"Tokens this chat (all time): 5000 (3 turn(s))",
		"Tokens (all chats): 9000 (5 turn(s))",
		"Max steps per turn: 20",
	} {
		if !strings.Contains(reply, want) {
			t.Errorf("/usage reply =\n%s\nmissing %q", reply, want)
		}
	}
}

func TestUsageWithoutUsageForChatShowsTotalsOnly(t *testing.T) {
	// Hand-built envs (nil UsageForChat) keep the old body: the
	// per-chat block is additive, never required.
	env := usageCostEnv(t, nil, nil)
	r := DefaultRegistry(env)
	reply, handled, err := r.Dispatch(context.Background(), env, "/usage")
	if err != nil || !handled {
		t.Fatalf("Dispatch /usage = handled=%v err=%v, want handled with no error", handled, err)
	}
	if strings.Contains(reply, "Tokens today") || strings.Contains(reply, "Est. cost") {
		t.Errorf("/usage reply without UsageForChat =\n%s\nmust not contain the per-chat block", reply)
	}
	if !strings.Contains(reply, "Tokens this chat (all time): 5000 (3 turn(s))") {
		t.Errorf("/usage reply =\n%s\nmissing the session totals line", reply)
	}
}

// TestUsageShowsDeadLetterLedger is the F-A10-2 consumer test:
// the dead-letter ledger (evicted subagent tasks, never charged)
// is surfaced in /usage as its own daemon-wide line — the
// charged/uncharged split — when it holds something, and omitted
// when it is empty or unwired, so ordinary /usage output stays
// clean. The read is non-draining: two /usage calls in a row
// show the same counts.
func TestUsageShowsDeadLetterLedger(t *testing.T) {
	usageForChat := func(chatID int64) string {
		return "Tokens today (this chat): 1,000 (900 prompt + 100 completion, incl. 50 subagent)\n" +
			"Daily token budget: unlimited"
	}
	deadLetter := func() (int, int) { return 500, 5 }
	env := usageCostEnv(t, usageForChat, deadLetter)
	r := DefaultRegistry(env)
	reply, handled, err := r.Dispatch(context.Background(), env, "/usage")
	if err != nil || !handled {
		t.Fatalf("Dispatch /usage = handled=%v err=%v, want handled with no error", handled, err)
	}
	// The charged side is in the per-chat block; the uncharged
	// side is the ledger line — the split must be visible, and
	// the ledger tokens must NOT be folded into the chat's total.
	if !strings.Contains(reply, "Evicted subagent tasks (never charged, daemon-wide): 5 task(s), 500 tokens") {
		t.Errorf("/usage reply =\n%s\nmissing the dead-letter line with the ledger counts", reply)
	}
	if strings.Contains(reply, "Tokens today (this chat): 1,500") {
		t.Errorf("/usage reply =\n%s\nmust not fold the ledger tokens into the chat's total", reply)
	}
	// A second /usage shows the same counts: the read does not
	// drain the ledger.
	reply2, _, err := r.Dispatch(context.Background(), env, "/usage")
	if err != nil {
		t.Fatalf("second Dispatch /usage: %v", err)
	}
	if !strings.Contains(reply2, "Evicted subagent tasks (never charged, daemon-wide): 5 task(s), 500 tokens") {
		t.Errorf("second /usage reply =\n%s\nmissing the dead-letter line (the read must not drain the ledger)", reply2)
	}
}

// TestUsageHidesEmptyDeadLetterLedger proves the ledger line is
// omitted when the ledger is empty (the normal case) or the env
// does not wire it: /usage stays exactly as before.
func TestUsageHidesEmptyDeadLetterLedger(t *testing.T) {
	usageForChat := func(chatID int64) string { return "Tokens today (this chat): 1,000\nDaily token budget: unlimited" }
	for _, tc := range []struct {
		name       string
		deadLetter func() (int, int)
	}{
		{"empty ledger", func() (int, int) { return 0, 0 }},
		{"unwired", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := usageCostEnv(t, usageForChat, tc.deadLetter)
			r := DefaultRegistry(env)
			reply, handled, err := r.Dispatch(context.Background(), env, "/usage")
			if err != nil || !handled {
				t.Fatalf("Dispatch /usage = handled=%v err=%v, want handled with no error", handled, err)
			}
			if strings.Contains(reply, "Evicted subagent tasks") {
				t.Errorf("/usage reply =\n%s\nmust not show the dead-letter line for an empty/unwired ledger", reply)
			}
		})
	}
}
