// Owner warning-only daily budget tests (layer-0 cost
// reduction, P2): the owner's own chat (first allowlisted chat)
// is never hard-refused by the daily budget — turns and
// /compress run past the limit and the owner is warned by the
// alert instead — while every other chat keeps the hard refusal.
// The owner tests fail on the base commit, which refused the
// owner exactly like any other chat; the non-owner tests pin
// that the exemption does not leak.
package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/budget"
	"github.com/irfan/orion/internal/model"
)

// ownerChatID is the owner's chat in these tests (the same ID
// the rc24 alert tests use); the first allowlisted chat is the
// owner by the codebase's convention.
const ownerChatID = 7429947930

// TestOwnerChatTurnRunsPastBudget: the owner, at 99/100 of a
// 100-token budget, still runs a turn — the pre-flight Check
// and the mid-turn abort do not apply to his chat. The turn
// completes, the provider is called, and the spend is recorded
// (99 + 150 = 249). On the base commit the turn is refused
// pre-flight with ErrBudgetExceeded and the provider is never
// called.
func TestOwnerChatTurnRunsPastBudget(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(100)
	a.Config.TelegramAllowedChats = []int64{ownerChatID}
	prov := &usageScriptProvider{chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		return finalUsageResponse("ok", model.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}), nil
	}}
	a.Provider = prov
	if err := a.Budget.RecordUsage(ownerChatID, model.Usage{TotalTokens: 99}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	reply, stats, err := a.HandleStreamForChat(context.Background(), ownerChatID, "first", nil)
	if err != nil {
		t.Fatalf("owner HandleStreamForChat over budget: err = %v, want nil (owner turns are never refused)", err)
	}
	if reply != "ok" {
		t.Errorf("owner turn reply = %q, want %q", reply, "ok")
	}
	if stats == nil || stats.Tokens != 150 {
		t.Errorf("owner turn stats = %+v, want Tokens 150", stats)
	}
	if got := prov.Calls(); got != 1 {
		t.Errorf("provider calls = %d, want 1 (the owner turn must reach the provider)", got)
	}
	if got := a.Budget.Usage(ownerChatID); got != 249 {
		t.Errorf("budget Usage(owner) after over-budget turn = %d, want 249 (spend is still recorded)", got)
	}
}

// TestNonOwnerChatStillRefusedAtTurnStart: the same numbers for
// a non-owner chat still refuse pre-flight — the exemption is
// the owner only. (Passes on the base commit too; it pins that
// the exemption does not leak to other chats.)
func TestNonOwnerChatStillRefusedAtTurnStart(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(100)
	a.Config.TelegramAllowedChats = []int64{ownerChatID}
	prov := &usageScriptProvider{chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		return finalUsageResponse("ok", model.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}), nil
	}}
	a.Provider = prov
	if err := a.Budget.RecordUsage(111, model.Usage{TotalTokens: 99}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if _, _, err := a.HandleStreamForChat(context.Background(), 111, "first", nil); !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("non-owner HandleStreamForChat over budget err = %v, want ErrBudgetExceeded", err)
	}
	if got := prov.Calls(); got != 0 {
		t.Errorf("provider calls = %d, want 0 (the refused turn must never reach the provider)", got)
	}
}

// TestOwnerChatCompressRunsPastBudget: the owner, already at
// 100/100 of the budget, still runs /compress — the F-A7-1
// pre-flight refusal does not apply to his chat. The
// summarisation runs, the provider is called, and the spend is
// recorded (100 + 1500 = 1600). On the base commit /compress is
// refused with ErrBudgetExceeded and the provider is never
// called.
func TestOwnerChatCompressRunsPastBudget(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(100)
	a.Config.TelegramAllowedChats = []int64{ownerChatID}
	if err := a.Budget.Record(ownerChatID, 100); err != nil {
		t.Fatalf("prefill Budget.Record: %v", err)
	}
	prov := &compressScriptProvider{usage: model.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}}
	a.Provider = prov
	seedChatHistoryRC11(t, a, ownerChatID, 12)

	before, after, summaryLen, err := a.CompressForChat(context.Background(), ownerChatID, 10)
	if err != nil {
		t.Fatalf("owner CompressForChat at daily limit: err = %v (before=%d after=%d summaryLen=%d), want nil", err, before, after, summaryLen)
	}
	if prov.calls.Load() != 1 {
		t.Errorf("provider calls = %d, want 1 (the owner /compress must reach the provider)", prov.calls.Load())
	}
	if got := a.Budget.Usage(ownerChatID); got != 1600 {
		t.Errorf("Budget.Usage(owner) after over-budget /compress = %d, want 1600 (spend is still recorded)", got)
	}
}

// TestOwnerAlertSaysWarningOnly: when the owner's own chat
// crosses the alert threshold, the alert states his turns
// continue — the finite-limit line must not imply a refusal
// that will not happen. On the base commit the alert fires but
// carries no such sentence.
func TestOwnerAlertSaysWarningOnly(t *testing.T) {
	// Limit 1000 (not 100): the projected input grows with the
	// history, so a 100-token limit would refuse the later turns
	// before the threshold is reached; the threshold (80) is
	// what this test exercises.
	a, rec := alertTestApp(t, 1000, 80, 10)
	defer a.Close()
	a.Config.TelegramAllowedChats = []int64{ownerChatID}
	for i := 0; i < 8; i++ {
		runAlertTurn(t, a, ownerChatID, "hi")
	}
	delivered := rec.delivered()
	if len(delivered) != 1 {
		t.Fatalf("delivered alerts = %d, want 1 (owner crossed 80 at turn 8)", len(delivered))
	}
	if delivered[0].chatID != ownerChatID {
		t.Errorf("alert chatID = %d, want %d", delivered[0].chatID, ownerChatID)
	}
	if !strings.Contains(delivered[0].text, "never refuses your turns") {
		t.Errorf("owner alert missing the warning-only sentence:\n%s", delivered[0].text)
	}
}

// TestNonOwnerAlertHasNoWarningOnlyLine: a non-owner chat's
// alert keeps the plain finite-limit wording — the warning-only
// sentence is the owner's only. (Passes on the base commit too.)
func TestNonOwnerAlertHasNoWarningOnlyLine(t *testing.T) {
	// Limit 1000 (not 100): the projected input grows with the
	// history, so a 100-token limit would refuse the later turns
	// before the threshold is reached; the threshold (80) is
	// what this test exercises.
	a, rec := alertTestApp(t, 1000, 80, 10)
	defer a.Close()
	a.Config.TelegramAllowedChats = []int64{ownerChatID}
	for i := 0; i < 8; i++ {
		runAlertTurn(t, a, 111, "hi")
	}
	delivered := rec.delivered()
	if len(delivered) != 1 {
		t.Fatalf("delivered alerts = %d, want 1 (chat 111 crossed 80 at turn 8)", len(delivered))
	}
	if strings.Contains(delivered[0].text, "never refuses your turns") {
		t.Errorf("non-owner alert must not carry the warning-only sentence:\n%s", delivered[0].text)
	}
}

// TestOwnerUsageLineSaysWarningOnly: /usage for the owner's own
// chat with a finite budget states the budget is warning-only,
// so the remaining count is not read as a refusal countdown.
// On the base commit the line carries no such note.
func TestOwnerUsageLineSaysWarningOnly(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(100)
	a.Config.TelegramAllowedChats = []int64{ownerChatID}
	if got := a.UsageLineForChat(ownerChatID); !strings.Contains(got, "warning only") {
		t.Errorf("owner /usage line missing the warning-only note:\n%s", got)
	}
	if got := a.UsageLineForChat(111); strings.Contains(got, "warning only") {
		t.Errorf("non-owner /usage line must not carry the warning-only note:\n%s", got)
	}
}

// TestOwnerChatIDZeroBucketNeverOwner: the CLI/unattributed
// bucket (chatID 0) is never the owner — with no allowlist, or
// with the owner allowlisted — so the CLI /compress gate stays
// enforced and /usage never calls bucket 0 warning-only.
func TestOwnerChatIDZeroBucketNeverOwner(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(100)
	a.Config.TelegramAllowedChats = nil
	if got := a.UsageLineForChat(0); strings.Contains(got, "warning only") {
		t.Errorf("chat 0 /usage line must not carry the warning-only note with no allowlist:\n%s", got)
	}
	a.Config.TelegramAllowedChats = []int64{ownerChatID}
	if got := a.UsageLineForChat(0); strings.Contains(got, "warning only") {
		t.Errorf("chat 0 /usage line must not carry the warning-only note with the owner allowlisted:\n%s", got)
	}
}
