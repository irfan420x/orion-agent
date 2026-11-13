package model

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- Audit 2: consecutive same-role turns --------------------------------

// TestToAnthropicMessagesMergesReminderAfterToolResult: the agent loop
// appends a plain user message (the deferred-call reminder) right
// after a tool_result user turn. Anthropic requires strictly
// alternating turns, so the two user turns must merge into one —
// otherwise the next Chat fails with a 400.
func TestToAnthropicMessagesMergesReminderAfterToolResult(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "do two things"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "toolu_1", Name: "echo", Arguments: `{}`}}},
		{Role: "tool", ToolCallID: "toolu_1", Content: "echo:hi"},
		{Role: "user", Content: "You still have pending tool calls from your previous message: echo. Call them, with correct arguments based on the tool results so far, before giving your final answer."},
	}
	system, out, err := toAnthropicMessages(msgs)
	if err != nil {
		t.Fatalf("toAnthropicMessages: %v", err)
	}
	if system != "You are helpful." {
		t.Errorf("system = %q", system)
	}
	// Roles must strictly alternate: user, assistant, user.
	if len(out) != 3 {
		t.Fatalf("converted messages = %d, want 3 (user, assistant, user): %+v", len(out), out)
	}
	for i, want := range []string{"user", "assistant", "user"} {
		if out[i].Role != want {
			t.Errorf("out[%d].Role = %q, want %q: %+v", i, out[i].Role, want, out)
		}
	}
	// The merged user turn must carry both the tool result and the
	// reminder text.
	blocks, ok := out[2].Content.([]anthropicContentBlock)
	if !ok {
		t.Fatalf("out[2].Content = %T, want []anthropicContentBlock", out[2].Content)
	}
	var sawResult, sawReminder bool
	for _, b := range blocks {
		if b.Type == "tool_result" && b.ToolUseID == "toolu_1" && b.Content == "echo:hi" {
			sawResult = true
		}
		if b.Type == "text" && strings.Contains(b.Text, "pending tool calls") {
			sawReminder = true
		}
	}
	if !sawResult {
		t.Errorf("merged user turn lost the tool_result block: %+v", blocks)
	}
	if !sawReminder {
		t.Errorf("merged user turn lost the reminder text block: %+v", blocks)
	}
}

// TestToAnthropicMessagesMergesConsecutiveUserText: two adjacent plain
// user turns merge into one, joined with a newline.
func TestToAnthropicMessagesMergesConsecutiveUserText(t *testing.T) {
	_, out, err := toAnthropicMessages([]Message{
		{Role: "user", Content: "first question"},
		{Role: "user", Content: "second question"},
	})
	if err != nil {
		t.Fatalf("toAnthropicMessages: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("converted messages = %d, want 1: %+v", len(out), out)
	}
	if out[0].Role != "user" || out[0].Content != "first question\nsecond question" {
		t.Errorf("merged message = %+v, want one user turn with both texts", out[0])
	}
}

// TestAnthropicChatRejectsSystemOnlyRequest: a system prompt alone is
// not a conversation — Anthropic rejects an empty messages array, so
// Chat must fail client-side instead of sending a 400.
func TestAnthropicChatRejectsSystemOnlyRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("Chat sent a request with no messages; it should have failed client-side")
	}))
	defer srv.Close()

	_, err := mustProvider(t, "anthropic", srv.URL).Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "system", Content: "You are helpful."}},
	})
	if err == nil {
		t.Fatal("Chat(system only): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no messages") {
		t.Errorf("error = %q, want it to mention no messages", err.Error())
	}
}

// TestAnthropicStreamResetsEventOnBlankLine: a blank line ends one SSE
// event. A data-only line after it must not be misrouted to the
// previous event type — here a text_delta-shaped line must be ignored,
// not emitted as a chunk.
func TestAnthropicStreamResetsEventOnBlankLine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n")
		// Data-only line after the blank line: no event, so it
		// belongs to no event and must be ignored.
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"SHOULD-NOT-APPEAR\"}}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	var chunks []string
	resp, err := mustProvider(t, "anthropic", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if resp.Message.Content != "hello" {
		t.Errorf("Stream content = %q, want %q (the eventless line must not be emitted)", resp.Message.Content, "hello")
	}
	if strings.Join(chunks, "") != "hello" {
		t.Errorf("chunks = %q, want only hello", chunks)
	}
}
