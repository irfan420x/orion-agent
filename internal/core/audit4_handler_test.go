// Audit 4 B P1-2 regression test: per-run event handlers.
//
// Two concurrent runs on ONE shared Agent must each receive exactly
// their own TurnFinished event. The old HandleStream swapped the
// agent-wide handler outside runMu, so the second run stole the
// first run's TurnFinished (handler A: 0, handler B: 2).
package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/tools"
)

func TestAudit4PerRunHandlerIsolation(t *testing.T) {
	prov := &fakeProvider{
		chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
			time.Sleep(300 * time.Millisecond)
			return &model.ChatResponse{
				Message:      model.Message{Role: "assistant", Content: "canned answer"},
				FinishReason: "stop",
			}, nil
		},
	}
	a := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), newFakeStore())

	var mu sync.Mutex
	finished := map[string]int{"A": 0, "B": 0}
	handler := func(name string) EventHandler {
		return func(ev ProgressEvent) {
			if ev.Kind == EventTurnFinished {
				mu.Lock()
				finished[name]++
				mu.Unlock()
			}
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := a.RunWithEventHandler(context.Background(), "one", handler("A")); err != nil {
			t.Errorf("run A: %v", err)
		}
	}()
	// Start B while A is still running: with the old shared-handler
	// swap, B's handler would steal A's TurnFinished.
	time.Sleep(50 * time.Millisecond)
	go func() {
		defer wg.Done()
		if _, err := a.RunWithEventHandler(context.Background(), "two", handler("B")); err != nil {
			t.Errorf("run B: %v", err)
		}
	}()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if finished["A"] != 1 || finished["B"] != 1 {
		t.Errorf("TurnFinished counts = %v, want A:1 B:1 (each run's events must go to its own handler)", finished)
	}
}

// TestAudit4RunWithEventHandlerNilFallsBack: a nil per-run handler
// behaves like Run — events go to the agent-wide handler.
func TestAudit4RunWithEventHandlerNilFallsBack(t *testing.T) {
	a := newV2TestAgent(t)
	seen := 0
	a.SetEventHandler(func(ev ProgressEvent) {
		if ev.Kind == EventTurnFinished {
			seen++
		}
	})
	if _, err := a.RunWithEventHandler(context.Background(), "hi", nil); err != nil {
		t.Fatalf("RunWithEventHandler(nil): %v", err)
	}
	if seen != 1 {
		t.Errorf("agent-wide handler saw %d TurnFinished, want 1", seen)
	}
	// And a per-run handler overrides the agent-wide one for that
	// run only; the agent-wide handler sees nothing.
	seen = 0
	perRun := 0
	if _, err := a.RunWithEventHandler(context.Background(), "hi", func(ev ProgressEvent) {
		if ev.Kind == EventTurnFinished {
			perRun++
		}
	}); err != nil {
		t.Fatalf("RunWithEventHandler: %v", err)
	}
	if perRun != 1 || seen != 0 {
		t.Errorf("per-run=%d agent-wide=%d, want per-run=1 agent-wide=0", perRun, seen)
	}
	// After the run the agent-wide handler is live again.
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run after per-run handler: %v", err)
	}
	if seen != 1 {
		t.Errorf("agent-wide handler saw %d TurnFinished after the per-run run, want 1 (per-run handler must not leak)", seen)
	}
}
