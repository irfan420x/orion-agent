// Tests for the computer-use shared rules (F-A23W3-18 / F-M2R-18,
// F-M2R-3): the unattended-turn markers, set in two different
// places, and the vision gate — the tools must refuse computer-use
// when EITHER unattended marker is set, and ground/screenshot must
// refuse on a known non-vision model.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/permissions"
)

func TestUnattendedMarkersUnified(t *testing.T) {
	bg := context.Background()

	// Neither marker: interactive.
	if UnattendedTurnFromContext(bg) {
		t.Errorf("UnattendedTurnFromContext(background) = true, want false")
	}
	if UnattendedTurnFromContext(nil) {
		t.Errorf("UnattendedTurnFromContext(nil) = true, want false")
	}
	if permissions.UnattendedFrom(bg) {
		t.Errorf("permissions.UnattendedFrom(background) = true, want false")
	}

	// Tools marker only (the old scheduled path): both readers see it.
	toolsOnly := context.WithValue(bg, unattendedContextKey{}, true)
	if !UnattendedTurnFromContext(toolsOnly) {
		t.Errorf("UnattendedTurnFromContext(tools marker) = false, want true")
	}

	// Permissions marker only (a future direct
	// runAgentTurn(unattended=true) call, which never wraps the
	// context with WithUnattendedTurn): the tools MUST still see
	// the turn as unattended — this is the F-A23W3-18 hole.
	permsOnly := permissions.WithUnattendedContext(bg)
	if !UnattendedTurnFromContext(permsOnly) {
		t.Errorf("UnattendedTurnFromContext(permissions marker) = false, want true (F-A23W3-18)")
	}

	// WithUnattendedTurn sets BOTH markers, so the gate sees a
	// tools-marked turn as unattended too.
	both := WithUnattendedTurn(bg)
	if !UnattendedTurnFromContext(both) {
		t.Errorf("UnattendedTurnFromContext(WithUnattendedTurn) = false, want true")
	}
	if !permissions.UnattendedFrom(both) {
		t.Errorf("permissions.UnattendedFrom(WithUnattendedTurn) = false, want true")
	}
}

func TestComputerUseRefusesPermissionsOnlyMarker(t *testing.T) {
	// A context carrying ONLY the permissions marker must refuse
	// every computer-use tool, exactly as a fully marked
	// unattended turn does.
	ctx := permissions.WithUnattendedContext(context.Background())

	shotEnv := newFakeScreenshotEnv(t)
	if _, err := shotEnv.tool.ExecuteResult(ctx, json.RawMessage(`{}`)); err == nil {
		t.Errorf("screenshot with permissions-only marker = nil, want a denial")
	} else if terr, ok := err.(*ToolError); !ok || terr.Kind != "denied" {
		t.Errorf("screenshot with permissions-only marker error = %v, want kind denied", err)
	}
	if shotEnv.captures != 0 {
		t.Errorf("screenshot ran %d captures on a permissions-only unattended turn, want 0", shotEnv.captures)
	}

	groundEnv := newFakeGroundEnv(t)
	if _, err := groundEnv.tool.ExecuteResult(ctx, json.RawMessage(`{}`)); err == nil {
		t.Errorf("ground with permissions-only marker = nil, want a denial")
	} else if terr, ok := err.(*ToolError); !ok || terr.Kind != "denied" {
		t.Errorf("ground with permissions-only marker error = %v, want kind denied", err)
	}
	if groundEnv.captures != 0 {
		t.Errorf("ground captured %d screenshot(s) on a permissions-only unattended turn, want 0", groundEnv.captures)
	}

	inputEnv := newFakeInputEnv(t)
	if _, err := inputEnv.tool.ExecuteResult(ctx, json.RawMessage(`{"op": "move", "x": 200, "y": 200}`)); err == nil {
		t.Errorf("input with permissions-only marker = nil, want a denial")
	} else if terr, ok := err.(*ToolError); !ok || terr.Kind != "denied" {
		t.Errorf("input with permissions-only marker error = %v, want kind denied", err)
	}
	if len(inputEnv.ydotoolCalls) != 0 {
		t.Errorf("input made %d ydotool call(s) on a permissions-only unattended turn, want 0", len(inputEnv.ydotoolCalls))
	}
}

func TestVisionInfoContext(t *testing.T) {
	// Unmarked context: unknown — the gate must not refuse.
	if _, _, _, known := VisionInfoFromContext(context.Background()); known {
		t.Errorf("unmarked context known = true, want false")
	}
	if _, _, _, known := VisionInfoFromContext(nil); known {
		t.Errorf("nil context known = true, want false")
	}
	if err := visionGateError(context.Background(), "ground"); err != nil {
		t.Errorf("visionGateError(unmarked) = %v, want nil", err)
	}
	cases := []struct {
		provider, model string
		capable         bool
	}{
		{"deepseek", "deepseek-flash", true},
		{"deepseek", "deepseek-chat", false},
		{"deepseek", "deepseek-v4-pro", false},
		{"openai", "gpt-4o", true},
		{"openai", "gpt-3.5-turbo", false},
		{"ollama", "llama3.1", false},
		{"anthropic", "claude-sonnet-4-5", false},
		{"unknown-provider", "whatever", false},
	}
	for _, c := range cases {
		ctx := WithVisionInfo(context.Background(), c.provider, c.model)
		provider, modelName, capable, known := VisionInfoFromContext(ctx)
		if !known {
			t.Errorf("WithVisionInfo(%q, %q): known = false, want true", c.provider, c.model)
		}
		if provider != c.provider || modelName != c.model {
			t.Errorf("WithVisionInfo(%q, %q): got (%q, %q)", c.provider, c.model, provider, modelName)
		}
		if capable != c.capable {
			t.Errorf("WithVisionInfo(%q, %q): capable = %v, want %v", c.provider, c.model, capable, c.capable)
		}
		if err := visionGateError(ctx, "screenshot"); (err != nil) == c.capable {
			t.Errorf("visionGateError(%q, %q) = %v, want the opposite of capable=%v", c.provider, c.model, err, c.capable)
		}
	}
	// The refusal names the model, so the user knows what to switch.
	ctx := WithVisionInfo(context.Background(), "deepseek", "deepseek-chat")
	err := visionGateError(ctx, "ground")
	if err == nil || !strings.Contains(err.Error(), "deepseek-chat") {
		t.Errorf("vision refusal = %v, want it to name deepseek-chat", err)
	}
}

// TestGroundVisionGateRefused pins F-M2R-3 for ground: on a
// text-only model the grounding is refused before anything is
// captured — the image would be unreadable, so capturing it is
// pure waste (and a silent "[Unsupported Image]" substitution
// would let the model answer about a screen it never saw).
func TestGroundVisionGateRefused(t *testing.T) {
	resetGroundSnapshots()
	env := newFakeGroundEnv(t)
	ctx := WithVisionInfo(context.Background(), "deepseek", "deepseek-chat")
	_, err := env.tool.ExecuteResult(ctx, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("ground on a text-only model = nil, want a denial")
	}
	var terr *ToolError
	if !errors.As(err, &terr) || terr.Kind != "denied" {
		t.Errorf("error = %v, want denied ToolError", err)
	}
	if !strings.Contains(err.Error(), "cannot see images") {
		t.Errorf("refusal = %q, want the no-vision explanation", err.Error())
	}
	if env.captures != 0 {
		t.Errorf("vision-gated grounding still captured %d screenshot(s)", env.captures)
	}
	// A vision-capable model on the same env proceeds.
	visionCtx := WithVisionInfo(context.Background(), "deepseek", "deepseek-flash")
	res, err := env.tool.ExecuteResult(visionCtx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("ground on deepseek-flash = %v, want nil", err)
	}
	if s := parseGroundSummary(t, res); s.Coverage != "grounded" {
		t.Errorf("coverage on deepseek-flash = %q, want grounded", s.Coverage)
	}
}

// TestScreenshotVisionGateRefused pins F-M2R-3 for screenshot:
// same gate, same refusal, no capture on a text-only model.
func TestScreenshotVisionGateRefused(t *testing.T) {
	env := newFakeScreenshotEnv(t)
	ctx := WithVisionInfo(context.Background(), "openai", "gpt-3.5-turbo")
	_, err := env.tool.ExecuteResult(ctx, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("screenshot on a text-only model = nil, want a denial")
	}
	var terr *ToolError
	if !errors.As(err, &terr) || terr.Kind != "denied" {
		t.Errorf("error = %v, want denied ToolError", err)
	}
	if env.captures != 0 {
		t.Errorf("vision-gated screenshot still captured %d time(s)", env.captures)
	}
	visionCtx := WithVisionInfo(context.Background(), "openai", "gpt-4o")
	if _, err := env.tool.ExecuteResult(visionCtx, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("screenshot on gpt-4o = %v, want nil", err)
	}
}
