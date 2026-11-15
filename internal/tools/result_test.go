// Tests for image-bearing tool results (computer-use M1,
// plan §4.7-1): ToolResult caps, ExecuteResult dispatch, and the
// legacy Execute adapter.
package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestToolResultValidate(t *testing.T) {
	good := ToolResult{Text: "ok", Images: []ToolImage{{Data: []byte{1, 2, 3}, MIMEType: "image/png"}}}
	if err := good.Validate(); err != nil {
		t.Errorf("Validate(good) = %v, want nil", err)
	}
	if err := TextOnly("x").Validate(); err != nil {
		t.Errorf("Validate(text-only) = %v, want nil", err)
	}
	tooMany := ToolResult{Images: make([]ToolImage, MaxImagesPerResult+1)}
	for i := range tooMany.Images {
		tooMany.Images[i] = ToolImage{Data: []byte{1}, MIMEType: "image/png"}
	}
	if err := tooMany.Validate(); err == nil {
		t.Errorf("Validate(%d images) = nil, want an error", MaxImagesPerResult+1)
	}
	tooBig := ToolResult{Images: []ToolImage{{Data: make([]byte, MaxImageBytes+1), MIMEType: "image/jpeg"}}}
	if err := tooBig.Validate(); err == nil {
		t.Errorf("Validate(over-cap image) = nil, want an error")
	}
	empty := ToolResult{Images: []ToolImage{{Data: nil, MIMEType: "image/png"}}}
	if err := empty.Validate(); err == nil {
		t.Errorf("Validate(empty image) = nil, want an error")
	}
	badType := ToolResult{Images: []ToolImage{{Data: []byte{1}, MIMEType: "image/gif"}}}
	if err := badType.Validate(); err == nil {
		t.Errorf("Validate(gif image) = nil, want an error")
	}
}

// TestExecuteResultWrapsLegacyTools pins the adapter: a text-only
// tool run through ExecuteResult returns its text with no images.
// The ImageResultTool dispatch itself is covered by the screenshot
// tests (a registered factory tool returning images).
func TestExecuteResultWrapsLegacyTools(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	res, err := reg.ExecuteResult(context.Background(), "calculator", json.RawMessage(`{"expression":"1+1"}`))
	if err != nil {
		t.Fatalf("ExecuteResult(calculator) = %v, want nil", err)
	}
	if len(res.Images) != 0 {
		t.Errorf("ExecuteResult(calculator) carried %d images, want 0", len(res.Images))
	}
	if !strings.Contains(res.Text, "2") {
		t.Errorf("ExecuteResult(calculator) text = %q, want it to contain 2", res.Text)
	}
}

func TestExecuteStillReturnsTextOnly(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	text, err := reg.Execute(context.Background(), "calculator", json.RawMessage(`{"expression":"2+3"}`))
	if err != nil {
		t.Fatalf("Execute(calculator) = %v, want nil", err)
	}
	if !strings.Contains(text, "5") {
		t.Errorf("Execute(calculator) = %q, want it to contain 5", text)
	}
}

func TestExecuteResultRejectsUnknownTool(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	if _, err := reg.ExecuteResult(context.Background(), "no_such_tool", json.RawMessage(`{}`)); err == nil {
		t.Errorf("ExecuteResult(unknown) = nil, want unknown_tool error")
	} else if terr, ok := err.(*ToolError); !ok || terr.Kind != "unknown_tool" {
		t.Errorf("ExecuteResult(unknown) error = %v, want kind unknown_tool", err)
	}
}

func TestExecuteResultRejectsOverCapImages(t *testing.T) {
	// A tool that returns an over-cap image must fail loudly at
	// the registry, never silently drop the image. The fake is
	// registered through the factory path of a hand-built
	// registry: Register rejects it (not in the gate maps), so
	// exercise Validate directly as the registry does — the
	// registry call is covered by the screenshot shrink tests.
	big := ToolResult{Text: "x", Images: []ToolImage{{Data: make([]byte, MaxImageBytes+1), MIMEType: "image/png"}}}
	if err := big.Validate(); err == nil {
		t.Fatalf("Validate(over-cap) = nil, want an error")
	}
}

func TestRegistryBeginTurnIsNilSafe(t *testing.T) {
	var r *Registry
	r.BeginTurn() // must not panic
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	reg.BeginTurn() // no turn-scoped tools except screenshot; must not fail
}

func TestTitleLooksSensitive(t *testing.T) {
	sensitive := []string{
		"KeePassXC", "Bitwarden", "1Password", "Chase Bank",
		"DBS iBanking", "PayPal", "Google Authenticator",
		"My Vault", "OTP verification",
	}
	for _, title := range sensitive {
		if !TitleLooksSensitive(title) {
			t.Errorf("TitleLooksSensitive(%q) = false, want true", title)
		}
	}
	ordinary := []string{
		"Firefox", "Kate — notes.txt", "Terminal", "Calculator",
		"GitHub — orion-agent", "Telegram",
	}
	for _, title := range ordinary {
		if TitleLooksSensitive(title) {
			t.Errorf("TitleLooksSensitive(%q) = true, want false", title)
		}
	}
	if got := SensitiveTitleCategory("KeePassXC"); got != "password manager" {
		t.Errorf("SensitiveTitleCategory(KeePassXC) = %q, want %q", got, "password manager")
	}
	if got := SensitiveTitleCategory("Chase Bank"); got != "bank / payment app" {
		t.Errorf("SensitiveTitleCategory(Chase Bank) = %q, want %q", got, "bank / payment app")
	}
	// The refusal names the category, never the title itself.
	if hit := FindSensitiveTitle([]string{"Firefox", "Bitwarden vault"}); hit == "" {
		t.Errorf("FindSensitiveTitle found nothing, want the Bitwarden title")
	}
}

func TestUnattendedTurnContext(t *testing.T) {
	ctx := context.Background()
	if UnattendedTurnFromContext(ctx) {
		t.Errorf("UnattendedTurnFromContext(background) = true, want false")
	}
	if UnattendedTurnFromContext(nil) {
		t.Errorf("UnattendedTurnFromContext(nil) = true, want false")
	}
	marked := WithUnattendedTurn(ctx)
	if !UnattendedTurnFromContext(marked) {
		t.Errorf("UnattendedTurnFromContext(marked) = false, want true")
	}
}
