// W1 (rc12) regression tests for Audit 7 F-A7-1 (/compress is
// metered but never refused at the daily limit) and F-A7-P2-4
// (manual /compress has no summary-preservation check).
package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/budget"
	"github.com/irfan/orion/internal/model"
)

// compressFixtureMessages are varied /compress fixture turns.
// They discuss the project plan and build — the same content
// the compressScriptProvider summary preserves — so the
// preservation check (F-A7-P2-4) passes on the happy path. One
// repeated phrase would have only two distinct content tokens
// to share, and no summary could ever pass the >= 3 shared
// token bar.
var compressFixtureMessages = []string{
	"project plan build discussion turn",
	"decided to proceed with the project build",
	"reviewed the project plan and build schedule",
}

// TestW1CompressRefusedAtDailyLimit proves F-A7-1 with the
// audit's probe shape: budget limit 100, usage pre-filled to
// exactly 100, then /compress with a 1500-token summary. On
// rc11 the call succeeded and usage went 100 -> 1600; it must
// now be refused BEFORE the provider call (errors.Is
// ErrBudgetExceeded), with the provider never called and usage
// unchanged.
func TestW1CompressRefusedAtDailyLimit(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(100)
	if err := a.Budget.Record(111, 100); err != nil {
		t.Fatalf("prefill Budget.Record: %v", err)
	}
	prov := &compressScriptProvider{usage: model.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}}
	a.Provider = prov
	seedChatHistoryRC11(t, a, 111, 12)

	before, after, summaryLen, err := a.CompressForChat(context.Background(), 111, 10)
	if !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("CompressForChat at daily limit: err = %v (before=%d after=%d summaryLen=%d), want ErrBudgetExceeded", err, before, after, summaryLen)
	}
	if prov.calls.Load() != 0 {
		t.Errorf("provider calls = %d, want 0 (the refusal must happen before the provider call)", prov.calls.Load())
	}
	if got := a.Budget.Usage(111); got != 100 {
		t.Errorf("Budget.Usage(111) after refused /compress = %d, want 100 (unchanged)", got)
	}
	cs, err := a.chatSessionFor(111)
	if err != nil {
		t.Fatalf("chatSessionFor: %v", err)
	}
	msgs, err := cs.store.RecentMessages(0)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	if len(msgs) != 12 {
		t.Errorf("history after refused /compress = %d messages, want 12 (untouched)", len(msgs))
	}
}

// TestW1CompressRefusedWhenProjectedOverLimit proves the
// projection leg of F-A7-1: a chat UNDER its limit is still
// refused when the summarisation input alone would push it
// over — mirroring the turn path's pre-flight projection
// (F-A5-6), where Check(chatID, 0) was found insufficient for
// the same reason. Limit 100, usage 90, projected input ~148
// tokens: the call must be refused and usage must stay 90.
func TestW1CompressRefusedWhenProjectedOverLimit(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(100)
	if err := a.Budget.Record(111, 90); err != nil {
		t.Fatalf("prefill Budget.Record: %v", err)
	}
	prov := &compressScriptProvider{usage: model.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}}
	a.Provider = prov
	seedChatHistoryRC11(t, a, 111, 12)

	if _, _, _, err := a.CompressForChat(context.Background(), 111, 10); !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("CompressForChat projected over limit: err = %v, want ErrBudgetExceeded", err)
	}
	if prov.calls.Load() != 0 {
		t.Errorf("provider calls = %d, want 0 (the refusal must happen before the provider call)", prov.calls.Load())
	}
	if got := a.Budget.Usage(111); got != 90 {
		t.Errorf("Budget.Usage(111) after refused /compress = %d, want 90 (unchanged)", got)
	}
}

// TestW1GlobalCompressRefusedAtDailyLimit proves F-A7-1 on the
// CLI/global path (compressWith): the CLI has no chat, so its
// /compress is checked against the unattributed bucket
// (chatID 0). At-limit there, the global /compress is refused
// before the provider call and usage is unchanged.
func TestW1GlobalCompressRefusedAtDailyLimit(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(100)
	if err := a.Budget.Record(0, 100); err != nil {
		t.Fatalf("prefill Budget.Record: %v", err)
	}
	prov := &compressScriptProvider{usage: model.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}}
	a.Provider = prov
	for i := 0; i < 12; i++ {
		if err := a.Memory.AddMessage(model.Message{Role: "user", Content: compressFixtureMessages[i%len(compressFixtureMessages)]}); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
	}

	if _, _, _, err := a.Compress(context.Background(), 10); !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("Compress at daily limit: err = %v, want ErrBudgetExceeded", err)
	}
	if prov.calls.Load() != 0 {
		t.Errorf("provider calls = %d, want 0 (the refusal must happen before the provider call)", prov.calls.Load())
	}
	if got := a.Budget.Usage(0); got != 100 {
		t.Errorf("Budget.Usage(0) after refused /compress = %d, want 100 (unchanged)", got)
	}
	msgs, err := a.Memory.RecentMessages(0)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	if len(msgs) != 12 {
		t.Errorf("history after refused /compress = %d messages, want 12 (untouched)", len(msgs))
	}
}

// TestW1CompressDegenerateSummaryRefused proves F-A7-P2-4: a
// degenerate summary on the manual /compress path — one word
// repeated 200 times, which passes the what-not-to-save gate
// but preserves nothing — is refused and history is kept,
// mirroring auto-compaction. The summarisation call itself
// still happened, so its tokens stay metered (the sink runs
// before the check): usage is 1500, not 0.
func TestW1CompressDegenerateSummaryRefused(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(100000)
	prov := &recordingCompressProvider{
		summary: strings.Repeat("project ", 200),
		usage:   model.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500},
	}
	a.Provider = prov
	seedChatHistoryRC11(t, a, 111, 12)

	before, after, summaryLen, err := a.CompressForChat(context.Background(), 111, 10)
	if err == nil {
		t.Fatalf("CompressForChat with degenerate summary: err = nil (before=%d after=%d summaryLen=%d), want a preservation refusal", before, after, summaryLen)
	}
	if !strings.Contains(err.Error(), "preserved nothing") {
		t.Fatalf("CompressForChat err = %v, want the preservation refusal (not the gate or another failure)", err)
	}
	cs, err := a.chatSessionFor(111)
	if err != nil {
		t.Fatalf("chatSessionFor: %v", err)
	}
	msgs, err := cs.store.RecentMessages(0)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	if len(msgs) != 12 {
		t.Errorf("history after refused /compress = %d messages, want 12 (kept)", len(msgs))
	}
	if _, found, err := cs.store.GetFact("conversation-summary"); err != nil {
		t.Fatalf("GetFact: %v", err)
	} else if found {
		t.Errorf("a conversation-summary fact was stored for the refused degenerate summary")
	}
	if got := a.Budget.Usage(111); got != 1500 {
		t.Errorf("Budget.Usage(111) after refused /compress = %d, want 1500 (the summarisation call ran, so its tokens stay metered)", got)
	}
}
