package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/model"
)

// TestAppWiresToolJournalIntoChatAgents (F-A9-1) proves the app
// installs its tool journal on the per-chat agents it builds:
// a Telegram turn (update id carried on the context, as
// PollStream sets it) that executes a tool leaves exactly one
// record in the journal, which a re-delivered turn replays.
func TestAppWiresToolJournalIntoChatAgents(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	j, err := core.NewToolJournal(filepath.Join(a.Config.DataDir, "tool-journal"))
	if err != nil {
		t.Fatalf("NewToolJournal: %v", err)
	}
	a.SetToolJournal(j)
	if a.ToolJournal() != j {
		t.Error("ToolJournal() did not return the installed journal")
	}
	a.Provider = &usageScriptProvider{chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		switch call {
		case 1:
			return &model.ChatResponse{
				Message: model.Message{
					Role:      "assistant",
					ToolCalls: []model.ToolCall{{ID: "call-1", Name: "calculator", Arguments: `{"expression":"17*23+5"}`}},
				},
				FinishReason: "tool_calls",
			}, nil
		default:
			return &model.ChatResponse{
				Message:      model.Message{Role: "assistant", Content: "396"},
				FinishReason: "stop",
			}, nil
		}
	}}
	reply, _, err := a.HandleStreamForChat(core.WithUpdateID(context.Background(), 100), 111, "calc", nil)
	if err != nil {
		t.Fatalf("HandleStreamForChat: %v", err)
	}
	if reply != "396" {
		t.Errorf("reply = %q, want \"396\"", reply)
	}
	recs := j.Records(100)
	if len(recs) != 1 {
		t.Fatalf("journal holds %d records for update 100, want 1", len(recs))
	}
	if recs[0].ToolName != "calculator" {
		t.Errorf("recorded tool = %q, want \"calculator\"", recs[0].ToolName)
	}
	if recs[0].Result == "" {
		t.Error("recorded result is empty, want the calculator output")
	}
}

// TestAppWithoutJournalRunsTurnsNormally proves a hand-built App
// with no journal (nil) runs turns exactly as before: the
// journal is best-effort wiring, never a requirement.
func TestAppWithoutJournalRunsTurnsNormally(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	if a.ToolJournal() != nil {
		t.Error("hand-built App has a journal, want nil")
	}
	a.Provider = &usageScriptProvider{chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		return &model.ChatResponse{
			Message:      model.Message{Role: "assistant", Content: "hi"},
			FinishReason: "stop",
		}, nil
	}}
	reply, _, err := a.HandleStreamForChat(context.Background(), 111, "hi", nil)
	if err != nil {
		t.Fatalf("HandleStreamForChat: %v", err)
	}
	if reply != "hi" {
		t.Errorf("reply = %q, want \"hi\"", reply)
	}
}
