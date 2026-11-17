// Tests for the shared computer-use budget (F-A23W2-14 /
// F-M2R-16): one per-turn + per-hour counter across ground,
// screenshot and input, shared by parent and child registries.
package tools

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCaptureBudgetChargeAndReset(t *testing.T) {
	b := NewCaptureBudget()
	now := time.Now()
	// 15 charges succeed; the 16th hits the per-turn limit and
	// records nothing.
	for i := 0; i < captureMaxPerTurn; i++ {
		if err := b.Charge(now.Add(time.Duration(i)*time.Second), captureMaxPerTurn, captureMaxPerHour); err != nil {
			t.Fatalf("Charge #%d = %v, want nil", i+1, err)
		}
	}
	if err := b.Charge(now.Add(30*time.Second), captureMaxPerTurn, captureMaxPerHour); !errors.Is(err, errCaptureBudgetTurn) {
		t.Errorf("16th Charge = %v, want errCaptureBudgetTurn", err)
	}
	if got := b.TurnCalls(); got != captureMaxPerTurn {
		t.Errorf("TurnCalls after refusal = %d, want %d (a refusal must record nothing)", got, captureMaxPerTurn)
	}
	// BeginTurn resets the per-turn counter but not the hour's.
	b.BeginTurn()
	if got := b.TurnCalls(); got != 0 {
		t.Errorf("TurnCalls after BeginTurn = %d, want 0", got)
	}
	if got := b.HourCalls(); got != captureMaxPerTurn {
		t.Errorf("HourCalls after BeginTurn = %d, want %d", got, captureMaxPerTurn)
	}
	// The hour window slides: entries older than an hour are
	// pruned on the next charge, so an hour later the budget is
	// whole again.
	if err := b.Charge(now.Add(61*time.Minute), captureMaxPerTurn, captureMaxPerHour); err != nil {
		t.Errorf("Charge an hour later = %v, want nil (old entries pruned)", err)
	}
	if got := b.HourCalls(); got != 1 {
		t.Errorf("HourCalls after pruning = %d, want 1", got)
	}
}

func TestCaptureBudgetRefund(t *testing.T) {
	b := NewCaptureBudget()
	now := time.Now()
	// Refunding an empty budget is a no-op, never negative.
	b.Refund()
	if got := b.TurnCalls(); got != 0 {
		t.Errorf("TurnCalls after refund of empty budget = %d, want 0", got)
	}
	if got := b.HourCalls(); got != 0 {
		t.Errorf("HourCalls after refund of empty budget = %d, want 0", got)
	}
	// Charge 15 (the per-turn cap), refund one, and the next
	// call fits again — the refunded slot is spendable.
	for i := 0; i < captureMaxPerTurn; i++ {
		if err := b.Charge(now.Add(time.Duration(i)*time.Second), captureMaxPerTurn, captureMaxPerHour); err != nil {
			t.Fatalf("Charge #%d = %v, want nil", i+1, err)
		}
	}
	if err := b.Charge(now.Add(30*time.Second), captureMaxPerTurn, captureMaxPerHour); !errors.Is(err, errCaptureBudgetTurn) {
		t.Errorf("16th Charge = %v, want errCaptureBudgetTurn", err)
	}
	b.Refund()
	if got := b.TurnCalls(); got != captureMaxPerTurn-1 {
		t.Errorf("TurnCalls after refund = %d, want %d", got, captureMaxPerTurn-1)
	}
	if got := b.HourCalls(); got != captureMaxPerTurn-1 {
		t.Errorf("HourCalls after refund = %d, want %d", got, captureMaxPerTurn-1)
	}
	if err := b.Charge(now.Add(31*time.Second), captureMaxPerTurn, captureMaxPerHour); err != nil {
		t.Errorf("Charge after refund = %v, want nil (the refunded slot is spendable)", err)
	}
}

func TestCaptureBudgetHourCap(t *testing.T) {
	b := NewCaptureBudget()
	now := time.Now()
	// Fill the hour without hitting the per-turn cap: reset the
	// turn every 15 charges.
	for i := 0; i < captureMaxPerHour; i++ {
		if i > 0 && i%captureMaxPerTurn == 0 {
			b.BeginTurn()
		}
		if err := b.Charge(now.Add(time.Duration(i)*time.Second), captureMaxPerTurn, captureMaxPerHour); err != nil {
			t.Fatalf("Charge #%d = %v, want nil", i+1, err)
		}
	}
	// A new turn, 2h later: the window has slid past every entry.
	b.BeginTurn()
	if err := b.Charge(now.Add(2*time.Hour), captureMaxPerTurn, captureMaxPerHour); err != nil {
		t.Errorf("Charge 2h later = %v, want nil", err)
	}
	// Refill, then the 121st inside the hour hits the hour cap.
	b2 := NewCaptureBudget()
	for i := 0; i < captureMaxPerHour; i++ {
		if i > 0 && i%captureMaxPerTurn == 0 {
			b2.BeginTurn()
		}
		if err := b2.Charge(now.Add(time.Duration(i)*time.Second), captureMaxPerTurn, captureMaxPerHour); err != nil {
			t.Fatalf("Charge #%d = %v, want nil", i+1, err)
		}
	}
	b2.BeginTurn() // the hour cap, not the turn cap, is under test
	if err := b2.Charge(now.Add(30*time.Minute), captureMaxPerTurn, captureMaxPerHour); !errors.Is(err, errCaptureBudgetHour) {
		t.Errorf("121st Charge in an hour = %v, want errCaptureBudgetHour", err)
	}
}

// TestCaptureBudgetSharedAcrossTools: ground, screenshot and
// input draw on ONE 15/turn pool — 10 groundings + 5 screenshots
// leave no room for an input call, and the refusal names the
// caller's own tool.
func TestCaptureBudgetSharedAcrossTools(t *testing.T) {
	budget := NewCaptureBudget()
	clock := time.Now()
	tick := func() time.Time { clock = clock.Add(2 * time.Second); return clock }

	ground := newGroundToolWithBudget(budget, true)
	ground.now = func() time.Time { return clock }
	shot := newScreenshotToolWithBudget(budget, true)
	shot.now = func() time.Time { return clock }
	input := newInputToolWithBudget(budget, true)
	input.now = func() time.Time { return clock }

	for i := 0; i < 10; i++ {
		tick()
		if err := ground.checkRateLimit(); err != nil {
			t.Fatalf("ground checkRateLimit #%d = %v, want nil", i+1, err)
		}
	}
	for i := 0; i < 5; i++ {
		tick()
		if err := shot.checkRateLimit(true); err != nil {
			t.Fatalf("screenshot checkRateLimit #%d = %v, want nil", i+1, err)
		}
	}
	if got := budget.TurnCalls(); got != 15 {
		t.Fatalf("TurnCalls after 10 ground + 5 screenshots = %d, want 15", got)
	}
	// The pool is spent: an input call is refused even though
	// input itself charged nothing yet.
	tick()
	if err := input.checkRateLimit(); err == nil || !strings.Contains(err.Error(), "at most 15 input calls per turn") {
		t.Errorf("input checkRateLimit on a spent pool = %v, want the per-turn limit", err)
	}
	// And a 16th grounding is refused in ground's own words.
	tick()
	if err := ground.checkRateLimit(); err == nil || !strings.Contains(err.Error(), "at most 15 groundings per turn") {
		t.Errorf("16th grounding = %v, want the per-turn limit", err)
	}
	// One owner's BeginTurn resets the pool for every tool.
	shot.BeginTurn()
	tick()
	if err := input.checkRateLimit(); err != nil {
		t.Errorf("input checkRateLimit after BeginTurn = %v, want nil", err)
	}
	if got := budget.TurnCalls(); got != 1 {
		t.Errorf("TurnCalls after reset + 1 input = %d, want 1", got)
	}
}

// TestCaptureBudgetParentAndChildShare: a child registry charges
// the parent's budget and cannot reset it — only the parent
// registry's BeginTurn resets.
func TestCaptureBudgetParentAndChildShare(t *testing.T) {
	budget := NewCaptureBudget()
	clock := time.Now()

	parentReg, err := DefaultRegistryWithDeps(ToolDeps{CaptureBudget: budget, CaptureBudgetOwner: true})
	if err != nil {
		t.Fatalf("parent DefaultRegistryWithDeps: %v", err)
	}
	childReg, err := DefaultRegistryWithDeps(ToolDeps{CaptureBudget: budget, CaptureBudgetOwner: false})
	if err != nil {
		t.Fatalf("child DefaultRegistryWithDeps: %v", err)
	}
	parentGroundRaw, ok := parentReg.Get("ground")
	if !ok {
		t.Fatalf("parent registry has no ground tool")
	}
	parentGround, ok := parentGroundRaw.(*groundTool)
	if !ok {
		t.Fatalf("parent registry ground tool is %T, want *groundTool", parentGroundRaw)
	}
	childShotRaw, ok := childReg.Get("screenshot")
	if !ok {
		t.Fatalf("child registry has no screenshot tool")
	}
	childShot, ok := childShotRaw.(*screenshotTool)
	if !ok {
		t.Fatalf("child registry screenshot tool is %T, want *screenshotTool", childShotRaw)
	}
	childInputRaw, ok := childReg.Get("input")
	if !ok {
		t.Fatalf("child registry has no input tool")
	}
	childInput, ok := childInputRaw.(*inputTool)
	if !ok {
		t.Fatalf("child registry input tool is %T, want *inputTool", childInputRaw)
	}
	parentGround.now = func() time.Time { return clock }
	childShot.now = func() time.Time { return clock }
	childInput.now = func() time.Time { return clock }

	// The parent spends the whole pool on groundings.
	for i := 0; i < captureMaxPerTurn; i++ {
		clock = clock.Add(2 * time.Second)
		if err := parentGround.checkRateLimit(); err != nil {
			t.Fatalf("parent ground checkRateLimit #%d = %v, want nil", i+1, err)
		}
	}
	// The child gets nothing: no fresh 15/turn of its own.
	clock = clock.Add(2 * time.Second)
	if err := childShot.checkRateLimit(true); err == nil || !strings.Contains(err.Error(), "at most 15 captures per turn") {
		t.Errorf("child screenshot on a spent pool = %v, want the per-turn limit", err)
	}
	clock = clock.Add(2 * time.Second)
	if err := childInput.checkRateLimit(); err == nil || !strings.Contains(err.Error(), "at most 15 input calls per turn") {
		t.Errorf("child input on a spent pool = %v, want the per-turn limit", err)
	}
	// The child's BeginTurn must NOT reset the parent's budget:
	// a child turn starting mid-parent-turn cannot mint a fresh
	// pool. (This is the reset-ownership rule.)
	childReg.BeginTurn()
	clock = clock.Add(2 * time.Second)
	if err := childShot.checkRateLimit(true); err == nil {
		t.Errorf("child screenshot after child BeginTurn = nil, want the per-turn limit (child must not reset the shared budget)")
	}
	if got := budget.TurnCalls(); got != captureMaxPerTurn {
		t.Errorf("TurnCalls after child BeginTurn = %d, want %d", got, captureMaxPerTurn)
	}
	// The parent's BeginTurn resets the pool for parent and child.
	parentReg.BeginTurn()
	clock = clock.Add(2 * time.Second)
	if err := childShot.checkRateLimit(true); err != nil {
		t.Errorf("child screenshot after parent BeginTurn = %v, want nil", err)
	}
}

// TestCaptureBudgetGroundDoesNotDoubleCharge: one grounding is
// one capture — the embedded screenshot must not charge the
// shared budget a second time. (Pre-fix behaviour: a grounding
// consumed one ground slot AND one screenshot slot.)
func TestCaptureBudgetGroundDoesNotDoubleCharge(t *testing.T) {
	resetGroundSnapshots()
	env := newFakeGroundEnv(t)
	for i := 0; i < captureMaxPerTurn; i++ {
		if i > 0 {
			env.advance()
		}
		env.call(t, `{}`)
	}
	if got := env.tool.budget.TurnCalls(); got != captureMaxPerTurn {
		t.Errorf("TurnCalls after %d groundings = %d, want %d (one charge per grounding, not two)", captureMaxPerTurn, got, captureMaxPerTurn)
	}
	if got := env.captures; got != captureMaxPerTurn {
		t.Errorf("spectacle captures = %d, want %d", got, captureMaxPerTurn)
	}
}

// TestGroundEmbeddedScreenshotSurvivesTurnReset (F-M2R-1, fixed
// as a side effect of the shared budget): the embedded
// screenshot's counter used to never reset — its BeginTurn was
// never called, it is not a registry tool — so ground died
// permanently after 15 groundings across all turns. With the
// shared budget, ground's BeginTurn resets it: 15 groundings,
// a new turn, 15 more groundings, all succeed.
func TestGroundEmbeddedScreenshotSurvivesTurnReset(t *testing.T) {
	resetGroundSnapshots()
	env := newFakeGroundEnv(t)
	for i := 0; i < captureMaxPerTurn; i++ {
		if i > 0 {
			env.advance()
		}
		env.call(t, `{}`)
	}
	env.tool.BeginTurn()
	for i := 0; i < captureMaxPerTurn; i++ {
		env.advance()
		env.call(t, `{}`)
	}
	if got := env.captures; got != 2*captureMaxPerTurn {
		t.Errorf("spectacle captures = %d, want %d (ground must survive a turn reset)", got, 2*captureMaxPerTurn)
	}
}
