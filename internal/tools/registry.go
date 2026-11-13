// Package tools defines the tool contract every Orion tool implements,
// plus a registry. Tool execution is always context-aware (timeouts),
// output-capped, and routed through the security denylist for shell work.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/security"
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

// Register adds a tool; duplicate names are an error. A nil tool or a
// tool with an empty (or whitespace-only) name is rejected instead of
// panicking later on a nil dereference or registering an uncallable
// entry.
func (r *Registry) Register(t Tool) error {
	if t == nil {
		return fmt.Errorf("tools: cannot register nil tool")
	}
	if strings.TrimSpace(t.Name()) == "" {
		return fmt.Errorf("tools: cannot register tool with empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[t.Name()]; ok {
		return fmt.Errorf("tools: duplicate tool %q", t.Name())
	}
	r.tools[t.Name()] = t
	return nil
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
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
	// A panicking tool must not crash the daemon (or the agent run):
	// recover here and report the panic as an execution error, the
	// same way any other tool failure is reported.
	defer func() {
		if rec := recover(); rec != nil {
			result = ""
			err = &ToolError{
				Tool: name,
				Kind: "execution",
				Err:  errors.New(security.RedactSecrets(fmt.Sprintf("tool panicked: %v", rec))),
			}
		}
	}()
	t, ok := r.Get(name)
	if !ok {
		return "", &ToolError{Tool: name, Kind: "unknown_tool", Err: fmt.Errorf("unknown tool")}
	}
	result, err = t.Execute(ctx, args)
	if err != nil {
		var toolErr *ToolError
		if errors.As(err, &toolErr) {
			// Tool errors are fed back to the model as text too,
			// so they get the same size cap as results: a failing
			// tool must not be able to flood the context through
			// its error string either.
			return "", &ToolError{
				Tool: toolErr.Tool,
				Kind: toolErr.Kind,
				Err:  errors.New(TruncateToolResult(security.RedactSecrets(toolErr.Err.Error()), r.MaxResultBytes())),
			}
		}
		return "", fmt.Errorf("%s", TruncateToolResult(security.RedactSecrets(err.Error()), r.MaxResultBytes()))
	}
	// Central choke point for tool-result size: every tool's output
	// passes through here, so no tool — built-in or added later —
	// can return unbounded data to the model. Per-tool caps bound
	// memory while a tool runs; this bounds what the model sees.
	// Over-cap results are truncated with a marker carrying the
	// original size, never dropped and never an error.
	return TruncateToolResult(security.RedactSecrets(result), r.MaxResultBytes()), nil
}
