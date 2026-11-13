// Audit 4 B P1-1 / B P1-2 regression tests.
package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
)

// wireTestAppMemoryV2 wires a typed store (with one feedback memory)
// and a session log into a test App, exactly as Build does.
func wireTestAppMemoryV2(t *testing.T, a *App) {
	t.Helper()
	dir := a.Config.DataDir
	ts := mustTypedStoreForApp(t, dir)
	if err := ts.Save(memory.TypedMemory{
		Name:        "prefer-brevity",
		Description: "User prefers short answers",
		Type:        memory.TypeFeedback,
		Body:        "Keep replies short.\n\nWhy: long replies bury the answer.\n\nHow to apply: answer first, then stop.\n",
		Provenance:  memory.ProvenanceUser,
	}); err != nil {
		t.Fatalf("Save feedback: %v", err)
	}
	slog, err := memory.NewSessionLog(dir)
	if err != nil {
		t.Fatalf("NewSessionLog: %v", err)
	}
	a.typedStore = ts
	a.sessionLog = slog
	a.typedAdapter = newTypedStoreAdapter(ts)
	a.driftRoot = dir
	a.applyMemoryV2Wiring(a.Agent)
}

// TestAudit4SwitchProviderKeepsMemoryV2: the Workstream B probe —
// TurnStats.MemoriesUsed must survive a /model switch. Before the
// switch the feedback memory is injected (1); after switching to
// ollama (no key needed; the call fails fast on connection refused,
// but TurnFinished still carries MemoriesUsed) it must still be 1.
func TestAudit4SwitchProviderKeepsMemoryV2(t *testing.T) {
	a := testApp(t)
	wireTestAppMemoryV2(t, a)

	_, before, err := a.HandleStream(context.Background(), "hello", nil)
	if err != nil {
		t.Fatalf("HandleStream before switch: %v", err)
	}
	if before == nil || before.MemoriesUsed != 1 {
		t.Fatalf("MemoriesUsed before switch = %v, want 1", before)
	}
	if err := a.SwitchProvider("ollama"); err != nil {
		t.Fatalf("SwitchProvider(ollama): %v", err)
	}
	// The ollama provider has no local server in the test, so the
	// turn errors on connection refused — but the TurnFinished
	// event (and hence TurnStats) is still emitted, with the
	// memories injected before the provider call.
	_, after, err := a.HandleStream(context.Background(), "hello again", nil)
	if err == nil {
		t.Log("note: ollama turn unexpectedly succeeded (a local Ollama server is running); the MemoriesUsed assertion still applies")
	}
	if after == nil {
		t.Fatalf("HandleStream after switch returned nil stats (err=%v)", err)
	}
	if after.MemoriesUsed != 1 {
		t.Errorf("MemoriesUsed after switch = %d, want 1 (Memory v2 wiring was lost by SwitchProvider)", after.MemoriesUsed)
	}
}

// slowStubProvider answers after a delay, so two HandleStream calls
// overlap in time (the second starts while the first is running).
type slowStubProvider struct{ delay time.Duration }

func (s slowStubProvider) Name() string { return "slow-stub" }
func (s slowStubProvider) Chat(_ context.Context, _ model.ChatRequest) (*model.ChatResponse, error) {
	time.Sleep(s.delay)
	return &model.ChatResponse{
		Message:      model.Message{Role: "assistant", Content: "ok"},
		FinishReason: "stop",
	}, nil
}
func (s slowStubProvider) Stream(_ context.Context, _ model.ChatRequest, fn func(chunk string) error) (*model.ChatResponse, error) {
	if err := fn("ok"); err != nil {
		return nil, err
	}
	return &model.ChatResponse{
		Message:      model.Message{Role: "assistant", Content: "ok"},
		FinishReason: "stop",
	}, nil
}

// TestAudit4ConcurrentHandleStreamHandlers: two concurrent chats on
// one App must each receive exactly their own TurnFinished (1/1),
// never 0/2. Runs serialise on the agent's runMu; the per-run
// handler is what keeps the events separate.
func TestAudit4ConcurrentHandleStreamHandlers(t *testing.T) {
	a := testApp(t)
	slow := slowStubProvider{delay: 300 * time.Millisecond}
	agent, err := core.NewAgent(a.Config, slow, a.Tools, a.Memory)
	if err != nil {
		t.Fatalf("NewAgent(slow): %v", err)
	}
	a.Agent = agent
	a.Provider = slow

	var mu sync.Mutex
	finished := map[string]int{"A": 0, "B": 0}
	stats := map[string]*core.TurnStats{}
	emit := func(name string) func(core.ProgressEvent) {
		return func(ev core.ProgressEvent) {
			if ev.Kind == core.EventTurnFinished {
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
		_, st, err := a.HandleStream(context.Background(), "one", emit("A"))
		if err != nil {
			t.Errorf("HandleStream A: %v", err)
			return
		}
		mu.Lock()
		stats["A"] = st
		mu.Unlock()
	}()
	time.Sleep(50 * time.Millisecond)
	go func() {
		defer wg.Done()
		_, st, err := a.HandleStream(context.Background(), "two", emit("B"))
		if err != nil {
			t.Errorf("HandleStream B: %v", err)
			return
		}
		mu.Lock()
		stats["B"] = st
		mu.Unlock()
	}()
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if finished["A"] != 1 || finished["B"] != 1 {
		t.Errorf("TurnFinished counts = %v, want A:1 B:1", finished)
	}
	if stats["A"] == nil || stats["B"] == nil {
		t.Errorf("nil TurnStats: A=%v B=%v (each chat must get its own stats)", stats["A"], stats["B"])
	}
}
