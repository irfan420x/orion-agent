package subagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRC23W3PersistenceRoundTrip (F-A17-12): finished tasks and
// the dead-letter ledger survive a Manager restart through the
// state file — results, usage, settled/charged flags and owning
// chat are all recovered, and a drain after reload still hands
// the ledger out exactly once.
func TestRC23W3PersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	exec := func(ctx context.Context, brief string) (string, Usage, error) {
		return "output for " + brief, Usage{Tokens: 42}, nil
	}
	m1 := NewManagerWithExecutorAndStore(4, exec, dir)
	id1, err := m1.SpawnForChat(111, "first brief", time.Minute)
	if err != nil {
		t.Fatalf("SpawnForChat: %v", err)
	}
	id2, err := m1.SpawnForChat(222, "second brief", time.Minute)
	if err != nil {
		t.Fatalf("SpawnForChat: %v", err)
	}
	for _, id := range []string{id1, id2} {
		if _, err := m1.Wait(context.Background(), id); err != nil {
			t.Fatalf("Wait(%s): %v", id, err)
		}
	}
	if !m1.MarkCharged(id1) {
		t.Fatal("MarkCharged(id1): expected true")
	}
	// Seed the ledger (eviction is the production path; the
	// ledger itself is what persistence must preserve).
	m1.mu.Lock()
	m1.deadLetterTokens = 77
	m1.deadLetterTasks = 3
	m1.mu.Unlock()
	m1.persistSnapshot()

	m2 := NewManagerWithExecutorAndStore(4, exec, dir)
	if m2.StoreDir() != dir {
		t.Errorf("StoreDir() = %q, want %q", m2.StoreDir(), dir)
	}
	for _, tc := range []struct {
		id    string
		brief string
		chat  int64
	}{
		{id1, "first brief", 111},
		{id2, "second brief", 222},
	} {
		id, wantBrief := tc.id, tc.brief
		res, ok := m2.GetResult(id)
		if !ok {
			t.Fatalf("reloaded GetResult(%s): not found", id)
		}
		if res.Output != "output for "+wantBrief {
			t.Errorf("reloaded GetResult(%s).Output = %q, want %q", id, res.Output, "output for "+wantBrief)
		}
		if res.Usage.Tokens != 42 || !res.UsageSettled {
			t.Errorf("reloaded GetResult(%s).Usage = %+v, want 42 tokens settled", id, res.Usage)
		}
	}
	if st, ok := m2.Status(id1); !ok || st != StatusDone {
		t.Errorf("reloaded Status(id1) = %q, %v, want done, true", st, ok)
	}
	// The charge flag survived: the reloaded task must not be
	// charged a second time.
	if m2.MarkCharged(id1) {
		t.Error("reloaded MarkCharged(id1): charged flag was lost across restart")
	}
	if !m2.MarkCharged(id2) {
		t.Error("reloaded MarkCharged(id2): expected true (was uncharged)")
	}
	list := m2.List()
	if len(list) != 2 {
		t.Fatalf("reloaded List() = %d tasks, want 2", len(list))
	}
	if list[0].ChatID != 111 || list[1].ChatID != 222 {
		t.Errorf("reloaded List() chats = %d, %d, want 111, 222", list[0].ChatID, list[1].ChatID)
	}
	if got := m2.DeadLetterTokens(); got != 77 {
		t.Errorf("reloaded DeadLetterTokens() = %d, want 77", got)
	}
	if got := m2.DeadLetterTasks(); got != 3 {
		t.Errorf("reloaded DeadLetterTasks() = %d, want 3", got)
	}
	if got := m2.DrainDeadLetter(); got != 77 {
		t.Errorf("reloaded DrainDeadLetter() = %d, want 77", got)
	}
	if got := m2.DrainDeadLetter(); got != 0 {
		t.Errorf("second DrainDeadLetter() = %d, want 0", got)
	}
	// The drain persisted: a third manager sees an empty ledger.
	m3 := NewManagerWithExecutorAndStore(4, exec, dir)
	if got := m3.DeadLetterTokens(); got != 0 {
		t.Errorf("after drain, reloaded DeadLetterTokens() = %d, want 0", got)
	}
}

// TestRC23W3PersistenceRunningTaskLost (F-A17-12): a task that
// was running when the daemon stopped reloads as FAILED with an
// explicit lost-on-restart error — never as done, never silently
// dropped — and contributes nothing to the ledger (its usage was
// never reported).
func TestRC23W3PersistenceRunningTaskLost(t *testing.T) {
	dir := t.TempDir()
	started := make(chan struct{})
	exec := func(ctx context.Context, brief string) (string, Usage, error) {
		close(started)
		<-ctx.Done()
		return "", Usage{}, ctx.Err()
	}
	m1 := NewManagerWithExecutorAndStore(1, exec, dir)
	id, err := m1.Spawn("blocking brief", time.Minute)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	<-started
	// Snapshot while the task is still running (the daemon stops
	// here: no finish, no Shutdown). m2 must load BEFORE m1's
	// executor is released: releasing it would let m1 record the
	// finish and re-persist, and the test would stop proving the
	// running-at-stop path.
	m1.persistSnapshot()

	m2 := NewManagerWithExecutorAndStore(1, exec, dir)
	res, ok := m2.GetResult(id)
	if !ok {
		t.Fatalf("reloaded GetResult(%s): not found — a lost task must reload, not vanish", id)
	}
	if st, _ := m2.Status(id); st != StatusFailed {
		t.Errorf("reloaded Status = %q, want failed", st)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "lost on restart") {
		t.Errorf("reloaded Result.Err = %v, want a lost-on-restart error", res.Err)
	}
	if res.UsageSettled {
		t.Error("reloaded lost task: UsageSettled is true, want false (usage was never reported)")
	}
	if got := m2.DeadLetterTokens(); got != 0 {
		t.Errorf("DeadLetterTokens() = %d, want 0 (lost usage is unknowable, never folded)", got)
	}
	// Clean up the still-blocked first manager: Shutdown cancels
	// the executor, so the test does not leak a goroutine.
	m1.Shutdown()
}

// TestRC23W3PersistenceCorruptFile (F-A17-12): a corrupt or
// wrong-version state file logs and starts empty — it never stops
// the Manager starting and never invents results.
func TestRC23W3PersistenceCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, subagentStateFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}
	exec := func(ctx context.Context, brief string) (string, Usage, error) {
		return "ok", Usage{Tokens: 1}, nil
	}
	m := NewManagerWithExecutorAndStore(4, exec, dir)
	if got := len(m.List()); got != 0 {
		t.Errorf("corrupt state: List() = %d tasks, want 0", got)
	}
	id, err := m.Spawn("after corrupt", time.Minute)
	if err != nil {
		t.Fatalf("Spawn after corrupt state: %v", err)
	}
	if _, err := m.Wait(context.Background(), id); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	// A wrong-version file is treated the same way.
	if err := os.WriteFile(path, []byte(`{"version": 99, "tasks": []}`), 0o600); err != nil {
		t.Fatalf("write wrong-version state: %v", err)
	}
	m2 := NewManagerWithExecutorAndStore(4, exec, dir)
	if got := len(m2.List()); got != 0 {
		t.Errorf("wrong-version state: List() = %d tasks, want 0", got)
	}
}

// TestRC23W3PersistenceNoDir: a Manager with no store directory
// behaves exactly as before — in-memory only, no file created.
func TestRC23W3PersistenceNoDir(t *testing.T) {
	dir := t.TempDir()
	m := NewManagerWithExecutorAndStore(4, func(ctx context.Context, brief string) (string, Usage, error) {
		return "ok", Usage{Tokens: 1}, nil
	}, "")
	id, err := m.Spawn("brief", time.Minute)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if _, err := m.Wait(context.Background(), id); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("in-memory Manager wrote %d file(s) to %s, want none", len(entries), dir)
	}
}
