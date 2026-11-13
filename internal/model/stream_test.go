package model

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// --- P1-2(d): streaming a tool-call turn yields ToolCalls ----------

// TestOpenAIStreamToolCallDeltas: SSE tool_call deltas arrive split
// across chunks (id+name first, arguments as fragments); Stream must
// merge them into one ToolCall and return it in the response.
func TestOpenAIStreamToolCallDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_9\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"city\\\":\"}}]},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"Paris\\\"}\"}}]},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
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
	if len(chunks) != 0 {
		t.Fatalf("chunks = %q, want none (tool-call turn has no text)", chunks)
	}
	if resp == nil {
		t.Fatal("Stream response is nil")
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v, want 1", resp.Message.ToolCalls)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "call_9" || tc.Name != "get_weather" {
		t.Fatalf("ToolCall = %+v", tc)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil || args["city"] != "Paris" {
		t.Fatalf("Arguments = %q, err %v", tc.Arguments, err)
	}
}

// TestAnthropicStreamToolUse: Anthropic SSE tool_use blocks arrive as
// content_block_start + input_json_delta fragments; Stream must
// assemble them into a ToolCall.
func TestAnthropicStreamToolUse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n")
		fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_9\",\"name\":\"get_weather\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"Paris\\\"}\"}}\n\n")
		fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":12}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "anthropic", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error {
		t.Errorf("unexpected text chunk %q on a tool-call turn", chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if resp == nil {
		t.Fatal("Stream response is nil")
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v, want 1", resp.Message.ToolCalls)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "toolu_9" || tc.Name != "get_weather" {
		t.Fatalf("ToolCall = %+v", tc)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil || args["city"] != "Paris" {
		t.Fatalf("Arguments = %q, err %v", tc.Arguments, err)
	}
}

// TestAnthropicStreamFallbackReturnsToolCalls: an SSE stream that
// yields nothing usable falls back to Chat; the fallback response's
// tool calls must be returned, not dropped.
func TestAnthropicStreamFallbackReturnsToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Paris"}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":6}}`)
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "anthropic", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error {
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if resp == nil || len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Name != "get_weather" {
		t.Fatalf("Stream response = %+v, want fallback tool call", resp)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}
}

// TestOllamaStreamToolCalls: NDJSON tool_calls are parsed from the
// streamed message, IDs are synthesised, and done_reason "stop" must
// not overwrite the tool_calls finish reason (P2-13).
func TestOllamaStreamToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, "{\"message\":{\"role\":\"assistant\",\"content\":\"Checking.\"},\"done\":false}\n")
		fmt.Fprint(w, "{\"message\":{\"role\":\"assistant\",\"content\":\"\",\"tool_calls\":[{\"function\":{\"name\":\"get_weather\",\"arguments\":{\"city\":\"Paris\"}}}]},\"done_reason\":\"stop\",\"done\":true}\n")
	}))
	defer srv.Close()

	var chunks []string
	resp, err := mustProvider(t, "ollama", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if strings.Join(chunks, "") != "Checking." {
		t.Fatalf("chunks = %q, want Checking.", chunks)
	}
	if resp == nil {
		t.Fatal("Stream response is nil")
	}
	if resp.Message.Content != "Checking." {
		t.Fatalf("content = %q, want Checking.", resp.Message.Content)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("FinishReason = %q, want tool_calls (done_reason must not overwrite it)", resp.FinishReason)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v, want 1", resp.Message.ToolCalls)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID == "" || tc.Name != "get_weather" {
		t.Fatalf("ToolCall = %+v, want synthesised ID and get_weather", tc)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil || args["city"] != "Paris" {
		t.Fatalf("Arguments = %q, err %v", tc.Arguments, err)
	}
}

// --- P2-6: Content:null --------------------------------------------------

// TestOpenAIChatNullContentToolCall: real OpenAI-compatible servers
// send content:null on tool-call turns; it must parse as "" and the
// tool call must still be returned.
func TestOpenAIChatNullContentToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "openai", srv.URL).Chat(context.Background(), sampleRequest())
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

// TestOllamaChatNullContent: same null-content shape on Ollama.
func TestOllamaChatNullContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"message":{"role":"assistant","content":null},"done":true}`)
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "ollama", srv.URL).Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Message.Content != "" {
		t.Fatalf("Content = %q, want empty string for JSON null", resp.Message.Content)
	}
}

// --- P2-7: usage fallback only estimates zero fields --------------------

func TestOpenAIUsageFallbackKeepsReportedFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Prompt tokens reported, completion/total zero: only the
		// missing fields may be estimated.
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"hello world, this is a test"},"finish_reason":"stop"}],"usage":{"prompt_tokens":42,"completion_tokens":0,"total_tokens":0}}`)
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "openai", srv.URL).Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Usage.PromptTokens != 42 {
		t.Fatalf("PromptTokens = %d, want 42 (reported value must not be overwritten)", resp.Usage.PromptTokens)
	}
	if resp.Usage.CompletionTokens != 6 {
		t.Fatalf("CompletionTokens = %d, want 6 (estimated from 27-char content)", resp.Usage.CompletionTokens)
	}
	if resp.Usage.TotalTokens != 48 {
		t.Fatalf("TotalTokens = %d, want 48 (42 + 6)", resp.Usage.TotalTokens)
	}
}

func TestOpenAIUsageKeepsReportedTotal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":5,"total_tokens":5}}`)
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "openai", srv.URL).Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Usage.CompletionTokens != 5 || resp.Usage.TotalTokens != 5 {
		t.Fatalf("Usage = %+v, want completion/total kept at 5", resp.Usage)
	}
	if resp.Usage.PromptTokens <= 0 {
		t.Fatalf("PromptTokens = %d, want estimated > 0", resp.Usage.PromptTokens)
	}
}

// --- P2-8: Temperature==0 is sendable ------------------------------------

func TestTemperatureZeroSentExplicitly(t *testing.T) {
	for _, name := range []string{"openai", "anthropic", "ollama"} {
		t.Run(name, func(t *testing.T) {
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				w.Header().Set("Content-Type", "application/json")
				switch name {
				case "openai":
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
				case "anthropic":
					fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
				case "ollama":
					fmt.Fprint(w, `{"message":{"role":"assistant","content":"ok"},"done":true}`)
				}
			}))
			defer srv.Close()

			req := sampleRequest()
			req.Temperature = tempPtr(0)
			if _, err := mustProvider(t, name, srv.URL).Chat(context.Background(), req); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			var temp any
			var present bool
			if name == "ollama" {
				opts, _ := gotBody["options"].(map[string]any)
				temp, present = opts["temperature"]
			} else {
				temp, present = gotBody["temperature"]
			}
			if !present {
				t.Fatalf("temperature missing from %s request body: %v", name, gotBody)
			}
			if temp != float64(0) {
				t.Fatalf("temperature = %v, want 0", temp)
			}

			// Nil temperature must omit the field (provider default).
			gotBody = nil
			req.Temperature = nil
			if _, err := mustProvider(t, name, srv.URL).Chat(context.Background(), req); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if name == "ollama" {
				if opts, _ := gotBody["options"].(map[string]any); opts != nil {
					if _, present := opts["temperature"]; present {
						t.Fatalf("temperature present with nil Temperature: %v", gotBody)
					}
				}
			} else if _, present := gotBody["temperature"]; present {
				t.Fatalf("temperature present with nil Temperature: %v", gotBody)
			}
		})
	}
}

// --- P2-9: stream setup retries on 429/5xx ------------------------------

func TestOpenAIStreamRetriesSetupOn429(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"slow down"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "openai", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error {
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3 (two 429s retried, then success)", calls.Load())
	}
	if resp == nil || resp.Message.Content != "Hello" {
		t.Fatalf("Stream response = %+v, want Hello", resp)
	}
}

func TestOllamaStreamRetriesSetupOn500(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "boom")
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, "{\"message\":{\"role\":\"assistant\",\"content\":\"ok\"},\"done\":true}\n")
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "ollama", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error {
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (one 500 retried)", calls.Load())
	}
	if resp == nil || resp.Message.Content != "ok" {
		t.Fatalf("Stream response = %+v, want ok", resp)
	}
}

// --- P2-10: empty role maps to user (Anthropic) -------------------------

func TestAnthropicEmptyRoleMapsToUser(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()

	req := ChatRequest{Messages: []Message{
		{Role: "user", Content: "hi"},
		{Role: "", Content: "stray empty role"},
	}}
	if _, err := mustProvider(t, "anthropic", srv.URL).Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	msgs, _ := gotBody["messages"].([]any)
	// The empty role maps to user, and the two adjacent user turns
	// merge into one: Anthropic rejects consecutive same-role turns
	// with a 400, so sending two user turns would fail.
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1 merged user turn", len(msgs))
	}
	merged := msgs[0].(map[string]any)
	if merged["role"] != "user" {
		t.Fatalf("empty role mapped to %v, want user", merged["role"])
	}
	if merged["content"] != "hi\nstray empty role" {
		t.Fatalf("merged content = %v, want both texts joined", merged["content"])
	}
}

// --- P2-11: invalid tool-call JSON surfaces an error (Anthropic) --------

func TestAnthropicInvalidToolCallJSONErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()

	req := ChatRequest{Messages: []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "toolu_1", Name: "get_weather", Arguments: `not json`}}},
	}}
	_, err := mustProvider(t, "anthropic", srv.URL).Chat(context.Background(), req)
	if err == nil {
		t.Fatal("Chat with invalid tool-call JSON: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid JSON arguments") {
		t.Fatalf("error = %q, want it to mention invalid JSON arguments", err.Error())
	}
	if calls.Load() != 0 {
		t.Fatalf("server calls = %d, want 0 (conversion must fail before any request)", calls.Load())
	}
}

// --- P2-12: Ollama requests send object-form arguments ------------------

func TestOllamaRequestSendsObjectArguments(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"message":{"role":"assistant","content":"done"},"done":true}`)
	}))
	defer srv.Close()

	req := ChatRequest{Messages: []Message{
		{Role: "user", Content: "weather?"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "call-1", Name: "get_weather", Arguments: `{"city":"Paris"}`}}},
	}}
	if _, err := mustProvider(t, "ollama", srv.URL).Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	assistant, _ := msgs[1].(map[string]any)
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(calls))
	}
	fn, _ := calls[0].(map[string]any)["function"].(map[string]any)
	args, ok := fn["arguments"].(map[string]any)
	if !ok {
		t.Fatalf("arguments = %#v (%T), want a JSON object, not a string", fn["arguments"], fn["arguments"])
	}
	if args["city"] != "Paris" {
		t.Fatalf("arguments = %v, want city Paris", args)
	}
}

func TestOllamaInvalidToolCallJSONErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"message":{"role":"assistant","content":"ok"},"done":true}`)
	}))
	defer srv.Close()

	req := ChatRequest{Messages: []Message{
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "call-1", Name: "a", Arguments: `{broken`}}},
	}}
	_, err := mustProvider(t, "ollama", srv.URL).Chat(context.Background(), req)
	if err == nil {
		t.Fatal("Chat with invalid tool-call JSON: expected error, got nil")
	}
	if calls.Load() != 0 {
		t.Fatalf("server calls = %d, want 0", calls.Load())
	}
}

// --- P2-14: doWithRetry ---------------------------------------------------

func TestRetryDelayHonoursRetryAfter(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Retry-After": []string{"2"}}}
	if got := retryDelay(resp, 0); got != 2*time.Second {
		t.Fatalf("Retry-After 2: delay = %v, want 2s", got)
	}
	// Capped at maxRetryAfter.
	resp.Header.Set("Retry-After", "120")
	if got := retryDelay(resp, 0); got != maxRetryAfter {
		t.Fatalf("Retry-After 120: delay = %v, want capped %v", got, maxRetryAfter)
	}
	// HTTP-date form.
	future := time.Now().Add(5 * time.Second).UTC().Format(http.TimeFormat)
	resp.Header.Set("Retry-After", future)
	if got := retryDelay(resp, 0); got < 4*time.Second || got > 5*time.Second {
		t.Fatalf("Retry-After date: delay = %v, want ~5s", got)
	}
	// Unparseable value falls back to the backoff table.
	resp.Header.Set("Retry-After", "garbage")
	if got := retryDelay(resp, 0); got != backoffDelays[0] {
		t.Fatalf("Retry-After garbage: delay = %v, want backoff %v", got, backoffDelays[0])
	}
	// No header at all: plain backoff.
	if got := retryDelay(&http.Response{Header: http.Header{}}, 1); got != backoffDelays[1] {
		t.Fatalf("no header: delay = %v, want backoff %v", got, backoffDelays[1])
	}
}

type timeoutNetError struct{}

func (timeoutNetError) Error() string   { return "test timeout" }
func (timeoutNetError) Timeout() bool   { return true }
func (timeoutNetError) Temporary() bool { return true }

func TestIsRetryableTransportError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"dns", &url.Error{Op: "Post", URL: "http://x", Err: &net.DNSError{Err: "no such host", IsNotFound: true}}, false},
		{"tls verify", &url.Error{Op: "Post", URL: "https://x", Err: &tls.CertificateVerificationError{Err: errors.New("unknown authority")}}, false},
		{"tls record", &url.Error{Op: "Post", URL: "https://x", Err: tls.RecordHeaderError{}}, false},
		{"timeout", &url.Error{Op: "Post", URL: "http://x", Err: timeoutNetError{}}, true},
		{"conn reset", &url.Error{Op: "Post", URL: "http://x", Err: syscall.ECONNRESET}, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"generic", errors.New("something else"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableTransportError(tc.err); got != tc.want {
				t.Fatalf("isRetryableTransportError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestDoWithRetryReportsTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Just over the 4 MiB cap.
		w.Write(make([]byte, maxResponseBytes+10))
	}))
	defer srv.Close()

	_, err := mustProvider(t, "openai", srv.URL).Chat(context.Background(), sampleRequest())
	if err == nil {
		t.Fatal("Chat with oversized response: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeded") || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("error = %q, want an explicit truncation error", err.Error())
	}
}

func TestDoWithRetryHonoursRetryAfterHeader(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, "slow down")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"4"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "openai", srv.URL).Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Chat after Retry-After retry: %v", err)
	}
	if resp.Message.Content != "4" {
		t.Fatalf("content = %q, want 4", resp.Message.Content)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

// --- P2-15: CountTokens counts runes -------------------------------------

func TestCountTokensNonASCII(t *testing.T) {
	// 8 two-byte runes: bytes/4 would say 4 tokens, runes/4 says 2.
	if got := CountTokens(strings.Repeat("é", 8)); got != 2 {
		t.Fatalf("CountTokens(8 x é) = %d, want 2 (rune-based)", got)
	}
	if got := CountTokens(strings.Repeat("é", 4)); got != 1 {
		t.Fatalf("CountTokens(4 x é) = %d, want 1", got)
	}
	// ASCII behaviour is unchanged.
	if got := CountTokens(strings.Repeat("a", 40)); got != 10 {
		t.Fatalf("CountTokens(40 x a) = %d, want 10", got)
	}
}
