package messaging

// Poll-recovery tests (live incident 2026-11-15 01:28–01:36: 26 failed
// getUpdates attempts on one black-holed pooled connection, then
// self-recovery on a fresh dial). The fix makes that recovery
// deterministic: split clients, HTTP/1.1-only poll transport, 45s
// window+grace deadline, fresh-dial rebuild on the 3rd consecutive
// timeout, 120s outer watchdog. Test shapes copied from spool
// (stalling stand-in server, retry on a different connection) and
// talon (hung poll aborts at window+grace).

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/core"
)

// stallFake is a stand-in Telegram server for poll-recovery tests. It
// records every getUpdates call's remote address (connection identity),
// protocol, and arrival order, tracks the maximum in-flight getUpdates
// count, and can hold chosen calls forever (until the client aborts
// them), fail chosen calls with HTTP 500, or answer chosen calls with
// scripted updates. Non-poll methods answer success.
type stallFake struct {
	mu          sync.Mutex
	addrs       []string // RemoteAddr of each getUpdates call, in order
	protos      []string // r.Proto of each getUpdates call, in order
	calls       int
	inFlight    int
	maxInFlight int
	holdCalls   map[int]bool // getUpdates call numbers held forever
	fail500     map[int]bool // getUpdates call numbers answered HTTP 500
	scripted    map[int][]Update
}

func (f *stallFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if !strings.HasSuffix(r.URL.Path, "/getUpdates") {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":1}}`)
		default:
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		}
		return
	}
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.addrs = append(f.addrs, r.RemoteAddr)
	f.protos = append(f.protos, r.Proto)
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	hold := f.holdCalls[call]
	fail := f.fail500[call]
	updates := f.scripted[call]
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()
	if fail {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"ok":false,"error_code":500,"description":"simulated getUpdates failure"}`)
		return
	}
	if hold {
		// Hold forever: return only when the client aborts the
		// poll (its deadline fires or its context is cancelled).
		<-r.Context().Done()
		return
	}
	if updates == nil {
		updates = []Update{}
	}
	data, _ := json.Marshal(map[string]any{"ok": true, "result": updates})
	w.Write(data)
}

func (f *stallFake) snapshot() (calls int, addrs, protos []string, maxInFlight int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, append([]string(nil), f.addrs...), append([]string(nil), f.protos...), f.maxInFlight
}

// pollTestLogger captures gateway log lines for assertions.
type pollTestLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *pollTestLogger) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *pollTestLogger) contains(substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

func newPollTestGateway(t *testing.T, srv *httptest.Server, logger *pollTestLogger) *TelegramGateway {
	t.Helper()
	g, err := NewTelegramGateway(testToken,
		WithBaseURL(srv.URL),
		WithAllowedChats(77),
		WithLogger(logger.logf))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	return g
}

// okTurn is a PollStream handler that answers every message with "ok"
// and records the texts it saw, in order.
type okTurn struct {
	mu   sync.Mutex
	seen []string
}

func (h *okTurn) fn(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
	h.mu.Lock()
	h.seen = append(h.seen, text)
	h.mu.Unlock()
	return "ok", nil, nil
}

func (h *okTurn) texts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}

// TestPollClientsAreSplitAndPollTransportIsHTTP1Only verifies the spool
// half of the fix: getUpdates has its own client with its own transport,
// pinned to HTTP/1.1, and the send/message client is untouched by it.
func TestPollClientsAreSplitAndPollTransportIsHTTP1Only(t *testing.T) {
	g, err := NewTelegramGateway(testToken)
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	if g.pollClient == nil {
		t.Fatal("poll client is nil")
	}
	if g.pollClient == g.client {
		t.Fatal("poll client is the send client: getUpdates must have its own client")
	}
	if g.pollClient.Timeout != pollHTTPTimeout {
		t.Fatalf("poll client timeout = %v, want %v (30s window + 15s grace)", g.pollClient.Timeout, pollHTTPTimeout)
	}
	if g.client.Timeout != defaultHTTPTimeout {
		t.Fatalf("send client timeout = %v, want %v", g.client.Timeout, defaultHTTPTimeout)
	}
	tr, ok := g.pollClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("poll transport = %T, want *http.Transport", g.pollClient.Transport)
	}
	if tr.ForceAttemptHTTP2 {
		t.Fatal("poll transport still attempts HTTP/2: it must be pinned to HTTP/1.1 so a timed-out poll closes its connection")
	}
	// The bool alone is NOT the pin (regression 2026-11-15: Clone()
	// + ForceAttemptHTTP2=false still offered h2 in ALPN with no h2
	// implementation, and every getUpdates failed with `malformed
	// HTTP response`). The real pin is the ALPN offer plus the
	// disabled h2 implementation — assert both, not just the bool.
	if tr.TLSNextProto == nil {
		t.Fatal("poll transport TLSNextProto is nil: a nil map lets the transport configure HTTP/2 on first use; it must be a non-nil (empty) map")
	}
	if _, hasH2 := tr.TLSNextProto["h2"]; hasH2 {
		t.Fatal("poll transport TLSNextProto has an h2 handler: the poll transport must have no HTTP/2 implementation")
	}
	if cfg := tr.TLSClientConfig; cfg != nil {
		for _, p := range cfg.NextProtos {
			if p == "h2" {
				t.Fatalf("poll transport TLSClientConfig.NextProtos = %v: it must not offer h2 in ALPN (ForceAttemptHTTP2=false does not remove it)", cfg.NextProtos)
			}
		}
		if len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != "http/1.1" {
			t.Fatalf("poll transport TLSClientConfig.NextProtos = %v, want [http/1.1]", cfg.NextProtos)
		}
	}
	// The pin must not mutate the shared default transport: it still
	// offers h2 and still has its h2 implementation.
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		if dt.TLSClientConfig != nil {
			hasH2 := false
			for _, p := range dt.TLSClientConfig.NextProtos {
				if p == "h2" {
					hasH2 = true
				}
			}
			if !hasH2 {
				t.Fatalf("default transport TLSClientConfig.NextProtos = %v after newPollTransport(): the pin must not have removed h2 from the shared default", dt.TLSClientConfig.NextProtos)
			}
		}
		if _, hasH2 := dt.TLSNextProto["h2"]; !hasH2 {
			t.Fatal("default transport TLSNextProto lost its h2 handler after newPollTransport(): the pin must not mutate the shared default")
		}
	}
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		if tr == dt {
			t.Fatal("poll transport IS the default transport: it must be a clone with its own pool")
		}
		if dt.ForceAttemptHTTP2 == tr.ForceAttemptHTTP2 {
			t.Fatal("poll transport did not change ForceAttemptHTTP2 from the default transport")
		}
	}
	// A custom send client (WithHTTPClient) must not become the poll
	// client: the two wait on different clocks.
	custom := &http.Client{Timeout: 50 * time.Millisecond}
	g2, err := NewTelegramGateway(testToken, WithHTTPClient(custom))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	if g2.client != custom {
		t.Fatal("WithHTTPClient did not set the send client")
	}
	if g2.pollClient == custom || g2.pollClient.Timeout != pollHTTPTimeout {
		t.Fatal("WithHTTPClient leaked into the poll client")
	}
}

// TestIsTimeoutError verifies timeout-class detection is by error
// inspection (context deadline / net.Error Timeout), never by matching
// error strings — and that a 429/5xx apiError is NOT a timeout.
func TestIsTimeoutError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context deadline", context.DeadlineExceeded, true},
		{"wrapped deadline", fmt.Errorf("request: %w", context.DeadlineExceeded), true},
		{"net timeout", &net.DNSError{IsTimeout: true}, true},
		{"net non-timeout", &net.DNSError{IsTimeout: false}, false},
		{"plain error", errors.New("boom"), false},
		{"429 apiError", &apiError{Method: "getUpdates", StatusCode: 429}, false},
		{"500 apiError", &apiError{Method: "getUpdates", StatusCode: 500}, false},
		// A string that merely MENTIONS a deadline must not count:
		// detection is by error kind, not substring.
		{"string mention only", errors.New("context deadline exceeded (in a log line, not an error)"), false},
	}
	for _, tc := range cases {
		if got := isTimeoutError(tc.err); got != tc.want {
			t.Errorf("isTimeoutError(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
	// The redaction wrapper must not hide the timeout kind: a timeout
	// that travelled through call() is still detectable.
	g, err := NewTelegramGateway(testToken)
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	redacted := g.redact(fmt.Errorf("request: %w", context.DeadlineExceeded))
	if !isTimeoutError(redacted) {
		t.Fatalf("isTimeoutError(redacted deadline) = false, want true (redaction must preserve errors.Is/As)")
	}
}

// TestPollHungPollAbortsAtDeadlineAndNextPollSucceeds is the talon test:
// a hung poll is aborted at window+grace and the next poll succeeds.
func TestPollHungPollAbortsAtDeadlineAndNextPollSucceeds(t *testing.T) {
	fake := &stallFake{holdCalls: map[int]bool{1: true}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newPollTestGateway(t, srv, &pollTestLogger{})
	// Shrink the deadline so the test does not wait out a real 45s.
	g.pollTimeout = 200 * time.Millisecond

	start := time.Now()
	_, err := g.GetUpdates(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("GetUpdates against a hung server: expected timeout error, got nil")
	}
	if !isTimeoutError(err) {
		t.Fatalf("GetUpdates error = %v, want a timeout-class error", err)
	}
	// Aborted at the deadline, not at the 45s production deadline and
	// not left hanging: generous bounds, the point is the order of
	// magnitude, not the exact millisecond.
	if elapsed > 10*time.Second {
		t.Fatalf("hung poll took %v to abort, want it aborted near the 200ms deadline", elapsed)
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("hung poll aborted after %v, faster than the deadline — it did not wait out the poll", elapsed)
	}

	// The next poll succeeds: the abort did not poison the gateway.
	updates, err := g.GetUpdates(context.Background())
	if err != nil {
		t.Fatalf("GetUpdates after aborted poll: %v", err)
	}
	if len(updates) != 0 {
		t.Fatalf("second GetUpdates = %d updates, want 0", len(updates))
	}
	if h := g.PollHealth(); h.Rebuilds != 0 {
		t.Fatalf("one-shot GetUpdates rebuilt the transport %d time(s), want 0 (rebuilds belong to the PollStream loop)", h.Rebuilds)
	}
}

// TestPollRebuildUsesFreshConnection is the spool test: a stand-in
// server holds the first three polls forever and answers later ones;
// the retry must arrive on a DIFFERENT CONNECTION than the stalled one,
// the transport must be rebuilt on the 3rd consecutive timeout, and no
// two polls may ever be in flight at once.
func TestPollRebuildUsesFreshConnection(t *testing.T) {
	fake := &stallFake{holdCalls: map[int]bool{1: true, 2: true, 3: true}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	logger := &pollTestLogger{}
	g := newPollTestGateway(t, srv, logger)
	g.pollTimeout = 300 * time.Millisecond

	turn := &okTurn{}
	cancel, errCh := runPollStream(t, g, turn.fn)
	// Three 300ms timeouts with 1s then 2s backoff take ~3.9s; allow
	// generous time under -race / full-suite load.
	waitForLong(t, "poll transport rebuild", func() bool {
		return g.PollHealth().Rebuilds >= 1
	})
	// Let the post-rebuild poll complete so health settles, then stop.
	waitForLong(t, "completed poll after rebuild", func() bool {
		calls, _, _, _ := fake.snapshot()
		return calls >= 4 && !g.PollHealth().LastCompletedAt.IsZero()
	})
	stopPollStream(t, cancel, errCh)

	calls, addrs, protos, maxInFlight := fake.snapshot()
	if calls < 4 {
		t.Fatalf("getUpdates calls = %d, want >= 4 (3 held + 1 answered)", calls)
	}
	if len(addrs) < 4 {
		t.Fatalf("recorded remote addrs = %d, want >= 4", len(addrs))
	}
	// Every retry after the stalled first poll arrived on a different
	// connection: the HTTP/1.1 pin means each timed-out poll closes
	// its connection, so the next attempt dials fresh.
	for i := 1; i < 4; i++ {
		if addrs[i] == addrs[0] {
			t.Fatalf("getUpdates call %d arrived on %s, the same connection as the stalled call 1 (%s): the retry reused the dead connection", i+1, addrs[i], addrs[0])
		}
	}
	for i, proto := range protos {
		if proto != "HTTP/1.1" {
			t.Fatalf("getUpdates call %d spoke %s, want HTTP/1.1 (poll transport must be pinned)", i+1, proto)
		}
	}
	if maxInFlight != 1 {
		t.Fatalf("max in-flight getUpdates = %d, want 1 (fetchMu serialisation: no duplicate poll ever in flight)", maxInFlight)
	}
	if !logger.contains("rebuilt poll transport") {
		t.Fatalf("no transport-rebuild log line; log lines: %v", logger.lines)
	}
	if !logger.contains("3 consecutive timeout") {
		t.Fatalf("rebuild log line does not name the 3-consecutive-timeout trigger; log lines: %v", logger.lines)
	}
	if h := g.PollHealth(); h.Rebuilds < 1 {
		t.Fatalf("PollHealth().Rebuilds = %d, want >= 1", h.Rebuilds)
	} else if h.ConsecutiveTimeouts != 0 {
		t.Fatalf("PollHealth().ConsecutiveTimeouts = %d after recovery, want 0", h.ConsecutiveTimeouts)
	}
	if got := g.Offset(); got != 0 {
		t.Fatalf("offset = %d, want 0 (empty results must not move it)", got)
	}
}

// TestPollWatchdogRebuildsWithoutThreeTimeouts proves the outer watchdog
// fires on its own: with the watchdog shrunk to 400ms and every poll
// held, the first rebuild must be a watchdog rebuild — it happens after
// ~1.3s (one 300ms timeout + 1s backoff), long before the N=3 trigger
// could fire (~3.9s).
func TestPollWatchdogRebuildsWithoutThreeTimeouts(t *testing.T) {
	fake := &stallFake{holdCalls: map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true, 6: true, 7: true, 8: true, 9: true, 10: true}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	logger := &pollTestLogger{}
	g := newPollTestGateway(t, srv, logger)
	g.pollTimeout = 300 * time.Millisecond
	g.pollWatchdogInterval = 400 * time.Millisecond

	turn := &okTurn{}
	cancel, errCh := runPollStream(t, g, turn.fn)
	waitForLong(t, "watchdog rebuild", func() bool {
		return g.PollHealth().Rebuilds >= 1
	})
	stopPollStream(t, cancel, errCh)

	if !logger.contains("watchdog") {
		t.Fatalf("no watchdog rebuild log line; log lines: %v", logger.lines)
	}
	// The FIRST rebuild must be the watchdog's, not the N=3 trigger's.
	logger.mu.Lock()
	first := ""
	for _, line := range logger.lines {
		if strings.Contains(line, "rebuilt poll transport") {
			first = line
			break
		}
	}
	logger.mu.Unlock()
	if first == "" {
		t.Fatal("no rebuild log line at all")
	}
	if !strings.Contains(first, "watchdog") {
		t.Fatalf("first rebuild = %q, want the watchdog trigger (N=3 needs ~3.9s; the watchdog must win)", first)
	}
}

// TestPollRebuildPreservesOffsetAndSerialises is the regression test for
// the rebuild: update 100 is delivered and completed, then three polls
// stall and the transport rebuilds — the offset must stay at 101, the
// update must be delivered exactly once, and no two polls may ever be
// in flight.
func TestPollRebuildPreservesOffsetAndSerialises(t *testing.T) {
	fake := &stallFake{
		holdCalls: map[int]bool{2: true, 3: true, 4: true},
		scripted: map[int][]Update{
			1: {{UpdateID: 100, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 77}, Text: "hi", From: &TGUser{ID: 77}}}},
		},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	logger := &pollTestLogger{}
	g := newPollTestGateway(t, srv, logger)
	g.pollTimeout = 300 * time.Millisecond

	turn := &okTurn{}
	cancel, errCh := runPollStream(t, g, turn.fn)
	// The update must be delivered AND the rebuild must happen.
	waitForLong(t, "update delivered + transport rebuilt", func() bool {
		return len(turn.texts()) >= 1 && g.PollHealth().Rebuilds >= 1
	})
	stopPollStream(t, cancel, errCh)

	if texts := turn.texts(); len(texts) != 1 || texts[0] != "hi" {
		t.Fatalf("handler saw texts %v, want exactly [\"hi\"] (no duplicate delivery across the rebuild)", texts)
	}
	if got := g.Offset(); got != 101 {
		t.Fatalf("offset = %d, want 101 (unchanged across the rebuild: only handler completion moves it)", got)
	}
	if _, _, _, maxInFlight := fake.snapshot(); maxInFlight != 1 {
		t.Fatalf("max in-flight getUpdates = %d, want 1", maxInFlight)
	}
	if h := g.PollHealth(); h.Rebuilds < 1 {
		t.Fatalf("PollHealth().Rebuilds = %d, want >= 1", h.Rebuilds)
	}
}

// TestPollNonTimeoutErrorsDoNotRebuild proves the rebuild is gated on
// timeout-class errors only: repeated HTTP 500s back off but never
// rebuild the transport.
func TestPollNonTimeoutErrorsDoNotRebuild(t *testing.T) {
	fake := &stallFake{fail500: map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true, 6: true, 7: true, 8: true, 9: true, 10: true}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	logger := &pollTestLogger{}
	g := newPollTestGateway(t, srv, logger)
	g.pollTimeout = 300 * time.Millisecond

	turn := &okTurn{}
	cancel, errCh := runPollStream(t, g, turn.fn)
	// Two attempts (immediate 500, 1s backoff, immediate 500) inside
	// this window; the point is that none of them rebuilds.
	time.Sleep(2500 * time.Millisecond)
	stopPollStream(t, cancel, errCh)

	if h := g.PollHealth(); h.Rebuilds != 0 {
		t.Fatalf("PollHealth().Rebuilds = %d after HTTP 500s, want 0 (rebuild is timeout-class only)", h.Rebuilds)
	} else if h.ConsecutiveTimeouts != 0 {
		t.Fatalf("PollHealth().ConsecutiveTimeouts = %d after HTTP 500s, want 0", h.ConsecutiveTimeouts)
	}
	if !logger.contains("transient getUpdates error") {
		t.Fatalf("no transient-error log line for the 500s; log lines: %v", logger.lines)
	}
	calls, _, _, _ := fake.snapshot()
	if calls < 2 {
		t.Fatalf("getUpdates calls = %d, want >= 2 (polling must keep retrying 500s with backoff)", calls)
	}
}

// TestPollHealthReportsUnhealthyInBackoff verifies the health contract
// directly: a poller in backoff (or never started) must NOT report
// healthy, and a poller with a completed poll and no outstanding
// timeouts reports healthy.
func TestPollHealthReportsUnhealthyInBackoff(t *testing.T) {
	g, err := NewTelegramGateway(testToken)
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	if h := g.PollHealth(); h.Healthy {
		t.Fatalf("PollHealth before polling started = healthy, want unhealthy (%q)", h.Reason)
	}
	g.notePollLoopStarted()
	g.setPollBackoff(true)
	if h := g.PollHealth(); h.Healthy {
		t.Fatal("PollHealth in backoff = healthy, want unhealthy")
	} else if !strings.Contains(h.Reason, "backing off") {
		t.Fatalf("PollHealth reason in backoff = %q, want it to name the backoff", h.Reason)
	}
	g.setPollBackoff(false)
	g.notePollCompleted()
	if h := g.PollHealth(); !h.Healthy {
		t.Fatalf("PollHealth after a completed poll = unhealthy (%q), want healthy", h.Reason)
	}
	g.notePollTimeout()
	if h := g.PollHealth(); h.Healthy {
		t.Fatal("PollHealth with a consecutive timeout outstanding = healthy, want unhealthy")
	} else if h.ConsecutiveTimeouts != 1 {
		t.Fatalf("PollHealth().ConsecutiveTimeouts = %d, want 1", h.ConsecutiveTimeouts)
	}
	if s := g.PollHealth().String(); !strings.Contains(s, "healthy=") || !strings.Contains(s, "rebuilds=") {
		t.Fatalf("PollHealth().String() = %q, want the one-line snapshot shape", s)
	}
}

// TestPollSendStillUsesSendClient proves the split did not reroute
// sends: a sendMessage against a slow server times out on the SEND
// client's deadline, and a getUpdates against the same slow server is
// governed by the poll deadline instead. (The send half is covered by
// TestCallRedactsTokenOnTimeout; this pins the routing.)
func TestPollSendStillUsesSendClient(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			// Answer polls at once: the poll client must not be the
			// 50ms send client, or this would time out.
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true,"result":[]}`)
			return
		}
		time.Sleep(500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"result":{"id":1,"username":"x","first_name":"X"}}`)
	}))
	defer slow.Close()

	g, err := NewTelegramGateway(testToken,
		WithBaseURL(slow.URL),
		WithHTTPClient(&http.Client{Timeout: 50 * time.Millisecond}))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	// getUpdates answers at once: it must succeed despite the 50ms
	// SEND client, because polls ride the poll client.
	if _, err := g.GetUpdates(context.Background()); err != nil {
		t.Fatalf("GetUpdates with a 50ms send client: %v (polls must not use the send client)", err)
	}
	// getMe is slow: it must fail on the 50ms send client.
	if _, err := g.GetMe(context.Background()); err == nil {
		t.Fatal("GetMe against a slow server with a 50ms send client: expected timeout, got nil")
	} else if !isTimeoutError(err) {
		t.Fatalf("GetMe error = %v, want a timeout-class error", err)
	}
}

// ---------------------------------------------------------------------------
// HTTP/1.1 pin behavioural test (poll regression 2026-11-15, b6aaa62).
//
// b6aaa62's newPollTransport() was Clone() + ForceAttemptHTTP2=false
// only. That clone still offered h2 in ALPN with no h2 implementation,
// so api.telegram.org selected h2 and every getUpdates failed with
// `malformed HTTP response "\x00\x00\x12\x04…"` — three live deploys.
// The tests below prove the pin against a server that REALLY negotiates
// h2. They deliberately do NOT use httptest.NewTLSServer: it does not
// negotiate h2 server-side, so even the broken transport passes
// against it and the test would prove nothing.
// ---------------------------------------------------------------------------

// testSelfSignedCert generates a self-signed leaf for 127.0.0.1 /
// localhost, for the h2 test server below.
func testSelfSignedCert(t *testing.T) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create test certificate: %v", err)
	}
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
	)
	if err != nil {
		t.Fatalf("load test certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse test certificate: %v", err)
	}
	return cert, leaf
}

// h2PollServer is a real TLS server with HTTP/2 enabled (http.Server +
// ServeTLS auto-configures server-side h2). It records every request's
// server-side negotiated protocol and r.Proto, and answers getUpdates
// with an empty result.
type h2PollServer struct {
	url  string
	leaf *x509.Certificate
	mu   sync.Mutex
	// negotiated[i] / protos[i] describe the i-th request received.
	negotiated []string
	protos     []string
	srv        *http.Server
}

func (s *h2PollServer) snapshot() (negotiated, protos []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.negotiated...), append([]string(nil), s.protos...)
}

func startH2PollServer(t *testing.T) *h2PollServer {
	t.Helper()
	cert, leaf := testSelfSignedCert(t)
	s := &h2PollServer{leaf: leaf}
	s.srv = &http.Server{
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			proto := ""
			if r.TLS != nil {
				proto = r.TLS.NegotiatedProtocol
			}
			s.mu.Lock()
			s.negotiated = append(s.negotiated, proto)
			s.protos = append(s.protos, r.Proto)
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true,"result":[]}`)
		}),
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for h2 test server: %v", err)
	}
	s.url = "https://" + ln.Addr().String()
	t.Cleanup(func() { s.srv.Close() })
	go s.srv.ServeTLS(ln, "", "")
	return s
}

// trustTestLeaf adds the test server's self-signed leaf to the
// transport's root pool, cloning the TLS config so the pin fields
// (NextProtos, TLSNextProto) are preserved exactly.
func trustTestLeaf(t *testing.T, tr *http.Transport, leaf *x509.Certificate) {
	t.Helper()
	cfg := &tls.Config{}
	if tr.TLSClientConfig != nil {
		cfg = tr.TLSClientConfig.Clone()
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	cfg.RootCAs = pool
	tr.TLSClientConfig = cfg
}

// oldBrokenPollTransport replicates the b6aaa62 newPollTransport
// verbatim (Clone + ForceAttemptHTTP2=false only) as the negative
// control. It must FAIL against an h2-negotiating server; if it does
// not, the test server is not negotiating h2 and the test proves
// nothing.
func oldBrokenPollTransport() *http.Transport {
	dt := http.DefaultTransport.(*http.Transport)
	tr := dt.Clone()
	tr.ForceAttemptHTTP2 = false
	return tr
}

// TestPollTransportNegotiatesHTTP1AgainstH2Server is the behavioural
// proof of the HTTP/1.1 pin: against a server that really negotiates
// h2, the production poll client must complete a getUpdates over
// HTTP/1.1 — and the old b6aaa62 transport must fail on the same
// server with the live error.
func TestPollTransportNegotiatesHTTP1AgainstH2Server(t *testing.T) {
	srv := startH2PollServer(t)

	// Snapshot the shared default transport BEFORE the factory runs:
	// the pin must leave it alone.
	dt := http.DefaultTransport.(*http.Transport)
	var beforeNextProtos []string
	if dt.TLSClientConfig != nil {
		beforeNextProtos = append([]string(nil), dt.TLSClientConfig.NextProtos...)
	}
	_, beforeHasH2Handler := dt.TLSNextProto["h2"]

	// Production factory, not a hand-built transport.
	client := newPollHTTPClient()
	client.Timeout = 10 * time.Second
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("poll transport = %T, want *http.Transport", client.Transport)
	}
	if tr.TLSNextProto == nil {
		t.Fatal("poll transport TLSNextProto is nil: it must be a non-nil (empty) map so the transport never configures HTTP/2")
	}
	if _, hasH2 := tr.TLSNextProto["h2"]; hasH2 {
		t.Fatal("poll transport TLSNextProto has an h2 handler")
	}
	cfg := tr.TLSClientConfig
	if cfg == nil {
		t.Fatal("poll transport TLSClientConfig is nil: the clone must carry an explicit HTTP/1.1-only NextProtos")
	}
	for _, p := range cfg.NextProtos {
		if p == "h2" {
			t.Fatalf("poll transport NextProtos = %v: must not offer h2 in ALPN", cfg.NextProtos)
		}
	}
	if len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != "http/1.1" {
		t.Fatalf("poll transport NextProtos = %v, want [http/1.1]", cfg.NextProtos)
	}

	// The shared default transport must be untouched by the factory.
	if dt.TLSClientConfig == nil {
		t.Fatal("default transport TLSClientConfig is nil after newPollTransport(): expected the lazy HTTP/2 setup to have run")
	}
	hasH2 := false
	for _, p := range dt.TLSClientConfig.NextProtos {
		if p == "h2" {
			hasH2 = true
		}
	}
	if !hasH2 {
		t.Fatalf("default transport NextProtos = %v after newPollTransport(): the pin must not remove h2 from the shared default", dt.TLSClientConfig.NextProtos)
	}
	if beforeNextProtos != nil {
		if len(dt.TLSClientConfig.NextProtos) != len(beforeNextProtos) {
			t.Fatalf("default transport NextProtos changed from %v to %v", beforeNextProtos, dt.TLSClientConfig.NextProtos)
		}
		for i, p := range beforeNextProtos {
			if dt.TLSClientConfig.NextProtos[i] != p {
				t.Fatalf("default transport NextProtos changed from %v to %v", beforeNextProtos, dt.TLSClientConfig.NextProtos)
			}
		}
	}
	if _, hasH2Handler := dt.TLSNextProto["h2"]; !hasH2Handler {
		t.Fatalf("default transport TLSNextProto lost its h2 handler after newPollTransport() (had one before: %v)", beforeHasH2Handler)
	}

	trustTestLeaf(t, tr, srv.leaf)

	// Transport-level proof: a real getUpdates-shaped request.
	resp, err := client.Get(srv.url + "/bot" + testToken + "/getUpdates?timeout=30")
	if err != nil {
		t.Fatalf("GET getUpdates with the production poll client: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET getUpdates status = %d, want 200", resp.StatusCode)
	}
	if resp.Proto != "HTTP/1.1" {
		t.Fatalf("GET getUpdates resp.Proto = %q, want HTTP/1.1 (the server negotiates h2 — a broken pin would speak h2 or fail)", resp.Proto)
	}
	negotiated, protos := srv.snapshot()
	if len(negotiated) != 1 || negotiated[0] != "http/1.1" {
		t.Fatalf("server-side negotiated protocol = %v, want [http/1.1]", negotiated)
	}
	if len(protos) != 1 || protos[0] != "HTTP/1.1" {
		t.Fatalf("server-side request proto = %v, want [HTTP/1.1]", protos)
	}

	// Production-path proof: a real gateway GetUpdates over the same
	// server, through call() routing and the poll client.
	g, err := NewTelegramGateway(testToken, WithBaseURL(srv.url))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	gtr, ok := g.pollClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("gateway poll transport = %T, want *http.Transport", g.pollClient.Transport)
	}
	trustTestLeaf(t, gtr, srv.leaf)
	g.pollTimeout = 10 * time.Second
	updates, err := g.GetUpdates(context.Background())
	if err != nil {
		t.Fatalf("GetUpdates via the production poll client: %v", err)
	}
	if len(updates) != 0 {
		t.Fatalf("GetUpdates = %d updates, want 0", len(updates))
	}
	negotiated, protos = srv.snapshot()
	if len(negotiated) != 2 {
		t.Fatalf("server saw %d requests, want 2 (direct GET + gateway GetUpdates)", len(negotiated))
	}
	for i, p := range negotiated {
		if p != "http/1.1" {
			t.Fatalf("server-side negotiated protocol for request %d = %q, want http/1.1", i+1, p)
		}
		if protos[i] != "HTTP/1.1" {
			t.Fatalf("server-side request proto for request %d = %q, want HTTP/1.1", i+1, protos[i])
		}
	}

	// Negative control: the old b6aaa62 transport must fail on this
	// exact server with the live error. If it does not, the server is
	// not negotiating h2 and this test proves nothing.
	t.Run("broken b6aaa62 transport fails", func(t *testing.T) {
		old := oldBrokenPollTransport()
		if oldCfg := old.TLSClientConfig; oldCfg != nil {
			offersH2 := false
			for _, p := range oldCfg.NextProtos {
				if p == "h2" {
					offersH2 = true
				}
			}
			if !offersH2 {
				t.Fatalf("negative control is invalid: old transport NextProtos = %v, want it to still offer h2 (else the trap is not armed)", oldCfg.NextProtos)
			}
		} else {
			t.Fatal("negative control is invalid: old transport TLSClientConfig is nil, want it to carry the inherited [h2 http/1.1] offer")
		}
		oldClient := &http.Client{Timeout: 10 * time.Second, Transport: old}
		trustTestLeaf(t, old, srv.leaf)
		_, err := oldClient.Get(srv.url + "/bot" + testToken + "/getUpdates?timeout=30")
		if err == nil {
			t.Fatal("negative control: the old Clone()+ForceAttemptHTTP2=false transport SUCCEEDED against the h2 server — the server is not negotiating h2, so this test proves nothing")
		}
		// Either failure proves the server negotiated h2: had ALPN
		// picked HTTP/1.1 the request would have succeeded. The
		// usual error is "malformed HTTP response" (h2 SETTINGS
		// read as HTTP/1.1); bare "EOF" is also possible — the
		// server rejects the client's bogus preface and closes
		// the connection, and with unread request bytes buffered
		// the close sends RST, discarding the in-flight SETTINGS
		// frame before the client reads it.
		if !strings.Contains(err.Error(), "malformed HTTP response") && !strings.Contains(err.Error(), "EOF") {
			t.Fatalf("negative control: old transport failed with %v, want malformed HTTP response or EOF (h2 negotiated, request rejected)", err)
		}
		t.Logf("negative control failed as expected: %v", err)
	})
}
