package messaging

// P2 follow-up tests (audit 2026-11-13): token URL-escaping, redacted
// errors, concurrent Poll, atomic default base URL, CLI EOF prompt.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- P2-30: token URL-escaping ---

// TestCallEscapesTokenInURLPath: a malformed token containing "/" or
// "?" must be escaped when interpolated into the URL path, so it can
// neither alter the path nor inject a query string.
func TestCallEscapesTokenInURLPath(t *testing.T) {
	// Fake token with a hostile shape — never a real Telegram token.
	hostileToken := "bad/token?x=1&y=2"
	var gotPath, gotDecodedPath, gotRawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotDecodedPath = r.URL.Path
		gotRawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"result":{"id":1,"username":"b","first_name":"B"}}`)
	}))
	defer srv.Close()

	g, err := NewTelegramGateway(hostileToken, WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	if _, err := g.GetMe(context.Background()); err != nil {
		t.Fatalf("GetMe: %v", err)
	}
	// The structural characters must stay escaped in the wire path:
	// "/" must not add a path segment, "?" must not start a query.
	if !strings.Contains(gotPath, "%2F") || !strings.Contains(gotPath, "%3F") {
		t.Fatalf("request escaped path = %q, want / and ? escaped (%%2F, %%3F)", gotPath)
	}
	// The token arrives as one path segment and injects no query.
	if gotDecodedPath != "/bot"+hostileToken+"/getMe" {
		t.Fatalf("request path = %q, want %q", gotDecodedPath, "/bot"+hostileToken+"/getMe")
	}
	if gotRawQuery != "" {
		t.Fatalf("request query = %q, want empty (the token must not inject a query)", gotRawQuery)
	}
}

// --- P2-31: redacted errors ---

// TestRedactedErrorDirect: a redactedError's Error() never contains the
// token, errors.Is/As still reach the wrapped error, and Unwrap returns
// a redacted view of the cause — never the raw token-bearing error.
func TestRedactedErrorDirect(t *testing.T) {
	inner := fmt.Errorf("request to %q: %w", "https://api.telegram.org/botSECRET123/getMe", io.EOF)
	re := &redactedError{err: inner, token: "SECRET123"}
	if strings.Contains(re.Error(), "SECRET123") {
		t.Fatalf("redactedError.Error() leaks the token: %q", re.Error())
	}
	if !strings.Contains(re.Error(), "[redacted]") {
		t.Fatalf("redactedError.Error() = %q, want a [redacted] marker", re.Error())
	}
	if !errors.Is(re, io.EOF) {
		t.Fatal("errors.Is(redactedError, io.EOF) = false, want true")
	}
	u := errors.Unwrap(re)
	if u == nil {
		t.Fatal("errors.Unwrap(redactedError) = nil, want the redacted cause")
	}
	if strings.Contains(u.Error(), "SECRET123") {
		t.Fatalf("errors.Unwrap(redactedError) leaks the token: %q", u.Error())
	}
	if u2 := errors.Unwrap(u); u2 != nil && strings.Contains(u2.Error(), "SECRET123") {
		t.Fatalf("second unwrap leaks the token: %q", u2.Error())
	}
	if strings.Contains(fmt.Sprintf("%+v", re), "SECRET123") {
		t.Fatalf("%%+v formatting of the redacted error leaks the token")
	}
}

// TestRedactedErrorUnwrapNeverLeaksToken: for a real gateway failure
// (transport error whose *url.Error embeds the request URL), the
// returned error, every level of unwrapping it, and %+v formatting
// are all token-free — the only safe handle is the wrapped error.
func TestRedactedErrorUnwrapNeverLeaksToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // every request now fails with connection refused

	g, err := NewTelegramGateway(testToken, WithBaseURL(url))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	_, err = g.GetMe(context.Background())
	if err == nil {
		t.Fatal("GetMe against a closed server: expected error, got nil")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("GetMe error leaks the token: %q", err.Error())
	}
	for i, e := 0, err; e != nil && i < 5; i++ {
		if strings.Contains(e.Error(), testToken) {
			t.Fatalf("unwrap level %d leaks the token: %q", i, e.Error())
		}
		e = errors.Unwrap(e)
	}
	if strings.Contains(fmt.Sprintf("%+v", err), testToken) {
		t.Fatalf("%%+v formatting of the gateway error leaks the token")
	}
}

// TestRedactedErrorKeepsErrorsAs: the redaction must not break
// errors.As — a client timeout is still detectable as net.Error with
// Timeout() true, and *url.Error is still reachable through As.
func TestRedactedErrorKeepsErrorsAs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"result":{"id":1,"username":"x","first_name":"X"}}`)
	}))
	defer srv.Close()

	g, err := NewTelegramGateway(testToken,
		WithBaseURL(srv.URL),
		WithHTTPClient(&http.Client{Timeout: 50 * time.Millisecond}))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	_, err = g.GetMe(context.Background())
	if err == nil {
		t.Fatal("GetMe against a slow server: expected timeout error, got nil")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("GetMe error leaks the token: %q", err.Error())
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("GetMe error = %v, want a timeout error detectable via errors.As(net.Error)", err)
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Fatalf("GetMe error = %v, want *url.Error reachable via errors.As", err)
	}
}

// --- P2-32: concurrent Poll ---

// TestPollHandlesChatsConcurrently: with two chats in one batch and a
// slow handler for the first, the second chat's reply must arrive
// while the first handler is still blocked — one slow chat must not
// stall polling/handling for others.
func TestPollHandlesChatsConcurrently(t *testing.T) {
	fake := &fakeTelegram{
		sendCh: make(chan sentMessage, 10),
		updates: []Update{
			{UpdateID: 1, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 1}, Text: "slow"}},
			{UpdateID: 2, Message: &TGMessage{MessageID: 2, Chat: TGChat{ID: 2}, Text: "fast"}},
		},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)

	slowStarted := make(chan struct{})
	releaseSlow := make(chan struct{})
	handler := func(ctx context.Context, chatID int64, text string) (string, error) {
		if chatID == 1 {
			close(slowStarted)
			<-releaseSlow // blocks until the test releases it
			return "slow done", nil
		}
		return "fast done", nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- g.Poll(ctx, handler) }()

	select {
	case <-slowStarted:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("slow handler never started")
	}
	// The fast chat's reply must arrive while the slow handler is
	// still blocked.
	select {
	case sent := <-fake.sendCh:
		if sent.ChatID != 2 || sent.Text != "fast done" {
			t.Fatalf("first reply = %+v, want {ChatID:2 Text:fast done}", sent)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("fast chat's reply was blocked behind the slow handler")
	}
	close(releaseSlow)
	select {
	case sent := <-fake.sendCh:
		if sent.ChatID != 1 || sent.Text != "slow done" {
			t.Fatalf("second reply = %+v, want {ChatID:1 Text:slow done}", sent)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("slow chat's reply never arrived after release")
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

// --- P2-33: atomic default base URL ---

// TestDefaultBaseURLConcurrentAccess: concurrent SetDefaultBaseURL and
// NewTelegramGateway calls must not race (run with -race) and every
// gateway must end up with a non-empty base URL.
func TestDefaultBaseURLConcurrentAccess(t *testing.T) {
	t.Cleanup(func() { SetDefaultBaseURL("") }) // reset to the default
	stop := make(chan struct{})
	var setters sync.WaitGroup
	for i := 0; i < 4; i++ {
		setters.Add(1)
		go func(n int) {
			defer setters.Done()
			for {
				select {
				case <-stop:
					return
				default:
					SetDefaultBaseURL(fmt.Sprintf("https://example-%d.invalid", n))
				}
			}
		}(i)
	}
	var creators sync.WaitGroup
	errCh := make(chan error, 1)
	for i := 0; i < 4; i++ {
		creators.Add(1)
		go func() {
			defer creators.Done()
			for j := 0; j < 500; j++ {
				g, err := NewTelegramGateway(testToken)
				if err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				if g.baseURL == "" {
					select {
					case errCh <- errors.New("gateway with empty baseURL"):
					default:
					}
					return
				}
			}
		}()
	}
	creators.Wait()
	close(stop)
	setters.Wait()
	select {
	case err := <-errCh:
		t.Fatalf("concurrent NewTelegramGateway: %v", err)
	default:
	}
	if got := DefaultBaseURLValue(); got == "" {
		t.Fatal("DefaultBaseURLValue() is empty after concurrent use")
	}
}

// TestDefaultBaseURLValueAndReset: the getter/setter round-trip, and an
// empty set resets to the production default.
func TestDefaultBaseURLValueAndReset(t *testing.T) {
	t.Cleanup(func() { SetDefaultBaseURL("") })
	SetDefaultBaseURL("https://proxy.example.invalid/")
	if got := DefaultBaseURLValue(); got != "https://proxy.example.invalid" {
		t.Fatalf("DefaultBaseURLValue() = %q, want the set value without trailing slash", got)
	}
	SetDefaultBaseURL("")
	if got := DefaultBaseURLValue(); got != "https://api.telegram.org" {
		t.Fatalf("DefaultBaseURLValue() after reset = %q, want the production default", got)
	}
}

// --- P2-34: CLI prompt on EOF ---

// TestRunCLINoTrailingPromptOnEOF: EOF ends the session without a
// trailing "orion> " prompt — both after a line and on empty input.
func TestRunCLINoTrailingPromptOnEOF(t *testing.T) {
	handler := func(ctx context.Context, input string) (string, error) {
		return "echo: " + input, nil
	}
	var out strings.Builder
	if err := RunCLI(context.Background(), strings.NewReader("hello\n"), &out, handler); err != nil {
		t.Fatalf("RunCLI on EOF: %v, want nil", err)
	}
	if got := out.String(); got != "orion> echo: hello\n" {
		t.Fatalf("RunCLI output = %q, want %q (no trailing prompt)", got, "orion> echo: hello\n")
	}

	var empty strings.Builder
	if err := RunCLI(context.Background(), strings.NewReader(""), &empty, handler); err != nil {
		t.Fatalf("RunCLI on empty input: %v, want nil", err)
	}
	if empty.String() != "" {
		t.Fatalf("RunCLI on empty input output = %q, want no output at all", empty.String())
	}
}
