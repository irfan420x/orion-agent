// App-level fallback tests: the whole path from a Telegram chat turn
// through the agent loop and back, against fake provider servers.
//
// The fake servers are real model.Provider instances (deepseek and
// openai providers pointed at httptest URLs), so these tests prove
// the wired behaviour — chain factory, agent failover, budget
// recording, reply notice — not just the loop in isolation.
package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/irfan/orion/internal/budget"
	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/security"
	"github.com/irfan/orion/internal/tools"
)

// fakeProviderServer is an OpenAI-compatible chat-completions server
// whose behaviour the test drives: it either answers with content or
// fails with a status. Every request body is captured for assertions
// about what the provider was actually sent.
type fakeProviderServer struct {
	mu       sync.Mutex
	srv      *httptest.Server
	failWith int    // 0 = answer; otherwise the HTTP status to fail with
	failBody string // body for the failure
	answer   string // content for the success case
	usage    string // usage JSON fragment for the success case
	bodies   []string
	models   []string
}

func newFakeProviderServer(t *testing.T, answer string) *fakeProviderServer {
	t.Helper()
	f := &fakeProviderServer{answer: answer, usage: `"prompt_tokens":10,"completion_tokens":2,"total_tokens":12`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(body))
		var raw struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &raw)
		f.models = append(f.models, raw.Model)
		failWith, failBody, answer := f.failWith, f.failBody, f.answer
		f.mu.Unlock()
		if failWith != 0 {
			w.WriteHeader(failWith)
			_, _ = w.Write([]byte(failBody))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":` + quoteJSON(answer) + `},"finish_reason":"stop"}],"usage":{` + f.usage + `}}`))
	}))
	return f
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (f *fakeProviderServer) setFail(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWith, f.failBody = status, body
}

func (f *fakeProviderServer) setAnswer(answer string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWith, f.answer = 0, answer
}

func (f *fakeProviderServer) setUsage(usage string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usage = usage
}

func (f *fakeProviderServer) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

func (f *fakeProviderServer) requestModels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.models...)
}

// fallbackTestApp builds an App whose default provider (deepseek)
// and fallback provider (openai) are fake servers. The chain is
// wired exactly as Build wires it (config order + resolveProvider
// factory); the App is hand-built otherwise, following testApp.
func fallbackTestApp(t *testing.T) (*App, *fakeProviderServer, *fakeProviderServer) {
	t.Helper()
	dir := t.TempDir()
	deepseekSrv := newFakeProviderServer(t, "from-deepseek")
	t.Cleanup(deepseekSrv.srv.Close)
	openaiSrv := newFakeProviderServer(t, "from-openai")
	t.Cleanup(openaiSrv.srv.Close)
	mem, err := memory.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	reg, err := tools.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.MaxSteps = 5
	cfg.TimeoutSeconds = 30
	cfg.DefaultProvider = "deepseek"
	cfg.Providers["deepseek"] = config.ProviderConfig{Name: "deepseek", APIKey: "test-fake-key", BaseURL: deepseekSrv.srv.URL, Model: "deepseek-flash"}
	cfg.Providers["openai"] = config.ProviderConfig{Name: "openai", APIKey: "test-fake-key", BaseURL: openaiSrv.srv.URL, Model: "gpt-4o-mini"}
	cfg.FallbackProviders = []string{"openai"}
	// The App's provider is the REAL deepseek fake-server
	// provider (not the stub): a chat without an override reuses
	// App.Provider, so the stub would never touch the fake
	// servers and the 402s would never fire.
	deepseekProv, err := model.NewProvider(model.ProviderConfig{Name: "deepseek", APIKey: "test-fake-key", BaseURL: deepseekSrv.srv.URL, Model: "deepseek-flash"})
	if err != nil {
		t.Fatalf("NewProvider deepseek: %v", err)
	}
	agent, err := core.NewAgent(cfg, deepseekProv, reg, mem)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	audit, err := security.NewAuditLog(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	a := &App{Config: cfg, Provider: deepseekProv, Tools: reg, Memory: mem, Agent: agent, Audit: audit, Budget: budget.New(100000)}
	a.fallbackChain = model.NewFailoverChain(cfg.FallbackChain(), func(name string) (model.Provider, error) {
		p, _, _, err := a.resolveProvider(name)
		return p, err
	})
	return a, deepseekSrv, openaiSrv
}

func TestChatTurnFallsBackOnBillingError(t *testing.T) {
	a, deepseekSrv, openaiSrv := fallbackTestApp(t)
	defer a.Close()
	deepseekSrv.setFail(402, `{"error":{"message":"Insufficient Balance","type":"insufficient_quota"}}`)
	const chatID = 7
	reply, stats, err := a.HandleStreamForChat(context.Background(), chatID, "first question", nil)
	if err != nil {
		t.Fatalf("HandleStreamForChat with deepseek 402: %v (want continuation on openai)", err)
	}
	if !strings.Contains(reply, "from-openai") {
		t.Errorf("reply = %q, want the openai answer", reply)
	}
	// The user is told the switch happened: a silent provider
	// change would be a surprise on the bill and in the footer.
	if !strings.Contains(reply, "fallback") || !strings.Contains(reply, "openai") {
		t.Errorf("reply = %q, want a fallback notice naming openai", reply)
	}
	if stats == nil {
		t.Fatal("stats = nil, want TurnStats from the continued turn")
	}
	if stats.Provider != "openai" {
		t.Errorf("stats.Provider = %q, want openai (the provider that answered)", stats.Provider)
	}
	if stats.Model != "gpt-4o-mini" {
		t.Errorf("stats.Model = %q, want gpt-4o-mini", stats.Model)
	}
	if len(stats.FallbackHops) != 1 || stats.FallbackHops[0].Class != model.FailoverBilling {
		t.Errorf("stats.FallbackHops = %+v, want one billing hop", stats.FallbackHops)
	}
	if got := len(deepseekSrv.requests()); got != 1 {
		t.Errorf("deepseek requests = %d, want 1 (one attempt, no blind retry)", got)
	}
	if got := len(openaiSrv.requests()); got != 1 {
		t.Errorf("openai requests = %d, want 1", got)
	}
	// Token-aware: the failed attempt reported no usage, so the
	// budget holds exactly the serving provider's tokens, under
	// the serving provider/model — not lumped on deepseek.
	byModel := a.Budget.DayUsageByModel(chatID)
	if got := byModel[budget.ModelKey{Provider: "openai", Model: "gpt-4o-mini"}].Total; got != 12 {
		t.Errorf("budget openai/gpt-4o-mini = %d tokens, want 12", got)
	}
	if _, ok := byModel[budget.ModelKey{Provider: "deepseek", Model: "deepseek-flash"}]; ok {
		t.Errorf("budget has a deepseek bucket %+v, want none (the failed attempt spent nothing reportable)", byModel)
	}
	// History intact: a second turn sends the first turn's user
	// text to the provider again — and it stays on the fallback:
	// deepseek's billing bench (60m) is still active, so the
	// turn must not waste its first attempt on it.
	openaiSrv.setAnswer("from-openai-again")
	reply2, _, err := a.HandleStreamForChat(context.Background(), chatID, "second question", nil)
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if !strings.Contains(reply2, "from-openai-again") {
		t.Errorf("second turn reply = %q, want openai again (deepseek still benched)", reply2)
	}
	if got := len(deepseekSrv.requests()); got != 1 {
		t.Errorf("deepseek requests after turn 2 = %d, want 1 (a cooled primary must not be retried)", got)
	}
	found := false
	for _, b := range openaiSrv.requests() {
		if strings.Contains(b, "first question") {
			found = true
		}
	}
	if !found {
		t.Error("no openai request in the second turn carried the first turn's user text (history lost across the switch)")
	}
}

func TestChatTurnFallbackRecordsCacheSplit(t *testing.T) {
	// The serving provider reports the 2026-11-19 benchmark
	// shape (10,530 prompt, 9,000 cache hits, 3 completion): after
	// a deepseek 402 failover the turn's stats and the budget
	// bucket carry the split under openai — the failed leg spent
	// nothing reportable, and the cache saving is visible, not
	// hidden inside the total.
	a, deepseekSrv, openaiSrv := fallbackTestApp(t)
	defer a.Close()
	deepseekSrv.setFail(402, `{"error":{"message":"Insufficient Balance","type":"insufficient_quota"}}`)
	openaiSrv.setUsage(`"prompt_tokens":10530,"completion_tokens":3,"total_tokens":10533,"prompt_cache_hit_tokens":9000,"prompt_cache_miss_tokens":1530`)
	const chatID = 11
	reply, stats, err := a.HandleStreamForChat(context.Background(), chatID, "cache question", nil)
	if err != nil {
		t.Fatalf("HandleStreamForChat with deepseek 402: %v", err)
	}
	if !strings.Contains(reply, "from-openai") {
		t.Errorf("reply = %q, want the openai answer", reply)
	}
	if stats == nil {
		t.Fatal("stats = nil, want TurnStats from the continued turn")
	}
	if stats.CacheHitTokens != 9000 || !stats.CacheReported {
		t.Errorf("stats cache = %d hits (reported=%v), want 9000 hits reported", stats.CacheHitTokens, stats.CacheReported)
	}
	if stats.PromptTokens != 10530 || stats.Tokens != 10533 {
		t.Errorf("stats prompt/total = %d/%d, want 10530/10533", stats.PromptTokens, stats.Tokens)
	}
	byModel := a.Budget.DayUsageByModel(chatID)
	if got := byModel[budget.ModelKey{Provider: "openai", Model: "gpt-4o-mini"}].CacheHit; got != 9000 {
		t.Errorf("budget openai/gpt-4o-mini CacheHit = %d, want 9000", got)
	}
	if _, ok := byModel[budget.ModelKey{Provider: "deepseek", Model: "deepseek-flash"}]; ok {
		t.Errorf("budget has a deepseek bucket %+v, want none (the failed attempt spent nothing reportable)", byModel)
	}
}

func TestChatTurnWithoutChainFailsHonestly(t *testing.T) {
	// No fallback_providers configured: the old behaviour —
	// the turn fails with the provider error. Fallback is
	// opt-in config, never a surprise.
	a, deepseekSrv, _ := fallbackTestApp(t)
	defer a.Close()
	a.fallbackChain = nil
	deepseekSrv.setFail(402, `{"error":{"message":"Insufficient Balance"}}`)
	_, _, err := a.HandleStreamForChat(context.Background(), 7, "hello", nil)
	if err == nil {
		t.Fatal("turn without a chain: expected the 402 error, got nil")
	}
	if !strings.Contains(err.Error(), "402") {
		t.Errorf("error = %v, want it to carry the 402", err)
	}
}

func TestChatOnceSwitch(t *testing.T) {
	a, _, _ := fallbackTestApp(t)
	defer a.Close()
	const chatID = 7
	if err := a.SwitchOnceForChat(chatID, "openai"); err != nil {
		t.Fatalf("SwitchOnceForChat: %v", err)
	}
	// The persistent route is untouched by --once.
	if provider, modelName := a.chatProviderInfo(chatID); provider != "deepseek" || modelName != "deepseek-flash" {
		t.Errorf("route after --once = %s/%s, want deepseek/deepseek-flash (once is not a persistent switch)", provider, modelName)
	}
	reply, _, err := a.HandleStreamForChat(context.Background(), chatID, "hello", nil)
	if err != nil {
		t.Fatalf("once turn: %v", err)
	}
	if !strings.Contains(reply, "from-openai") {
		t.Errorf("once turn reply = %q, want the openai answer", reply)
	}
	// The next turn is back on the persistent route, with no
	// further action: --once restores itself.
	reply, _, err = a.HandleStreamForChat(context.Background(), chatID, "hello again", nil)
	if err != nil {
		t.Fatalf("turn after once: %v", err)
	}
	if !strings.Contains(reply, "from-deepseek") {
		t.Errorf("turn after once reply = %q, want the deepseek answer (restored)", reply)
	}
	// --once is never persisted: no override file is written, so
	// a restart (or a fresh App on the same data dir) never
	// inherits the one-turn route.
	if _, err := os.Stat(filepath.Join(a.Config.DataDir, "chats", "7", "provider.json")); !os.IsNotExist(err) {
		t.Errorf("--once wrote a provider override file (err = %v); a one-turn switch must leave no trace on disk", err)
	}
}

func TestChatSameProviderModelSwitch(t *testing.T) {
	a, deepseekSrv, _ := fallbackTestApp(t)
	defer a.Close()
	const chatID = 7
	if err := a.SwitchProviderForChat(chatID, "deepseek/deepseek-v4-pro"); err != nil {
		t.Fatalf("SwitchProviderForChat(deepseek/deepseek-v4-pro): %v", err)
	}
	provider, modelName := a.chatProviderInfo(chatID)
	if provider != "deepseek" || modelName != "deepseek-v4-pro" {
		t.Errorf("route = %s/%s, want deepseek/deepseek-v4-pro", provider, modelName)
	}
	// The override file carries the model, so a restart keeps it.
	data, err := os.ReadFile(filepath.Join(a.Config.DataDir, "chats", "7", "provider.json"))
	if err != nil {
		t.Fatalf("read override file: %v", err)
	}
	if !strings.Contains(string(data), "deepseek-v4-pro") {
		t.Errorf("override file = %s, want it to carry deepseek-v4-pro", string(data))
	}
	// The next turn actually runs the new model: the request
	// carries it.
	if _, _, err := a.HandleStreamForChat(context.Background(), chatID, "hello", nil); err != nil {
		t.Fatalf("turn on the switched model: %v", err)
	}
	models := deepseekSrv.requestModels()
	if len(models) == 0 || models[len(models)-1] != "deepseek-v4-pro" {
		t.Errorf("deepseek request models = %v, want the last to be deepseek-v4-pro", models)
	}
}

func TestChatAliasSwitch(t *testing.T) {
	a, _, openaiSrv := fallbackTestApp(t)
	defer a.Close()
	a.Config.ProviderAliases = map[string]string{"fast": "openai/gpt-4o-mini"}
	const chatID = 7
	if err := a.SwitchProviderForChat(chatID, "fast"); err != nil {
		t.Fatalf("SwitchProviderForChat(fast): %v", err)
	}
	provider, modelName := a.chatProviderInfo(chatID)
	if provider != "openai" || modelName != "gpt-4o-mini" {
		t.Errorf("route = %s/%s, want openai/gpt-4o-mini (alias fast)", provider, modelName)
	}
	if _, _, err := a.HandleStreamForChat(context.Background(), chatID, "hello", nil); err != nil {
		t.Fatalf("turn on the alias route: %v", err)
	}
	if got := len(openaiSrv.requests()); got != 1 {
		t.Errorf("openai requests = %d, want 1 (the alias resolved to openai)", got)
	}
	if err := a.SwitchProviderForChat(chatID, "no-such-alias"); err == nil {
		t.Error("SwitchProviderForChat(unknown alias): expected an error, got nil")
	}
}

func TestChatProviderReset(t *testing.T) {
	a, _, _ := fallbackTestApp(t)
	defer a.Close()
	const chatID = 7
	if err := a.SwitchProviderForChat(chatID, "openai"); err != nil {
		t.Fatalf("switch to openai: %v", err)
	}
	if err := a.ClearProviderOverrideForChat(chatID); err != nil {
		t.Fatalf("ClearProviderOverrideForChat: %v", err)
	}
	provider, modelName := a.chatProviderInfo(chatID)
	if provider != "deepseek" || modelName != "deepseek-flash" {
		t.Errorf("route after reset = %s/%s, want deepseek/deepseek-flash", provider, modelName)
	}
	if _, err := os.Stat(filepath.Join(a.Config.DataDir, "chats", "7", "provider.json")); !os.IsNotExist(err) {
		t.Errorf("override file still exists after reset (err = %v)", err)
	}
}

func TestExpensiveSwitchWarning(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	cases := []struct {
		fromProvider, fromModel, toProvider, toModel string
		wantWarning                                  bool
	}{
		// 16x dearer and over $1/M input: warn.
		{"deepseek", "deepseek-flash", "openai", "gpt-4o", true},
		// 20x dearer: warn.
		{"deepseek", "deepseek-flash", "anthropic", "claude-opus-4", true},
		// Same price tier: no warning.
		{"deepseek", "deepseek-flash", "openai", "gpt-4o-mini", false},
		// Cheaper: no warning.
		{"anthropic", "claude-opus-4", "deepseek", "deepseek-flash", false},
		// Unknown price on either side: never fires (Hermes rule
		// — a warning must not invent a price).
		{"deepseek", "no-such-model", "anthropic", "claude-opus-4", false},
		{"deepseek", "deepseek-flash", "anthropic", "no-such-model", false},
		// Free local to a cheap paid model: no warning (under $1/M).
		{"ollama", "llama3.1", "deepseek", "deepseek-flash", false},
		// Free local to a pricey model: warn.
		{"ollama", "llama3.1", "anthropic", "claude-opus-4", true},
		// 2.3x dearer: under the 3x bar, no warning.
		{"openai", "o4-mini", "openai", "gpt-4o", false},
	}
	for _, tc := range cases {
		warn, warning := a.ExpensiveSwitchWarning(tc.fromProvider, tc.fromModel, tc.toProvider+"/"+tc.toModel)
		if tc.wantWarning && !warn {
			t.Errorf("ExpensiveSwitchWarning(%s/%s -> %s/%s): no warning, want one", tc.fromProvider, tc.fromModel, tc.toProvider, tc.toModel)
		}
		if !tc.wantWarning && warn {
			t.Errorf("ExpensiveSwitchWarning(%s/%s -> %s/%s) = %q, want no warning", tc.fromProvider, tc.fromModel, tc.toProvider, tc.toModel, warning)
		}
		if tc.wantWarning && !(strings.Contains(warning, "$") && strings.Contains(warning, "--confirm")) {
			t.Errorf("warning %q must carry the $ figures and name --confirm", warning)
		}
	}
}

func TestExpensiveSwitchWarningFreeModelRendersAbsolutePrice(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	// Free local model -> pricey paid model: the warning must name
	// the target's absolute price, never a "+Inf" multiplier (x/0).
	warn, warning := a.ExpensiveSwitchWarning("ollama", "llama3.1", "anthropic/claude-opus-4")
	if !warn {
		t.Fatalf("ExpensiveSwitchWarning(ollama/llama3.1 -> anthropic/claude-opus-4): no warning, want one")
	}
	if strings.Contains(warning, "Inf") || strings.Contains(warning, "NaN") {
		t.Errorf("warning %q must not contain Inf/NaN", warning)
	}
	if !strings.Contains(warning, "$15.00/M input") {
		t.Errorf("warning %q must render the target's absolute price ($15.00/M input)", warning)
	}
	if !strings.Contains(warning, "--confirm") {
		t.Errorf("warning %q must name --confirm", warning)
	}
	// Nonzero current price keeps the multiplier form exactly.
	warn, warning = a.ExpensiveSwitchWarning("deepseek", "deepseek-flash", "openai/gpt-4o")
	if !warn {
		t.Fatalf("ExpensiveSwitchWarning(deepseek/deepseek-flash -> openai/gpt-4o): no warning, want one")
	}
	if !strings.Contains(warning, "x more per input token") {
		t.Errorf("warning %q must keep the multiplier form for nonzero current prices", warning)
	}
	if strings.Contains(warning, "Inf") || strings.Contains(warning, "NaN") {
		t.Errorf("warning %q must not contain Inf/NaN", warning)
	}
}
