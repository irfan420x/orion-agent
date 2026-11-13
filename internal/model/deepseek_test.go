package model

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// DeepSeek shares the OpenAI-compatible client, so these tests pin the
// DeepSeek-specific behaviour: the wire call is the OpenAI-compatible
// /chat/completions shape, and every error is labelled "deepseek:",
// never "openai:" (AUDIT2 DeepSeek P1-1: openai.go hardcoded the
// "openai:" prefix in Chat/Stream errors even for DeepSeek clients).

func TestDeepSeekChatSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions (OpenAI-compatible endpoint)", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testKey {
			t.Errorf("Authorization = %q, want Bearer test key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"396"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "deepseek", srv.URL).Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("deepseek Chat: %v", err)
	}
	if resp.Message.Content != "396" {
		t.Fatalf("content = %q, want 396", resp.Message.Content)
	}
	if resp.Usage.TotalTokens != 12 {
		t.Fatalf("Usage.TotalTokens = %d, want 12", resp.Usage.TotalTokens)
	}
}

func TestDeepSeekErrorsLabelledDeepSeek(t *testing.T) {
	// 401 from the DeepSeek endpoint.
	srv401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"Incorrect API key","type":"invalid_request_error"}}`)
	}))
	defer srv401.Close()
	_, err := mustProvider(t, "deepseek", srv401.URL).Chat(context.Background(), sampleRequest())
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "deepseek:") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %q, want deepseek: label and status 401", err.Error())
	}
	if strings.Contains(err.Error(), "openai") {
		t.Fatalf("error = %q, must not mention openai for a DeepSeek client", err.Error())
	}

	// Undecodable body: the decode error must also carry deepseek:.
	srvBad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `this is not json`)
	}))
	defer srvBad.Close()
	_, err = mustProvider(t, "deepseek", srvBad.URL).Chat(context.Background(), sampleRequest())
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	if !strings.Contains(err.Error(), "deepseek:") || strings.Contains(err.Error(), "openai") {
		t.Fatalf("error = %q, want deepseek: label and no openai mention", err.Error())
	}

	// Stream setup failure: same label rule on the Stream path.
	_, err = mustProvider(t, "deepseek", srv401.URL).Stream(context.Background(), sampleRequest(), func(string) error { return nil })
	if err == nil {
		t.Fatal("expected Stream error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "deepseek:") || strings.Contains(err.Error(), "openai") {
		t.Fatalf("Stream error = %q, want deepseek: label and no openai mention", err.Error())
	}
}

func TestOpenAIErrorLabelUnchanged(t *testing.T) {
	// Control: the label fix must not change the default provider's
	// errors — an OpenAI client still says "openai:".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"Incorrect API key"}}`)
	}))
	defer srv.Close()
	_, err := mustProvider(t, "openai", srv.URL).Chat(context.Background(), sampleRequest())
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "openai:") {
		t.Fatalf("error = %q, want openai: label for an OpenAI client", err.Error())
	}
}

func TestDeepSeekNameCaseInsensitive(t *testing.T) {
	p, err := NewProvider(ProviderConfig{Name: "DeepSeek", APIKey: testKey})
	if err != nil {
		t.Fatalf("NewProvider(DeepSeek): %v", err)
	}
	if p.Name() != "deepseek" {
		t.Fatalf("Name() = %q, want deepseek", p.Name())
	}
}
