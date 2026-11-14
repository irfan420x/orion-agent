package main

import (
	"strings"
	"testing"
)

// TestConfigShowPrintsSandboxProbeResult (F-A21-11): `orion config
// show` prints the live probe result, not just the config flag —
// "sandbox_probe: available (ordering …)" on a working host, or
// "sandbox_probe: unavailable: <reason>" where the sandbox cannot
// be established. Either branch must name the probe line; the flag
// line stays alongside it.
func TestConfigShowPrintsSandboxProbeResult(t *testing.T) {
	scratchHome(t)
	out, _, err := captureOutput(func() error { return cmdConfig([]string{"show"}) })
	if err != nil {
		t.Fatalf("config show: %v", err)
	}
	if !strings.Contains(out, "sandbox_enabled:") {
		t.Errorf("config show lost the sandbox_enabled flag line, output:\n%s", out)
	}
	if !strings.Contains(out, "sandbox_probe:") {
		t.Errorf("config show has no sandbox_probe line, output:\n%s", out)
	}
	if strings.Contains(out, "sandbox_probe:   available") {
		if !strings.Contains(out, "ordering userns-") {
			t.Errorf("available probe line does not name the ordering, output:\n%s", out)
		}
	} else if strings.Contains(out, "sandbox_probe:   unavailable:") {
		// The reason travels on the same line; an empty reason
		// would leave the operator with nothing to act on.
		line := ""
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "sandbox_probe:") {
				line = l
			}
		}
		if strings.TrimSpace(strings.TrimPrefix(line, "sandbox_probe:   unavailable:")) == "" {
			t.Errorf("unavailable probe line carries no reason, output:\n%s", out)
		}
	} else {
		t.Errorf("sandbox_probe line is neither available nor unavailable, output:\n%s", out)
	}
}
