// rc10 coordinator integration tests: Audit 5 F-A5-7 sweep
// attribution (per-task owning chat) — the residual W3b documented
// as needing a coordinator decision, fixed here by recording the
// owning chat on the task at spawn.
package app

import (
	"context"
	"testing"
	"time"

	"github.com/irfan/orion/internal/subagent"
)

// TestCoordSweepChargesOnlyOwningChat proves the F-A5-7 sweep
// attributes finished tasks to their owning chat: a task chat A
// spawned is charged by A's sweep only, never by B's, even when it
// finishes inside B's sweep window — and each chat's task is
// charged exactly once.
func TestCoordSweepChargesOnlyOwningChat(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	mgr := subagent.NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, subagent.Usage, error) {
		return "done", subagent.Usage{Tokens: 100}, nil
	})
	a.Subagents = mgr
	if _, err := mgr.SpawnForChat(111, "work for A", time.Minute); err != nil {
		t.Fatalf("SpawnForChat A: %v", err)
	}
	if _, err := mgr.SpawnForChat(222, "work for B", time.Minute); err != nil {
		t.Fatalf("SpawnForChat B: %v", err)
	}
	// Wait for both children to finish (bounded; a stuck child
	// fails the test instead of hanging it).
	deadline := time.Now().Add(5 * time.Second)
	for {
		done := 0
		for _, ts := range mgr.List() {
			if ts.Status == subagent.StatusDone {
				done++
			}
		}
		if done == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("children did not finish: %d/2 done", done)
		}
		time.Sleep(2 * time.Millisecond)
	}
	csA, err := a.chatSessionFor(111)
	if err != nil {
		t.Fatalf("chatSessionFor 111: %v", err)
	}
	csB, err := a.chatSessionFor(222)
	if err != nil {
		t.Fatalf("chatSessionFor 222: %v", err)
	}
	// Sweep B first: only B's task (100) may be charged.
	if got := a.sweepFinishedSubagents(csB, 222); got != 100 {
		t.Errorf("sweep B = %d, want 100 (only B's task)", got)
	}
	// Sweep A: only A's task (100); B's task must not leak here.
	if got := a.sweepFinishedSubagents(csA, 111); got != 100 {
		t.Errorf("sweep A = %d, want 100 (only A's task)", got)
	}
	// Second sweep charges nothing (already swept).
	if got := a.sweepFinishedSubagents(csA, 111); got != 0 {
		t.Errorf("second sweep A = %d, want 0 (already charged)", got)
	}
}

// TestCoordUnattributedTaskIsNeverSwept: a task spawned without a
// chat (Spawn, e.g. the CLI) is never charged by any chat's sweep;
// the fetch path remains its only charging route.
func TestCoordUnattributedTaskIsNeverSwept(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	mgr := subagent.NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, subagent.Usage, error) {
		return "done", subagent.Usage{Tokens: 100}, nil
	})
	a.Subagents = mgr
	if _, err := mgr.Spawn("unattributed work", time.Minute); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		done := false
		for _, ts := range mgr.List() {
			if ts.Status == subagent.StatusDone {
				done = true
			}
		}
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not finish")
		}
		time.Sleep(2 * time.Millisecond)
	}
	cs, err := a.chatSessionFor(111)
	if err != nil {
		t.Fatalf("chatSessionFor 111: %v", err)
	}
	if got := a.sweepFinishedSubagents(cs, 111); got != 0 {
		t.Errorf("sweep = %d, want 0 (unattributed task must not be charged)", got)
	}
}
