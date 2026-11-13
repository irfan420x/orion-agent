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

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/security"
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
	Logger    *log.Logger
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

	// mu guards the Agent/Provider pair against a concurrent
	// SwitchProvider. agent()/provider() take the read lock;
	// SwitchProvider takes the write lock.
	mu sync.RWMutex
	// runMu guards runs; runWG counts live turns for Drain.
	runMu sync.Mutex
	runs  map[int64]runRegistration
	runWG sync.WaitGroup
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

	// Memory v2: typed memories (with a one-time migration of the
	// JSON facts), session-memory extraction, and the layers the
	// Telegram memory commands use. Best-effort by design — see
	// wireMemoryV2.
	typedStore, sessionLog, typedAdapter := wireMemoryV2(logger, cfg.DataDir, mem, agent)

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
	mgr := subagent.NewManagerWithExecutor(4, newSubagentExecutor(provider, cfg, typedAdapter, logger))

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

	skillReg := skills.NewRegistry()
	skillDir := filepath.Join(cfg.DataDir, "skills")
	if _, err := os.Stat(skillDir); err == nil {
		if _, err := skillReg.LoadDir(skillDir); err != nil {
			logger.Printf("skills: some skills failed to load: %v", err)
		}
	} else if !os.IsNotExist(err) {
		// A stat failure other than "missing" (e.g. permission denied)
		// used to be swallowed silently, leaving skills unloaded with
		// no trace; log it instead of pretending the dir is absent.
		logger.Printf("skills: cannot stat skills dir %s: %v", skillDir, err)
	}

	var audit *security.AuditLog
	audit, err = security.NewAuditLog(filepath.Join(cfg.DataDir, "audit.jsonl"))
	if err != nil {
		// Auditing must never block startup; log and continue unaudited.
		logger.Printf("audit: disabled (%v)", err)
	}

	app = &App{
		Config: cfg, Provider: provider, Tools: registry, Memory: mem,
		Agent: agent, Subagents: mgr, Skills: skillReg, Audit: audit, Logger: logger,
		StartedAt:  time.Now(),
		typedStore: typedStore, sessionLog: sessionLog, typedAdapter: typedAdapter,
	}
	return app, nil
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
	mem, err := memory.NewFileStore(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("app: memory: %w", err)
	}
	skillReg := skills.NewRegistry()
	skillDir := filepath.Join(cfg.DataDir, "skills")
	if _, err := os.Stat(skillDir); err == nil {
		if _, err := skillReg.LoadDir(skillDir); err != nil {
			logger.Printf("skills: some skills failed to load: %v", err)
		}
	} else if !os.IsNotExist(err) {
		logger.Printf("skills: cannot stat skills dir %s: %v", skillDir, err)
	}
	return &Local{Config: cfg, Tools: registry, Memory: mem, Skills: skillReg}, nil
}

// Close releases the local state's memory store. It is safe on a nil
// or partially-built Local.
func (l *Local) Close() error {
	if l == nil || l.Memory == nil {
		return nil
	}
	return l.Memory.Close()
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
	reply, err := a.agent().Run(ctx, input)
	if a.Audit != nil {
		_, _ = a.Audit.Append("user", "agent_run", truncateRunes(security.RedactSecrets(input), 200))
	}
	return reply, err
}

// Close shuts the app down: subagents first, then the agent (which saves
// and closes memory). It is safe on a zero or partially-built App: nil
// subagents/agent are skipped instead of panicking.
func (a *App) Close() error {
	if a == nil {
		return nil
	}
	if a.Subagents != nil {
		a.Subagents.Shutdown()
	}
	if a.Agent == nil {
		return nil
	}
	return a.Agent.Shutdown()
}
