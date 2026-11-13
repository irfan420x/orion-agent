package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- Hardening tests (audit P0-4, P0-5, P1-3) ---

// TestAnthropicMergesConsecutiveToolMessages: two tool results in a row
// must become ONE user turn carrying two tool_result blocks. Anthropic
// requires strictly alternating turns; one user turn per result used to
// produce consecutive user turns and an HTTP 400.
func TestAnthropicMergesConsecutiveToolMessages(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`)
	}))
	defer srv.Close()

	req := ChatRequest{Messages: []Message{
		{Role: "user", Content: "do two things"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "toolu_1", Name: "a", Arguments: `{}`},
			{ID: "toolu_2", Name: "b", Arguments: `{}`},
		}},
		{Role: "tool", Content: "result-a", ToolCallID: "toolu_1"},
		{Role: "tool", Content: "result-b", ToolCallID: "toolu_2"},
	}}
	if _, err := mustProvider(t, "anthropic", srv.URL).Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 (user, assistant, merged tool user turn)", len(msgs))
	}
	toolMsg, _ := msgs[2].(map[string]any)
	if toolMsg["role"] != "user" {
		t.Fatalf("merged tool message role = %v, want user", toolMsg["role"])
	}
	content, _ := toolMsg["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("merged tool message blocks = %d, want 2 tool_result blocks", len(content))
	}
	for i, want := range []string{"toolu_1", "toolu_2"} {
		block, _ := content[i].(map[string]any)
		if block["type"] != "tool_result" || block["tool_use_id"] != want {
			t.Fatalf("block %d = %v, want tool_result for %s", i, block, want)
		}
	}
}

// TestAnthropicUnknownRoleMapsToUser: Anthropic only accepts user /
// assistant (plus extracted system); a stray role used to be passed
// through verbatim and produce a 400.
func TestAnthropicUnknownRoleMapsToUser(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()

	req := ChatRequest{Messages: []Message{
		{Role: "user", Content: "hi"},
		{Role: "mystery", Content: "stray role"},
	}}
	if _, err := mustProvider(t, "anthropic", srv.URL).Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	msgs, _ := gotBody["messages"].([]any)
	// The stray role maps to user, and the two adjacent user turns
	// merge into one: Anthropic rejects consecutive same-role turns
	// with a 400, so sending two user turns would fail.
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1 merged user turn", len(msgs))
	}
	merged := msgs[0].(map[string]any)
	if merged["role"] != "user" {
		t.Fatalf("stray role mapped to %v, want user", merged["role"])
	}
	if merged["content"] != "hi\nstray role" {
		t.Fatalf("merged content = %v, want both texts joined", merged["content"])
	}
}

// TestOllamaToolCallsGetIDs: Ollama returns no tool-call IDs; the
// provider must synthesise stable ones (call-1, call-2) or tool-result
// linkage breaks when history is converted for OpenAI/Anthropic.
func TestOllamaToolCallsGetIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"test-model","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"a","arguments":{}}},{"function":{"name":"b","arguments":{}}}]},"done_reason":"stop","done":true}`)
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "ollama", srv.URL).Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(resp.Message.ToolCalls) != 2 {
		t.Fatalf("ToolCalls = %d, want 2", len(resp.Message.ToolCalls))
	}
	for i, tc := range resp.Message.ToolCalls {
		if tc.ID == "" {
			t.Fatalf("ToolCall %d has an empty ID", i)
		}
	}
	if resp.Message.ToolCalls[0].ID == resp.Message.ToolCalls[1].ID {
		t.Fatalf("tool-call IDs must be distinct, got %q twice", resp.Message.ToolCalls[0].ID)
	}
	// DoneReason must not overwrite the tool_calls finish reason.
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}
}

// TestOpenAIStreamEmptyDeltasFallback: a valid SSE stream of role-only
// (empty) deltas used to return nil having delivered nothing, leaving
// the caller with an empty reply. It must fall back to Chat.
func TestOpenAIStreamEmptyDeltasFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "chat/completions") {
			var reqBody map[string]any
			_ = json.NewDecoder(r.Body).Decode(&reqBody)
			if reqBody["stream"] == true {
				w.Header().Set("Content-Type", "text/event-stream")
				// Role-only deltas: valid SSE, no content at all.
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
				fmt.Fprint(w, "data: [DONE]\n\n")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"fallback answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
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
	if strings.Join(got, "") != "fallback answer" {
		t.Fatalf("chunks = %q, want fallback answer", got)
	}
	if resp == nil || resp.Message.Content != "fallback answer" {
		t.Fatalf("Stream response = %+v, want fallback answer", resp)
	}
}

// TestOllamaStreamDoneEmptyContentBreaks: a done:true chunk with empty
// content used to be skipped, so Stream waited for EOF / the context
// deadline. It must return as soon as done arrives, even if the
// server keeps the connection open afterwards. The stream carries
// nothing usable here, so Stream falls back to a Chat call, which the
// server answers with a normal reply.
func TestOllamaStreamDoneEmptyContentBreaks(t *testing.T) {
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] == true {
			w.Header().Set("Content-Type", "application/json")
			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Error("ResponseWriter does not flush")
				return
			}
			fmt.Fprint(w, "{\"message\":{\"role\":\"assistant\",\"content\":\"\"},\"done\":true}\n")
			flusher.Flush()
			// Hold the connection open: if Stream does not break on done,
			// it blocks here until the test gives up.
			<-released
			return
		}
		// Chat fallback: a normal one-shot reply.
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"message":{"role":"assistant","content":"fallback"},"done":true}`)
	}))
	defer srv.Close()
	defer close(released)

	done := make(chan error, 1)
	provider := mustProvider(t, "ollama", srv.URL)
	go func() {
		resp, err := provider.Stream(context.Background(), sampleRequest(), func(chunk string) error {
			return nil
		})
		if err == nil && (resp == nil || resp.Message.Content != "fallback") {
			err = fmt.Errorf("Stream response = %+v, want fallback content", resp)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stream did not return after a done:true chunk with empty content")
	}
}
