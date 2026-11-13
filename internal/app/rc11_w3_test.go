// W3 (rc11) regression tests for Audit 6 findings F-A6-1 (/compress
// metering), F-A6-P2-6 (mid-turn check blind to subagent spend),
// F-A6-P2-7 (timed-out and late-finishing subagent tasks never
// charged), and the W1 follow-up on the /compress path (total input
// cap + redaction, F-A6-P2-2/P2-3 shape).
package app

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/irfan/orion/internal/budget"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/subagent"
	"github.com/irfan/orion/internal/tools"
)

// compressScriptProvider answers every Chat call with the same
// summary text and the same reported usage, so /compress metering
// can be measured exactly: the summarisation call is the only
// provider call on the compress path.
type compressScriptProvider struct {
	usage model.Usage
	calls int
}

func (p *compressScriptProvider) Name() string { return "stub" }

func (p *compressScriptProvider) Chat(_ context.Context, _ model.ChatRequest) (*model.ChatResponse, error) {
	p.calls++
	return &model.ChatResponse{
		Message:      model.Message{Role: "assistant", Content: "The user discussed the project plan and decided to proceed with the build."},
		FinishReason: "stop",
		Usage:        p.usage,
	}, nil
}

func (p *compressScriptProvider) Stream(ctx context.Context, req model.ChatRequest, fn func(chunk string) error) (*model.ChatResponse, error) {
	resp, err := p.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := fn(resp.Message.Content); err != nil {
		return nil, err
	}
	return resp, nil
}

// recordingCompressProvider captures the summarisation prompt
// /compress sends, and answers with a fixed summary — the
// summary here deliberately echoes the secret from the prefix,
// so the test proves the stored summary is redacted too.
type recordingCompressProvider struct {
	prompt  string
	summary string
	usage   model.Usage
}

func (p *recordingCompressProvider) Name() string { return "stub" }

func (p *recordingCompressProvider) Chat(_ context.Context, req model.ChatRequest) (*model.ChatResponse, error) {
	for _, m := range req.Messages {
		if m.Role == "user" {
			p.prompt = m.Content
		}
	}
	return &model.ChatResponse{
		Message:      model.Message{Role: "assistant", Content: p.summary},
		FinishReason: "stop",
		Usage:        p.usage,
	}, nil
}

func (p *recordingCompressProvider) Stream(ctx context.Context, req model.ChatRequest, fn func(chunk string) error) (*model.ChatResponse, error) {
	resp, err := p.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := fn(resp.Message.Content); err != nil {
		return nil, err
	}
	return resp, nil
}

// seedChatHistoryRC11 adds n user messages to one chat's store, so
// /compress has a prefix to summarise (keepRecent 10 needs >10).
func seedChatHistoryRC11(t *testing.T, a *App, chatID int64, n int) {
	t.Helper()
	cs, err := a.chatSessionFor(chatID)
	if err != nil {
		t.Fatalf("chatSessionFor %d: %v", chatID, err)
	}
	for i := 0; i < n; i++ {
		if err := cs.store.AddMessage(model.Message{Role: "user", Content: "turn message"}); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
	}
}

// TestW3HandleCompressMetering proves F-A6-1: a REAL /compress
// (CompressForChat, the Telegram dispatch path — not compressWithOn
// with a hand-passed sink) records its summarisation tokens to the
// chat's Budget. The audit's repro ran handleCompress with a
// 1500-token summarisation and read Budget.Usage 0 afterwards;
// the same shape must now read 1500.
func TestW3HandleCompressMetering(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(100000)
	prov := &compressScriptProvider{usage: model.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}}
	a.Provider = prov
	seedChatHistoryRC11(t, a, 111, 12)

	before, after, summaryLen, err := a.CompressForChat(context.Background(), 111, 10)
	if err != nil {
		t.Fatalf("CompressForChat: %v", err)
	}
	if before != 12 || after != 10 || summaryLen == 0 {
		t.Fatalf("CompressForChat: before=%d after=%d summaryLen=%d, want 12/10/>0", before, after, summaryLen)
	}
	if got := a.Budget.Usage(111); got != 1500 {
		t.Errorf("Budget.Usage(111) after /compress = %d, want 1500 (the summarisation tokens must be metered)", got)
	}
	if du := a.Budget.DayUsage(111); du.Prompt != 1000 || du.Completion != 500 || du.Subagent != 0 || du.Total != 1500 {
		t.Errorf("Budget.DayUsage(111) = %+v, want {Prompt:1000 Completion:500 Subagent:0 Total:1500}", du)
	}
	// The session-memory variant meters the same way, on its own
	// chat, and never leaks into another chat's budget.
	seedChatHistoryRC11(t, a, 222, 12)
	if _, _, _, err := a.CompressWithSessionMemoryForChat(context.Background(), 222, 10, "## 2026-11-13\nDecision: use DeepSeek as the default provider."); err != nil {
		t.Fatalf("CompressWithSessionMemoryForChat: %v", err)
	}
	if got := a.Budget.Usage(222); got != 1500 {
		t.Errorf("Budget.Usage(222) after /compress with session memory = %d, want 1500", got)
	}
	if got := a.Budget.Usage(111); got != 1500 {
		t.Errorf("Budget.Usage(111) after chat 222's /compress = %d, want 1500 (no cross-chat leak)", got)
	}
}

// TestW3GlobalCompressReportsSideChannelUsage proves the F-A6-1
// audit of the other compressWithOn caller (commands.go): the
// CLI/global /compress reports its summarisation tokens to the
// global agent's side-channel usage, so the next turn's usage
// carries them instead of dropping them.
func TestW3GlobalCompressReportsSideChannelUsage(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	prov := &compressScriptProvider{usage: model.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}}
	a.Provider = prov
	for i := 0; i < 12; i++ {
		if err := a.Memory.AddMessage(model.Message{Role: "user", Content: "turn message"}); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
	}
	before, after, summaryLen, err := a.Compress(context.Background(), 10)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if before != 12 || after != 10 || summaryLen == 0 {
		t.Fatalf("Compress: before=%d after=%d summaryLen=%d, want 12/10/>0", before, after, summaryLen)
	}
	// The next turn runs on the stub provider (zero usage), so
	// its TurnFinished total must be exactly the 1500 side
	// tokens the /compress reported.
	var finishedTokens = -1
	_, _, err = a.HandleStream(context.Background(), "hi", func(ev core.ProgressEvent) {
		if ev.Kind == core.EventTurnFinished {
			finishedTokens = ev.Usage.TotalTokens
		}
	})
	if err != nil {
		t.Fatalf("HandleStream after /compress: %v", err)
	}
	if finishedTokens != 1500 {
		t.Errorf("next turn's TurnFinished tokens = %d, want 1500 (the /compress side-channel tokens, folded into the next turn)", finishedTokens)
	}
}

// TestW3CompressRedactsSecretsAndCapsInput proves the W1
// follow-up on the /compress path (F-A6-P2-2/P2-3 shape): the
// summarisation prompt was built from the raw prefix with no
// total cap and no redaction. A fake secret in the prefix must
// reach neither the provider (prompt) nor the stored fact (the
// provider echoing it back must not smuggle it into storage),
// and one giant message must be truncated to the 16,000-token
// total cap instead of being sent whole.
func TestW3CompressRedactsSecretsAndCapsInput(t *testing.T) {
	// Fake test key only (sk- + 20+ alphanumerics is the shape
	// security.RedactSecrets recognises); it must never survive
	// into a prompt, a fact, or a returned summary.
	fakeKey := "sk-proj-FAKE0123456789abcdefTESTKEY"
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(100000)
	prov := &recordingCompressProvider{
		summary: "The user shared their key " + fakeKey + " and discussed the project plan.",
		usage:   model.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
	}
	a.Provider = prov
	cs, err := a.chatSessionFor(111)
	if err != nil {
		t.Fatalf("chatSessionFor: %v", err)
	}
	// One giant message (~25,000 tokens) plus small ones, then
	// the 10-message recent window: the old prefix is far over
	// the 16,000-token cap on its own.
	giant := strings.Repeat("word ", 20000)
	msgs := []model.Message{
		{Role: "user", Content: giant},
		{Role: "user", Content: "My API key is " + fakeKey + " — remember it for later."},
	}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, model.Message{Role: "user", Content: "turn message"})
	}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, model.Message{Role: "assistant", Content: "recent reply"})
	}
	for _, m := range msgs {
		if err := cs.store.AddMessage(m); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
	}
	sessionMemory := "Decision: the key is " + fakeKey + " and the project plan was agreed."
	before, after, summaryLen, err := a.CompressWithSessionMemoryForChat(context.Background(), 111, 10, sessionMemory)
	if err != nil {
		t.Fatalf("CompressWithSessionMemoryForChat: %v", err)
	}
	if before != 22 || after != 10 || summaryLen == 0 {
		t.Fatalf("compress: before=%d after=%d summaryLen=%d, want 22/10/>0 (the giant message is still dropped whole)", before, after, summaryLen)
	}
	// The prompt the provider received: no secret, within cap.
	if strings.Contains(prov.prompt, fakeKey) {
		t.Errorf("summarisation prompt contains the fake secret (the raw prefix must be redacted before the provider call)")
	}
	if !strings.Contains(prov.prompt, "[REDACTED]") {
		t.Errorf("summarisation prompt has no [REDACTED] marker — the secret should have been replaced, not dropped silently\nprompt excerpt: %.200s", prov.prompt)
	}
	if got := model.CountTokens(prov.prompt); got > 16000 {
		t.Errorf("summarisation prompt = %d estimated tokens, want <= 16000 (total input cap)", got)
	}
	// The stored summary fact: no secret, redacted marker kept.
	fact, found, err := cs.store.GetFact("conversation-summary")
	if err != nil || !found {
		t.Fatalf("summary fact: found=%v err=%v", found, err)
	}
	if strings.Contains(fact.Value, fakeKey) {
		t.Errorf("stored summary fact contains the fake secret (a summariser echo must not reach storage)")
	}
	if !strings.Contains(fact.Value, "[REDACTED]") {
		t.Errorf("stored summary fact = %q, want it to carry the [REDACTED] marker", fact.Value)
	}
	if !strings.Contains(fact.Value, "(Drew on session memory.)") {
		t.Errorf("stored summary fact = %q, want the session-memory citation", fact.Value)
	}
}

// TestW3MidTurnCheckSeesFetchedSubagentSpend proves F-A6-P2-6: a
// turn that fetches a subagent result is caught MID-TURN when the
// child's spend blows the budget — even though the fetched tokens
// were charged to the global agent (here a no-op sink, the test
// stand-in for "charged elsewhere"), never to the chat agent's
// own usage. Without the fix the chat agent's usage (620) is
// under the 2000 limit at every check and the turn completes;
// with it the check sees 620 + 1500 pending child tokens and
// aborts before the final provider call.
func TestW3MidTurnCheckSeesFetchedSubagentSpend(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(2000)
	mgr := subagent.NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, subagent.Usage, error) {
		return "child done", subagent.Usage{Tokens: 1500}, nil
	})
	a.Subagents = mgr
	subTools, err := tools.NewSubagentTools(mgr, tools.UsageSinkFunc(func(int) {}))
	if err != nil {
		t.Fatalf("NewSubagentTools: %v", err)
	}
	for _, tool := range subTools {
		if err := a.Tools.Register(tool); err != nil {
			t.Fatalf("Register %s: %v", tool.Name(), err)
		}
	}
	// The second provider call waits until the spawned child has
	// finished, so the fetch deterministically happens after the
	// child spent its 1500 tokens.
	childDone := make(chan struct{})
	var sawDone atomic.Bool
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			for _, ts := range mgr.List() {
				if ts.Status == subagent.StatusDone {
					sawDone.Store(true)
					close(childDone)
					return
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
		close(childDone)
	}()
	prov := &usageScriptProvider{chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		switch call {
		case 1:
			return &model.ChatResponse{
				Message: model.Message{
					Role:      "assistant",
					ToolCalls: []model.ToolCall{{ID: "c1", Name: "subagent_spawn", Arguments: `{"brief":"count the words in the notes"}`}},
				},
				FinishReason: "tool_calls",
				Usage:        model.Usage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10},
			}, nil
		case 2:
			<-childDone
			id := ""
			for _, ts := range mgr.List() {
				id = ts.ID
			}
			if id == "" {
				t.Error("no spawned child to fetch")
			}
			return &model.ChatResponse{
				Message: model.Message{
					Role:      "assistant",
					ToolCalls: []model.ToolCall{{ID: "c2", Name: "subagent_result", Arguments: `{"id":"` + id + `"}`}},
				},
				FinishReason: "tool_calls",
				Usage:        model.Usage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10},
			}, nil
		case 3:
			// The final answer: 600 more main-loop tokens. The
			// mid-turn check after this response must abort the
			// turn (620 main + 1500 child > 2000) instead of
			// letting it complete.
			return finalUsageResponse("done", model.Usage{PromptTokens: 400, CompletionTokens: 200, TotalTokens: 600}), nil
		default:
			return finalUsageResponse("done again", model.Usage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10}), nil
		}
	}}
	a.Provider = prov

	_, stats, err := a.HandleStreamForChat(permissions.WithApprovalContext(context.Background(), 111, 111), 111, "delegate this", nil)
	if !sawDone.Load() {
		t.Error("the spawned child never finished mid-turn (fetch timing not exercised)")
	}
	if !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("HandleStreamForChat err = %v, want ErrBudgetExceeded (mid-turn abort on child spend)", err)
	}
	if got := prov.Calls(); got != 3 {
		t.Errorf("provider calls = %d, want 3 (the turn must stop after the over-budget response, with no further call)", got)
	}
	if stats == nil {
		t.Fatal("HandleStreamForChat stats = nil, want stats from the aborted turn")
	}
	if stats.Tokens != 2120 || stats.SubagentTokens != 1500 {
		t.Errorf("TurnStats = %+v, want {Tokens:2120 SubagentTokens:1500} (620 main + 1500 fetched child, charged once at turn end)", stats)
	}
	if got := a.Budget.Usage(111); got != 2120 {
		t.Errorf("Budget.Usage(111) after aborted turn = %d, want 2120 (the spend already happened, including the child)", got)
	}
}

// TestW3SweepChargesTimedOutTasks proves the F-A6-P2-7 timeout
// leg: a task that times out is swept (marked charged) exactly
// like a Done/Failed task, instead of being skipped forever.
// A timed-out executor that ignores its context has its late
// result discarded by the Manager, so the recorded usage here is
// 0 — the assertion that pins the fix is the swept mark itself:
// before the fix the task was never marked, so every later sweep
// re-examined it (and any usage recorded on the deadline race
// would never be charged). The second turn must charge nothing
// more for the same task.
func TestW3SweepChargesTimedOutTasks(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(100000)
	mgr := subagent.NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, subagent.Usage, error) {
		<-ctx.Done()
		return "late", subagent.Usage{Tokens: 700}, nil
	})
	a.Subagents = mgr
	prov := &usageScriptProvider{chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		return finalUsageResponse("done", model.Usage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10}), nil
	}}
	a.Provider = prov

	id, err := mgr.SpawnForChat(111, "slow work", 20*time.Millisecond)
	if err != nil {
		t.Fatalf("SpawnForChat: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if st, _ := mgr.Status(id); st == subagent.StatusTimeout {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s never timed out", id)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, stats, err := a.HandleStreamForChat(context.Background(), 111, "anything?", nil); err != nil {
		t.Fatalf("HandleStreamForChat: %v", err)
	} else if stats == nil || stats.Tokens != 10 {
		t.Fatalf("TurnStats = %+v, want Tokens 10 (the timed-out task recorded 0 tokens)", stats)
	}
	cs, err := a.chatSessionFor(111)
	if err != nil {
		t.Fatalf("chatSessionFor: %v", err)
	}
	a.chatsMu.Lock()
	marked := cs.sweptSubagents[id]
	a.chatsMu.Unlock()
	if !marked {
		t.Errorf("timed-out task %s was not marked swept after the turn (the sweep skipped it; it would never be charged)", id)
	}
	// Exactly once: a second turn charges nothing more.
	if _, stats, err := a.HandleStreamForChat(context.Background(), 111, "again?", nil); err != nil {
		t.Fatalf("second HandleStreamForChat: %v", err)
	} else if stats == nil || stats.SubagentTokens != 0 || stats.Tokens != 10 {
		t.Errorf("second TurnStats = %+v, want {Tokens:10 SubagentTokens:0}", stats)
	}
	if got := a.Budget.Usage(111); got != 20 {
		t.Errorf("Budget.Usage(111) = %d, want 20 (two 10-token turns; the timed-out task added 0 recorded tokens, once)", got)
	}
}

// TestW3SweepChargesLateFinishingTask proves the F-A6-P2-7
// late-finisher leg at the sweep level: a task that finished
// before the sweep's turnStart was silently dropped by the old
// turnStart lower bound (the first sweep of a turn whose start
// postdates the finish). The window is now anchored at the last
// sweep (or the zero time on the first sweep), so the task is
// charged.
func TestW3SweepChargesLateFinishingTask(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	mgr := subagent.NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, subagent.Usage, error) {
		return "done", subagent.Usage{Tokens: 250}, nil
	})
	a.Subagents = mgr
	if _, err := mgr.SpawnForChat(111, "work", time.Minute); err != nil {
		t.Fatalf("SpawnForChat: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		done := false
		for _, ts := range mgr.List() {
			if ts.Status == subagent.StatusDone {
				done = true
			}
		}
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not finish")
		}
		time.Sleep(2 * time.Millisecond)
	}
	cs, err := a.chatSessionFor(111)
	if err != nil {
		t.Fatalf("chatSessionFor: %v", err)
	}
	// A turnStart AFTER the task finished: the old bound charged 0.
	if got := a.sweepFinishedSubagents(cs, 111, time.Now().Add(time.Minute)); got != 250 {
		t.Errorf("sweep with a late turnStart = %d, want 250 (a task finishing before the turn must still be charged)", got)
	}
	if got := a.sweepFinishedSubagents(cs, 111, time.Now()); got != 0 {
		t.Errorf("second sweep = %d, want 0 (already charged)", got)
	}
}

// TestW3SweepChargesTaskFinishingBetweenTurns proves the same
// late-finisher leg end to end: a child spawned in turn 1 that
// finishes AFTER turn 1 ends is charged by turn 2's sweep.
func TestW3SweepChargesTaskFinishingBetweenTurns(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(100000)
	release := make(chan struct{})
	mgr := subagent.NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, subagent.Usage, error) {
		<-release
		return "child done", subagent.Usage{Tokens: 250}, nil
	})
	a.Subagents = mgr
	subTools, err := tools.NewSubagentTools(mgr, tools.UsageSinkFunc(func(int) {}))
	if err != nil {
		t.Fatalf("NewSubagentTools: %v", err)
	}
	for _, tool := range subTools {
		if err := a.Tools.Register(tool); err != nil {
			t.Fatalf("Register %s: %v", tool.Name(), err)
		}
	}
	prov := &usageScriptProvider{chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		switch call {
		case 1:
			return &model.ChatResponse{
				Message: model.Message{
					Role:      "assistant",
					ToolCalls: []model.ToolCall{{ID: "c1", Name: "subagent_spawn", Arguments: `{"brief":"slow work"}`}},
				},
				FinishReason: "tool_calls",
				Usage:        model.Usage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10},
			}, nil
		default:
			return finalUsageResponse("done", model.Usage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10}), nil
		}
	}}
	a.Provider = prov
	ctx := permissions.WithApprovalContext(context.Background(), 111, 111)

	// Turn 1 spawns the child and answers while it is still
	// running: nothing to sweep yet.
	if _, stats, err := a.HandleStreamForChat(ctx, 111, "delegate this", nil); err != nil {
		t.Fatalf("turn 1: %v", err)
	} else if stats == nil || stats.Tokens != 20 || stats.SubagentTokens != 0 {
		t.Fatalf("turn 1 stats = %+v, want {Tokens:20 SubagentTokens:0}", stats)
	}
	// The child finishes BETWEEN turns.
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		done := false
		for _, ts := range mgr.List() {
			if ts.Status == subagent.StatusDone {
				done = true
			}
		}
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not finish between turns")
		}
		time.Sleep(2 * time.Millisecond)
	}
	// Turn 2 charges the late finisher.
	if _, stats, err := a.HandleStreamForChat(ctx, 111, "anything else?", nil); err != nil {
		t.Fatalf("turn 2: %v", err)
	} else if stats == nil || stats.Tokens != 260 || stats.SubagentTokens != 250 {
		t.Errorf("turn 2 stats = %+v, want {Tokens:260 SubagentTokens:250} (10 main + 250 late child)", stats)
	}
	if got := a.Budget.Usage(111); got != 280 {
		t.Errorf("Budget.Usage(111) = %d, want 280 (20 turn 1 + 260 turn 2)", got)
	}
}

// TestW3CloseSweepsLateFinishingTasks proves the F-A6-P2-7
// session-close leg: a task that finished after the last turn
// has no next turn to sweep it, so session close is its last
// charging opportunity. A turn never runs here at all.
func TestW3CloseSweepsLateFinishingTasks(t *testing.T) {
	a := testApp(t)
	a.Budget = budget.New(100000)
	mgr := subagent.NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, subagent.Usage, error) {
		return "done", subagent.Usage{Tokens: 250}, nil
	})
	a.Subagents = mgr
	// The chat ran at least one turn before (that is how the child
	// got spawned), so its session exists — closeChatAgents sweeps
	// per chat session, and a chat with no session at all has no
	// turns to close.
	if _, err := a.chatSessionFor(111); err != nil {
		t.Fatalf("chatSessionFor: %v", err)
	}
	if _, err := mgr.SpawnForChat(111, "work", time.Minute); err != nil {
		t.Fatalf("SpawnForChat: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		done := false
		for _, ts := range mgr.List() {
			if ts.Status == subagent.StatusDone {
				done = true
			}
		}
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not finish")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := a.Budget.Usage(111); got != 250 {
		t.Errorf("Budget.Usage(111) after Close = %d, want 250 (the finished task must be charged at session close)", got)
	}
}
