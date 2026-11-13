//go:build live

// Audit 3, Workstream 1 — live comparison against DeepSeek.
//
// Runs a SMALL number of the same conversation shapes as the fake
// harness (TestAudit3ReliabilityHarness) against the real default
// provider (DeepSeek, deepseek-chat) using the key from
// ~/.orion/config.json — the same loading pattern as
// internal/model/deepseek_live_test.go. The key is never printed.
//
// Run: go test -tags live -run TestAudit3LiveDeepSeek -count=1 ./internal/core/
// (a handful of provider calls only — live API usage is deliberate
// and small). The fake harness is the volume proof; this test checks
// the loop behaves the same way with a real model: one tool call per
// turn, deferred-call reminders when the model over-plans, clean
// history across turns, and no empty replies.
package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/tools"
)

// audit3CountingProvider wraps a real provider and records what the
// loop asked of it, for the fake-vs-live comparison.
type audit3CountingProvider struct {
	inner model.Provider
	mu    sync.Mutex

	chatCalls    int
	streamCalls  int
	toolCalls    int
	multiCallReq int // requests where the model issued 2+ tool calls
	reminderSeen int // requests ending in the deferred-call reminder
	maxReqTokens int
	invalid      []string
}

func (p *audit3CountingProvider) Name() string { return p.inner.Name() }

func (p *audit3CountingProvider) Chat(ctx context.Context, req model.ChatRequest) (*model.ChatResponse, error) {
	p.observe(req)
	p.mu.Lock()
	p.chatCalls++
	p.mu.Unlock()
	resp, err := p.inner.Chat(ctx, req)
	if err == nil && resp != nil {
		p.mu.Lock()
		p.toolCalls += len(resp.Message.ToolCalls)
		p.mu.Unlock()
	}
	return resp, err
}

func (p *audit3CountingProvider) Stream(ctx context.Context, req model.ChatRequest, fn func(chunk string) error) (*model.ChatResponse, error) {
	p.observe(req)
	p.mu.Lock()
	p.streamCalls++
	p.mu.Unlock()
	resp, err := p.inner.Stream(ctx, req, fn)
	if err == nil && resp != nil {
		p.mu.Lock()
		p.toolCalls += len(resp.Message.ToolCalls)
		p.mu.Unlock()
	}
	return resp, err
}

func (p *audit3CountingProvider) observe(req model.ChatRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := 0
	for _, m := range req.Messages {
		total += messageTokens(m)
	}
	if total > p.maxReqTokens {
		p.maxReqTokens = total
	}
	for i, m := range req.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) > 1 {
			p.multiCallReq++
		}
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				found := false
				for j := i + 1; j < len(req.Messages) && req.Messages[j].Role == "tool"; j++ {
					if req.Messages[j].ToolCallID == tc.ID {
						found = true
					}
				}
				if !found {
					p.invalid = append(p.invalid, "dangling tool call "+tc.Name)
				}
			}
		}
	}
	if len(req.Messages) > 0 {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "user" && strings.HasPrefix(last.Content, "You still have pending tool calls") {
			p.reminderSeen++
		}
	}
}

func (p *audit3CountingProvider) stats() (chats, streams, toolCalls, multi, reminders, maxTokens int, invalid []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.chatCalls, p.streamCalls, p.toolCalls, p.multiCallReq, p.reminderSeen, p.maxReqTokens, append([]string(nil), p.invalid...)
}

// audit3LoadDeepSeek builds the DeepSeek provider from ~/.orion/config.json.
// It skips (never fails) when DeepSeek is not the configured default
// or has no key — the same pattern as TestLiveDeepSeek.
func audit3LoadDeepSeek(t *testing.T) model.Provider {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".orion", "config.json"))
	if err != nil {
		t.Skipf("no orion config: %v", err)
	}
	var cfg struct {
		DefaultProvider string `json:"default_provider"`
		Providers       map[string]struct {
			Name    string `json:"name"`
			APIKey  string `json:"api_key"`
			BaseURL string `json:"base_url"`
			Model   string `json:"model"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if cfg.DefaultProvider != "deepseek" {
		t.Skipf("default_provider = %q, want deepseek (live Audit 3 test not applicable)", cfg.DefaultProvider)
	}
	ds, ok := cfg.Providers["deepseek"]
	if !ok || strings.TrimSpace(ds.APIKey) == "" {
		t.Skip("no deepseek api_key in config (live Audit 3 test not applicable)")
	}
	p, err := model.NewProvider(model.ProviderConfig{Name: "deepseek", APIKey: ds.APIKey, BaseURL: ds.BaseURL, Model: ds.Model})
	if err != nil {
		t.Fatalf("NewProvider(deepseek): %v", err)
	}
	return p
}

func TestAudit3LiveDeepSeek(t *testing.T) {
	inner := audit3LoadDeepSeek(t)
	prov := &audit3CountingProvider{inner: inner}
	reg, err := tools.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	mem := newFakeStore()
	cfg := config.Default()
	cfg.MaxSteps = 10
	cfg.TimeoutSeconds = 120
	agent := newTestAgent(t, cfg, prov, reg, mem)

	dir := t.TempDir()
	resultPath := filepath.Join(dir, "live-answer.txt")

	// Conversation 1, turn 1: the calc -> write -> read chain, with
	// firm tool-use instructions (a real model may otherwise answer
	// from its own arithmetic and skip the tools).
	run := func(input string) (string, time.Duration) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
		defer cancel()
		t0 := time.Now()
		out, err := agent.Run(ctx, input)
		dur := time.Since(t0)
		if err != nil {
			t.Fatalf("live turn %q failed after %v: %v", input, dur, err)
		}
		if out == "" {
			t.Fatalf("live turn %q returned an empty reply", input)
		}
		return out, dur
	}

	out1, d1 := run("Use your calculator tool to compute 17*23+5. Then use the file_write tool to write ONLY the resulting number to " + resultPath + ". Then use the file_read tool to read the file back, and tell me the number it contains.")
	t.Logf("AUDIT3 LIVE turn 1 (%v): %q", d1, out1)
	if !strings.Contains(out1, "396") {
		t.Errorf("turn 1 reply %q does not contain 396 — model did not complete the chain as instructed", out1)
	}
	if b, err := os.ReadFile(resultPath); err != nil {
		t.Errorf("turn 1 did not write %s: %v (model skipped file_write?)", resultPath, err)
	} else if strings.TrimSpace(string(b)) != "396" {
		t.Errorf("file contains %q, want 396", string(b))
	}

	// Conversation 1, turn 2: multi-turn history — the answer must
	// come from the earlier turn, not a fresh calculation.
	out2, d2 := run("What number did you just write to the file?")
	t.Logf("AUDIT3 LIVE turn 2 (%v): %q", d2, out2)
	if !strings.Contains(out2, "396") {
		t.Errorf("turn 2 reply %q does not contain 396 — multi-turn history did not survive", out2)
	}

	// Conversation 1, turn 3: shell_exec, in the same conversation.
	out3, d3 := run("Run `echo hello-orion-live` in the shell and tell me exactly what it printed.")
	t.Logf("AUDIT3 LIVE turn 3 (%v): %q", d3, out3)
	if !strings.Contains(out3, "hello-orion-live") {
		t.Errorf("turn 3 reply %q does not contain hello-orion-live", out3)
	}

	// History must be clean after the live conversation, exactly as
	// the fake harness requires.
	if problem := audit3HistoryClean(mem.allMessages()); problem != "" {
		t.Fatalf("live history corrupted: %s", problem)
	}

	chats, _, toolCalls, multi, reminders, maxTokens, invalid := prov.stats()
	t.Logf("AUDIT3 LIVE: provider calls=%d (all Chat), tool calls issued=%d, multi-call requests=%d, deferred reminders=%d, largest request=%d tokens, invalid requests=%v",
		chats, toolCalls, multi, reminders, maxTokens, invalid)
	if len(invalid) > 0 {
		t.Fatalf("loop sent invalid request(s) to DeepSeek: %v", invalid)
	}
	if toolCalls == 0 {
		t.Errorf("DeepSeek issued zero tool calls across 3 turns — it answered from its own knowledge instead of using tools; loop behaviour untested live")
	}
}
