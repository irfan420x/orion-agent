// W3 (rc11) regression tests for Audit 6 F-A6-P2-8 (Resolve/timer
// race). The fix closes the race by construction: Resolve, the
// timer branch and the cancellation branch all settle a pending
// request through one mutex-guarded transition, so exactly one of
// them wins. These tests pin the observable invariant — a Resolve
// that returns true is always honoured by Check, and a Resolve
// that returns false always corresponds to a Check that denied —
// instead of probing a microsecond window.
package permissions

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestW3BResolveTimerConsistency races Resolve against a 1ms
// approval window, many times, and asserts the two sides never
// disagree: whenever Resolve reports success, Check must have
// approved; whenever Resolve reports the request gone, Check must
// have denied by timeout. The old code could return true from
// Resolve (the callback handler then told the user "Approved")
// while Check denied on the timer — the two outcomes were
// decided by independent interleavings, not one transition.
// Both sides are exercised deterministically: even iterations race
// Resolve against the timer, odd iterations sleep past the 1ms
// window first so the timer wins by construction. (Relying on
// scheduling luck for the split made this test flaky: under -race
// Resolve won 300/300. The timer side is also covered on its own
// by TestW3BResolveAfterExpiryReturnsFalse.)
func TestW3BResolveTimerConsistency(t *testing.T) {
	const iterations = 300
	var resolveWins, timerWins int
	var expireHooks atomic.Int32
	for i := 0; i < iterations; i++ {
		g := NewGate(NewRuleStore(""), func(ctx context.Context, req ApprovalRequest) error {
			return nil
		})
		g.SetTimeout(1 * time.Millisecond)
		g.SetOnExpire(func(req ApprovalRequest) { expireHooks.Add(1) })
		resultCh := make(chan bool, 1)
		go func() {
			ok, _ := g.Check(ctxWithChat(t), "shell_exec", `{"command":"echo hi"}`)
			resultCh <- ok
		}()
		if i%2 == 1 {
			// Timer-win iteration: let the 1ms window elapse
			// before looking for the request, so the timer
			// settles first and the poll loop below observes
			// Check's denial.
			time.Sleep(5 * time.Millisecond)
		}
		// Resolve as soon as the request is pending: the race
		// is Resolve vs the 1ms timer, so resolving promptly
		// (not after a sleep) is what exercises it. The poll
		// must also stop as soon as Check has returned: with
		// a 1ms window the timer can fire before this goroutine
		// is scheduled, and polling for a request that no
		// longer exists would otherwise burn the whole 5s
		// deadline on every missed window (that is what made
		// this test take 155s before the early exit — a timer
		// win observed in ~1ms was paid for with a 5s poll).
		var resolved bool
		var done *bool
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if g.PendingCount() > 0 {
				break
			}
			select {
			case ok := <-resultCh:
				done = &ok
			default:
			}
			if done != nil {
				break
			}
			time.Sleep(10 * time.Microsecond)
		}
		if done != nil {
			// The timer won before Resolve could run: Check
			// already denied, and there is nothing to resolve.
			resolved = false
			timerWins++
			if g.PendingCount() != 0 {
				t.Fatalf("iteration %d: %d pending request(s) leaked", i, g.PendingCount())
			}
			continue
		}
		// Find the pending request's ID without knowing it: the
		// gate has exactly one pending request here.
		// (PendingCount is 1; resolve via the stored request by
		// re-reading through a second Check is not possible, so
		// the ID is taken from the gate's own pending map under
		// its mutex — this test is in-package.)
		g.mu.Lock()
		var id string
		for pid := range g.pending {
			id = pid
			break
		}
		g.mu.Unlock()
		if id == "" {
			t.Fatalf("iteration %d: request was pending but vanished before Resolve (timer fired in between — the poll loop should have caught the result)", i)
		}
		resolved = g.Resolve(id, true, nil)
		select {
		case ok := <-resultCh:
			switch {
			case resolved && !ok:
				t.Fatalf("iteration %d: Resolve returned true but Check denied (the approved call was denied)", i)
			case !resolved && ok:
				t.Fatalf("iteration %d: Resolve returned false but Check approved (a denied/expired call ran)", i)
			case resolved && ok:
				resolveWins++
			default:
				timerWins++
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: Check did not return", i)
		}
		if g.PendingCount() != 0 {
			t.Fatalf("iteration %d: %d pending request(s) leaked", i, g.PendingCount())
		}
	}
	t.Logf("Resolve wins: %d, timer wins: %d, expire hooks: %d", resolveWins, timerWins, expireHooks.Load())
	if resolveWins == 0 {
		t.Error("Resolve never won the race in 300 iterations — the test did not exercise the Resolve side")
	}
	if timerWins == 0 {
		t.Error("the timer never won the race in 300 iterations — the test did not exercise the expiry side")
	}
	if got := expireHooks.Load(); int(got) != timerWins {
		t.Errorf("OnExpire fired %d times, want %d (exactly the timer wins — never on a Resolve win)", got, timerWins)
	}
}

// TestW3BResolveAfterExpiryReturnsFalse pins the other half of
// the single-winner contract: once the window has elapsed and
// Check has denied, a late tap resolves nothing — Resolve
// returns false, so the callback handler reports the request
// expired instead of acting on a dead approval.
func TestW3BResolveAfterExpiryReturnsFalse(t *testing.T) {
	g := NewGate(NewRuleStore(""), func(ctx context.Context, req ApprovalRequest) error {
		return nil
	})
	g.SetTimeout(20 * time.Millisecond)
	expired := make(chan struct{})
	g.SetOnExpire(func(req ApprovalRequest) { close(expired) })
	resultCh := make(chan bool, 1)
	go func() {
		ok, _ := g.Check(ctxWithChat(t), "shell_exec", `{"command":"echo hi"}`)
		resultCh <- ok
	}()
	// Wait until the request is pending, then let the window elapse.
	// (Same early exit as the consistency test: if Check returns
	// before pending is ever observed, stop polling instead of
	// burning the 5s deadline on a request that no longer exists.)
	deadline := time.Now().Add(5 * time.Second)
	var id string
	for time.Now().Before(deadline) {
		g.mu.Lock()
		for pid := range g.pending {
			id = pid
		}
		g.mu.Unlock()
		if id != "" {
			break
		}
		select {
		case ok := <-resultCh:
			t.Fatalf("Check returned (approved=%v) before the request was observed pending — the 20ms window elapsed before the test goroutine was scheduled", ok)
		default:
		}
		time.Sleep(10 * time.Microsecond)
	}
	if id == "" {
		t.Fatal("request never became pending")
	}
	select {
	case ok := <-resultCh:
		if ok {
			t.Fatal("Check approved without an answer")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Check did not return after the timeout")
	}
	select {
	case <-expired:
	default:
		t.Error("OnExpire did not fire on timeout")
	}
	if g.Resolve(id, true, nil) {
		t.Error("Resolve after expiry returned true — a late tap must not resolve a dead request")
	}
}

// TestW3BSettleIsSingleWinner is a direct unit check of the
// settle transition: the first settler wins, every later settler
// (Resolve, timer or cancel) loses, and the recorded outcome is
// the winner's.
func TestW3BSettleIsSingleWinner(t *testing.T) {
	g := NewGate(NewRuleStore(""), func(ctx context.Context, req ApprovalRequest) error {
		return nil
	})
	pr := &pendingRequest{req: ApprovalRequest{ID: "x"}, ch: make(chan pendingResult, 1)}
	g.mu.Lock()
	g.pending["x"] = pr
	g.mu.Unlock()
	res := pendingResult{approved: true}
	if !g.settle(pr, &res) {
		t.Fatal("first settle lost, want win")
	}
	if g.settle(pr, nil) {
		t.Error("second settle won, want loss (exactly one winner)")
	}
	if g.settle(pr, &pendingResult{approved: false}) {
		t.Error("third settle won, want loss")
	}
	got, ok := g.resolvedOutcome(pr)
	if !ok || !got.approved {
		t.Errorf("resolvedOutcome = %+v, %v, want the winner's approval", got, ok)
	}
	// Concurrent settlers: exactly one of N wins.
	var wins atomic.Int32
	var wg sync.WaitGroup
	pr2 := &pendingRequest{req: ApprovalRequest{ID: "y"}, ch: make(chan pendingResult, 1)}
	g.mu.Lock()
	g.pending["y"] = pr2
	g.mu.Unlock()
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if g.settle(pr2, nil) {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Errorf("concurrent settle winners = %d, want exactly 1", wins.Load())
	}
}
