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
)

// Audit 3, Workstream 5 (installation and recovery): the daemon must
// start cleanly, stop cleanly on SIGTERM (exit 0), and start again
// over the same data directory. The live Telegram path is already
// proven elsewhere; this test runs oriond end-to-end against a fake
// Bot API server so restart behaviour is covered without a real token
// or network, and without touching the running production daemon.

// fakeBotAPI answers the three Bot API calls oriond makes at startup
// and while polling: getMe, setMyCommands, getUpdates.
func fakeBotAPI(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			fmt.Fprint(w, `{"ok":true,"result":{"id":123456,"username":"TestBot","first_name":"Test"}}`)
		case strings.HasSuffix(r.URL.Path, "/setMyCommands"):
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			// Short pause so an idle poll does not spin the test CPU.
			time.Sleep(10 * time.Millisecond)
			fmt.Fprint(w, `{"ok":true,"result":[]}`)
		default:
			t.Logf("fakeBotAPI: unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"ok":false,"description":"not found"}`)
		}
	})
	return httptest.NewServer(mux)
}

// startDaemon runs run(nil) in the background with stdout/stderr
// daemonOutput is the captured output of a daemon started by
// startDaemon: the pipe-reader goroutine appends while the daemon
// runs, and the test reads while it runs too, so every access is
// locked. A plain strings.Builder is NOT safe for that (the reader
// and a test's String() call race), and the lock must live with the
// buffer — startDaemon's old local mutex could not be reached by
// callers that kept the builder.
type daemonOutput struct {
	mu  sync.Mutex
	buf strings.Builder
}

// String returns everything captured so far.
func (d *daemonOutput) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.buf.String()
}

// Write appends p to the buffer.
func (d *daemonOutput) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.buf.Write(p)
}

// captured, and waits until the daemon reports it is polling (or the
// test fails). It returns the exit-code channel and the captured
// output buffer.
func startDaemon(t *testing.T) (chan int, *daemonOutput) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	out := &daemonOutput{}
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				_, _ = out.Write(buf[:n])
			}
			if err != nil {
				close(done)
				return
			}
		}
	}()
	codeCh := make(chan int, 1)
	go func() {
		code := run(nil)
		_ = w.Close()
		<-done
		os.Stdout, os.Stderr = oldOut, oldErr
		codeCh <- code
	}()
	// Wait for the "running as" line: the daemon is polling only then.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "oriond running as @TestBot") {
			return codeCh, out
		}
		select {
		case code := <-codeCh:
			t.Fatalf("daemon exited early with code %d; output:\n%s", code, out.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("daemon did not report running within 30s; output:\n%s", out.String())
	return nil, nil
}

func TestDaemonStartStopRestart(t *testing.T) {
	scratchHome(t)
	home := os.Getenv("HOME")
	// Fake-but-well-formed config: a token matching the validation
	// pattern (never a real token) and the local ollama default, so
	// startup needs no network beyond the fake Bot API.
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

	// Start 1: must reach polling, then stop cleanly on SIGTERM.
	codeCh, _ := startDaemon(t)
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

	// State must have survived the stop: the daemon writes its
	// memory/audit/sessions under the data dir on startup.
	for _, f := range []string{"memory.json", "audit.jsonl", "sessions.json"} {
		if _, err := os.Stat(home + "/.orion/" + f); err != nil {
			t.Errorf("after stop: %s missing: %v", f, err)
		}
	}

	// Start 2 (restart over the same data dir): must reach polling
	// again and stop cleanly — this is the resume path a supervisor
	// (systemd Restart=) relies on.
	codeCh, _ = startDaemon(t)
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM (restart): %v", err)
	}
	select {
	case code := <-codeCh:
		if code != 0 {
			t.Errorf("restart: SIGTERM exit code = %d, want 0", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("restart: daemon did not exit within 30s of SIGTERM")
	}
}
