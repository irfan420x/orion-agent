package main

// rc9 (Worker E) tests: the `orion daemon` turn path enforces the
// per-chat daily token budget — the gap this phase closes. The
// handler is exercised directly (no Telegram network): a chat over
// budget gets a plain refusal reply and the provider is never
// called; a chat under budget runs its turn on the per-chat agent.
import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/irfan/orion/internal/app"
	"github.com/irfan/orion/internal/budget"
	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/security"
	"github.com/irfan/orion/internal/tools"
)

// daemonScriptProvider answers "ok" with fixed usage and counts
// calls, so the tests can prove a refused turn never reached it.
// It also records the images each call carried (F-V1 vision
// tests): the agent attaches a turn's images to the user message,
// so a vision turn's call must carry exactly the photo's bytes.
type daemonScriptProvider struct {
	mu         sync.Mutex
	calls      int
	imageCalls [][]model.ImageContent
}

func (p *daemonScriptProvider) Name() string { return "stub" }

func (p *daemonScriptProvider) Chat(_ context.Context, req model.ChatRequest) (*model.ChatResponse, error) {
	p.mu.Lock()
	p.calls++
	var images []model.ImageContent
	for _, m := range req.Messages {
		images = append(images, m.Images...)
	}
	p.imageCalls = append(p.imageCalls, images)
	p.mu.Unlock()
	return &model.ChatResponse{
		Message:      model.Message{Role: "assistant", Content: "ok"},
		FinishReason: "stop",
		Usage:        model.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}, nil
}

// ImageCalls returns, per provider call in order, the images that
// call carried (nil when the call carried none).
func (p *daemonScriptProvider) ImageCalls() [][]model.ImageContent {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]model.ImageContent, len(p.imageCalls))
	for i, imgs := range p.imageCalls {
		out[i] = append([]model.ImageContent(nil), imgs...)
	}
	return out
}

func (p *daemonScriptProvider) Stream(ctx context.Context, req model.ChatRequest, fn func(chunk string) error) (*model.ChatResponse, error) {
	resp, err := p.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := fn(resp.Message.Content); err != nil {
		return nil, err
	}
	return resp, nil
}

func (p *daemonScriptProvider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// daemonTestApp builds a hand-wired App (like internal/app's
// testApp) whose provider is the scripted one, so daemon turns run
// with no network.
func daemonTestApp(t *testing.T, prov *daemonScriptProvider) *app.App {
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
	return &app.App{Config: cfg, Provider: prov, Tools: reg, Memory: mem, Agent: agent, Audit: audit}
}

func TestDaemonTurnHandlerUnderBudgetRunsTurn(t *testing.T) {
	prov := &daemonScriptProvider{}
	a := daemonTestApp(t, prov)
	defer a.Close()
	a.Budget = budget.New(100000)
	handler := daemonTurnHandler(a)
	reply, stats, err := handler(context.Background(), 42, "hi", nil, nil)
	if err != nil {
		t.Fatalf("daemon turn under budget: err = %v, want a reply", err)
	}
	if reply != "ok" {
		t.Fatalf("daemon turn reply = %q, want %q", reply, "ok")
	}
	if stats == nil {
		t.Error("daemon turn stats = nil, want non-nil (a real turn carries footer data)")
	}
	if got := prov.Calls(); got != 1 {
		t.Errorf("provider calls = %d, want 1", got)
	}
	if got := a.Budget.Usage(42); got != 15 {
		t.Errorf("budget Usage after daemon turn = %d, want 15 (the turn's spend is recorded on the daemon path)", got)
	}
}

func TestDaemonTurnHandlerOverBudgetRefusesWithoutProviderCall(t *testing.T) {
	prov := &daemonScriptProvider{}
	a := daemonTestApp(t, prov)
	defer a.Close()
	a.Budget = budget.New(10)
	// Seed the chat over its limit: the next turn must be refused
	// before the model is called.
	if err := a.Budget.Record(42, 11); err == nil {
		t.Fatal("seed Record(42, 11) over a 10 limit = nil, want the over-budget error")
	}
	handler := daemonTurnHandler(a)
	reply, stats, err := handler(context.Background(), 42, "hi", nil, nil)
	if err != nil {
		t.Fatalf("daemon turn over budget: err = %v, want a plain refusal reply with nil error", err)
	}
	if stats != nil {
		t.Errorf("refused daemon turn stats = %+v, want nil (a refused turn produces no footer)", stats)
	}
	for _, want := range []string{
		"Daily token budget reached for this chat",
		"11 used",
		"10 limit",
		"resets at UTC midnight",
	} {
		if !strings.Contains(reply, want) {
			t.Errorf("daemon refusal reply = %q, missing %q", reply, want)
		}
	}
	if got := prov.Calls(); got != 0 {
		t.Errorf("provider calls = %d, want 0 (the refused turn must never reach the provider)", got)
	}
	// A refused turn records nothing.
	if got := a.Budget.Usage(42); got != 11 {
		t.Errorf("budget Usage after refused daemon turn = %d, want 11", got)
	}
}
