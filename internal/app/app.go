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
		return nil, fmt.Errorf("app: config: %w", err)
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
	registry, err := tools.DefaultRegistryWithSandbox(cfg.Sandbox)
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

	mgr := subagent.NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, error) {
		return agent.Run(ctx, brief)
	})

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

	return &App{
		Config: cfg, Provider: provider, Tools: registry, Memory: mem,
		Agent: agent, Subagents: mgr, Skills: skillReg, Audit: audit, Logger: logger,
		StartedAt: time.Now(),
	}, nil
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
		return nil, fmt.Errorf("app: config: %w", err)
	}
	registry, err := tools.DefaultRegistryWithSandbox(cfg.Sandbox)
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
