package subagent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRC24ShutdownWaitsForLateOutcomePersist: Shutdown must not
// return until the late-outcome waiter's persist has landed.
// When a task's executor is still running at Shutdown, runTask's
// select takes the cancellation branch and spawns
// awaitLateOutcome, which persists the settled usage once the
// executor returns. Before this fix that waiter was not tracked
// by m.wg, so Shutdown returned while it was still parked and a
// persist could land after the caller had started cleaning up
// the store dir — the pre-existing flake in
// TestRC23W3PersistenceRunningTaskLost (TempDir RemoveAll:
// "directory not empty"), which failed ~1 in 3 full-suite runs.
//
// The executor sleeps 200ms after ctx is cancelled before
// returning, so the cancellation branch wins deterministically
// (the outcome is not ready when Shutdown cancels ctx). If
// Shutdown returns early, the snapshot on disk still shows the
// task unsettled; with the fix, Shutdown waits for the waiter
// and the snapshot shows the settled usage.
func TestRC24ShutdownWaitsForLateOutcomePersist(t *testing.T) {
	dir := t.TempDir()
	started := make(chan struct{})
	exec := func(ctx context.Context, brief string) (string, Usage, error) {
		close(started)
		<-ctx.Done()
		time.Sleep(200 * time.Millisecond)
		return "", Usage{Tokens: 123}, ctx.Err()
	}
	m := NewManagerWithExecutorAndStore(1, exec, dir)
	id, err := m.Spawn("late outcome brief", time.Minute)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	<-started
	m.Shutdown()

	// Read the snapshot immediately after Shutdown returns: the
	// late-outcome persist must already be on disk.
	data, err := os.ReadFile(filepath.Join(dir, subagentStateFileName))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if len(st.Tasks) != 1 {
		t.Fatalf("state file has %d tasks, want 1", len(st.Tasks))
	}
	pt := st.Tasks[0]
	if pt.ID != id {
		t.Fatalf("state task id = %q, want %q", pt.ID, id)
	}
	if pt.Tokens != 123 || !pt.UsageSettled {
		t.Errorf("state task usage = %d tokens settled=%v, want 123 tokens settled=true (Shutdown returned before the late-outcome persist)", pt.Tokens, pt.UsageSettled)
	}
}
