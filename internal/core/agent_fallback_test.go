// In-turn provider failover tests.
//
// The acceptance criterion: a turn that fails on provider A (credit
// exhausted / auth error / rate-limit) continues on provider B from
// the SAME session state, history intact — no restart, no lost
// context. The failover happens inside the agent's step loop: the
// failed step is retried on the next provider with the same in-flight
// messages (tool results included), so nothing already done is
// re-executed and the history is never re-sent from scratch.
package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/tools"
)

// namedScriptProvider is a scriptable provider with a chosen name:
// the chain distinguishes providers by Name(), so the test doubles
// need distinct names (the shared fakeProvider is always "fake").
type namedScriptProvider struct {
	mu          sync.Mutex
	name        string
	chatCalls   int
	streamCalls int
	lastReq     model.ChatRequest
	chatFn      func(call int, req model.ChatRequest) (*model.ChatResponse, error)
	streamFn    func(call int, req model.ChatRequest, emit func(string) error) (*model.ChatResponse, error)
}

func (p *namedScriptProvider) Name() string { return p.name }

func (p *namedScriptProvider) Chat(_ context.Context, req model.ChatRequest) (*model.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.chatCalls++
	p.lastReq = req
	if p.chatFn != nil {
		return p.chatFn(p.chatCalls, req)
	}
	return textResponse("answer from " + p.name), nil
}

func (p *namedScriptProvider) Stream(_ context.Context, req model.ChatRequest, fn func(chunk string) error) (*model.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.streamCalls++
	p.lastReq = req
	if p.streamFn != nil {
		return p.streamFn(p.streamCalls, req, fn)
	}
	if err := fn("answer from " + p.name); err != nil {
		return nil, err
	}
	return textResponse("answer from " + p.name), nil
}

func (p *namedScriptProvider) chatCallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.chatCalls
}

func (p *namedScriptProvider) streamCallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.streamCalls
}

func (p *namedScriptProvider) lastRequest() model.ChatRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastReq
}

func billingError() error {
	// A real provider returns a typed *APIError (see apiError);
	// the doubles do the same so classification is exercised
	// exactly as in production.
	return &model.APIError{Provider: "deepseek", Status: 402, Body: "Insufficient Balance"}
}

func authError() error {
	return &model.APIError{Provider: "deepseek", Status: 401, Body: "Incorrect API key provided"}
}

func rateLimitError() error {
	return &model.APIError{Provider: "deepseek", Status: 429, Body: "Rate limit reached"}
}

func badRequestError() error {
	return &model.APIError{Provider: "deepseek", Status: 400, Body: "bad request"}
}

func testChain(t *testing.T, providers map[string]*namedScriptProvider, order ...string) *model.FailoverChain {
	t.Helper()
	return model.NewFailoverChain(order, func(name string) (model.Provider, error) {
		p, ok := providers[name]
		if !ok {
			return nil, errors.New("test chain: no provider named " + name)
		}
		return p, nil
	})
}

// runWithTurnFinished runs one turn and captures the TurnFinished
// event (the serving provider, hops and per-provider usage ride on
// it — the caller must not read the agent's provider afterwards,
// because the run may already have moved on).
func runWithTurnFinished(t *testing.T, a *Agent, input string) (string, ProgressEvent, error) {
	t.Helper()
	var finished ProgressEvent
	reply, err := a.RunWithEventHandler(context.Background(), input, func(ev ProgressEvent) {
		if ev.Kind == EventTurnFinished {
			finished = ev
		}
	})
	return reply, finished, err
}

func TestAgentFallbackBillingContinuesOnNextProvider(t *testing.T) {
	deepseek := &namedScriptProvider{name: "deepseek", chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
		return nil, billingError()
	}}
	openai := &namedScriptProvider{name: "openai", chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
		return &model.ChatResponse{
			Message:      model.Message{Role: "assistant", Content: "answered by openai"},
			FinishReason: "stop",
			Usage:        model.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
		}, nil
	}}
	a := newTestAgent(t, testConfig(5), deepseek, tools.NewRegistry(), newFakeStore())
	a.SetFallbackChain(testChain(t, map[string]*namedScriptProvider{"deepseek": deepseek, "openai": openai}, "deepseek", "openai"))
	reply, finished, err := runWithTurnFinished(t, a, "hello")
	if err != nil {
		t.Fatalf("Run with a 402 on deepseek: %v (want the turn to continue on openai)", err)
	}
	if reply != "answered by openai" {
		t.Errorf("reply = %q, want the openai answer", reply)
	}
	if deepseek.chatCallCount() != 1 {
		t.Errorf("deepseek Chat calls = %d, want 1 (one attempt, no blind retry)", deepseek.chatCallCount())
	}
	if openai.chatCallCount() != 1 {
		t.Errorf("openai Chat calls = %d, want 1", openai.chatCallCount())
	}
	if len(finished.FallbackHops) != 1 {
		t.Fatalf("FallbackHops = %+v, want exactly one hop", finished.FallbackHops)
	}
	hop := finished.FallbackHops[0]
	if hop.From != "deepseek" || hop.To != "openai" || hop.Class != model.FailoverBilling {
		t.Errorf("hop = %+v, want deepseek -> openai (billing)", hop)
	}
	if finished.ServingProvider != "openai" {
		t.Errorf("ServingProvider = %q, want openai (the provider that answered)", finished.ServingProvider)
	}
	if got := finished.ProviderUsage["openai"].TotalTokens; got != 120 {
		t.Errorf("ProviderUsage[openai] = %d tokens, want 120", got)
	}
	if finished.Usage.TotalTokens != 120 {
		t.Errorf("turn Usage.TotalTokens = %d, want 120", finished.Usage.TotalTokens)
	}
}

func TestAgentFallbackAuthAndRateLimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		class model.FailoverClass
	}{
		{"auth", authError(), model.FailoverAuth},
		{"rate-limit", rateLimitError(), model.FailoverRateLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deepseek := &namedScriptProvider{name: "deepseek", chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
				return nil, tc.err
			}}
			ollama := &namedScriptProvider{name: "ollama"}
			a := newTestAgent(t, testConfig(5), deepseek, tools.NewRegistry(), newFakeStore())
			a.SetFallbackChain(testChain(t, map[string]*namedScriptProvider{"deepseek": deepseek, "ollama": ollama}, "deepseek", "ollama"))
			reply, finished, err := runWithTurnFinished(t, a, "hello")
			if err != nil {
				t.Fatalf("Run: %v, want continuation on ollama", err)
			}
			if reply != "answer from ollama" {
				t.Errorf("reply = %q, want the ollama answer", reply)
			}
			if len(finished.FallbackHops) != 1 || finished.FallbackHops[0].Class != tc.class {
				t.Errorf("hops = %+v, want one %v hop", finished.FallbackHops, tc.class)
			}
		})
	}
}

func TestAgentNoFallbackOnBadRequest(t *testing.T) {
	// A 400 is the caller's fault (bad prompt / bad tool args):
	// another provider would reject the same request, so the
	// turn fails honestly instead of burning a call elsewhere.
	deepseek := &namedScriptProvider{name: "deepseek", chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
		return nil, badRequestError()
	}}
	openai := &namedScriptProvider{name: "openai"}
	a := newTestAgent(t, testConfig(5), deepseek, tools.NewRegistry(), newFakeStore())
	a.SetFallbackChain(testChain(t, map[string]*namedScriptProvider{"deepseek": deepseek, "openai": openai}, "deepseek", "openai"))
	_, _, err := runWithTurnFinished(t, a, "hello")
	if err == nil {
		t.Fatal("Run with a 400: expected an error, got nil (a bad request must not fail over)")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error = %v, want it to carry the 400", err)
	}
	if openai.chatCallCount() != 0 {
		t.Errorf("openai Chat calls = %d, want 0 (no failover on a bad request)", openai.chatCallCount())
	}
}

func TestAgentFallbackExhaustedReportsAttempts(t *testing.T) {
	deepseek := &namedScriptProvider{name: "deepseek", chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
		return nil, billingError()
	}}
	openai := &namedScriptProvider{name: "openai", chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
		return nil, rateLimitError()
	}}
	a := newTestAgent(t, testConfig(5), deepseek, tools.NewRegistry(), newFakeStore())
	a.SetFallbackChain(testChain(t, map[string]*namedScriptProvider{"deepseek": deepseek, "openai": openai}, "deepseek", "openai"))
	_, finished, err := runWithTurnFinished(t, a, "hello")
	if err == nil {
		t.Fatal("Run with every provider failing: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "fallback") {
		t.Errorf("error = %v, want it to name the exhausted fallback", err)
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error = %v, want it to carry the last failure (429)", err)
	}
	if deepseek.chatCallCount() != 1 || openai.chatCallCount() != 1 {
		t.Errorf("calls = deepseek %d, openai %d, want 1 each (one attempt per provider)", deepseek.chatCallCount(), openai.chatCallCount())
	}
	if len(finished.FallbackHops) != 1 {
		t.Errorf("FallbackHops = %+v, want the single deepseek -> openai hop", finished.FallbackHops)
	}
}

func TestAgentFallbackPreservesToolResultsAndHistory(t *testing.T) {
	// Step 1 runs a tool on provider A; step 2 fails on A and the
	// turn continues on B. B must see the tool result in its
	// request (history intact) and the tool must NOT run again.
	echo := &echoTool{}
	reg := tools.NewRegistry()
	if err := reg.Register(echo); err != nil {
		t.Fatalf("Register calculator: %v", err)
	}
	deepseek := &namedScriptProvider{name: "deepseek", chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
		switch call {
		case 1:
			return toolCallResponse("call-1", "calculator", `{"text":"hi"}`), nil
		default:
			return nil, billingError()
		}
	}}
	openai := &namedScriptProvider{name: "openai", chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
		return &model.ChatResponse{
			Message:      model.Message{Role: "assistant", Content: "tool said calculator:hi"},
			FinishReason: "stop",
			Usage:        model.Usage{PromptTokens: 200, CompletionTokens: 10, TotalTokens: 210},
		}, nil
	}}
	mem := newFakeStore()
	a := newTestAgent(t, testConfig(5), deepseek, reg, mem)
	a.SetFallbackChain(testChain(t, map[string]*namedScriptProvider{"deepseek": deepseek, "openai": openai}, "deepseek", "openai"))
	reply, finished, err := runWithTurnFinished(t, a, "say hi via echo")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "tool said calculator:hi" {
		t.Errorf("reply = %q, want the openai follow-up", reply)
	}
	if got := echo.callCount(); got != 1 {
		t.Errorf("tool calls = %d, want 1 (the switch must not re-execute tools)", got)
	}
	// Provider B's request carries the tool result: it continued
	// mid-turn from the same messages, it did not restart.
	req := openai.lastRequest()
	found := false
	for _, m := range req.Messages {
		if m.Role == "tool" && m.ToolCallID == "call-1" && m.Content == "calculator:hi" {
			found = true
		}
	}
	if !found {
		t.Errorf("openai's request lacks the tool result; messages = %+v", req.Messages)
	}
	// Usage is attributed per provider, not lumped on the one
	// that answered: step 1 ran on deepseek.
	if got := finished.ProviderUsage["deepseek"].TotalTokens; got != 0 {
		// Step 1's tool-call response carried no usage in this
		// script (zero report), so deepseek's bucket is 0 — the
		// point is the bucket EXISTS and openai's is exact.
		t.Errorf("ProviderUsage[deepseek] = %d, want 0 (step 1 reported no usage)", got)
	}
	if got := finished.ProviderUsage["openai"].TotalTokens; got != 210 {
		t.Errorf("ProviderUsage[openai] = %d, want 210", got)
	}
}

func TestAgentFallbackOneAttemptPerProviderPerTurn(t *testing.T) {
	// Three providers, two failures, one answer: each provider is
	// tried exactly once in the turn — the chain never re-retries
	// a provider the turn already saw fail (the provider layer
	// already retried internally up to 3x; the chain must not
	// multiply that).
	deepseek := &namedScriptProvider{name: "deepseek", chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
		return nil, billingError()
	}}
	openai := &namedScriptProvider{name: "openai", chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
		return nil, rateLimitError()
	}}
	ollama := &namedScriptProvider{name: "ollama"}
	a := newTestAgent(t, testConfig(5), deepseek, tools.NewRegistry(), newFakeStore())
	a.SetFallbackChain(testChain(t, map[string]*namedScriptProvider{"deepseek": deepseek, "openai": openai, "ollama": ollama}, "deepseek", "openai", "ollama"))
	reply, finished, err := runWithTurnFinished(t, a, "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "answer from ollama" {
		t.Errorf("reply = %q, want the ollama answer", reply)
	}
	if deepseek.chatCallCount() != 1 || openai.chatCallCount() != 1 || ollama.chatCallCount() != 1 {
		t.Errorf("calls = deepseek %d, openai %d, ollama %d, want 1 each", deepseek.chatCallCount(), openai.chatCallCount(), ollama.chatCallCount())
	}
	if len(finished.FallbackHops) != 2 {
		t.Errorf("FallbackHops = %+v, want two hops", finished.FallbackHops)
	}
}

func TestAgentFallbackStreamBeforeChunks(t *testing.T) {
	// A stream that fails before delivering any chunk can fail
	// over cleanly: nothing was shown to the user yet, so the
	// next provider's stream starts the step from scratch.
	deepseek := &namedScriptProvider{name: "deepseek", streamFn: func(call int, _ model.ChatRequest, _ func(string) error) (*model.ChatResponse, error) {
		return nil, billingError()
	}}
	openai := &namedScriptProvider{name: "openai"}
	a := newTestAgent(t, testConfig(5), deepseek, tools.NewRegistry(), newFakeStore())
	a.SetFallbackChain(testChain(t, map[string]*namedScriptProvider{"deepseek": deepseek, "openai": openai}, "deepseek", "openai"))
	var chunks strings.Builder
	out, err := a.RunStream(context.Background(), "hello", func(chunk string) error {
		chunks.WriteString(chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("RunStream: %v, want continuation on openai", err)
	}
	if out != "answer from openai" {
		t.Errorf("RunStream = %q, want the openai answer", out)
	}
	if deepseek.streamCallCount() != 1 || openai.streamCallCount() != 1 {
		t.Errorf("stream calls = deepseek %d, openai %d, want 1 each", deepseek.streamCallCount(), openai.streamCallCount())
	}
}

func TestAgentFallbackStreamNoFailoverAfterChunks(t *testing.T) {
	// A stream that already delivered text cannot fail over: the
	// user has seen the first provider's partial reply, and a
	// second provider would duplicate it. The turn fails honestly
	// instead.
	deepseek := &namedScriptProvider{name: "deepseek", streamFn: func(call int, _ model.ChatRequest, emit func(string) error) (*model.ChatResponse, error) {
		_ = emit("partial-")
		return nil, billingError()
	}}
	openai := &namedScriptProvider{name: "openai"}
	a := newTestAgent(t, testConfig(5), deepseek, tools.NewRegistry(), newFakeStore())
	a.SetFallbackChain(testChain(t, map[string]*namedScriptProvider{"deepseek": deepseek, "openai": openai}, "deepseek", "openai"))
	_, err := a.RunStream(context.Background(), "hello", func(chunk string) error { return nil })
	if err == nil {
		t.Fatal("RunStream after delivered chunks: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "402") {
		t.Errorf("error = %v, want it to carry the 402", err)
	}
	if openai.streamCallCount() != 0 {
		t.Errorf("openai Stream calls = %d, want 0 (no failover after chunks were delivered)", openai.streamCallCount())
	}
}

func TestAgentFallbackStaysOnFallbackWhilePrimaryCooled(t *testing.T) {
	// Turn 1 fails over to B (A is benched for 60m on billing).
	// Turn 2 starts while A is still cooled: it must NOT waste
	// its first attempt on A — it starts on B directly.
	deepseek := &namedScriptProvider{name: "deepseek", chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
		return nil, billingError()
	}}
	openai := &namedScriptProvider{name: "openai"}
	a := newTestAgent(t, testConfig(5), deepseek, tools.NewRegistry(), newFakeStore())
	a.SetFallbackChain(testChain(t, map[string]*namedScriptProvider{"deepseek": deepseek, "openai": openai}, "deepseek", "openai"))
	if _, _, err := runWithTurnFinished(t, a, "first"); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if deepseek.chatCallCount() != 1 || openai.chatCallCount() != 1 {
		t.Fatalf("turn 1 calls = deepseek %d, openai %d, want 1,1", deepseek.chatCallCount(), openai.chatCallCount())
	}
	if _, _, err := runWithTurnFinished(t, a, "second"); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if deepseek.chatCallCount() != 1 {
		t.Errorf("turn 2 called deepseek again (%d calls): a cooled primary must not be retried", deepseek.chatCallCount())
	}
	if openai.chatCallCount() != 2 {
		t.Errorf("turn 2 openai calls = %d, want 2 (turn 2 runs on openai directly)", openai.chatCallCount())
	}
}

func TestAgentFallbackRestoresPrimaryWhenCooldownClear(t *testing.T) {
	// The agent is left on the fallback provider (as a previous
	// turn's failover would leave it) with nothing cooled: the
	// next turn restores the primary before the first step.
	deepseek := &namedScriptProvider{name: "deepseek"}
	openai := &namedScriptProvider{name: "openai"}
	a := newTestAgent(t, testConfig(5), openai, tools.NewRegistry(), newFakeStore())
	a.SetFallbackChain(testChain(t, map[string]*namedScriptProvider{"deepseek": deepseek, "openai": openai}, "deepseek", "openai"))
	// Simulate "left on fallback": the provider is openai and the
	// flag says it came from a failover (same package, so the
	// test may set the unexported state directly).
	a.provider = openai
	a.fallbackProvider = true
	if _, _, err := runWithTurnFinished(t, a, "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if deepseek.chatCallCount() != 1 {
		t.Errorf("deepseek Chat calls = %d, want 1 (primary restored at turn start)", deepseek.chatCallCount())
	}
	if openai.chatCallCount() != 0 {
		t.Errorf("openai Chat calls = %d, want 0 (no longer on fallback)", openai.chatCallCount())
	}
}

func TestAgentSetProviderIsExplicit(t *testing.T) {
	// An explicit provider set (what --once uses) is NOT a
	// failover: the next turn must not "restore" away from it.
	deepseek := &namedScriptProvider{name: "deepseek"}
	openai := &namedScriptProvider{name: "openai"}
	a := newTestAgent(t, testConfig(5), deepseek, tools.NewRegistry(), newFakeStore())
	a.SetFallbackChain(testChain(t, map[string]*namedScriptProvider{"deepseek": deepseek, "openai": openai}, "deepseek", "openai"))
	a.SetProvider(openai)
	if got := a.ProviderName(); got != "openai" {
		t.Fatalf("ProviderName after SetProvider = %q, want openai", got)
	}
	if _, _, err := runWithTurnFinished(t, a, "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if openai.chatCallCount() != 1 {
		t.Errorf("openai Chat calls = %d, want 1 (explicit provider kept)", openai.chatCallCount())
	}
	if deepseek.chatCallCount() != 0 {
		t.Errorf("deepseek Chat calls = %d, want 0 (no restore after an explicit set)", deepseek.chatCallCount())
	}
}

func TestAgentNoChainMeansNoFallback(t *testing.T) {
	// Without a chain configured, a provider failure is exactly
	// the old behaviour: the turn fails with the provider error.
	deepseek := &namedScriptProvider{name: "deepseek", chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
		return nil, billingError()
	}}
	a := newTestAgent(t, testConfig(5), deepseek, tools.NewRegistry(), newFakeStore())
	_, err := a.Run(context.Background(), "hello")
	if err == nil {
		t.Fatal("Run without a chain: expected the provider error, got nil")
	}
	if !strings.Contains(err.Error(), "402") {
		t.Errorf("error = %v, want it to carry the 402", err)
	}
}
