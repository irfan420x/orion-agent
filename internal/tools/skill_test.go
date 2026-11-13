package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/skills"
)

// skillTestRegistry builds a skills registry with one restricted
// skill ("adder": only the echo tool) and one unrestricted skill
// ("wide": no Tools section).
func skillTestRegistry(t *testing.T) *skills.Registry {
	t.Helper()
	reg := skills.NewRegistry()
	if err := reg.Register(skills.Skill{
		Name:        "adder",
		Description: "Adds numbers with the echo tool.",
		Content:     "# Adder\nADD-BODY-MARKER-9z\nUse the echo tool to say the answer.",
		Tools:       []string{"echo"},
	}); err != nil {
		t.Fatalf("Register adder: %v", err)
	}
	if err := reg.Register(skills.Skill{
		Name:        "wide",
		Description: "A skill with no tool restriction.",
		Content:     "# Wide\nWIDE-BODY-MARKER-4q\nDo anything.",
	}); err != nil {
		t.Fatalf("Register wide: %v", err)
	}
	return reg
}

func TestNewSkillToolRejectsNilRegistry(t *testing.T) {
	if _, err := NewSkillTool(nil); err == nil {
		t.Fatal("NewSkillTool(nil): expected error, got nil")
	}
}

func TestSkillToolInvokeReturnsBodyAndRestriction(t *testing.T) {
	tool, err := NewSkillTool(skillTestRegistry(t))
	if err != nil {
		t.Fatalf("NewSkillTool: %v", err)
	}
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"adder","task":"add 6 and 7"}`))
	if err != nil {
		t.Fatalf("Execute(invoke): %v", err)
	}
	for _, want := range []string{
		"ADD-BODY-MARKER-9z",                 // the body, verbatim
		"Use the echo tool to say",           // body content
		"only these tools may be used: echo", // allowed-tools footer
		"Task: add 6 and 7",                  // the task, echoed back
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("result %q does not contain %q", out, want)
		}
	}
}

func TestSkillToolInvokeUnrestrictedSkill(t *testing.T) {
	tool, err := NewSkillTool(skillTestRegistry(t))
	if err != nil {
		t.Fatalf("NewSkillTool: %v", err)
	}
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"wide"}`))
	if err != nil {
		t.Fatalf("Execute(invoke wide): %v", err)
	}
	if !strings.Contains(out, "WIDE-BODY-MARKER-4q") {
		t.Fatalf("result %q does not contain the wide body", out)
	}
	if !strings.Contains(out, "no allowed tools") {
		t.Fatalf("result %q does not state the skill declares no restriction", out)
	}
}

func TestSkillToolUnknownSkill(t *testing.T) {
	tool, err := NewSkillTool(skillTestRegistry(t))
	if err != nil {
		t.Fatalf("NewSkillTool: %v", err)
	}
	_, err = tool.Execute(context.Background(), json.RawMessage(`{"name":"nope"}`))
	if err == nil {
		t.Fatal("Execute(unknown skill): expected error, got nil")
	}
	var te *ToolError
	if !errors.As(err, &te) || te.Kind != "invalid_args" {
		t.Fatalf("err = %v, want invalid_args ToolError", err)
	}
	if !strings.Contains(err.Error(), "unknown skill") || !strings.Contains(err.Error(), "adder") {
		t.Fatalf("err = %q, want it to name the unknown skill and list the available ones", err.Error())
	}
}

func TestSkillToolEnd(t *testing.T) {
	tool, err := NewSkillTool(skillTestRegistry(t))
	if err != nil {
		t.Fatalf("NewSkillTool: %v", err)
	}
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"adder","action":"end"}`))
	if err != nil {
		t.Fatalf("Execute(end): %v", err)
	}
	if !strings.Contains(out, "ended") {
		t.Fatalf("result %q does not confirm the skill ended", out)
	}
}

func TestSkillToolValidationThroughRegistry(t *testing.T) {
	r := NewRegistry()
	tool, err := NewSkillTool(skillTestRegistry(t))
	if err != nil {
		t.Fatalf("NewSkillTool: %v", err)
	}
	if err := r.Register(tool); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ctx := context.Background()

	// Missing required "name" is a clean invalid_args error from
	// central validation — never a panic, never a half-run.
	_, err = r.Execute(ctx, "skill", json.RawMessage(`{}`))
	assertInvalidArgs(t, err, `missing required parameter "name"`)

	// An action outside the enum is rejected the same way.
	_, err = r.Execute(ctx, "skill", json.RawMessage(`{"name":"adder","action":"explode"}`))
	assertInvalidArgs(t, err, `parameter "action" must be one of`)

	// A wrong-typed name is rejected before the tool runs.
	_, err = r.Execute(ctx, "skill", json.RawMessage(`{"name":7}`))
	assertInvalidArgs(t, err, `parameter "name" must be string, got integer`)

	// A valid invoke through the registry returns the body.
	out, err := r.Execute(ctx, "skill", json.RawMessage(`{"name":"adder"}`))
	if err != nil {
		t.Fatalf("Execute(valid invoke): %v", err)
	}
	if !strings.Contains(out, "ADD-BODY-MARKER-9z") {
		t.Fatalf("result %q does not contain the skill body", out)
	}
}
