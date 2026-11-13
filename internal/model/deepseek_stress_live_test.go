//go:build live

package model

// Live DeepSeek stress checks (Orion Audit 3, Workstream 3): a
// handful of real calls against api.deepseek.com, to confirm the
// fake-server behaviour matches the real API — timeouts, streaming,
// tool-call shape — and to report real latencies.
//
// Run: go test -tags live -run TestLiveDeepSeekStress -count=1 -v ./internal/model/
// The key is read from ~/.orion/config.json (the production config
// path), exactly as deepseek_live_test.go does, and is never printed;
// only replies, latencies and the provider name are logged. Three
// calls total, MaxTokens kept small to bound API usage. Skipped when
// DeepSeek is not the configured default or has no key.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// liveDeepSeekProvider loads the DeepSeek provider from the
// production config, or skips the test when DeepSeek is not
// configured.
func liveDeepSeekProvider(t *testing.T) Provider {
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
		t.Skipf("default_provider = %q, want deepseek (live DeepSeek stress not applicable)", cfg.DefaultProvider)
	}
	ds, ok := cfg.Providers["deepseek"]
	if !ok || strings.TrimSpace(ds.APIKey) == "" {
		t.Skip("no deepseek api_key in config (live DeepSeek stress not applicable)")
	}
	p, err := NewProvider(ProviderConfig{Name: "deepseek", APIKey: ds.APIKey, BaseURL: ds.BaseURL, Model: ds.Model})
	if err != nil {
		t.Fatalf("NewProvider(deepseek): %v", err)
	}
	return p
}

// TestLiveDeepSeekStressNormal: one ordinary live call — the
// fake-server baseline (17*23+5 = 396) against the real API, with
// latency.
func TestLiveDeepSeekStressNormal(t *testing.T) {
	p := liveDeepSeekProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	start := time.Now()
	resp, err := p.Chat(ctx, ChatRequest{
		Messages: []Message{
			{Role: "system", Content: "You are a calculator. Reply with just the number, nothing else."},
			{Role: "user", Content: "What is 17*23+5?"},
		},
		MaxTokens: 50,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("deepseek Chat: %v", err)
	}
	t.Logf("live deepseek Chat: %v, reply %q, usage %+v", elapsed, resp.Message.Content, resp.Usage)
	if !strings.Contains(resp.Message.Content, "396") {
		t.Fatalf("reply %q does not contain 396", resp.Message.Content)
	}
}

// TestLiveDeepSeekStressToolCall: one live call that must come back
// as a tool call — confirms the real API's tool-call wire shape
// (id, function name, arguments as a JSON string) matches what the
// fake servers stage, and that the arguments parse as JSON.
func TestLiveDeepSeekStressToolCall(t *testing.T) {
	p := liveDeepSeekProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	start := time.Now()
	resp, err := p.Chat(ctx, ChatRequest{
		Messages: []Message{
			{Role: "system", Content: "You have a calculate tool. Use it for every arithmetic question. Reply ONLY by calling the tool, with no text."},
			{Role: "user", Content: "Use the calculate tool to compute 17*23+5."},
		},
		Tools: []ToolDefinition{{
			Name:        "calculate",
			Description: "Evaluate an arithmetic expression and return the result.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"expression": map[string]any{"type": "string", "description": "The arithmetic expression to evaluate, e.g. 17*23+5."},
				},
				"required": []string{"expression"},
			},
		}},
		MaxTokens: 100,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("deepseek Chat: %v", err)
	}
	t.Logf("live deepseek tool-call Chat: %v, finish %q, reply %+v", elapsed, resp.FinishReason, resp.Message)
	if resp.FinishReason != "tool_calls" || len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("finish = %q, tool calls = %+v: expected exactly one tool call (model answered directly instead)", resp.FinishReason, resp.Message.ToolCalls)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.Name != "calculate" {
		t.Fatalf("tool call name = %q, want calculate", tc.Name)
	}
	if tc.ID == "" {
		t.Fatal("tool call has an empty id")
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
		t.Fatalf("tool call arguments %q are not valid JSON: %v", tc.Arguments, err)
	}
	expr, _ := args["expression"].(string)
	if !strings.Contains(strings.ReplaceAll(expr, " ", ""), "17*23+5") {
		t.Fatalf("tool call expression = %q, want 17*23+5", expr)
	}
	t.Logf("live tool call: id=%s name=%s arguments=%s", tc.ID, tc.Name, tc.Arguments)
}

// TestLiveDeepSeekStressStreamReasoning: one live streaming call
// with a multi-step reasoning prompt — confirms streaming works
// against the real API (time to first token, chunk flow, clean
// finish) and reports real latencies.
func TestLiveDeepSeekStressStreamReasoning(t *testing.T) {
	p := liveDeepSeekProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	var chunks []string
	var firstChunkAt time.Duration
	start := time.Now()
	resp, err := p.Stream(ctx, ChatRequest{
		Messages: []Message{
			{Role: "system", Content: "Show your working step by step, then give the final answer on its own last line."},
			{Role: "user", Content: "A train travels 60 km in the first hour, then 80 km/h for 2 more hours, then stops for 30 minutes, then travels 100 km in the last hour. How far did it travel in total, and what was its average speed excluding the stop?"},
		},
		MaxTokens: 400,
	}, func(chunk string) error {
		if len(chunks) == 0 {
			firstChunkAt = time.Since(start)
		}
		chunks = append(chunks, chunk)
		return nil
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("deepseek Stream: %v", err)
	}
	t.Logf("live deepseek Stream: total %v, time-to-first-chunk %v, %d chunks, finish %q, usage %+v", elapsed, firstChunkAt, len(chunks), resp.FinishReason, resp.Usage)
	t.Logf("live deepseek Stream reply: %q", resp.Message.Content)
	if resp.Message.Content == "" {
		t.Fatal("streamed reply is empty")
	}
	if !strings.Contains(resp.Message.Content, "320") {
		t.Fatalf("reply does not contain the total distance 320 km: %q", resp.Message.Content)
	}
	if !strings.Contains(resp.Message.Content, "80") {
		t.Fatalf("reply does not contain the average speed 80 km/h: %q", resp.Message.Content)
	}
	if len(chunks) == 0 {
		t.Fatal("no chunks were delivered to the callback")
	}
}
