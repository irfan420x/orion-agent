package main

import (
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/irfan/orion/internal/messaging"
)

// --- Telegram allowlist startup enforcement (ChatGPT rc2 audit,
// Finding 3) ---

// writeDaemonTestConfig writes a config with a fake-but-valid token
// and the local ollama provider into the scratch HOME. allowlist is
// the raw JSON for telegram_allowed_chats; allowAll sets
// allow_all_telegram_chats.
func writeDaemonTestConfig(t *testing.T, allowlist string, allowAll bool) {
	t.Helper()
	home := os.Getenv("HOME")
	body := `{"default_provider":"ollama","providers":{"ollama":{"name":"ollama","base_url":"http://localhost:11434","model":"llama3.1"}},"telegram_token":"123456:AAAAAAAAAAAAAAAAAAAA","telegram_allowed_chats":` + allowlist + `,"allow_all_telegram_chats":` + map[bool]string{true: "true", false: "false"}[allowAll] + `,"data_dir":"` + home + `/.orion"}`
	if err := os.MkdirAll(home+"/.orion", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home+"/.orion/config.json", []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// captureRun runs run(args) with stdout/stderr captured and returns
// the exit code and the combined output.
func captureRun(args []string) (int, string) {
	oldOut, oldErr := os.Stdout, os.Stderr
	r, w, _ := os.Pipe()
	os.Stdout, os.Stderr = w, w
	code := run(args)
	_ = w.Close()
	out, _ := io.ReadAll(r)
	os.Stdout, os.Stderr = oldOut, oldErr
	return code, string(out)
}

// TestRunEmptyAllowlistRefusesStartup: with an empty allowlist and no
// opt-in, oriond must refuse to start (exit 1) with an actionable
// error — before any Telegram network call. This is the fail-closed
// replacement for the old warning.
func TestRunEmptyAllowlistRefusesStartup(t *testing.T) {
	scratchHome(t)
	writeDaemonTestConfig(t, `[]`, false)
	code, out := captureRun(nil)
	if code != 1 {
		t.Fatalf("empty allowlist: exit %d, want 1 (output: %s)", code, out)
	}
	for _, want := range []string{
		"telegram_allowed_chats is empty",
		"allow_all_telegram_chats=true",
		"/whoami",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal output %q does not mention %q", out, want)
		}
	}
}

// TestRunAllowAllPermitsStartup: with allow_all_telegram_chats=true
// and an empty allowlist, oriond starts polling normally and stops
// cleanly on SIGTERM.
func TestRunAllowAllPermitsStartup(t *testing.T) {
	scratchHome(t)
	writeDaemonTestConfig(t, `[]`, true)
	srv := fakeBotAPI(t)
	defer srv.Close()
	messaging.SetDefaultBaseURL(srv.URL)
	defer messaging.SetDefaultBaseURL("")

	codeCh, _ := startDaemon(t)
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	select {
	case code := <-codeCh:
		if code != 0 {
			t.Errorf("allow-all run: SIGTERM exit code = %d, want 0", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("allow-all run: daemon did not exit within 30s of SIGTERM")
	}
}
