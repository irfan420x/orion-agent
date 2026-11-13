package config

// Tests for trusted_egress_cidrs (ChatGPT rc2 audit, finding 5):
// the 198.18/15 egress exception moved from a global in web_fetch
// into deployment configuration. Empty is strict.

import (
	"path/filepath"
	"testing"
)

func TestValidateTrustedEgressCIDRs(t *testing.T) {
	// Default (empty) validates: strict, no exception.
	if err := Default().Validate(); err != nil {
		t.Fatalf("default config: %v", err)
	}
	cfg := Default()
	cfg.TrustedEgressCIDRs = []string{"198.18.0.0/15"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid trusted CIDR rejected: %v", err)
	}
	cfg.TrustedEgressCIDRs = []string{"198.18.245.0/24", "203.0.113.0/24"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid trusted CIDRs rejected: %v", err)
	}
	for _, bad := range []string{
		"not-a-cidr",
		"198.18.0.0/33",
		"10.0.0.1", // an address, not a CIDR
		"",
		"  ",
	} {
		cfg := Default()
		cfg.TrustedEgressCIDRs = []string{bad}
		if err := cfg.Validate(); err == nil {
			t.Errorf("trusted_egress_cidrs %q: expected Validate error, got nil", bad)
		}
	}
	// One bad entry among good ones still fails.
	cfg = Default()
	cfg.TrustedEgressCIDRs = []string{"198.18.0.0/15", "junk"}
	if err := cfg.Validate(); err == nil {
		t.Errorf("trusted_egress_cidrs with one bad entry: expected Validate error, got nil")
	}
}

func TestTrustedEgressCIDRsSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.DataDir = dir
	cfg.TrustedEgressCIDRs = []string{"198.18.0.0/15"}
	path := filepath.Join(dir, "config.json")
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.TrustedEgressCIDRs) != 1 || loaded.TrustedEgressCIDRs[0] != "198.18.0.0/15" {
		t.Fatalf("round-trip TrustedEgressCIDRs = %v, want [198.18.0.0/15]", loaded.TrustedEgressCIDRs)
	}
	// A config file without the field loads as empty (strict).
	empty := Default()
	empty.DataDir = dir
	emptyPath := filepath.Join(dir, "empty.json")
	if err := empty.Save(emptyPath); err != nil {
		t.Fatalf("Save empty: %v", err)
	}
	loadedEmpty, err := Load(emptyPath)
	if err != nil {
		t.Fatalf("Load empty: %v", err)
	}
	if len(loadedEmpty.TrustedEgressCIDRs) != 0 {
		t.Fatalf("round-trip empty TrustedEgressCIDRs = %v, want []", loadedEmpty.TrustedEgressCIDRs)
	}
}
