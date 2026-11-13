package model

// Provider stress tests (Orion Audit 3, Workstream 3): fake-server
// (httptest) stress of all three providers — slow responses,
// connection drops mid-stream, malformed streams/responses, context
// overflow, retry behaviour — plus the fixes they forced (empty
// replies and empty-message error objects are now errors, SSE error
// events surface instead of being masked by the Chat fallback).
//
// Timeout vars are tuned down per test (see setTestTimeouts) so each
// behaviour is proved in ~seconds, not production minutes; the
// mechanisms are identical at production scale. No live network is
// used here — live DeepSeek checks live in
// deepseek_stress_live_test.go (//go:build live).
//
// NOTE: these tests mutate package-level timeout vars and rebuild the
// shared client, so they must not run in parallel with other model
// tests (none of this package uses t.Parallel).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- shared fake-server helpers ---------------------------------------

// stressDrop aborts the connection mid-response: the client sees an
// unexpected EOF, not a clean end of body. Flushing first puts the
// response into chunked encoding, so a close mid-body is a drop, not
// a normal EOF.
func stressDrop(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			conn.Close()
		}
	}
}

// requestIsStream reports whether a provider request asked for
// streaming (all three providers send a boolean "stream" field).
func requestIsStream(r *http.Request) bool {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return false
	}
	stream, _ := body["stream"].(bool)
	return stream
}

// stressChatOK is the minimal successful non-streaming payload per
// provider.
func stressChatOK(provider string) string {
	switch provider {
	case "anthropic":
		return `{"content":[{"type":"text","text":"four"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":1}}`
	case "ollama":
		return `{"message":{"role":"assistant","content":"four"},"done":true,"done_reason":"stop","prompt_eval_count":10,"eval_count":1}`
	default: // openai, deepseek
		return openAIChatOK
	}
}

// stressStreamOK is the minimal successful streaming payload per
// provider (one content chunk, then the end marker).
func stressStreamOK(provider string) string {
	switch provider {
	case "anthropic":
		return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"four\"}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	case "ollama":
		return "{\"message\":{\"role\":\"assistant\",\"content\":\"four\"},\"done\":true}\n"
	default: // openai, deepseek
		return "data: {\"choices\":[{\"delta\":{\"content\":\"four\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	}
}

// stressOKServer answers Chat and Stream with the success payloads.
func stressOKServer(provider string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requestIsStream(r) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, stressStreamOK(provider))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, stressChatOK(provider))
	}))
}

// readAllBody reads and returns the request body (for tests that
// need the raw bytes, e.g. to prove a huge history arrived intact).
func readAllBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(r.Body)
}

// stressProviders is the provider set every scenario runs against.
var stressProviders = []string{"openai", "anthropic", "ollama"}

// --- 1. slow responses -------------------------------------------------

// TestStressFirstByteJustUnderProgressTimeout: headers arriving just
// inside the progress window must succeed — the first-byte bound is
// a stall detector, not a speed limit.
func TestStressFirstByteJustUnderProgressTimeout(t *testing.T) {
	setTestTimeouts(t, 5*time.Second, 600*time.Millisecond, 30*time.Second)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		time.Sleep(400 * time.Millisecond) // just under the 600ms window
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, openAIChatOK)
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "openai", srv.URL).Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Chat with first byte at 400ms / 600ms window: %v", err)
	}
	if resp.Message.Content != "four" {
		t.Fatalf("content = %q, want four", resp.Message.Content)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("server saw %d requests, want 1 (no retry for a just-in-time first byte)", got)
	}
}

// TestStressFirstByteJustOverProgressTimeout: headers arriving just
// past the progress window must fail fast with a timeout error — and
// be retried (a header timeout is transient), so the server sees all
// 3 attempts.
func TestStressFirstByteJustOverProgressTimeout(t *testing.T) {
	setTestTimeouts(t, 5*time.Second, 600*time.Millisecond, 30*time.Second)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		time.Sleep(900 * time.Millisecond) // just over the 600ms window
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, openAIChatOK)
	}))
	defer srv.Close()

	start := time.Now()
	_, err := mustProvider(t, "openai", srv.URL).Chat(context.Background(), sampleRequest())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Chat with first byte at 900ms / 600ms window: expected an error, got nil")
	}
	if !isProgressTimeout(err) && !strings.Contains(strings.ToLower(err.Error()), "timeout") {
		t.Fatalf("error = %v, want a timeout error", err)
	}
	if got := requests.Load(); got != maxAttempts {
		t.Fatalf("server saw %d requests, want %d (header timeout must be retried)", got, maxAttempts)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Chat took %v, want ~2s (3 x 600ms + backoff), not a hang", elapsed)
	}
	t.Logf("first byte just over the window failed in %v after %d attempts: %v", elapsed, requests.Load(), err)
}

// TestStressStreamFirstChunkJustUnderProgress: a stream whose first
// chunk arrives just inside the window must complete normally.
func TestStressStreamFirstChunkJustUnderProgress(t *testing.T) {
	setTestTimeouts(t, 5*time.Second, 600*time.Millisecond, 30*time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(400 * time.Millisecond) // just under the 600ms window
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"four\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "openai", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error { return nil })
	if err != nil {
		t.Fatalf("Stream with first chunk at 400ms / 600ms window: %v", err)
	}
	if resp.Message.Content != "four" {
		t.Fatalf("content = %q, want four", resp.Message.Content)
	}
}

// TestStressStreamFirstChunkJustOverProgress: a stream whose first
// chunk arrives just past the window must abort that attempt and
// retry — nothing was delivered yet, so the retry is safe — and the
// second attempt must deliver each chunk exactly once.
func TestStressStreamFirstChunkJustOverProgress(t *testing.T) {
	setTestTimeouts(t, 5*time.Second, 600*time.Millisecond, 30*time.Second)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if n == 1 {
			time.Sleep(900 * time.Millisecond) // just over the 600ms window
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"four\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var chunks []string
	resp, err := mustProvider(t, "openai", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if resp.Message.Content != "four" {
		t.Fatalf("content = %q, want four", resp.Message.Content)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("server saw %d stream requests, want 2 (stalled attempt retried once)", got)
	}
	if len(chunks) != 1 || chunks[0] != "four" {
		t.Fatalf("chunks = %q, want exactly [four] (no duplicates from the retry)", chunks)
	}
}

// TestStressTricklingStreamStoppedByTotal: a stream that keeps
// trickling forever — progress resets, so the progress timeout never
// fires — must still be stopped by the total bound. (Production
// scale: a byte every few seconds for minutes; compressed here to a
// chunk every 200ms against a 1.5s total — same mechanism.)
func TestStressTricklingStreamStoppedByTotal(t *testing.T) {
	setTestTimeouts(t, 5*time.Second, 5*time.Second, 1500*time.Millisecond)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for i := 0; i < 50; i++ {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"tick \"}}]}\n\n")
			fl.Flush()
			time.Sleep(200 * time.Millisecond)
		}
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
		t.Fatal("endlessly trickling Stream: expected an error, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "deadline") && !strings.Contains(strings.ToLower(err.Error()), "context") {
		t.Fatalf("error = %v, want a total-deadline error", err)
	}
	if len(chunks) == 0 {
		t.Fatal("trickling stream delivered no chunks before the total bound — progress reads are not flowing")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Stream took %v, want the total bound (~1.5s), not a hang", elapsed)
	}
	t.Logf("trickling stream stopped by total bound in %v after %d chunks: %v", elapsed, len(chunks), err)
}

// TestStressSlowSteadyLongStreamCompletes: a stream that trickles one
// chunk every 400ms for ~4s — each gap inside the 1s progress window
// but the total far past any single window — must COMPLETE. Progress,
// not wall-clock, is the signal.
func TestStressSlowSteadyLongStreamCompletes(t *testing.T) {
	setTestTimeouts(t, 5*time.Second, 1*time.Second, 30*time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for _, word := range []string{"one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"%s \"}}]}\n\n", word)
			fl.Flush()
			time.Sleep(400 * time.Millisecond)
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	start := time.Now()
	resp, err := mustProvider(t, "openai", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error { return nil })
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("slow-but-steady Stream: %v", err)
	}
	want := "one two three four five six seven eight nine ten "
	if resp.Message.Content != want {
		t.Fatalf("content = %q, want %q", resp.Message.Content, want)
	}
	if elapsed < 4*time.Second {
		t.Fatalf("Stream took %v, want ~4s (chunks must really have trickled)", elapsed)
	}
	t.Logf("slow-but-steady 10-chunk stream completed in %v", elapsed)
}

// --- 2. connection drops mid-stream ------------------------------------

// TestStressChatDropMidBodyRetries: a non-streaming Chat whose body
// is cut mid-way must retry the request (nothing was delivered) and
// succeed on the next attempt.
func TestStressChatDropMidBodyRetries(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			// Partial body, then the connection drops.
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"par`)
			w.(http.Flusher).Flush()
			stressDrop(w)
			return
		}
		fmt.Fprint(w, openAIChatOK)
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "openai", srv.URL).Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Message.Content != "four" {
		t.Fatalf("content = %q, want four", resp.Message.Content)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("server saw %d requests, want 2 (dropped body must be retried)", got)
	}
}

// TestStressChatDropMidBodyEveryAttempt: a body cut on every attempt
// must give up after maxAttempts with a clear error, not hang and
// not return a partial reply.
func TestStressChatDropMidBodyEveryAttempt(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"par`)
		w.(http.Flusher).Flush()
		stressDrop(w)
	}))
	defer srv.Close()

	_, err := mustProvider(t, "openai", srv.URL).Chat(context.Background(), sampleRequest())
	if err == nil {
		t.Fatal("Chat with body dropped on every attempt: expected an error, got nil")
	}
	if got := requests.Load(); got != maxAttempts {
		t.Fatalf("server saw %d requests, want %d (all attempts must be used)", got, maxAttempts)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "eof") && !strings.Contains(strings.ToLower(err.Error()), "unexpected") {
		t.Fatalf("error = %v, want an EOF/truncation error, not a bare failure", err)
	}
	t.Logf("body dropped on every attempt failed after %d attempts: %v", requests.Load(), err)
}

// TestStressConnectionDropBeforeResponse: a server that accepts the
// connection and immediately drops it (EOF before any response) must
// be retried and then fail clearly after maxAttempts.
func TestStressConnectionDropBeforeResponse(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		stressDrop(w)
	}))
	defer srv.Close()

	_, err := mustProvider(t, "openai", srv.URL).Chat(context.Background(), sampleRequest())
	if err == nil {
		t.Fatal("Chat against a server that drops immediately: expected an error, got nil")
	}
	if got := requests.Load(); got != maxAttempts {
		t.Fatalf("server saw %d requests, want %d", got, maxAttempts)
	}
	t.Logf("immediate drop failed after %d attempts: %v", requests.Load(), err)
}

// TestStressStreamDropAfterOneChunk: a stream that delivers one chunk
// and then drops must fail with a clear error, deliver that chunk
// exactly once, and NOT be retried — the chunk is already live with
// the caller, so re-running the stream would duplicate it. All three
// providers.
func TestStressStreamDropAfterOneChunk(t *testing.T) {
	for _, name := range stressProviders {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				fl := w.(http.Flusher)
				switch name {
				case "openai":
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
				case "anthropic":
					fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"first\"}}\n\n")
				case "ollama":
					fmt.Fprint(w, "{\"message\":{\"role\":\"assistant\",\"content\":\"first\"},\"done\":false}\n")
				}
				fl.Flush()
				stressDrop(w)
			}))
			defer srv.Close()

			var chunks []string
			resp, err := mustProvider(t, name, srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error {
				chunks = append(chunks, chunk)
				return nil
			})
			if err == nil {
				t.Fatalf("Stream dropped after one chunk: expected an error, got resp %+v", resp)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "reading stream") {
				t.Fatalf("error = %v, want a clear stream-read error", err)
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("server saw %d stream requests, want 1 (no retry once a chunk is live)", got)
			}
			if len(chunks) != 1 || chunks[0] != "first" {
				t.Fatalf("chunks = %q, want exactly [first]", chunks)
			}
		})
	}
}

// TestStressStreamDropAfterPartialToolCall: a stream that delivers a
// tool-call request and then drops — before the arguments are even
// complete — must fail, and the caller must receive NO tool call at
// all: a partial call must never be executed. All three providers.
func TestStressStreamDropAfterPartialToolCall(t *testing.T) {
	for _, name := range stressProviders {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				fl := w.(http.Flusher)
				switch name {
				case "openai":
					// Tool-call delta with a name and only the
					// first half of its arguments.
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\\\"Par\"}}]}}]}\n\n")
				case "anthropic":
					fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"get_weather\"}}\n\n")
					fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\\\"Par\"}}\n\n")
				case "ollama":
					// Ollama sends tool calls whole per line; a
					// complete call followed by a drop still
					// must not be returned.
					fmt.Fprint(w, "{\"message\":{\"role\":\"assistant\",\"content\":\"\",\"tool_calls\":[{\"function\":{\"name\":\"get_weather\",\"arguments\":{\"city\":\"Paris\"}}}]},\"done\":false}\n")
				}
				fl.Flush()
				stressDrop(w)
			}))
			defer srv.Close()

			resp, err := mustProvider(t, name, srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error { return nil })
			if err == nil {
				t.Fatalf("Stream dropped after a partial tool call: expected an error, got resp %+v", resp)
			}
			if resp != nil {
				t.Fatalf("dropped stream returned a response (%+v): a partial tool call must never reach the caller", resp)
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("server saw %d stream requests, want 1 (no retry, no fallback Chat after a partial call)", got)
			}
			t.Logf("%s: partial tool call dropped cleanly: %v", name, err)
		})
	}
}

// --- 3. malformed streams / responses ----------------------------------

// TestStressChatEmptyBody: a 200 with an empty body must be a clear
// decode error on every provider, never a panic or an empty success.
func TestStressChatEmptyBody(t *testing.T) {
	for _, name := range stressProviders {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			_, err := mustProvider(t, name, srv.URL).Chat(context.Background(), sampleRequest())
			if err == nil {
				t.Fatal("Chat with an empty 200 body: expected an error, got nil")
			}
			if !strings.Contains(strings.ToLower(err.Error()), "decod") {
				t.Fatalf("error = %v, want a decoding error", err)
			}
		})
	}
}

// TestStressChatTruncatedJSON: a response cut mid-object must be a
// clear decode error on every provider.
func TestStressChatTruncatedJSON(t *testing.T) {
	for _, name := range stressProviders {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch name {
				case "anthropic":
					fmt.Fprint(w, `{"content":[{"type":"text","text":"hel`)
				case "ollama":
					fmt.Fprint(w, `{"message":{"role":"assistant","content":"hel`)
				default:
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"hel`)
				}
			}))
			defer srv.Close()

			_, err := mustProvider(t, name, srv.URL).Chat(context.Background(), sampleRequest())
			if err == nil {
				t.Fatal("Chat with truncated JSON: expected an error, got nil")
			}
			if !strings.Contains(strings.ToLower(err.Error()), "decod") {
				t.Fatalf("error = %v, want a decoding error", err)
			}
		})
	}
}

// TestStressChatZeroChoices: a response with no usable reply — zero
// choices (OpenAI), an empty content array (Anthropic), an empty
// message (Ollama) — must be a clear error, never a silent empty
// reply treated as success.
func TestStressChatZeroChoices(t *testing.T) {
	for _, name := range stressProviders {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch name {
				case "anthropic":
					fmt.Fprint(w, `{"content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":0}}`)
				case "ollama":
					fmt.Fprint(w, `{"message":{"role":"assistant","content":""},"done":true}`)
				default:
					fmt.Fprint(w, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`)
				}
			}))
			defer srv.Close()

			resp, err := mustProvider(t, name, srv.URL).Chat(context.Background(), sampleRequest())
			if err == nil {
				t.Fatalf("Chat with no usable reply: expected an error, got resp %+v", resp)
			}
			if !strings.Contains(err.Error(), "no choices") && !strings.Contains(err.Error(), "no content") {
				t.Fatalf("error = %v, want a no-choices/no-content error", err)
			}
		})
	}
}

// TestStressChatEmptyContentNoToolCalls: a choice/message whose
// content is empty and which requests no tool calls is an empty
// reply, not an answer, on every provider.
func TestStressChatEmptyContentNoToolCalls(t *testing.T) {
	for _, name := range stressProviders {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch name {
				case "anthropic":
					fmt.Fprint(w, `{"content":[{"type":"text","text":""}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":0}}`)
				case "ollama":
					fmt.Fprint(w, `{"message":{"role":"assistant","content":""},"done":true,"done_reason":"stop"}`)
				default:
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`)
				}
			}))
			defer srv.Close()

			resp, err := mustProvider(t, name, srv.URL).Chat(context.Background(), sampleRequest())
			if err == nil {
				t.Fatalf("Chat with empty content and no tool calls: expected an error, got resp %+v", resp)
			}
			if !strings.Contains(err.Error(), "no content") {
				t.Fatalf("error = %v, want a no-content error", err)
			}
		})
	}
}

// TestStressChatErrorObjectEmptyMessage: an error object whose message
// is empty (or absent) is still an error — it used to fall through
// and return the accompanying choices/content as success.
func TestStressChatErrorObjectEmptyMessage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // fragment the error must carry
	}{
		{"openai", `{"error":{"message":"","type":"server_error"},"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`, "server_error"},
		{"openai-no-message-field", `{"error":{"type":"server_error","code":"boom"},"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`, "server_error"},
		{"openai-empty-object", `{"error":{},"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`, "unspecified error"},
		{"anthropic", `{"error":{"type":"overloaded_error","message":""},"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`, "overloaded_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := "openai"
			if strings.HasPrefix(tc.name, "anthropic") {
				provider = "anthropic"
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()

			resp, err := mustProvider(t, provider, srv.URL).Chat(context.Background(), sampleRequest())
			if err == nil {
				t.Fatalf("Chat with an empty-message error object: expected an error, got resp %+v", resp)
			}
			if !strings.Contains(err.Error(), "API error") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want an API error mentioning %q", err, tc.want)
			}
		})
	}
}

// TestStressChatInvalidToolArgumentsPassthrough: a tool call whose
// arguments are not valid JSON is returned verbatim — the provider
// does not execute calls, so it cannot silently run one; the guard
// is the core's parseToolArguments, which rejects non-object
// arguments and feeds a clear error back to the model instead of
// executing (internal/core/agent_p2_test.go,
// TestParseToolArgumentsRequiresObject). The provider-level contract
// is: pass the raw arguments through untouched, never panic, never
// silently substitute {}.
func TestStressChatInvalidToolArgumentsPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"not json at all"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "openai", srv.URL).Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1 (invalid arguments must not drop the call silently)", len(resp.Message.ToolCalls))
	}
	if got := resp.Message.ToolCalls[0].Arguments; got != "not json at all" {
		t.Fatalf("Arguments = %q, want the raw string passed through untouched", got)
	}
}

// TestStressStreamInvalidToolArgumentsPassthrough: the same contract
// on the Anthropic stream path — input_json_delta fragments that
// concatenate to invalid JSON are returned raw for the core to
// reject, never executed, never panicking.
func TestStressStreamInvalidToolArgumentsPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"get_weather\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\"}}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "anthropic", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error { return nil })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(resp.Message.ToolCalls))
	}
	if got := resp.Message.ToolCalls[0].Arguments; got != `{"city":` {
		t.Fatalf("Arguments = %q, want the raw concatenated fragments", got)
	}
	if !json.Valid([]byte(resp.Message.ToolCalls[0].Arguments)) {
		t.Logf("arguments are invalid JSON as staged; core parseToolArguments rejects these before execution")
	}
}

// TestStressStreamGarbageLinesMidStream: garbage SSE / NDJSON lines
// in the middle of a flowing stream must be skipped without killing
// the turn or panicking, and the valid chunks around them must still
// be delivered.
func TestStressStreamGarbageLinesMidStream(t *testing.T) {
	for _, name := range stressProviders {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				switch name {
				case "openai":
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
					fmt.Fprint(w, "data: this is not json at all\n\n")
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n")
					fmt.Fprint(w, "garbage line with no data prefix\n\n")
					fmt.Fprint(w, "data: [DONE]\n\n")
				case "anthropic":
					fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hel\"}}\n\n")
					fmt.Fprint(w, "event: content_block_delta\ndata: not json\n\n")
					fmt.Fprint(w, "event: some_unknown_event\ndata: {\"whatever\":true}\n\n")
					fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}\n\n")
					fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				case "ollama":
					fmt.Fprint(w, "{\"message\":{\"role\":\"assistant\",\"content\":\"Hel\"},\"done\":false}\n")
					fmt.Fprint(w, "this is not json\n")
					fmt.Fprint(w, "{\"message\":{\"role\":\"assistant\",\"content\":\"lo\"},\"done\":true}\n")
				}
			}))
			defer srv.Close()

			var chunks []string
			resp, err := mustProvider(t, name, srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error {
				chunks = append(chunks, chunk)
				return nil
			})
			if err != nil {
				t.Fatalf("Stream with garbage lines: %v", err)
			}
			if got := strings.Join(chunks, ""); got != "Hello" {
				t.Fatalf("chunks = %q, want Hello (garbage lines must not lose valid chunks)", chunks)
			}
			if resp.Message.Content != "Hello" {
				t.Fatalf("content = %q, want Hello", resp.Message.Content)
			}
		})
	}
}

// TestStressStreamTruncatedJSONChunk: a truncated JSON chunk after a
// valid one must be skipped (not treated as a fatal error, not a
// panic); the valid content already delivered stands.
func TestStressStreamTruncatedJSONChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"trun\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var chunks []string
	resp, err := mustProvider(t, "openai", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream with a truncated JSON chunk: %v", err)
	}
	if len(chunks) != 1 || chunks[0] != "first" {
		t.Fatalf("chunks = %q, want exactly [first]", chunks)
	}
	if resp.Message.Content != "first" {
		t.Fatalf("content = %q, want first", resp.Message.Content)
	}
}

// TestStressStreamEventWithNoData: an SSE event line with no data
// line (and a bare "data:" line) must be ignored, never misrouted to
// the previous event and never a panic.
func TestStressStreamEventWithNoData(t *testing.T) {
	t.Run("anthropic", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			// message_delta with no data line at all, then a real one.
			fmt.Fprint(w, "event: message_delta\n\n")
			fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
			fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
			fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		}))
		defer srv.Close()

		resp, err := mustProvider(t, "anthropic", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error { return nil })
		if err != nil {
			t.Fatalf("Stream with a data-less event: %v", err)
		}
		if resp.Message.Content != "hi" {
			t.Fatalf("content = %q, want hi", resp.Message.Content)
		}
	})
	t.Run("openai", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data:\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		}))
		defer srv.Close()

		resp, err := mustProvider(t, "openai", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error { return nil })
		if err != nil {
			t.Fatalf("Stream with an empty data line: %v", err)
		}
		if resp.Message.Content != "hi" {
			t.Fatalf("content = %q, want hi", resp.Message.Content)
		}
	})
}

// TestStressStreamErrorEvent: an explicit error event inside a 200
// stream must surface as an error immediately. It used to be skipped
// as an unusable chunk, so the Chat fallback silently masked a
// failure the provider explicitly reported. The fallback must NOT
// run: the server counts non-stream requests and there must be none.
func TestStressStreamErrorEvent(t *testing.T) {
	t.Run("openai", func(t *testing.T) {
		var chatRequests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !requestIsStream(r) {
				chatRequests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, openAIChatOK)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"error\":{\"message\":\"overloaded\",\"type\":\"server_error\"}}\n\n")
		}))
		defer srv.Close()

		resp, err := mustProvider(t, "openai", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error { return nil })
		if err == nil {
			t.Fatalf("Stream with an SSE error event: expected an error, got resp %+v", resp)
		}
		if !strings.Contains(err.Error(), "stream error") || !strings.Contains(err.Error(), "overloaded") {
			t.Fatalf("error = %v, want a stream error mentioning overloaded", err)
		}
		if got := chatRequests.Load(); got != 0 {
			t.Fatalf("fallback Chat ran %d time(s): an explicit stream error must not fall back", got)
		}
	})
	t.Run("anthropic", func(t *testing.T) {
		var chatRequests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !requestIsStream(r) {
				chatRequests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, stressChatOK("anthropic"))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"overloaded\"}}\n\n")
		}))
		defer srv.Close()

		resp, err := mustProvider(t, "anthropic", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error { return nil })
		if err == nil {
			t.Fatalf("Stream with an error event: expected an error, got resp %+v", resp)
		}
		if !strings.Contains(err.Error(), "stream error") || !strings.Contains(err.Error(), "overloaded") {
			t.Fatalf("error = %v, want a stream error mentioning overloaded", err)
		}
		if got := chatRequests.Load(); got != 0 {
			t.Fatalf("fallback Chat ran %d time(s): an explicit stream error must not fall back", got)
		}
	})
	t.Run("ollama", func(t *testing.T) {
		var chatRequests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !requestIsStream(r) {
				chatRequests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, stressChatOK("ollama"))
				return
			}
			w.Header().Set("Content-Type", "application/x-ndjson")
			fmt.Fprint(w, "{\"error\":\"model exploded\"}\n")
		}))
		defer srv.Close()

		resp, err := mustProvider(t, "ollama", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error { return nil })
		if err == nil {
			t.Fatalf("Stream with an error line: expected an error, got resp %+v", resp)
		}
		if !strings.Contains(err.Error(), "model exploded") {
			t.Fatalf("error = %v, want the provider's error message", err)
		}
		if got := chatRequests.Load(); got != 0 {
			t.Fatalf("fallback Chat ran %d time(s): an explicit stream error must not fall back", got)
		}
	})
}

// TestStressStreamEmptyThenFallbackFails: a stream of nothing but
// garbage falls back to Chat — and when the fallback ALSO fails, the
// error must surface, never a silent empty reply.
func TestStressStreamEmptyThenFallbackFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requestIsStream(r) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "this is not sse\n")
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "server exploded")
	}))
	defer srv.Close()

	_, err := mustProvider(t, "openai", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error { return nil })
	if err == nil {
		t.Fatal("garbage stream + failing fallback: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "fallback") {
		t.Fatalf("error = %v, want it to name the stream fallback", err)
	}
}

// TestStressStreamZeroChoiceChunks: SSE chunks with zero choices are
// unusable, not fatal — a stream of only those falls back to Chat
// and succeeds.
func TestStressStreamZeroChoiceChunks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requestIsStream(r) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, openAIChatOK)
	}))
	defer srv.Close()

	var chunks []string
	resp, err := mustProvider(t, "openai", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream with zero-choice chunks: %v", err)
	}
	if resp.Message.Content != "four" || len(chunks) != 1 || chunks[0] != "four" {
		t.Fatalf("resp = %+v chunks = %q, want the Chat fallback content four", resp, chunks)
	}
}

// TestStressStreamCleanEOFWithoutDone: a stream that ends cleanly
// after its chunks — no [DONE], no error, just EOF — is a complete
// turn, not a drop.
func TestStressStreamCleanEOFWithoutDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\n")
		// Handler returns: clean EOF, no [DONE].
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "openai", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error { return nil })
	if err != nil {
		t.Fatalf("Stream ending on clean EOF: %v", err)
	}
	if resp.Message.Content != "Hello" {
		t.Fatalf("content = %q, want Hello", resp.Message.Content)
	}
}

// TestStressOllamaChatNullContentWithToolCall: null content is fine
// when a tool call is present — the null-decoding coverage that used
// to live in TestOllamaChatNullContent (which now asserts the
// empty-reply error for the no-tool-call case).
func TestStressOllamaChatNullContentWithToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"message":{"role":"assistant","content":null,"tool_calls":[{"function":{"name":"get_weather","arguments":{"city":"Paris"}}}]},"done":true}`)
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "ollama", srv.URL).Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Message.Content != "" {
		t.Fatalf("Content = %q, want empty string for JSON null", resp.Message.Content)
	}
	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Name != "get_weather" {
		t.Fatalf("ToolCalls = %+v, want 1 get_weather call", resp.Message.ToolCalls)
	}
}

// --- 4. context overflow -------------------------------------------------

// historyBudgetTokens is the default history budget the agent feeds
// each provider (config.DefaultMaxHistoryTokens = 32768; DeepSeek's
// 64K window leaves headroom for tools, completion and estimate
// error). Trimming to it is the core's job (trimHistoryToBudget,
// internal/core/history_caps_test.go); the provider-side contract
// proved here is that a maximal-but-legal request is transmitted
// intact, and an over-budget one is sent as-is — the provider never
// truncates, so the core's trim is the only guard and must stay in
// place.
const historyBudgetTokens = 32768

// hugeHistory builds n alternating user/assistant turns of about
// charsPerMsg characters each.
func hugeHistory(n, charsPerMsg int) []Message {
	msgs := []Message{{Role: "system", Content: "You are helpful."}}
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, Message{Role: role, Content: strings.Repeat("x", charsPerMsg)})
	}
	return msgs
}

// TestStressMaximalHistoryPassesThrough: a history just under the
// 32768-token budget (~32.5K estimated tokens, ~130KB on the wire)
// must reach the provider intact on every provider — all messages
// present, nothing truncated — and succeed.
func TestStressMaximalHistoryPassesThrough(t *testing.T) {
	for _, name := range stressProviders {
		t.Run(name, func(t *testing.T) {
			msgs := hugeHistory(100, 1300)
			est := CountTokens(joinContents(msgs))
			if est >= historyBudgetTokens || est < 30000 {
				t.Fatalf("test setup: estimated history = %d tokens, want just under %d", est, historyBudgetTokens)
			}
			var gotMsgs int
			var gotSystem string
			var gotBytes int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var raw struct {
					System   string `json:"system"`
					Messages []struct {
						Role string `json:"role"`
					} `json:"messages"`
				}
				body, _ := readAllBody(r)
				gotBytes = len(body)
				_ = json.Unmarshal(body, &raw)
				gotMsgs = len(raw.Messages)
				gotSystem = raw.System
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, stressChatOK(name))
			}))
			defer srv.Close()

			resp, err := mustProvider(t, name, srv.URL).Chat(context.Background(), ChatRequest{Messages: msgs})
			if err != nil {
				t.Fatalf("Chat with ~%d-token history: %v", est, err)
			}
			if resp.Message.Content != "four" {
				t.Fatalf("content = %q, want four", resp.Message.Content)
			}
			wantMsgs := len(msgs)
			if name == "anthropic" {
				// System is extracted to a top-level field.
				wantMsgs = len(msgs) - 1
				if gotSystem == "" {
					t.Fatal("anthropic request lost the system prompt")
				}
			}
			if gotMsgs != wantMsgs {
				t.Fatalf("server received %d messages, want %d (history must arrive intact)", gotMsgs, wantMsgs)
			}
			if gotBytes < 100*1300 {
				t.Fatalf("server received %d request bytes, want > %d (content must not be truncated)", gotBytes, 100*1300)
			}
			t.Logf("%s: ~%d-token history transmitted intact (%d bytes, %d messages)", name, est, gotBytes, gotMsgs)
		})
	}
}

// TestStressOverBudgetHistorySentAsIs: a history OVER the budget
// (~42K estimated tokens) is still sent whole by the provider —
// providers do not trim, and must not silently truncate. The guard
// is the core's trimHistoryToBudget before the call; this test pins
// the layering so a future provider-side truncation cannot hide a
// broken trim.
func TestStressOverBudgetHistorySentAsIs(t *testing.T) {
	msgs := hugeHistory(130, 1300)
	est := CountTokens(joinContents(msgs))
	if est <= historyBudgetTokens {
		t.Fatalf("test setup: estimated history = %d tokens, want over %d", est, historyBudgetTokens)
	}
	var gotMsgs int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		body, _ := readAllBody(r)
		_ = json.Unmarshal(body, &raw)
		gotMsgs = len(raw.Messages)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, openAIChatOK)
	}))
	defer srv.Close()

	_, err := mustProvider(t, "openai", srv.URL).Chat(context.Background(), ChatRequest{Messages: msgs})
	if err != nil {
		t.Fatalf("Chat with over-budget history: %v", err)
	}
	if gotMsgs != len(msgs) {
		t.Fatalf("server received %d messages, want all %d (provider must not trim)", gotMsgs, len(msgs))
	}
	t.Logf("over-budget history (~%d est. tokens) sent whole: %d messages; trimming is the core's job", est, gotMsgs)
}

// TestStressSingleHugeMessage: one 200KB message (a large tool
// result) must be sent intact on every provider — no provider-side
// request cap, no silent truncation.
func TestStressSingleHugeMessage(t *testing.T) {
	for _, name := range stressProviders {
		t.Run(name, func(t *testing.T) {
			const huge = 200 * 1024
			var gotBytes int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := readAllBody(r)
				gotBytes = len(body)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, stressChatOK(name))
			}))
			defer srv.Close()

			req := ChatRequest{Messages: []Message{
				{Role: "system", Content: "You are helpful."},
				{Role: "user", Content: strings.Repeat("r", huge)},
			}}
			_, err := mustProvider(t, name, srv.URL).Chat(context.Background(), req)
			if err != nil {
				t.Fatalf("Chat with a %d-byte message: %v", huge, err)
			}
			if gotBytes < huge {
				t.Fatalf("server received %d request bytes, want >= %d (message must arrive intact)", gotBytes, huge)
			}
		})
	}
}

// --- 5. retry behaviour --------------------------------------------------

// TestStressRetryAfterHonouredLive: a 429 carrying Retry-After: 1
// must actually wait ~1s before the retry on every provider's Chat
// (not just consult the header), then succeed on the second attempt.
func TestStressRetryAfterHonouredLive(t *testing.T) {
	for _, name := range stressProviders {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == 1 {
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(http.StatusTooManyRequests)
					fmt.Fprint(w, "slow down")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, stressChatOK(name))
			}))
			defer srv.Close()

			start := time.Now()
			resp, err := mustProvider(t, name, srv.URL).Chat(context.Background(), sampleRequest())
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("Chat after 429+Retry-After: %v", err)
			}
			if resp.Message.Content != "four" {
				t.Fatalf("content = %q, want four", resp.Message.Content)
			}
			if got := requests.Load(); got != 2 {
				t.Fatalf("server saw %d requests, want 2", got)
			}
			if elapsed < 900*time.Millisecond {
				t.Fatalf("Chat took %v, want >= ~1s (the Retry-After delay must really be waited out)", elapsed)
			}
			t.Logf("%s: 429 + Retry-After 1 honoured, succeeded after %v", name, elapsed)
		})
	}
}

// TestStressRetry429Forever: a 429 on every attempt must give up
// after maxAttempts with a clear 429 error.
func TestStressRetry429Forever(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, "slow down")
	}))
	defer srv.Close()

	_, err := mustProvider(t, "openai", srv.URL).Chat(context.Background(), sampleRequest())
	if err == nil {
		t.Fatal("Chat with 429 forever: expected an error, got nil")
	}
	if got := requests.Load(); got != maxAttempts {
		t.Fatalf("server saw %d requests, want %d", got, maxAttempts)
	}
	if !strings.Contains(err.Error(), "429") {
		t.Fatalf("error = %v, want a clear 429 error", err)
	}
}

// TestStressRetry500ForeverAnthropic: 500 forever must give up after
// maxAttempts with a clear 500 error. (OpenAI and Ollama 500-forever
// are covered by TestOpenAIChatError500ExhaustsRetries and
// TestOllamaChatError500.)
func TestStressRetry500ForeverAnthropic(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "overloaded")
	}))
	defer srv.Close()

	_, err := mustProvider(t, "anthropic", srv.URL).Chat(context.Background(), sampleRequest())
	if err == nil {
		t.Fatal("Chat with 500 forever: expected an error, got nil")
	}
	if got := requests.Load(); got != maxAttempts {
		t.Fatalf("server saw %d requests, want %d", got, maxAttempts)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error = %v, want a clear 500 error", err)
	}
}

// TestStressRetry500ThenSuccessOllama: 500 then success must retry
// once and succeed. (OpenAI and Anthropic 500-then-success are
// covered by TestOpenAIRetryOn500ThenSuccess and
// TestAnthropicChatError401AndRetry.)
func TestStressRetry500ThenSuccessOllama(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":"overloaded"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, stressChatOK("ollama"))
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "ollama", srv.URL).Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Chat after 500: %v", err)
	}
	if resp.Message.Content != "four" {
		t.Fatalf("content = %q, want four", resp.Message.Content)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("server saw %d requests, want 2 (one retry)", got)
	}
}

// TestStressNoRetryOn401Ollama: a 401 must NOT be retried on Ollama
// either (it is an auth/configuration failure, not transient), and
// the error must name the status. (OpenAI and Anthropic 401
// no-retry are covered by TestOpenAIChatError401 and
// TestAnthropicChatError401AndRetry.)
func TestStressNoRetryOn401Ollama(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"unauthorized"}`)
	}))
	defer srv.Close()

	_, err := mustProvider(t, "ollama", srv.URL).Chat(context.Background(), sampleRequest())
	if err == nil {
		t.Fatal("Chat with 401: expected an error, got nil")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("server saw %d requests, want 1 (no retry on auth errors)", got)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %v, want a clear 401 error", err)
	}
}

// TestStressStreamSetup429ThenSuccess: a 429 on stream setup must be
// retried (honouring Retry-After) and then stream normally, on the
// providers not already covered by TestOpenAIStreamRetriesSetupOn429.
func TestStressStreamSetup429ThenSuccess(t *testing.T) {
	for _, name := range []string{"anthropic", "ollama"} {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == 1 {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(http.StatusTooManyRequests)
					fmt.Fprint(w, "slow down")
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, stressStreamOK(name))
			}))
			defer srv.Close()

			resp, err := mustProvider(t, name, srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error { return nil })
			if err != nil {
				t.Fatalf("Stream after 429 on setup: %v", err)
			}
			if resp.Message.Content != "four" {
				t.Fatalf("content = %q, want four", resp.Message.Content)
			}
			if got := requests.Load(); got != 2 {
				t.Fatalf("server saw %d stream requests, want 2 (setup retried once)", got)
			}
		})
	}
}

// TestStressStreamSetup500Forever: a 500 on every stream setup must
// give up after maxAttempts with a clear error — and the outer
// Stream loop must not add its own retries on top of the setup
// retries (exactly maxAttempts setup requests, not maxAttempts^2).
func TestStressStreamSetup500Forever(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "overloaded")
	}))
	defer srv.Close()

	_, err := mustProvider(t, "openai", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error { return nil })
	if err == nil {
		t.Fatal("Stream with 500 on every setup: expected an error, got nil")
	}
	if got := requests.Load(); got != maxAttempts {
		t.Fatalf("server saw %d setup requests, want exactly %d", got, maxAttempts)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error = %v, want a clear 500 error", err)
	}
}
