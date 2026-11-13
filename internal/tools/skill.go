// Package tools — the skill tool: the model's invocation path for
// skills.
//
// Skills live only in the user data dir (~/.orion/skills, loaded by
// internal/skills into a Registry); they are never loaded from the
// repo working directory, so a checked-out project can never smuggle
// instructions into the agent. The agent lists skill names and
// descriptions in the system prompt (see core.skillSection) but NOT
// their bodies: invoking a skill is what loads its body, lazily, as
// this tool's result — the model reads the full instructions only
// for the skill it actually uses, instead of paying context for
// every skill on every turn.
//
// The tool itself is deliberately stateless: it looks the skill up
// and returns its body. The agent loop (core.executeSkillTool) owns
// activation state — which skill is active and which tools it
// allows — so the same tool works for direct registry callers (CLI
// `orion tools exec skill ...`) without pretending to enforce
// anything, and enforcement lives in exactly one place.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/skills"
)

// skillActions are the values of the skill tool's "action" parameter.
const (
	skillActionInvoke = "invoke"
	skillActionEnd    = "end"
)

// maxSkillNamesListed bounds the "available skills" list in an
// unknown-skill error: a data dir with hundreds of skills must not
// turn an error into a context flood.
const maxSkillNamesListed = 20

// skillTool is the model-facing skill tool, backed by a skills
// registry. Construct it with NewSkillTool.
type skillTool struct {
	reg *skills.Registry
}

// NewSkillTool builds the "skill" tool backed by reg. A nil registry
// is a constructor error: the tool must fail at wiring time, never
// panic later on a nil dereference mid-turn. A registry that loaded
// zero skills is fine — the tool then reports "no skills loaded"
// instead of failing to build.
func NewSkillTool(reg *skills.Registry) (Tool, error) {
	if reg == nil {
		return nil, fmt.Errorf("tools: skill tool requires a non-nil skills registry")
	}
	return &skillTool{reg: reg}, nil
}

func (t *skillTool) Name() string { return "skill" }

func (t *skillTool) Description() string {
	return "Invoke a skill: load its full instructions and follow them. " +
		"Skills are named workflows in the user's data dir; the system prompt lists the available ones with a one-line description each. " +
		"Call skill with the skill's name (and the task, when the skill needs one) and its full instructions are returned as the result — read them and do what they say. " +
		"While a skill is active, only the tools it allows may be used. " +
		"End the active skill with action \"end\" when its task is done and you need other tools again."
}

func (t *skillTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "Name of the skill to invoke (or to end), exactly as listed in the system prompt.",
				},
				"task": map[string]any{
					"type":        "string",
					"description": "The specific task to do with this skill, when the skill needs one. Optional.",
				},
				"action": map[string]any{
					"type":        "string",
					"description": "What to do: \"invoke\" loads the skill (default), \"end\" ends the active skill.",
					"enum":        []any{"invoke", "end"},
				},
			},
			"required": []string{"name"},
		},
	}
}

type skillArgs struct {
	Name   string `json:"name"`
	Task   string `json:"task"`
	Action string `json:"action"`
}

// Execute loads the named skill's body (invoke) or confirms its end
// (end). The body is returned verbatim — the skill file is the
// author's instructions — plus a footer stating the tools the skill
// allows and the task, so the model can see the restriction in the
// same result that activates it.
func (t *skillTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a skillArgs
	if err := decodeArgs(t.Name(), args, &a); err != nil {
		return "", err
	}
	action := strings.TrimSpace(a.Action)
	if action == "" {
		action = skillActionInvoke
	}
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("skill name must not be empty")}
	}
	switch action {
	case skillActionInvoke:
		// Registry validation already enforced required "name"
		// and the action enum before Execute runs; the checks
		// here are the second layer for direct Execute callers.
		skill, ok := t.reg.Get(name)
		if !ok {
			return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("unknown skill %q (available: %s)", name, t.availableNames())}
		}
		if err := ctx.Err(); err != nil {
			return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: err}
		}
		return formatSkillBody(skill, strings.TrimSpace(a.Task)), nil
	case skillActionEnd:
		// Ending is stateless here: the agent loop clears the
		// active skill (and refuses to end a skill that is not
		// active) before this result is ever used. A direct
		// registry caller just gets the confirmation.
		return fmt.Sprintf("Skill %q ended. Its tool restriction no longer applies; use any tool.", name), nil
	default:
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("unknown skill action %q (want %q or %q)", a.Action, skillActionInvoke, skillActionEnd)}
	}
}

// availableNames lists registered skill names for error messages,
// capped so a large data dir cannot flood the error text.
func (t *skillTool) availableNames() string {
	if t.reg == nil {
		return "(none)"
	}
	list := t.reg.List()
	if len(list) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(list))
	for i, s := range list {
		if i >= maxSkillNamesListed {
			names = append(names, fmt.Sprintf("... and %d more", len(list)-maxSkillNamesListed))
			break
		}
		names = append(names, s.Name)
	}
	return strings.Join(names, ", ")
}

// formatSkillBody renders an invoked skill for the model: the file
// body verbatim, then a footer with the allowed-tools restriction
// and the task, so the restriction is visible in the same result
// that activates the skill. A skill with no "## Tools" section
// declares no restriction (any tool may be used while it runs).
func formatSkillBody(skill skills.Skill, task string) string {
	var b strings.Builder
	b.WriteString(skill.Content)
	if !strings.HasSuffix(skill.Content, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\n---\n")
	fmt.Fprintf(&b, "[Skill %q is now active. ", skill.Name)
	if len(skill.Tools) == 0 {
		b.WriteString("It declares no allowed tools, so any tool may be used while it runs.")
	} else {
		fmt.Fprintf(&b, "While it runs, only these tools may be used: %s.", strings.Join(skill.Tools, ", "))
	}
	if task != "" {
		fmt.Fprintf(&b, " Task: %s", task)
	}
	b.WriteString(" Follow the skill instructions above.]")
	return b.String()
}
