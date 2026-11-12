package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/model"
)

// --- Hardening tests (audit P0-3, P1-4, P1-5, P1-11; security wiring) ---

// stubTool is a minimal Tool returning canned output, for registry tests.
type stubTool struct{ output string }

func (s *stubTool) Name() string { return "stub" }
func (s *stubTool) Description() string {
	return "stub tool for tests"
}
func (s *stubTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: s.Name(), Description: s.Description(), Parameters: map[string]any{"type": "object", "properties": map[string]any{}}}
}
func (s *stubTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return s.output, nil
}

// TestFileToolsConfined: file tools may only touch the working directory
// and the temp directory, and never sensitive directories — even when a
// sensitive path sits under an allowed root or behind a symlink.
func TestFileToolsConfined(t *testing.T) {
	ctx := context.Background()
	read := &fileReadTool{}
	write := &fileWriteTool{}

	// Outside the allowed roots entirely.
	if _, err := read.Execute(ctx, mustJSON(t, map[string]any{"path": "/etc/passwd"})); err == nil {
		t.Fatal("file_read /etc/passwd: expected denial")
	} else if !strings.Contains(err.Error(), "outside the allowed") {
		t.Fatalf("file_read /etc/passwd: err = %v, want confinement message", err)
	}
	if _, err := write.Execute(ctx, mustJSON(t, map[string]any{"path": "/etc/orion-test", "content": "x"})); err == nil {
		t.Fatal("file_write /etc/orion-test: expected denial")
	}

	// Sensitive directories are denied even when they sit under an
	// allowed root: give the tool the home dir as a root and try the
	// agent's own config (which holds the Telegram token + API keys).
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	homeRead := &fileReadTool{roots: []string{home}}
	if _, err := homeRead.Execute(ctx, mustJSON(t, map[string]any{"path": filepath.Join(home, ".orion", "config.json")})); err == nil {
		t.Fatal("file_read ~/.orion/config.json: expected denial even under an allowed root")
	}
	if _, err := homeRead.Execute(ctx, mustJSON(t, map[string]any{"path": filepath.Join(home, ".ssh", "id_ed25519")})); err == nil {
		t.Fatal("file_read ~/.ssh/id_ed25519: expected denial")
	}

	// Symlink escape: a link inside the temp dir pointing at /etc must
	// not smuggle the read out of bounds.
	dir := t.TempDir()
	link := filepath.Join(dir, "escape")
	if err := os.Symlink("/etc", link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if _, err := read.Execute(ctx, mustJSON(t, map[string]any{"path": filepath.Join(link, "passwd")})); err == nil {
		t.Fatal("file_read via symlink to /etc: expected denial")
	}

	// Legitimate paths still work: temp dir is an allowed root.
	if _, err := write.Execute(ctx, mustJSON(t, map[string]any{"path": filepath.Join(dir, "ok.txt"), "content": "hello"})); err != nil {
		t.Fatalf("file_write in temp dir: %v", err)
	}
	out, err := read.Execute(ctx, mustJSON(t, map[string]any{"path": filepath.Join(dir, "ok.txt")}))
	if err != nil || out != "hello" {
		t.Fatalf("file_read in temp dir: out=%q err=%v", out, err)
	}
}

// TestShellEnvScrubbed: the shell child must not inherit secrets, so
// they can never leak into tool output. (Bare `env` is itself denied
// by the denylist, so the check expands the variables explicitly.)
func TestShellEnvScrubbed(t *testing.T) {
	t.Setenv("ORION_TEST_API_KEY", "fake-orion-secret-123")
	t.Setenv("MY_SERVICE_TOKEN", "fake-service-token-456")
	t.Setenv("MY_DB_PASSWORD", "fake-db-password-789")
	tool := &shellExecTool{}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": `echo "key=$ORION_TEST_API_KEY tok=$MY_SERVICE_TOKEN pw=$MY_DB_PASSWORD path=$PATH"`}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, secret := range []string{"fake-orion-secret-123", "fake-service-token-456", "fake-db-password-789"} {
		if strings.Contains(out, secret) {
			t.Fatalf("shell env leaked secret %q into output", secret)
		}
	}
	if !strings.Contains(out, "path=/") {
		t.Fatalf("scrubbed env should still contain PATH, got %q", out)
	}
}

// TestShellFloodingCommandBounded: a command flooding stdout must not
// grow memory without bound, and must still return a truncated success
// (not an error) with the marker.
func TestShellFloodingCommandBounded(t *testing.T) {
	tool := &shellExecTool{}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": "seq 1 200000"}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "[truncated]") {
		t.Fatalf("expected [truncated] marker, output len %d", len(out))
	}
	if len(out) > 64*1024+256 {
		t.Fatalf("output not capped: %d bytes", len(out))
	}
}

// TestRegistryRedactsToolOutput: tool output is redacted before it
// reaches the model, so a file or command that surfaces a secret does
// not leak it into the conversation.
func TestRegistryRedactsToolOutput(t *testing.T) {
	r := NewRegistry()
	// A fake OpenAI-style key: clearly fake, matching the scanner's
	// sk- pattern (48 chars after the prefix).
	fakeKey := "sk-" + strings.Repeat("a", 48)
	if err := r.Register(&stubTool{output: "the key is " + fakeKey + " done"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	out, err := r.Execute(context.Background(), "stub", json.RawMessage("{}"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out, fakeKey) {
		t.Fatalf("tool output leaked the fake key: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] in output, got %q", out)
	}
}

// TestRegistryUnknownToolKind: an unknown tool is a distinct kind, so
// the model learns the tool does not exist instead of retrying with
// different arguments.
func TestRegistryUnknownToolKind(t *testing.T) {
	r := NewRegistry()
	_, err := r.Execute(context.Background(), "nope", json.RawMessage("{}"))
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("err type = %T, want *ToolError", err)
	}
	if toolErr.Kind != "unknown_tool" {
		t.Fatalf("kind = %q, want unknown_tool", toolErr.Kind)
	}
}

// TestWebFetchBlocksSSRF: loopback, private, and link-local targets are
// refused — a model must not reach the local Ollama server, cloud
// metadata endpoints, or the Docker daemon through web_fetch.
func TestWebFetchBlocksSSRF(t *testing.T) {
	f := &webFetchTool{}
	for _, u := range []string{
		"http://localhost:11434/api/tags",
		"http://127.0.0.1:11434/",
		"http://[::1]/",
		"http://169.254.169.254/latest/meta-data/",
		"http://192.168.1.1/",
		"http://10.0.0.5/admin",
	} {
		_, err := f.Execute(context.Background(), mustJSON(t, map[string]any{"url": u}))
		if err == nil {
			t.Fatalf("url %q: expected SSRF denial", u)
		}
		if !strings.Contains(err.Error(), "non-public") {
			t.Fatalf("url %q: err = %v, want non-public address denial", u, err)
		}
	}
}

// TestFetchCheckRedirectBlocksPrivate: a redirect onto a private
// address is refused, even when the original URL was public.
func TestFetchCheckRedirectBlocksPrivate(t *testing.T) {
	req, err := http.NewRequest("GET", "http://127.0.0.1:9/x", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if err := fetchCheckRedirect(req, nil); err == nil {
		t.Fatal("redirect to 127.0.0.1: expected refusal")
	}
	public, err := http.NewRequest("GET", "https://example.com/x", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if err := fetchCheckRedirect(public, nil); err != nil {
		t.Fatalf("redirect to example.com: %v, want nil (public host passes the check)", err)
	}
}
