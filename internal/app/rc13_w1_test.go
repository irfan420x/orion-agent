// W1 (rc13) regression tests for Audit 8 F-A8-1 (CLI /compress
// budget gate is dead) and F-A8-P2-3 (/compress overshoot).
package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/irfan/orion/internal/budget"
	"github.com/irfan/orion/internal/model"
)

// seedGlobalHistoryRC13 adds n fixture messages to the global
// (CLI) store, so /compress has a prefix to summarise (keepRecent
// 10 needs >10). The fixture turns discuss the project plan and
// build, the same content the script provider's summary preserves,
// so the preservation check passes on the happy path.
func seedGlobalHistoryRC13(t *testing.T, a *App, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := a.Memory.AddMessage(model.Message{Role: "user", Content: compressFixtureMessages[i%len(compressFixtureMessages)]}); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
	}
}

// globalHistoryLenRC13 returns the global store's message count.
func globalHistoryLenRC13(t *testing.T, a *App) int {
	t.Helper()
	msgs, err := a.Memory.RecentMessages(0)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	return len(msgs)
}

// maxTokensCaptureProvider records the MaxTokens of the
// summarisation ChatRequest it receives, and answers with a fixed
// summary and a fixed reported usage — so the test can prove the
// /compress call actually carries a completion cap instead of
// relying on a real provider honouring an unset field.
type maxTokensCaptureProvider struct {
	maxTokens atomic.Int64
	calls     atomic.Int64
	usage     model.Usage
}

func (p *maxTokensCaptureProvider) Name() string { return "stub" }

func (p *maxTokensCaptureProvider) Chat(_ context.Context, req model.ChatRequest) (*model.ChatResponse, error) {
	p.calls.Add(1)
	p.maxTokens.Store(int64(req.MaxTokens))
	return &model.ChatResponse{
		Message:      model.Message{Role: "assistant", Content: "The user discussed the project plan and decided to proceed with the build."},
		FinishReason: "stop",
		Usage:        p.usage,
	}, nil
}

func (p *maxTokensCaptureProvider) Stream(ctx context.Context, req model.ChatRequest, fn func(chunk string) error) (*model.ChatResponse, error) {
	resp, err := p.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := fn(resp.Message.Content); err != nil {
		return nil, err
	}
	return resp, nil
}

// TestRC13W1CLICompressGateRecordsToBucket0 proves F-A8-1 with
// the Audit 8 W4 probe shape: limit 1000, provider reports 1500
// tokens per compress, three consecutive a.Compress calls
// (history re-seeded before each). On rc12 all 3 were admitted
// (provider calls = 3, calls #2/#3 err = nil) and 4,500 tokens
// were spent against a 1,000-token limit, because no production
// code path ever recorded CLI compress spend to bucket 0 — the
// pre-flight Check at commands.go:674 checked a bucket nothing
// filled. Now the first call records its 1500 tokens to bucket 0
// (over the limit, so the spend is accounted), and calls #2/#3
// are refused pre-flight with ErrBudgetExceeded, the provider
// never called again, and usage stays 1500.
func TestRC13W1CLICompressGateRecordsToBucket0(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(1000)
	prov := &compressScriptProvider{usage: model.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}}
	a.Provider = prov
	seedGlobalHistoryRC13(t, a, 12)

	// Call #1: admitted (nothing spent yet), 1500 tokens recorded.
	before, after, summaryLen, err := a.Compress(context.Background(), 10)
	if err != nil {
		t.Fatalf("Compress #1: %v", err)
	}
	if before != 12 || after != 10 || summaryLen == 0 {
		t.Fatalf("Compress #1: before=%d after=%d summaryLen=%d, want 12/10/>0", before, after, summaryLen)
	}
	if prov.calls.Load() != 1 {
		t.Fatalf("provider calls after Compress #1 = %d, want 1", prov.calls.Load())
	}
	if got := a.Budget.Usage(0); got != 1500 {
		t.Fatalf("Budget.Usage(0) after Compress #1 = %d, want 1500 (the CLI compress spend must land in the checked bucket)", got)
	}
	if du := a.Budget.DayUsage(0); du.Prompt != 1000 || du.Completion != 500 || du.Total != 1500 {
		t.Fatalf("Budget.DayUsage(0) after Compress #1 = %+v, want {Prompt:1000 Completion:500 Total:1500}", du)
	}

	// Call #2: re-seeded history, but the bucket is already over
	// its limit — refused before the provider call.
	seedGlobalHistoryRC13(t, a, 2)
	if got := globalHistoryLenRC13(t, a); got != 12 {
		t.Fatalf("history before Compress #2 = %d messages, want 12 (re-seeded)", got)
	}
	if _, _, _, err := a.Compress(context.Background(), 10); !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("Compress #2: err = %v, want ErrBudgetExceeded", err)
	}
	if prov.calls.Load() != 1 {
		t.Errorf("provider calls after refused Compress #2 = %d, want 1 (the refusal must happen before the provider call)", prov.calls.Load())
	}
	if got := a.Budget.Usage(0); got != 1500 {
		t.Errorf("Budget.Usage(0) after refused Compress #2 = %d, want 1500 (unchanged)", got)
	}
	if got := globalHistoryLenRC13(t, a); got != 12 {
		t.Errorf("history after refused Compress #2 = %d messages, want 12 (untouched)", got)
	}

	// Call #3: same refusal, still no provider call, still 1500.
	seedGlobalHistoryRC13(t, a, 2)
	if _, _, _, err := a.Compress(context.Background(), 10); !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("Compress #3: err = %v, want ErrBudgetExceeded", err)
	}
	if prov.calls.Load() != 1 {
		t.Errorf("provider calls after refused Compress #3 = %d, want 1", prov.calls.Load())
	}
	if got := a.Budget.Usage(0); got != 1500 {
		t.Errorf("Budget.Usage(0) after refused Compress #3 = %d, want 1500 (unchanged)", got)
	}
}

// TestRC13W1CompressOvershootRefusedByCompletionProjection proves
// F-A8-P2-3 with the Audit 8 W4 probe shape: limit 1000, usage
// 800, projected input ~148 tokens — admitted on rc12, because
// the projection counted input only and the summarisation call
// carried no completion cap — then the provider reported 5000
// total tokens and usage ended at 5800, 5.8x the limit, from one
// call. The projection must now count the completion allowance
// (the 800-token summary cap) as well: 800 used + ~148 input +
// 800 completion > 1000, so the call is refused BEFORE the
// provider runs, usage stays 800, and no overshoot can happen.
// The same refusal holds on the per-chat path, which shares the
// projection.
func TestRC13W1CompressOvershootRefusedByCompletionProjection(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(1000)
	prov := &compressScriptProvider{usage: model.Usage{PromptTokens: 100, CompletionTokens: 4900, TotalTokens: 5000}}
	a.Provider = prov

	// CLI path: usage 800/1000, projected input ~148.
	if err := a.Budget.Record(0, 800); err != nil {
		t.Fatalf("prefill Budget.Record(0, 800): %v", err)
	}
	seedGlobalHistoryRC13(t, a, 12)
	if _, _, _, err := a.Compress(context.Background(), 10); !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("Compress with 800/1000 used: err = %v, want ErrBudgetExceeded (input + completion allowance must not fit)", err)
	}
	if prov.calls.Load() != 0 {
		t.Errorf("provider calls after refused /compress = %d, want 0 (the refusal must happen before the provider call)", prov.calls.Load())
	}
	if got := a.Budget.Usage(0); got != 800 {
		t.Errorf("Budget.Usage(0) after refused /compress = %d, want 800 (unchanged — no 5000-token overshoot)", got)
	}
	if got := globalHistoryLenRC13(t, a); got != 12 {
		t.Errorf("history after refused /compress = %d messages, want 12 (untouched)", got)
	}

	// Per-chat path: same shape, same refusal (shared projection).
	if err := a.Budget.Record(111, 800); err != nil {
		t.Fatalf("prefill Budget.Record(111, 800): %v", err)
	}
	seedChatHistoryRC11(t, a, 111, 12)
	if _, _, _, err := a.CompressForChat(context.Background(), 111, 10); !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("CompressForChat with 800/1000 used: err = %v, want ErrBudgetExceeded", err)
	}
	if prov.calls.Load() != 0 {
		t.Errorf("provider calls after refused per-chat /compress = %d, want 0", prov.calls.Load())
	}
	if got := a.Budget.Usage(111); got != 800 {
		t.Errorf("Budget.Usage(111) after refused /compress = %d, want 800 (unchanged)", got)
	}
}

// TestRC13W1CompressSetsMaxTokens proves the F-A8-P2-3
// completion cap itself: the /compress summarisation call must
// carry MaxTokens = compressMaxSummaryTokens (800, the same cap
// auto-compaction uses), so a real provider cannot generate an
// unbounded summary and blow past the daily limit from one
// admitted call. The cap is set in compressWithOn, which both
// the CLI and the per-chat path share, so both are checked.
func TestRC13W1CompressSetsMaxTokens(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(100000)
	prov := &maxTokensCaptureProvider{usage: model.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}}
	a.Provider = prov

	seedGlobalHistoryRC13(t, a, 12)
	if _, _, _, err := a.Compress(context.Background(), 10); err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if prov.calls.Load() != 1 {
		t.Fatalf("provider calls after CLI /compress = %d, want 1", prov.calls.Load())
	}
	if got := prov.maxTokens.Load(); got != int64(compressMaxSummaryTokens) {
		t.Errorf("CLI /compress MaxTokens = %d, want %d (the summarisation call must carry a completion cap)", got, 800)
	}

	seedChatHistoryRC11(t, a, 111, 12)
	if _, _, _, err := a.CompressForChat(context.Background(), 111, 10); err != nil {
		t.Fatalf("CompressForChat: %v", err)
	}
	if prov.calls.Load() != 2 {
		t.Fatalf("provider calls after per-chat /compress = %d, want 2", prov.calls.Load())
	}
	if got := prov.maxTokens.Load(); got != int64(compressMaxSummaryTokens) {
		t.Errorf("per-chat /compress MaxTokens = %d, want %d (the shared summarisation call must carry the same cap)", got, 800)
	}
}
