// Package app wires Orion's modules into one running application:
// config -> provider -> tools -> memory -> agent -> subagents -> skills.
// Both cmd/orion (CLI) and cmd/oriond (daemon) build through here so the
// wiring exists exactly once.
package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/irfan/orion/internal/budget"
	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/hooks"
	"github.com/irfan/orion/internal/mcp"
	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/security"
	"github.com/irfan/orion/internal/session"
	"github.com/irfan/orion/internal/skills"
	"github.com/irfan/orion/internal/subagent"
	"github.com/irfan/orion/internal/tools"
)

// App is a fully-wired Orion instance.
type App struct {
	Config    *config.Config
	Provider  model.Provider
	Tools     *tools.Registry
	Memory    memory.Store
	Agent     *core.Agent
	Subagents *subagent.Manager
	Skills    *skills.Registry
	Audit     *security.AuditLog
	// Budget is the per-chat daily token budget (P2-O9), built from
	// config.PerChatDailyTokenBudget in Build; nil for hand-built
	// Apps, which are then unbudgeted.
	Budget *budget.Budget
	// toolJournal is the durable per-update record of completed
	// tool calls (F-A9-1), built from <dataDir>/tool-journal in
	// Build; nil for hand-built Apps and when the journal could
	// not be opened (the daemon logs and runs without one).
	// Every per-chat agent gets it at build time (see chats.go),
	// so a re-delivered turn replays recorded tool results
	// instead of re-executing them.
	toolJournal *core.ToolJournal
	// Hooks is the hook bus (rc9, audit P0 #6), loaded from
	// <dataDir>/hooks in Build; nil for hand-built Apps, which run
	// without hooks. The bus is shared by the global agent and
	// every per-chat agent (see chats.go).
	Hooks *hooks.Bus
	// MCP is the MCP client manager (rc9, audit P0 #7), started
	// from <dataDir>/mcp.json in Build; nil when no MCP servers
	// are configured (or for hand-built Apps). Its tools live on
	// the shared registry, so every agent sees them.
	MCP    *mcp.Manager
	Logger *log.Logger
	// StartedAt is when the app was built (zero for a hand-built App);
	// StatusLine reports uptime from it and omits the line when zero.
	StartedAt time.Time

	// typedStore and sessionLog are the Memory v2 layers, wired in
	// Build; nil for hand-built Apps (the Telegram memory commands
	// degrade gracefully without them). typedAdapter is the parent
	// agent's typed-memory adapter; subagent children get a
	// read-only view over it (see subagent.go), never the adapter.
	typedStore   *memory.TypedStore
	sessionLog   *memory.SessionLog
	typedAdapter *typedStoreAdapter
	// driftRoot is the repo root drift verification checks typed
	// memories against (found by wireMemoryV2 from the working
	// directory; "" means "process working directory"). It is kept
	// so SwitchProvider can re-apply it to a replacement agent.
	driftRoot string

	// mu guards the Agent/Provider pair against a concurrent
	// SwitchProvider. agent()/provider() take the read lock;
	// SwitchProvider takes the write lock.
	mu sync.RWMutex
	// runMu guards runs; runWG counts live turns for Drain.
	runMu sync.Mutex
	runs  map[int64]runRegistration
	runWG sync.WaitGroup
	// chats holds one chatSession per Telegram chat (P1-O3
	// isolation), guarded by chatsMu. chatsMu is always taken
	// before mu, never after.
	chatsMu sync.Mutex
	chats   map[int64]*chatSession
	// gate is the permission/confirmation gate (P1-O2), installed
	// by the daemon via SetPermissionGate; nil means ungated (the
	// CLI never installs one). gateMu guards it.
	gate   *permissions.Gate
	gateMu sync.RWMutex
	// budgetAlertNotifier sends the daily budget threshold alert
	// to the owner (F-A18-11): installed by the Telegram
	// entrypoints via SetBudgetAlertNotifier; nil means no alerts
	// (hand-built Apps, the CLI). It is set at startup before
	// polling starts and only read afterwards, like the gate's
	// prompter, so it needs no lock.
	budgetAlertNotifier BudgetAlertNotifier
	// budgetAlertNoOwnerLogged records, per chat, that the
	// permanent "no owner" budget-alert failure has already been
	// logged once in this process (F-A19-6). maybeAlertBudget
	// logs a no-owner failure only the first time for a chat,
	// then stays quiet — retrying (and logging) it on every
	// record until UTC midnight would spam the log for a failure
	// that cannot succeed until the daemon restarts with an owner
	// configured. It is deliberately NOT the persisted "alerted"
	// state: the owner was never notified, so a restart with an
	// owner configured must still be able to fire the alert.
	// Guarded by budgetAlertNoOwnerMu.
	budgetAlertNoOwnerMu     sync.Mutex
	budgetAlertNoOwnerLogged map[int64]bool
	// globalTranscript/globalSessionID are the CLI/global live
	// session transcript and id (see sessions.go); nil/"" until
	// wireGlobalTranscript runs in Build, or when the transcript
	// could not open (the CLI then runs on memory only).
	globalTranscript *session.Transcript
	globalSessionID  string
}

// Build loads config (default path when path == "") and wires every module.
// Provider selection comes from config only; no provider is hardcoded here.
func Build(path string) (*App, error) {
	logger := log.New(os.Stderr, "orion: ", log.LstdFlags)
	cfg, err := config.Load(path)
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	active, err := cfg.ActiveProvider()
	if err != nil {
		return nil, fmt.Errorf("app: active provider: %w", err)
	}
	provider, err := model.NewProvider(model.ProviderConfig{
		Name:    active.Name,
		APIKey:  active.APIKey,
		BaseURL: active.BaseURL,
		Model:   active.Model,
	})
	if err != nil {
		return nil, fmt.Errorf("app: provider %q: %w", active.Name, err)
	}
	registry, err := tools.DefaultRegistryWithSandboxAndTrustedEgress(cfg.Sandbox, cfg.TrustedEgressCIDRs)
	if err != nil {
		return nil, err
	}
	// Point the planning store at the data dir so todo/task state
	// persists per chat (rc9 B2 gap: without this the store stays
	// in-memory and planning state is lost on restart).
	if ps := registry.PlanningStore(); ps != nil {
		if err := ps.SetDataDir(cfg.DataDir); err != nil {
			return nil, fmt.Errorf("app: planning store: %w", err)
		}
	}
	mem, err := memory.NewFileStore(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("app: memory: %w", err)
	}
	agent, err := core.NewAgent(cfg, provider, registry, mem)
	if err != nil {
		_ = mem.Close()
		return nil, fmt.Errorf("app: agent: %w", err)
	}
	agent.SetLogger(logger)

	// Hook bus (rc9, audit P0 #6): loads executables from
	// <dataDir>/hooks ONLY — never from the repo working directory.
	// A missing hooks dir means "no hooks", not an error; bad hooks
	// are skipped and logged, never fatal. The bus is installed on
	// the global agent here and on every per-chat agent at build
	// time (see chats.go), and re-asserted in Handle so a provider
	// switch (which rebuilds the agent) cannot silently drop it.
	hookBus := hooks.NewBus(cfg.DataDir)
	for _, err := range hookBus.LoadErrors() {
		logger.Printf("hooks: %v", err)
	}
	if hookBus.HasHooks() {
		logger.Printf("hooks: %d PreToolUse, %d PostToolUse, %d Stop hook(s) loaded",
			hookBus.Count(hooks.EventPreToolUse), hookBus.Count(hooks.EventPostToolUse), hookBus.Count(hooks.EventStop))
	}
	agent.SetHooks(hookBus)

	// MCP client (rc9, audit P0 #7): starts the servers configured
	// in <dataDir>/mcp.json ONLY and registers their tools
	// (mcp__<server>__<tool>) on the registry, so the permission
	// gate, redaction and caps apply to them like any built-in
	// tool. See wireMCP: a missing config means "no MCP
	// configured", and a bad config or a failing server never
	// fails the build.
	mcpMgr := wireMCP(cfg.DataDir, registry, logger)

	// Memory v2: typed memories (with a one-time migration of the
	// JSON facts), session-memory extraction, and the layers the
	// Telegram memory commands use. Best-effort by design — see
	// wireMemoryV2.
	typedStore, sessionLog, typedAdapter, driftRoot := wireMemoryV2(logger, cfg.DataDir, mem, agent)

	// app is declared here so the usage-sink closure below can close
	// over it: it is assigned at the return, and the closure only runs
	// during turns (after Build returns), so it always sees the live
	// App.
	var app *App

	// Subagents: each spawn runs a FRESH child Agent (see
	// subagent.go) — the old executor called agent.Run on the
	// SHARED agent, which deadlocks (runMu is held for the whole
	// parent turn) and pollutes the parent's history. The child
	// shares only the provider (concurrent-safe) and the typed
	// memories (read-only view).
	mgr := subagent.NewManagerWithExecutorAndStore(4, newSubagentExecutor(provider, cfg, typedAdapter, logger, hookBus, registry.PlanningStore()), cfg.DataDir)

	// Model-visible subagent tools, registered on the PARENT
	// registry only (children get a registry without them, so no
	// recursion). The usage sink resolves the CURRENT agent on every
	// call: when the parent consumes a finished result, its tokens
	// are added to the parent turn's usage — and after a provider
	// switch rebuilds the agent, tokens accrue to the live agent,
	// not the orphaned one the tools were constructed with.
	// Registering after agent creation is safe: Registry.Register is
	// concurrency-safe, and the agent loop reads a.tools.Definitions()
	// fresh on every step, so tools registered here are visible to
	// subsequent runs.
	sink := tools.UsageSinkFunc(func(n int) {
		if app == nil {
			return
		}
		if ag := app.agent(); ag != nil {
			ag.AddSubagentUsage(n)
		}
	})
	subagentTools, err := tools.NewSubagentTools(mgr, sink)
	if err != nil {
		return nil, fmt.Errorf("app: subagent tools: %w", err)
	}
	for _, t := range subagentTools {
		if err := registry.Register(t); err != nil {
			return nil, fmt.Errorf("app: register subagent tool %q: %w", t.Name(), err)
		}
	}

	skillReg := loadSkills(cfg.DataDir, logger, knownToolNames(registry))
	// The skill tool is the model's invocation path for skills:
	// registered on the parent registry (children get a registry
	// without it — see newChildRegistry — so a subagent cannot
	// invoke skills, only the parent turn can). Registering after
	// agent creation is safe: the agent loop reads definitions
	// fresh on every step.
	if skillTool, err := tools.NewSkillTool(skillReg); err != nil {
		return nil, fmt.Errorf("app: skill tool: %w", err)
	} else if err := registry.Register(skillTool); err != nil {
		return nil, fmt.Errorf("app: register skill tool: %w", err)
	}
	agent.SetSkills(skillReg)

	var audit *security.AuditLog
	audit, err = security.NewAuditLog(filepath.Join(cfg.DataDir, "audit.jsonl"))
	if err != nil {
		// Auditing must never block startup; log and continue unaudited.
		logger.Printf("audit: disabled (%v)", err)
	}

	app = &App{
		Config: cfg, Provider: provider, Tools: registry, Memory: mem,
		Agent: agent, Subagents: mgr, Skills: skillReg, Audit: audit, Logger: logger,
		Budget:     budget.New(cfg.PerChatDailyTokenBudget),
		Hooks:      hookBus,
		MCP:        mcpMgr,
		StartedAt:  time.Now(),
		typedStore: typedStore, sessionLog: sessionLog, typedAdapter: typedAdapter, driftRoot: driftRoot,
	}
	// Budget persistence (Audit 5 F-A5-P2-19): without a file, a
	// daemon restart resets every chat's daily spend to zero. The
	// file holds today's per-chat totals (atomic write-temp+rename
	// in budget.SetFile); a stale day loads as empty.
	// Daily usage alert threshold (F-A18-11, rc24): an absolute
	// token count per chat per day; 0 disables the alert. It is
	// independent of the budget limit — it fires with an
	// unlimited budget exactly as with a finite one.
	app.Budget.SetAlertThreshold(cfg.DailyBudgetAlertThresholdTokens)
	if cfg.DailyBudgetAlertThresholdTokens > 0 {
		logger.Printf("budget: daily usage alert threshold %d tokens per chat (owner notified once per chat per day)", cfg.DailyBudgetAlertThresholdTokens)
	}
	if n, err := app.Budget.SetFile(filepath.Join(cfg.DataDir, "budget.json")); err != nil {
		logger.Printf("budget: persist disabled (%v); usage is in-memory only", err)
	} else if n > 0 {
		logger.Printf("budget: restored today's usage for %d chat(s) from %s", n, filepath.Join(cfg.DataDir, "budget.json"))
	}
	// Per-session transcript (audit P0 #4): the CLI/global agent
	// records every stored turn into the live session transcript
	// under <DataDir>/sessions/. Best-effort — see
	// wireGlobalTranscript.
	app.wireGlobalTranscript()
	// Durable tool journal (F-A9-1): per-update records of
	// completed tool calls, so a turn killed mid-execution and
	// re-delivered after a restart replays recorded results
	// instead of re-running its tools. Best-effort like the
	// audit log above: a journal that cannot be opened is
	// logged and the daemon runs without one (the pre-journal
	// behaviour), never a startup failure.
	if journal, err := core.NewToolJournal(filepath.Join(cfg.DataDir, "tool-journal")); err != nil {
		logger.Printf("tool journal: disabled (%v); re-delivered turns may re-run tools", err)
	} else {
		app.toolJournal = journal
		agent.SetToolJournal(journal)
	}
	return app, nil
}

// ToolJournal returns the app's durable per-update tool-call
// journal (F-A9-1), or nil when the app has none (hand-built
// Apps, or a journal that could not be opened at Build).
func (a *App) ToolJournal() *core.ToolJournal {
	if a == nil {
		return nil
	}
	return a.toolJournal
}

// SetToolJournal installs the durable per-update tool-call
// journal on the app and on its global agent. It is setup-time
// only (tests, and Build): per-chat agents pick it up at build
// time from the app (see AgentForChat), so a journal installed
// after a chat's agent was built applies to that chat's next
// agent rebuild, not to a run in flight.
func (a *App) SetToolJournal(j *core.ToolJournal) {
	if a == nil {
		return
	}
	a.toolJournal = j
	if a.Agent != nil {
		a.Agent.SetToolJournal(j)
	}
}

// Local is the provider-free slice of the app: config, tools, memory
// and skills. Commands that never call a model (config show, status,
// tools, memory, skills) build this instead of the full app, so they
// keep working even when the configured provider has no API key yet.
// This is the single local-wiring path: cmd/orion used to duplicate
// it with different error handling, and the two copies drifted.
type Local struct {
	Config *config.Config
	Tools  *tools.Registry
	Memory memory.Store
	Skills *skills.Registry
	// MCP is the MCP client manager started from <dataDir>/mcp.json
	// (rc9); nil when no MCP servers are configured. Closed by
	// Local.Close.
	MCP *mcp.Manager
}

// BuildLocal loads config (default path when path == "") and wires the
// provider-free modules, mirroring Build's config/tools/memory/skills
// steps (including its skills error handling: a partially broken
// skills dir is logged, not fatal, and a stat failure other than
// "missing" is logged instead of silently skipped).
func BuildLocal(path string) (*Local, error) {
	logger := log.New(os.Stderr, "orion: ", log.LstdFlags)
	cfg, err := config.Load(path)
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	registry, err := tools.DefaultRegistryWithSandboxAndTrustedEgress(cfg.Sandbox, cfg.TrustedEgressCIDRs)
	if err != nil {
		return nil, err
	}
	// Planning store persistence, as in Build: without this the
	// CLI's todo/task state stays in-memory and is lost on exit.
	if ps := registry.PlanningStore(); ps != nil {
		if err := ps.SetDataDir(cfg.DataDir); err != nil {
			return nil, fmt.Errorf("app: planning store: %w", err)
		}
	}
	mem, err := memory.NewFileStore(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("app: memory: %w", err)
	}
	skillReg := loadSkills(cfg.DataDir, logger, knownToolNames(registry))
	if skillTool, err := tools.NewSkillTool(skillReg); err != nil {
		return nil, fmt.Errorf("app: skill tool: %w", err)
	} else if err := registry.Register(skillTool); err != nil {
		return nil, fmt.Errorf("app: register skill tool: %w", err)
	}
	// MCP tools, so `orion tools` lists them: the local commands
	// never call a model, so the servers are started for listing
	// only. The manager is retained on the Local and closed by
	// Local.Close, so no server process is left behind.
	mcpMgr := wireMCP(cfg.DataDir, registry, logger)
	return &Local{Config: cfg, Tools: registry, Memory: mem, Skills: skillReg, MCP: mcpMgr}, nil
}

// wireMCP starts the MCP servers configured in <dataDir>/mcp.json
// and registers their tools on registry. It is shared by Build (the
// manager is retained on the App and closed at shutdown) and
// BuildLocal (listing only). It never returns an error and never
// fails the caller: a missing config means "no MCP configured"
// (nil manager), a malformed config is logged loudly and MCP stays
// off, a server that fails to start or list is recorded in
// StartErrors and skipped, and a registration failure is logged
// with the tools that did register kept. A bad mcp.json must never
// brick the daemon or a local command.
func wireMCP(dataDir string, registry *tools.Registry, logger *log.Logger) *mcp.Manager {
	m, err := mcp.StartManager(dataDir, logger)
	if err != nil {
		logger.Printf("mcp: disabled (%v)", err)
		return nil
	}
	if m == nil {
		return nil
	}
	for name, err := range m.StartErrors() {
		logger.Printf("mcp: server %q failed to start: %v", name, err)
	}
	if tools := m.Tools(); len(tools) > 0 {
		if err := registry.RegisterAll(tools); err != nil {
			// A duplicate name here is near-impossible (the
			// mcp__ prefix is distinct from built-ins and the
			// manager dedupes); log it and keep the tools that
			// did register rather than failing the build.
			logger.Printf("mcp: some tools failed to register: %v", err)
		}
	}
	return m
}

// knownToolNames returns the tool names registered on reg, plus
// "skill": the skill tool is registered right after loadSkills, so
// it is not listed yet, but a skill may always name it (the agent
// loop never blocks it). The daemon passes this into loadSkills so
// ## Tools validation (F-A17-14) knows the live tools — built-ins,
// subagent tools and MCP tools — instead of only the built-in set.
func knownToolNames(reg *tools.Registry) []string {
	names := []string{"skill"}
	if reg == nil {
		return names
	}
	for _, t := range reg.List() {
		names = append(names, t.Name())
	}
	return names
}

// ReloadSkills re-reads the skills directory without restarting
// the daemon (F-A17-15, wired to /skills reload): new/changed
// files load, removed files drop. The reload swaps the registry's
// contents in place, so the agent and the skill tool — which hold
// the same registry pointer — see the new skills on their next
// use, with no rebuild. A directory that cannot be read at all
// leaves the loaded skills untouched (see Registry.Reload).
func (a *App) ReloadSkills() (int, error) {
	if a == nil || a.Config == nil {
		return 0, errors.New("app: ReloadSkills called on uninitialised App")
	}
	if a.Skills == nil {
		return 0, errors.New("app: ReloadSkills: no skills registry")
	}
	return a.Skills.Reload(filepath.Join(a.Config.DataDir, "skills"))
}

// loadSkills loads skills from the user data dir ONLY
// (<dataDir>/skills) — never from the repo working directory, so a
// checked-out project can never smuggle skill instructions into the
// agent. A missing dir means "no skills", not an error. A partially
// broken dir is logged, not fatal: the skills that parsed still
// load. A stat failure other than "missing" (e.g. permission denied)
// is logged instead of silently skipped. knownTools extends the
// ## Tools validation set (F-A17-14): unknown names warn at load
// instead of silently never matching a tool.
func loadSkills(dataDir string, logger *log.Logger, knownTools []string) *skills.Registry {
	reg := skills.NewRegistry()
	reg.SetKnownTools(knownTools)
	skillDir := filepath.Join(dataDir, "skills")
	if _, err := os.Stat(skillDir); err == nil {
		if _, err := reg.LoadDir(skillDir); err != nil {
			logger.Printf("skills: some skills failed to load: %v", err)
		}
	} else if !os.IsNotExist(err) {
		logger.Printf("skills: cannot stat skills dir %s: %v", skillDir, err)
	}
	return reg
}

// Close releases the local state's memory store and stops any MCP
// servers started by BuildLocal. It is safe on a nil or
// partially-built Local.
func (l *Local) Close() error {
	if l == nil {
		return nil
	}
	var errs []error
	if l.MCP != nil {
		if err := l.MCP.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if l.Memory == nil {
		return errors.Join(errs...)
	}
	if err := l.Memory.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// truncateRunes shortens s to at most n runes, appending "..." when it
// truncates. Truncating by runes (not bytes) keeps the result valid
// UTF-8: a byte slice at an arbitrary offset can split a multi-byte
// rune, which the old detail[:200] byte truncation did.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

// Handle runs one user input through the agent and audits the turn.
// It is the shared handler for the CLI channel and the Telegram gateway:
// same agent, same memory, whichever channel the input came from.
//
// The audited detail is redacted BEFORE truncation: truncating first
// could cut a secret in half, leaving a fragment that no longer matches
// any secret pattern and would land in the audit log in the clear.
func (a *App) Handle(ctx context.Context, input string) (string, error) {
	if a == nil || a.agent() == nil {
		return "", errors.New("app: Handle called on uninitialised App (nil Agent)")
	}
	// Re-assert the skill registry on the current agent: a provider
	// switch rebuilds the agent (see SwitchProvider), and the
	// rebuild path must not silently drop skill discovery. This is
	// idempotent — SetSkills just installs the pointer.
	a.agent().SetSkills(a.Skills)
	// Same for the hook bus: a provider switch rebuilds the agent
	// (see SwitchProvider in commands.go, which does not re-apply
	// hooks itself), so re-assert it here on every turn — a switch
	// must never silently drop the hook policy.
	a.agent().SetHooks(a.Hooks)
	reply, err := a.agent().Run(ctx, input)
	if a.Audit != nil {
		_, _ = a.Audit.Append("user", "agent_run", truncateRunes(security.RedactSecrets(input), 200))
	}
	return reply, err
}

// NewPermissionGate builds the permission/confirmation gate
// (P1-O2) shared by every Telegram entrypoint (F-A6-3): oriond,
// `orion daemon` and `orion telegram poll` all call this one
// constructor instead of each wiring a gate (or, as before rc11,
// forgetting to). Rules persist under the data dir
// (permissions.json, 0600); a load error is logged and the gate
// starts with no rules rather than refusing startup. The prompter
// is the caller's: it sends the approval prompt on that
// entrypoint's gateway. The gate is installed on the app
// (SetPermissionGate) before it is returned, so per-chat agents
// built afterwards carry it, and the global agent gets it now.
// A nil prompter builds a gate that denies every gated call
// (fail closed). Callers must still install their callback
// handler on the gateway, or approvals can never be answered.
func (a *App) NewPermissionGate(prompter permissions.Prompter) (*permissions.Gate, error) {
	if a == nil || a.Config == nil {
		return nil, errors.New("app: NewPermissionGate called on uninitialised App (nil Config)")
	}
	rulePath := ""
	if a.Config.DataDir != "" {
		rulePath = filepath.Join(a.Config.DataDir, "permissions.json")
	}
	ruleStore := permissions.NewRuleStore(rulePath)
	if err := ruleStore.LoadError(); err != nil && a.Logger != nil {
		a.Logger.Printf("permissions: file unusable, starting with no rules: %v", err)
	}
	gate := permissions.NewGate(ruleStore, prompter)
	a.SetPermissionGate(gate)
	return gate, nil
}

// Close shuts the app down: subagents first, then the per-chat
// agents (P1-O3 — each saves its chat store and closes only that
// store), then the global agent (which saves and closes the shared
// memory), then the MCP servers. It is safe on a zero or
// partially-built App: nil subagents/agent are skipped instead of
// panicking.
func (a *App) Close() error {
	if a == nil {
		return nil
	}
	if a.Subagents != nil {
		a.Subagents.Shutdown()
	}
	var errs []error
	if err := a.closeChatAgents(); err != nil {
		errs = append(errs, err)
	}
	if a.Agent != nil {
		if err := a.Agent.Shutdown(); err != nil {
			errs = append(errs, err)
		}
	}
	// MCP servers last: a turn in flight (or a subagent) may still
	// be calling one while the agents shut down, and killing the
	// servers first would turn those calls into errors.
	if a.MCP != nil {
		if err := a.MCP.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
