package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSandboxDefaults(t *testing.T) {
	cfg := Default()
	if !cfg.Sandbox.Enabled {
		t.Error("sandbox must be ON by default (P0-O1: default-off let shell_exec read the agent's own config and exfiltrate it)")
	}
	want := filepath.Join(cfg.DataDir, "sandbox")
	if cfg.Sandbox.Root != want {
		t.Errorf("default sandbox root = %q, want %q", cfg.Sandbox.Root, want)
	}
}

// TestSandboxExplicitOptOutPreserved proves the opt-out is explicit
// and only explicit: a config file that sets sandbox.enabled=false
// loads disabled, while a file whose sandbox block omits "enabled"
// entirely loads ENABLED — omission is not an opt-out, because Load
// unmarshals over the enabled-by-default value.
func TestSandboxExplicitOptOutPreserved(t *testing.T) {
	dir := t.TempDir()

	off := filepath.Join(dir, "off.json")
	if err := os.WriteFile(off, []byte(`{"sandbox": {"enabled": false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(off)
	if err != nil {
		t.Fatalf("Load explicit-off config: %v", err)
	}
	if cfg.Sandbox.Enabled {
		t.Error("sandbox.enabled=false in the file must load disabled (explicit opt-out)")
	}

	omitted := filepath.Join(dir, "omitted.json")
	root := filepath.Join(dir, "sbx")
	if err := os.WriteFile(omitted, []byte(`{"sandbox": {"root": "`+root+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(omitted)
	if err != nil {
		t.Fatalf("Load omitted-enabled config: %v", err)
	}
	if !cfg.Sandbox.Enabled {
		t.Error("a sandbox block that omits \"enabled\" must load ENABLED (omission is not an opt-out)")
	}
	if cfg.Sandbox.Root != root {
		t.Errorf("sandbox root = %q, want %q", cfg.Sandbox.Root, root)
	}
}

// TestSandboxOptOutRoundTrip proves an explicit opt-out survives
// Save + Load: disabling the sandbox is a durable operator choice,
// not something a re-save silently re-enables.
func TestSandboxOptOutRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.Sandbox.Enabled = false
	path := filepath.Join(dir, "config.json")
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Sandbox.Enabled {
		t.Error("explicit sandbox opt-out did not survive the Save/Load round trip")
	}
}

func TestSandboxEnvOverrides(t *testing.T) {
	t.Setenv("ORION_SANDBOX", "on")
	t.Setenv("ORION_SANDBOX_ROOT", "/tmp/orion-sandbox-test")
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Sandbox.Enabled {
		t.Error("ORION_SANDBOX=on did not enable the sandbox")
	}
	if cfg.Sandbox.Root != "/tmp/orion-sandbox-test" {
		t.Errorf("ORION_SANDBOX_ROOT override failed: %q", cfg.Sandbox.Root)
	}
}

func TestSandboxEnvOff(t *testing.T) {
	t.Setenv("ORION_SANDBOX", "off")
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Sandbox.Enabled {
		t.Error("ORION_SANDBOX=off must leave the sandbox disabled")
	}
}

func TestSandboxValidateRejectsBadRoot(t *testing.T) {
	cfg := Default()
	cfg.Sandbox.Enabled = true
	cfg.Sandbox.Root = "/"
	if err := cfg.Validate(); err == nil {
		t.Error("Validate with sandbox root /: want error, got nil")
	}
	cfg.Sandbox.Root = "/etc/x"
	if err := cfg.Validate(); err == nil {
		t.Error("Validate with sandbox root /etc/x: want error, got nil")
	}
	cfg.Sandbox.Root = "relative/path"
	if err := cfg.Validate(); err == nil {
		t.Error("Validate with relative sandbox root: want error, got nil")
	}
}

func TestSandboxSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.Sandbox.Enabled = true
	cfg.Sandbox.Root = filepath.Join(dir, "sbx")
	cfg.Sandbox.AllowWithoutSeccomp = true
	path := filepath.Join(dir, "config.json")
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !loaded.Sandbox.Enabled || loaded.Sandbox.Root != filepath.Join(dir, "sbx") {
		t.Errorf("sandbox round trip mismatch: %+v", loaded.Sandbox)
	}
	if !loaded.Sandbox.AllowWithoutSeccomp {
		t.Errorf("sandbox.allow_without_seccomp did not survive the round trip: %+v", loaded.Sandbox)
	}
}
