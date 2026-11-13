package subagent

import (
	"context"
	"testing"
	"time"
)

// --- Hardening tests (audit P1-9, P1-10) ---

// TestForget: a finished task can be forgotten (its result freed); a
// running or unknown task cannot.
//
// The executor blocks on release until the test says so, and signals
// started when it begins executing. That synchronisation is what makes
// the "running task" assertion deterministic: with the original instant
// executor, runTask could finish between Spawn and Forget under
// full-suite load, and Forget then (correctly) returned true — a
// timing flake, not an implementation bug. Here the task cannot finish
// before the test closes release, so Forget on it must return false.
func TestForget(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	m := NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, Usage, error) {
		close(started)
		select {
		case <-release:
			return "done:" + brief, Usage{}, nil
		case <-ctx.Done():
			return "", Usage{}, ctx.Err()
		}
	})
	defer m.Shutdown()
	id, err := m.Spawn("hello", 0)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// Synchronise on task start: the executor is now running (and
	// blocked), so the task is definitely still unfinished.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not start within 5s")
	}
	if m.Forget(id) {
		t.Fatal("Forget on a running task must return false")
	}
	close(release)
	if _, err := m.Wait(context.Background(), id); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !m.Forget(id) {
		t.Fatal("Forget on a finished task must return true")
	}
	if _, ok := m.Status(id); ok {
		t.Fatal("forgotten task must no longer be listed")
	}
	if m.Forget(id) {
		t.Fatal("Forget on an unknown task must return false")
	}
}

// TestEvictsOldestFinished: the registry is bounded — spawning more
// than maxEntries finished tasks evicts the oldest first, so a long
// daemon run cannot grow List/WaitAll without bound. Running tasks
// are never evicted.
func TestEvictsOldestFinished(t *testing.T) {
	m := NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, Usage, error) {
		return "ok", Usage{}, nil
	})
	defer m.Shutdown()
	const total = maxEntries + 5
	var firstID string
	for i := 0; i < total; i++ {
		id, err := m.Spawn("task", 0)
		if err != nil {
			t.Fatalf("Spawn %d: %v", i, err)
		}
		if i == 0 {
			firstID = id
		}
		if _, err := m.Wait(context.Background(), id); err != nil {
			t.Fatalf("Wait %d: %v", i, err)
		}
	}
	listed := m.List()
	if len(listed) > maxEntries {
		t.Fatalf("registry holds %d tasks, want at most %d", len(listed), maxEntries)
	}
	if _, ok := m.Status(firstID); ok {
		t.Fatal("oldest finished task should have been evicted")
	}
}

// TestSpawnedTaskContextReleased: a task whose executor returns
// promptly must be recorded finished quickly, and a later Cancel must
// be a no-op — the task context is released on completion (defer
// cancel in runTask) instead of living until the per-task timeout.
func TestSpawnedTaskContextReleased(t *testing.T) {
	m := NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, Usage, error) {
		return "fast", Usage{}, nil
	})
	defer m.Shutdown()
	id, err := m.Spawn("fast", 5*time.Minute)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	res, err := m.Wait(context.Background(), id)
	if err != nil || res.Err != nil || res.Output != "fast" {
		t.Fatalf("Wait: res=%+v err=%v", res, err)
	}
	if m.Cancel(id) {
		t.Fatal("Cancel on a finished task must return false")
	}
	if st, ok := m.Status(id); !ok || st != StatusDone {
		t.Fatalf("Status = %v, %v, want done", st, ok)
	}
}
