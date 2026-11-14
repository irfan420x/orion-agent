// F-A17-2 (rc23, W2; supersedes the F-A16-1 exit-0 rule) / F-A16-5
// (rc22, Team A): the shipped systemd unit must restart the daemon
// on /restart's exit 42 (Restart=on-failure +
// RestartForceExitStatus=42), the live unit must agree, the stale
// "Restart=on-success" comment must be gone, and a SIGTERM stop must
// log which signal stopped the daemon.
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

func TestShippedUnitRestartsOnRestartRequest(t *testing.T) {
	// F-A17-2 (rc23, supersedes F-A16-1's exit-0 rule): /restart
	// exits 42 (exitRestartRequested), and the unit restarts it with
	// Restart=on-failure + RestartForceExitStatus=42. A clean exit 0
	// (SIGTERM shutdown) no longer restarts: only a fault or an
	// explicit restart request does. (The check looks at directive
	// lines only: the header comment explains the old policy by name,
	// which is fine.)
	if got := rc22UnitValue(t, "../../oriond.service", "Restart"); got != "on-failure" {
		t.Errorf("shipped oriond.service Restart = %q, want %q (only faults and /restart exit 42 restart)", got, "on-failure")
	}
	if got := rc22UnitValue(t, "../../oriond.service", "RestartForceExitStatus"); got != "42" {
		t.Errorf("shipped oriond.service RestartForceExitStatus = %q, want %q (/restart exit code)", got, "42")
	}
	if got := rc22UnitValue(t, "../../oriond.service", "Restart"); got == "always" {
		t.Errorf("shipped oriond.service Restart = %q — Restart=always restarts clean exits too, which is what let /restart burn burst budget (F-A17-2)", got)
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
	if got := rc22UnitValue(t, "../../systemd/oriond.service", "Restart"); got != "on-failure" {
		t.Errorf("live systemd/oriond.service Restart = %q, want %q", got, "on-failure")
	}
	if got := rc22UnitValue(t, "../../systemd/oriond.service", "RestartForceExitStatus"); got != "42" {
		t.Errorf("live systemd/oriond.service RestartForceExitStatus = %q, want %q", got, "42")
	}
	if got := rc22UnitValue(t, "../../systemd/oriond.service", "StartLimitBurst"); got != "12" {
		t.Errorf("live systemd/oriond.service StartLimitBurst = %q, want %q", got, "12")
	}
}

func TestShippedUnitHardeningDirectives(t *testing.T) {
	// F-A18-4 (rc24): the shipped user unit must carry the same
	// three verified-safe hardening directives as the live unit
	// (systemd/oriond.service) — they lived only in the live unit
	// before, so a fresh install ran unhardened. The values are
	// pinned exactly, in both files, so the two copies cannot drift
	// the way the restart policy did (F-A16-1).
	//
	// Why these three are safe for the shipped user unit:
	// NoNewPrivileges — oriond never needs to gain privileges.
	// ProtectSystem=true (NOT strict/full) — makes /usr, /boot and
	// /etc read-only while leaving /home writable, and the data dir
	// (~/.orion) and WorkingDirectory (%h) both live under /home;
	// strict/full would make /home read-only and break the daemon.
	// PrivateTmp — oriond shares no /tmp files with other services
	// (the sandbox root is under ~/.orion/sandbox).
	// The two must-NOTs are pinned too: ProtectHome would hide the
	// data dir, and any CapabilityBoundingSet breaks the sandbox's
	// `unshare --user --map-root-user` uid_map write (see the live
	// unit's header for the 9-unit matrix that proved it).
	for _, path := range []string{"../../oriond.service", "../../systemd/oriond.service"} {
		for _, tc := range []struct {
			key  string
			want string
		}{
			{"NoNewPrivileges", "true"},
			{"ProtectSystem", "true"},
			{"PrivateTmp", "true"},
		} {
			if got := rc22UnitValue(t, path, tc.key); got != tc.want {
				t.Errorf("%s %s = %q, want %q (F-A18-4 hardening)", path, tc.key, got, tc.want)
			}
		}
		if got := rc22UnitValue(t, path, "ProtectHome"); got != "" {
			t.Errorf("%s ProtectHome = %q, want unset — the data dir lives in $HOME (F-A18-4)", path, got)
		}
		if got := rc22UnitValue(t, path, "CapabilityBoundingSet"); got != "" {
			t.Errorf("%s CapabilityBoundingSet = %q, want unset — any restriction breaks the sandbox unshare uid_map write (F-A18-4)", path, got)
		}
	}
}

func TestDaemonRestartCommentIsAccurate(t *testing.T) {
	// F-A17-2: the /restart comment must name the real policy —
	// exit 42 with Restart=on-failure + RestartForceExitStatus=42 —
	// and the old "Restart=on-success" claim (which existed nowhere)
	// must stay gone. A comment claiming exit 0 restarts the daemon
	// would be the F-A16-1 misunderstanding all over again.
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "on-success") {
		t.Errorf("cmd/oriond/main.go still mentions Restart=on-success, which no unit file sets")
	}
	for _, want := range []string{"RestartForceExitStatus=42", "Restart=on-failure", "exitRestartRequested"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("cmd/oriond/main.go does not mention %q — the /restart comment must name the real policy", want)
		}
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
