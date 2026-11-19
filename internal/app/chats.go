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
	"sync"
	"time"

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/cost"
	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/security"
	"github.com/irfan/orion/internal/session"
	"github.com/irfan/orion/internal/subagent"
	"github.com/irfan/orion/internal/tools"
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
	// modelOverride is the chat's model override for the override
	// provider ("" = the entry's model): a same-provider model
	// switch persists here, next to the provider, so /model
	// deepseek/deepseek-v4-pro survives a restart. It is only
	// meaningful while override != "".
	modelOverride string
	// once is a one-turn provider route (--once): applied at the
	// start of the next turn and cleared as soon as that turn
	// begins, so the turn after runs on the override (or the
	// daemon default) again. In-memory only, never persisted —
	// a restart must not resurrect a single-turn switch.
	once string
	// onceActive is true while the chat's agent is running on a
	// consumed --once route: the next turn's start rebuilds the
	// agent on the effective route and clears it. It is the
	// record that distinguishes "agent on a once provider" from
	// "agent on a failover provider" — the two restores look
	// alike but land on different providers.
	onceActive bool
	// sweptSubagents records subagent task IDs whose finished
	// tokens the runChatTurn sweep already charged to this chat
	// (F-A5-7), so a task is never charged twice across turns.
	// It is the ONLY exactly-once guard: there is deliberately no
	// time window (F-A7-2b) — a task that finishes mid-sweep must
	// still be charged by the next sweep, and a FinishedAt window
	// would skip it forever. Guarded by chatsMu.
	sweptSubagents map[string]bool
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
		if name, modelName, err := readChatProviderOverride(dir); err == nil && name != "" {
			cs.override = name
			cs.modelOverride = modelName
			cs.providerName = name
			cs.modelName = modelName
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
	home := cs.override
	if home == "" {
		home = a.defaultProviderName()
	}
	if err := a.rebuildChatAgentLocked(cs, provider, providerName, modelName, home); err != nil {
		return nil, fmt.Errorf("app: chat %d: build agent: %w", chatID, err)
	}
	return cs.agent, nil
}

// rebuildChatAgentLocked builds a fresh agent for cs on provider
// and swaps it in, with the same wiring every agent build applies
// (Memory v2, skills, hooks, failover chain, tool journal,
// permission gate, live transcript). It is the ONE build path for
// per-chat agents — AgentForChat, SwitchProviderForChat,
// ClearProviderOverrideForChat and the --once restore all go
// through it, so a wiring step added here can never be lost by one
// switch path (the Audit 4 B P1-1 class of bug). The swap happens
// only on success: a build failure leaves the chat on its old
// agent. The home provider is the provider the agent restores to
// after a failover — the chat's effective provider, NOT the
// provider it happens to run on this turn (a --once agent's home
// is the override/default, never the once route). Callers must
// hold chatsMu.
func (a *App) rebuildChatAgentLocked(cs *chatSession, provider model.Provider, providerName, modelName, homeName string) error {
	agent, err := core.NewAgent(a.Config, provider, a.Tools, cs.store)
	if err != nil {
		return err
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
	// Failover wiring: the chat agent shares the daemon's chain,
	// so a failed turn in this chat continues on the next
	// provider exactly like a global turn.
	a.applyFallbackWiring(agent)
	// Home wiring: the provider a failover restores to.
	agent.SetHomeProvider(homeName)
	// Tool journal (F-A9-1): the chat agent replays recorded
	// tool results on a re-delivered turn instead of
	// re-executing them (nil-safe: a hand-built App has no
	// journal, and SetToolJournal(nil) means "no dedup").
	agent.SetToolJournal(a.toolJournal)
	if g := a.permissionGate(); g != nil {
		agent.SetPermissionGate(g)
	}
	// The rebuilt agent records into the same live session
	// transcript as the agent it replaces.
	a.wireChatTranscript(cs, agent)
	cs.agent = agent
	cs.provider = provider
	cs.providerName = providerName
	cs.modelName = modelName
	return nil
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
		// The override stores provider and model separately, so
		// a same-provider model switch builds without
		// re-encoding the pair as a route string.
		entry, key, found := a.lookupProviderEntry(cs.override)
		if !found {
			return nil, "", "", fmt.Errorf("app: provider %q is not configured (supported providers: %s)", cs.override, strings.Join(config.KnownProviderNames(), ", "))
		}
		provider, entry, key, err := a.buildProviderEntry(entry, key, cs.modelOverride)
		if err != nil {
			return nil, "", "", err
		}
		// entry.Model is the model that was actually built
		// (buildProviderEntry applied the override with the
		// entry/global fallback).
		return provider, key, entry.Model, nil
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

// PermissionGate returns the installed gate, or nil. It is the
// exported read of permissionGate, for the Telegram entrypoints
// (cmd/orion, cmd/oriond) to assert the gate is wired before a
// polling loop starts (F-A6-3).
func (a *App) PermissionGate() *permissions.Gate { return a.permissionGate() }

// PlanningStore returns the planning store the registry's tools
// share (todo/task/plan/cron state, per chat), or nil for a
// hand-built App. The daemon's cron scheduler reads it to scan
// for due jobs; it never writes through it directly.
func (a *App) PlanningStore() *tools.PlanningStore {
	if a == nil {
		return nil
	}
	return a.planningStore
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

// turnLock returns the serialising mutex for one chat's turns,
// creating it (and the map) lazily. Hand-built Apps start with a
// nil map; Build-created Apps get one on first use either way.
// Entries are never evicted (F-C22): one mutex per chat that ever
// ran a turn — bounded in practice by the allowlisted chats, and
// far smaller than the per-chat session the chats map holds for
// the same chat (see the turnMu comment in app.go).
func (a *App) turnLock(chatID int64) *sync.Mutex {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	if a.turnLocks == nil {
		a.turnLocks = map[int64]*sync.Mutex{}
	}
	l, ok := a.turnLocks[chatID]
	if !ok {
		l = &sync.Mutex{}
		a.turnLocks[chatID] = l
	}
	return l
}

// runChatTurn is the single per-chat turn path behind HandleForChat
// and HandleStreamForChat, and the ONLY place the per-chat daily
// token budget is enforced — every channel that runs a chat's agent
// (oriond's Telegram turns, `orion daemon`, /retry) goes through
// here, so no path can silently skip the budget:
//
//   - pre-flight with projection (F-A5-6): the turn's input tokens
//     are estimated up front (retained history + the new input)
//     and Check refuses the turn when the chat cannot afford even
//     the input. The old Check(chatID, 0) only refused a chat
//     already over the limit, so one heavy turn could blow past it
//     by a large multiple.
//   - mid-turn abort (F-A5-6): after every provider response the
//     agent asks the Budget whether the turn's cumulative usage is
//     still within budget; an over-budget turn stops there instead
//     of running to max steps. The spend that already happened is
//     still recorded.
//   - post-turn: the turn's reported usage is recorded with its
//     prompt/completion/subagent split, per (provider, model)
//     (RecordUsageForModel), even when the turn errored — a failed
//     turn still consumed tokens, and the next turn must see the
//     spend. A Record error (the turn pushed the chat over the
//     limit) is logged, never swallowed silently: the turn already
//     ran, and the NEXT turn is the one that gets refused.
//     A nil Budget (hand-built Apps) means unbudgeted, exactly as
//     before.
//   - subagent sweep (F-A5-7): finished subagent tasks the parent
//     never fetched are charged here, at turn end, so
//     spawn-and-never-fetch spend is no longer invisible. Timed-out
//     tasks are charged too (F-A6-P2-7), and a task finishing after
//     the last turn is charged by the next turn's sweep or at
//     session close (closeChatAgents runs the same sweep).
//
// The returned stats carry the chat's provider/model plus the
// prompt/completion/subagent split, so the footer and /usage can
// show the turn honestly.
func (a *App) runChatTurn(ctx context.Context, chatID int64, input string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
	return a.runChatTurnWithImages(ctx, chatID, input, nil, emit)
}

// runChatTurnWithImages is runChatTurn with image input attached to
// the user turn (vision): the images ride the agent run for this
// turn only and are never persisted in the chat's message store
// (see Agent.RunWithEventHandlerWithImages).
func (a *App) runChatTurnWithImages(ctx context.Context, chatID int64, input string, images []model.ImageContent, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
	// Per-chat turn serialisation (cron): a scheduled turn fires
	// from the scheduler goroutine, outside PollStream's per-chat
	// lock, so this lock — taken here, for every chat turn — is
	// what keeps a scheduled turn from overlapping a user turn
	// in the same chat and interleaving that chat's history.
	// Different chats take different locks and still run in
	// parallel.
	// This lock covers per-chat turns only (F-C23): the
	// interactive CLI channel (`orion chat` / `orion run`) runs
	// on the global agent via App.Handle and never takes this
	// lock, so there is no CLI turn in a chat for it to block —
	// the lock serialises Telegram user turns against scheduled
	// turns, never the CLI channel. User-vs-user serialisation
	// is unchanged by this lock: PollStream already serialises
	// user turns per chat, so the lock only adds the
	// scheduled-vs-user case it was introduced for.
	lock := a.turnLock(chatID)
	lock.Lock()
	defer lock.Unlock()
	agent, err := a.AgentForChat(chatID)
	if err != nil {
		return "", nil, err
	}
	// Turn-start route: a pending --once is consumed here (the
	// agent is rebuilt on it for THIS turn), and the turn after
	// a --once turn restores the chat's effective route. The
	// agent's own turn-start restore (core) covers the failover
	// case in place; this covers the --once case, which only the
	// App can see.
	agent = a.applyTurnStartRoute(chatID, agent)
	if a.Budget != nil {
		if err := a.Budget.Check(chatID, a.projectedTurnTokens(chatID, input)); err != nil {
			return "", nil, err
		}
		// Mid-turn abort (F-A5-6): installed for this turn only
		// and cleared afterwards. The agent is per-chat and its
		// runs are serialised, so no other turn can observe the
		// hook; the closure reads a.Budget on every call, so a
		// budget replaced mid-turn is honoured at once.
		// Child spend (F-A6-P2-6): the subagent tools' sink
		// charges a fetched result to the app's GLOBAL agent,
		// never to this chat's agent — so the chat agent's own
		// usage (the u this check receives) is blind to child
		// spend during the turn. The check therefore adds the
		// chat's still-unrecorded child tokens (finished tasks
		// not yet swept — fetched or not) to u before asking
		// the Budget: a turn spawning children that blow the
		// budget is caught here, mid-turn, instead of only at
		// turn end. The added tokens are a CHECK input only —
		// nothing is recorded here, so there is no double
		// count with the turn-end sweep that charges them.
		agent.SetTurnUsageLimitCheck(func(u model.Usage) error {
			u.TotalTokens += a.pendingSubagentTokens(chatID)
			return a.Budget.Check(chatID, u.TotalTokens)
		})
		defer agent.SetTurnUsageLimitCheck(nil)
	}
	providerName, modelName := a.chatProviderInfo(chatID)
	// Vision gate (F-M2R-3): mark the run context with the chat's
	// provider/model, so ground/screenshot refuse the capture on
	// a model that cannot see images instead of returning an
	// image the model can never read.
	ctx = tools.WithVisionInfo(ctx, providerName, modelName)
	var stats *core.TurnStats
	wrapped := func(ev core.ProgressEvent) {
		if ev.Kind == core.EventTurnFinished {
			// The SERVING provider names the turn, not the
			// provider the turn started on: a turn that fell
			// back mid-turn is metered and reported under the
			// provider that answered it, with the per-provider
			// split in ProviderUsage. The pre-turn names are
			// the fallback when the event carries none
			// (a hand-built agent that never sets them).
			servingProvider, servingModel := ev.ServingProvider, ev.ServingModel
			if servingProvider == "" {
				servingProvider = providerName
			}
			if servingModel == "" {
				servingModel = modelName
			}
			stats = &core.TurnStats{
				Provider:         servingProvider,
				Model:            servingModel,
				Tokens:           ev.Usage.TotalTokens,
				PromptTokens:     ev.Usage.PromptTokens,
				CompletionTokens: ev.Usage.CompletionTokens,
				SubagentTokens:   ev.SubagentTokens,
				CacheHitTokens:   ev.Usage.CacheHit(),
				CacheReported:    ev.Usage.CacheReported,
				Steps:            ev.Step,
				Duration:         ev.Duration,
				MemoriesUsed:     ev.MemoriesUsed,
				ToolCalls:        ev.ToolCalls,
				GateDeniedCalls:  ev.GateDeniedCalls,
				FallbackHops:     ev.FallbackHops,
				ProviderUsage:    ev.ProviderUsage,
			}
		}
		if emit != nil {
			emit(ev)
		}
	}
	// F-M2R-32: tell the tools whether this chat's model can see
	// images, so ground refuses vision_only on a non-vision model
	// instead of returning an unusable image as a coverage mode.
	ctx = tools.WithVisionSupport(ctx, model.SupportsVision(providerName, modelName))
	reply, err := agent.RunWithEventHandlerWithImages(ctx, input, images, wrapped)
	// Post-turn sync: the agent may have restored its provider in
	// place at turn start (failover restore) or switched mid-turn;
	// sync the session's provider name from the live agent so the
	// next turn's /model display names the provider actually
	// serving. Only when this turn's agent is still the live one
	// — a concurrent switch must win. The model is synced only
	// for chats WITHOUT an override: an override chat's model is
	// the override's model (or its entry's), never the fallback
	// provider's — syncing it would show the override provider
	// with the wrong model.
	if cs, csErr := a.chatSessionFor(chatID); csErr == nil && cs != nil {
		a.chatsMu.Lock()
		if cs.agent == agent {
			cs.providerName = agent.ProviderName()
			if cs.override == "" {
				if m := agent.ModelName(); m != "" {
					cs.modelName = m
				}
			}
		}
		a.chatsMu.Unlock()
	}
	if stats != nil {
		// Subagent sweep (F-A5-7): charge finished tasks the
		// parent never fetched. Note the subagent tools' sink
		// charges fetched results to the app's GLOBAL agent,
		// never to this chat's agent — so the sweep is the first
		// time this chat's stats see these tokens, fetched or
		// not — and the per-chat swept set guarantees a task is
		// never charged twice across turns. The sweep runs only
		// when stats exist: a turn with no TurnFinished records
		// nothing, so its swept tokens must stay unswept for a
		// later turn instead of being marked and lost.
		if cs, csErr := a.chatSessionFor(chatID); csErr == nil {
			if swept := a.sweepFinishedSubagents(cs, chatID); swept > 0 {
				stats.Tokens += swept
				stats.SubagentTokens += swept
			}
		}
	}
	if stats != nil && a.Budget != nil {
		// The spend is recorded even for failed turns, and an
		// over-budget record is expected — but it is logged,
		// not swallowed: a silent over-limit record is how the
		// budget looked like a next-turn gate instead of a cap.
		if recErr := a.recordTurnUsage(chatID, stats); recErr != nil && a.Logger != nil {
			a.Logger.Printf("app: chat %d: turn recorded %d tokens, over daily budget: %v", chatID, stats.Tokens, recErr)
		}
		// Daily usage alert (F-A18-11): the turn's spend may
		// have crossed the threshold — check after recording,
		// so the alert sees the spend that just happened.
		// Never fatal: a failed delivery is logged inside
		// maybeAlertBudget and retried on the next record.
		a.maybeAlertBudget(ctx, chatID)
	}
	// A turn that succeeded after a provider switch says so: the
	// user sees which provider took over and why, instead of a
	// silent change of model under them. A failed turn carries
	// no notice — its error already names the failure.
	if stats != nil && err == nil && len(stats.FallbackHops) > 0 {
		reply = appendFallbackNotice(reply, stats.FallbackHops)
	}
	if a.Audit != nil {
		_, _ = a.Audit.Append("user", "agent_run", truncateRunes(security.RedactSecrets(input), 200))
	}
	return reply, stats, err
}

// recordTurnUsage records a turn's spend in the budget. A turn
// that switched providers records EACH leg under its own provider
// (the TurnFinished event's ProviderUsage split), so the daily
// per-model totals stay honest instead of lumping the whole turn
// on the provider that answered last — and the day total still
// sums exactly once, because the buckets partition the turn. The
// serving provider's bucket carries the model the turn served
// under and the turn's subagent tokens; a fallback leg's model is
// its config entry's model (or the serving model when the config
// cannot name one). A turn with no split records exactly as
// before: the total under the serving provider/model.
func (a *App) recordTurnUsage(chatID int64, stats *core.TurnStats) error {
	if a == nil || a.Budget == nil || stats == nil {
		return nil
	}
	if len(stats.ProviderUsage) <= 1 {
		return a.Budget.RecordUsageForModel(chatID, stats.Provider, stats.Model, model.Usage{
			PromptTokens:         stats.PromptTokens,
			CompletionTokens:     stats.CompletionTokens,
			TotalTokens:          stats.Tokens,
			PromptCacheHitTokens: stats.CacheHitTokens,
			CacheReported:        stats.CacheReported,
		}, stats.SubagentTokens)
	}
	var errs []error
	for provider, usage := range stats.ProviderUsage {
		modelName := stats.Model
		subagent := 0
		if strings.EqualFold(provider, stats.Provider) {
			subagent = stats.SubagentTokens
		} else if entry, ok := a.providerEntry(provider); ok {
			globalModel := ""
			if a.Config != nil {
				globalModel = a.Config.Model
			}
			modelName = providerModel(entry, globalModel)
		}
		if err := a.Budget.RecordUsageForModel(chatID, provider, modelName, usage, subagent); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// appendFallbackNotice appends a one-line note to a reply that
// succeeded after a provider switch: which provider took over and
// why, in plain words. The notice is part of the reply text (it
// rides the same send), so it cannot be lost by a missing event
// handler.
func appendFallbackNotice(reply string, hops []model.FallbackHop) string {
	if len(hops) == 0 {
		return reply
	}
	var b strings.Builder
	b.WriteString(reply)
	if !strings.HasSuffix(reply, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\n_(Provider fallback")
	for i, h := range hops {
		if i == 0 {
			fmt.Fprintf(&b, ": switched from %s to %s (%s)", h.From, h.To, fallbackClassPlain(h.Class))
		} else {
			fmt.Fprintf(&b, ", then %s to %s (%s)", h.From, h.To, fallbackClassPlain(h.Class))
		}
	}
	b.WriteString(".)_")
	return b.String()
}

// fallbackClassPlain renders a failover class as the plain-words
// reason a fallback notice shows the user.
func fallbackClassPlain(class model.FailoverClass) string {
	switch class {
	case model.FailoverBilling:
		return "credit exhausted"
	case model.FailoverAuth:
		return "authentication failed"
	case model.FailoverRateLimit:
		return "rate-limited"
	case model.FailoverModelNotFound:
		return "model unavailable"
	case model.FailoverTransient:
		return "temporary error"
	default:
		return "provider error"
	}
}

// projectedTurnTokens estimates the input tokens the turn's first
// provider call will carry: the retained history (content plus
// tool-call names and arguments, the same units the agent's
// request trim measures) plus the new user input. It is the
// pre-flight projection for the budget (F-A5-6): the system prompt
// and recall sections are NOT included — they are roughly constant
// and the mid-turn abort covers them — while history and input are
// the user-controlled parts that can jump by tens of thousands of
// tokens in one turn (the rc6 RSS-feed case). model.CountTokens is
// the calibrated estimator, deliberately conservative. A store read
// failure degrades to the input alone, never to refusing the turn
// on a missing estimate.
func (a *App) projectedTurnTokens(chatID int64, input string) int {
	n := model.CountTokens(input) + 4
	cs, err := a.chatSessionFor(chatID)
	if err != nil || cs == nil || cs.store == nil {
		return n
	}
	all, err := cs.store.RecentMessages(0)
	if err != nil {
		return n
	}
	for _, m := range all {
		n += model.CountTokens(m.Content) + 4
		for _, tc := range m.ToolCalls {
			n += model.CountTokens(tc.Name) + model.CountTokens(tc.Arguments) + 4
		}
	}
	return n
}

// pendingSubagentTokens returns the tokens of this chat's
// finished subagent tasks that have not been charged yet — i.e.
// finished tasks absent from the chat's swept set (F-A6-P2-6).
// Fetched and unfetched tasks both count: a fetched result's
// tokens went to the global agent's accumulator, never to this
// chat's agent or Budget, so until the turn-end sweep charges
// them they are invisible to the mid-turn check. The value is a
// check-time estimate only; the sweep is what charges them, once.
func (a *App) pendingSubagentTokens(chatID int64) int {
	if a == nil || a.Subagents == nil {
		return 0
	}
	cs, err := a.chatSessionFor(chatID)
	if err != nil || cs == nil {
		return 0
	}
	total := 0
	for _, ts := range a.Subagents.List() {
		if ts.ChatID != chatID {
			continue
		}
		if !isChargeableSubagentStatus(ts.Status) {
			continue
		}
		res, finished := a.Subagents.GetResult(ts.ID)
		if !finished {
			continue
		}
		a.chatsMu.Lock()
		swept := cs.sweptSubagents[ts.ID]
		a.chatsMu.Unlock()
		if swept {
			continue
		}
		total += res.Usage.Tokens
	}
	return total
}

// isChargeableSubagentStatus reports whether a finished task's
// tokens are chargeable at all (F-A6-P2-7): Done and Failed tasks
// spent tokens and are charged; timed-out tasks are charged too —
// a task that timed out still spent provider tokens before the
// deadline (when the executor returned at the deadline its usage
// is recorded on the result; when it ignored the deadline and
// returned late, the Manager records the late usage on the entry
// (F-A7-2a) and the sweep charges it once it has settled; when it
// never returns, the manager records zero — so charging a timed-out
// task never invents tokens, it charges exactly what the manager
// recorded). Running/pending tasks have no result yet and are never
// charged.
func isChargeableSubagentStatus(s subagent.Status) bool {
	return s == subagent.StatusDone || s == subagent.StatusFailed || s == subagent.StatusTimeout
}

// sweepFinishedSubagents charges this chat for subagent tasks that
// finished but were never fetched (F-A5-7). It returns the tokens
// charged. A task is charged once: tasks already in the chat's
// swept set are skipped, and a task still running contributes
// nothing. Timed-out tasks are charged on the same terms
// (F-A6-P2-7): they spent tokens before timing out, and skipping
// them made every timeout free.
//
// There is deliberately NO time window on this sweep (F-A7-2b):
// the swept set (check-and-mark under chatsMu) is the exactly-once
// guard, and a FinishedAt window was itself a leak — a task
// finishing mid-sweep, after the loop passed it but before the
// sweep recorded its end time, fell before every future window
// bound and was skipped by every later sweep forever. So the sweep
// charges every finished, chargeable, unswept task of this chat,
// whenever it finished.
//
// The one skip that remains is an UNSETTLED task (F-A7-2a): a
// timed-out or cancelled task whose executor is still running has
// zero recorded usage that may still grow when the executor
// returns. Charging it now would charge zero AND mark it swept,
// losing the late usage forever — so the sweep skips it without
// marking, and the next sweep (or session close) charges it once
// the Manager has recorded the late usage. An executor that never
// returns stays unsettled and uncharged; its spend is unknowable,
// and charging zero for it would change nothing.
//
// Attribution is by the task's owning chat (recorded at spawn):
// a task another chat spawned is skipped here even if it finished
// inside this chat's window, and is charged by its own chat's
// sweep instead. Unattributed tasks (chatID 0, e.g. CLI spawns)
// are never swept; the fetch path still charges them to whoever
// fetches the result.
func (a *App) sweepFinishedSubagents(cs *chatSession, chatID int64) int {
	if a == nil || a.Subagents == nil || cs == nil {
		return 0
	}
	a.chatsMu.Lock()
	if cs.sweptSubagents == nil {
		cs.sweptSubagents = map[string]bool{}
	}
	swept := cs.sweptSubagents
	a.chatsMu.Unlock()
	total := 0
	for _, ts := range a.Subagents.List() {
		if ts.ChatID != chatID {
			continue
		}
		res, finished := a.Subagents.GetResult(ts.ID)
		if !finished {
			continue
		}
		if !isChargeableSubagentStatus(ts.Status) {
			continue
		}
		if !res.UsageSettled {
			// The executor is still running past the timeout /
			// cancellation: its usage is not final yet. Skip
			// WITHOUT marking swept, so the next sweep charges
			// the late usage once it arrives (F-A7-2a).
			continue
		}
		a.chatsMu.Lock()
		already := swept[ts.ID]
		if !already {
			swept[ts.ID] = true
		}
		a.chatsMu.Unlock()
		if already {
			continue
		}
		// Mark the usage charged in the Manager too (F-A9-P2-8):
		// if this task is later evicted, its tokens must not be
		// folded into the dead-letter ledger on top of this charge.
		// MarkCharged only marks settled tasks, and this task is
		// settled (unsettled ones were skipped above).
		a.Subagents.MarkCharged(ts.ID)
		total += res.Usage.Tokens
	}
	return total
}

// sweepAndRecordSubagents sweeps one chat's finished subagent
// tasks and records the swept tokens to the chat's Budget
// (F-A6-P2-7, late finishers): it is the turn-end sweep's
// record step, factored out so session close can run the same
// sweep for tasks that finished after the last turn — a task
// finishing after the final turn has no next turn to sweep it,
// so without this its tokens would never be charged. With no
// Budget (or no finished tasks) it records nothing. A Record
// error (the sweep pushed the chat over its limit) is logged,
// never fatal: the spend already happened.
func (a *App) sweepAndRecordSubagents(chatID int64) int {
	if a == nil || a.Subagents == nil || a.Budget == nil {
		return 0
	}
	cs, err := a.chatSessionFor(chatID)
	if err != nil || cs == nil {
		return 0
	}
	swept := a.sweepFinishedSubagents(cs, chatID)
	if swept <= 0 {
		return 0
	}
	providerName, modelName := a.chatProviderInfo(chatID)
	if err := a.Budget.RecordUsageForModel(chatID, providerName, modelName, model.Usage{TotalTokens: swept}, swept); err != nil && a.Logger != nil {
		a.Logger.Printf("app: chat %d: session-close subagent sweep recorded %d tokens, over daily budget: %v", chatID, swept, err)
	}
	// Daily usage alert (F-A18-11): the swept spend may have
	// crossed the threshold. No turn context exists at session
	// close, so the notifier runs on context.Background();
	// delivery is best-effort and logged on failure, never a
	// reason to fail the shutdown.
	a.maybeAlertBudget(context.Background(), chatID)
	return swept
}

// HandleStreamForChat is HandleStream for one chat: the turn runs on
// the chat's agent, and the TurnStats carry the chat's provider and
// model (so the footer shows the model THAT chat is using). The
// per-chat daily token budget is enforced (see runChatTurn).
func (a *App) HandleStreamForChat(ctx context.Context, chatID int64, input string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
	return a.runChatTurn(ctx, chatID, input, emit)
}

// HandleStreamForChatWithImages is HandleStreamForChat with image
// input attached to the user turn (vision). This is the path a
// Telegram photo turn takes, via cmd/oriond's runTurnWithImages.
func (a *App) HandleStreamForChatWithImages(ctx context.Context, chatID int64, input string, images []model.ImageContent, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
	return a.runChatTurnWithImages(ctx, chatID, input, images, emit)
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
	// Route-aware: an alias, a "provider/model" pair (including
	// the SAME provider with a different model) or a bare
	// provider name. The route's model is what gets built and
	// persisted — a same-provider model switch lands here.
	provider, entry, key, err := a.resolveProvider(name)
	if err != nil {
		return err
	}
	cs, err := a.chatSessionFor(chatID)
	if err != nil {
		return err
	}
	// The model the route resolved: the entry's model when the
	// route named none. It is stored separately from the
	// provider, so a later read never has to re-parse a route.
	modelName := entry.Model
	// Persist first, then swap: if the write fails the chat keeps
	// running on its old provider instead of diverging from disk.
	if cs.dir != "" {
		if err := writeChatProviderOverride(cs.dir, key, modelName); err != nil {
			return fmt.Errorf("app: switch provider for chat %d: save override: %w", chatID, err)
		}
	}
	a.chatsMu.Lock()
	defer a.chatsMu.Unlock()
	cs.override = key
	cs.modelOverride = modelName
	cs.providerName = key
	cs.modelName = modelName
	// A manual switch cancels an active --once: the agent is no
	// longer on the once provider, so the next turn must not
	// "restore" away from this switch. A PENDING --once (set but
	// not yet applied) survives: it was set for the next turn.
	cs.onceActive = false
	if cs.agent == nil {
		// No agent yet: the override is recorded and the next
		// AgentForChat builds with it. Nothing else to do.
		return nil
	}
	if err := a.rebuildChatAgentLocked(cs, provider, key, modelName, key); err != nil {
		// The override is already persisted, so a restart picks
		// it up; the live chat keeps its old agent and the build
		// failure is reported honestly.
		return fmt.Errorf("app: switch provider for chat %d: build agent: %w", chatID, err)
	}
	return nil
}

// SwitchOnceForChat switches this chat's provider for the NEXT
// turn only (--once): the route is validated and built now (a bad
// route fails here, before any turn runs on it), but nothing is
// persisted and the override is untouched. runChatTurn applies the
// route at the start of the next turn — rebuilding the chat's
// agent on it — and clears it as soon as that turn begins, so the
// turn after runs on the override (or the daemon default) again.
// A --once set while a turn is in flight applies to the NEXT turn,
// not the running one: the running turn keeps its agent.
func (a *App) SwitchOnceForChat(chatID int64, route string) error {
	if a == nil || a.Config == nil {
		return errors.New("app: SwitchOnceForChat called on uninitialised App")
	}
	if _, _, _, err := a.resolveProvider(route); err != nil {
		return err
	}
	cs, err := a.chatSessionFor(chatID)
	if err != nil {
		return err
	}
	a.chatsMu.Lock()
	defer a.chatsMu.Unlock()
	cs.once = strings.TrimSpace(route)
	return nil
}

// ClearProviderOverrideForChat drops this chat's provider override
// (and any pending --once) and rebuilds the chat's agent on the
// daemon default for the next turn. The override file is removed
// (a remove failure is reported — the chat keeps its override
// rather than diverging from disk). /model reset.
func (a *App) ClearProviderOverrideForChat(chatID int64) error {
	cs, err := a.chatSessionFor(chatID)
	if err != nil {
		return err
	}
	a.chatsMu.Lock()
	defer a.chatsMu.Unlock()
	if cs.dir != "" {
		if err := os.Remove(filepath.Join(cs.dir, chatProviderOverrideFile)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("app: clear provider override for chat %d: %w", chatID, err)
		}
	}
	cs.override = ""
	cs.modelOverride = ""
	cs.once = ""
	cs.onceActive = false
	cs.providerName = a.defaultProviderName()
	cs.modelName = a.activeModel()
	if cs.agent == nil {
		return nil
	}
	// Rebuild on the daemon default.
	provider, providerName, modelName, err := a.buildChatProviderLocked(cs)
	if err != nil {
		return fmt.Errorf("app: clear provider override for chat %d: %w", chatID, err)
	}
	if err := a.rebuildChatAgentLocked(cs, provider, providerName, modelName, a.defaultProviderName()); err != nil {
		return fmt.Errorf("app: clear provider override for chat %d: build agent: %w", chatID, err)
	}
	return nil
}

// rebuildChatAgentOnRoute rebuilds cs's agent on a route (alias,
// provider/model or bare provider) WITHOUT persisting anything:
// the --once path. The agent's home stays the chat's effective
// provider, so the turn after the --once turn restores there —
// never to the once route. Callers must hold chatsMu.
func (a *App) rebuildChatAgentOnRoute(cs *chatSession, route string) error {
	provider, entry, key, err := a.resolveProvider(route)
	if err != nil {
		return err
	}
	home := cs.override
	if home == "" {
		home = a.defaultProviderName()
	}
	return a.rebuildChatAgentLocked(cs, provider, key, entry.Model, home)
}

// applyTurnStartRoute consumes a pending --once route, or restores
// the chat's effective route after a --once turn. It runs at the
// start of every chat turn, before the budget check, and returns
// the agent that runs the turn (the rebuilt one, or the one passed
// in). A route that fails to build leaves the chat on its current
// agent and logs the failure: a bad route must not wedge every
// later turn (it was validated at set time; a build failure now
// means the provider broke meanwhile, e.g. a revoked key). A
// failed restore keeps onceActive set, so the next turn retries
// it instead of silently keeping the once provider forever.
func (a *App) applyTurnStartRoute(chatID int64, agent *core.Agent) *core.Agent {
	cs, err := a.chatSessionFor(chatID)
	if err != nil || cs == nil {
		return agent
	}
	a.chatsMu.Lock()
	defer a.chatsMu.Unlock()
	once := cs.once
	cs.once = ""
	if once != "" {
		if err := a.rebuildChatAgentOnRoute(cs, once); err != nil {
			if a.Logger != nil {
				a.Logger.Printf("app: chat %d: --once route %q failed to build (%v); running on %s", chatID, once, err, cs.providerName)
			}
			return agent
		}
		cs.onceActive = true
		return cs.agent
	}
	if cs.onceActive {
		provider, providerName, modelName, err := a.buildChatProviderLocked(cs)
		if err != nil {
			if a.Logger != nil {
				a.Logger.Printf("app: chat %d: restore after --once failed (%v); staying on %s", chatID, err, cs.providerName)
			}
			return agent
		}
		home := cs.override
		if home == "" {
			home = a.defaultProviderName()
		}
		if err := a.rebuildChatAgentLocked(cs, provider, providerName, modelName, home); err != nil {
			if a.Logger != nil {
				a.Logger.Printf("app: chat %d: restore after --once failed to build (%v); staying on %s", chatID, err, cs.providerName)
			}
			return agent
		}
		cs.onceActive = false
		return cs.agent
	}
	return agent
}

// ExpensiveSwitchWarning reports whether switching from the
// current provider/model to the target route is expensive enough
// to warn about, and the warning text when it is. It fires only
// when BOTH prices are known AND the target's input price is more
// than 3x the current input price AND the target costs at least
// $1 per million input tokens — so a free local model switching
// to a cheap paid one stays silent, and a 2.3x jump stays silent,
// but free → opus warns. The text names the figures and the
// --confirm flag that accepts the switch. Manual switches only:
// automatic fallback never asks (configuring the chain IS the
// confirmation) and never warns mid-turn.
func (a *App) ExpensiveSwitchWarning(currentProvider, currentModel, targetRoute string) (bool, string) {
	if a == nil || a.Config == nil {
		return false, ""
	}
	targetProvider, targetModel, err := a.Config.ResolveRoute(targetRoute)
	if err != nil {
		return false, ""
	}
	tbl := cost.DefaultTable()
	curPrice, curOK := tbl.PriceFor(currentProvider, currentModel)
	tgtPrice, tgtOK := tbl.PriceFor(targetProvider, targetModel)
	if !curOK || !tgtOK {
		// Unknown price on either side: no warning (a warning
		// that cannot name a figure is noise, and blocking on
		// unknown pricing would punish new models).
		return false, ""
	}
	if tgtPrice.InputPerMillion < 1 || tgtPrice.InputPerMillion <= 3*curPrice.InputPerMillion {
		return false, ""
	}
	if curPrice.InputPerMillion == 0 {
		// The current model is free, so there is no multiplier to
		// quote (x/0 renders +Inf): name the target's absolute
		// price instead.
		return true, fmt.Sprintf("switching from %s/%s ($%.2f/M input) to %s/%s ($%.2f/M input) costs $%.2f/M input, up from free — reply with --confirm to accept",
			currentProvider, currentModel, curPrice.InputPerMillion,
			targetProvider, targetModel, tgtPrice.InputPerMillion,
			tgtPrice.InputPerMillion)
	}
	return true, fmt.Sprintf("switching from %s/%s ($%.2f/M input) to %s/%s ($%.2f/M input) costs %.1fx more per input token — reply with --confirm to accept",
		currentProvider, currentModel, curPrice.InputPerMillion,
		targetProvider, targetModel, tgtPrice.InputPerMillion,
		tgtPrice.InputPerMillion/curPrice.InputPerMillion)
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
	oldTr := cs.transcript
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
	// The old transcript is no longer the live one: release its
	// lock/fd (a later resume back to it reopens it). Without
	// this, every rotation leaked one fd + flock (Audit 5 W1
	// sync item 3).
	if oldTr != nil && oldTr != tr {
		_ = oldTr.Close()
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
	// Pre-flight budget refusal (F-A7-1): /compress runs outside
	// any turn, so without this a chat already at its daily limit
	// could run /compress repeatedly and the spend would only be
	// recorded, never refused. Mirror the turn path's pre-flight
	// (runChatTurn): Check the projected summarisation input
	// against the chat's Budget BEFORE the provider call. The
	// returned *BudgetExceededError unwraps to
	// budget.ErrBudgetExceeded. A nil Budget means unbudgeted,
	// exactly as for turns.
	if a.Budget != nil {
		if err := a.Budget.Check(chatID, projectedCompressTokens(cs.store, keepRecent, sessionMemory)); err != nil {
			return 0, 0, 0, err
		}
	}
	// Metering (F-A6-1): /compress runs outside any turn, so its
	// summarisation tokens never pass through TurnFinished — the
	// sink records them to this chat's Budget directly, at the
	// chat's provider/model. The pre-flight check above refuses
	// a chat that cannot afford the call; the sink still records
	// the spend of a call that does run, so the accounting
	// reflects reality even when the call pushes the chat over
	// its limit. A nil Budget means unbudgeted (the sink is
	// still passed: it no-ops, and compressWithOn skips nil
	// sinks only when the caller passes none). A Record error
	// (the compress pushed the chat over its limit) is logged,
	// never fatal: the summary already happened, and the NEXT
	// turn is the one that gets refused.
	before, after, summaryLen, summary, err := compressWithOn(ctx, cs.store, provider, keepRecent, sessionMemory, a.compressUsageSink(ctx, chatID))
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

// compressUsageSink returns the metering sink for one chat's
// /compress summarisation call (F-A6-1): it records the reported
// usage to the chat's Budget at the chat's provider/model. It is
// safe to pass when the app has no Budget (it no-ops) and safe to
// call from compressWithOn even when the summary is later refused
// by the what-not-to-save gate — the tokens were spent either way.
// The summarisation spend may cross the daily usage alert
// threshold (F-A18-11), so the sink checks for an alert after
// recording, on the turn's context.
func (a *App) compressUsageSink(ctx context.Context, chatID int64) func(model.Usage) {
	return func(u model.Usage) {
		if a == nil || a.Budget == nil {
			return
		}
		providerName, modelName := a.chatProviderInfo(chatID)
		if err := a.Budget.RecordUsageForModel(chatID, providerName, modelName, u, 0); err != nil && a.Logger != nil {
			a.Logger.Printf("app: chat %d: /compress recorded %d tokens, over daily budget: %v", chatID, u.TotalTokens, err)
		}
		a.maybeAlertBudget(ctx, chatID)
	}
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
		if st.DroppedWithoutSummary > 0 {
			fmt.Fprintf(&b, "\nCompaction: %d turn(s) dropped without summary; full turns are in the session transcript", st.DroppedWithoutSummary)
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
// agent's shutdown afterwards, in App.Close. Before each agent
// shuts down, its chat's finished subagent tasks are swept and
// recorded to the Budget (F-A6-P2-7, late finishers): App.Close
// runs after Subagents.Shutdown, so tasks cancelled by shutdown
// have finished by now, and a task that finished after the chat's
// last turn has no next turn to sweep it — session close is its
// last charging opportunity. A chat with no agent is swept too:
// spawning needs an agent, but the sweep must not depend on one.
func (a *App) closeChatAgents() error {
	if a == nil {
		return nil
	}
	a.chatsMu.Lock()
	ids := make([]int64, 0, len(a.chats))
	sessions := make([]*chatSession, 0, len(a.chats))
	for id, cs := range a.chats {
		ids = append(ids, id)
		sessions = append(sessions, cs)
	}
	a.chatsMu.Unlock()
	var errs []error
	for i, cs := range sessions {
		if cs == nil {
			continue
		}
		a.sweepAndRecordSubagents(ids[i])
		if cs.agent == nil {
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
		// Failover wiring: the rebuilt agent keeps the daemon's
		// chain (whose head already follows the new default —
		// SwitchProvider reordered it before this ran).
		a.applyFallbackWiring(agent)
		// Tool journal (F-A9-1): same journal as the global agent
		// (nil-safe — see AgentForChat).
		agent.SetToolJournal(a.toolJournal)
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
// v2 adds the optional model: a same-provider model switch
// persists next to the provider. v1 files (provider only) still
// read fine — Model unmarshals as "" and the entry's model applies.
type chatProviderOverride struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
}

// readChatProviderOverride reads the override for a chat directory.
// A missing file means "no override" (nil error, empty names); a
// corrupt file is an error the caller logs — but chatSessionFor
// treats it as no override rather than failing the chat, so a bad
// override file can never stop a chat from working (it just follows
// the default until the operator re-switches).
func readChatProviderOverride(dir string) (provider, modelName string, err error) {
	data, err := os.ReadFile(filepath.Join(dir, chatProviderOverrideFile))
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", nil
		}
		return "", "", err
	}
	var p chatProviderOverride
	if err := json.Unmarshal(data, &p); err != nil {
		return "", "", err
	}
	return strings.ToLower(strings.TrimSpace(p.Provider)), strings.TrimSpace(p.Model), nil
}

// writeChatProviderOverride persists a chat's provider override
// atomically (temp file 0600 + rename + dir fsync). A model of ""
// omits the field (v1 shape); a non-empty model writes it.
func writeChatProviderOverride(dir, name, modelName string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("app: create chat dir %q: %w", dir, err)
	}
	data, err := json.MarshalIndent(chatProviderOverride{Provider: name, Model: modelName}, "", "  ")
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
