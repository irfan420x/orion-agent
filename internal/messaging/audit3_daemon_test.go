package messaging

// Audit 3, Workstream 4 (long-running daemon): polling-recovery tests.
//
// The live daemon's polling loop is the one thing that must never die
// quietly: a network drop between oriond and api.telegram.org must be
// a transient backoff-and-continue, never a daemon exit. The fatal
// cases (401/403/409) are already covered by
// TestPollExitsOnFatalAPIError; these tests cover the other side —
// transient failures must NOT kill polling, and the updates that
// arrive after the outage must still be delivered.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// outageServer fails the first failN getUpdates calls the way a real
// outage would, then serves one queued update and empty polls after
// that. sendMessage always succeeds (the handler's reply must go
// through for the turn to complete).
type outageServer struct {
	failN     int32
	failMode  string // "http500" or "reset"
	calls     atomic.Int32
	delivered atomic.Int32
}

func (s *outageServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/getUpdates"):
		n := s.calls.Add(1)
		if n <= s.failN {
			switch s.failMode {
			case "reset":
				// Simulate a dropped connection: hijack and close
				// without a response, so the client sees EOF.
				hj, ok := w.(http.Hijacker)
				if !ok {
					w.WriteHeader(http.StatusInternalServerError)
					fmt.Fprint(w, `{"ok":false,"error_code":500,"description":"no hijack"}`)
					return
				}
				conn, _, err := hj.Hijack()
				if err != nil {
					return
				}
				_ = conn.Close()
				return
			default: // "http500"
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"ok":false,"error_code":500,"description":"simulated outage"}`)
				return
			}
		}
		if s.delivered.Add(1) == 1 {
			fmt.Fprint(w, `{"ok":true,"result":[{"update_id":7,"message":{"message_id":1,"chat":{"id":1},"from":{"id":1},"text":"hello after outage"}}]}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":[]}`)
	case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":99,"chat":{"id":1},"text":"ack"}}`)
	default:
		fmt.Fprint(w, `{"ok":true,"result":{}}`)
	}
}

// TestSessionStoreSaveCreatesFile: Save must create the sessions file
// even when no turn ever mutated the store — otherwise a daemon that
// ran with zero turns leaves no file, and a restart cannot tell "no
// sessions yet" from "sessions lost" (this is what
// TestDaemonStartStopRestart in cmd/oriond checks after SIGTERM).
func TestSessionStoreSaveCreatesFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sessions.json")
	s := NewSessionStore(path, true)
	if err := s.LoadError(); err != nil {
		t.Fatalf("LoadError: %v", err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("sessions file missing after Save: %v", err)
	}
	var p persistedSessions
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("sessions file does not parse after Save: %v (contents %q)", err, data)
	}
	if p.Version != 1 {
		t.Fatalf("sessions version = %d, want 1", p.Version)
	}
	// A second Save over the existing file must still succeed.
	if err := s.Save(); err != nil {
		t.Fatalf("second Save: %v", err)
	}
}

// TestPollStreamLogsTransientFailuresAndRecovery: with a logger set,
// each transient getUpdates failure must produce a log line and the
// first success after the outage must produce exactly one recovery
// line — otherwise a network outage is invisible in the daemon log.
// No log line may contain the bot token.
func TestPollStreamLogsTransientFailuresAndRecovery(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(&outageServer{failN: 2, failMode: "http500"})
	defer srv.Close()

	var mu sync.Mutex
	var lines []string
	logf := func(format string, args ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	g, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithLogger(logf))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}

	handled := make(chan string, 1)
	handler := func(ctx context.Context, chatID int64, text string) (string, error) {
		handled <- text
		return "ack", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- g.Poll(ctx, handler) }()

	select {
	case <-handled:
	case err := <-errCh:
		t.Fatalf("Poll returned early (err=%v); want it to keep polling", err)
	case <-time.After(15 * time.Second):
		cancel()
		t.Fatal("handler was never called after the transient outage cleared")
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Poll after cancel: %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Poll did not return after ctx cancel")
	}

	mu.Lock()
	defer mu.Unlock()
	var failures, recoveries int
	for _, line := range lines {
		if strings.Contains(line, testToken) {
			t.Fatalf("log line leaks the token: %q", line)
		}
		switch {
		case strings.Contains(line, "transient getUpdates error"):
			failures++
		case strings.Contains(line, "recovered after 2 failed"):
			recoveries++
		}
	}
	if failures != 2 {
		t.Fatalf("got %d transient-failure log lines, want 2 (lines: %q)", failures, lines)
	}
	if recoveries != 1 {
		t.Fatalf("got %d recovery log lines, want 1 (lines: %q)", recoveries, lines)
	}
}

// TestPollStreamRecoversFromTransientHTTP500: two consecutive HTTP 500
// getUpdates failures (a short api.telegram.org outage) must be
// absorbed by the backoff, and the update that arrives once the API
// is back must still reach the handler — polling must not exit.
func TestPollStreamRecoversFromTransientHTTP500(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(&outageServer{failN: 2, failMode: "http500"})
	defer srv.Close()
	g := newTestGateway(t, srv)

	handled := make(chan string, 1)
	handler := func(ctx context.Context, chatID int64, text string) (string, error) {
		handled <- text
		return "ack", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- g.Poll(ctx, handler) }()

	select {
	case text := <-handled:
		if text != "hello after outage" {
			t.Fatalf("handler got text %q, want %q", text, "hello after outage")
		}
	case err := <-errCh:
		t.Fatalf("Poll returned early (err=%v) during a transient outage; polling must continue", err)
	case <-time.After(15 * time.Second):
		cancel()
		t.Fatal("handler was never called after the transient outage cleared")
	}

	// Polling must still be alive after the recovery: cancelling the
	// context is what stops it, and it must report nil.
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Poll after cancel: %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Poll did not return after ctx cancel")
	}
}

// TestPollStreamRecoversFromConnectionResets: the same recovery, but
// the outage is a transport failure (connection reset, no HTTP
// response at all) — the shape a real network drop takes — rather
// than a clean HTTP 500.
func TestPollStreamRecoversFromConnectionResets(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(&outageServer{failN: 2, failMode: "reset"})
	defer srv.Close()
	g := newTestGateway(t, srv)

	handled := make(chan string, 1)
	handler := func(ctx context.Context, chatID int64, text string) (string, error) {
		handled <- text
		return "ack", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- g.Poll(ctx, handler) }()

	select {
	case text := <-handled:
		if text != "hello after outage" {
			t.Fatalf("handler got text %q, want %q", text, "hello after outage")
		}
	case err := <-errCh:
		t.Fatalf("Poll returned early (err=%v) during connection resets; polling must continue", err)
	case <-time.After(15 * time.Second):
		cancel()
		t.Fatal("handler was never called after the connection resets cleared")
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Poll after cancel: %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Poll did not return after ctx cancel")
	}
}

// TestPollStreamSurvivesSustainedTransientOutage: a longer outage
// (every getUpdates fails with HTTP 500 for the whole run) must keep
// polling in its backoff loop, not exit — only ctx cancellation ends
// it, and then with a nil error.
func TestPollStreamSurvivesSustainedTransientOutage(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(&outageServer{failN: 1 << 30, failMode: "http500"})
	defer srv.Close()
	g := newTestGateway(t, srv)

	handler := func(ctx context.Context, chatID int64, text string) (string, error) {
		t.Error("handler called during a total outage")
		return "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- g.Poll(ctx, handler) }()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Poll exited during a sustained transient outage with err=%v; want it to keep polling until ctx cancel", err)
		}
		// Nil here means ctx cancellation ended it — the expected
		// outcome; the 6s timeout fired, so this is the pass path.
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("Poll did not return after ctx cancel during a sustained outage")
	}
}
