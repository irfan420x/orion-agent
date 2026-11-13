// rc12 W2 regression tests for Audit 7 finding F-A7-2(a): late usage
// from an executor that ignores its context was discarded, so the
// tokens were never charged to any budget.
package subagent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestLateUsageFromIgnoringExecutorIsCharged is the port of Audit 7's
// probe TestAudit7LateUsageFromIgnoringExecutorIsLost, inverted into
// a positive test: an executor that ignores ctx, works 300 ms past a
// 20 ms deadline and returns 700 tokens must have those 700 tokens
// recorded on the entry, so a later GetResult (and the per-chat
// sweep, and session close) charges them.
//
// The prompt timeout itself is unchanged: Wait still returns at the
// deadline with zero usage and UsageSettled false. What changed is
// that the zero is provisional, not final.
func TestLateUsageFromIgnoringExecutorIsCharged(t *testing.T) {
	m := NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, Usage, error) {
		// Ignores ctx entirely: works 300 ms past the deadline.
		time.Sleep(320 * time.Millisecond)
		return "late", Usage{Tokens: 700}, nil
	})
	defer m.Shutdown()

	id, err := m.Spawn("slow", 20*time.Millisecond)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	start := time.Now()
	res, err := m.Wait(context.Background(), id)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Wait took %v, want prompt timeout (~20ms)", elapsed)
	}
	if !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Fatalf("Err = %v, want context.DeadlineExceeded", res.Err)
	}
	if st, _ := m.Status(id); st != StatusTimeout {
		t.Fatalf("Status = %q, want %q", st, StatusTimeout)
	}
	// The prompt result is still zero and unsettled: the executor
	// has not returned yet, so the usage is unknowable, not zero.
	if res.Usage.Tokens != 0 {
		t.Fatalf("prompt Usage.Tokens = %d, want 0 (late usage not yet arrived)", res.Usage.Tokens)
	}
	if res.UsageSettled {
		t.Fatalf("prompt UsageSettled = true, want false (executor still running past the deadline)")
	}

	// The executor returns ~300 ms after the deadline. The
	// late-arrival waiter must record its 700 tokens on the entry.
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, ok := m.GetResult(id)
		if ok && got.UsageSettled && got.Usage.Tokens == 700 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("late usage never recorded: GetResult = %+v, want 700 settled tokens", got)
		}
		time.Sleep(2 * time.Millisecond)
	}
	// Recording the late usage must not rewrite the terminal state:
	// the task stays timed out with the deadline error.
	if st, _ := m.Status(id); st != StatusTimeout {
		t.Fatalf("Status after late return = %q, want %q (late usage must not revive the task)", st, StatusTimeout)
	}
	got, ok := m.GetResult(id)
	if !ok {
		t.Fatalf("GetResult after late return: unknown task")
	}
	if !errors.Is(got.Err, context.DeadlineExceeded) {
		t.Fatalf("Err after late return = %v, want context.DeadlineExceeded", got.Err)
	}
	if got.Output != "" {
		t.Fatalf("Output after late return = %q, want empty (late output is discarded; only usage is recovered)", got.Output)
	}
}

// TestLateUsageFromCancelledExecutorIsCharged is the cancellation
// twin of the timeout test: Cancel wins the race the same way a
// deadline does, and the executor's late usage must be recorded
// just the same.
func TestLateUsageFromCancelledExecutorIsCharged(t *testing.T) {
	m := NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, Usage, error) {
		time.Sleep(300 * time.Millisecond) // ignores ctx entirely
		return "late", Usage{Tokens: 123}, nil
	})
	defer m.Shutdown()

	id, err := m.Spawn("work", time.Minute)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !m.Cancel(id) {
		t.Fatalf("Cancel(%s) = false, want true", id)
	}
	res, err := m.Wait(context.Background(), id)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("Err = %v, want context.Canceled", res.Err)
	}
	if res.UsageSettled {
		t.Fatalf("prompt UsageSettled = true, want false (executor still running past the cancel)")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, ok := m.GetResult(id)
		if ok && got.UsageSettled && got.Usage.Tokens == 123 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("late usage never recorded: GetResult = %+v, want 123 settled tokens", got)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if st, _ := m.Status(id); st != StatusFailed {
		t.Fatalf("Status after late return = %q, want %q", st, StatusFailed)
	}
}
