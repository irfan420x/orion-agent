package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/security"
	"github.com/irfan/orion/internal/tools"
)

// testAppOnDir is testApp rooted at dir (so two Apps can share one
// data dir, to prove per-chat state survives a restart).
func testAppOnDir(t *testing.T, dir string) *App {
	t.Helper()
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
	// A fake key for openai (synthetic, unusable) so per-chat
	// provider switches to openai can be exercised without network.
	// NewProvider only stores the key; no request is ever made.
	openai := cfg.Providers["openai"]
	openai.APIKey = "test-fake-key-for-per-chat-switch"
	cfg.Providers["openai"] = openai
	agent, err := core.NewAgent(cfg, stubProvider{}, reg, mem)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	audit, err := security.NewAuditLog(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	return &App{Config: cfg, Provider: stubProvider{}, Tools: reg, Memory: mem, Agent: agent, Audit: audit}
}

func chatMessages(t *testing.T, a *App, chatID int64) []model.Message {
	t.Helper()
	cs, err := a.chatSessionFor(chatID)
	if err != nil {
		t.Fatalf("chatSessionFor(%d): %v", chatID, err)
	}
	msgs, err := cs.store.RecentMessages(0)
	if err != nil {
		t.Fatalf("chat %d RecentMessages: %v", chatID, err)
	}
	return msgs
}

func contents(msgs []model.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Role+":"+m.Content)
	}
	return out
}

func containsText(msgs []model.Message, sub string) bool {
	for _, m := range msgs {
		if strings.Contains(m.Content, sub) {
			return true
		}
	}
	return false
}

func TestPerChatHistoryIsolation(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	ctx := context.Background()

	if _, _, err := a.HandleStreamForChat(ctx, 111, "hello from chat A", nil); err != nil {
		t.Fatalf("HandleStreamForChat A: %v", err)
	}
	if _, _, err := a.HandleStreamForChat(ctx, 222, "hello from chat B", nil); err != nil {
		t.Fatalf("HandleStreamForChat B: %v", err)
	}

	// Each chat sees only its own history.
	msgsA := chatMessages(t, a, 111)
	if !containsText(msgsA, "hello from chat A") {
		t.Errorf("chat A history missing its own message: %v", contents(msgsA))
	}
	if containsText(msgsA, "hello from chat B") {
		t.Errorf("chat A history leaked chat B's message: %v", contents(msgsA))
	}
	msgsB := chatMessages(t, a, 222)
	if !containsText(msgsB, "hello from chat B") {
		t.Errorf("chat B history missing its own message: %v", contents(msgsB))
	}
	if containsText(msgsB, "hello from chat A") {
		t.Errorf("chat B history leaked chat A's message: %v", contents(msgsB))
	}

	// The two chats run on different agents.
	agA, err := a.AgentForChat(111)
	if err != nil {
		t.Fatalf("AgentForChat A: %v", err)
	}
	agB, err := a.AgentForChat(222)
	if err != nil {
		t.Fatalf("AgentForChat B: %v", err)
	}
	if agA == agB {
		t.Error("both chats share one agent")
	}

	// Telegram turns never touch the global (CLI) store.
	global, err := a.Memory.RecentMessages(0)
	if err != nil {
		t.Fatalf("global RecentMessages: %v", err)
	}
	if len(global) != 0 {
		t.Errorf("global history has %d message(s) after Telegram-only turns, want 0", len(global))
	}

	// Per-chat history survives a restart: a fresh App over the
	// same data dir reloads each chat's file.
	dir := a.Config.DataDir
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	a2 := testAppOnDir(t, dir)
	defer a2.Close()
	if msgs := chatMessages(t, a2, 111); !containsText(msgs, "hello from chat A") {
		t.Errorf("after restart, chat A history lost its message: %v", contents(msgs))
	}
	if msgs := chatMessages(t, a2, 222); !containsText(msgs, "hello from chat B") {
		t.Errorf("after restart, chat B history lost its message: %v", contents(msgs))
	}
}

func TestNewSessionForChatOnlyWipesCaller(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	ctx := context.Background()
	if _, _, err := a.HandleStreamForChat(ctx, 111, "secret A", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.HandleStreamForChat(ctx, 222, "secret B", nil); err != nil {
		t.Fatal(err)
	}
	// A shared fact survives /new (long-term memory is not wiped).
	csA, _ := a.chatSessionFor(111)
	if err := csA.store.AddFact("favourite-number", "17"); err != nil {
		t.Fatalf("AddFact: %v", err)
	}

	if err := a.NewSessionForChat(111); err != nil {
		t.Fatalf("NewSessionForChat: %v", err)
	}
	if msgs := chatMessages(t, a, 111); len(msgs) != 0 {
		t.Errorf("chat A history after /new = %d message(s), want 0", len(msgs))
	}
	if msgs := chatMessages(t, a, 222); !containsText(msgs, "secret B") {
		t.Errorf("chat B history wiped by chat A's /new: %v", contents(msgs))
	}
	if _, found, err := csA.store.GetFact("favourite-number"); err != nil || !found {
		t.Errorf("shared fact lost by /new (found=%v, err=%v)", found, err)
	}
	// The other chat sees the shared fact too (knowledge base is
	// shared; only conversation history is per chat).
	csB, _ := a.chatSessionFor(222)
	if _, found, err := csB.store.GetFact("favourite-number"); err != nil || !found {
		t.Errorf("chat B cannot see the shared fact (found=%v, err=%v)", found, err)
	}
}

func TestUndoLastTurnsForChatOnlyUndoesCaller(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	ctx := context.Background()
	if _, _, err := a.HandleStreamForChat(ctx, 111, "undo me A", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.HandleStreamForChat(ctx, 222, "keep me B", nil); err != nil {
		t.Fatal(err)
	}
	undone, err := a.UndoLastTurnsForChat(111, 1)
	if err != nil {
		t.Fatalf("UndoLastTurnsForChat: %v", err)
	}
	if len(undone) != 1 || undone[0] != "undo me A" {
		t.Errorf("undone = %v, want [undo me A]", undone)
	}
	if msgs := chatMessages(t, a, 111); containsText(msgs, "undo me A") {
		t.Errorf("chat A still has the undone turn: %v", contents(msgs))
	}
	if msgs := chatMessages(t, a, 222); !containsText(msgs, "keep me B") {
		t.Errorf("chat B lost its turn to chat A's /undo: %v", contents(msgs))
	}
}

func TestPerChatModelSwitch(t *testing.T) {
	dir := t.TempDir()
	a := testAppOnDir(t, dir)
	defer a.Close()

	// Both chats start on the daemon default (ollama).
	if got := a.ChatProviderName(111); got != "ollama" {
		t.Errorf("chat A provider = %q, want ollama (default)", got)
	}
	if got := a.ChatProviderName(222); got != "ollama" {
		t.Errorf("chat B provider = %q, want ollama (default)", got)
	}

	// Switching chat A to openai touches only chat A...
	if err := a.SwitchProviderForChat(111, "openai"); err != nil {
		t.Fatalf("SwitchProviderForChat A: %v", err)
	}
	if got := a.ChatProviderName(111); got != "openai" {
		t.Errorf("chat A provider = %q, want openai", got)
	}
	provider, modelName, _ := a.ModelInfoForChat(111)
	if provider != "openai" || modelName != "gpt-4o-mini" {
		t.Errorf("chat A model info = %q/%q, want openai/gpt-4o-mini", provider, modelName)
	}
	if got := a.ChatProviderName(222); got != "ollama" {
		t.Errorf("chat B provider = %q after A's switch, want ollama (unaffected)", got)
	}
	// ...and the daemon default is untouched.
	if got := a.ProviderName(); got != "stub" {
		t.Errorf("global provider = %q, want stub (per-chat switch must not move the default)", got)
	}
	if a.Config.DefaultProvider != "ollama" {
		t.Errorf("config DefaultProvider = %q, want ollama (per-chat switch must not persist to config)", a.Config.DefaultProvider)
	}

	// The override survives a restart (provider.json in the chat dir).
	if _, err := os.Stat(filepath.Join(dir, "chats", "111", "provider.json")); err != nil {
		t.Errorf("chat A provider override file missing: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	a2 := testAppOnDir(t, dir)
	defer a2.Close()
	if got := a2.ChatProviderName(111); got != "openai" {
		t.Errorf("after restart, chat A provider = %q, want openai", got)
	}
	if got := a2.ChatProviderName(222); got != "ollama" {
		t.Errorf("after restart, chat B provider = %q, want ollama", got)
	}

	// Unknown providers and key-less providers are rejected.
	if err := a2.SwitchProviderForChat(111, "bogus"); err == nil {
		t.Error("switch to unknown provider: expected error")
	}
	if err := a2.SwitchProviderForChat(111, "deepseek"); err == nil {
		t.Error("switch to deepseek without a key: expected error")
	}
	// ...and a failed switch leaves the previous provider in place.
	if got := a2.ChatProviderName(111); got != "openai" {
		t.Errorf("chat A provider after failed switches = %q, want openai", got)
	}
}

func TestGlobalSwitchRebuildsDefaultChatsOnly(t *testing.T) {
	dir := t.TempDir()
	a := testAppOnDir(t, dir)
	defer a.Close()
	// Chat A overrides to openai; chat B follows the default.
	if err := a.SwitchProviderForChat(111, "openai"); err != nil {
		t.Fatal(err)
	}
	agA1, _ := a.AgentForChat(111)
	agB1, _ := a.AgentForChat(222)
	// Global switch to openai (daemon default moves).
	if err := a.SwitchProvider("openai"); err != nil {
		t.Fatalf("SwitchProvider: %v", err)
	}
	agA2, _ := a.AgentForChat(111)
	agB2, _ := a.AgentForChat(222)
	if agA1 != agA2 {
		t.Error("chat A (override) agent was rebuilt by a global switch; overrides must survive it")
	}
	if agB1 == agB2 {
		t.Error("chat B (no override) agent was NOT rebuilt by a global switch; it must follow the new default")
	}
	if got := a.ChatProviderName(222); got != "openai" {
		t.Errorf("chat B provider after global switch = %q, want openai", got)
	}
}

func TestCompressForChatOnlyCompressesCaller(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	ctx := context.Background()
	// The script provider answers every Chat call — turns and
	// the summarisation — with a summary that preserves the
	// seeded turns (project plan / build), so the /compress
	// preservation check (F-A7-P2-4) passes. The stub
	// provider's "ok" shares nothing with any history and is
	// refused. Chat A's turns discuss the project plan and
	// build for the same reason.
	a.Provider = &compressScriptProvider{usage: model.Usage{}}
	// 12 user turns in chat A (>10 recent window), 1 in chat B.
	for i := 0; i < 12; i++ {
		if _, _, err := a.HandleStreamForChat(ctx, 111, "turn A about the project plan and build", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := a.HandleStreamForChat(ctx, 222, "turn B", nil); err != nil {
		t.Fatal(err)
	}
	before, after, _, err := a.CompressForChat(ctx, 111, 10)
	if err != nil {
		t.Fatalf("CompressForChat: %v", err)
	}
	if before <= after {
		t.Errorf("compress A: before=%d after=%d, want before > after", before, after)
	}
	if msgs := chatMessages(t, a, 222); len(msgs) == 0 || !containsText(msgs, "turn B") {
		t.Errorf("chat B history changed by chat A's /compress: %v", contents(msgs))
	}
}

func TestStatusAndMemorySummaryArePerChat(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	ctx := context.Background()
	if _, _, err := a.HandleStreamForChat(ctx, 111, "hello A", nil); err != nil {
		t.Fatal(err)
	}
	line := a.StatusLineForChat(111)
	if !strings.Contains(line, "Messages: 2") {
		t.Errorf("chat A status line = %q, want it to count chat A's 2 messages", line)
	}
	lineB := a.StatusLineForChat(222)
	if !strings.Contains(lineB, "Messages: 0") {
		t.Errorf("chat B status line = %q, want 0 messages", lineB)
	}
	summary, err := a.MemorySummaryForChat(111)
	if err != nil {
		t.Fatalf("MemorySummaryForChat: %v", err)
	}
	if !strings.Contains(summary, "2 message(s)") {
		t.Errorf("chat A memory summary = %q, want 2 message(s)", summary)
	}
}

func TestChatExportOnlyExportsCaller(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	ctx := context.Background()
	if _, _, err := a.HandleStreamForChat(ctx, 111, "export me A", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.HandleStreamForChat(ctx, 222, "not for A B", nil); err != nil {
		t.Fatal(err)
	}
	path, count, err := a.ExportConversation(111, "chat A", 0, 0)
	if err != nil {
		t.Fatalf("ExportConversation: %v", err)
	}
	if count != 2 {
		t.Errorf("export count = %d, want 2 (chat A's messages only)", count)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "export me A") {
		t.Error("export missing chat A's message")
	}
	if strings.Contains(string(data), "not for A B") {
		t.Error("export leaked chat B's message")
	}
}
