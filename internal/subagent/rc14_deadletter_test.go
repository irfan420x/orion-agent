// rc14 regression tests for Audit 9 F-A9-P2-8: the dead-letter
// ledger folded evicted tokens unconditionally, so a task charged
// via GetResult/sweep and later evicted was counted twice
// (charged + ledger), there was no consumer of the ledger, and
// nothing was logged on eviction. The ledger is now uncharged-only
// (taskEntry.charged, set by MarkCharged), evictions are logged,
// and DrainDeadLetter is the consuming read.
package subagent

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// rc14ChargeListed charges every listed task the way a consumer
// does: read the settled result via GetResult, then mark it
// charged. It returns the total tokens charged.
func rc14ChargeListed(t *testing.T, m *Manager) int {
	t.Helper()
	total := 0
	for _, ts := range m.List() {
		res, ok := m.GetResult(ts.ID)
		if !ok {
			t.Fatalf("List() contains %s but GetResult(%s) fails", ts.ID, ts.ID)
		}
		if !res.UsageSettled {
			t.Fatalf("task %s usage unsettled, want settled before charging", ts.ID)
		}
		total += res.Usage.Tokens
		if !m.MarkCharged(ts.ID) {
			t.Fatalf("MarkCharged(%s) = false, want true (first charge of a settled task)", ts.ID)
		}
		if m.MarkCharged(ts.ID) {
			t.Fatalf("MarkCharged(%s) = true on second call, want false (already charged)", ts.ID)
		}
	}
	return total
}

// TestRC14DeadLetterChargedPlusLedgerEqualsTotal is the audit's
// reproduction shape: 1005 sequential tasks x 100 tokens, no
// fetch/sweep. The 5 evicted tasks land in the ledger (500);
// charging all 1000 survivors must leave the ledger at 500, so
// charged (100,000) + ledger (500) == total real tokens (100,500)
// — no loss, no double-count. On unmodified rc13 code the second
// half is unobservable (nothing marks charged), and any consumer
// that charged the survivors and added the ledger would read
// 100,500 charged + 500 ledger against 100,500 real tokens.
func TestRC14DeadLetterChargedPlusLedgerEqualsTotal(t *testing.T) {
	m := NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, Usage, error) {
		return "ok", Usage{Tokens: 100}, nil
	})
	defer m.Shutdown()

	const extra = 5
	totalTasks := maxEntries + extra
	for i := 0; i < totalTasks; i++ {
		rc13w3Spawn(t, m, fmt.Sprintf("task-%d", i), 0)
	}
	m.WaitAll(context.Background())

	if got := m.DeadLetterTasks(); got != extra {
		t.Fatalf("DeadLetterTasks() = %d, want %d", got, extra)
	}
	if got := m.DeadLetterTokens(); got != extra*100 {
		t.Fatalf("DeadLetterTokens() = %d, want %d (evicted, never charged)", got, extra*100)
	}

	charged := rc14ChargeListed(t, m)
	if charged != maxEntries*100 {
		t.Fatalf("charged survivors = %d, want %d", charged, maxEntries*100)
	}
	if got := m.DeadLetterTokens(); got != extra*100 {
		t.Fatalf("DeadLetterTokens() after charging survivors = %d, want %d (charging survivors must not move the ledger)", got, extra*100)
	}
	if charged+m.DeadLetterTokens() != totalTasks*100 {
		t.Fatalf("charged (%d) + ledger (%d) = %d, want %d total real tokens",
			charged, m.DeadLetterTokens(), charged+m.DeadLetterTokens(), totalTasks*100)
	}
}

// TestRC14DeadLetterChargedTaskEvictedContributesZero is the
// double-count case: charge a task, then flood the registry past
// the cap so it is evicted. The ledger must NOT include its
// tokens — it still counts in DeadLetterTasks.
func TestRC14DeadLetterChargedTaskEvictedContributesZero(t *testing.T) {
	m := NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, Usage, error) {
		return "ok", Usage{Tokens: 100}, nil
	})
	defer m.Shutdown()

	target := rc13w3Spawn(t, m, "target", 0)
	if _, err := m.Wait(context.Background(), target); err != nil {
		t.Fatalf("Wait(target): %v", err)
	}
	res, ok := m.GetResult(target)
	if !ok || !res.UsageSettled || res.Usage.Tokens != 100 {
		t.Fatalf("GetResult(target) = %+v, ok=%v; want settled 100 tokens", res, ok)
	}
	if !m.MarkCharged(target) {
		t.Fatalf("MarkCharged(target) = false, want true")
	}

	// Flood past the cap: the target is the oldest settled entry,
	// so it is the first eviction.
	for i := 0; i < maxEntries; i++ {
		rc13w3Spawn(t, m, fmt.Sprintf("filler-%d", i), 0)
	}
	m.WaitAll(context.Background())

	if _, ok := m.GetResult(target); ok {
		t.Fatalf("GetResult(target) still succeeds after the flood: want the target evicted")
	}
	if got := m.DeadLetterTasks(); got != 1 {
		t.Fatalf("DeadLetterTasks() = %d, want 1 (only the charged target evicted)", got)
	}
	if got := m.DeadLetterTokens(); got != 0 {
		t.Fatalf("DeadLetterTokens() = %d, want 0: the charged target's 100 tokens must not enter the ledger", got)
	}
}

// TestRC14DeadLetterLateUsageLandsExactlyOnce covers the
// last-resort eviction path: an unsettled task evicted while every
// finished entry is unsettled, whose late usage arrives afterwards,
// must land in the ledger exactly once — it was never chargeable
// before eviction, so the fold is exact, not a double-count.
func TestRC14DeadLetterLateUsageLandsExactlyOnce(t *testing.T) {
	release := make(chan struct{})
	m := NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, Usage, error) {
		// Ignores ctx entirely: blocks until released, then
		// reports its tokens (700 for the slow task, 100 for
		// fillers).
		<-release
		if brief == "slow" {
			return "late", Usage{Tokens: 700}, nil
		}
		return "late", Usage{Tokens: 100}, nil
	})
	defer m.Shutdown()

	slowID := rc13w3Spawn(t, m, "slow", 50*time.Millisecond)
	if _, err := m.Wait(context.Background(), slowID); err != nil {
		t.Fatalf("Wait(slow): %v", err)
	}
	if res, ok := m.GetResult(slowID); !ok || res.UsageSettled {
		t.Fatalf("GetResult(slow) = %+v, ok=%v; want unsettled (usage not final yet)", res, ok)
	}
	if m.MarkCharged(slowID) {
		t.Fatalf("MarkCharged(slow) = true on an unsettled task, want false (only settled usage can be charged)")
	}

	// Flood the registry past the cap with tasks that are also
	// unsettled (their executors are parked on release and time
	// out at 50 ms). The slow task is the oldest finished
	// unsettled entry, so last-resort eviction takes it first —
	// and only it, since one eviction restores the cap.
	for i := 0; i < maxEntries; i++ {
		rc13w3Spawn(t, m, fmt.Sprintf("filler-%d", i), 50*time.Millisecond)
	}
	if _, ok := m.GetResult(slowID); ok {
		t.Fatalf("GetResult(slow) still succeeds after the flood: want the slow task evicted as last resort")
	}
	if got := m.DeadLetterTasks(); got != 1 {
		t.Fatalf("DeadLetterTasks() = %d, want 1 (only the slow task evicted)", got)
	}

	// Now let every executor return its late usage.
	close(release)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if m.DeadLetterTokens() == 700 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("DeadLetterTokens() = %d, want 700: late usage of the evicted unsettled task was lost", m.DeadLetterTokens())
		}
		time.Sleep(2 * time.Millisecond)
	}
	// Exactly once: the late-arrival waiter folds once, and no
	// other path touches the ledger afterwards.
	time.Sleep(200 * time.Millisecond)
	if got := m.DeadLetterTokens(); got != 700 {
		t.Fatalf("DeadLetterTokens() = %d after settling, want 700 (late usage folded more than once)", got)
	}

	// Full accounting: every filler token (on its entry) plus the
	// late 700 (in the ledger) must be recoverable.
	for {
		if rc13w3SumListedTokens(t, m) == maxEntries*100 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("listed tokens = %d, want %d (fillers' late usage not recorded)",
				rc13w3SumListedTokens(t, m), maxEntries*100)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if total := rc13w3SumListedTokens(t, m) + m.DeadLetterTokens(); total != maxEntries*100+700 {
		t.Fatalf("listed + ledger = %d, want %d", total, maxEntries*100+700)
	}
}

// TestRC14DrainDeadLetter is the ledger's consumer contract: drain
// hands the uncharged total out once and resets the ledger, so a
// second drain returns 0 until further evictions occur.
func TestRC14DrainDeadLetter(t *testing.T) {
	m := NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, Usage, error) {
		return "ok", Usage{Tokens: 100}, nil
	})
	defer m.Shutdown()

	const extra = 3
	for i := 0; i < maxEntries+extra; i++ {
		rc13w3Spawn(t, m, fmt.Sprintf("task-%d", i), 0)
	}
	m.WaitAll(context.Background())
	if got := m.DeadLetterTokens(); got != extra*100 {
		t.Fatalf("DeadLetterTokens() = %d, want %d before drain", got, extra*100)
	}

	if got := m.DrainDeadLetter(); got != extra*100 {
		t.Fatalf("DrainDeadLetter() = %d, want %d", got, extra*100)
	}
	if got := m.DeadLetterTokens(); got != 0 {
		t.Fatalf("DeadLetterTokens() after drain = %d, want 0", got)
	}
	if got := m.DeadLetterTasks(); got != 0 {
		t.Fatalf("DeadLetterTasks() after drain = %d, want 0", got)
	}
	if got := m.DrainDeadLetter(); got != 0 {
		t.Fatalf("second DrainDeadLetter() = %d, want 0 (ledger already drained)", got)
	}
}

// TestRC14MarkChargedSemantics pins the flag's contract: unknown
// and unfinished tasks never mark, a settled task marks exactly
// once, and GetResult alone (a read, also used by non-charging
// readers) does not mark.
func TestRC14MarkChargedSemantics(t *testing.T) {
	m := NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, Usage, error) {
		return "ok", Usage{Tokens: 100}, nil
	})
	defer m.Shutdown()

	if m.MarkCharged("no-such-task") {
		t.Fatalf("MarkCharged(unknown) = true, want false")
	}
	id := rc13w3Spawn(t, m, "task", 0)
	if _, err := m.Wait(context.Background(), id); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	// A GetResult read must not mark: the task is still uncharged
	// afterwards, and an eviction would still fold its tokens.
	if _, ok := m.GetResult(id); !ok {
		t.Fatalf("GetResult(%s) = not found, want the finished result", id)
	}
	if !m.MarkCharged(id) {
		t.Fatalf("MarkCharged after a GetResult read = false, want true (the read must not have marked it)")
	}
	if m.MarkCharged(id) {
		t.Fatalf("second MarkCharged = true, want false")
	}
}
