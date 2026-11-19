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

	"github.com/irfan/orion/internal/budget"
	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/cost"
	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/security"
	"github.com/irfan/orion/internal/tools"
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
// captured). The handler is per-run (Agent.RunWithEventHandler), not
// installed on the shared agent: two chats running concurrently on
// one App each receive exactly their own run's events (Audit 4
// B P1-2 — the old SetEventHandler swap let a second chat steal the
// first chat's TurnFinished). The turn is audited exactly like Handle.
func (a *App) HandleStream(ctx context.Context, input string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
	agent := a.agent()
	if a == nil || agent == nil {
		return "", nil, errors.New("app: HandleStream called on uninitialised App (nil Agent)")
	}
	providerName := ""
	if p := a.provider(); p != nil {
		providerName = p.Name()
	}
	modelName := a.activeModel()
	// Vision gate (F-M2R-3): mark the run context with the
	// provider/model, so ground/screenshot refuse the capture on
	// a model that cannot see images instead of returning an
	// image the model can never read.
	ctx = tools.WithVisionInfo(ctx, providerName, modelName)
	var stats *core.TurnStats
	wrapped := func(ev core.ProgressEvent) {
		if ev.Kind == core.EventTurnFinished {
			// The SERVING provider names the turn, not the
			// provider it started on (see runChatTurn): a turn
			// that fell back mid-turn is reported under the
			// provider that answered it.
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
				// MemoriesUsed comes from the TurnFinished event
				// (the agent counts typed memories injected this
				// turn); the footer renders it when non-zero.
				MemoriesUsed:  ev.MemoriesUsed,
				FallbackHops:  ev.FallbackHops,
				ProviderUsage: ev.ProviderUsage,
			}
		}
		if emit != nil {
			emit(ev)
		}
	}
	reply, err := agent.RunWithEventHandler(ctx, input, wrapped)
	if stats != nil && err == nil && len(stats.FallbackHops) > 0 {
		reply = appendFallbackNotice(reply, stats.FallbackHops)
	}
	if a.Audit != nil {
		_, _ = a.Audit.Append("user", "agent_run", truncateRunes(security.RedactSecrets(input), 200))
	}
	return reply, stats, err
}

// providerNeedsAPIKey mirrors config's rule (openai/deepseek/anthropic/
// openrouter need a key, ollama does not). It lives here because config's
// providerNeedsAPIKey is unexported and SwitchProvider must name the
// missing key in its error before it builds anything.
func providerNeedsAPIKey(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "openai", "deepseek", "anthropic", "openrouter":
		return true
	default:
		return false
	}
}

// resolveProvider validates a provider route and builds the
// provider, without changing any state. It is the shared
// validation/construction step for SwitchProvider (daemon default)
// and SwitchProviderForChat (one chat's override): both must reject
// the same unknown/misconfigured providers, and neither may install
// a half-built provider. A route is an alias, a "provider/model"
// pair or a bare provider name — the same resolution /model
// accepts — so a switch, a chain entry and an alias can never
// disagree about what a name means. It returns the provider, the
// resolved config entry (with the route's model applied), and the
// canonical (lowercase) provider key.
func (a *App) resolveProvider(route string) (model.Provider, config.ProviderConfig, string, error) {
	if a == nil || a.Config == nil {
		return nil, config.ProviderConfig{}, "", errors.New("app: no config loaded")
	}
	name, modelName, err := a.Config.ResolveRoute(route)
	if err != nil {
		return nil, config.ProviderConfig{}, "", err
	}
	entry, key, found := a.lookupProviderEntry(name)
	if !found {
		return nil, config.ProviderConfig{}, "", fmt.Errorf("app: provider %q is not configured (supported providers: %s)", name, strings.Join(config.KnownProviderNames(), ", "))
	}
	return a.buildProviderEntry(entry, key, modelName)
}

// lookupProviderEntry resolves a provider name to its config entry
// case-insensitively (the map key is the canonical lowercase name,
// but never assume it).
func (a *App) lookupProviderEntry(name string) (config.ProviderConfig, string, bool) {
	if a == nil || a.Config == nil {
		return config.ProviderConfig{}, "", false
	}
	for k, p := range a.Config.Providers {
		if strings.EqualFold(k, name) {
			return p, k, true
		}
	}
	return config.ProviderConfig{}, "", false
}

// buildProviderEntry builds a provider from a config entry with an
// explicit model applied: a non-empty model overrides the entry's
// model (a route's model, or a chat override's model); an empty one
// keeps the entry's model with the global model as fallback. The
// returned entry carries the model that was actually built, so
// callers can persist or report it. The API-key check runs here,
// so every build path rejects a keyless paid provider the same way.
func (a *App) buildProviderEntry(entry config.ProviderConfig, key, modelName string) (model.Provider, config.ProviderConfig, string, error) {
	if strings.TrimSpace(entry.Name) == "" {
		entry.Name = key
	}
	if strings.TrimSpace(modelName) != "" {
		entry.Model = modelName
	} else if strings.TrimSpace(entry.Model) == "" && a.Config != nil {
		entry.Model = a.Config.Model
	}
	if providerNeedsAPIKey(key) && strings.TrimSpace(entry.APIKey) == "" {
		return nil, config.ProviderConfig{}, "", fmt.Errorf("app: provider %q requires an API key (set providers.%s.api_key or ORION_API_KEY)", key, key)
	}
	provider, err := model.NewProvider(model.ProviderConfig{
		Name:    entry.Name,
		APIKey:  entry.APIKey,
		BaseURL: entry.BaseURL,
		Model:   entry.Model,
	})
	if err != nil {
		return nil, config.ProviderConfig{}, "", fmt.Errorf("app: provider %q: %w", key, err)
	}
	return provider, entry, key, nil
}

// applyFallbackWiring installs the daemon's failover chain on an
// agent (nil-safe: a hand-built App has no chain). Every agent the
// App builds — global, per-chat, rebuilt — goes through this, so
// no agent can silently lose the chain on a rebuild.
func (a *App) applyFallbackWiring(agent *core.Agent) {
	if a == nil || agent == nil {
		return
	}
	agent.SetFallbackChain(a.fallbackChain)
}

// ModelDetail renders the /model detail block: the alias map and
// the fallback chain, one line each. It is daemon-wide (aliases
// and the chain do not vary per chat). Empty when there are no
// aliases and no chain beyond the default — a single-provider
// daemon shows nothing extra.
func (a *App) ModelDetail() string {
	if a == nil || a.Config == nil {
		return ""
	}
	var lines []string
	if len(a.Config.ProviderAliases) > 0 {
		aliases := make([]string, 0, len(a.Config.ProviderAliases))
		for alias, target := range a.Config.ProviderAliases {
			aliases = append(aliases, alias+" -> "+target)
		}
		sort.Strings(aliases)
		lines = append(lines, "Aliases: "+strings.Join(aliases, ", "))
	}
	if chain := a.Config.FallbackChain(); len(chain) > 1 {
		lines = append(lines, "Fallback chain: "+strings.Join(chain, " -> "))
	}
	return strings.Join(lines, "\n")
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
//
// Per-chat effect (P1-O3): chats WITH a provider override keep their
// provider; chats WITHOUT one follow the new default — their agents
// are rebuilt after the swap (rebuildChatAgentsForDefault). The
// daemon default is the default for chats, not a switch that
// reaches into every chat.
func (a *App) SwitchProvider(name string) error {
	if a == nil || a.Config == nil {
		return errors.New("app: SwitchProvider called on uninitialised App")
	}
	// Route-aware: a "provider/model" switch also changes the
	// model's model, and an alias resolves first.
	provider, entry, key, err := a.resolveProvider(name)
	if err != nil {
		return err
	}
	// Persist first, then swap: if the save fails the daemon keeps
	// running on the old provider instead of diverging from disk.
	a.mu.Lock()
	oldDefault := a.Config.DefaultProvider
	oldEntry := a.Config.Providers[key]
	a.Config.DefaultProvider = key
	if strings.TrimSpace(entry.Model) != "" {
		// A route with an explicit model updates the entry, so
		// the model survives a restart (the same-provider model
		// switch, persisted). A bare provider switch leaves the
		// entry's model untouched.
		if p, ok := a.Config.Providers[key]; ok {
			p.Model = entry.Model
			a.Config.Providers[key] = p
		}
	}
	if err := a.Config.Save(""); err != nil {
		a.Config.DefaultProvider = oldDefault
		a.Config.Providers[key] = oldEntry
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
	// Re-apply the Memory v2 wiring to the replacement agent
	// (typed source, session appender, drift root): NewAgent leaves
	// those fields nil, and without this a /model switch silently
	// drops typed recall, feedback injection, drift checking and
	// session extraction for every subsequent turn (Audit 4 B P1-1).
	a.applyMemoryV2Wiring(agent)
	// Re-assert skills and hooks on the replacement agent: NewAgent
	// leaves both unset, and without this a /model switch silently
	// drops skill discovery and hook enforcement for every
	// subsequent turn (same class as Audit 4 B P1-1 for Memory v2).
	agent.SetSkills(a.Skills)
	agent.SetHooks(a.Hooks)
	// The daemon default moved: the chain head follows it (the
	// cooldown table is kept — a provider that just failed stays
	// benched), and the new agent gets the chain like every agent
	// the App builds.
	if a.fallbackChain != nil {
		a.fallbackChain.SetOrder(a.Config.FallbackChain())
	}
	a.applyFallbackWiring(agent)
	agent.SetHomeProvider(key)
	a.Agent = agent
	a.Provider = provider
	if a.Subagents != nil {
		// Re-point the subagent executor at a child-agent factory
		// built on the NEW provider (children share the active
		// provider). The factory builds a fresh child per spawn,
		// so it never captures the old agent. The chain accessor
		// reads the live chain, so children follow a default
		// switch without another re-point.
		// The subagent tools' usage sink resolves the CURRENT
		// agent on every call (see Build), so no re-pointing is
		// needed here: tokens from consumed results accrue to
		// this new agent automatically.
		a.Subagents.SetExecutor(newSubagentExecutor(provider, a.Config, a.typedAdapter, a.Logger, a.Hooks, a.planningStore, a.captureBudget, func() *model.FailoverChain { return a.fallbackChain }))
	}
	a.mu.Unlock()
	// Chats without an override follow the new default; chats with
	// one keep their provider. This runs after mu is released
	// (lock order: chatsMu before mu, never the reverse).
	_ = a.rebuildChatAgentsForDefault()
	return nil
}

// BudgetInfoForChat returns the per-chat daily token budget for
// /budget: the configured limit (<= 0 means unlimited) and the
// tokens this chat has used today (UTC day). A nil Budget
// (hand-built Apps) reports (0, 0): unbudgeted, which behaves as
// unlimited — turns are never refused — so the /budget reply
// honestly says "unlimited" instead of inventing a limit.
func (a *App) BudgetInfoForChat(chatID int64) (limit int, usedToday int) {
	if a == nil || a.Budget == nil {
		return 0, 0
	}
	return a.Budget.Limit(), a.Budget.DayUsage(chatID).Total
}

// SetDailyBudget changes the per-chat daily token limit (0 =
// unlimited, otherwise 1000–10000000, the range config validation
// enforces) for /budget, and persists it: the config is saved
// atomically (0600, like SwitchProvider) BEFORE the live budget
// is touched, so a save failure leaves the daemon running on the
// old limit instead of diverging from disk. The new limit takes
// effect immediately for every chat (the Budget is shared) and
// survives restarts, because the next Build reads it from the
// config. Today's recorded usage is untouched: lowering the limit
// below today's spend makes the next Check fail until the UTC day
// rolls over, it does not erase the usage.
func (a *App) SetDailyBudget(limit int) error {
	if a == nil || a.Config == nil {
		return errors.New("app: SetDailyBudget called on uninitialised App")
	}
	if limit != 0 && (limit < 1000 || limit > 10000000) {
		return fmt.Errorf("app: daily token budget must be 0 (unlimited) or between 1000 and 10000000, got %d", limit)
	}
	a.mu.Lock()
	old := a.Config.PerChatDailyTokenBudget
	a.Config.PerChatDailyTokenBudget = limit
	if err := a.Config.Save(""); err != nil {
		a.Config.PerChatDailyTokenBudget = old
		a.mu.Unlock()
		return fmt.Errorf("app: set daily budget: save config: %w", err)
	}
	if a.Budget != nil {
		a.Budget.SetLimit(limit)
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

// UsageLineForChat renders the per-chat, per-day usage block for
// /usage: tokens consumed TODAY by this chat (with the
// prompt/completion/subagent split), the estimated USD cost of
// today's spend at this chat's provider/model, and the daily token
// budget (limit, remaining, UTC-midnight reset). It is the
// per-chat view; the daemon-wide session totals come from the
// messaging session store and are rendered by the /usage handler
// alongside this block.
//
// The USD figure is an ESTIMATE, labelled as such with the price
// table's version and effective date: it is the standard on-demand
// list price for the tokens' split, not the bill (peak/off-peak
// rates and discounts are invisible to Orion). Prompt tokens the
// provider reported serving from cache price at the cache-hit rate
// (the day's cached total rides on the split); a day with no cache
// report prices the whole prompt at the cache-miss rate, as before.
// When the chat's model has no price in the table the cost is reported
// as unknown — never $0. A nil Budget (hand-built Apps) reports
// the day as untracked instead of inventing numbers. It never
// returns an error: a missing piece is shown as "unknown" rather
// than failing the /usage command.
func (a *App) UsageLineForChat(chatID int64) string {
	if a == nil {
		return "Usage today (this chat): unknown (app not initialised)"
	}
	providerName, modelName := a.chatProviderInfo(chatID)
	if providerName == "" {
		providerName = "unknown"
	}
	if modelName == "" {
		modelName = "unknown"
	}
	if a.Budget == nil {
		return fmt.Sprintf("Usage today (this chat): not tracked (no budget configured)\nProvider: %s\nModel: %s", providerName, modelName)
	}
	du := a.Budget.DayUsage(chatID)
	var b strings.Builder
	// The prompt part names the cached tokens when the day reported
	// any — "3,000 prompt, 2,900 cached" — so /usage shows the
	// saving; with no cache report the line renders exactly as
	// before, never "0 cached".
	promptPart := fmt.Sprintf("%s prompt", formatThousands(du.Prompt))
	if du.CacheHit > 0 {
		promptPart = fmt.Sprintf("%s prompt, %s cached", formatThousands(du.Prompt), formatThousands(du.CacheHit))
	}
	fmt.Fprintf(&b, "Tokens today (this chat): %s (%s + %s completion, incl. %s subagent)\n",
		formatThousands(du.Total), promptPart, formatThousands(du.Completion), formatThousands(du.Subagent))
	tbl := cost.DefaultTable()
	// Per-model pricing (F-A5-P2-18): the budget keeps the day's
	// split per (provider, model), and each model's tokens are
	// priced at that model's own rate — a morning on
	// deepseek-chat is never repriced at an afternoon switch to
	// claude-opus-4. Records made without a model (the "" bucket)
	// are priced at the chat's current model, as before. When
	// any bucket has no price in the table the whole cost is
	// unknown, never a partial $ figure.
	byModel := a.Budget.DayUsageByModel(chatID)
	if usd, label, ok := estimateDayCost(tbl, byModel, providerName, modelName); ok {
		fmt.Fprintf(&b, "Est. cost today (this chat): %s (est., %s, price table v%s effective %s)\n",
			cost.FormatUSD(usd), label, tbl.Version, tbl.Effective)
	} else {
		fmt.Fprintf(&b, "Est. cost today (this chat): unknown (no price for %s/%s in price table v%s effective %s)\n",
			providerName, modelName, tbl.Version, tbl.Effective)
	}
	if limit := a.Budget.Limit(); limit <= 0 {
		b.WriteString("Daily token budget: unlimited")
	} else {
		fmt.Fprintf(&b, "Daily token budget: %s tokens (%s remaining, resets %s)",
			formatThousands(limit), formatThousands(a.Budget.Remaining(chatID)), a.Budget.ResetAt().Format("2006-01-02 15:04 UTC"))
		if a.isOwnerChat(chatID) {
			// The remaining count is not a refusal countdown
			// for the owner's own chat (warning-only): say so,
			// or /usage would imply his turns stop at zero.
			b.WriteString(" — warning only, your turns are never refused")
		}
	}
	return b.String()
}

// estimateDayCost prices one chat's day split per (provider,
// model) at each model's own rate (F-A5-P2-18). It returns the
// summed USD estimate, the label for the cost line, and false when
// any bucket has no price (the caller then reports "unknown").
// The "" bucket (records made without a model) is priced at the
// chat's current provider/model. With a single bucket the label is
// that bucket's model, matching the old single-model line; with
// several it says "priced per model". An empty split prices $0 at
// the current model.
func estimateDayCost(tbl *cost.Table, byModel map[budget.ModelKey]budget.DayUsage, providerName, modelName string) (float64, string, bool) {
	if len(byModel) == 0 {
		usd, ok := tbl.EstimateTotals(providerName, modelName, 0, 0, 0)
		return usd, modelName, ok
	}
	total := 0.0
	label := ""
	for key, du := range byModel {
		prov, mod := key.Provider, key.Model
		if prov == "" && mod == "" {
			prov, mod = providerName, modelName
		}
		// Each bucket prices its own split at its own model's rate,
		// cache hits included: the day's cached prompt tokens ride
		// on the DayUsage, and Estimate prices them at the
		// cache-hit rate — a cached day is no longer priced as if
		// the whole prompt were a cache miss. A bucket with no
		// cache report (CacheHit 0) prices the whole prompt at
		// the input rate, exactly as before.
		usd, ok := tbl.Estimate(prov, mod, model.Usage{
			PromptTokens:         du.Prompt,
			CompletionTokens:     du.Completion,
			TotalTokens:          du.Total,
			PromptCacheHitTokens: du.CacheHit,
			CacheReported:        du.CacheHit > 0,
		}, du.Subagent)
		if !ok {
			return 0, "", false
		}
		total += usd
		if len(byModel) == 1 {
			label = mod
		}
	}
	if len(byModel) > 1 {
		label = "priced per model"
	}
	return total, label, true
}

// formatThousands renders n with "," thousands separators ("1240" ->
// "1,240").
func formatThousands(n int) string {
	if n < 0 {
		return "-" + formatThousands(-n)
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	rem := len(s) % 3
	if rem > 0 {
		b.WriteString(s[:rem])
	}
	for i := rem; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
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
// wiped by starting a new chat) and persisted, and the live
// session rotates: a fresh session id is recorded in
// sessions/current.json and the global agent records further turns
// into the new transcript. The old transcript stays on disk for
// listing/resume. The caller resets the per-chat session
// bookkeeping (undo stack, last input). A rotation failure is
// logged and the old transcript stays live — /new's primary job
// has already succeeded, and no turn is ever lost to it.
func (a *App) NewSession() error {
	if a == nil || a.Memory == nil {
		return errors.New("app: NewSession called on uninitialised App (nil Memory)")
	}
	if err := a.Memory.ClearMessages(); err != nil {
		return err
	}
	if a.Config == nil || a.Config.DataDir == "" {
		return nil
	}
	tr, id, err := rotateTranscript(a.Config.DataDir)
	if err != nil {
		if a.Logger != nil {
			a.Logger.Printf("app: rotate global session failed (%v); keeping live session %s", err, a.globalSessionID)
		}
		return nil
	}
	oldTr := a.globalTranscript
	a.globalTranscript = tr
	a.globalSessionID = id
	if ag := a.agent(); ag != nil {
		// A new session starts with a clean compaction slate.
		ag.SetTranscript(tr)
		ag.ResetCompactionBreaker()
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

// UndoLastTurns removes the last n user turns from memory — each turn
// is a user message plus everything that followed it up to the next
// user message — and returns the undone user texts in chronological
// order (oldest undone turn first). n < 1 and an empty history are
// errors; nothing is removed when there is nothing to undo.
func (a *App) UndoLastTurns(n int) ([]string, error) {
	if a == nil || a.Memory == nil {
		return nil, errors.New("app: UndoLastTurns called on uninitialised App (nil Memory)")
	}
	return undoLastTurnsOn(a.Memory, n)
}

// undoLastTurnsOn removes the last n user turns from store — each
// turn is a user message plus everything that followed it up to the
// next user message — and returns the undone user texts in
// chronological order (oldest undone turn first). n < 1 and an empty
// history are errors; nothing is removed when there is nothing to
// undo. It is the store-level primitive behind UndoLastTurns (the
// global store, for the CLI) and UndoLastTurnsForChat (one chat's
// store, for Telegram): the logic is identical, only the store
// differs, so there is exactly one implementation.
func undoLastTurnsOn(store memory.Store, n int) ([]string, error) {
	if n < 1 {
		return nil, fmt.Errorf("app: undo count must be at least 1, got %d", n)
	}
	all, err := store.RecentMessages(0)
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
	if err := store.ReplaceMessages(kept); err != nil {
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
	// Pre-flight budget refusal (F-A7-1): the CLI/global
	// /compress runs outside any turn, so without this it could
	// spend without limit even when the budget is exhausted —
	// the side-channel sink below only accounts the spend after
	// the fact. Mirror the turn path's pre-flight (runChatTurn):
	// Check the projected summarisation input against the
	// Budget BEFORE the provider call. The CLI has no chat, so
	// its compress spend is checked against the unattributed
	// bucket (cliCompressChatID) — the same chatID 0 CLI
	// subagent spawns use. A nil Budget means unbudgeted.
	if a.Budget != nil {
		if err := a.Budget.Check(cliCompressChatID, projectedCompressTokens(a.Memory, keepRecent, sessionMemory)); err != nil {
			return 0, 0, 0, err
		}
	}
	// Metering (F-A6-1, F-A8-1): the CLI/global /compress runs
	// outside any turn, so its tokens never pass through
	// TurnFinished either — the sink reports them to the global
	// agent's side-channel usage, which the next turn folds
	// into its usage (the F-A5-5 side-channel mechanism), and
	// the CLI budget sink records them to the unattributed
	// bucket (cliCompressChatID) the pre-flight check above
	// reads, so repeated CLI compresses past the limit are
	// refused instead of spending without limit. Recording to
	// the Budget is not double-counting: the CLI/global turn
	// path never touches the Budget, so the side-channel
	// folding carries the tokens into turn stats only, never
	// into a second Budget record. A Record error (the
	// compress pushed bucket 0 over its limit) is logged by
	// the sink, never fatal: the summary already happened, and
	// the NEXT /compress is the one that gets refused.
	var sink func(model.Usage)
	if ag := a.agent(); ag != nil {
		sink = ag.AddSideChannelUsage
	}
	before, after, summaryLen, summary, err := compressWithOn(ctx, a.Memory, a.provider(), keepRecent, sessionMemory, sink, a.cliCompressUsageSink())
	if err == nil && summaryLen > 0 {
		// A successful manual /compress clears the auto-compaction
		// circuit breaker: the operator just proved summarisation
		// works, so auto-compaction may try again.
		if ag := a.agent(); ag != nil {
			ag.ResetCompactionBreaker()
		}
		// The summary is preserved in the live session transcript
		// as well as the conversation-summary fact, so the
		// transcript keeps the full compaction record.
		if a.globalTranscript != nil {
			if err := a.globalTranscript.AppendSummary(summary); err != nil && a.Logger != nil {
				a.Logger.Printf("app: compress: record summary in transcript: %v", err)
			}
		}
	}
	return before, after, summaryLen, err
}

// cliCompressChatID is the budget bucket the CLI/global /compress
// is checked against AND recorded to (F-A7-1, F-A8-1): the CLI has
// no chat, so its compress spend is checked against and recorded
// to the unattributed bucket, chatID 0 — the same bucket CLI
// subagent spawns use (internal/subagent: Spawn starts
// unattributed tasks at chatID 0). Checking without recording
// (the rc12 state) left the gate dead: the bucket stayed empty
// no matter how much CLI /compress spent. cliCompressUsageSink
// is the record half; the pre-flight Check in compressWith is
// the refusal half.
const cliCompressChatID = 0

// cliCompressUsageSink returns the metering sink for the CLI/global
// /compress summarisation call (F-A8-1): it records the reported
// usage to the unattributed bucket (cliCompressChatID) at the
// daemon default provider/model, mirroring compressUsageSink for
// per-chat /compress. It does NOT go through chatProviderInfo:
// the CLI has no chat session, and resolving one for chatID 0
// would create a phantom chat session. It is safe to pass when
// the app has no Budget (it no-ops) and safe to call from
// compressWithOn even when the summary is later refused by a
// gate — the tokens were spent either way. A Record error (the
// compress pushed bucket 0 over its limit) is logged, never
// fatal: the summary already happened, and the NEXT /compress
// is the one that gets refused.
func (a *App) cliCompressUsageSink() func(model.Usage) {
	return func(u model.Usage) {
		if a == nil || a.Budget == nil {
			return
		}
		if err := a.Budget.RecordUsageForModel(cliCompressChatID, a.providerName(), a.activeModel(), u, 0); err != nil && a.Logger != nil {
			a.Logger.Printf("app: CLI /compress recorded %d tokens, over daily budget: %v", u.TotalTokens, err)
		}
	}
}

// projectedCompressTokens estimates the tokens the /compress
// summarisation call will cost in total: the bounded old prefix
// actually sent to the provider (same sizing as compressWithOn —
// boundedCompressPrefix, the prompt overhead and the gated
// session-memory section), in model.CountTokens units, PLUS the
// completion allowance compressMaxSummaryTokens — the most the
// capped summary can add. It is the pre-flight projection for
// the budget (F-A7-1, F-A8-P2-3), mirroring projectedTurnTokens
// for turns: a chat that cannot afford even the summarisation
// input is refused before the provider call, instead of spending
// first and being refused only on the next turn. Counting input
// only (the rc12 shape) admitted a call at 800/1000 whose
// provider then reported 5000 tokens — 5.8x the limit from one
// call — because the completion was never projected and never
// capped. A store read failure degrades to 0 — compressWithOn
// fails on the same read with its own error, and no provider
// call happens either way. When there is nothing old enough to
// compress the projection is 0: no provider call will happen.
func projectedCompressTokens(store memory.Store, keepRecent int, sessionMemory string) int {
	if keepRecent < 1 {
		keepRecent = compressRecentWindow
	}
	if store == nil {
		return 0
	}
	all, err := store.RecentMessages(0)
	if err != nil || len(all) <= keepRecent {
		return 0
	}
	old := all[:len(all)-keepRecent]
	// Session memory is gated and capped exactly as
	// compressWithOn gates it (redaction only shrinks, so the
	// unredacted estimate is conservative).
	sessionTokens := 0
	if strings.TrimSpace(sessionMemory) != "" {
		if kept, ok := memory.FilterExplicitSave(sessionMemory); ok {
			sessionTokens = model.CountTokens(truncateCompressRunesToTokens(strings.TrimSpace(kept), compressSessionMemoryMaxTokens)) + 4
		}
	}
	toSummarise := boundedCompressPrefix(old, compressMaxInputTokens-compressPromptOverheadTokens-sessionTokens)
	n := compressPromptOverheadTokens + sessionTokens
	for _, m := range toSummarise {
		n += compressMessageTokens(m)
	}
	// Plus the completion allowance: the summary the provider
	// is capped at generating (F-A8-P2-3 — see
	// compressMaxSummaryTokens). The worst case this call can
	// cost is input + capped completion; projecting only the
	// input let one admitted call overshoot the limit several
	// times over.
	return n + compressMaxSummaryTokens
}

// compressWithOn summarises store's conversation prefix older than
// the recent window with a real model call (provider) and drops it:
// the summary is stored as the "conversation-summary" fact (so
// fact-aware recall still surfaces it) and only the recent window
// stays in history, and the summary text is returned so callers
// can also record it in the session transcript and clear the
// auto-compaction circuit breaker. It is the store-level primitive
// behind Compress (the global store, for the CLI) and
// CompressForChat (one chat's store, for Telegram). When nothing is
// old enough (history fits the window) it returns before == after
// with summaryLen == 0 and no error. A provider failure leaves
// history untouched.
//
// Metering (F-A5-5, F-A6-1): the summarisation call's reported usage is
// handed to each recordUsage sink (nil sinks are skipped), so the
// caller can charge it — /compress runs outside any turn, so its
// tokens never pass through TurnFinished and must be recorded
// directly. The per-chat caller passes a sink that records to the
// chat's Budget; the CLI caller passes a sink that reports to the
// global agent's side-channel usage, folded into the next turn.
// Every production caller passes a sink: a /compress with no sink
// is an unmetered summarisation call, the F-A6-1 hole.
// compressMaxInputTokens bounds one /compress summarisation
// call's TOTAL provider-bound input (Audit 6 F-A6-P2-2/P2-3,
// same shape W1 fixed in internal/core/compact.go): the prompt
// header, the system message, the session-memory section and the
// summarised messages together fit 16,000 estimated tokens.
// Before this the cap was per-message only in the auto-compaction
// path and absent here entirely — one giant message was handed
// to the provider whole. Keep in sync with core's
// maxCompactionInputTokens: the two paths summarise the same
// history for the same provider, so they must agree on what
// "fits" means.
const compressMaxInputTokens = 16000

// compressPromptOverheadTokens reserves room inside
// compressMaxInputTokens for the summariser prompt header, the
// system message and per-message role framing, so the TOTAL
// provider-bound input — not just the message bodies — stays
// within the cap (mirrors core's summariserPromptOverheadTokens).
const compressPromptOverheadTokens = 128

// compressMaxSummaryTokens caps the /compress summarisation
// call's completion (F-A8-P2-3): the ChatRequest in
// compressWithOn carries MaxTokens = 800, the same cap
// auto-compaction uses (internal/core/compact.go), so one
// admitted call cannot generate an unbounded summary. It is
// also the completion allowance projectedCompressTokens counts
// in the pre-flight budget check, so admission only happens
// when input + the full capped completion fit the remaining
// budget — a provider that ignores MaxTokens still cannot
// overshoot several-fold from an admitted call, because the
// call was only admitted when the worst case fit.
const compressMaxSummaryTokens = 800

// compressSessionMemoryMaxTokens bounds the session-memory
// section of the /compress prompt: session memory is
// model-derived text of unbounded length, and without its own
// bound it could eat the whole input cap (or blow past it).
// 2,000 tokens keeps the durable notes while leaving the bulk
// of the cap for the conversation itself.
const compressSessionMemoryMaxTokens = 2000

// compressMessageTokens estimates one message's token cost the
// way the agent loop does (content plus tool-call names and
// arguments, plus a small per-message framing allowance), in
// model.CountTokens units — the same units providers bill in.
// Mirrors core.messageTokens; keep the two in sync.
func compressMessageTokens(m model.Message) int {
	n := model.CountTokens(m.Content) + 4
	for _, tc := range m.ToolCalls {
		n += model.CountTokens(tc.Name) + model.CountTokens(tc.Arguments) + 4
	}
	return n
}

// truncateCompressRunesToTokens returns the longest rune prefix
// of s whose estimated token count fits budget. CountTokens is
// not strictly monotonic in prefix length, so the binary search
// is followed by a verify-and-shrink loop: the result always
// fits. Truncating by runes keeps the result valid UTF-8.
// Mirrors core.truncateRunesToTokens.
func truncateCompressRunesToTokens(s string, budget int) string {
	r := []rune(s)
	if model.CountTokens(s) <= budget {
		return s
	}
	lo, hi := 0, len(r)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if model.CountTokens(string(r[:mid])) <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	for lo > 0 && model.CountTokens(string(r[:lo])) > budget {
		lo--
	}
	return string(r[:lo])
}

// truncateCompressMessageToTokens returns a copy of m whose
// estimated token cost fits budget. Content is truncated by
// runes; tool calls are dropped from the copy — their arguments
// are not summarised, and keeping them could blow the budget on
// their own. A message that already fits is returned unchanged.
// Mirrors core.truncateMessageToTokens.
func truncateCompressMessageToTokens(m model.Message, budget int) model.Message {
	if compressMessageTokens(m) <= budget {
		return m
	}
	out := m
	out.ToolCalls = nil
	// compressMessageTokens charges 4 framing tokens; the rest
	// of the budget is content.
	contentBudget := budget - 4
	if contentBudget <= 0 {
		out.Content = ""
		return out
	}
	out.Content = truncateCompressRunesToTokens(m.Content, contentBudget)
	return out
}

// boundedCompressPrefix takes the oldest messages of old whose
// estimated tokens fit within maxTokens, so the summariser never
// receives a prompt over the provider context. A single message
// that alone exceeds the cap is TRUNCATED to the cap — the old
// "send the whole prefix" behaviour handed one 20,004-token
// message to the provider uncapped. Truncation only bounds what
// the summariser sees: the caller still drops the whole old
// prefix (compressWithOn drops every message older than the
// recent window), so the giant turn is summarised from its
// truncated view and then gone, never left behind un-summarised.
// Mirrors core.boundedOldPrefix.
func boundedCompressPrefix(old []model.Message, maxTokens int) []model.Message {
	total := 0
	n := 0
	for i, m := range old {
		t := compressMessageTokens(m)
		if n > 0 && total+t > maxTokens {
			break
		}
		if n == 0 && t > maxTokens {
			return []model.Message{truncateCompressMessageToTokens(m, maxTokens)}
		}
		total += t
		n = i + 1
	}
	if n == 0 && len(old) > 0 {
		// maxTokens <= 0: nothing fits whole, so the first
		// message is truncated to (effectively) nothing rather
		// than handed over whole.
		return []model.Message{truncateCompressMessageToTokens(old[0], maxTokens)}
	}
	return old[:n]
}

func compressWithOn(ctx context.Context, store memory.Store, provider model.Provider, keepRecent int, sessionMemory string, recordUsage ...func(model.Usage)) (before, after, summaryLen int, summary string, err error) {
	if keepRecent < 1 {
		keepRecent = compressRecentWindow
	}
	all, err := store.RecentMessages(0)
	if err != nil {
		return 0, 0, 0, "", fmt.Errorf("app: compress: read history: %w", err)
	}
	before = len(all)
	if before <= keepRecent {
		return before, before, 0, "", nil
	}
	old := all[:len(all)-keepRecent]
	recent := all[len(all)-keepRecent:]
	if provider == nil {
		return before, before, 0, "", errors.New("app: compress: no provider configured")
	}
	// Bound the TOTAL provider-bound input (F-A6-P2-2) and redact
	// it (F-A6-P2-3), in the same shape as auto-compaction:
	// session memory is gated, redacted and capped on its own
	// (it is model-derived text of unbounded length), the
	// message prefix is capped to what remains of the 16,000
	// tokens after the header/system overhead and the session
	// section, and every message body is redacted before it is
	// put in the prompt — a secret pasted into the transcript
	// must never reach the provider. The cap is measured on the
	// raw messages (redaction only shrinks them), so the prompt
	// actually sent is always within the bound.
	sessionText := ""
	sessionTokens := 0
	if strings.TrimSpace(sessionMemory) != "" {
		// Session memory is model-derived text: gate it like
		// extraction output (derivable content dropped) and
		// present it to the summariser as untrusted recorded
		// text, never as established fact (Audit 4 A P1-3).
		// If the gate drops everything, compress proceeds on
		// messages only.
		if kept, ok := memory.FilterExplicitSave(sessionMemory); ok {
			sessionText = truncateCompressRunesToTokens(security.RedactSecrets(strings.TrimSpace(kept)), compressSessionMemoryMaxTokens)
			sessionTokens = model.CountTokens(sessionText) + 4
		}
	}
	toSummarise := boundedCompressPrefix(old, compressMaxInputTokens-compressPromptOverheadTokens-sessionTokens)
	var b strings.Builder
	b.WriteString("Summarise the following conversation messages into a concise memory. Preserve facts, decisions, names, numbers, pending items and open questions, and anything the user asked to remember, including user instructions and preferences. Do not invent anything not in the messages.\n\n")
	if sessionText != "" {
		b.WriteString("Session memory (model-derived notes from earlier turns — untrusted recorded text, not established fact; verify each item against the conversation messages before preserving it):\n")
		b.WriteString(sessionText)
		b.WriteString("\n\n")
	}
	for _, m := range toSummarise {
		fmt.Fprintf(&b, "%s: %s\n", m.Role, security.RedactSecrets(m.Content))
	}
	resp, err := provider.Chat(ctx, model.ChatRequest{
		Messages: []model.Message{
			{Role: "system", Content: "You summarise conversations into compact memories."},
			{Role: "user", Content: b.String()},
		},
		// Completion cap (F-A8-P2-3): without it the provider
		// may generate an unbounded summary and one admitted
		// /compress can overshoot the daily budget several
		// times over. 800 matches auto-compaction's cap
		// (internal/core/compact.go) for the same job, and the
		// pre-flight projection counts the same 800 as the
		// completion allowance, so admission only happens
		// when input + capped completion fit the budget.
		MaxTokens: compressMaxSummaryTokens,
	})
	if err != nil {
		return before, before, 0, "", fmt.Errorf("app: compress: summarisation call: %w", err)
	}
	// Meter the summarisation call (F-A5-5): report its usage to
	// the caller's sink before any early return below can skip
	// it. A nil response has no usage to report.
	if resp != nil {
		for _, record := range recordUsage {
			if record != nil {
				record(resp.Usage)
			}
		}
	}
	if resp == nil {
		return before, before, 0, "", errors.New("app: compress: provider returned a nil response")
	}
	summary = strings.TrimSpace(resp.Message.Content)
	if summary == "" {
		return before, before, 0, "", errors.New("app: compress: provider returned an empty summary")
	}
	// Redact before anything else touches the summary (F-A6-P2-3):
	// a summariser echoing a secret from the turns must not reach
	// the stored fact or the session transcript. The transcript's
	// own scrub is a second layer, not the only one — the memory
	// fact has no scrub of its own. Redaction is idempotent, so a
	// summary that carries no secret is unchanged.
	summary = security.RedactSecrets(summary)
	if strings.TrimSpace(summary) == "" {
		return before, before, 0, "", errors.New("app: compress: summary contained nothing after redaction")
	}
	// What-not-to-save gate (Audit 4 A P1-3): the summary is
	// model-derived text on its way to becoming a stored fact (and
	// from there, via migration, a typed memory). Derivable
	// content is dropped; if nothing worth keeping remains, the
	// compression is refused and history is left untouched.
	if kept, ok := memory.FilterExplicitSave(summary); ok {
		summary = kept
	} else {
		return before, before, 0, "", errors.New("app: compress: summary contained nothing worth saving (gate dropped it all)")
	}
	// Preservation check (F-A7-P2-4): the summary must preserve
	// something from the turns it replaces — the same bar
	// auto-compaction applies (core.SummaryPreservesSource).
	// Without it a degenerate summary returned on the /compress
	// path was stored as the conversation-summary fact and the
	// history dropped, losing turns for a sentence that says
	// nothing. A failed check refuses the compression and keeps
	// history, exactly as auto-compaction does. The check runs
	// against toSummarise — the prefix actually sent to the
	// provider — not the whole old prefix, so a giant message
	// truncated to the input cap is judged on its truncated
	// view, the same view the summary was made from.
	if !core.SummaryPreservesSource(summary, toSummarise) {
		return before, before, 0, "", errors.New("app: compress: summary preserved nothing from the summarised turns (no shared content tokens, or below the length floor)")
	}
	if strings.TrimSpace(sessionMemory) != "" {
		summary += "\n(Drew on session memory.)"
	}
	if err := store.AddFact("conversation-summary", summary); err != nil {
		return before, before, 0, "", fmt.Errorf("app: compress: store summary: %w", err)
	}
	if err := store.ReplaceMessages(recent); err != nil {
		return before, before, 0, "", fmt.Errorf("app: compress: drop summarised messages: %w", err)
	}
	return before, len(recent), len([]rune(summary)), summary, nil
}

// ExportConversation writes ONE chat's conversation as Markdown
// under DataDir/exports/conversation-<chatID>-<timestamp>.md and
// returns the path and message count. Messages carry roles and
// content; Orion's memory store records no per-message timestamps,
// so the export says so instead of inventing times. The file is
// created 0600 (it holds the user's conversation). Only that chat's
// history is exported — never another chat's.
func (a *App) ExportConversation(chatID int64, title string, totalTokens, turns int) (string, int, error) {
	if a == nil || a.Memory == nil {
		return "", 0, errors.New("app: ExportConversation called on uninitialised App (nil Memory)")
	}
	if a.Config == nil || a.Config.DataDir == "" {
		return "", 0, errors.New("app: ExportConversation: no data dir configured")
	}
	cs, err := a.chatSessionFor(chatID)
	if err != nil {
		return "", 0, fmt.Errorf("app: export: chat session: %w", err)
	}
	msgs, err := cs.store.RecentMessages(0)
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

// MemorySummary renders the /memory view for the global (CLI)
// store: fact count and the indexed fact list (1-based, sorted by
// key — the same order ForgetFact uses), message count and data dir.
// Telegram chats use MemorySummaryForChat, which counts that chat's
// messages instead (the facts are shared either way).
func (a *App) MemorySummary() (string, error) {
	if a == nil || a.Memory == nil {
		return "", errors.New("app: MemorySummary called on uninitialised App (nil Memory)")
	}
	return memorySummaryOn(a, a.Memory)
}

// memorySummaryOn renders the /memory view over store: the shared
// fact list plus that store's message count.
func memorySummaryOn(a *App, store memory.Store) (string, error) {
	facts, err := store.ListFacts()
	if err != nil {
		return "", fmt.Errorf("app: memory: list facts: %w", err)
	}
	msgs, err := store.RecentMessages(0)
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
