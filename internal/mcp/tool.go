package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/tools"
)

// Tool is one MCP server tool exposed as an Orion tool. It
// implements tools.Tool, so the registry — and through it the
// permission gate, the central redact/truncate choke point, and the
// result caps — applies to MCP tools exactly as to built-in ones.
// The MCP server process itself is reached only through Client, and
// its environment is scrubbed (see Client).
type Tool struct {
	server      string
	name        string
	description string
	schema      map[string]any
	client      *Client
}

// newTool builds the Orion tool for one advertised server tool.
func newTool(client *Client, info ToolInfo) *Tool {
	desc := info.Description
	if strings.TrimSpace(desc) == "" {
		desc = fmt.Sprintf("MCP tool %q from server %q.", info.Name, client.Name())
	}
	return &Tool{
		server:      client.Name(),
		name:        info.Name,
		description: desc,
		schema:      info.InputSchema,
		client:      client,
	}
}

// Name returns the model-facing tool name: mcp__<server>__<tool>.
// The prefix is what the permission gate keys its ClassNetwork
// classification on, and what keeps MCP tools distinct from
// built-in tools even when a server advertises a colliding name.
func (t *Tool) Name() string { return "mcp__" + t.server + "__" + t.name }

// Description returns the server-advertised description (or the
// generated default when the server gave none).
func (t *Tool) Description() string { return t.description }

// Definition returns the model-facing tool definition: the MCP
// input schema, normalised to an object schema by ListTools.
func (t *Tool) Definition() model.ToolDefinition {
	schema := t.schema
	if schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.description,
		Parameters:  schema,
	}
}

// Execute calls the server tool with args (a JSON object) and
// returns its text. A timeout is reported as kind "timeout" so the
// agent can tell a slow server from a failed one; bad arguments
// are "invalid_args"; anything else from the server is
// "execution". The result itself is NOT redacted or capped here —
// Registry.Execute does that centrally for every tool, MCP
// included, so there is exactly one choke point.
func (t *Tool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var m map[string]any
	trimmed := strings.TrimSpace(string(args))
	if trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal(args, &m); err != nil {
			return "", &tools.ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("arguments are not a JSON object: %w", err)}
		}
		if m == nil {
			m = map[string]any{}
		}
	} else {
		m = map[string]any{}
	}
	out, err := t.client.CallTool(ctx, t.name, m)
	if err != nil {
		kind := "execution"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			kind = "timeout"
		}
		return "", &tools.ToolError{Tool: t.Name(), Kind: kind, Err: err}
	}
	return out, nil
}

// Compile-time check: Tool implements tools.Tool.
var _ tools.Tool = (*Tool)(nil)
