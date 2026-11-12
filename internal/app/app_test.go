package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/security"
	"github.com/irfan/orion/internal/tools"
)

// stubProvider is a minimal model.Provider for app tests: Chat always
// answers "ok", Stream emits it once.
type stubProvider struct{}

func (stubProvider) Name() string { return "stub" }
func (stubProvider) Chat(_ context.Context, _ model.ChatRequest) (*model.ChatResponse, error) {
	return &model.ChatResponse{
		Message:      model.Message{Role: "assistant", Content: "ok"},
		FinishReason: "stop",
	}, nil
}
func (stubProvider) Stream(_ context.Context, _ model.ChatRequest, fn func(chunk string) error) (*model.ChatResponse, error) {
	if err := fn("ok"); err != nil {
		return nil, err
	}
	return &model.ChatResponse{
		Message:      model.Message{Role: "assistant", Content: "ok"},
		FinishReason: "stop",
	}, nil
}

func testApp(t *testing.T) *App {
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
	agent, err := core.NewAgent(cfg, stubProvider{}, reg, mem)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	audit, err := security.NewAuditLog(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	return &App{Config: cfg, Provider: stubProvider{}, Tools: reg, Memory: mem, Agent: agent, Audit: audit}
}

func TestHandleNilAppDoesNotPanic(t *testing.T) {
	var a *App
	if _, err := a.Handle(context.Background(), "hi"); err == nil {
		t.Error("nil App Handle: expected error, got nil")
	}
	if err := a.Close(); err != nil {
		t.Errorf("nil App Close: %v, want nil", err)
	}
	if err := (&App{}).Close(); err != nil {
		t.Errorf("zero App Close: %v, want nil (nil Agent/Subagents must be skipped)", err)
	}
}

func TestHandleAuditTruncatesByRunesAndRedactsFirst(t *testing.T) {
	a := testApp(t)
	defer a.Close()

	// 250 two-byte runes: a byte truncation at 200 would split a rune
	// and produce invalid UTF-8. The audit detail must stay valid
	// UTF-8 and hold at most 200 runes plus the "..." marker.
	input := strings.Repeat("é", 250)
	reply, err := a.Handle(context.Background(), input)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if reply != "ok" {
		t.Fatalf("Handle reply = %q, want %q", reply, "ok")
	}
	entries, err := a.Audit.Entries()
	if err != nil {
		t.Fatalf("Audit.Entries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(entries))
	}
	detail := entries[0].Detail
	if !utf8.ValidString(detail) {
		t.Errorf("audit detail is not valid UTF-8: %q", detail)
	}
	if n := utf8.RuneCountInString(detail); n > 203 {
		t.Errorf("audit detail = %d runes, want at most 203 (200 + ...)", n)
	}
	if !strings.HasSuffix(detail, "...") {
		t.Errorf("audit detail %q does not end with ... (truncation marker)", detail)
	}

	// A secret early in the input must be redacted and the marker must
	// survive truncation. (Fake test key only.)
	secret := "sk-testfakekey1234567890"
	if _, err := a.Handle(context.Background(), secret+" "+strings.Repeat("x", 250)); err != nil {
		t.Fatalf("Handle with secret: %v", err)
	}
	entries, err = a.Audit.Entries()
	if err != nil {
		t.Fatalf("Audit.Entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("audit entries = %d, want 2", len(entries))
	}
	detail = entries[1].Detail
	if strings.Contains(detail, "testfakekey") {
		t.Errorf("audit detail still contains a secret fragment: %q", detail)
	}
	if !strings.HasPrefix(detail, security.Redacted) {
		t.Errorf("audit detail %q does not start with the %q marker", detail, security.Redacted)
	}

	// A secret that straddles the truncation boundary must still be
	// redacted: redaction runs before truncation, so no fragment of
	// the secret may survive in the audit log. (The "[REDACTED]"
	// marker itself may be cut by the truncation — that reveals
	// nothing; the secret fragment must not survive.)
	prefix := strings.Repeat("x", 195)
	if _, err := a.Handle(context.Background(), prefix+secret); err != nil {
		t.Fatalf("Handle with boundary secret: %v", err)
	}
	entries, err = a.Audit.Entries()
	if err != nil {
		t.Fatalf("Audit.Entries: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("audit entries = %d, want 3", len(entries))
	}
	detail = entries[2].Detail
	if !utf8.ValidString(detail) {
		t.Errorf("audit detail is not valid UTF-8: %q", detail)
	}
	if strings.Contains(detail, "testfakekey") {
		t.Errorf("audit detail still contains a secret fragment: %q", detail)
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("hello", 10); got != "hello" {
		t.Errorf("short input: got %q", got)
	}
	if got := truncateRunes("hello world", 5); got != "hello..." {
		t.Errorf("got %q, want %q", got, "hello...")
	}
	if got := truncateRunes("éééééé", 3); got != "ééé..." || !utf8.ValidString(got) {
		t.Errorf("multi-byte: got %q, want valid UTF-8 %q", got, "ééé...")
	}
}
