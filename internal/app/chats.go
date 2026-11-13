// Per-chat session/memory isolation (external audit P1-O3).
//
// Before this, every allowlisted Telegram chat shared one agent and
// one memory store: /new wiped everyone's history, /model switched
// the provider for everyone, and one chat's conversation was visible
// in every other chat's context. Each chat now gets its own agent
// state:
//
//   - its own conversation history (a per-chat memory store under
//     <DataDir>/chats/<chatID>/memory.json),
//   - its own provider/model override (persisted per chat as
//     <DataDir>/chats/<chatID>/provider.json; a chat without an
//     override follows the daemon default),
//   - its own session-memory log (<DataDir>/chats/<chatID>/
//     session-memory.md), so session extraction never mixes chats,
//   - its own usage/undo/title/footer state, which already lived in
//     the per-chat SessionStore.
//
// What stays SHARED, deliberately, is the user's own knowledge base:
// the long-term facts in the global memory.json and the typed
// memories under <DataDir>/memories/*.md. Those are the user's
// knowledge, not a conversation: a fact the user teaches the bot in
// one chat ("my favourite number is 17") is true in every chat, and
// splitting it per chat would silently fork the user's memory. The
// boundary is: facts + typed memories are shared; conversation
// history, provider choice, session memory, usage and undo state are
// per chat.
//
// The global agent (App.Agent, used by the local CLI) is untouched:
// orion run / orion chat keep running on the shared store, ungated,
// exactly as before. Telegram turns go through HandleStreamForChat.
//
// Concurrency: chat sessions are created lazily under chatsMu; a run
// in flight keeps the agent it started with, and a provider switch
// rebuilds the chat's agent for the NEXT turn only — the same
// contract the global SwitchProvider already had. Lock order is
// always chatsMu before App.mu (never the reverse): global
// SwitchProvider releases mu before rebuilding chat agents, so the
// two paths can never deadlock each other.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/security"
	"github.com/irfan/orion/internal/session"
)

// chatStore is a memory.Store that splits the two halves of memory
// along the P1-O3 boundary: facts delegate to the shared store (the
// user's knowledge base), messages delegate to the chat's own store
// (per-chat conversation history). Save persists both; Close closes
// only the chat's store — the shared store's lifetime belongs to the
// App/global agent, and a per-chat agent shutting down must never
// close it under the other chats.
type chatStore struct {
	shared memory.Store
	chat   memory.Store
}

var _ memory.Store = (*chatStore)(nil)

func (s *chatStore) AddFact(key, value string) error { return s.shared.AddFact(key, value) }
func (s *chatStore) GetFact(key string) (memory.Fact, bool, error) {
	return s.shared.GetFact(key)
}
func (s *chatStore) DeleteFact(key string) error { return s.shared.DeleteFact(key) }
func (s *chatStore) SearchFacts(query string) ([]memory.Fact, error) {
	return s.shared.SearchFacts(query)
}
func (s *chatStore) ListFacts() ([]memory.Fact, error)  { return s.shared.ListFacts() }
func (s *chatStore) AddMessage(msg model.Message) error { return s.chat.AddMessage(msg) }
func (s *chatStore) RecentMessages(n int) ([]model.Message, error) {
	return s.chat.RecentMessages(n)
}
func (s *chatStore) ReplaceMessages(msgs []model.Message) error {
	return s.chat.ReplaceMessages(msgs)
}
func (s *chatStore) ClearMessages() error { return s.chat.ClearMessages() }
func (s *chatStore) Save() error {
	errShared := s.shared.Save()
	errChat := s.chat.Save()
	return errors.Join(errShared, errChat)
}

// Close closes only the per-chat store. See the type doc: the shared
// store is owned by the App, not by this chat.
func (s *chatStore) Close() error { return s.chat.Close() }

// chatSession is one Telegram chat's agent state.
type chatSession struct {
	// dir is the chat's state directory
	// (<DataDir>/chats/<chatID>); "" when the app has no DataDir.
	dir string
	// store is the split memory store (shared facts, per-chat
	// messages). Created once per chat and shared by every agent
	// the chat ever builds: two FileStores on the same memory.json
	// would diverge, so a provider switch reuses this store.
	store *chatStore
	// sessionLog is the chat's own session-memory log (nil when
	// the app has no DataDir, or the log could not open — session
	// extraction then degrades to off for this chat only).
	sessionLog *memory.SessionLog
	// sessionID is the chat's live per-session transcript id (see
	// sessions.go); "" until the first transcript is opened.
	// transcript is that session's append-only JSONL transcript
	// (nil when the chat has no data dir, or it could not open —
	// the chat then runs on memory only, exactly as before).
	sessionID  string
	transcript *session.Transcript
	// agent is the chat's current agent (nil until the first turn).
	agent *core.Agent
	// provider is the agent's provider instance.
	provider model.Provider
	// override is the chat's provider override ("" = follow the
	// daemon default). providerName/modelName are the effective
	// values for the current agent (or the override, when no agent
	// exists yet).
	override     string
	providerName string
	modelName    string
}

// chatSessionFor returns the chat's session, creating its store (and
// loading a persisted provider override) on first use. It never
// builds the agent — AgentForChat does that. Callers must not hold
// chatsMu (it takes it).
func (a *App) chatSessionFor(chatID int64) (*chatSession, error) {
	a.chatsMu.Lock()
	defer a.chatsMu.Unlock()
	if a.chats == nil {
		a.chats = map[int64]*chatSession{}
	}
	if cs, ok := a.chats[chatID]; ok && cs != nil {
		return cs, nil
	}
	if a.Config == nil || a.Memory == nil {
		return nil, errors.New("app: per-chat session needs a configured App (nil Config or Memory)")
	}
	cs := &chatSession{}
	dataDir := ""
	if a.Config != nil {
		dataDir = a.Config.DataDir
	}
	if dataDir == "" {
		// No data dir (hand-built test Apps): per-chat history
		// lives in an ephemeral store — still isolated, just not
		// persisted. The boundary is the same; only durability
		// changes.
		cs.store = &chatStore{shared: a.Memory, chat: memory.NewEphemeralStore()}
	} else {
		dir := filepath.Join(dataDir, "chats", strconv.FormatInt(chatID, 10))
		cs.dir = dir
		chatMem, err := memory.NewFileStore(dir)
		if err != nil {
			return nil, fmt.Errorf("app: chat %d: open chat memory: %w", chatID, err)
		}
		cs.store = &chatStore{shared: a.Memory, chat: chatMem}
		if slog, err := memory.NewSessionLog(dir); err != nil {
			if a.Logger != nil {
				a.Logger.Printf("app: chat %d: session log unavailable (%v); session extraction disabled for this chat", chatID, err)
			}
		} else {
			cs.sessionLog = slog
		}
		if name, err := readChatProviderOverride(dir); err == nil && name != "" {
			cs.override = name
			cs.providerName = name
		}
	}
	a.chats[chatID] = cs
	return cs, nil
}

// AgentForChat returns the chat's agent, building it on first use
// (per-chat store, per-chat session log, effective provider). A run
// in flight keeps the agent it started with; a provider switch
// replaces the stored agent for the next turn.
func (a *App) AgentForChat(chatID int64) (*core.Agent, error) {
	cs, err := a.chatSessionFor(chatID)
	if err != nil {
		return nil, err
	}
	a.chatsMu.Lock()
	defer a.chatsMu.Unlock()
	if cs.agent != nil {
		return cs.agent, nil
	}
	provider, providerName, modelName, err := a.buildChatProviderLocked(cs)
	if err != nil {
		return nil, err
	}
	agent, err := core.NewAgent(a.Config, provider, a.Tools, cs.store)
	if err != nil {
		return nil, fmt.Errorf("app: chat %d: build agent: %w", chatID, err)
	}
	if a.Logger != nil {
		agent.SetLogger(a.Logger)
	}
	// Memory v2 wiring, with the CHAT's session log (not the
	// shared one): session extraction for this chat appends to
	// this chat's session-memory.md only.
	applyMemoryV2WiringTo(agent, a.typedAdapter, cs.sessionLog, a.driftRoot)
	// Skill wiring: the rebuilt agent advertises the same skills
	// as the app (nil-safe: a hand-built App has no Skills).
	agent.SetSkills(a.Skills)
	// Hook wiring (rc9): the chat agent enforces the same hook
	// policy as the global agent (nil-safe: a hand-built App has
	// no Hooks, and SetHooks(nil) means "no hooks").
	agent.SetHooks(a.Hooks)
	if g := a.permissionGate(); g != nil {
		agent.SetPermissionGate(g)
	}
	// Per-session transcript (audit P0 #4): the agent records
	// every stored turn into the chat's live session transcript.
	// Best-effort — see wireChatTranscript.
	a.wireChatTranscript(cs, agent)
	cs.agent = agent
	cs.provider = provider
	cs.providerName = providerName
	cs.modelName = modelName
	return agent, nil
}

// wireChatTranscript opens (or reuses) the chat's live session
// transcript and wires it into agent as its transcript sink, so
// every turn the agent stores is also appended to the session's
// JSONL file. The first open migrates any pre-transcript turns
// from the chat's memory.json into the transcript (see
// openOrCreateTranscript); later opens reuse the live session.
// A chat with no data dir has no transcript. A failure is logged
// and the agent runs on memory only — a transcript problem must
// never fail a chat. Callers must hold chatsMu.
func (a *App) wireChatTranscript(cs *chatSession, agent *core.Agent) {
	if cs == nil || agent == nil || cs.dir == "" {
		return
	}
	if cs.transcript == nil {
		tr, id, err := openOrCreateTranscript(cs.dir, cs.store, filepath.Join(cs.dir, "memory.json"), a.Logger)
		if err != nil {
			if a.Logger != nil {
				a.Logger.Printf("app: chat transcript unavailable (%v); turns will not be recorded to a transcript", err)
			}
			return
		}
		cs.transcript = tr
		cs.sessionID = id
	}
	agent.SetTranscript(cs.transcript)
}

// buildChatProviderLocked resolves the chat's effective provider:
// the override when one is set, else the daemon default. Callers
// must hold chatsMu (it reads App.mu, preserving the chatsMu ->
// mu lock order). It builds a fresh provider instance for an
// override; for the default it reuses the app's current provider
// (providers are concurrent-safe — see subagent.go).
func (a *App) buildChatProviderLocked(cs *chatSession) (model.Provider, string, string, error) {
	if cs.override != "" {
		provider, entry, key, err := a.resolveProvider(cs.override)
		if err != nil {
			return nil, "", "", err
		}
		return provider, key, providerModel(entry, a.Config.Model), nil
	}
	p := a.provider()
	if p == nil {
		return nil, "", "", errors.New("app: no provider configured")
	}
	name := ""
	if a.Config != nil {
		name = strings.ToLower(strings.TrimSpace(a.Config.DefaultProvider))
	}
	if name == "" {
		name = strings.ToLower(strings.TrimSpace(p.Name()))
	}
	return p, name, a.activeModel(), nil
}

// chatProviderInfo returns the chat's effective provider/model
// names (override when set, else the daemon default). It never
// builds an agent.
func (a *App) chatProviderInfo(chatID int64) (provider, modelName string) {
	cs, err := a.chatSessionFor(chatID)
	if err != nil {
		return a.providerName(), a.activeModel()
	}
	a.chatsMu.Lock()
	defer a.chatsMu.Unlock()
	if cs.override != "" {
		name := cs.override
		modelName := cs.modelName
		if modelName == "" {
			if entry, ok := a.providerEntry(name); ok {
				modelName = providerModel(entry, a.Config.Model)
			}
		}
		return name, modelName
	}
	if cs.agent != nil {
		return cs.providerName, cs.modelName
	}
	// No agent yet and no override: the chat follows the daemon
	// default, whose name comes from the CONFIG (not from the
	// provider instance's Name() — a hand-built App's provider
	// need not match its config, and the config is authoritative
	// for what the default is called).
	return a.defaultProviderName(), a.activeModel()
}

// defaultProviderName returns the daemon default provider's name
// from config (lowercased, trimmed), or "" when unconfigured.
func (a *App) defaultProviderName() string {
	if a == nil || a.Config == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(a.Config.DefaultProvider))
}

// providerEntry returns the config entry for name (case-insensitive),
// if one exists.
func (a *App) providerEntry(name string) (config.ProviderConfig, bool) {
	if a == nil || a.Config == nil {
		return config.ProviderConfig{}, false
	}
	for k, p := range a.Config.Providers {
		if strings.EqualFold(k, name) {
			return p, true
		}
	}
	return config.ProviderConfig{}, false
}

// providerModel returns the effective model for a provider entry:
// the entry's model, else the fallback (the config's top-level
// model).
func providerModel(entry config.ProviderConfig, fallback string) string {
	if strings.TrimSpace(entry.Model) != "" {
		return entry.Model
	}
	return fallback
}

// ChatProviderName returns the chat's effective provider name
// (override, else the daemon default). It is the per-chat read used
// by /model.
func (a *App) ChatProviderName(chatID int64) string {
	provider, _ := a.chatProviderInfo(chatID)
	return provider
}

// ModelInfoForChat returns the chat's provider/model and the known
// provider names, for /model.
func (a *App) ModelInfoForChat(chatID int64) (provider, modelName string, providers []string) {
	provider, modelName = a.chatProviderInfo(chatID)
	return provider, modelName, config.KnownProviderNames()
}

// SetPermissionGate installs the permission gate on the app: future
// per-chat agents get it at build time, and the global agent gets
// it now (it never runs in the daemon, but if it ever did the gate
// would apply there too — fail closed). A nil gate removes it. The
// CLI never installs a gate, so local runs stay ungated.
func (a *App) SetPermissionGate(g *permissions.Gate) {
	if a == nil {
		return
	}
	a.gateMu.Lock()
	a.gate = g
	a.gateMu.Unlock()
	if a.Agent != nil {
		a.Agent.SetPermissionGate(g)
	}
}

// permissionGate returns the installed gate, or nil.
func (a *App) permissionGate() *permissions.Gate {
	if a == nil {
		return nil
	}
	a.gateMu.RLock()
	defer a.gateMu.RUnlock()
	return a.gate
}

// HandleForChat runs one user input through the chat's agent (the
// per-chat equivalent of Handle, for non-streaming callers). The
// per-chat daily token budget is enforced (see runChatTurn): a chat
// over budget is refused before the model is called, and the turn's
// spend is recorded afterwards.
func (a *App) HandleForChat(ctx context.Context, chatID int64, input string) (string, error) {
	reply, _, err := a.runChatTurn(ctx, chatID, input, nil)
	return reply, err
}

// runChatTurn is the single per-chat turn path behind HandleForChat
// and HandleStreamForChat, and the ONLY place the per-chat daily
// token budget is enforced — every channel that runs a chat's agent
// (oriond's Telegram turns, `orion daemon`, /retry) goes through
// here, so no path can silently skip the budget:
//
//   - pre-flight: when the chat is already over budget, the turn is
//     refused BEFORE the provider is called, with the
//     BudgetExceededError (the caller turns it into a plain reply
//     for the user; the error names the usage, the limit and the
//     UTC-midnight reset). Nothing is recorded for a refused turn.
//   - post-turn: the turn's reported usage is recorded with its
//     prompt/completion/subagent split (RecordUsage), even when the
//     turn errored — a failed turn still consumed tokens, and the
//     next turn must see the spend. A Record error (the turn pushed
//     the chat over the limit) is swallowed deliberately: the turn
//     already ran, and the NEXT turn is the one that gets refused.
//     A nil Budget (hand-built Apps) means unbudgeted, exactly as
//     before.
//
// The returned stats carry the chat's provider/model plus the
// prompt/completion/subagent split, so the footer and /usage can
// show the turn honestly.
func (a *App) runChatTurn(ctx context.Context, chatID int64, input string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
	agent, err := a.AgentForChat(chatID)
	if err != nil {
		return "", nil, err
	}
	if a.Budget != nil {
		if err := a.Budget.Check(chatID, 0); err != nil {
			return "", nil, err
		}
	}
	providerName, modelName := a.chatProviderInfo(chatID)
	var stats *core.TurnStats
	wrapped := func(ev core.ProgressEvent) {
		if ev.Kind == core.EventTurnFinished {
			stats = &core.TurnStats{
				Provider:         providerName,
				Model:            modelName,
				Tokens:           ev.Usage.TotalTokens,
				PromptTokens:     ev.Usage.PromptTokens,
				CompletionTokens: ev.Usage.CompletionTokens,
				SubagentTokens:   ev.SubagentTokens,
				Steps:            ev.Step,
				Duration:         ev.Duration,
				MemoriesUsed:     ev.MemoriesUsed,
			}
		}
		if emit != nil {
			emit(ev)
		}
	}
	reply, err := agent.RunWithEventHandler(ctx, input, wrapped)
	if stats != nil && a.Budget != nil {
		// Best effort: a failed record must never fail the turn
		// that just completed (and an over-budget record is
		// expected — the spend happened; the next turn refuses).
		_ = a.Budget.RecordUsage(chatID, model.Usage{
			PromptTokens:     stats.PromptTokens,
			CompletionTokens: stats.CompletionTokens,
			TotalTokens:      stats.Tokens,
		}, stats.SubagentTokens)
	}
	if a.Audit != nil {
		_, _ = a.Audit.Append("user", "agent_run", truncateRunes(security.RedactSecrets(input), 200))
	}
	return reply, stats, err
}

// HandleStreamForChat is HandleStream for one chat: the turn runs on
// the chat's agent, and the TurnStats carry the chat's provider and
// model (so the footer shows the model THAT chat is using). The
// per-chat daily token budget is enforced (see runChatTurn).
func (a *App) HandleStreamForChat(ctx context.Context, chatID int64, input string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
	return a.runChatTurn(ctx, chatID, input, emit)
}

// SwitchProviderForChat switches ONLY this chat's provider: it
// validates and builds the provider, persists the override in the
// chat's directory (never the config file — the daemon default is
// untouched), and rebuilds the chat's agent for the next turn. Other
// chats keep their providers; a run in flight on this chat keeps
// the old agent and finishes on it. The daemon default still
// applies to chats without an override.
func (a *App) SwitchProviderForChat(chatID int64, name string) error {
	if a == nil || a.Config == nil {
		return errors.New("app: SwitchProviderForChat called on uninitialised App")
	}
	provider, entry, key, err := a.resolveProvider(name)
	if err != nil {
		return err
	}
	cs, err := a.chatSessionFor(chatID)
	if err != nil {
		return err
	}
	// Persist first, then swap: if the write fails the chat keeps
	// running on its old provider instead of diverging from disk.
	if cs.dir != "" {
		if err := writeChatProviderOverride(cs.dir, key); err != nil {
			return fmt.Errorf("app: switch provider for chat %d: save override: %w", chatID, err)
		}
	}
	modelName := providerModel(entry, a.Config.Model)
	a.chatsMu.Lock()
	defer a.chatsMu.Unlock()
	cs.override = key
	cs.providerName = key
	cs.modelName = modelName
	if cs.agent == nil {
		// No agent yet: the override is recorded and the next
		// AgentForChat builds with it. Nothing else to do.
		return nil
	}
	agent, err := core.NewAgent(a.Config, provider, a.Tools, cs.store)
	if err != nil {
		// The override is already persisted, so a restart picks
		// it up; the live chat keeps its old agent and the build
		// failure is reported honestly.
		return fmt.Errorf("app: switch provider for chat %d: build agent: %w", chatID, err)
	}
	if a.Logger != nil {
		agent.SetLogger(a.Logger)
	}
	// Memory v2 wiring with the CHAT's session log (not the
	// shared one — applyMemoryV2Wiring would wire the shared
	// log, so call the underlying helper directly).
	applyMemoryV2WiringTo(agent, a.typedAdapter, cs.sessionLog, a.driftRoot)
	// Skill wiring: the rebuilt agent advertises the same skills
	// as the app (nil-safe: a hand-built App has no Skills).
	agent.SetSkills(a.Skills)
	// Hook wiring (rc9): same hook policy as the global agent
	// (nil-safe — see AgentForChat).
	agent.SetHooks(a.Hooks)
	if g := a.permissionGate(); g != nil {
		agent.SetPermissionGate(g)
	}
	// The rebuilt agent records into the same live session
	// transcript as the agent it replaces.
	a.wireChatTranscript(cs, agent)
	cs.agent = agent
	cs.provider = provider
	return nil
}

// NewSessionForChat starts a fresh session for ONE chat: only that
// chat's conversation history is dropped (facts survive — long-term
// memory is not wiped by starting a new chat), and the live session
// rotates: a fresh session id is recorded in sessions/current.json
// and the chat's agent records further turns into the new
// transcript. The old transcript file stays on disk, listed by
// /sessions and resumable by /resume. Other chats are untouched.
// A rotation failure is logged and the chat keeps recording into
// its old transcript — /new's primary job (clearing history) has
// already succeeded, and no turn is ever lost to a rotation error.
func (a *App) NewSessionForChat(chatID int64) error {
	cs, err := a.chatSessionFor(chatID)
	if err != nil {
		return err
	}
	if err := cs.store.ClearMessages(); err != nil {
		return err
	}
	if cs.dir == "" {
		return nil
	}
	tr, id, err := rotateTranscript(cs.dir)
	if err != nil {
		if a.Logger != nil {
			a.Logger.Printf("app: chat %d: rotate session failed (%v); keeping live session %s", chatID, err, cs.sessionID)
		}
		return nil
	}
	a.chatsMu.Lock()
	cs.transcript = tr
	cs.sessionID = id
	agent := cs.agent
	a.chatsMu.Unlock()
	if agent != nil {
		// A new session starts with a clean compaction slate:
		// the breaker state belonged to the previous session's
		// history.
		agent.SetTranscript(tr)
		agent.ResetCompactionBreaker()
	}
	return nil
}

// UndoLastTurnsForChat removes the last n user turns from ONE
// chat's history and returns the undone user texts. Other chats are
// untouched.
func (a *App) UndoLastTurnsForChat(chatID int64, n int) ([]string, error) {
	cs, err := a.chatSessionFor(chatID)
	if err != nil {
		return nil, err
	}
	return undoLastTurnsOn(cs.store, n)
}

// CompressForChat summarises ONE chat's conversation prefix older
// than the recent window and drops it. Other chats are untouched.
func (a *App) CompressForChat(ctx context.Context, chatID int64, keepRecent int) (before, after, summaryLen int, err error) {
	return a.compressForChatWith(ctx, chatID, keepRecent, "")
}

// CompressWithSessionMemoryForChat is the /compress variant for one
// chat, drawing on that chat's session memory.
func (a *App) CompressWithSessionMemoryForChat(ctx context.Context, chatID int64, keepRecent int, sessionMemory string) (before, after, summaryLen int, err error) {
	return a.compressForChatWith(ctx, chatID, keepRecent, sessionMemory)
}

func (a *App) compressForChatWith(ctx context.Context, chatID int64, keepRecent int, sessionMemory string) (before, after, summaryLen int, err error) {
	cs, err := a.chatSessionFor(chatID)
	if err != nil {
		return 0, 0, 0, err
	}
	provider := a.chatCompressProvider(chatID, cs)
	before, after, summaryLen, summary, err := compressWithOn(ctx, cs.store, provider, keepRecent, sessionMemory)
	if err == nil && summaryLen > 0 {
		// A successful manual /compress clears this chat's
		// auto-compaction circuit breaker, and the summary is
		// preserved in the chat's live session transcript as
		// well as the conversation-summary fact.
		a.chatsMu.Lock()
		agent := cs.agent
		tr := cs.transcript
		a.chatsMu.Unlock()
		if agent != nil {
			agent.ResetCompactionBreaker()
		}
		if tr != nil {
			if err := tr.AppendSummary(summary); err != nil && a.Logger != nil {
				a.Logger.Printf("app: chat %d: compress: record summary in transcript: %v", chatID, err)
			}
		}
	}
	return before, after, summaryLen, err
}

// chatCompressProvider returns the provider a chat's /compress
// summarisation call should use: the chat's own provider when its
// agent exists, else a freshly resolved override, else the daemon
// default. It never builds an agent just to compress.
func (a *App) chatCompressProvider(chatID int64, cs *chatSession) model.Provider {
	a.chatsMu.Lock()
	if cs.agent != nil && cs.provider != nil {
		p := cs.provider
		a.chatsMu.Unlock()
		return p
	}
	override := cs.override
	a.chatsMu.Unlock()
	if override != "" {
		if provider, _, _, err := a.resolveProvider(override); err == nil {
			return provider
		}
	}
	return a.provider()
}

// SessionMemoryTextForChat returns ONE chat's session-memory.md
// content ("" when absent or unwired), for /compress to draw on.
// Other chats' session memories are never read here.
func (a *App) SessionMemoryTextForChat(ctx context.Context, chatID int64) (string, error) {
	cs, err := a.chatSessionFor(chatID)
	if err != nil {
		return "", err
	}
	if cs.sessionLog == nil {
		return "", nil
	}
	return cs.sessionLog.Read()
}

// StatusLineForChat returns the /status body for ONE chat: that
// chat's provider/model, its own message count, the shared fact
// count, limits, data dir and uptime.
func (a *App) StatusLineForChat(chatID int64) string {
	if a == nil {
		return "orion: not initialised"
	}
	providerName, modelName := a.chatProviderInfo(chatID)
	if modelName == "" {
		modelName = "unknown"
	}
	if providerName == "" {
		providerName = "unknown"
	}
	maxSteps, timeout := 0, 0
	if a.Config != nil {
		maxSteps, timeout = a.Config.MaxSteps, a.Config.TimeoutSeconds
	}
	msgs, facts := 0, 0
	if cs, err := a.chatSessionFor(chatID); err == nil {
		if all, err := cs.store.RecentMessages(0); err == nil {
			msgs = len(all)
		}
		if list, err := cs.store.ListFacts(); err == nil {
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
	if id, err := a.CurrentSessionIDForChat(chatID); err == nil && id != "" {
		fmt.Fprintf(&b, "\nSession: %s", id)
	}
	// Auto-compaction state, so a tripped breaker or an
	// approaching compaction is visible in /status instead of
	// silent: the user sees it before history pressure forces a
	// drop, and a tripped breaker names its remedy (/compress).
	if st, err := a.CompactionStatusForChat(chatID); err == nil {
		switch {
		case st.Tripped:
			fmt.Fprintf(&b, "\nCompaction: breaker tripped after %d failures (%s); run /compress", st.ConsecutiveFailures, st.LastError)
		case st.Warning:
			fmt.Fprintf(&b, "\nCompaction: approaching (history %.0f%% full; auto-compacts at 80%%)", st.Pressure*100)
		case st.Compactions > 0:
			fmt.Fprintf(&b, "\nCompaction: %d auto-compaction(s), history %.0f%% full", st.Compactions, st.Pressure*100)
		}
	}
	if !a.StartedAt.IsZero() {
		fmt.Fprintf(&b, "\nUptime: %s", time.Since(a.StartedAt).Round(time.Second))
	}
	return b.String()
}

// MemorySummaryForChat renders the /memory view for ONE chat: the
// shared fact list plus THAT chat's message count.
func (a *App) MemorySummaryForChat(chatID int64) (string, error) {
	cs, err := a.chatSessionFor(chatID)
	if err != nil {
		return "", err
	}
	return memorySummaryOn(a, cs.store)
}

// closeChatAgents shuts down every per-chat agent: each saves its
// chat store (and, through it, the shared facts) and closes only
// its chat store. The shared store itself is closed by the global
// agent's shutdown afterwards, in App.Close.
func (a *App) closeChatAgents() error {
	if a == nil {
		return nil
	}
	a.chatsMu.Lock()
	sessions := make([]*chatSession, 0, len(a.chats))
	for _, cs := range a.chats {
		sessions = append(sessions, cs)
	}
	a.chatsMu.Unlock()
	var errs []error
	for _, cs := range sessions {
		if cs == nil || cs.agent == nil {
			continue
		}
		if err := cs.agent.Shutdown(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// rebuildChatAgentsForDefault rebuilds every chat agent that has no
// provider override, on the daemon's (new) default provider. It
// runs after a global SwitchProvider: chats with an override keep
// their provider; chats without one follow the new default. Runs in
// flight keep the old agent. A rebuild failure leaves that chat on
// its old agent and is reported.
func (a *App) rebuildChatAgentsForDefault() error {
	if a == nil {
		return nil
	}
	a.chatsMu.Lock()
	sessions := make([]*chatSession, 0, len(a.chats))
	for _, cs := range a.chats {
		sessions = append(sessions, cs)
	}
	a.chatsMu.Unlock()
	var errs []error
	for _, cs := range sessions {
		if cs == nil || cs.override != "" || cs.agent == nil {
			continue
		}
		provider := a.provider()
		if provider == nil {
			continue
		}
		agent, err := core.NewAgent(a.Config, provider, a.Tools, cs.store)
		if err != nil {
			errs = append(errs, fmt.Errorf("app: rebuild chat agent: %w", err))
			continue
		}
		if a.Logger != nil {
			agent.SetLogger(a.Logger)
		}
		applyMemoryV2WiringTo(agent, a.typedAdapter, cs.sessionLog, a.driftRoot)
		// Skill wiring: the rebuilt agent advertises the same skills
		// as the app (nil-safe: a hand-built App has no Skills).
		agent.SetSkills(a.Skills)
		// Hook wiring (rc9): same hook policy as the global agent
		// (nil-safe — see AgentForChat).
		agent.SetHooks(a.Hooks)
		if g := a.permissionGate(); g != nil {
			agent.SetPermissionGate(g)
		}
		a.chatsMu.Lock()
		// Only swap when the session still has no override and
		// still holds the agent we rebuilt from: a concurrent
		// per-chat switch must win over this default rebuild.
		if cs.override == "" {
			cs.agent = agent
			cs.provider = provider
			cs.providerName, cs.modelName = a.chatProviderInfoFromLocked()
			// The rebuilt agent records into the same live
			// session transcript as the agent it replaces.
			if cs.transcript != nil {
				agent.SetTranscript(cs.transcript)
			}
		}
		a.chatsMu.Unlock()
	}
	return errors.Join(errs...)
}

// chatProviderInfoFromLocked returns the daemon default provider
// and model names. Callers must hold chatsMu (it reads App.mu).
func (a *App) chatProviderInfoFromLocked() (provider, modelName string) {
	return a.defaultProviderName(), a.activeModel()
}

// chatProviderOverrideFile is the per-chat provider override file
// name inside a chat's state directory.
const chatProviderOverrideFile = "provider.json"

// chatProviderOverride is the on-disk shape of the override file.
type chatProviderOverride struct {
	Provider string `json:"provider"`
}

// readChatProviderOverride reads the override for a chat directory.
// A missing file means "no override" (nil error, empty name); a
// corrupt file is an error the caller logs — but chatSessionFor
// treats it as no override rather than failing the chat, so a bad
// override file can never stop a chat from working (it just follows
// the default until the operator re-switches).
func readChatProviderOverride(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, chatProviderOverrideFile))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var p chatProviderOverride
	if err := json.Unmarshal(data, &p); err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(p.Provider)), nil
}

// writeChatProviderOverride persists a chat's provider override
// atomically (temp file 0600 + rename + dir fsync).
func writeChatProviderOverride(dir, name string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("app: create chat dir %q: %w", dir, err)
	}
	data, err := json.MarshalIndent(chatProviderOverride{Provider: name}, "", "  ")
	if err != nil {
		return fmt.Errorf("app: encode provider override: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".provider-*.tmp")
	if err != nil {
		return fmt.Errorf("app: create temp provider file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("app: chmod temp provider file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("app: write temp provider file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("app: sync temp provider file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("app: close temp provider file: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, chatProviderOverrideFile)); err != nil {
		return fmt.Errorf("app: rename temp provider file: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
