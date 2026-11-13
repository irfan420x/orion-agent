// F-A3-2 (rc21): the oriond daemon serves the pprof endpoint
// when config debug.pprof_enabled is on, and listens on nothing
// extra when it is off. End to end against the fake Bot API, so
// the wiring — config → Start → banner → polling → SIGTERM
// shutdown — is exercised exactly as the live daemon runs it.
package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/irfan/orion/internal/messaging"
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

// rc21WriteDaemonDebugConfig writes a fake-but-well-formed daemon
// config like TestDaemonStartStopRestart's, with the debug
// section set: enabled + port when enabled is true, omitted
// entirely when it is false (the off-by-default path).
func rc21WriteDaemonDebugConfig(t *testing.T, home string, enabled bool, port int) {
	t.Helper()
	debugSection := ""
	if enabled {
		debugSection = fmt.Sprintf(`,"debug":{"pprof_enabled":true,"pprof_port":%d}`, port)
	}
	cfg := `{"default_provider":"ollama","providers":{"ollama":{"name":"ollama","base_url":"http://localhost:11434","model":"llama3.1"}},"telegram_token":"123456:AAAAAAAAAAAAAAAAAAAA","telegram_allowed_chats":[42],"data_dir":"` + home + `/.orion"` + debugSection + `}`
	if err := os.MkdirAll(home+"/.orion", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home+"/.orion/config.json", []byte(cfg), 0o600); err != nil {
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

// rc21StopDaemon SIGTERMs the test process (which the daemon's
// signal context catches, exactly as in
// TestDaemonStartStopRestart) and asserts a clean exit 0.
func rc21StopDaemon(t *testing.T, codeCh chan int) {
	t.Helper()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	select {
	case code := <-codeCh:
		if code != 0 {
			t.Errorf("daemon SIGTERM exit code = %d, want 0", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("daemon did not exit within 30s of SIGTERM")
	}
}

func TestDaemonDebugEndpointEnabled(t *testing.T) {
	scratchHome(t)
	home := os.Getenv("HOME")
	port := rc21FreePort(t)
	rc21WriteDaemonDebugConfig(t, home, true, port)

	srv := fakeBotAPI(t)
	defer srv.Close()
	messaging.SetDefaultBaseURL(srv.URL)
	defer messaging.SetDefaultBaseURL("")

	codeCh, out := startDaemon(t)
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

	rc21StopDaemon(t, codeCh)
	// The daemon logs the listen address on stderr at startup;
	// the output is stable now that the daemon has exited.
	if !strings.Contains(out.String(), fmt.Sprintf("debug pprof listening on 127.0.0.1:%d", port)) {
		t.Errorf("daemon output does not contain the debug listen line; output:\n%s", out.String())
	}
}

func TestDaemonDebugEndpointDisabled(t *testing.T) {
	scratchHome(t)
	home := os.Getenv("HOME")
	// No debug section at all: the off-by-default path.
	rc21WriteDaemonDebugConfig(t, home, false, 0)

	srv := fakeBotAPI(t)
	defer srv.Close()
	messaging.SetDefaultBaseURL(srv.URL)
	defer messaging.SetDefaultBaseURL("")

	codeCh, out := startDaemon(t)

	// The default pprof port must refuse: with the endpoint off,
	// the daemon listens on nothing extra.
	conn, err := net.DialTimeout("tcp", "127.0.0.1:6060", 2*time.Second)
	if err == nil {
		_ = conn.Close()
		t.Errorf("dial 127.0.0.1:6060 succeeded with the endpoint disabled — something is listening on the default pprof port")
	}

	rc21StopDaemon(t, codeCh)
	if strings.Contains(out.String(), "debug pprof listening") {
		t.Errorf("daemon output mentions a debug listener with the endpoint disabled; output:\n%s", out.String())
	}
}
