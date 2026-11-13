package sandbox

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultConfig proves the sandbox is ON by default (P0-O1):
// the default configuration confines shell_exec, rooted at
// <dataDir>/sandbox, and a data-dir-less default still enables it
// (Resolve fills the root in from the home directory later).
func TestDefaultConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	if !cfg.Enabled {
		t.Error("DefaultConfig must enable the sandbox")
	}
	if cfg.Root != filepath.Join(dir, "sandbox") {
		t.Errorf("DefaultConfig root = %q, want %q", cfg.Root, filepath.Join(dir, "sandbox"))
	}
	if cfg.AllowWithoutSeccomp || cfg.AllowUnsandboxedFallback || cfg.DisableSeccomp {
		t.Errorf("DefaultConfig must not opt into any degradation: %+v", cfg)
	}

	bare := DefaultConfig("")
	if !bare.Enabled {
		t.Error("DefaultConfig(\"\") must still enable the sandbox")
	}
	if bare.Root != "" {
		t.Errorf("DefaultConfig(\"\") root = %q, want empty (Resolve fills it in)", bare.Root)
	}
	resolved := bare.Resolve(dir)
	if !resolved.Enabled || resolved.Root != filepath.Join(dir, "sandbox") {
		t.Errorf("DefaultConfig(\"\").Resolve(%q) = %+v, want enabled with root under the data dir", dir, resolved)
	}
}

// TestDefaultConfigRefusesWithoutLauncher proves the new default
// composes with the F-A3-1 fail-closed rule: a default-constructed
// config (no opt-ins, no launcher discoverable anywhere) REFUSES
// in Prepare with ErrSeccompUnavailable — the default-on flip
// never silently becomes an unfiltered run.
func TestDefaultConfigRefusesWithoutLauncher(t *testing.T) {
	requireSandbox(t)
	hermeticLauncherEnv(t)
	cfg := DefaultConfig(t.TempDir())
	if got := cfg.findSeccompLauncher(); got != "" {
		t.Fatalf("findSeccompLauncher() = %q, want empty (no launcher discoverable)", got)
	}
	cmd := exec.Command("/bin/sh", "-c", "true")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	err := cfg.Prepare(cmd, "true")
	if err == nil {
		t.Fatal("Prepare with the default config and no launcher: want refusal, got nil error")
	}
	if !errors.Is(err, ErrSeccompUnavailable) {
		t.Fatalf("Prepare error %v: want it to wrap ErrSeccompUnavailable", err)
	}
	if !strings.Contains(err.Error(), "allow_without_seccomp") {
		t.Fatalf("refusal should name the opt-in, got: %v", err)
	}
}
