package model

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAnthropicChatReportsCacheRead: usage.cache_read_input_tokens
// lands on the ChatResponse as the hit count.
func TestAnthropicChatReportsCacheRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1000,"output_tokens":10,"cache_read_input_tokens":800}}`)
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "anthropic", srv.URL).Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("anthropic Chat: %v", err)
	}
	if !resp.Usage.CacheReported {
		t.Fatal("CacheReported must be true when cache_read_input_tokens arrived")
	}
	if got := resp.Usage.CacheHit(); got != 800 {
		t.Fatalf("CacheHit() = %d, want 800", got)
	}
	if got := resp.Usage.CacheMiss(); got != 200 {
		t.Fatalf("CacheMiss() = %d, want 200", got)
	}
}

// TestAnthropicChatWithoutCacheRead: no cache_read field means the
// split is unreported, never a measured 0.
func TestAnthropicChatWithoutCacheRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":10}}`)
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "anthropic", srv.URL).Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("anthropic Chat: %v", err)
	}
	if resp.Usage.CacheReported {
		t.Fatal("CacheReported must be false when cache_read_input_tokens is absent")
	}
}

// TestAnthropicStreamReportsCacheRead: the message_start usage
// carrying cache_read_input_tokens surfaces the split on the
// streamed response.
func TestAnthropicStreamReportsCacheRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1000,\"cache_read_input_tokens\":800}}}\n\n")
		fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "anthropic", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error { return nil })
	if err != nil {
		t.Fatalf("anthropic Stream: %v", err)
	}
	if resp.Message.Content != "hello" {
		t.Fatalf("content = %q, want hello", resp.Message.Content)
	}
	if !resp.Usage.CacheReported {
		t.Fatal("CacheReported must be true when message_start carried cache_read_input_tokens")
	}
	if got := resp.Usage.CacheHit(); got != 800 {
		t.Fatalf("CacheHit() = %d, want 800", got)
	}
}
