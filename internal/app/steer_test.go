// App-level steer bridge tests: SteerTurn offers a message to a
// chat's live turn (true only while a run is registered AND the
// agent has a run active), and DrainSteeredLeftovers returns the
// steers the run did not consume.
package app

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/security"
	"github.com/irfan/orion/internal/tools"
)

// steerBlockingProvider is a model.Provider whose first Chat blocks
// until release, so the test can steer mid-run deterministically.
// Later calls answer at once.
type steerBlockingProvider struct {
	mu      sync.Mutex
	calls   int
	started chan struct{} // sent on the first Chat
	release chan struct{}
}

func (p *steerBlockingProvider) Name() string { return "blocking" }

func (p *steerBlockingProvider) Chat(_ context.Context, _ model.ChatRequest) (*model.ChatResponse, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call == 1 {
		select {
		case p.started <- struct{}{}:
		default:
		}
		<-p.release
	}
	return &model.ChatResponse{
		Message:      model.Message{Role: "assistant", Content: "ok"},
		FinishReason: "stop",
	}, nil
}

func (p *steerBlockingProvider) Stream(ctx context.Context, req model.ChatRequest, fn func(chunk string) error) (*model.ChatResponse, error) {
	resp, err := p.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp != nil {
		if err := fn(resp.Message.Content); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// testSteerApp is testAppOnDir with the blocking provider, so a
// chat turn can be held mid-run.
func testSteerApp(t *testing.T, prov model.Provider) *App {
	t.Helper()
	dir := t.TempDir()
	mem, err := memory.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	reg, err := tools.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.MaxSteps = 5
	cfg.TimeoutSeconds = 30
	agent, err := core.NewAgent(cfg, prov, reg, mem)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	audit, err := security.NewAuditLog(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	return &App{Config: cfg, Provider: prov, Tools: reg, Memory: mem, Agent: agent, Audit: audit}
}

func TestAppSteerTurn(t *testing.T) {
	prov := &steerBlockingProvider{started: make(chan struct{}, 1), release: make(chan struct{})}
	a := testSteerApp(t, prov)
	defer a.Close()

	// No run anywhere: steering declines everywhere, and there are
	// no leftovers.
	if a.SteerTurn(77, "too early", 1) {
		t.Error("SteerTurn with no run registered returned true, want false")
	}
	if left := a.DrainSteeredLeftovers(77); left != nil {
		t.Errorf("DrainSteeredLeftovers with no agent = %v, want nil", left)
	}
	if a.SteerTurn(999, "nobody", 2) {
		t.Error("SteerTurn for an unknown chat returned true, want false")
	}

	// Start a real turn for chat 77, registered exactly as
	// oriond's runTurn registers it, and hold it in the first
	// provider call.
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	if !a.RegisterRun(77, runCtx, runCancel) {
		t.Fatal("RegisterRun(77) returned false, want true")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = a.HandleStreamForChat(runCtx, 77, "first", nil)
	}()
	select {
	case <-prov.started:
	case <-time.After(5 * time.Second):
		t.Fatal("chat turn did not reach the provider call")
	}
	// A run is registered and the agent has a run active: the
	// steer is accepted. A chat with no run still declines.
	if !a.SteerTurn(77, "steered in", 5) {
		t.Error("SteerTurn during a live run returned false, want true")
	}
	if a.SteerTurn(999, "nobody", 6) {
		t.Error("SteerTurn for a chat with no run returned true, want false")
	}
	close(prov.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("chat turn did not finish after release")
	}
	// The run is over: steering declines even before the
	// registration is removed (the agent has no run active), and
	// again after it is removed (no run registered).
	if a.SteerTurn(77, "too late", 7) {
		t.Error("SteerTurn after the run returned true, want false")
	}
	if !a.UnregisterRun(77, runCtx) {
		t.Error("UnregisterRun(77) returned false, want true")
	}
	if a.SteerTurn(77, "too late", 8) {
		t.Error("SteerTurn after unregister returned true, want false")
	}
	// The steer landed during the run's only (final) provider
	// call, so the run could not consume it: the drain returns it
	// for the gateway to run as a leftover turn.
	left := a.DrainSteeredLeftovers(77)
	if len(left) != 1 || left[0].Text != "steered in" || left[0].UpdateID != 5 {
		t.Fatalf("DrainSteeredLeftovers = %v, want [{steered in 5}]", left)
	}
	if left := a.DrainSteeredLeftovers(77); len(left) != 0 {
		t.Errorf("second DrainSteeredLeftovers = %v, want empty", left)
	}
}

// TestAppSteerTurnNilAppIsSafe: the bridge never panics on a nil App.
func TestAppSteerTurnNilAppIsSafe(t *testing.T) {
	var a *App
	if a.SteerTurn(77, "x", 1) {
		t.Error("nil App SteerTurn returned true, want false")
	}
	if left := a.DrainSteeredLeftovers(77); left != nil {
		t.Errorf("nil App DrainSteeredLeftovers = %v, want nil", left)
	}
}
