// Package tools — subagent tools: model-facing delegation to Orion's
// in-process subagent Manager.
//
// The four tools (subagent_spawn, subagent_list, subagent_result,
// subagent_stop) let the parent agent delegate work to child agents
// that run their own agent loop with the same provider, tools,
// denylist, and sandbox rules — but cannot spawn further subagents.
// They are constructed with NewSubagentTools; Worker B wires them into
// the app (a closure resolving the CURRENT agent is passed as the
// SubagentUsageSink, so tokens keep accruing to the live agent after a
// provider switch).
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/subagent"
)

// SubagentUsageSink receives the token count of a subagent result the
// parent actually consumes. The Agent implements it by adding the
// tokens to the parent turn's usage, so the footer / usage accounting
// reflects delegated work.
//
// Usage accounting rule (see subagentResultTool.Execute): tokens are
// counted in the parent turn when the parent consumes the result —
// i.e. on the first subagent_result fetch of a finished (DONE,
// FAILED or TIMED-OUT) task, once its usage has settled. A result
// that is never fetched is charged instead by the per-chat turn-end
// sweep (app.runChatTurn, F-A5-7), so spawn-and-never-fetch spend
// is no longer invisible; fetching the same result twice counts it
// once in either path.
type SubagentUsageSink interface {
	AddSubagentUsage(tokens int)
}

// UsageSinkFunc is a SubagentUsageSink implemented by a function. It
// exists so a sink can resolve its target dynamically on every call —
// the app passes a closure resolving the CURRENT agent, so tokens
// keep accruing to the live agent after a provider switch rebuilds
// it, instead of to the orphaned agent the tools were constructed
// with. A nil func is a no-op sink, never a panic.
type UsageSinkFunc func(tokens int)

// AddSubagentUsage implements SubagentUsageSink.
func (f UsageSinkFunc) AddSubagentUsage(tokens int) {
	if f != nil {
		f(tokens)
	}
}

// Subagent timeout bounds (seconds) for subagent_spawn.
const (
	defaultSubagentTimeoutSeconds = 300
	minSubagentTimeoutSeconds     = 10
	maxSubagentTimeoutSeconds     = 3600
)

// maxListBriefRunes bounds the brief shown per task by subagent_list
// (~80 runes); the list never returns full outputs.
const maxListBriefRunes = 80

// subagentTools is the shared state behind the four subagent tools:
// the Manager that runs tasks, the sink that receives consumed-result
// usage, and the set of task ids whose DONE results have already been
// charged (so a second fetch of the same result never double-counts).
type subagentTools struct {
	mgr       *subagent.Manager
	sink      SubagentUsageSink
	mu        sync.Mutex
	delivered map[string]bool
}

// NewSubagentTools builds the four subagent tools backed by mgr, with
// usage reported to sink. A nil manager or nil sink is a constructor
// error — the tools must fail at wiring time, never panic later on a
// nil dereference mid-turn.
func NewSubagentTools(mgr *subagent.Manager, sink SubagentUsageSink) ([]Tool, error) {
	if mgr == nil {
		return nil, fmt.Errorf("tools: subagent tools require a non-nil manager")
	}
	if sink == nil {
		return nil, fmt.Errorf("tools: subagent tools require a non-nil usage sink")
	}
	set := &subagentTools{
		mgr:       mgr,
		sink:      sink,
		delivered: make(map[string]bool),
	}
	return []Tool{
		&subagentSpawnTool{set: set},
		&subagentListTool{set: set},
		&subagentResultTool{set: set},
		&subagentStopTool{set: set},
	}, nil
}

// mapManagerError converts a Manager error into the ToolError kind the
// model can act on. At capacity is "denied" (a resource limit — the
// model should do the work itself or retry later); anything else from
// the Manager (no executor, shut down) is "execution". The manager's
// message is preserved; the registry redacts secrets as usual.
func mapManagerError(tool string, err error) error {
	kind := "execution"
	if strings.Contains(err.Error(), "at capacity") {
		kind = "denied"
	}
	return &ToolError{Tool: tool, Kind: kind, Err: err}
}

// truncateBrief shortens s to at most maxListBriefRunes runes for list
// display, appending an ellipsis when truncated. Rune-aware: cutting
// mid-rune would corrupt multi-byte text.
func truncateBrief(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes]) + "…"
}

// ---------------------------------------------------------------- spawn

type subagentSpawnTool struct{ set *subagentTools }

func (t *subagentSpawnTool) Name() string { return "subagent_spawn" }

func (t *subagentSpawnTool) Description() string {
	return "Delegate work to a subagent: spawn a child agent that runs its own agent loop with its own conversation to complete the brief, then fetch its output with subagent_result. " +
		"Delegate when the work is independent research, parallel work, or a long-running job that does not need the parent's conversation context. " +
		"Do the work directly instead for quick lookups, work that needs the parent's conversation, or anything needing a user confirmation. " +
		"The subagent shares the same provider, tools, denylist, and sandbox rules as the parent, but it CANNOT spawn further subagents. " +
		"Spawn returns immediately with a task id; the task runs in the background — poll it with subagent_result (which never blocks) or check subagent_list."
}

func (t *subagentSpawnTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"brief": map[string]any{
					"type":        "string",
					"description": "Self-contained instructions for the subagent: what to do, what output to produce, and any constraints. It has no access to the parent's conversation, so include everything it needs.",
				},
				"timeout_seconds": map[string]any{
					"type":        "integer",
					"description": "Maximum time the subagent may run before it is timed out. Default 300. Clamped to 10–3600.",
				},
			},
			"required": []string{"brief"},
		},
	}
}

type subagentSpawnArgs struct {
	Brief          string `json:"brief"`
	TimeoutSeconds *int   `json:"timeout_seconds,omitempty"`
}

func (t *subagentSpawnTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a subagentSpawnArgs
	if err := decodeArgs(t.Name(), args, &a); err != nil {
		return "", err
	}
	brief := strings.TrimSpace(a.Brief)
	if brief == "" {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("brief must not be empty: describe the work for the subagent")}
	}
	timeoutSeconds := defaultSubagentTimeoutSeconds
	if a.TimeoutSeconds != nil {
		timeoutSeconds = *a.TimeoutSeconds
		if timeoutSeconds < minSubagentTimeoutSeconds {
			timeoutSeconds = minSubagentTimeoutSeconds
		}
		if timeoutSeconds > maxSubagentTimeoutSeconds {
			timeoutSeconds = maxSubagentTimeoutSeconds
		}
	}
	if err := ctx.Err(); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: err}
	}
	// Attribute the task to its owning chat (Audit 5 F-A5-7):
	// the per-chat sweep charges a finished task only to the
	// chat that spawned it. No approval context (CLI-driven
	// use) means chatID 0 — unattributed, never swept.
	chatID, _, _ := permissions.ApprovalContextFrom(ctx)
	id, err := t.set.mgr.SpawnForChat(chatID, brief, time.Duration(timeoutSeconds)*time.Second)
	if err != nil {
		return "", mapManagerError(t.Name(), err)
	}
	out, err := json.Marshal(map[string]any{
		"id":              id,
		"status":          string(subagent.StatusRunning),
		"timeout_seconds": timeoutSeconds,
	})
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("encoding spawn result: %w", err)}
	}
	return string(out), nil
}

// ---------------------------------------------------------------- list

type subagentListTool struct{ set *subagentTools }

func (t *subagentListTool) Name() string { return "subagent_list" }

func (t *subagentListTool) Description() string {
	return "List subagent tasks (running and finished): id, status, a short preview of the brief, and duration. Never returns full outputs — use subagent_result with a task id to fetch one. Listing never counts token usage."
}

func (t *subagentListTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}
}

type subagentListEntry struct {
	ID              string  `json:"id"`
	Status          string  `json:"status"`
	Brief           string  `json:"brief"`
	DurationSeconds float64 `json:"duration_seconds"`
}

func (t *subagentListTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	// No arguments expected; tolerate empty/null args, reject malformed JSON.
	if len(args) > 0 && strings.TrimSpace(string(args)) != "" && strings.TrimSpace(string(args)) != "null" {
		var probe map[string]any
		if err := json.Unmarshal(args, &probe); err != nil {
			return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("invalid JSON arguments: %w", err)}
		}
	}
	now := time.Now()
	entries := make([]subagentListEntry, 0)
	for _, ts := range t.set.mgr.List() {
		duration := now.Sub(ts.StartedAt).Seconds()
		if res, ok := t.set.mgr.GetResult(ts.ID); ok {
			// Finished: report the recorded total, not time since start.
			duration = res.Duration.Seconds()
		}
		if duration < 0 {
			duration = 0
		}
		entries = append(entries, subagentListEntry{
			ID:              ts.ID,
			Status:          string(ts.Status),
			Brief:           truncateBrief(ts.Brief, maxListBriefRunes),
			DurationSeconds: duration,
		})
	}
	out, err := json.Marshal(map[string]any{
		"tasks": entries,
		"count": len(entries),
	})
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("encoding task list: %w", err)}
	}
	return string(out), nil
}

// ---------------------------------------------------------------- result

type subagentResultTool struct{ set *subagentTools }

func (t *subagentResultTool) Name() string { return "subagent_result" }

func (t *subagentResultTool) Description() string {
	return "Fetch a subagent task's result by id. Non-blocking: if the task is still running or pending, this returns its status and elapsed time — call again later instead of waiting. If the task finished, it returns the status, duration, tokens used, and the output. The first fetch of a finished task's result charges its tokens to the parent turn; fetching the same result again does not charge twice."
}

func (t *subagentResultTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string", "description": "Task id returned by subagent_spawn (or shown by subagent_list)."},
			},
			"required": []string{"id"},
		},
	}
}

type subagentIDArgs struct {
	ID string `json:"id"`
}

func (t *subagentResultTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a subagentIDArgs
	if err := decodeArgs(t.Name(), args, &a); err != nil {
		return "", err
	}
	id := strings.TrimSpace(a.ID)
	if id == "" {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("id must not be empty: pass the task id from subagent_spawn or subagent_list")}
	}
	status, known := t.set.mgr.Status(id)
	if !known {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("unknown subagent task %q: use subagent_list to see known tasks", id)}
	}
	res, finished := t.set.mgr.GetResult(id)
	if !finished {
		// Still running/pending: report status + elapsed, never block.
		elapsed := 0.0
		for _, ts := range t.set.mgr.List() {
			if ts.ID == id {
				elapsed = time.Since(ts.StartedAt).Seconds()
				break
			}
		}
		if elapsed < 0 {
			elapsed = 0
		}
		out, err := json.Marshal(map[string]any{
			"id":              id,
			"status":          string(status),
			"elapsed_seconds": elapsed,
		})
		if err != nil {
			return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("encoding task status: %w", err)}
		}
		return string(out), nil
	}

	// Finished: charge the usage exactly once, for DONE, FAILED
	// and TIMED-OUT tasks — tokens are counted in the parent turn
	// when the parent consumes the result; a second fetch of the
	// same result must not double-count. A result never fetched is
	// charged instead by the per-chat turn-end sweep (app.runChatTurn,
	// F-A5-7).
	// Failed tasks count too: their tokens were genuinely spent
	// (the executor did work before failing) and the parent is told
	// about them, so hiding them would undercount the turn.
	// Timed-out tasks count too (F-A8-P2-2): a task that timed out
	// still spent provider tokens, and when its executor ignored
	// the deadline the Manager records the late usage on the entry
	// (F-A7-2a) — the old claim that timed-out tasks record zero
	// usage was false post-rc12. Charging them on fetch is what
	// saves the CLI path, which has no sweep: without it the late
	// tokens vanish even though the payload reports them.
	// The charge happens only once the usage is SETTLED. Fetching
	// a timed-out (or cancellation-failed) task while its executor
	// is still running reports the provisional zero and charges
	// nothing — and, crucially, does NOT mark the result
	// delivered, so a later fetch still charges the late usage
	// once it arrives. Marking on an unsettled fetch would burn
	// the single charge on zero and lose the late tokens forever
	// (the same trap the sweep avoids by skipping unsettled
	// tasks without marking them swept).
	// Exactly-once across fetch AND sweep (chat path): the two
	// chargers write to separate buckets — the fetch sink resolves
	// to the global agent (app.go), while the sweep charges the
	// chat's stats/budget — and each has its own guard (the
	// delivered set here, the swept set in the sweep), exactly as
	// DONE and FAILED tasks already do today. Adding TIMEOUT to
	// the fetch set introduces no new double-count: within each
	// bucket the task is charged once, and the fetch touches no
	// Manager state the sweep depends on.
	if status == subagent.StatusDone || status == subagent.StatusFailed || status == subagent.StatusTimeout {
		if res.UsageSettled {
			t.set.mu.Lock()
			already := t.set.delivered[id]
			if !already {
				t.set.delivered[id] = true
			}
			t.set.mu.Unlock()
			if !already {
				t.set.sink.AddSubagentUsage(res.Usage.Tokens)
			}
		}
	}

	payload := map[string]any{
		"id":               id,
		"status":           string(status),
		"duration_seconds": res.Duration.Seconds(),
		"tokens":           res.Usage.Tokens,
		"output":           res.Output,
	}
	if res.Err != nil {
		payload["error"] = res.Err.Error()
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("encoding task result: %w", err)}
	}
	return string(out), nil
}

// ---------------------------------------------------------------- stop

type subagentStopTool struct{ set *subagentTools }

func (t *subagentStopTool) Name() string { return "subagent_stop" }

func (t *subagentStopTool) Description() string {
	return "Cancel a running subagent task by id. Returns whether the task was running and got cancelled, or was already finished (or unknown) and so was not cancelled."
}

func (t *subagentStopTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string", "description": "Task id returned by subagent_spawn (or shown by subagent_list)."},
			},
			"required": []string{"id"},
		},
	}
}

func (t *subagentStopTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a subagentIDArgs
	if err := decodeArgs(t.Name(), args, &a); err != nil {
		return "", err
	}
	id := strings.TrimSpace(a.ID)
	if id == "" {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("id must not be empty: pass the task id from subagent_spawn or subagent_list")}
	}
	cancelled := t.set.mgr.Cancel(id)
	status, known := t.set.mgr.Status(id)
	statusStr := string(status)
	if !known {
		statusStr = "unknown"
	}
	out, err := json.Marshal(map[string]any{
		"id":        id,
		"cancelled": cancelled,
		"status":    statusStr,
	})
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("encoding stop result: %w", err)}
	}
	return string(out), nil
}
