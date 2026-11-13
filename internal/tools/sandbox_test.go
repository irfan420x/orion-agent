package tools

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/sandbox"
)

// captureLog swaps the standard logger's output for buf for the
// duration of the test, so a test can assert about what a degraded
// run logged. Tests in this package run sequentially (no
// t.Parallel), so swapping the process-global logger is safe here.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := log.Default().Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return &buf
}

// hermeticLauncherEnv makes orion-seccomp undiscoverable: no env
// override and a PATH with no launcher in it. (The test binary's
// own directory has no launcher next to it either.)
func hermeticLauncherEnv(t *testing.T) {
	t.Helper()
	t.Setenv(sandbox.SeccompLauncherEnv, "")
	t.Setenv("PATH", t.TempDir())
}

// TestShellExecSandboxed runs a legitimate command with sandbox mode
// on: it must succeed, see /work as its cwd, and persist the file it
// wrote into the sandbox root's work dir on the host.
func TestShellExecSandboxed(t *testing.T) {
	if !sandbox.Available() {
		t.Skipf("sandbox unavailable: %v", sandbox.AvailabilityError())
	}
	root := filepath.Join(t.TempDir(), "root")
	// The filter is not what this test proves (tool-level sandbox
	// containment is); it opts into an unfiltered run explicitly
	// via AllowWithoutSeccomp, since the test binary has no
	// orion-seccomp next to it and the F-A3-1 fail-closed change
	// would otherwise refuse every run.
	tool := &shellExecTool{sandboxCfg: sandbox.Config{Enabled: true, Root: root, AllowWithoutSeccomp: true}}
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
	// Unfiltered by explicit opt-in (see TestShellExecSandboxed):
	// this test proves containment, not the filter.
	tool := &shellExecTool{sandboxCfg: sandbox.Config{Enabled: true, Root: root, AllowWithoutSeccomp: true}}
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

// TestShellExecSandboxRefusesWhenSetupFails proves fail-closed: with
// sandbox mode on but setup impossible (root under /proc, which can
// never be created), the command is REFUSED — kind "denied", naming
// the opt-in — and never runs, instead of silently running
// unsandboxed. The marker file must not exist afterwards.
func TestShellExecSandboxRefusesWhenSetupFails(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker.txt")
	tool := &shellExecTool{sandboxCfg: sandbox.Config{Enabled: true, Root: "/proc/orion-sandbox-test"}}
	_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": fmt.Sprintf("echo ran > %q", marker)}))
	if err == nil {
		t.Fatal("Execute with an unestablishable sandbox: want refusal, got nil error")
	}
	if kind := toolErrKind(t, err); kind != "denied" {
		t.Fatalf("expected kind denied, got %q (%v)", kind, err)
	}
	if !strings.Contains(err.Error(), "sandbox.allow_unsandboxed_fallback=true") {
		t.Fatalf("refusal should name the opt-in, got: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("marker file %s exists: the command ran despite the refusal", marker)
	}
}

// TestShellExecSandboxFallbackOptIn proves the explicit opt-in: with
// AllowUnsandboxedFallback set, a command whose sandbox cannot be
// established still runs unsandboxed and the result carries a
// warning, instead of failing outright.
func TestShellExecSandboxFallbackOptIn(t *testing.T) {
	tool := &shellExecTool{sandboxCfg: sandbox.Config{Enabled: true, Root: "/proc/orion-sandbox-test", AllowUnsandboxedFallback: true}}
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

// TestShellExecSandboxRefusesWithoutSeccompLauncher is the F-A3-1
// fail-closed proof through the tool: with the sandbox enabled, the
// filter enabled (the default) and no launcher discoverable
// anywhere, the command is REFUSED — kind "denied", naming the
// allow_without_seccomp opt-in — and NOT executed. The two markers
// prove non-execution both ways: /work/marker.txt would exist if
// the command had run sandboxed, and the host-temp marker would
// exist if it had run unsandboxed.
func TestShellExecSandboxRefusesWithoutSeccompLauncher(t *testing.T) {
	if !sandbox.Available() {
		t.Skipf("sandbox unavailable: %v", sandbox.AvailabilityError())
	}
	hermeticLauncherEnv(t)
	base := t.TempDir()
	root := filepath.Join(base, "root")
	workMarker := filepath.Join(root, "work", "marker.txt")
	hostMarker := filepath.Join(base, "host-marker.txt")
	tool := &shellExecTool{sandboxCfg: sandbox.Config{Enabled: true, Root: root}}
	cmd := fmt.Sprintf("echo ran > /work/marker.txt; echo ran > %q", hostMarker)
	_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": cmd}))
	if err == nil {
		t.Fatal("Execute with no seccomp launcher: want refusal, got nil error")
	}
	if kind := toolErrKind(t, err); kind != "denied" {
		t.Fatalf("expected kind denied, got %q (%v)", kind, err)
	}
	if !strings.Contains(err.Error(), "allow_without_seccomp") {
		t.Fatalf("refusal should name the opt-in, got: %v", err)
	}
	if _, err := os.Stat(workMarker); !os.IsNotExist(err) {
		t.Errorf("marker %s exists: the command ran sandboxed despite the refusal", workMarker)
	}
	if _, err := os.Stat(hostMarker); !os.IsNotExist(err) {
		t.Errorf("marker %s exists: the command ran unsandboxed despite the refusal", hostMarker)
	}
}

// TestShellExecSandboxSeccompOptInRunsDegraded proves the explicit
// opt-in through the tool: with AllowWithoutSeccomp set and no
// launcher discoverable, the command runs (the work marker proves
// it ran sandboxed), and the degraded run is logged on this
// command, naming the opt-in.
func TestShellExecSandboxSeccompOptInRunsDegraded(t *testing.T) {
	if !sandbox.Available() {
		t.Skipf("sandbox unavailable: %v", sandbox.AvailabilityError())
	}
	hermeticLauncherEnv(t)
	logged := captureLog(t)
	root := filepath.Join(t.TempDir(), "root")
	tool := &shellExecTool{sandboxCfg: sandbox.Config{Enabled: true, Root: root, AllowWithoutSeccomp: true}}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": "echo optin-hi > /work/marker.txt; cat /work/marker.txt"}))
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !strings.Contains(out, "exit_code: 0") || !strings.Contains(out, "optin-hi") {
		t.Fatalf("opt-in run should succeed, output:\n%s", out)
	}
	if got, err := os.ReadFile(filepath.Join(root, "work", "marker.txt")); err != nil || strings.TrimSpace(string(got)) != "optin-hi" {
		t.Fatalf("marker.txt on host: got %q, err %v; want optin-hi (proves the command ran sandboxed)", got, err)
	}
	if !strings.Contains(logged.String(), "WITHOUT syscall filtering") || !strings.Contains(logged.String(), "allow_without_seccomp=true") {
		t.Fatalf("opt-in run: want a per-command degraded warning in the log, got:\n%s", logged.String())
	}
}

// TestShellExecSandboxDisableSeccompRuns proves DisableSeccomp is
// a deliberate choice, not a refusal: with the filter explicitly
// disabled and no launcher discoverable, the command runs
// sandboxed (the work marker proves it) instead of being refused.
func TestShellExecSandboxDisableSeccompRuns(t *testing.T) {
	if !sandbox.Available() {
		t.Skipf("sandbox unavailable: %v", sandbox.AvailabilityError())
	}
	hermeticLauncherEnv(t)
	root := filepath.Join(t.TempDir(), "root")
	tool := &shellExecTool{sandboxCfg: sandbox.Config{Enabled: true, Root: root, DisableSeccomp: true}}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": "echo disabled-hi > /work/marker.txt; cat /work/marker.txt"}))
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !strings.Contains(out, "exit_code: 0") || !strings.Contains(out, "disabled-hi") {
		t.Fatalf("disabled-filter run should succeed, output:\n%s", out)
	}
	if got, err := os.ReadFile(filepath.Join(root, "work", "marker.txt")); err != nil || strings.TrimSpace(string(got)) != "disabled-hi" {
		t.Fatalf("marker.txt on host: got %q, err %v; want disabled-hi (proves the command ran sandboxed)", got, err)
	}
}

// TestShellExecSandboxSeccompFallbackComposition is the
// composition proof for the two opt-ins: a missing filter plus
// AllowUnsandboxedFallback=true but AllowWithoutSeccomp=false
// must still be REFUSED. The fallback opt-in covers sandbox
// ESTABLISHMENT failure, not filter absence — letting it run the
// command fully unsandboxed here would be a worse degradation
// than sandboxed-without-filter, and would make the filter opt-in
// meaningless. The host marker proves the command did not run
// unsandboxed.
func TestShellExecSandboxSeccompFallbackComposition(t *testing.T) {
	if !sandbox.Available() {
		t.Skipf("sandbox unavailable: %v", sandbox.AvailabilityError())
	}
	hermeticLauncherEnv(t)
	base := t.TempDir()
	root := filepath.Join(base, "root")
	workMarker := filepath.Join(root, "work", "marker.txt")
	hostMarker := filepath.Join(base, "host-marker.txt")
	tool := &shellExecTool{sandboxCfg: sandbox.Config{Enabled: true, Root: root, AllowUnsandboxedFallback: true}}
	cmd := fmt.Sprintf("echo ran > /work/marker.txt; echo ran > %q", hostMarker)
	_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"command": cmd}))
	if err == nil {
		t.Fatal("Execute with no launcher and only the fallback opt-in: want refusal, got nil error")
	}
	if kind := toolErrKind(t, err); kind != "denied" {
		t.Fatalf("expected kind denied, got %q (%v)", kind, err)
	}
	if !strings.Contains(err.Error(), "allow_without_seccomp") {
		t.Fatalf("refusal should name the filter opt-in (not the fallback one), got: %v", err)
	}
	if strings.Contains(err.Error(), "allow_unsandboxed_fallback") {
		t.Fatalf("refusal must not point at the fallback opt-in, got: %v", err)
	}
	if _, err := os.Stat(workMarker); !os.IsNotExist(err) {
		t.Errorf("marker %s exists: the command ran sandboxed despite the refusal", workMarker)
	}
	if _, err := os.Stat(hostMarker); !os.IsNotExist(err) {
		t.Errorf("marker %s exists: the command ran UNSANDBOXED despite the refusal", hostMarker)
	}
}
