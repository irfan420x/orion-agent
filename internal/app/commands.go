// Command support for the Telegram slash-command framework
// (internal/messaging/commands.go). Everything here is real app-side
// behaviour — memory manipulation, provider switching, run tracking —
// so the messaging layer stays a thin registry over these methods and
// never fakes a command.
//
// Concurrency: the agent/provider pair is swapped by SwitchProvider
// while Handle/HandleStream may be running. agent() and provider()
// take a read lock and return the current pair; SwitchProvider takes
// the write lock. A run in flight keeps the agent it started with;
// the next run uses the new one. Run registration (RegisterRun /
// StopRun / Drain) is guarded by its own mutex and a WaitGroup, so
// /stop and /restart see exactly the turns that are live.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/security"
)

// agent returns the current agent under a read lock. Handle and
// HandleStream must go through this (never a.Agent directly) so a
// concurrent SwitchProvider cannot race the read.
func (a *App) agent() *core.Agent {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Agent
}

// provider returns the current provider under a read lock.
func (a *App) provider() model.Provider {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Provider
}

// activeModel returns the effective model name for the active
// provider, or "" when the config cannot say (the caller then falls
// back to the provider name).
func (a *App) activeModel() string {
	if a == nil || a.Config == nil {
		return ""
	}
	if active, err := a.Config.ActiveProvider(); err == nil {
		return active.Model
	}
	return ""
}

// HandleStream runs one user input through the agent like Handle, but
// with a progress-event handler: every event is forwarded to emit
// (the Telegram terminal card feed), and the TurnFinished event is
// captured into the returned TurnStats (provider, model, total tokens,
// steps, wall duration). emit may be nil (no card, stats still
// captured). The previous event handler is saved and restored around
// the run, so a per-turn handler never leaks into the next turn. The
// turn is audited exactly like Handle.
func (a *App) HandleStream(ctx context.Context, input string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
	agent := a.agent()
	if a == nil || agent == nil {
		return "", nil, errors.New("app: HandleStream called on uninitialised App (nil Agent)")
	}
	prev := agent.EventHandler()
	providerName := ""
	if p := a.provider(); p != nil {
		providerName = p.Name()
	}
	modelName := a.activeModel()
	var stats *core.TurnStats
	wrapped := func(ev core.ProgressEvent) {
		if ev.Kind == core.EventTurnFinished {
			stats = &core.TurnStats{
				Provider: providerName,
				Model:    modelName,
				Tokens:   ev.Usage.TotalTokens,
				Steps:    ev.Step,
				Duration: ev.Duration,
				// MemoriesUsed comes from the TurnFinished event
				// (the agent counts typed memories injected this
				// turn); the footer renders it when non-zero.
				MemoriesUsed: ev.MemoriesUsed,
			}
		}
		if emit != nil {
			emit(ev)
		}
	}
	agent.SetEventHandler(wrapped)
	defer agent.SetEventHandler(prev)
	reply, err := agent.Run(ctx, input)
	if a.Audit != nil {
		_, _ = a.Audit.Append("user", "agent_run", truncateRunes(security.RedactSecrets(input), 200))
	}
	return reply, stats, err
}

// providerNeedsAPIKey mirrors config's rule (openai/deepseek/anthropic
// need a key, ollama does not). It lives here because config's
// providerNeedsAPIKey is unexported and SwitchProvider must name the
// missing key in its error before it builds anything.
func providerNeedsAPIKey(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "openai", "deepseek", "anthropic":
		return true
	default:
		return false
	}
}

// SwitchProvider switches the daemon's default provider, persists the
// choice to the config file, and rebuilds the agent for subsequent
// turns. Mechanism: the agent holds its provider by value, so there
// is no in-place swap — a new agent is built with the same tools,
// memory and config (memory is shared, so conversation history and
// facts survive the switch), the App's Agent/Provider pointers are
// swapped under the write lock, and the subagent executor is
// re-pointed at a child-agent factory built on the new provider.
// A run already in flight keeps the old agent and finishes on it.
// The old agent is deliberately NOT shut down: Agent.Shutdown would
// Save+Close the shared memory store.
//
// Errors: unknown provider (lists the valid names), or a key-requiring
// provider with no API key configured (names the missing key). No
// broken agent is ever installed: validation and provider
// construction happen before any state changes.
func (a *App) SwitchProvider(name string) error {
	if a == nil || a.Config == nil {
		return errors.New("app: SwitchProvider called on uninitialised App")
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return fmt.Errorf("app: unknown provider %q (supported providers: %s)", name, strings.Join(config.KnownProviderNames(), ", "))
	}
	if !config.KnownProvider(name) {
		return fmt.Errorf("app: unknown provider %q (supported providers: %s)", name, strings.Join(config.KnownProviderNames(), ", "))
	}
	// Resolve the entry case-insensitively: the map key is the
	// canonical lowercase name, but never assume it.
	var entry config.ProviderConfig
	var key string
	found := false
	for k, p := range a.Config.Providers {
		if strings.EqualFold(k, name) {
			entry, key, found = p, k, true
			break
		}
	}
	if !found {
		return fmt.Errorf("app: provider %q is not configured (supported providers: %s)", name, strings.Join(config.KnownProviderNames(), ", "))
	}
	if strings.TrimSpace(entry.Name) == "" {
		entry.Name = key
	}
	if strings.TrimSpace(entry.Model) == "" {
		entry.Model = a.Config.Model
	}
	if providerNeedsAPIKey(name) && strings.TrimSpace(entry.APIKey) == "" {
		return fmt.Errorf("app: provider %q requires an API key (set providers.%s.api_key or ORION_API_KEY)", name, name)
	}
	provider, err := model.NewProvider(model.ProviderConfig{
		Name:    entry.Name,
		APIKey:  entry.APIKey,
		BaseURL: entry.BaseURL,
		Model:   entry.Model,
	})
	if err != nil {
		return fmt.Errorf("app: provider %q: %w", name, err)
	}
	// Persist first, then swap: if the save fails the daemon keeps
	// running on the old provider instead of diverging from disk.
	a.mu.Lock()
	oldDefault := a.Config.DefaultProvider
	a.Config.DefaultProvider = name
	if err := a.Config.Save(""); err != nil {
		a.Config.DefaultProvider = oldDefault
		a.mu.Unlock()
		return fmt.Errorf("app: switch provider: save config: %w", err)
	}
	agent, err := core.NewAgent(a.Config, provider, a.Tools, a.Memory)
	if err != nil {
		// The config on disk already names the new provider, but
		// the in-memory app cannot run it; leave DefaultProvider
		// switched (a restart will pick it up) and report the
		// build failure honestly.
		a.mu.Unlock()
		return fmt.Errorf("app: switch provider: build agent: %w", err)
	}
	if a.Logger != nil {
		agent.SetLogger(a.Logger)
	}
	a.Agent = agent
	a.Provider = provider
	if a.Subagents != nil {
		// Re-point the subagent executor at a child-agent factory
		// built on the NEW provider (children share the active
		// provider). The factory builds a fresh child per spawn,
		// so it never captures the old agent.
		// The subagent tools' usage sink resolves the CURRENT
		// agent on every call (see Build), so no re-pointing is
		// needed here: tokens from consumed results accrue to
		// this new agent automatically.
		a.Subagents.SetExecutor(newSubagentExecutor(provider, a.Config, a.typedAdapter, a.Logger))
	}
	a.mu.Unlock()
	return nil
}

// StatusLine returns a multi-line status with real values only:
// provider, model, step/timeout limits, memory counts, data dir, and
// uptime when the app knows when it started (Build sets StartedAt; a
// hand-built App has the zero time and the line is omitted, never
// faked). It never returns an error: a missing piece is shown as
// "unknown" rather than failing the /status command.
func (a *App) StatusLine() string {
	if a == nil {
		return "orion: not initialised"
	}
	providerName := "unknown"
	if p := a.provider(); p != nil {
		providerName = p.Name()
	}
	modelName := a.activeModel()
	if modelName == "" {
		modelName = "unknown"
	}
	maxSteps, timeout := 0, 0
	if a.Config != nil {
		maxSteps, timeout = a.Config.MaxSteps, a.Config.TimeoutSeconds
	}
	msgs, facts := 0, 0
	if a.Memory != nil {
		if all, err := a.Memory.RecentMessages(0); err == nil {
			msgs = len(all)
		}
		if list, err := a.Memory.ListFacts(); err == nil {
			facts = len(list)
		}
	}
	dataDir := "unknown"
	if a.Config != nil && a.Config.DataDir != "" {
		dataDir = a.Config.DataDir
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Provider: %s\nModel: %s\nMax steps: %d\nTimeout: %ds\nMessages: %d\nFacts: %d\nData dir: %s",
		providerName, modelName, maxSteps, timeout, msgs, facts, dataDir)
	if !a.StartedAt.IsZero() {
		fmt.Fprintf(&b, "\nUptime: %s", time.Since(a.StartedAt).Round(time.Second))
	}
	return b.String()
}

// runRegistration is one live turn: its context (compared by
// identity) and its cancel function.
type runRegistration struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// RegisterRun records a live agent turn for chatID with its context
// and cancel function, so /stop can cancel it and /restart can drain
// it. It reports whether this call registered the turn: a second
// registration for the same chat returns false and keeps the first —
// the outer turn (e.g. a /retry running inside a command turn) must
// not lose its cancel to an inner registration, and the inner run
// must not unregister the outer turn when it finishes.
func (a *App) RegisterRun(chatID int64, ctx context.Context, cancel context.CancelFunc) bool {
	if a == nil || cancel == nil || ctx == nil {
		return false
	}
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if a.runs == nil {
		a.runs = map[int64]runRegistration{}
	}
	if _, ok := a.runs[chatID]; ok {
		return false
	}
	a.runs[chatID] = runRegistration{ctx: ctx, cancel: cancel}
	a.runWG.Add(1)
	return true
}

// UnregisterRun removes a turn's registration, but only when ctx is
// the context that registered it — an inner run must not unregister
// the outer turn it nested inside. It reports whether anything was
// removed.
func (a *App) UnregisterRun(chatID int64, ctx context.Context) bool {
	if a == nil {
		return false
	}
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if a.runs == nil {
		return false
	}
	if cur, ok := a.runs[chatID]; ok && cur.ctx == ctx {
		delete(a.runs, chatID)
		a.runWG.Done()
		return true
	}
	return false
}

// StopRun cancels the live turn for chatID, if any. It reports
// whether anything was running, so /stop can answer honestly.
func (a *App) StopRun(chatID int64) bool {
	if a == nil {
		return false
	}
	a.runMu.Lock()
	reg, ok := a.runs[chatID]
	a.runMu.Unlock()
	if !ok || reg.cancel == nil {
		return false
	}
	reg.cancel()
	return true
}

// RunningChats returns the chat IDs with a live turn, sorted.
func (a *App) RunningChats() []int64 {
	if a == nil {
		return nil
	}
	a.runMu.Lock()
	defer a.runMu.Unlock()
	out := make([]int64, 0, len(a.runs))
	for id := range a.runs {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Drain waits for all live turns to finish, up to timeout. It
// reports true when the daemon is idle; false means the timeout hit
// with runs still in flight (the /restart caller then proceeds
// anyway and says so). A zero timeout returns at once.
func (a *App) Drain(timeout time.Duration) bool {
	if a == nil {
		return true
	}
	done := make(chan struct{})
	go func() {
		a.runWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// NewSession starts a fresh session: the conversation history is
// dropped from memory (facts survive — long-term memory is not
// wiped by starting a new chat) and persisted. The caller resets
// the per-chat session bookkeeping (undo stack, last input).
func (a *App) NewSession() error {
	if a == nil || a.Memory == nil {
		return errors.New("app: NewSession called on uninitialised App (nil Memory)")
	}
	return a.Memory.ClearMessages()
}

// UndoLastTurns removes the last n user turns from memory — each turn
// is a user message plus everything that followed it up to the next
// user message — and returns the undone user texts in chronological
// order (oldest undone turn first). n < 1 and an empty history are
// errors; nothing is removed when there is nothing to undo.
func (a *App) UndoLastTurns(n int) ([]string, error) {
	if a == nil || a.Memory == nil {
		return nil, errors.New("app: UndoLastTurns called on uninitialised App (nil Memory)")
	}
	if n < 1 {
		return nil, fmt.Errorf("app: undo count must be at least 1, got %d", n)
	}
	all, err := a.Memory.RecentMessages(0)
	if err != nil {
		return nil, fmt.Errorf("app: undo: read history: %w", err)
	}
	// Turn boundaries are the user messages.
	var userIdx []int
	for i, m := range all {
		if m.Role == "user" {
			userIdx = append(userIdx, i)
		}
	}
	if len(userIdx) == 0 {
		return nil, errors.New("app: nothing to undo: no user turns in history")
	}
	if n > len(userIdx) {
		n = len(userIdx)
	}
	cut := userIdx[len(userIdx)-n]
	undone := make([]string, 0, n)
	for _, i := range userIdx[len(userIdx)-n:] {
		undone = append(undone, all[i].Content)
	}
	kept := append([]model.Message(nil), all[:cut]...)
	if err := a.Memory.ReplaceMessages(kept); err != nil {
		return nil, fmt.Errorf("app: undo: rewrite history: %w", err)
	}
	return undone, nil
}

// compressRecentWindow is how many recent messages /compress keeps
// (mirrors the agent loop's context window of 10). Older messages
// are summarised by the provider and dropped.
const compressRecentWindow = 10

// Compress summarises the conversation prefix older than the recent
// window with a real model call and drops it: the summary is stored
// as the "conversation-summary" fact (so fact-aware recall still
// surfaces it) and only the recent window stays in history. It
// returns messages before/after and the summary length in runes. When
// nothing is old enough (history fits the window) it returns
// before == after with an error-free "nothing to compress" signal via
// summaryLen == 0 and before <= compressRecentWindow — callers check
// before/after, not the error. A provider failure leaves history
// untouched and returns the error.
func (a *App) Compress(ctx context.Context, keepRecent int) (before, after, summaryLen int, err error) {
	return a.compressWith(ctx, keepRecent, "")
}

// CompressWithSessionMemory is the /compress variant that draws on
// session memory: the session-memory text (decisions and facts
// extracted from earlier turns) is included in the summarisation
// prompt, and the saved fact cites that it drew on session memory.
// An empty sessionMemory behaves exactly like Compress.
func (a *App) CompressWithSessionMemory(ctx context.Context, keepRecent int, sessionMemory string) (before, after, summaryLen int, err error) {
	return a.compressWith(ctx, keepRecent, sessionMemory)
}

func (a *App) compressWith(ctx context.Context, keepRecent int, sessionMemory string) (before, after, summaryLen int, err error) {
	if a == nil || a.Memory == nil {
		return 0, 0, 0, errors.New("app: Compress called on uninitialised App (nil Memory)")
	}
	if keepRecent < 1 {
		keepRecent = compressRecentWindow
	}
	all, err := a.Memory.RecentMessages(0)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("app: compress: read history: %w", err)
	}
	before = len(all)
	if before <= keepRecent {
		return before, before, 0, nil
	}
	old := all[:len(all)-keepRecent]
	recent := all[len(all)-keepRecent:]
	provider := a.provider()
	if provider == nil {
		return before, before, 0, errors.New("app: compress: no provider configured")
	}
	var b strings.Builder
	b.WriteString("Summarise the following conversation messages into a concise memory. Preserve facts, decisions, names, numbers and anything the user asked to remember. Do not invent anything not in the messages.\n\n")
	if strings.TrimSpace(sessionMemory) != "" {
		b.WriteString("Session memory (decisions and facts extracted from earlier turns — treat these as established, and preserve them in the summary):\n")
		b.WriteString(strings.TrimSpace(sessionMemory))
		b.WriteString("\n\n")
	}
	for _, m := range old {
		fmt.Fprintf(&b, "%s: %s\n", m.Role, m.Content)
	}
	resp, err := provider.Chat(ctx, model.ChatRequest{
		Messages: []model.Message{
			{Role: "system", Content: "You summarise conversations into compact memories."},
			{Role: "user", Content: b.String()},
		},
	})
	if err != nil {
		return before, before, 0, fmt.Errorf("app: compress: summarisation call: %w", err)
	}
	summary := strings.TrimSpace(resp.Message.Content)
	if summary == "" {
		return before, before, 0, errors.New("app: compress: provider returned an empty summary")
	}
	if strings.TrimSpace(sessionMemory) != "" {
		summary += "\n(Drew on session memory.)"
	}
	if err := a.Memory.AddFact("conversation-summary", summary); err != nil {
		return before, before, 0, fmt.Errorf("app: compress: store summary: %w", err)
	}
	if err := a.Memory.ReplaceMessages(recent); err != nil {
		return before, before, 0, fmt.Errorf("app: compress: drop summarised messages: %w", err)
	}
	return before, len(recent), len([]rune(summary)), nil
}

// ExportConversation writes the current conversation as Markdown
// under DataDir/exports/conversation-<chatID>-<timestamp>.md and
// returns the path and message count. Messages carry roles and
// content; Orion's memory store records no per-message timestamps,
// so the export says so instead of inventing times. The file is
// created 0600 (it holds the user's conversation).
func (a *App) ExportConversation(chatID int64, title string, totalTokens, turns int) (string, int, error) {
	if a == nil || a.Memory == nil {
		return "", 0, errors.New("app: ExportConversation called on uninitialised App (nil Memory)")
	}
	if a.Config == nil || a.Config.DataDir == "" {
		return "", 0, errors.New("app: ExportConversation: no data dir configured")
	}
	msgs, err := a.Memory.RecentMessages(0)
	if err != nil {
		return "", 0, fmt.Errorf("app: export: read history: %w", err)
	}
	dir := filepath.Join(a.Config.DataDir, "exports")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, fmt.Errorf("app: export: create exports dir: %w", err)
	}
	stamp := time.Now().Format("20060102-150405")
	path := filepath.Join(dir, fmt.Sprintf("conversation-%d-%s.md", chatID, stamp))
	var b strings.Builder
	if strings.TrimSpace(title) != "" {
		fmt.Fprintf(&b, "# %s\n\n", title)
	} else {
		fmt.Fprintf(&b, "# Conversation %d\n\n", chatID)
	}
	fmt.Fprintf(&b, "Chat ID: %d\nExported: %s\nProvider: %s\nModel: %s\nMessages: %d\nTokens this chat: %d\nTurns this chat: %d\n\n",
		chatID, time.Now().Format(time.RFC3339), a.providerName(), a.activeModel(), len(msgs), totalTokens, turns)
	b.WriteString("Message timestamps are not recorded by Orion's memory store.\n\n")
	for _, m := range msgs {
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", m.Role, m.Content)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return "", 0, fmt.Errorf("app: export: write %s: %w", path, err)
	}
	return path, len(msgs), nil
}

// providerName returns the current provider name, or "unknown".
func (a *App) providerName() string {
	if p := a.provider(); p != nil {
		return p.Name()
	}
	return "unknown"
}

// ProviderName returns the current provider's name, or "unknown".
// It is the exported read used by cmd/oriond's /model command.
func (a *App) ProviderName() string { return a.providerName() }

// ActiveModel returns the effective model name for the active
// provider, or "" when the config cannot say. It is the exported
// read used by cmd/oriond's /model command.
func (a *App) ActiveModel() string { return a.activeModel() }

// MemorySummary renders the /memory view: fact count and the indexed
// fact list (1-based, sorted by key — the same order ForgetFact
// uses), message count and data dir.
func (a *App) MemorySummary() (string, error) {
	if a == nil || a.Memory == nil {
		return "", errors.New("app: MemorySummary called on uninitialised App (nil Memory)")
	}
	facts, err := a.Memory.ListFacts()
	if err != nil {
		return "", fmt.Errorf("app: memory: list facts: %w", err)
	}
	msgs, err := a.Memory.RecentMessages(0)
	if err != nil {
		return "", fmt.Errorf("app: memory: read history: %w", err)
	}
	dataDir := "unknown"
	if a.Config != nil && a.Config.DataDir != "" {
		dataDir = a.Config.DataDir
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Memory: %d fact(s), %d message(s)\nData dir: %s\n", len(facts), len(msgs), dataDir)
	if len(facts) == 0 {
		b.WriteString("No facts stored yet.")
		return b.String(), nil
	}
	b.WriteString("Facts:\n")
	for i, f := range facts {
		fmt.Fprintf(&b, "  %d. %s: %s\n", i+1, f.Key, f.Value)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// ForgetFact deletes fact number index (1-based, sorted by key, the
// order MemorySummary shows) and returns the deleted key. Out of
// range is an error that names the valid range.
func (a *App) ForgetFact(index int) (string, error) {
	if a == nil || a.Memory == nil {
		return "", errors.New("app: ForgetFact called on uninitialised App (nil Memory)")
	}
	facts, err := a.Memory.ListFacts()
	if err != nil {
		return "", fmt.Errorf("app: memory: list facts: %w", err)
	}
	if index < 1 || index > len(facts) {
		return "", fmt.Errorf("app: no fact %d (have %d fact(s), numbered 1-%d)", index, len(facts), len(facts))
	}
	key := facts[index-1].Key
	if err := a.Memory.DeleteFact(key); err != nil {
		return "", fmt.Errorf("app: memory: delete fact %q: %w", key, err)
	}
	return key, nil
}

// AgentsSummary renders the /agents view: the subagent manager's
// tasks (count + IDs, from Manager.List — never fabricated) and the
// currently running chat turns (the per-chat cancel registry).
// Per task it shows a short id (first 8 chars of the 16-hex id),
// status, brief truncated to ~80 runes, duration (elapsed for
// running tasks, recorded total for finished ones), tokens used when
// finished, and a result summary truncated to ~200 runes when done
// (the error text for failed/timed-out tasks).
func (a *App) AgentsSummary() string {
	var b strings.Builder
	if a == nil {
		return "orion: not initialised"
	}
	if a.Subagents == nil {
		b.WriteString("Subagents: manager not available\n")
	} else {
		tasks := a.Subagents.List()
		fmt.Fprintf(&b, "Subagents: %d task(s)\n", len(tasks))
		for _, t := range tasks {
			brief := truncateRunes(t.Brief, 80)
			shortID := t.ID
			if len([]rune(shortID)) > 8 {
				shortID = string([]rune(shortID)[:8])
			}
			// Duration is elapsed time for running tasks and the
			// recorded total for finished ones.
			duration := time.Since(t.StartedAt)
			res, finished := a.Subagents.GetResult(t.ID)
			if finished {
				duration = res.Duration
			}
			fmt.Fprintf(&b, "  • %s [%s] %s (%s)", shortID, t.Status, brief, duration.Round(time.Second))
			if finished {
				// Tokens are shown only when the task reported
				// any (a failed task may report zero); the
				// summary is the output for done tasks and the
				// error text otherwise.
				if res.Usage.Tokens > 0 {
					fmt.Fprintf(&b, ", %d tokens", res.Usage.Tokens)
				}
				summary := res.Output
				if res.Err != nil {
					summary = "error: " + res.Err.Error()
				}
				summary = strings.TrimSpace(summary)
				if summary != "" {
					fmt.Fprintf(&b, "\n      ↳ %s", truncateRunes(summary, 200))
				}
			}
			b.WriteString("\n")
		}
	}
	chats := a.RunningChats()
	if len(chats) == 0 {
		b.WriteString("Active turns: none")
	} else {
		fmt.Fprintf(&b, "Active turns: %d chat(s): %v", len(chats), chats)
	}
	return b.String()
}
