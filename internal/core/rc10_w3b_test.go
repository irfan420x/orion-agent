// W3b (rc10) regression tests for Audit 5 F-A5-5: side-channel
// provider calls — the memory selector, the auto-compaction
// summariser, and session extraction — must be metered into the
// turn's usage instead of being discarded.
package core

import (
	"context"
	"testing"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/tools"
)

// turnFinishedUsage runs one turn and returns the TurnFinished
// event's usage, failing the test when the turn errors or no
// TurnFinished event arrives.
func turnFinishedUsage(t *testing.T, a *Agent, input string) model.Usage {
	t.Helper()
	var got model.Usage
	seen := false
	handler := func(ev ProgressEvent) {
		if ev.Kind == EventTurnFinished {
			got = ev.Usage
			seen = true
		}
	}
	if _, err := a.RunWithEventHandler(context.Background(), input, handler); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !seen {
		t.Fatal("no TurnFinished event observed")
	}
	return got
}

// TestW3BSideChannelSelectorUsageInTurnFinished proves the memory
// selector's provider call (F-A5-5) is metered: a scripted provider
// returning known usage from the selector must have those tokens in
// TurnFinished.Usage, on top of the main loop's own usage.
func TestW3BSideChannelSelectorUsageInTurnFinished(t *testing.T) {
	prov := &fakeProvider{chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		switch req.MaxTokens {
		case maxSelectorTokens:
			// The selector side-query: pick the project memory,
			// reporting 100 prompt + 10 completion tokens.
			return &model.ChatResponse{
				Message:      model.Message{Role: "assistant", Content: `["deploy-notes"]`},
				FinishReason: "stop",
				Usage:        model.Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110},
			}, nil
		default:
			return finalUsageResponseForTest("done", model.Usage{PromptTokens: 20, CompletionTokens: 5, TotalTokens: 25}), nil
		}
	}}
	agent := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), newFakeStore())
	agent.SetTypedMemorySource(&fakeTypedSource{memories: []TypedMemory{
		{Name: "deploy-notes", Description: "How to deploy the web service", Type: "project", Body: "Deploy with the deploy script."},
	}})

	usage := turnFinishedUsage(t, agent, "how do I deploy?")
	if usage.TotalTokens != 135 || usage.PromptTokens != 120 || usage.CompletionTokens != 15 {
		t.Errorf("TurnFinished Usage = %+v, want {Prompt:120 Completion:15 Total:135} (110 selector + 25 main loop)", usage)
	}
}

// TestW3BSideChannelCompactorUsageInTurnFinished proves the
// auto-compaction summariser's provider call (F-A5-5) is metered:
// with the DEFAULT provider-backed summariser (no test override),
// its reported usage must reach TurnFinished.Usage. This is the leg
// the first W3 left unwired — defaultCompactionSummariser used to
// discard resp.Usage.
func TestW3BSideChannelCompactorUsageInTurnFinished(t *testing.T) {
	prov := &fakeProvider{chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		switch req.MaxTokens {
		case 800:
			// The auto-compaction summariser: 500 prompt + 20
			// completion tokens for the summary.
			return &model.ChatResponse{
				Message:      model.Message{Role: "assistant", Content: "The user's favourite number is 17."},
				FinishReason: "stop",
				Usage:        model.Usage{PromptTokens: 500, CompletionTokens: 20, TotalTokens: 520},
			}, nil
		default:
			return finalUsageResponseForTest("done", model.Usage{PromptTokens: 30, CompletionTokens: 5, TotalTokens: 35}), nil
		}
	}}
	cfg := testConfig(5)
	cfg.MaxHistoryTokens = 1024
	store := newFakeStore()
	seedHistory(t, store, 30, 50) // ~30 x 54 tokens: pressure >> 0.8
	agent := newTestAgent(t, cfg, prov, tools.NewRegistry(), store)
	// No SetCompactionSummariser: the default provider-backed
	// summariser runs, which is the path under test.

	usage := turnFinishedUsage(t, agent, "what is my favourite number?")
	if usage.TotalTokens != 555 || usage.PromptTokens != 530 || usage.CompletionTokens != 25 {
		t.Errorf("TurnFinished Usage = %+v, want {Prompt:530 Completion:25 Total:555} (520 compactor + 35 main loop)", usage)
	}
	if _, found, _ := store.GetFact("conversation-summary"); !found {
		t.Error("auto-compaction did not store a conversation-summary fact")
	}
}

// TestW3BSessionExtractorUsageMetered proves the session
// extractor's provider call (F-A5-5) is metered into the agent's
// side-channel total and folded into the NEXT turn's usage: the
// extraction runs in a background goroutine after its turn
// finished, so its tokens are never lost, only attributed one turn
// late.
func TestW3BSessionExtractorUsageMetered(t *testing.T) {
	prov := &fakeProvider{chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		switch req.MaxTokens {
		case 400:
			// The extraction call: 40 prompt + 8 completion.
			return &model.ChatResponse{
				Message:      model.Message{Role: "assistant", Content: "Favourite number is 17."},
				FinishReason: "stop",
				Usage:        model.Usage{PromptTokens: 40, CompletionTokens: 8, TotalTokens: 48},
			}, nil
		default:
			return finalUsageResponseForTest("done", model.Usage{PromptTokens: 20, CompletionTokens: 5, TotalTokens: 25}), nil
		}
	}}
	agent := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), newFakeStore())

	turns := []model.Message{
		{Role: "user", Content: "My favourite number is 17."},
		{Role: "assistant", Content: "Noted."},
	}
	summary, err := agent.defaultSessionExtractor(context.Background(), turns)
	if err != nil {
		t.Fatalf("defaultSessionExtractor: %v", err)
	}
	if summary == "" {
		t.Error("defaultSessionExtractor returned an empty summary")
	}
	if soFar := agent.sideChannelUsageSoFar(); soFar.TotalTokens != 48 || soFar.PromptTokens != 40 || soFar.CompletionTokens != 8 {
		t.Fatalf("side-channel usage after extraction = %+v, want {Prompt:40 Completion:8 Total:48}", soFar)
	}

	// The next turn folds the extraction's tokens into its own
	// TurnFinished usage (48 extraction + 25 main loop).
	usage := turnFinishedUsage(t, agent, "hello")
	if usage.TotalTokens != 73 || usage.PromptTokens != 60 || usage.CompletionTokens != 13 {
		t.Errorf("next TurnFinished Usage = %+v, want {Prompt:60 Completion:13 Total:73} (48 extraction + 25 main loop)", usage)
	}
}

func finalUsageResponseForTest(content string, u model.Usage) *model.ChatResponse {
	return &model.ChatResponse{
		Message:      model.Message{Role: "assistant", Content: content},
		FinishReason: "stop",
		Usage:        u,
	}
}
