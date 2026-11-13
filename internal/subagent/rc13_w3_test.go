// rc13 W3 regression tests for Audit 8 findings F-A8-P2-1 (registry
// eviction silently voids token accounting) and the late-usage leg of
// the same finding.
//
// Negative control: these tests were written FIRST and run against
// the unmodified rc12 code, where they FAIL (see the report for the
// fail-before output). Only then was the fix implemented.
package subagent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// rc13w3Spawn spawns a task, retrying through transient "at capacity"
// rejections (the spawn loop can outpace the 4-slot semaphore when
// the executor is instant). Any other error fails the test.
func rc13w3Spawn(t *testing.T, m *Manager, brief string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		id, err := m.Spawn(brief, timeout)
		if err == nil {
			return id
		}
		if !strings.Contains(err.Error(), "at capacity") {
			t.Fatalf("Spawn(%q): %v", brief, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("Spawn(%q) still at capacity after 60s: %v", brief, err)
		}
		time.Sleep(time.Millisecond)
	}
}

// rc13w3SumListedTokens sums Usage.Tokens over every task visible in
// List() via GetResult. On unmodified rc12 code this loses evicted
// tasks' tokens; with the fix the recoverable total is this sum plus
// DeadLetterTokens().
func rc13w3SumListedTokens(t *testing.T, m *Manager) int {
	t.Helper()
	total := 0
	for _, ts := range m.List() {
		res, ok := m.GetResult(ts.ID)
		if !ok {
			t.Fatalf("List() contains %s but GetResult(%s) fails", ts.ID, ts.ID)
		}
		total += res.Usage.Tokens
	}
	return total
}

// TestRC13W3EvictionPreservesTokenAccounting is the Audit 8
// F-A8-P2-1(a) reproduction, scaled to the current cap: spawn
// maxEntries+100 instant tasks at 100 tokens each (11,000 tokens at
// the old cap of 100; maxEntries+100 x 100 at any cap), WaitAll, then
// sum GetResult over List(). Every spawned token must still be
// recoverable — from the registry for live entries, plus the
// dead-letter ledger for evicted ones. On unmodified rc12 code the
// List() sum alone is short by exactly the evicted tasks' tokens
// (1,000 of 11,000 at the old cap).
func TestRC13W3EvictionPreservesTokenAccounting(t *testing.T) {
	m := NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, Usage, error) {
		return "ok", Usage{Tokens: 100}, nil
	})
	defer m.Shutdown()

	const extra = 100
	totalTasks := maxEntries + extra
	for i := 0; i < totalTasks; i++ {
		rc13w3Spawn(t, m, fmt.Sprintf("task-%d", i), 0)
	}
	// WaitAll waits for the tasks still in the registry. It cannot
	// return the evicted 100: they left the registry at spawn time
	// (their results live on only in the dead-letter ledger), so
	// its length is the registry size, not the spawned count — the
	// token total below is the assertion that matters.
	m.WaitAll(context.Background())
	if got := len(m.List()); got > maxEntries {
		t.Fatalf("registry holds %d tasks, want at most %d", got, maxEntries)
	}

	wantTotal := totalTasks * 100
	recoverable := rc13w3SumListedTokens(t, m) + m.DeadLetterTokens()
	if recoverable != wantTotal {
		t.Fatalf("recoverable tokens = %d (listed %d + dead-letter %d), want %d: %d tasks' tokens lost to eviction",
			recoverable, rc13w3SumListedTokens(t, m), m.DeadLetterTokens(), wantTotal, (wantTotal-recoverable)/100)
	}
	if got := m.DeadLetterTasks(); got != extra {
		t.Fatalf("DeadLetterTasks() = %d, want %d (exactly the evicted tasks)", got, extra)
	}
}

// TestRC13W3LateUsageSurvivesRegistryFlooding is the Audit 8
// F-A8-P2-1(b) reproduction: a task times out while its executor is
// still running (its usage is unsettled), the registry is then
// flooded past the cap, and only afterwards does the executor
// return its 700 late tokens. The timed-out task must survive the
// flood — eviction must skip unsettled finished tasks while any
// settled finished task can be evicted instead — so GetResult still
// finds it and reports the settled 700 tokens. On unmodified rc12
// code the timed-out task is the oldest finished entry, is evicted
// by the flood, and GetResult returns not-found: the late 700
// tokens are unrecoverable.
func TestRC13W3LateUsageSurvivesRegistryFlooding(t *testing.T) {
	release := make(chan struct{})
	m := NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, Usage, error) {
		if brief == "slow" {
			// Ignores ctx entirely: blocks until the test
			// releases it, then reports 700 tokens.
			<-release
			return "late", Usage{Tokens: 700}, nil
		}
		return "ok", Usage{Tokens: 100}, nil
	})
	defer m.Shutdown()

	slowID := rc13w3Spawn(t, m, "slow", 50*time.Millisecond)
	// Wait for the timeout to be recorded (prompt, ~50 ms).
	if _, err := m.Wait(context.Background(), slowID); err != nil {
		t.Fatalf("Wait(slow): %v", err)
	}
	if st, _ := m.Status(slowID); st != StatusTimeout {
		t.Fatalf("Status(slow) = %q, want %q", st, StatusTimeout)
	}
	if res, ok := m.GetResult(slowID); !ok || res.UsageSettled {
		t.Fatalf("GetResult(slow) before late return = %+v, ok=%v; want unsettled (usage not final yet)", res, ok)
	}

	// Flood the registry past the cap with settled filler tasks.
	// The slow task is unsettled throughout the flood (release is
	// still closed only afterwards), so eviction must skip it.
	const extra = 10
	for i := 0; i < maxEntries+extra; i++ {
		rc13w3Spawn(t, m, fmt.Sprintf("filler-%d", i), 0)
	}
	m.WaitAll(context.Background())
	if got := len(m.List()); got > maxEntries {
		t.Fatalf("registry holds %d tasks, want at most %d", got, maxEntries)
	}

	// Now let the slow executor return its 700 late tokens.
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, ok := m.GetResult(slowID)
		if ok && got.UsageSettled && got.Usage.Tokens == 700 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("late usage never recoverable: GetResult(slow) = %+v, ok=%v, want 700 settled tokens (task evicted by the flood, or late usage lost)", got, ok)
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Full accounting: every filler token plus the late 700 must be
	// recoverable from the registry plus the dead-letter ledger.
	wantTotal := (maxEntries+extra)*100 + 700
	recoverable := rc13w3SumListedTokens(t, m) + m.DeadLetterTokens()
	if recoverable != wantTotal {
		t.Fatalf("recoverable tokens = %d, want %d (flood + late 700)", recoverable, wantTotal)
	}
}

// TestRC13W3ForgottenUnsettledTaskLateUsageIsNotLost covers the
// last-resort eviction path through Forget: a timed-out task whose
// entry leaves the registry while its usage is still unsettled
// (evicted as a last resort when every finished entry is unsettled,
// or explicitly forgotten) must still have its late usage
// recovered — the late-arrival waiter folds it into the dead-letter
// ledger instead of writing it onto a detached entry object where
// no reader could ever see it.
func TestRC13W3ForgottenUnsettledTaskLateUsageIsNotLost(t *testing.T) {
	release := make(chan struct{})
	m := NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, Usage, error) {
		<-release // ignores ctx entirely
		return "late", Usage{Tokens: 700}, nil
	})
	defer m.Shutdown()

	id := rc13w3Spawn(t, m, "slow", 50*time.Millisecond)
	if _, err := m.Wait(context.Background(), id); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res, ok := m.GetResult(id); !ok || res.UsageSettled {
		t.Fatalf("GetResult before forget = %+v, ok=%v; want unsettled", res, ok)
	}
	if !m.Forget(id) {
		t.Fatalf("Forget(%s) = false, want true (finished task)", id)
	}
	if _, ok := m.GetResult(id); ok {
		t.Fatalf("GetResult after Forget: want not-found")
	}

	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if m.DeadLetterTokens() == 700 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("DeadLetterTokens() = %d, want 700: late usage of a forgotten unsettled task was lost", m.DeadLetterTokens())
		}
		time.Sleep(2 * time.Millisecond)
	}
}
