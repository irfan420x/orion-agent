// F-A3-2 (rc21): `orion daemon` serves the pprof endpoint when
// config debug.pprof_enabled is on, and listens on nothing extra
// when it is off — the same wiring as oriond, proven end to end
// against the rc11 fake Bot API + fake provider.
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// rc21FreePort returns a currently-free loopback port for the
// debug endpoint: listen on 127.0.0.1:0, read the assigned port,
// close. The daemon binds it immediately after, so the race
// window is negligible in tests.
func rc21FreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("rc21FreePort: listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("rc21FreePort: close: %v", err)
	}
	return port
}

// rc21WriteDaemonDebugConfig writes a config file for the
// end-to-end debug tests, mirroring rc11WriteDaemonTestConfig
// (fake provider as deepseek, fake bot token, allowlist [111],
// sandbox off) with the debug section set: enabled + port when
// enabled is true, omitted entirely when it is false.
func rc21WriteDaemonDebugConfig(t *testing.T, home string, providerURL string, enabled bool, port int) {
	t.Helper()
	debugSection := ""
	if enabled {
		debugSection = fmt.Sprintf(`,
  "debug": {"pprof_enabled": true, "pprof_port": %d}`, port)
	}
	body := `{
  "default_provider": "deepseek",
  "providers": {"deepseek": {"api_key": "fake-test-key", "base_url": "` + providerURL + `", "model": "deepseek-chat"}},
  "telegram_token": "123456:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
  "telegram_allowed_chats": [111],
  "data_dir": "` + home + `/.orion",
  "max_steps": 5,
  "timeout_seconds": 30,
  "sandbox": {"enabled": false}` + debugSection + `
}`
	if err := os.MkdirAll(home+"/.orion", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home+"/.orion/config.json", []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// rc21GetUntil polls url until it returns 200 or the deadline
// passes, and returns the status and body of the last attempt.
func rc21GetUntil(t *testing.T, url string) (int, []byte) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var lastStatus int
	var lastBody []byte
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			lastStatus, lastBody = resp.StatusCode, body
			if resp.StatusCode == http.StatusOK {
				return lastStatus, lastBody
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return lastStatus, lastBody
}

// rc21BotGetMe reads the fake bot's getMe count under its lock.
func rc21BotGetMe(bot *rc11FakeBot) int {
	bot.mu.Lock()
	defer bot.mu.Unlock()
	return bot.getMe
}

func TestDaemonDebugEndpointEnabled(t *testing.T) {
	home := scratchHome(t)
	provider := &rc11FakeProvider{}
	provSrv := httptest.NewServer(provider)
	defer provSrv.Close()
	bot := &rc11FakeBot{}
	botSrv := httptest.NewServer(bot)
	defer botSrv.Close()
	port := rc21FreePort(t)
	rc21WriteDaemonDebugConfig(t, home, provSrv.URL, true, port)
	t.Setenv("ORION_TELEGRAM_BASE_URL", botSrv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := capture(func() error { return cmdDaemon(ctx, nil) })
		errCh <- err
	}()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	status, body := rc21GetUntil(t, base+"/debug/pprof/")
	if status != http.StatusOK {
		t.Fatalf("GET /debug/pprof/: status = %d, want 200", status)
	}
	for _, want := range []string{"goroutine", "heap"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("index body does not mention %q", want)
		}
	}
	status, body = rc21GetUntil(t, base+"/debug/pprof/goroutine?debug=1")
	if status != http.StatusOK {
		t.Fatalf("GET /debug/pprof/goroutine?debug=1: status = %d, want 200", status)
	}
	if !strings.Contains(string(body), "goroutine profile") {
		t.Errorf("goroutine body does not contain %q", "goroutine profile")
	}
	status, _ = rc21GetUntil(t, base+"/debug/pprof/heap")
	if status != http.StatusOK {
		t.Fatalf("GET /debug/pprof/heap: status = %d, want 200", status)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("cmdDaemon after cancel: err = %v, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cmdDaemon did not return after cancelling its context")
	}
}

func TestDaemonDebugEndpointDisabled(t *testing.T) {
	home := scratchHome(t)
	provider := &rc11FakeProvider{}
	provSrv := httptest.NewServer(provider)
	defer provSrv.Close()
	bot := &rc11FakeBot{}
	botSrv := httptest.NewServer(bot)
	defer botSrv.Close()
	// No debug section at all: the off-by-default path.
	rc21WriteDaemonDebugConfig(t, home, provSrv.URL, false, 0)
	t.Setenv("ORION_TELEGRAM_BASE_URL", botSrv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := capture(func() error { return cmdDaemon(ctx, nil) })
		errCh <- err
	}()
	// The daemon is up once it has called getMe; only then is
	// "nothing listens" a meaningful assertion.
	rc11WaitFor(t, "daemon getMe", func() bool { return rc21BotGetMe(bot) > 0 })

	// The default pprof port must refuse: with the endpoint off,
	// the daemon listens on nothing extra.
	conn, err := net.DialTimeout("tcp", "127.0.0.1:6060", 2*time.Second)
	if err == nil {
		_ = conn.Close()
		t.Errorf("dial 127.0.0.1:6060 succeeded with the endpoint disabled — something is listening on the default pprof port")
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("cmdDaemon after cancel: err = %v, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cmdDaemon did not return after cancelling its context")
	}
}
