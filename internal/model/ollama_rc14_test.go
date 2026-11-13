// rc14 regression tests for Audit 9 F-A9-P2-6: the Ollama provider
// silently dropped MaxTokens — the captured wire request carried
// "options":{"temperature":0.5} and no num_predict, while DeepSeek
// and OpenAI both carried "max_tokens":100. buildRequest now maps
// MaxTokens to options.num_predict, merged with temperature into
// a single Options map.
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

// rc14OllamaCapture runs one Ollama Chat against a capturing server
// and returns the decoded wire request body.
func rc14OllamaCapture(t *testing.T, req ChatRequest) map[string]any {
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
		fmt.Fprint(w, `{"message":{"role":"assistant","content":"hi"},"done":true,"done_reason":"stop"}`)
	}))
	defer srv.Close()

	if _, err := mustProvider(t, "ollama", srv.URL).Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if captured == nil {
		t.Fatal("server captured no request body")
	}
	return captured
}

func rc14OllamaOptions(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	opts, ok := body["options"].(map[string]any)
	if !ok {
		t.Fatalf("wire request options = %v (%T), want an object", body["options"], body["options"])
	}
	return opts
}

// TestRC14OllamaMaxTokensSentAsNumPredict is the audit's
// reproduction shape: MaxTokens=100 with Temperature=0.5 (the
// sampleRequest) must produce both keys in one options map.
func TestRC14OllamaMaxTokensSentAsNumPredict(t *testing.T) {
	opts := rc14OllamaOptions(t, rc14OllamaCapture(t, sampleRequest()))
	if got, ok := opts["num_predict"]; !ok || got != float64(100) {
		t.Fatalf("options.num_predict = %v (present=%v), want 100", got, ok)
	}
	if got, ok := opts["temperature"]; !ok || got != 0.5 {
		t.Fatalf("options.temperature = %v (present=%v), want 0.5", got, ok)
	}
}

// TestRC14OllamaMaxTokensWithoutTemperature: with no temperature
// set, num_predict must still be sent on its own.
func TestRC14OllamaMaxTokensWithoutTemperature(t *testing.T) {
	req := sampleRequest()
	req.Temperature = nil
	req.MaxTokens = 200
	opts := rc14OllamaOptions(t, rc14OllamaCapture(t, req))
	if got, ok := opts["num_predict"]; !ok || got != float64(200) {
		t.Fatalf("options.num_predict = %v (present=%v), want 200", got, ok)
	}
	if _, ok := opts["temperature"]; ok {
		t.Fatalf("options.temperature present = %v, want absent (no temperature requested)", opts["temperature"])
	}
}

// TestRC14OllamaNoMaxTokensOmitsNumPredict: an uncapped request
// sends no num_predict, and with no temperature either the
// options object is omitted entirely.
func TestRC14OllamaNoMaxTokensOmitsNumPredict(t *testing.T) {
	req := sampleRequest()
	req.Temperature = nil
	req.MaxTokens = 0
	body := rc14OllamaCapture(t, req)
	if _, ok := body["options"]; ok {
		t.Fatalf("wire request options = %v, want absent (nothing set)", body["options"])
	}

	req2 := sampleRequest()
	req2.MaxTokens = 0
	opts := rc14OllamaOptions(t, rc14OllamaCapture(t, req2))
	if _, ok := opts["num_predict"]; ok {
		t.Fatalf("options.num_predict present = %v, want absent (MaxTokens=0)", opts["num_predict"])
	}
	if got, ok := opts["temperature"]; !ok || got != 0.5 {
		t.Fatalf("options.temperature = %v (present=%v), want 0.5 (temperature must survive without MaxTokens)", got, ok)
	}
}

// TestRC14OllamaStreamSendsNumPredict: buildRequest is shared by
// Chat and Stream, so a streamed call must carry the cap too.
func TestRC14OllamaStreamSendsNumPredict(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Errorf("decoding request body %q: %v", body, err)
		}
		fmt.Fprint(w, `{"message":{"role":"assistant","content":"hi"},"done":true,"done_reason":"stop"}`+"\n")
	}))
	defer srv.Close()

	if _, err := mustProvider(t, "ollama", srv.URL).Stream(context.Background(), sampleRequest(), func(string) error { return nil }); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	opts := rc14OllamaOptions(t, captured)
	if got, ok := opts["num_predict"]; !ok || got != float64(100) {
		t.Fatalf("stream options.num_predict = %v (present=%v), want 100", got, ok)
	}
	if got, ok := opts["temperature"]; !ok || got != 0.5 {
		t.Fatalf("stream options.temperature = %v (present=%v), want 0.5", got, ok)
	}
}
