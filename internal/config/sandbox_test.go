package config

import (
	"path/filepath"
	"testing"
)

func TestSandboxDefaults(t *testing.T) {
	cfg := Default()
	if cfg.Sandbox.Enabled {
		t.Error("sandbox must be off by default")
	}
	want := filepath.Join(cfg.DataDir, "sandbox")
	if cfg.Sandbox.Root != want {
		t.Errorf("default sandbox root = %q, want %q", cfg.Sandbox.Root, want)
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
