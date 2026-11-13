package model

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// --- Audit 2: Ollama stream robustness ------------------------------------

// TestOllamaStreamDedupesRepeatedToolCalls: Ollama may repeat the full
// tool-call list in the final done chunk. An identical (name,
// arguments) call already recorded is the same call, not a second
// one — without the dedupe the agent defers a phantom duplicate and
// later reminds the model to perform a call it already executed.
func TestOllamaStreamDedupesRepeatedToolCalls(t *testing.T) {
	callLine := `{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"get_weather","arguments":{"city":"Paris"}}}]},"done":false}` + "\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, callLine)
		fmt.Fprint(w, callLine) // repeated verbatim, as a done chunk may
		fmt.Fprint(w, `{"message":{"role":"assistant","content":""},"done":true,"done_reason":"stop"}`+"\n")
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "ollama", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error {
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v, want exactly 1 (the repeated call must be deduped)", resp.Message.ToolCalls)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.Name != "get_weather" || tc.Arguments != `{"city":"Paris"}` {
		t.Fatalf("ToolCall = %+v, want get_weather with the Paris arguments", tc)
	}
}

// TestOllamaStreamSkipsMalformedLine: a single malformed NDJSON line
// must not kill a stream that is otherwise flowing (cf. OpenAI, which
// skips bad SSE chunks).
func TestOllamaStreamSkipsMalformedLine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "this is not json\n")
		fmt.Fprint(w, `{"message":{"role":"assistant","content":"hello"},"done":true}`+"\n")
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "ollama", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error {
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v (a malformed line must be skipped, not fatal)", err)
	}
	if resp.Message.Content != "hello" {
		t.Errorf("Stream content = %q, want %q", resp.Message.Content, "hello")
	}
}
