// Context-window tests: per-provider ContextWindow tables
// (values ONLY from provider-windows-research.md, verified
// against official docs 2026-11-15), the ContextWindow helper,
// and Ollama num_ctx in requests.
package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// nonDeclaringProvider implements Provider but deliberately does
// NOT implement ContextWindowProvider: the helper must report
// ok=false for it.
type nonDeclaringProvider struct{}

func (nonDeclaringProvider) Name() string { return "stub" }
func (nonDeclaringProvider) Chat(context.Context, ChatRequest) (*ChatResponse, error) {
	return &ChatResponse{Message: Message{Role: "assistant", Content: "hi"}, FinishReason: "stop"}, nil
}
func (nonDeclaringProvider) Stream(context.Context, ChatRequest, func(string) error) (*ChatResponse, error) {
	return &ChatResponse{Message: Message{Role: "assistant", Content: "hi"}, FinishReason: "stop"}, nil
}

func TestContextWindowHelper(t *testing.T) {
	if _, ok := ContextWindow(nil); ok {
		t.Fatal("ContextWindow(nil) = ok, want false")
	}
	if _, ok := ContextWindow(nonDeclaringProvider{}); ok {
		t.Fatal("ContextWindow(non-declaring) = ok, want false")
	}
	p, err := NewProvider(ProviderConfig{Name: "deepseek", APIKey: testKey, Model: "deepseek-chat"})
	if err != nil {
		t.Fatalf("NewProvider(deepseek): %v", err)
	}
	w, ok := ContextWindow(p)
	if !ok || w != 131072 {
		t.Fatalf("ContextWindow(deepseek-chat) = (%d, %v), want (131072, true)", w, ok)
	}
}

func TestContextWindowTables(t *testing.T) {
	cases := []struct {
		provider string
		model    string
		want     int
	}{
		// DeepSeek: 1M for the two current models (deepseek-flash,
		// deepseek-v4-pro); the retired deepseek-chat /
		// deepseek-reasoner aliases keep their native 128K window,
		// and an unknown model falls to the smallest verified
		// window (131072), never over-claiming 1M.
		{"deepseek", "deepseek-chat", 131072},
		{"deepseek", "deepseek-reasoner", 131072},
		{"deepseek", "deepseek-flash", 1000000},
		{"deepseek", "deepseek-v4-pro", 1000000},
		{"deepseek", "deepseek-something-new", 131072}, // unknown -> 131072 fallback
		// OpenAI.
		{"openai", "gpt-4o-mini", 128000},
		{"openai", "gpt-4o", 128000},
		{"openai", "gpt-4.1-mini", 1047576},
		{"openai", "gpt-4.1", 1047576},
		{"openai", "gpt-9-future", 128000}, // unknown -> smallest verified
		// Anthropic.
		{"anthropic", "claude-sonnet-4-5", 200000},
		{"anthropic", "claude-opus-4-5", 200000},
		{"anthropic", "claude-haiku-4-5", 200000},
		{"anthropic", "claude-unknown", 200000}, // unknown -> 200K fallback
		// Ollama.
		{"ollama", "llama3.1", 131072},
		{"ollama", "llama3.2", 131072},
		{"ollama", "qwen2.5-0.5b", 32768},
		{"ollama", "qwen2.5-7b", 32768},
		{"ollama", "qwen2.5-14b", 32768},
		{"ollama", "mistral", 32768}, // UNVERIFIED: no table value, falls to fallback
		{"ollama", "some-new-model", 32768},
	}
	for _, tc := range cases {
		t.Run(tc.provider+"/"+tc.model, func(t *testing.T) {
			p, err := NewProvider(ProviderConfig{Name: tc.provider, APIKey: testKey, Model: tc.model})
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}
			cw, ok := p.(ContextWindowProvider)
			if !ok {
				t.Fatalf("%s provider does not implement ContextWindowProvider", tc.provider)
			}
			if got := cw.ContextWindow(); got != tc.want {
				t.Fatalf("ContextWindow(%s %q) = %d, want %d", tc.provider, tc.model, got, tc.want)
			}
		})
	}
}

func TestContextWindowModelNameNormalisation(t *testing.T) {
	cases := []struct {
		provider string
		model    string
		want     int
	}{
		{"deepseek", "  DeepSeek-Chat  ", 131072}, // case + whitespace
		{"openai", "GPT-4O-MINI", 128000},
		{"anthropic", "Claude-Sonnet-4-5", 200000},
		{"ollama", "LLAMA3.1:LATEST", 131072}, // case + :tag suffix
		{"ollama", "llama3.1:latest", 131072},
		{"ollama", "qwen2.5-0.5b:0.5b", 32768},
		{"ollama", "  llama3.2  ", 131072},
	}
	for _, tc := range cases {
		t.Run(tc.provider+"/"+tc.model, func(t *testing.T) {
			p, err := NewProvider(ProviderConfig{Name: tc.provider, APIKey: testKey, Model: tc.model})
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}
			if got := p.(ContextWindowProvider).ContextWindow(); got != tc.want {
				t.Fatalf("ContextWindow(%s %q) = %d, want %d", tc.provider, tc.model, got, tc.want)
			}
		})
	}
}

// TestDeepSeekUsesDeepSeekTableNotOpenAI proves the branch that
// matters: the DeepSeek provider is an openAIProvider with
// name="deepseek", so its window must come from the DeepSeek table
// (1M for the current models), not the OpenAI table (128K for
// gpt-4o-mini-class models). A DeepSeek provider configured with
// an OpenAI model name must still fall to the DeepSeek
// unknown-model fallback (131072), never an OpenAI table value.
func TestDeepSeekUsesDeepSeekTableNotOpenAI(t *testing.T) {
	p, err := NewProvider(ProviderConfig{Name: "deepseek", APIKey: testKey, Model: "gpt-4o-mini"})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if got := p.(ContextWindowProvider).ContextWindow(); got != 131072 {
		t.Fatalf("deepseek provider with model gpt-4o-mini: ContextWindow = %d, want 131072 (DeepSeek fallback, not OpenAI's 128000)", got)
	}
	p2, err := NewProvider(ProviderConfig{Name: "openai", APIKey: testKey, Model: "deepseek-chat"})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if got := p2.(ContextWindowProvider).ContextWindow(); got != 128000 {
		t.Fatalf("openai provider with model deepseek-chat: ContextWindow = %d, want 128000 (OpenAI fallback, not DeepSeek's 131072)", got)
	}
}

// ollamaCapture runs one Ollama Chat against a capturing server
// for the given model and returns the decoded wire request body.
func ollamaCapture(t *testing.T, modelName string, req ChatRequest) map[string]any {
	t.Helper()
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Errorf("decoding request body %q: %v", body, err)
		}
		w.Write([]byte(`{"message":{"role":"assistant","content":"hi"},"done":true,"done_reason":"stop"}`))
	}))
	defer srv.Close()
	p, err := NewProvider(ProviderConfig{Name: "ollama", BaseURL: srv.URL, Model: modelName})
	if err != nil {
		t.Fatalf("NewProvider(ollama %q): %v", modelName, err)
	}
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if captured == nil {
		t.Fatal("server captured no request body")
	}
	return captured
}

func ollamaNumCtx(t *testing.T, body map[string]any) float64 {
	t.Helper()
	opts, ok := body["options"].(map[string]any)
	if !ok {
		t.Fatalf("wire request options = %v (%T), want an object", body["options"], body["options"])
	}
	got, ok := opts["num_ctx"]
	if !ok {
		t.Fatalf("options.num_ctx absent, options = %v", opts)
	}
	f, ok := got.(float64)
	if !ok {
		t.Fatalf("options.num_ctx = %v (%T), want a number", got, got)
	}
	return f
}

func TestOllamaNumCtxEqualsDeclaredWindow(t *testing.T) {
	cases := []struct {
		model string
		want  float64
	}{
		{"llama3.1", 131072},
		{"llama3.2", 131072},
		{"qwen2.5-0.5b", 32768},
		{"qwen2.5-7b:latest", 32768}, // :tag stripped for the lookup
		{"unknown-model-xyz", 32768}, // unknown -> fallback window
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			body := ollamaCapture(t, tc.model, sampleRequest())
			if got := ollamaNumCtx(t, body); got != tc.want {
				t.Fatalf("options.num_ctx for model %q = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

// TestOllamaNumCtxAlwaysPresent: num_ctx is sent even when the
// request sets neither temperature nor MaxTokens — the server
// default must never silently apply.
func TestOllamaNumCtxAlwaysPresent(t *testing.T) {
	req := sampleRequest()
	req.Temperature = nil
	req.MaxTokens = 0
	body := ollamaCapture(t, "llama3.1", req)
	if got := ollamaNumCtx(t, body); got != 131072 {
		t.Fatalf("options.num_ctx = %v, want 131072", got)
	}
	if _, ok := body["options"].(map[string]any)["num_predict"]; ok {
		t.Fatal("options.num_predict present, want absent (MaxTokens=0)")
	}
}

// TestOllamaNumCtxInStream: buildRequest is shared by Chat and
// Stream, so a streamed call must carry num_ctx too.
func TestOllamaNumCtxInStream(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Errorf("decoding request body %q: %v", body, err)
		}
		w.Write([]byte("{\"message\":{\"role\":\"assistant\",\"content\":\"hi\"},\"done\":true,\"done_reason\":\"stop\"}\n"))
	}))
	defer srv.Close()
	p, err := NewProvider(ProviderConfig{Name: "ollama", BaseURL: srv.URL, Model: "llama3.1"})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := p.Stream(context.Background(), sampleRequest(), func(string) error { return nil }); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := ollamaNumCtx(t, captured); got != 131072 {
		t.Fatalf("stream options.num_ctx = %v, want 131072", got)
	}
}
