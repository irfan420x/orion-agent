package model

// Timeout-design tests (Audit 2 §6): the three distinct timeouts —
// connection, progress (first byte / mid-stream silence), total —
// proved against fake slow servers. No live network is used; the
// timeout vars are tuned down so each behaviour is proved in about a
// second instead of waiting out production values.
//
// NOTE: the tests mutate package-level timeout vars and rebuild the
// shared client, so they must not run in parallel with other model
// tests (none of this package uses t.Parallel).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// setTestTimeouts tunes the timeout vars and rebuilds the shared
// client so the transport fields (ResponseHeaderTimeout,
// TLSHandshakeTimeout) pick the tuned values up. Providers must be
// created AFTER calling it. Everything is restored on cleanup.
func setTestTimeouts(t *testing.T, connect, progress, total time.Duration) {
	t.Helper()
	oldConnect, oldProgress, oldTotal := connectTimeout, progressTimeout, totalTimeout
	oldClient := sharedHTTPClient
	connectTimeout, progressTimeout, totalTimeout = connect, progress, total
	sharedHTTPClient = newSharedHTTPClient()
	t.Cleanup(func() {
		connectTimeout, progressTimeout, totalTimeout = oldConnect, oldProgress, oldTotal
		sharedHTTPClient = oldClient
	})
}

const openAIChatOK = `{"choices":[{"message":{"role":"assistant","content":"four"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`

// --- (a) hung connection -------------------------------------------------

// TestHungConnectionFailsFast: a server that accepts the connection
// but never responds must fail well under the old 120s — the
// first-byte (ResponseHeaderTimeout) bound fires per attempt and the
// attempt is retried, so the call ends after the retry budget, still
// in ~1s with tuned timeouts.
func TestHungConnectionFailsFast(t *testing.T) {
	setTestTimeouts(t, 500*time.Millisecond, 300*time.Millisecond, 30*time.Second)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		// Accept and never respond. Block boundedly: the client
		// gives up on its own timeout, but the server cannot
		// reliably notice a disconnect while the request body is
		// unread (no background read runs then), so blocking on
		// r.Context().Done() alone would hang srv.Close()
		// forever.
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	start := time.Now()
	_, err := mustProvider(t, "openai", srv.URL).Chat(context.Background(), sampleRequest())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Chat against a hung server: expected an error, got nil")
	}
	if !isProgressTimeout(err) && !strings.Contains(strings.ToLower(err.Error()), "timeout") {
		t.Fatalf("Chat error = %v, want a timeout error", err)
	}
	// 3 attempts: the header timeout is retryable, so a hung server
	// is retried, not given up on after one try.
	if got := requests.Load(); got != maxAttempts {
		t.Fatalf("server saw %d requests, want %d (retries must happen)", got, maxAttempts)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Chat took %v, want well under the old 120s", elapsed)
	}
	t.Logf("hung connection failed in %v after %d attempts: %v", elapsed, requests.Load(), err)
}

// TestHungConnectionDialFailsFast: a black-holed endpoint (SYNs
// dropped) must fail on the dial timeout, not hang. 10.255.255.1 is
// unroutable from this host; if the sandbox rejects the route
// outright the dial still fails fast.
func TestHungConnectionDialFailsFast(t *testing.T) {
	setTestTimeouts(t, 500*time.Millisecond, 5*time.Second, 30*time.Second)
	p, err := NewProvider(ProviderConfig{Name: "openai", APIKey: testKey, BaseURL: "http://10.255.255.1:81", Model: "test-model"})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	start := time.Now()
	_, err = p.Chat(context.Background(), sampleRequest())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Chat against a black-holed endpoint: expected an error, got nil")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Chat took %v, want a fast dial failure", elapsed)
	}
	t.Logf("black-holed dial failed in %v: %v", elapsed, err)
}

// --- (b) mid-stream stall + retry ---------------------------------------

// TestStreamStallBeforeFirstChunkRetries: a stream that sends headers
// and then goes silent aborts on the progress timeout and is set up
// again; the second attempt streams normally and the call succeeds,
// with each chunk delivered exactly once.
func TestStreamStallBeforeFirstChunkRetries(t *testing.T) {
	setTestTimeouts(t, 5*time.Second, 300*time.Millisecond, 30*time.Second)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			// The Chat fallback path is not expected here, but
			// answer it anyway so a fallback still succeeds.
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, openAIChatOK)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if n == 1 {
			// Headers out, then silence: the progress reader
			// must abort this attempt. Bounded wait (see
			// TestHungConnectionFailsFast).
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"four\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var chunks []string
	start := time.Now()
	resp, err := mustProvider(t, "openai", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error {
		chunks = append(chunks, chunk)
		return nil
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if resp == nil || resp.Message.Content != "four" {
		t.Fatalf("Stream content = %q, want four", respMessageContent(resp))
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("server saw %d stream requests, want 2 (the stalled attempt must be retried)", got)
	}
	if len(chunks) != 1 || chunks[0] != "four" {
		t.Fatalf("chunks = %q, want exactly [four] (no duplicates from the retry)", chunks)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Stream took %v, want fast", elapsed)
	}
	t.Logf("stalled stream retried and succeeded in %v after %d requests", elapsed, requests.Load())
}

// TestStreamStallAfterChunksErrorsClearly: a stream that delivers a
// chunk and then goes silent must NOT be retried — the chunk is
// already live with the caller, so re-running the stream would
// duplicate it — and must fail with a clear progress-timeout error,
// not a silent hang.
func TestStreamStallAfterChunksErrorsClearly(t *testing.T) {
	setTestTimeouts(t, 5*time.Second, 300*time.Millisecond, 30*time.Second)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		w.(http.Flusher).Flush()
		// Stall after the first chunk, on every attempt.
		// Bounded wait (see TestHungConnectionFailsFast).
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
		_ = n
	}))
	defer srv.Close()

	var chunks []string
	start := time.Now()
	_, err := mustProvider(t, "openai", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error {
		chunks = append(chunks, chunk)
		return nil
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Stream stalled after a chunk: expected an error, got nil")
	}
	if !isProgressTimeout(err) {
		t.Fatalf("Stream error = %v, want a progress timeout", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("server saw %d stream requests, want 1 (no retry once chunks are live)", got)
	}
	if len(chunks) != 1 || chunks[0] != "first" {
		t.Fatalf("chunks = %q, want exactly [first]", chunks)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Stream took %v, want the progress window, not a hang", elapsed)
	}
	t.Logf("stream stalled after chunks failed in %v with a clear timeout: %v", elapsed, err)
}

// TestChatBodyStallRetries: a non-streaming Chat whose server sends
// headers and then stalls on the body must abort the read on the
// progress timeout and retry the request.
func TestChatBodyStallRetries(t *testing.T) {
	setTestTimeouts(t, 5*time.Second, 300*time.Millisecond, 30*time.Second)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if n == 1 {
			// Headers out, then silence on the body. Bounded
			// wait (see TestHungConnectionFailsFast).
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			return
		}
		fmt.Fprint(w, openAIChatOK)
	}))
	defer srv.Close()

	start := time.Now()
	resp, err := mustProvider(t, "openai", srv.URL).Chat(context.Background(), sampleRequest())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp == nil || resp.Message.Content != "four" {
		t.Fatalf("Chat content = %q, want four", respMessageContent(resp))
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("server saw %d requests, want 2 (the stalled body read must be retried)", got)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Chat took %v, want fast", elapsed)
	}
	t.Logf("stalled Chat body retried and succeeded in %v after %d requests", elapsed, requests.Load())
}

// --- (c) slow-but-steady completes --------------------------------------

// TestSlowButSteadyStreamCompletes: a stream that trickles one chunk
// every 400ms — each gap well inside the 1s progress window, but a
// 3.2s total — must COMPLETE. Progress, not wall-clock, is the
// signal: steady bytes never trip the timeout.
func TestSlowButSteadyStreamCompletes(t *testing.T) {
	setTestTimeouts(t, 5*time.Second, 1*time.Second, 30*time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for _, word := range []string{"one", "two", "three", "four", "five", "six", "seven", "eight"} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"%s \"}}]}\n\n", word)
			fl.Flush()
			time.Sleep(400 * time.Millisecond)
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	start := time.Now()
	resp, err := mustProvider(t, "openai", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error {
		return nil
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("slow-but-steady Stream: %v", err)
	}
	want := "one two three four five six seven eight "
	if resp == nil || resp.Message.Content != want {
		t.Fatalf("Stream content = %q, want %q", respMessageContent(resp), want)
	}
	if elapsed < 3*time.Second {
		t.Fatalf("Stream took %v, want ~3.2s (chunks must really have trickled)", elapsed)
	}
	t.Logf("slow-but-steady stream completed in %v", elapsed)
}

// TestSlowButSteadyChatBodyCompletes: the same progress semantics on
// the non-streaming path — a body that trickles in over seconds must
// be read to the end, not aborted.
func TestSlowButSteadyChatBodyCompletes(t *testing.T) {
	setTestTimeouts(t, 5*time.Second, 1*time.Second, 30*time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for _, b := range []byte(openAIChatOK) {
			_, _ = w.Write([]byte{b})
			fl.Flush()
			time.Sleep(50 * time.Millisecond)
		}
	}))
	defer srv.Close()

	start := time.Now()
	resp, err := mustProvider(t, "openai", srv.URL).Chat(context.Background(), sampleRequest())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("slow-but-steady Chat: %v", err)
	}
	if resp == nil || resp.Message.Content != "four" {
		t.Fatalf("Chat content = %q, want four", respMessageContent(resp))
	}
	if elapsed < 5*time.Second {
		t.Fatalf("Chat took %v, want ~%v (body must really have trickled)", elapsed, time.Duration(len(openAIChatOK))*100*time.Millisecond)
	}
	t.Logf("slow-but-steady Chat body completed in %v", elapsed)
}

// --- total deadline ------------------------------------------------------

// TestTotalTimeoutBoundsTricklingCall: a server that keeps sending
// bytes forever — progress resets, so the progress timeout never
// fires — must still be stopped by the total deadline.
func TestTotalTimeoutBoundsTricklingCall(t *testing.T) {
	setTestTimeouts(t, 5*time.Second, 5*time.Second, 1500*time.Millisecond)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		// Trickle forever — or until the client goes away (the
		// write then fails) or the iteration cap is hit, so the
		// handler always returns and srv.Close() cannot hang.
		for i := 0; i < 50; i++ {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			if _, err := w.Write([]byte(" ")); err != nil {
				return
			}
			fl.Flush()
			time.Sleep(100 * time.Millisecond)
		}
	}))
	defer srv.Close()

	start := time.Now()
	_, err := mustProvider(t, "openai", srv.URL).Chat(context.Background(), sampleRequest())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("endlessly trickling Chat: expected an error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(strings.ToLower(err.Error()), "deadline") {
		t.Fatalf("Chat error = %v, want a deadline error", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Chat took %v, want the total deadline (~1.5s), not a hang", elapsed)
	}
	t.Logf("endlessly trickling call stopped by total deadline in %v: %v", elapsed, err)
}

// --- progressReader units ------------------------------------------------

// TestProgressReaderDirect exercises the reader with a short window
func TestProgressReaderDirect(t *testing.T) {
	prc, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("hello"))
		time.Sleep(2 * time.Second)
		_, _ = pw.Write([]byte(" world"))
		_ = pw.Close()
	}()
	pr := &progressReader{rc: prc, timeout: 300 * time.Millisecond, ch: make(chan readResult), done: make(chan struct{})}
	go pr.pump()
	buf := make([]byte, 64)
	n, err := pr.Read(buf)
	if err != nil || string(buf[:n]) != "hello" {
		t.Fatalf("first Read = %q, %v, want hello, nil", buf[:n], err)
	}
	// The second chunk arrives after 2s, far past the 300ms window:
	// the read must abort with a progress timeout, not wait.
	start := time.Now()
	n, err = pr.Read(buf)
	elapsed := time.Since(start)
	if !isProgressTimeout(err) {
		t.Fatalf("second Read = %d, %v, want a progress timeout", n, err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("second Read took %v, want ~300ms", elapsed)
	}
	var pte *progressTimeoutError
	if !errors.As(err, &pte) || !pte.Timeout() {
		t.Fatalf("progress timeout error = %#v, want a net.Error with Timeout()==true", err)
	}
	if err := pr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close is idempotent.
	if err := pr.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestProgressReaderSteadyBytesNeverTimeOut: bytes arriving inside the
// window, however slowly in total, are all delivered.
func TestProgressReaderSteadyBytesNeverTimeOut(t *testing.T) {
	prc, pw := io.Pipe()
	go func() {
		for _, chunk := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
			_, _ = pw.Write([]byte(chunk))
			time.Sleep(100 * time.Millisecond)
		}
		_ = pw.Close()
	}()
	pr := &progressReader{rc: prc, timeout: 300 * time.Millisecond, ch: make(chan readResult), done: make(chan struct{})}
	go pr.pump()
	defer pr.Close()
	var got strings.Builder
	buf := make([]byte, 64)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			got.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}
	if got.String() != "abcdefgh" {
		t.Fatalf("read = %q, want abcdefgh", got.String())
	}
}

// TestSharedClientHonoursProxyEnv is a regression test for the
// timeout rework: the first version of newSharedHTTPClient built a
// custom http.Transport without Proxy, which silently disabled the
// environment egress proxy (Proxy == nil means no proxy). Live
// DeepSeek calls then failed with "tls: first record does not look
// like a TLS handshake" because direct egress is intercepted on
// this host. The transport must keep ProxyFromEnvironment.
//
// NOTE: Go caches the proxy config on first use inside the
// process, so this test reads the process environment instead of
// mutating it with t.Setenv — env mutation after the cache is
// populated has no effect and makes the test order-dependent.
func TestSharedClientHonoursProxyEnv(t *testing.T) {
	tr, ok := newSharedHTTPClient().Transport.(*http.Transport)
	if !ok {
		t.Fatal("shared client transport is not *http.Transport")
	}
	if tr.Proxy == nil {
		t.Fatal("shared client transport has no Proxy: requests would bypass the environment egress proxy")
	}
	want := os.Getenv("HTTPS_PROXY")
	if want == "" {
		want = os.Getenv("https_proxy")
	}
	if want == "" {
		t.Skip("no proxy configured in the environment; non-nil Proxy field already verified above")
	}
	wantURL, err := url.Parse(want)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", want, err)
	}
	u, err := url.Parse("https://api.deepseek.com/v1/chat/completions")
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	got, err := tr.Proxy(&http.Request{URL: u})
	if err != nil {
		t.Fatalf("Proxy(): %v", err)
	}
	if got == nil || got.Host != wantURL.Host {
		t.Fatalf("Proxy() = %v, want proxy host %v (from HTTPS_PROXY)", got, wantURL.Host)
	}
}

// TestIsProgressTimeoutWrapped: the sentinel must survive the %w
// wrapping the helpers apply.
func TestIsProgressTimeoutWrapped(t *testing.T) {
	err := fmt.Errorf("reading response from http://x: %w", &progressTimeoutError{timeout: time.Second})
	if !isProgressTimeout(err) {
		t.Fatalf("isProgressTimeout(%v) = false, want true", err)
	}
	if isProgressTimeout(errors.New("boom")) {
		t.Fatal("isProgressTimeout(boom) = true, want false")
	}
}

// --- helpers -------------------------------------------------------------

func respMessageContent(resp *ChatResponse) string {
	if resp == nil {
		return "<nil response>"
	}
	return resp.Message.Content
}
