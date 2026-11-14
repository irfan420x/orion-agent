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
	"log"

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/core"
	"github.com/irfan/orion/internal/hooks"
	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
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
// on. The variadic form keeps the existing newChildRegistry(cfg)
// call sites compiling while the executor wiring passes the store.
func newChildRegistry(cfg *config.Config, parentPlanning ...*tools.PlanningStore) (*tools.Registry, error) {
	if cfg == nil {
		cfg = config.Default()
	}
	deps := tools.ToolDeps{SandboxCfg: cfg.Sandbox, TrustedEgressCIDRs: cfg.TrustedEgressCIDRs}
	if len(parentPlanning) > 0 && parentPlanning[0] != nil {
		deps.PlanningStore = parentPlanning[0]
	}
	return tools.DefaultRegistryWithDeps(deps)
}

// newSubagentExecutor returns a subagent.Executor that runs each brief
// in a FRESH child Agent: same provider (verified concurrent-safe,
// see the package note above), a child registry without subagent
// tools, an ephemeral in-memory store, the same config (timeouts and
// caps), the read-only typed-memory view, the parent's hook bus
// (a PreToolUse hook can block a child's tool call exactly as it
// blocks the parent's), and no session appender.
// MCP tools and the skill tool are deliberately NOT in the child
// registry: children run without the interactive permission gate,
// and MCP tools are ClassNetwork — adding ungated network tools to
// children would widen the hole the gate closes. Revisit when
// children get a non-interactive gate.
// The executor is safe to call concurrently — every spawn builds its
// own agent, registry and store, sharing only the provider.
// parentPlanning is the parent registry's planning store (Audit 5
// F-A5-P2-35): the child re-points its planning tools at it, so a
// child's task_create/todo_write lands in the chat's files instead
// of an ephemeral per-child bucket that dies with the child. Nil
// keeps the old isolated behaviour.
func newSubagentExecutor(provider model.Provider, cfg *config.Config, typedSrc *typedStoreAdapter, logger *log.Logger, hooksBus *hooks.Bus, parentPlanning *tools.PlanningStore) subagent.Executor {
	return func(ctx context.Context, brief string) (string, subagent.Usage, error) {
		childReg, err := newChildRegistry(cfg, parentPlanning)
		if err != nil {
			return "", subagent.Usage{}, err
		}
		mem := memory.NewEphemeralStore()
		child, err := core.NewAgent(cfg, provider, childReg, mem)
		if err != nil {
			return "", subagent.Usage{}, err
		}
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
