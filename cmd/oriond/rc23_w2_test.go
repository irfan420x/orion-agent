package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/irfan/orion/internal/messaging"
	"github.com/irfan/orion/internal/version"
)

// F-A17-2 / F-A17-3 / F-A17-16 / F-A17-17 (rc23, W2): /restart exits
// 42 (not 0) so the unit can force-restart it apart from crashes;
// a second oriond refuses to start (exit 2) instead of 409-flapping
// the good daemon; the startup banner carries version/PID/data
// dir/config path; both shutdown paths share one drain bound with
// honest log language.

// TestAcquireInstanceLockSecondFails: the flock guard (F-A17-3) —
// / a second holder gets "already running", and releasing the first
// lock lets the next instance start (a crash can never leave a stale
// lock behind: flock dies with the fd).
func TestAcquireInstanceLockSecondFails(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	if _, err := acquireInstanceLock(dir); err == nil {
		_ = first.Close()
		t.Fatal("second lock: got nil, want an error (already running)")
	} else if !strings.Contains(err.Error(), "already running") {
		_ = first.Close()
		t.Errorf("second lock error = %q, want it to say already running", err.Error())
	}
	// Release: the next instance must be able to start.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	defer second.Close()
	fi, err := os.Stat(dir + "/oriond.lock")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("lock file mode = %o, want 0600", fi.Mode().Perm())
	}
	if _, err := acquireInstanceLock(""); err == nil {
		t.Error("empty data dir: got nil, want an error")
	}
}

// TestDaemonSecondInstanceExits2: with one daemon polling over a data
// dir, a second oriond over the same dir must exit 2 with a clear
// log line — before getMe, before polling — and the first daemon must
// be undisturbed (it still stops cleanly on SIGTERM afterwards).
func TestDaemonSecondInstanceExits2(t *testing.T) {
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
	messaging.SetDefaultBaseURL(srv.URL)
	defer messaging.SetDefaultBaseURL("")

	codeCh, out := startDaemon(t)
	// The second instance runs in-process with NO descriptor swap:
	// the old runCaptured helper swapped os.Stdout/os.Stderr around
	// the call, but swapping the globals while the first daemon is
	// still writing to them races the daemon's Fprintf reads of
	// os.Stderr (the race detector proved it in the rc23 clean-clone
	// run). The second instance's "already running" line goes to the
	// same pipe the first daemon writes to, so it lands in the shared
	// output buffer — the pipe reader appends asynchronously, so poll
	// for it instead of reading it straight after run returns.
	code := run(nil)
	if code != exitAlreadyRunning {
		t.Errorf("second instance: exit %d, want %d (already running)", code, exitAlreadyRunning)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if strings.Contains(out.String(), "already running") {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("second instance: output does not say already running:\n%s", out.String())
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(home + "/.orion/oriond.lock"); err != nil {
		t.Errorf("first daemon did not create the instance lock: %v", err)
	}
	// First daemon undisturbed: stops cleanly on SIGTERM.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	select {
	case code := <-codeCh:
		if code != 0 {
			t.Errorf("first run: SIGTERM exit code = %d, want 0", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("first run: daemon did not exit within 30s of SIGTERM")
	}
}

// fakeRestartBotAPI is fakeBotAPI with one extra update: the first
// getUpdates returns a /restart message from the owner (chat 42,
// sender 42); later calls return nothing. It also answers
// sendMessage, which the daemon uses for the "Restarting…" reply.
func fakeRestartBotAPI(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	delivered := false
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			fmt.Fprint(w, `{"ok":true,"result":{"id":123456,"username":"TestBot","first_name":"Test"}}`)
		case strings.HasSuffix(r.URL.Path, "/setMyCommands"):
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":1}}`)
		case strings.HasSuffix(r.URL.Path, "/sendChatAction"):
			// Typing indicator while the /restart turn runs.
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			time.Sleep(10 * time.Millisecond)
			mu.Lock()
			defer mu.Unlock()
			if !delivered {
				delivered = true
				fmt.Fprint(w, `{"ok":true,"result":[{"update_id":1,"message":{"message_id":1,"from":{"id":42,"is_bot":false,"first_name":"Owner"},"chat":{"id":42,"type":"private"},"date":1763030000,"text":"/restart"}}]}`)
				return
			}
			fmt.Fprint(w, `{"ok":true,"result":[]}`)
		default:
			t.Logf("fakeRestartBotAPI: unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"ok":false,"description":"not found"}`)
		}
	})
	return httptest.NewServer(mux)
}

// TestDaemonRestartExits42: an owner /restart must exit 42
// (F-A17-2, exitRestartRequested), not 0 — the exit code the unit's
// RestartForceExitStatus=42 keys on. It also proves the F-A17-16
// banner (version, PID, data dir, config path on one line) and that
// the instance lock is released on the restart path, so the next
// start can take it.
func TestDaemonRestartExits42(t *testing.T) {
	scratchHome(t)
	home := os.Getenv("HOME")
	cfg := `{"default_provider":"ollama","providers":{"ollama":{"name":"ollama","base_url":"http://localhost:11434","model":"llama3.1"}},"telegram_token":"123456:AAAAAAAAAAAAAAAAAAAA","telegram_allowed_chats":[42],"data_dir":"` + home + `/.orion"}`
	if err := os.MkdirAll(home+"/.orion", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home+"/.orion/config.json", []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := fakeRestartBotAPI(t)
	defer srv.Close()
	messaging.SetDefaultBaseURL(srv.URL)
	defer messaging.SetDefaultBaseURL("")

	codeCh, out := startDaemon(t)
	select {
	case code := <-codeCh:
		if code != exitRestartRequested {
			t.Errorf("/restart: exit %d, want %d (exitRestartRequested)", code, exitRestartRequested)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("daemon did not exit after /restart within 30s")
	}
	// F-A17-16: the banner line carries version, PID, data dir and
	// config path — one line, so the journal can tell which build a
	// dead daemon was.
	banner := out.String()
	for _, want := range []string{
		"oriond running as @TestBot",
		"version " + version.String(),
		"pid ",
		"data dir " + home + "/.orion",
		"config " + home + "/.orion/config.json",
	} {
		if !strings.Contains(banner, want) {
			t.Errorf("banner missing %q:\n%s", want, banner)
		}
	}
	if !strings.Contains(banner, "restart requested") {
		t.Errorf("output missing the restart log line:\n%s", banner)
	}
	// The lock is released on the restart path: the next start can
	// take it (this is what lets the supervisor's restart succeed).
	f, err := acquireInstanceLock(home + "/.orion")
	if err != nil {
		t.Errorf("after /restart exit: instance lock still held: %v", err)
	} else {
		_ = f.Close()
	}
}
