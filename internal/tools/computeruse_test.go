// Tests for the unattended-turn markers (F-A23W3-18 / F-M2R-18):
// the tools package and the permissions package each carry their
// own marker, set in two different places, and the tools must
// refuse computer-use when EITHER is set.
package tools

import (
	"context"
	"encoding/json"
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
