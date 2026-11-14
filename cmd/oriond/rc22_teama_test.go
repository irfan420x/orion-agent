// F-A16-1 / F-A16-5 (rc22, Team A): the shipped systemd unit must
// restart the daemon on a clean exit 0 (/restart), the live unit
// must agree, the stale "Restart=on-success" comment must be gone,
// and a SIGTERM stop must log which signal stopped the daemon.
package main

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/irfan/orion/internal/messaging"
)

// rc22UnitValue reads a systemd unit file and returns the value of
// the first "key=value" line for key, or "" when absent.
func rc22UnitValue(t *testing.T, path, key string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func TestSignalName(t *testing.T) {
	// F-A16-5: the shutdown log must name the signal the way an
	// operator reads it — os.Signal's String() ("terminated")
	// reads like a crash, not a signal.
	for _, tc := range []struct {
		sig  os.Signal
		want string
	}{
		{os.Interrupt, "SIGINT"},
		{syscall.SIGTERM, "SIGTERM"},
		{syscall.SIGHUP, "SIGHUP"},
	} {
		if got := signalName(tc.sig); got != tc.want {
			t.Errorf("signalName(%v) = %q, want %q", tc.sig, got, tc.want)
		}
	}
}

func TestShippedUnitRestartsOnCleanExit(t *testing.T) {
	// F-A16-1: /restart exits 0. Only Restart=always restarts on
	// exit 0 — the rc21 unit's Restart=on-failure never did, so
	// /restart killed the daemon permanently. (The check looks at
	// directive lines only: the header comment explains the old
	// policy by name, which is fine.)
	if got := rc22UnitValue(t, "../../oriond.service", "Restart"); got != "always" {
		t.Errorf("shipped oriond.service Restart = %q, want %q (exit 0 must restart)", got, "always")
	}
	for _, key := range []string{"Restart"} {
		if got := rc22UnitValue(t, "../../oriond.service", key); got == "on-failure" || got == "on-success" {
			t.Errorf("shipped oriond.service %s = %q — the restart policy must be exactly Restart=always", key, got)
		}
	}
	// F-A16-6: the start-limit burst was raised 5 -> 12 (matching
	// the live unit) so a burst of transient failures does not
	// park the daemon in failed state after ~25s.
	if got := rc22UnitValue(t, "../../oriond.service", "StartLimitBurst"); got != "12" {
		t.Errorf("shipped oriond.service StartLimitBurst = %q, want %q", got, "12")
	}
	// The live unit must agree on the restart policy: the two
	// copies drifted before (shipped on-failure, live always),
	// which is exactly how F-A16-1 happened.
	if got := rc22UnitValue(t, "../../systemd/oriond.service", "Restart"); got != "always" {
		t.Errorf("live systemd/oriond.service Restart = %q, want %q", got, "always")
	}
	if got := rc22UnitValue(t, "../../systemd/oriond.service", "StartLimitBurst"); got != "12" {
		t.Errorf("live systemd/oriond.service StartLimitBurst = %q, want %q", got, "12")
	}
}

func TestDaemonRestartCommentIsAccurate(t *testing.T) {
	// F-A16-1: the old comment claimed "oriond.service with
	// Restart=on-success" — on-success exists nowhere. The comment
	// must not name a restart policy the unit does not have.
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "on-success") {
		t.Errorf("cmd/oriond/main.go still mentions Restart=on-success, which no unit file sets")
	}
	if !strings.Contains(string(raw), "Restart=always") {
		t.Errorf("cmd/oriond/main.go does not mention Restart=always — the /restart comment must name the real policy")
	}
}

func TestDaemonShutdownLogsSignal(t *testing.T) {
	// F-A16-5: a SIGTERM stop must log which signal stopped the
	// daemon (exit 0, with a shutdown line) instead of dying
	// silently. Reuses the Audit 3 fake-Bot-API harness.
	scratchHome(t)
	home := os.Getenv("HOME")
	cfg := `{"default_provider":"ollama","providers":{"ollama":{"name":"ollama","base_url":"http://localhost:11434","model":"llama3.1"}},"telegram_token":"123456:AAAAAAAAAAAAAAAAAAAA","telegram_allowed_chats":[42],"data_dir":"` + home + `/.orion"}`
	if err := os.MkdirAll(home+"/.orion", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home+"/.orion/config.json", []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := fakeBotAPI(t)
	defer srv.Close()
	// fakeBotAPI is served on srv.URL; point the daemon at it via
	// the same package-level override the Audit 3 test uses.
	messaging.SetDefaultBaseURL(srv.URL)
	defer messaging.SetDefaultBaseURL("")

	codeCh, out := startDaemon(t)
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	select {
	case code := <-codeCh:
		if code != 0 {
			t.Errorf("SIGTERM exit code = %d, want 0", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("daemon did not exit within 30s of SIGTERM")
	}
	logged := out.String()
	for _, want := range []string{
		"oriond: ignoring SIGHUP",
		"oriond: shutting down on SIGTERM",
		"oriond: shutdown complete",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("daemon output missing %q; output:\n%s", want, logged)
		}
	}
}
