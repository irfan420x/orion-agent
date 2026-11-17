// Package tools — the shared computer-use budget (F-A23W2-14 /
// F-M2R-16).
//
// Before this file, ground, screenshot and input each enforced
// their own identical 15/turn + 120/hour limit on their own
// per-instance counters: a turn could spend 15 groundings + 15
// screenshots + 15 input calls (45 computer-use calls), and every
// child agent got a fresh set of counters on top — a parent at
// 30 captures/turn plus 30 more per child, all ClassSafe
// ground/screenshot calls with no approval. The budget is now
// ONE counter, shared by all three tools and by parent and child
// registries alike: a turn (parent + all its children together)
// spends at most captureMaxPerTurn computer-use calls, and at
// most captureMaxPerHour per hour.
//
// The per-tool 1 s min-interval stays per tool instance (it
// throttles how fast ONE tool is called, not the budget), and the
// per-tool error messages stay per tool ("at most 15 groundings
// per turn", ...); only the turn/hour counting is shared.
//
// Reset ownership: the budget's per-turn counter resets only
// when a tool built as the budget's OWNER calls BeginTurn. The
// parent registry's tools are owners; a child registry's tools
// share the budget but are NOT owners — a child turn starting
// mid-parent-turn must not wipe the parent's count, or the child
// would mint itself a fresh 15/turn inside the parent's turn.
// Exactly one registry in a process should own a given budget;
// DefaultRegistryWithDeps enforces the wiring (a nil budget in
// ToolDeps becomes a fresh budget owned by that registry).
package tools

import (
	"errors"
	"sync"
	"time"
)

// captureMaxPerTurn / captureMaxPerHour are the shared
// computer-use budget (plan §4.1's 15/turn, 120/hour — the
// numbers are unchanged, the counting is now shared instead of
// once per tool). The per-tool constants (groundMaxPerTurn,
// screenshotMaxPerTurn, inputMaxPerTurn, ...) alias these so the
// limits cannot drift apart between tools.
const (
	captureMaxPerTurn = 15
	captureMaxPerHour = 120
)

// errCaptureBudgetTurn / errCaptureBudgetHour are the Charge
// refusals. They are internal signals: each tool wraps them in
// its own rate-limit message, so the model hears "at most 15
// groundings per turn" from ground and "at most 15 captures per
// turn" from screenshot, as before.
var (
	errCaptureBudgetTurn = errors.New("capture budget: per-turn limit reached")
	errCaptureBudgetHour = errors.New("capture budget: per-hour limit reached")
)

// CaptureBudget is the shared per-turn + per-hour counter for
// computer-use calls. The zero value is usable, but tools always
// get one from NewCaptureBudget or ToolDeps; use NewCaptureBudget
// so the intent is explicit.
type CaptureBudget struct {
	mu        sync.Mutex
	turnCalls int
	hourCalls []time.Time
}

// NewCaptureBudget returns a fresh shared budget.
func NewCaptureBudget() *CaptureBudget { return &CaptureBudget{} }

// Charge enforces the per-turn and per-hour limits and records
// the call. It returns errCaptureBudgetTurn or
// errCaptureBudgetHour on refusal — a refused call records
// nothing, so a refusal never consumes a slot. Hour entries
// older than an hour are pruned on every charge, so the hourly
// window slides instead of sticking at the first hour's calls.
func (b *CaptureBudget) Charge(now time.Time, maxPerTurn, maxPerHour int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.turnCalls >= maxPerTurn {
		return errCaptureBudgetTurn
	}
	cutoff := now.Add(-time.Hour)
	kept := b.hourCalls[:0]
	for _, ts := range b.hourCalls {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	b.hourCalls = kept
	if len(b.hourCalls) >= maxPerHour {
		return errCaptureBudgetHour
	}
	b.turnCalls++
	b.hourCalls = append(b.hourCalls, now)
	return nil
}

// Refund returns one previously charged slot, per-turn and
// per-hour alike. It is for a charge whose call was then
// REFUSED before acting — a sensitive-window denial, a focus
// re-check denial — never for a call that may have taken
// effect (a timed-out click may already have landed, a failed
// move already moved the cursor part-way): refunding one of
// those would hand the quota back for an effect already spent,
// and invite a retry into a double-click. Refunding when
// nothing is charged is a no-op, never negative.
func (b *CaptureBudget) Refund() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.turnCalls > 0 {
		b.turnCalls--
	}
	if len(b.hourCalls) > 0 {
		// The slot just charged is the newest entry (Charge
		// appends now last); dropping it restores the hour
		// count exactly. The counts are anonymous, so under
		// concurrent charges from another tool this still
		// returns exactly one slot, whichever entry it is.
		b.hourCalls = b.hourCalls[:len(b.hourCalls)-1]
	}
}

// BeginTurn resets the per-turn counter. Only the budget's owner
// calls this (see ToolDeps.CaptureBudgetOwner): a child
// registry's tools share the budget but never reset it.
func (b *CaptureBudget) BeginTurn() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.turnCalls = 0
}

// TurnCalls reports the calls charged this turn (tests and
// diagnostics only).
func (b *CaptureBudget) TurnCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.turnCalls
}

// HourCalls reports the calls charged in the last hour (tests
// and diagnostics only; entries older than an hour are pruned
// on the next charge, so this can over-count until then).
func (b *CaptureBudget) HourCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.hourCalls)
}
