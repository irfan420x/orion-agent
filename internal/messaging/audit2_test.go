package messaging

// Audit2 tests (fresh-eyes audit 2026-11-13): capped API error
// descriptions, fatal Poll errors, handler-panic recovery, and
// serialised GetUpdates.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCallCapsErrorDescription: a failing Bot API call must never
// return a multi-megabyte error string. Both the raw-body fallback
// (a non-JSON error page up to the 10 MiB read cap) and a JSON
// envelope with a huge description are capped.
func TestCallCapsErrorDescription(t *testing.T) {
	huge := strings.Repeat("x", 100_000)

	t.Run("raw body fallback", func(t *testing.T) {
		// A valid-JSON failure envelope with no description and a
		// huge payload: the description falls back to the raw body,
		// which must be capped. (A non-JSON body never reaches this
		// path — it fails earlier, at decode, with a short error
		// that never embeds the body.)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, `{"ok":false,"result":%q}`, huge)
		}))
		defer srv.Close()
		g, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL))
		if err != nil {
			t.Fatalf("NewTelegramGateway: %v", err)
		}
		_, err = g.GetMe(context.Background())
		if err == nil {
			t.Fatal("GetMe on a 502 error page: expected error, got nil")
		}
		if got := len(err.Error()); got > 2048 {
			t.Fatalf("GetMe error is %d chars, want capped well under 2048", got)
		}
		if !strings.Contains(err.Error(), "...") {
			t.Fatalf("GetMe error = %q, want a truncation marker", err.Error())
		}
		if strings.Count(err.Error(), "x") > maxErrorDescriptionRunes+10 {
			t.Fatalf("GetMe error carries most of the %d-char error body", len(huge))
		}
	})

	t.Run("JSON description", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, `{"ok":false,"error_code":500,"description":%q}`, huge)
		}))
		defer srv.Close()
		g, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL))
		if err != nil {
			t.Fatalf("NewTelegramGateway: %v", err)
		}
		_, err = g.GetMe(context.Background())
		if err == nil {
			t.Fatal("GetMe on a JSON 500: expected error, got nil")
		}
		if got := len(err.Error()); got > 2048 {
			t.Fatalf("GetMe error is %d chars, want capped well under 2048", got)
		}
		if !strings.Contains(err.Error(), "HTTP 500, error 500:") {
			t.Fatalf("GetMe error = %q, want the HTTP/error-code prefix kept", err.Error())
		}
	})
}

// TestPollExitsOnFatalAPIError: a revoked token (401) or a second
// poller holding the connection (409) must make Poll return promptly
// instead of retrying forever at the backoff cadence while looking
// alive.
func TestPollExitsOnFatalAPIError(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusConflict} {
		t.Run(fmt.Sprintf("HTTP %d", code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				fmt.Fprintf(w, `{"ok":false,"error_code":%d,"description":"fatal in test"}`, code)
			}))
			defer srv.Close()
			g, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL))
			if err != nil {
				t.Fatalf("NewTelegramGateway: %v", err)
			}
			called := make(chan struct{}, 1)
			handler := func(ctx context.Context, chatID int64, text string) (string, error) {
				called <- struct{}{}
				return "", nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			errCh := make(chan error, 1)
			go func() { errCh <- g.Poll(ctx, handler) }()
			select {
			case err := <-errCh:
				if err == nil {
					t.Fatal("Poll on a fatal API error returned nil, want an error")
				}
				if !strings.Contains(err.Error(), "unrecoverable") {
					t.Fatalf("Poll error = %v, want it to say unrecoverable", err)
				}
			case <-time.After(8 * time.Second):
				cancel()
				t.Fatal("Poll did not return on a fatal API error (it is retrying forever)")
			}
			select {
			case <-called:
				t.Fatal("handler was called despite every GetUpdates failing")
			default:
			}
		})
	}
}

// TestPollRecoversFromHandlerPanic: a panic in the handler must be
// reported to the chat like a handler error and polling must continue
// — it must never kill the daemon process.
func TestPollRecoversFromHandlerPanic(t *testing.T) {
	fake := &fakeTelegram{
		sendCh: make(chan sentMessage, 10),
		updates: []Update{
			{UpdateID: 1, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 1}, Text: "boom", From: &TGUser{ID: 1}}},
			{UpdateID: 2, Message: &TGMessage{MessageID: 2, Chat: TGChat{ID: 2}, Text: "hi", From: &TGUser{ID: 2}}},
		},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)

	handler := func(ctx context.Context, chatID int64, text string) (string, error) {
		if chatID == 1 {
			panic("kaboom-test")
		}
		return "pong", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- g.Poll(ctx, handler) }()

	// Both replies must arrive: the panic apology for chat 1 and the
	// normal reply for chat 2 (polling continued past the panic).
	got := map[int64]string{}
	deadline := time.After(5 * time.Second)
	for len(got) < 2 {
		select {
		case sent := <-fake.sendCh:
			got[sent.ChatID] = sent.Text
		case <-deadline:
			cancel()
			t.Fatalf("only %d/2 replies arrived after a handler panic: %v", len(got), got)
		}
	}
	if !strings.Contains(got[1], "Sorry, I hit an error: kaboom-test") {
		t.Fatalf("chat 1 reply = %q, want a panic apology mentioning kaboom-test", got[1])
	}
	if got[2] != "pong" {
		t.Fatalf("chat 2 reply = %q, want %q (polling must continue after the panic)", got[2], "pong")
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

// TestGetUpdatesConcurrentCallsDoNotDuplicate: two concurrent
// GetUpdates calls must not return the same updates. The offset read,
// the request, and the offset advance are one serialised step, so the
// second caller fetches with the advanced offset and gets nothing.
func TestGetUpdatesConcurrentCallsDoNotDuplicate(t *testing.T) {
	// The fake answers with one update only while the requested
	// offset is 0; any later offset gets an empty result.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("offset") == "0" {
			fmt.Fprint(w, `{"ok":true,"result":[{"update_id":5,"message":{"message_id":1,"chat":{"id":1},"text":"hi"}}]}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":[]}`)
	}))
	defer srv.Close()
	g := newTestGateway(t, srv)

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([][]Update, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = g.GetUpdates(context.Background())
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("GetUpdates %d: %v", i, err)
		}
	}
	delivered := len(results[0]) + len(results[1])
	if delivered != 1 {
		t.Fatalf("concurrent GetUpdates delivered %d updates in total, want exactly 1 (got %d and %d)",
			delivered, len(results[0]), len(results[1]))
	}
	if got := g.Offset(); got != 6 {
		t.Fatalf("offset after concurrent GetUpdates = %d, want 6", got)
	}
}

// TestPollRedactsHandlerErrorReplies: a handler error that carries a
// secret (e.g. a raw provider error embedding an API key) must reach
// the Telegram chat redacted, never verbatim.
func TestPollRedactsHandlerErrorReplies(t *testing.T) {
	// Synthetic fake key, labelled as such: the scanner's generic
	// sk- pattern must catch it. No real credential.
	fakeKey := "sk-" + strings.Repeat("a", 48)
	fake := &fakeTelegram{
		sendCh: make(chan sentMessage, 10),
		updates: []Update{
			{UpdateID: 1, Message: &TGMessage{MessageID: 1, Chat: TGChat{ID: 1}, Text: "hi", From: &TGUser{ID: 1}}},
		},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := newTestGateway(t, srv)

	handler := func(ctx context.Context, chatID int64, text string) (string, error) {
		return "", fmt.Errorf("deepseek: request failed, key %s rejected", fakeKey)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- g.Poll(ctx, handler) }()

	select {
	case sent := <-fake.sendCh:
		if strings.Contains(sent.Text, fakeKey) {
			t.Fatalf("apology sent to chat contains the raw key: %q", sent.Text)
		}
		if !strings.Contains(sent.Text, "[REDACTED]") {
			t.Fatalf("apology sent to chat = %q, want the key replaced by [REDACTED]", sent.Text)
		}
		if !strings.Contains(sent.Text, "Sorry, I hit an error") {
			t.Fatalf("apology sent to chat = %q, want the apology prefix kept", sent.Text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no apology arrived after a handler error")
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
