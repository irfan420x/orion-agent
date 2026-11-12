package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/sandbox"
)

// TestShellExecSandboxed runs a legitimate command with sandbox mode
// on: it must succeed, see /work as its cwd, and persist the file it
// wrote into the sandbox root's work dir on the host.
func TestShellExecSandboxed(t *testing.T) {
	if !sandbox.Available() {
		t.Skipf("sandbox unavailable: %v", sandbox.AvailabilityError())
	}
	root := filepath.Join(t.TempDir(), "root")
	tool := &shellExecTool{sandboxCfg: sandbox.Config{Enabled: true, Root: root}}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": "pwd; echo hello > hello.txt; cat hello.txt"}))
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	for _, want := range []string{"exit_code: 0", "/work", "hello"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "[warning:") {
		t.Fatalf("unexpected sandbox warning in output:\n%s", out)
	}
	if got, err := os.ReadFile(filepath.Join(root, "work", "hello.txt")); err != nil || strings.TrimSpace(string(got)) != "hello" {
		t.Fatalf("hello.txt on host: got %q, err %v", got, err)
	}
}

// TestShellExecSandboxContains proves, through the tool (denylist
// first, sandbox second), that a command the denylist allows cannot
// touch a file outside the sandbox root: `rm` of an explicit file
// is allowed, and writing to a host /tmp path passes the denylist
// (it is not a system path) but the path does not exist inside the
// chroot, so the outside canary survives while the work file is
// really deleted.
func TestShellExecSandboxContains(t *testing.T) {
	if !sandbox.Available() {
		t.Skipf("sandbox unavailable: %v", sandbox.AvailabilityError())
	}
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside.txt")
	if err := os.WriteFile(outside, []byte("OUTSIDE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "keep.txt"), []byte("KEEP\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := &shellExecTool{sandboxCfg: sandbox.Config{Enabled: true, Root: root}}
	cmd := fmt.Sprintf("rm /work/keep.txt; echo PWNED > %q", outside)
	if _, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": cmd})); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "keep.txt")); !os.IsNotExist(err) {
		t.Error("keep.txt survived the sandboxed rm; want it deleted (proves the command really ran)")
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "OUTSIDE\n" {
		t.Errorf("outside canary: got %q, err %v; want untouched OUTSIDE", got, err)
	}
}

// TestShellExecSandboxDenylistStillApplies proves the sandbox is a
// second layer, not a replacement: a denylisted command is refused
// before anything is spawned, sandbox on or not.
func TestShellExecSandboxDenylistStillApplies(t *testing.T) {
	tool := &shellExecTool{sandboxCfg: sandbox.Config{Enabled: true, Root: filepath.Join(t.TempDir(), "root")}}
	_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": "rm -rf /"}))
	if err == nil {
		t.Fatal("rm -rf / was not blocked with sandbox on")
	}
	if kind := toolErrKind(t, err); kind != "denied" {
		t.Fatalf("expected kind denied, got %q (%v)", kind, err)
	}
}

// TestShellExecSandboxFallback proves the graceful fallback: with
// sandbox mode on but setup impossible (root under /proc, which can
// never be created), the command still runs unsandboxed and the
// result carries a warning, instead of failing outright.
func TestShellExecSandboxFallback(t *testing.T) {
	tool := &shellExecTool{sandboxCfg: sandbox.Config{Enabled: true, Root: "/proc/orion-sandbox-test"}}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": "echo fallback-hi"}))
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !strings.Contains(out, "exit_code: 0") || !strings.Contains(out, "fallback-hi") {
		t.Fatalf("fallback run should still succeed, output:\n%s", out)
	}
	if !strings.Contains(out, "[warning: sandbox") {
		t.Fatalf("fallback run should carry a sandbox warning, output:\n%s", out)
	}
}
