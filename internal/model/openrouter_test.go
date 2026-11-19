package model

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// OpenRouter shares the OpenAI-compatible client, so these tests pin the
// OpenRouter-specific behaviour: the wire call is the OpenAI-compatible
// /chat/completions shape, and every error is labelled "openrouter:",
// never "openai:" (same rule as the DeepSeek P1-1 audit finding).

func TestOpenRouterChatSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions (OpenAI-compatible endpoint)", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testKey {
			t.Errorf("Authorization = %q, want Bearer <redacted> key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"396"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "openrouter", srv.URL).Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("openrouter Chat: %v", err)
	}
	if resp.Message.Content != "396" {
		t.Fatalf("content = %q, want 396", resp.Message.Content)
	}
	if resp.Usage.TotalTokens != 12 {
		t.Fatalf("Usage.TotalTokens = %d, want 12", resp.Usage.TotalTokens)
	}
}

func TestOpenRouterErrorsLabelledOpenRouter(t *testing.T) {
	// 401 from the OpenRouter endpoint.
	srv401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"No auth credentials found","type":"invalid_request_error"}}`)
	}))
	defer srv401.Close()
	_, err := mustProvider(t, "openrouter", srv401.URL).Chat(context.Background(), sampleRequest())
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "openrouter:") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %q, want openrouter: label and status 401", err.Error())
	}
	if strings.Contains(err.Error(), "openai") {
		t.Fatalf("error = %q, must not mention openai for an OpenRouter client", err.Error())
	}
}

func TestOpenRouterContextWindow(t *testing.T) {
	p, err := NewProvider(ProviderConfig{Name: "openrouter", APIKey: testKey, Model: "deepseek/deepseek-chat"})
	if err != nil {
		t.Fatalf("openrouter NewProvider: %v", err)
	}
	got, ok := ContextWindow(p)
	if !ok {
		t.Fatal("ContextWindow(openrouter) = not ok, want a window")
	}
	if got != 128000 {
		t.Fatalf("ContextWindow() = %d, want 128000 for deepseek/deepseek-chat via OpenRouter", got)
	}
}
