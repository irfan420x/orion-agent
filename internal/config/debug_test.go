// F-A3-2 (rc21): the debug/pprof endpoint is config-gated and
// OFF by default; an invalid port must fail validation at load
// time, never silently bind somewhere else.
package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/debug"
)

func TestDebugDefaultsDisabled(t *testing.T) {
	cfg := Default()
	if cfg.Debug.PprofEnabled {
		t.Errorf("default Debug.PprofEnabled = true, want false (off by default)")
	}
	if cfg.Debug.PprofPort != 0 {
		t.Errorf("default Debug.PprofPort = %d, want 0 (the default port)", cfg.Debug.PprofPort)
	}
	if got := cfg.DebugPprofPort(); got != debug.DefaultPprofPort {
		t.Errorf("DebugPprofPort() = %d, want %d (the 6060 default)", got, debug.DefaultPprofPort)
	}
	if got := cfg.DebugPprofPort(); got != 6060 {
		t.Errorf("DebugPprofPort() = %d, want 6060", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config should validate: %v", err)
	}
}

func TestDebugJSONRoundTrip(t *testing.T) {
	cfg := Default()
	raw := `{"debug": {"pprof_enabled": true, "pprof_port": 16060}}`
	if err := json.Unmarshal([]byte(raw), cfg); err != nil {
		t.Fatalf("unmarshal debug section: %v", err)
	}
	if !cfg.Debug.PprofEnabled {
		t.Errorf("after unmarshal: PprofEnabled = false, want true")
	}
	if cfg.Debug.PprofPort != 16060 {
		t.Errorf("after unmarshal: PprofPort = %d, want 16060", cfg.Debug.PprofPort)
	}
	if got := cfg.DebugPprofPort(); got != 16060 {
		t.Errorf("DebugPprofPort() = %d, want 16060", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("enabled debug config should validate: %v", err)
	}
	out, err := json.Marshal(cfg.Debug)
	if err != nil {
		t.Fatalf("marshal debug section: %v", err)
	}
	if !strings.Contains(string(out), `"pprof_enabled":true`) || !strings.Contains(string(out), `"pprof_port":16060`) {
		t.Errorf("marshalled debug section = %s, want pprof_enabled:true and pprof_port:16060", out)
	}
}

func TestDebugPortValidation(t *testing.T) {
	for _, tc := range []struct {
		port int
		want string // "" means valid
	}{
		{0, ""},
		{1, ""},
		{6060, ""},
		{65535, ""},
		{-1, "pprof_port"},
		{65536, "pprof_port"},
		{70000, "pprof_port"},
	} {
		cfg := Default()
		cfg.Debug.PprofEnabled = true
		cfg.Debug.PprofPort = tc.port
		err := cfg.Validate()
		if tc.want == "" {
			if err != nil {
				t.Errorf("port %d: Validate = %v, want nil", tc.port, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("port %d: Validate = nil, want an error", tc.port)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("port %d: Validate = %v, want it to name %q", tc.port, err, tc.want)
		}
	}
	// An invalid port is rejected even when the endpoint is
	// disabled: a typo must not sit silently in the config until
	// someone enables it.
	cfg := Default()
	cfg.Debug.PprofPort = 70000
	if err := cfg.Validate(); err == nil {
		t.Errorf("port 70000 with endpoint disabled: Validate = nil, want an error")
	}
}
