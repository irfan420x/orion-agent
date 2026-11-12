package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const testKey = "test-key"

// --- NewProvider ---

func TestNewProviderErrors(t *testing.T) {
	if _, err := NewProvider(ProviderConfig{Name: "unknown", APIKey: testKey}); err == nil {
		t.Fatal("expected error for unknown provider, got nil")
	}
	if _, err := NewProvider(ProviderConfig{Name: "openai"}); err == nil {
		t.Fatal("expected error for openai without API key, got nil")
	}
	if _, err := NewProvider(ProviderConfig{Name: "anthropic"}); err == nil {
		t.Fatal("expected error for anthropic without API key, got nil")
	}
	if _, err := NewProvider(ProviderConfig{Name: ""}); err == nil {
		t.Fatal("expected error for empty provider name, got nil")
	}
}

func TestNewProviderDefaults(t *testing.T) {
	p, err := NewProvider(ProviderConfig{Name: "openai", APIKey: testKey})
	if err != nil {
		t.Fatalf("openai NewProvider: %v", err)
	}
	if p.Name() != "openai" {
		t.Fatalf("Name() = %q, want openai", p.Name())
	}
	if got := p.(*openAIProvider).cfg.BaseURL; got != defaultOpenAIBaseURL {
		t.Fatalf("openai BaseURL = %q, want %q", got, defaultOpenAIBaseURL)
	}

	p, err = NewProvider(ProviderConfig{Name: "anthropic", APIKey: testKey})
	if err != nil {
		t.Fatalf("anthropic NewProvider: %v", err)
	}
	if got := p.(*anthropicProvider).cfg.BaseURL; got != defaultAnthropicBaseURL {
		t.Fatalf("anthropic BaseURL = %q, want %q", got, defaultAnthropicBaseURL)
	}

	// Ollama needs no API key and gets a local default.
	p, err = NewProvider(ProviderConfig{Name: "ollama"})
	if err != nil {
		t.Fatalf("ollama NewProvider: %v", err)
	}
	if got := p.(*ollamaProvider).cfg.BaseURL; got != defaultOllamaBaseURL {
		t.Fatalf("ollama BaseURL = %q, want %q", got, defaultOllamaBaseURL)
	}
}

// --- helpers ---

func mustProvider(t *testing.T, name, baseURL string) Provider {
	t.Helper()
	p, err := NewProvider(ProviderConfig{Name: name, APIKey: testKey, BaseURL: baseURL, Model: "test-model"})
	if err != nil {
		t.Fatalf("NewProvider(%s): %v", name, err)
	}
	return p
}

func sampleRequest() ChatRequest {
	return ChatRequest{
		Messages: []Message{
			{Role: "system", Content: "You are helpful."},
			{Role: "user", Content: "What is 2+2?"},
		},
		Tools: []ToolDefinition{{
			Name:        "get_weather",
			Description: "Get the weather",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		}},
		Temperature: tempPtr(0.5),
		MaxTokens:   100,
	}
}

func tempPtr(v float64) *float64 { return &v }

// --- OpenAI ---

func TestOpenAIChatSuccessWithToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testKey {
			t.Errorf("Authorization = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body["model"] != "test-model" {
			t.Errorf("model = %v", body["model"])
		}
		if _, ok := body["tools"]; !ok {
			t.Error("expected tools in request")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "openai", srv.URL).Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(resp.Message.ToolCalls))
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "get_weather" {
		t.Fatalf("ToolCall = %+v", tc)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil || args["city"] != "Paris" {
		t.Fatalf("Arguments = %q, err %v", tc.Arguments, err)
	}
	if resp.Usage.TotalTokens != 15 {
		t.Fatalf("Usage.TotalTokens = %d, want 15", resp.Usage.TotalTokens)
	}
}

func TestOpenAIChatError401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"Incorrect API key","type":"invalid_request_error"}}`)
	}))
	defer srv.Close()

	_, err := mustProvider(t, "openai", srv.URL).Chat(context.Background(), sampleRequest())
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "Incorrect API key") {
		t.Fatalf("error = %q, want status 401 and provider message", err.Error())
	}
}

func TestOpenAIRetryOn500ThenSuccess(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"message":"boom"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"4"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "openai", srv.URL).Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Chat after retry: %v", err)
	}
	if resp.Message.Content != "4" {
		t.Fatalf("content = %q, want 4", resp.Message.Content)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (one retry)", calls.Load())
	}
}

func TestOpenAIChatError500ExhaustsRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "server exploded")
	}))
	defer srv.Close()

	_, err := mustProvider(t, "openai", srv.URL).Chat(context.Background(), sampleRequest())
	if err == nil {
		t.Fatal("expected error after retries exhausted, got nil")
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3 attempts", calls.Load())
	}
}

func TestOpenAIStreamDeliversChunks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var got []string
	resp, err := mustProvider(t, "openai", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error {
		got = append(got, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if strings.Join(got, "") != "Hello" {
		t.Fatalf("chunks = %q, want Hello", got)
	}
	if resp == nil || resp.Message.Content != "Hello" {
		t.Fatalf("Stream response = %+v, want content Hello", resp)
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("FinishReason = %q, want stop", resp.FinishReason)
	}
}

func TestOpenAIStreamFallbackOnBadSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] == true {
			// Garbage SSE: no usable data lines.
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "this is not sse\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"fallback works"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	}))
	defer srv.Close()

	var got []string
	resp, err := mustProvider(t, "openai", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error {
		got = append(got, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream fallback: %v", err)
	}
	if strings.Join(got, "") != "fallback works" {
		t.Fatalf("chunks = %q, want fallback works", got)
	}
	if resp == nil || resp.Message.Content != "fallback works" {
		t.Fatalf("Stream response = %+v, want fallback content", resp)
	}
}

// --- Anthropic ---

func TestAnthropicChatSuccessWithToolUse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages" {
			t.Errorf("path = %q, want /messages", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != testKey {
			t.Errorf("x-api-key = %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("anthropic-version = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body["system"] != "You are helpful." {
			t.Errorf("system = %v, want extracted system prompt", body["system"])
		}
		if body["max_tokens"] != float64(100) {
			t.Errorf("max_tokens = %v, want 100", body["max_tokens"])
		}
		tools, _ := body["tools"].([]any)
		if len(tools) != 1 {
			t.Errorf("tools = %d, want 1", len(tools))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"content":[{"type":"text","text":"Checking weather."},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Paris"}}],"stop_reason":"tool_use","usage":{"input_tokens":12,"output_tokens":8}}`)
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "anthropic", srv.URL).Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}
	if resp.Message.Content != "Checking weather." {
		t.Fatalf("content = %q", resp.Message.Content)
	}
	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Name != "get_weather" {
		t.Fatalf("ToolCalls = %+v", resp.Message.ToolCalls)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(resp.Message.ToolCalls[0].Arguments), &args); err != nil || args["city"] != "Paris" {
		t.Fatalf("Arguments = %q, err %v", resp.Message.ToolCalls[0].Arguments, err)
	}
	if resp.Usage.TotalTokens != 20 {
		t.Fatalf("Usage.TotalTokens = %d, want 20", resp.Usage.TotalTokens)
	}
}

func TestAnthropicChatError401AndRetry(t *testing.T) {
	// 401 must not be retried.
	var calls atomic.Int32
	srv401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	}))
	defer srv401.Close()
	_, err := mustProvider(t, "anthropic", srv401.URL).Chat(context.Background(), sampleRequest())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 error, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("401 calls = %d, want 1 (no retry on auth errors)", calls.Load())
	}

	// 500 then success must retry.
	var calls2 atomic.Int32
	srv500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls2.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "overloaded")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`)
	}))
	defer srv500.Close()
	resp, err := mustProvider(t, "anthropic", srv500.URL).Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Chat after retry: %v", err)
	}
	if resp.Message.Content != "hi" || resp.FinishReason != "stop" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestAnthropicToolResultConversion(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"content":[{"type":"text","text":"Sunny."}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`)
	}))
	defer srv.Close()

	req := ChatRequest{Messages: []Message{
		{Role: "user", Content: "weather?"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "toolu_1", Name: "get_weather", Arguments: `{"city":"Paris"}`}}},
		{Role: "tool", Content: "sunny, 20C", ToolCallID: "toolu_1"},
	}}
	_, err := mustProvider(t, "anthropic", srv.URL).Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
	toolMsg, _ := msgs[2].(map[string]any)
	content, _ := toolMsg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("tool message content blocks = %d, want 1 tool_result", len(content))
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "tool_result" || block["tool_use_id"] != "toolu_1" {
		t.Fatalf("tool_result block = %v", block)
	}
}

func TestAnthropicStreamSingleChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			t.Errorf("stream = %v, want true for Stream", body["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter does not flush")
			return
		}
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n")
		fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"streamed via chat\"}}\n\n")
		fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	var got []string
	resp, err := mustProvider(t, "anthropic", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error {
		got = append(got, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if strings.Join(got, "") != "streamed via chat" {
		t.Fatalf("chunks = %q", got)
	}
	if resp == nil || resp.Message.Content != "streamed via chat" {
		t.Fatalf("Stream response = %+v, want streamed content", resp)
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("FinishReason = %q, want stop", resp.FinishReason)
	}
	if resp.Usage.PromptTokens != 3 || resp.Usage.CompletionTokens != 4 {
		t.Fatalf("Usage = %+v, want prompt 3 / completion 4", resp.Usage)
	}
}

// --- Ollama ---

func TestOllamaChatSuccessWithToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("path = %q, want /api/chat", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body["stream"] != false {
			t.Errorf("stream = %v, want false for Chat", body["stream"])
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"test-model","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"get_weather","arguments":{"city":"Paris"}}}]},"done_reason":"stop","done":true,"prompt_eval_count":9,"eval_count":4}`)
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "ollama", srv.URL).Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Name != "get_weather" {
		t.Fatalf("ToolCalls = %+v", resp.Message.ToolCalls)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(resp.Message.ToolCalls[0].Arguments), &args); err != nil || args["city"] != "Paris" {
		t.Fatalf("Arguments = %q, err %v", resp.Message.ToolCalls[0].Arguments, err)
	}
	if resp.Usage.TotalTokens != 13 {
		t.Fatalf("Usage.TotalTokens = %d, want 13", resp.Usage.TotalTokens)
	}
}

func TestOllamaChatError500(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"model not found"}`)
	}))
	defer srv.Close()

	_, err := mustProvider(t, "ollama", srv.URL).Chat(context.Background(), sampleRequest())
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected 500 error, got %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3 attempts", calls.Load())
	}
}

func TestOllamaStreamDeliversChunks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			t.Errorf("stream = %v, want true for Stream", body["stream"])
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, "{\"model\":\"test-model\",\"message\":{\"role\":\"assistant\",\"content\":\"Hel\"},\"done\":false}\n")
		fmt.Fprint(w, "{\"model\":\"test-model\",\"message\":{\"role\":\"assistant\",\"content\":\"lo\"},\"done\":true}\n")
	}))
	defer srv.Close()

	var got []string
	resp, err := mustProvider(t, "ollama", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error {
		got = append(got, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if strings.Join(got, "") != "Hello" {
		t.Fatalf("chunks = %q, want Hello", got)
	}
	if resp == nil || resp.Message.Content != "Hello" {
		t.Fatalf("Stream response = %+v, want content Hello", resp)
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("FinishReason = %q, want stop", resp.FinishReason)
	}
}

// --- CountTokens ---

func TestCountTokensSanity(t *testing.T) {
	if CountTokens("") != 0 {
		t.Fatalf("CountTokens(\"\") = %d, want 0", CountTokens(""))
	}
	if CountTokens("hi") != 1 {
		t.Fatalf("CountTokens(\"hi\") = %d, want 1 (minimum 1 for non-empty)", CountTokens("hi"))
	}
	if CountTokens(strings.Repeat("a", 40)) != 10 {
		t.Fatalf("CountTokens(40 chars) = %d, want 10", CountTokens(strings.Repeat("a", 40)))
	}
	if got := CountTokens("hello world, this is a test"); got <= 0 {
		t.Fatalf("CountTokens = %d, want > 0", got)
	}
}
