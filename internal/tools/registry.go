// Package tools defines the tool contract every Orion tool implements,
// plus a registry. Tool execution is always context-aware (timeouts),
// output-capped, and routed through the security denylist for shell work.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
)

// Tool is the interface for one callable tool.
type Tool interface {
	Name() string
	Description() string
	Definition() model.ToolDefinition
	// Execute runs the tool with JSON-object arguments.
	// It must respect ctx, cap output, and return a ToolError on failure.
	Execute(ctx context.Context, args json.RawMessage) (string, error)
}

// ToolError is a typed tool failure (distinguishes bad args, denied,
// timeout, and execution errors).
type ToolError struct {
	Tool string
	Kind string // invalid_args | denied | timeout | execution | unknown_tool
	Err  error
}

func (e *ToolError) Error() string {
	return fmt.Sprintf("tool %s (%s): %v", e.Tool, e.Kind, e.Err)
}

func (e *ToolError) Unwrap() error { return e.Err }

// Registry holds the available tools.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	// planMode is the plan-mode tool's PlanModeChecker, installed
	// by Register on first sight (see PlanModeChecker). The agent
	// loop consults it before running any tool, so a plan the user
	// has not approved yet can never be executed by a mutating
	// tool. A registry without a plan-mode tool has a nil checker
	// and never blocks.
	planMode PlanModeChecker
	// maxResultBytes caps every tool result (and tool error text)
	// returned by Execute. Zero means the default
	// (DefaultMaxToolResultBytes); SetMaxResultBytes overrides it,
	// normally from config via core.NewAgent.
	maxResultBytes int
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{tools: map[string]Tool{}} }

// SetMaxResultBytes overrides the per-result cap enforced by Execute.
// A non-positive value restores the default. It is safe for
// concurrent use with Execute.
func (r *Registry) SetMaxResultBytes(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 {
		n = DefaultMaxToolResultBytes
	}
	r.maxResultBytes = n
}

// MaxResultBytes returns the per-result cap Execute enforces.
func (r *Registry) MaxResultBytes() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.maxResultBytes <= 0 {
		return DefaultMaxToolResultBytes
	}
	return r.maxResultBytes
}

// PlanModeBlocks reports whether tool is blocked by a pending plan
// for the chat in ctx (see PlanModeChecker). A registry without a
// plan-mode tool never blocks. Nil-safe: a nil registry blocks
// nothing.
func (r *Registry) PlanModeBlocks(ctx context.Context, tool string) (bool, string) {
	if r == nil {
		return false, ""
	}
	r.mu.RLock()
	pm := r.planMode
	r.mu.RUnlock()
	if pm == nil {
		return false, ""
	}
	return pm.PlanModeBlocks(ctx, tool)
}

// IsNilValue reports whether v is nil, including a typed nil (a
// nil pointer stored in an interface): a plain v == nil check
// misses those. It is the ONE typed-nil guard for the tools
// framework and the reply chain (F-A22-9): the shape matches
// testify's isNil and samber/lo's IsNil — v == nil first, then
// reflect.ValueOf, then IsNil() ONLY for the nil-capable kinds
// (Chan, Func, Interface, Map, Ptr, Slice, UnsafePointer). The
// Kind guard is load-bearing: reflect.Value.IsNil panics on any
// other kind, so calling it unguarded would turn a check into a
// crash on ordinary values like 0 or "".
func IsNilValue(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice, reflect.UnsafePointer:
		return rv.IsNil()
	default:
		return false
	}
}

// isNilTool reports whether t is nil, including a typed nil (a
// nil pointer stored in the Tool interface): the t == nil check
// misses those, and the t.Name() call in Register would panic on
// the nil receiver (F-A21-8 — the auditor's probe was
// Register((*wireCallbackTool)(nil))). It is IsNilValue over the
// Tool interface; there is exactly one kind list, here.
func isNilTool(t Tool) bool {
	return IsNilValue(t)
}

// Register adds a tool; duplicate names are an error — the
// duplicate check folds the name (F-A22-7), so a mixed-case
// spelling of an already-registered tool is a duplicate, not a
// second entry. A nil tool or a
// tool with an empty (or whitespace-only) name is rejected instead of
// panicking later on a nil dereference or registering an uncallable
// entry. A typed nil (a nil pointer in the Tool interface) is
// rejected the same way (F-A21-8): t == nil misses it, so the
// check is isNilTool, not a plain nil comparison.
//
// A tool must implement RiskClasser (declare its permission-gate
// risk class explicitly): a tool that forgets to declare would
// otherwise default to Safe and could run a destructive action
// without approval. The declaration must also agree with the
// gate's hardcoded maps (permissions.Classify): a mismatch is a
// programming error — the maps are the authority, and a tool whose
// declaration disagrees is rejected instead of silently running at
// the wrong class.
//
// The name must ALSO be explicitly classified (F-A21-1): in one of
// the gate's maps, or an MCP tool (mcp__ prefix). A novel name is
// rejected whatever it declares — Safe, Network or Destructive —
// because Classify's fail-closed default would otherwise let a
// declaration agree vacuously with the default. Add the tool to
// the maps in internal/permissions/gate.go (or fix its
// declaration) and re-register.
func (r *Registry) Register(t Tool) error {
	if isNilTool(t) {
		return fmt.Errorf("tools: cannot register nil tool")
	}
	if strings.TrimSpace(t.Name()) == "" {
		return fmt.Errorf("tools: cannot register tool with empty name")
	}
	rc, ok := t.(RiskClasser)
	if !ok {
		return fmt.Errorf("tools: tool %q does not implement RiskClasser: declare its risk class explicitly", t.Name())
	}
	if _, known := permissions.ClassifyKnown(t.Name()); !known {
		return fmt.Errorf("tools: tool %q is not in the permission gate's classification (internal/permissions/gate.go): add it to the destructive, network or safe maps (MCP tools use the mcp__ prefix) and declare the same class", t.Name())
	}
	if got, want := rc.RiskClass(), permissions.Classify(t.Name()); got != want {
		return fmt.Errorf("tools: tool %q declares risk class %v but the permission gate classifies it %v: the gate's maps are the authority", t.Name(), got, want)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Folded identity (F-A22-7): the map key and the duplicate
	// check use the normalised name, so "Calculator" after
	// "calculator" is a duplicate, not a second entry sharing
	// one folded classification. The tool keeps its own display
	// spelling in t.Name(); the key is folded.
	key := permissions.NormalizeToolName(t.Name())
	if _, ok := r.tools[key]; ok {
		return fmt.Errorf("tools: duplicate tool %q", t.Name())
	}
	r.tools[key] = t
	// The plan-mode checker is installed on first sight (see
	// PlanModeChecker): exactly one tool per registry implements
	// it, so first-wins is unambiguous in practice.
	if pm, ok := t.(PlanModeChecker); ok && r.planMode == nil {
		r.planMode = pm
	}
	return nil
}

// RegisterAll registers every tool in ts, in order, stopping at
// the first error (duplicate name, nil tool, empty name — see
// Register). It is the dynamic-registration entry point: the MCP
// manager hands the registry its discovered tools in one call
// instead of the app looping Register itself.
func (r *Registry) RegisterAll(ts []Tool) error {
	for _, t := range ts {
		if err := r.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// ScrubEnv returns the process environment with secrets removed,
// for child processes the agent launches (hook executables, MCP
// servers): anything that looks like a credential never reaches a
// process the model can influence. It is the exported form of the
// scrub shell_exec already applies to its children (see shell.go);
// the rules live in exactly one place — scrubEnv — so the hook and
// MCP children can never drift from the shell child's scrub.
func ScrubEnv() []string { return scrubEnv() }

// Get returns a tool by name. The lookup folds the name
// (F-A22-7): the store is keyed by the normalised name, so a
// mixed-case lookup must find the one entry instead of
// reporting "unknown tool".
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[permissions.NormalizeToolName(name)]
	return t, ok
}

// Remove detaches the named tool from the registry so the model
// can no longer call it; it reports whether a tool was removed.
// It is the mechanism newChildRegistry uses to keep tools that
// cannot work in a child out of the child registry (F-A22-16:
// enter_plan_mode needs a gateway the child never gets, and
// exit_plan_mode would clear the PARENT's pending plan from the
// shared store).
//
// The installed PlanModeChecker is deliberately LEFT in place:
// the checker is consulted by the agent loop
// (Registry.PlanModeBlocks), never called by the model, so
// removing the plan-mode tool keeps the pending-plan block — a
// child must still be refused destructive tools while the
// parent's plan is pending — while taking away the callable
// tools. A registry that never had a plan-mode tool is
// unaffected.
func (r *Registry) Remove(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	// The store is keyed by the normalised name (F-A22-7), so
	// removal must fold too — Remove("Calculator") removes the
	// "calculator" entry instead of reporting "not present".
	key := permissions.NormalizeToolName(name)
	if _, ok := r.tools[key]; !ok {
		return false
	}
	delete(r.tools, key)
	return true
}

// List returns all tools in registration order (sorted by name).
func (r *Registry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	// Deterministic order: simple insertion-free sort by name.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Name() < out[i].Name() {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// Definitions returns model-facing tool definitions for all tools.
func (r *Registry) Definitions() []model.ToolDefinition {
	defs := make([]model.ToolDefinition, 0)
	for _, t := range r.List() {
		defs = append(defs, t.Definition())
	}
	return defs
}

// Execute runs a named tool, returning a ToolError for unknown tools.
//
// Tool output is redacted before it is returned: a tool that reads a
// file or runs a command can surface API keys, tokens, or passwords,
// and that output is fed straight to the model (and can reach a
// Telegram user). RedactSecrets replaces detected secrets with
// "[REDACTED]" in both results and error text; ordinary prose is left
// unchanged. Unknown tools are reported as kind "unknown_tool" so the
// model learns the tool does not exist instead of retrying with
// different arguments.
//
// Results (and error text) are also capped to MaxResultBytes via
// TruncateToolResult: a truncated result carries a marker with its
// original size, so the model knows data was cut.
func (r *Registry) Execute(ctx context.Context, name string, args json.RawMessage) (result string, err error) {
	// Text-only view of ExecuteResult (see result.go): the same
	// validation, redaction and truncation, with the images
	// dropped. Callers that need images use ExecuteResult
	// directly; this stays so the text-only tools and every
	// existing caller keep working unchanged.
	res, err := r.ExecuteResult(ctx, name, args)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}
