package model

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// --- Audit 2: stream merging ---------------------------------------------

// TestOpenAIStreamDropsPaddedEmptyToolCalls: mergeStreamToolCalls pads
// the list when a delta index jumps (here the first delta is index 1).
// The padded entry has no name and can never execute — Stream must
// drop it instead of handing the agent a nameless tool call.
func TestOpenAIStreamDropsPaddedEmptyToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call_7\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	resp, err := mustProvider(t, "openai", srv.URL).Stream(context.Background(), sampleRequest(), func(chunk string) error {
		t.Errorf("unexpected text chunk %q on a tool-call turn", chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v, want exactly 1 (the padded empty entry must be dropped)", resp.Message.ToolCalls)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "call_7" || tc.Name != "get_weather" {
		t.Fatalf("ToolCall = %+v, want call_7/get_weather", tc)
	}
}
