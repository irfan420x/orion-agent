package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestOpenAIChatReportsDeepSeekCacheSplit: a non-streaming DeepSeek
// response carrying prompt_cache_hit_tokens /
// prompt_cache_miss_tokens surfaces the split on the ChatResponse,
// and the request carries no stream_options (that field is for
// streams only).
func TestOpenAIChatReportsDeepSeekCacheSplit(t *testing.T) {
	var sawStreamOptions bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			StreamOptions *struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		sawStreamOptions = req.StreamOptions != nil
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"396"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10530,"completion_tokens":3,"total_tokens":10533,"prompt_cache_hit_tokens":10527,"prompt_cache_miss_tokens":3}}`)
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "deepseek", srv.URL).Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("deepseek Chat: %v", err)
	}
	if sawStreamOptions {
		t.Error("non-streaming Chat request must not carry stream_options")
	}
	if !resp.Usage.CacheReported {
		t.Fatal("CacheReported must be true on a DeepSeek cache report")
	}
	if got := resp.Usage.CacheHit(); got != 10527 {
		t.Fatalf("CacheHit() = %d, want 10527", got)
	}
	if got := resp.Usage.CacheMiss(); got != 3 {
		t.Fatalf("CacheMiss() = %d, want 3", got)
	}
}

// TestOpenAIChatReportsOpenAIDetailsCache: the OpenAI details form
// (prompt_tokens_details.cached_tokens, no top-level fields) is
// read as the hit count.
func TestOpenAIChatReportsOpenAIDetailsCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":10,"total_tokens":1010,"prompt_tokens_details":{"cached_tokens":800}}}`)
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "openai", srv.URL).Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("openai Chat: %v", err)
	}
	if !resp.Usage.CacheReported {
		t.Fatal("CacheReported must be true on a details-form cache report")
	}
	if got := resp.Usage.CacheHit(); got != 800 {
		t.Fatalf("CacheHit() = %d, want 800", got)
	}
}

// TestOpenAIStreamSendsIncludeUsageAndReportsSplit: a streaming
// request carries stream_options.include_usage, and the final usage
// chunk's cache split lands on the response — the 396-turn shape,
// where the streamed turn previously reported estimated usage with
// no cache data at all.
func TestOpenAIStreamSendsIncludeUsageAndReportsSplit(t *testing.T) {
	var sawIncludeUsage bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req openAIChatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		sawIncludeUsage = req.StreamOptions != nil && req.StreamOptions.IncludeUsage
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"39\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"6\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10530,\"completion_tokens\":3,\"total_tokens\":10533,\"prompt_cache_hit_tokens\":10527,\"prompt_cache_miss_tokens\":3}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "deepseek", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error { return nil })
	if err != nil {
		t.Fatalf("deepseek Stream: %v", err)
	}
	if !sawIncludeUsage {
		t.Error("streaming request must carry stream_options.include_usage=true")
	}
	if resp.Message.Content != "396" {
		t.Fatalf("content = %q, want 396", resp.Message.Content)
	}
	if !resp.Usage.CacheReported {
		t.Fatal("CacheReported must be true when the final usage chunk carried a cache split")
	}
	if got := resp.Usage.CacheHit(); got != 10527 {
		t.Fatalf("CacheHit() = %d, want 10527", got)
	}
}

// TestOpenAIStreamIgnoresUsageOnFinishChunk: a usage object
// attached to the finish chunk (empty delta + finish_reason) is a
// cumulative snapshot, not the turn's usage — it must be ignored
// like per-content-chunk usage, or the 59,635-token footer bug
// returns through the finish chunk instead of a content chunk.
func TestOpenAIStreamIgnoresUsageOnFinishChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":59635,\"completion_tokens\":3,\"total_tokens\":59638}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "openai", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error { return nil })
	if err != nil {
		t.Fatalf("openai Stream: %v", err)
	}
	if resp.Usage.CacheReported {
		t.Fatal("CacheReported must be false when usage rode on the finish chunk")
	}
	if resp.Usage.PromptTokens == 59635 {
		t.Fatal("PromptTokens must not take the finish chunk's cumulative usage (59635)")
	}
}
func TestOpenAIStreamWithoutUsageChunkFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "openai", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error { return nil })
	if err != nil {
		t.Fatalf("openai Stream: %v", err)
	}
	if resp.Usage.CacheReported {
		t.Fatal("CacheReported must be false when no usage chunk arrived")
	}
	if resp.Usage.PromptTokens == 0 {
		t.Fatal("prompt tokens must still be estimated when no usage chunk arrived")
	}
}
