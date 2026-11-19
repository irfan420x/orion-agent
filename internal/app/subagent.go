// Subagent child-agent wiring: the executor the Manager runs per
// spawned task.
//
// Design (why a fresh child Agent per spawn, not the shared one):
//
//   - Deadlock: core.Agent.run holds runMu for the whole turn. A
//     parent turn that calls subagent_spawn while holding runMu would
//     deadlock if the child's executor called the same Agent's Run —
//     it would block on runMu until the step timeout. A fresh child
//     Agent has its own runMu, so parent and child turns proceed
//     concurrently.
//   - History isolation: Agent.run reads RecentMessages from its
//     memory store and AddMessages every turn into it. A child built
//     on the parent's store would pollute the parent's conversation.
//     Each child gets its own ephemeral in-memory store (see
//     memory.NewEphemeralStore), which is discarded when the task
//     ends, so no subagent message ever reaches the parent's history
//     or disk.
//   - No recursion: the child registry is built from the same
//     DefaultRegistry call as the parent but the subagent tools are
//     only ever registered on the PARENT registry (in Build). A child
//     therefore has no subagent_spawn tool at all — it cannot
//     delegate further, by construction, not by a runtime check.
//
// Provider sharing: the child reuses the parent's provider instance.
// That is safe for concurrent use — every provider implementation
// holds only read-only config plus the package-level shared
// *http.Client (model.newSharedHTTPClient), which the standard
// library documents as safe for concurrent use, and all per-call
// state (request, response, stream scanner) is local to the call.
// No provider method mutates shared state.
//
// Typed memory: the child reads the same typed memories as the
// parent, through a READ-ONLY view (readOnlyTypedSource). The
// parent's adapter is NOT shared directly: it exposes
// MarkTypedMemoryStale, which persists a stale marker — a write to
// shared state a child must never perform. The view exposes only
// List/Load, so core's drift check on the child can only mark stale
// in the child's own (ephemeral) memory. The child's session
// appender is deliberately left UNSET (nil): core disables session
// extraction when no appender is wired (see
// maybeExtractSessionMemory — a nil appender returns before
// launching any background work), so a subagent turn never appends
// to the shared session log.
//
// Usage: the factory installs a temporary event handler on the child
// (SetEventHandler is per-agent) capturing the TurnFinished event's
// Usage.TotalTokens, and returns it as subagent.Usage. The child's
// final error is propagated as the error — including MaxStepsError,
// which is returned as err, never as output.
package app

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/hooks"
	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/subagent"
	"github.com/irfan/orion/internal/tools"
)

// errTypedSourceUnavailable is returned by the read-only typed-memory
// view when it has no underlying store (defensive only: the factory
// never installs the view without a store).
var errTypedSourceUnavailable = errors.New("app: typed memory store unavailable")

// readOnlyTypedSource is the read-only view of the parent's typed
// memories handed to child agents. It implements exactly the read
// half of core's typed-memory source (List + Load) and deliberately
// does NOT implement MarkTypedMemoryStale: core's drift check calls
// that method only when the source offers it (interface assertion in
// markStaleMemory), so a child that finds a stale memory records it
// in its own ephemeral state and never persists a marker into the
// parent's typed store.
type readOnlyTypedSource struct {
	src *typedStoreAdapter
}

func (s *readOnlyTypedSource) ListTypedMemories() ([]core.TypedMemory, error) {
	if s == nil || s.src == nil {
		return nil, nil
	}
	return s.src.ListTypedMemories()
}

func (s *readOnlyTypedSource) LoadTypedMemory(name string) (core.TypedMemory, error) {
	if s == nil || s.src == nil {
		return core.TypedMemory{}, errTypedSourceUnavailable
	}
	return s.src.LoadTypedMemory(name)
}

// childTypedSource returns the read-only typed-memory view for child
// agents, or nil when the app has no typed store (typed recall is
// then disabled for children, exactly as for a parent without one).
// A nil typedAdapter (hand-built App) yields nil.
func (a *App) childTypedSource() *readOnlyTypedSource {
	if a == nil || a.typedAdapter == nil {
		return nil
	}
	return &readOnlyTypedSource{src: a.typedAdapter}
}

// newChildRegistry builds the tool registry for a subagent child:
// the same tools, sandbox mode and trusted egress CIDRs as the
// parent — but WITHOUT the subagent tools, which are only registered
// on the parent registry. A child therefore cannot spawn subagents.
//
// The computer-use tools are NOT in a child registry at all
// (F-A23-2 — see the note on newChildRegistry below), so the
// shared computer-use budget (F-A23W2-14 / F-M2R-16) is spent
// only by the parent registry's tools. A child mints zero
// captures: there is no per-child pool to share or to reset,
// which exceeds F-A23W2-14's "no fresh pool per child" rather
// than violating it. captureBudget is still threaded through
// the executor wiring (uniform call shape) but is inert here.
//
// The plan-mode tools are likewise excluded (F-A22-16):
// enter_plan_mode needs a Telegram gateway, which a child never
// gets (it would guaranteed-fail), and exit_plan_mode touches the
// shared planning store, so a child calling it would clear the
// PARENT chat's pending plan — the approval boundary plan mode
// exists to enforce. Neither tool can work in a child, so neither
// is listed there. The exclusion is loud: if a future rename
// drops one of the names from the default registry, construction
// fails instead of silently re-exposing the tool. The plan-mode
// BLOCK is not removed with the tools: Registry.Remove leaves
// the installed PlanModeChecker in place, so a pending parent
// plan still refuses the child's destructive tools.
//
// The optional parentPlanning argument is the parent's planning
// store (F-A5-P2-35): when given, the child's planning tools are
// built on it (passed in ToolDeps), so a child's task_create lands
// in the chat's files instead of a fresh in-memory store that is
// discarded when the child exits. Sharing the store is safe —
// PlanningStore serialises in-process (mutex) and across processes
// (flock) — and the child still inherits the parent's chat id from
// the run context, so it lands in the same chat's bucket. Omit it
// (or pass nil) and the child keeps its own fresh in-memory store,
// the old behaviour, which callers that never wired a data dir rely
// on. The variadic form keeps the existing newChildRegistry(cfg,
// nil) call sites compiling while the executor wiring passes the
// store. (captureBudget is inert for a child — see the note on
// newChildRegistry — but is still accepted for a uniform shape.)
// The cron tools are likewise excluded: scheduling is a
// parent-chat commitment (recurring turns, token spend, approval
// prompts in the parent's chat), so a child running a brief must
// not be able to commit the parent's chat to recurring work, and
// cron_cancel in a child could drop the parent's jobs. The
// scheduled turn itself runs on the parent chat's agent, never
// in a child, so excluding the tools here costs nothing.
//
// The computer-use tools are excluded too (F-A23-2): input is
// ClassDestructive — click/type/key acts directly on the desktop
// — and a child turn has no approval surface, so a child that
// could call input could click, type or submit things the parent
// and the user never see (the child's transcript is ephemeral
// and discarded, and no Telegram card is ever shown for a child's
// tool call). ground and screenshot are ClassSafe, so no gate can
// restrain them — removal from the registry is the ONLY control
// for them — and it also removes each child's own capture budget
// (30 captures/turn per child). input is denied by the child's
// non-interactive gate as well (see newSubagentExecutor), so the
// two controls back each other up.
//
// NOTE on captureBudget (F-A23W2-14 vs F-A23-2): the parameter
// is still accepted so the executor wiring stays uniform, but it
// is INERT for a child — the tools it would charge (ground,
// screenshot, input) are excluded from this registry just above,
// so nothing in a child can spend from the shared pool. The
// shared budget is spent only by the parent registry's tools;
// F-A23W2-14's "no per-child fresh pool" is exceeded, not
// violated: a child mints zero captures, not a shared 15.
func newChildRegistry(cfg *config.Config, captureBudget *tools.CaptureBudget, parentPlanning ...*tools.PlanningStore) (*tools.Registry, error) {
	if cfg == nil {
		cfg = config.Default()
	}
	deps := tools.ToolDeps{
		SandboxCfg:         cfg.Sandbox,
		TrustedEgressCIDRs: cfg.TrustedEgressCIDRs,
		CaptureBudget:      captureBudget,
		// Never the owner: only the parent registry's BeginTurn
		// resets the shared budget (F-A23W2-14). A nil budget
		// becomes a fresh one owned by this registry (see
		// DefaultRegistryWithDeps), so a nil here keeps the old
		// per-child-budget behaviour.
		CaptureBudgetOwner: false,
	}
	if len(parentPlanning) > 0 && parentPlanning[0] != nil {
		deps.PlanningStore = parentPlanning[0]
	}
	reg, err := tools.DefaultRegistryWithDeps(deps)
	if err != nil {
		return nil, err
	}
	// F-A22-16: the plan-mode tools are not child tools (see the
	// doc comment above). Remove them loudly: a missing name
	// means the default registry changed under us, and failing
	// here beats silently handing a child a tool it must not
	// have.
	for _, name := range []string{"enter_plan_mode", "exit_plan_mode", "cron_schedule", "cron_list", "cron_cancel", "cron_enable", "input", "ground", "screenshot"} {
		if !reg.Remove(name) {
			return nil, fmt.Errorf("app: newChildRegistry: default registry has no %q to exclude from the child registry", name)
		}
	}
	return reg, nil
}

// newSubagentExecutor returns a subagent.Executor that runs each brief
// in a FRESH child Agent: same provider (verified concurrent-safe,
// see the package note above), a child registry without subagent
// tools, an ephemeral in-memory store, the same config (timeouts and
// caps), the read-only typed-memory view, the parent's hook bus
// (a PreToolUse hook can block a child's tool call exactly as it
// blocks the parent's), a non-interactive deny-by-default
// permission gate (F-A23-2), and no session appender.
// MCP tools and the skill tool are deliberately NOT in the child
// registry: MCP tools are ClassNetwork, and the skill tool's
// orchestration is a parent-turn concern. The child gate
// (permissions.NewChildGate) would deny any gated tool that ever
// reached the registry, so the exclusion is defence in depth
// now, not the only control — but it stays, because a tool that
// is never listed can never be called, whatever a future gate
// or classification change does.
// The executor is safe to call concurrently — every spawn builds its
// own agent, registry and store, sharing only the provider.
// parentPlanning is the parent registry's planning store (Audit 5
// F-A5-P2-35): the child re-points its planning tools at it, so a
// child's task_create/todo_write lands in the chat's files instead
// of an ephemeral per-child bucket that dies with the child. Nil
// keeps the old isolated behaviour. captureBudget is the parent
// registry's shared computer-use budget (F-A23W2-14 / F-M2R-16),
// threaded through for a uniform call shape — but inert for a
// child: the child's computer-use tools are excluded from its
// registry (F-A23-2), so nothing in a child can charge it. Nil
// means DefaultRegistryWithDeps mints a fresh private budget
// for the child registry — inert either way, for the same reason.
func newSubagentExecutor(provider model.Provider, cfg *config.Config, typedSrc *typedStoreAdapter, logger *log.Logger, hooksBus *hooks.Bus, parentPlanning *tools.PlanningStore, captureBudget *tools.CaptureBudget, chain func() *model.FailoverChain) subagent.Executor {
	return func(ctx context.Context, brief string) (string, subagent.Usage, error) {
		childReg, err := newChildRegistry(cfg, captureBudget, parentPlanning)
		if err != nil {
			return "", subagent.Usage{}, err
		}
		mem := memory.NewEphemeralStore()
		child, err := core.NewAgent(cfg, provider, childReg, mem)
		if err != nil {
			return "", subagent.Usage{}, err
		}
		// Failover: the child inherits the daemon's chain (resolved
		// through the accessor at spawn time, so a default switch
		// that happened after Build is already visible). A child
		// whose provider is down fails over exactly like a parent
		// turn instead of dying on the parent's dead provider.
		// The accessor is nil-safe: a nil chain means no failover.
		if chain != nil {
			child.SetFallbackChain(chain())
		}
		// Permission gate (F-A23-2): the child runs under a
		// non-interactive deny-by-default gate — destructive
		// and network tools (shell_exec, file_write, web_fetch,
		// input, ...) are denied promptly, with no approval
		// surface that could ever answer. A child turn has no
		// Telegram card and no user present, so prompting
		// would only burn the approval window; the child
		// gate has no prompter by construction. Safe tools
		// (file_read, grep, calculator, task_create) still
		// run, so a child can do read/compute work.
		child.SetPermissionGate(permissions.NewChildGate())
		if logger != nil {
			child.SetLogger(logger)
		}
		// Hooks: the child gets the parent's bus (nil-safe), so a
		// PreToolUse hook can block a child's tool call. The bus
		// is non-interactive and fail-closed, so it cannot stall
		// a child turn waiting on a user.
		child.SetHooks(hooksBus)
		if typedSrc != nil {
			child.SetTypedMemorySource(&readOnlyTypedSource{src: typedSrc})
		}
		// Session appender deliberately UNSET (nil): a nil appender
		// disables session extraction (core checks it before
		// launching any background work), so subagent turns never
		// write to the shared session log or shared state.

		// Capture the child's turn usage: TurnFinished carries the
		// accumulated Usage; the handler runs synchronously inside
		// child.Run, so reading `captured` after Run returns is
		// ordered after the write.
		var captured int
		child.SetEventHandler(func(ev core.ProgressEvent) {
			if ev.Kind == core.EventTurnFinished {
				captured = ev.Usage.TotalTokens
			}
		})
		// The child turn must NOT inherit the parent's Telegram
		// update id (F-A9-1): the child agent has no journal, and
		// even if one were ever wired, the child's tool calls
		// must not share the parent update's journal namespace —
		// a child's identical call could replay the parent's
		// result, or vice versa. Clearing the id keeps the
		// child's tools always executing fresh, and the run-1
		// child is dead with the old process, so a re-delivered
		// parent turn that re-spawns cannot duplicate the
		// child's work.
		out, runErr := child.Run(core.WithUpdateID(ctx, 0), brief)
		// Release the child's ephemeral store. Save/Close are
		// no-ops for the ephemeral store; the error is ignored
		// because there is nothing durable that could fail — the
		// turn's own error (if any) is what the caller needs.
		_ = child.Shutdown()
		// The child's final error propagates as the error,
		// including MaxStepsError — never as output.
		return out, subagent.Usage{Tokens: captured}, runErr
	}
}
