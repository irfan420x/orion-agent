// Package tools — computer-use shared rules (M1 screenshot; the
// input tools in M3+ reuse these).
//
// Two rules live here because every computer-use tool must enforce
// them the same way, in code, not in prompt text (plan §5.3/§11):
//
//   - Unattended turns: computer-use is OFF entirely in a cron /
//     scheduled turn — screenshot included, no exceptions (§11.3).
//     The daemon marks a scheduled turn's context with
//     WithUnattendedTurn (see cmd/oriond runScheduledTurn); a tool
//     that sees the mark refuses before doing anything.
//     There are TWO unattended markers in the codebase — this one
//     and permissions.WithUnattendedContext (set in runAgentTurn
//     when the turn is unattended) — and a future caller could set
//     only one of them (F-A23W3-18 / F-M2R-18). The two are
//     therefore unified HERE: WithUnattendedTurn sets both
//     markers, and UnattendedTurnFromContext reports true when
//     EITHER is set, so a computer-use tool can never silently
//     run in a turn the gate treats as unattended, or vice versa.
//     The reverse direction cannot be closed from the
//     permissions package (it cannot import tools — tools imports
//     permissions), so this file is the single unification point:
//     every scheduled-turn path sets the tools marker, which now
//     carries the permissions marker with it.
//   - Sensitive windows: when a password manager, bank/payment app
//     or similar is focused or visible, the tool refuses to
//     describe the screen at all — it never mentions, stores or
//     returns the sensitive content. The check is title matching
//     over the windows the platform can enumerate (XWayland via
//     xdotool today); native Wayland windows are invisible to that
//     path, which is a known gap the M2 safety layer must close
//     with the KWin window list — the tool reports whether the
//     check ran, so the gap is visible, never silent.
package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
)

type unattendedContextKey struct{}

// WithUnattendedTurn returns ctx marked as an unattended turn (a
// cron/scheduled turn with nobody present to approve or to see a
// portal dialog). Computer-use tools refuse on such a context.
//
// It sets BOTH unattended markers (F-A23W3-18 / F-M2R-18): this
// package's own marker and the permissions package's
// WithUnattendedContext, which the approval gate reads. A caller
// that marks a turn unattended through this function therefore
// marks it for the gate too, and the two markers cannot drift
// apart on this path.
func WithUnattendedTurn(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, unattendedContextKey{}, true)
	return permissions.WithUnattendedContext(ctx)
}

// UnattendedTurnFromContext reports whether ctx is marked as an
// unattended turn. It is false for every context that was never
// marked — interactive Telegram turns, CLI runs, tests.
//
// It honours EITHER unattended marker (F-A23W3-18 / F-M2R-18): this package's
// own WithUnattendedTurn marker, and the permission gate's
// WithUnattendedContext marker (permissions.UnattendedFrom). The
// two are set at different call sites — runScheduledTurn sets the
// tools marker, runAgentTurn(unattended=true) sets the gate marker
// — so a turn marked by only one of them used to pass the other
// package's check: a direct runAgentTurn(unattended=true) call
// (or any new scheduled path) would have the gate deny gated tools
// while screenshot/ground/input silently ran. Every computer-use
// tool reads this one function, so checking both here closes the
// gap for all three at once. (permissions does not import tools,
// so the reverse check — gate honouring the tools marker — would
// be an import cycle; the gate marker is the one runAgentTurn
// sets, and it is checked here. The reverse direction is closed
// from this side instead: WithUnattendedTurn above sets BOTH
// markers, so a caller that marks a turn unattended through this
// function marks it for the gate too.)
func UnattendedTurnFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if v, _ := ctx.Value(unattendedContextKey{}).(bool); v {
		return true
	}
	return permissions.UnattendedFrom(ctx)
}

// --- vision gate (F-M2R-3) ---
//
// ground and screenshot return an image the model must SEE: on a
// text-only provider/model the capture is pure waste — the image
// can never be read, and worse, a provider that silently
// substitutes "[Unsupported Image]" lets the model answer about
// a screen it never saw. The gate refuses the capture when the
// run's provider/model is known not to support vision.
//
// The tool cannot know the provider/model itself, so the app layer
// (which resolves the chat's provider/model per turn) marks the
// run context with WithVisionInfo before the agent run; the tools
// read it back here. A context that was never marked is "unknown"
// and the tools proceed — the gate only ever refuses on a KNOWN
// non-vision model, so CLI runs, tests and hand-built agents that
// never mark the context keep working.

type visionContextKey struct{}

type visionInfo struct {
	provider string
	model    string
	capable  bool
}

// WithVisionInfo returns ctx marked with the run's provider/model
// and whether that pair can see images (model.SupportsVision).
// The app layer calls this once per turn, before the agent run.
func WithVisionInfo(ctx context.Context, provider, modelName string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, visionContextKey{}, visionInfo{
		provider: provider,
		model:    modelName,
		capable:  model.SupportsVision(provider, modelName),
	})
}

// VisionInfoFromContext returns the provider/model vision info
// carried by ctx. known is false when the context was never marked
// with WithVisionInfo — the tools must then proceed (unknown is
// not a refusal).
func VisionInfoFromContext(ctx context.Context) (provider, modelName string, capable, known bool) {
	if ctx == nil {
		return "", "", false, false
	}
	info, _ := ctx.Value(visionContextKey{}).(visionInfo)
	if info.provider == "" && info.model == "" {
		return "", "", false, false
	}
	return info.provider, info.model, info.capable, true
}

// visionGateError is the computer-use vision gate: nil when the
// capture may proceed (vision-capable model, or an unmarked
// context where the model is unknown), otherwise a refusal that
// names the model so the user can switch to a vision-capable one.
func visionGateError(ctx context.Context, tool string) error {
	provider, modelName, capable, known := VisionInfoFromContext(ctx)
	if !known || capable {
		return nil
	}
	return fmt.Errorf("the current model %s/%s cannot see images (no vision support): %s refused — switch to a vision-capable model (e.g. deepseek-flash) to capture the screen", provider, modelName, tool)
}

// sensitiveWindowTitles are case-insensitive substrings of window
// titles that mark a window as sensitive: password managers,
// banks/payment/crypto apps, vaults and OTP/2FA dialogs. The list
// is deliberately narrow — a title match refuses the screenshot,
// so an over-broad entry would block ordinary work — and it is
// data, not a prompt hint: the screenshot tool checks it before
// capturing anything. Titles the platform cannot see (native
// Wayland windows under xdotool) are a known gap, not a pass.
var sensitiveWindowTitles = []string{
	// Password managers / vaults.
	"keepass", "bitwarden", "1password", "lastpass", "dashlane",
	"nordpass", "roboform", "enpass", "password manager", "vault",
	// Banks, payment, crypto.
	"bank", "banking", "chase", "hsbc", "dbs", "ocbc", "uob",
	"standard chartered", "paypal", "binance", "coinbase", "kraken",
	"ledger", "trezor",
	// One-time codes / second factors.
	"otp", "two-factor", "2fa", "authenticator",
}

// TitleLooksSensitive reports whether a window title matches the
// sensitive list (case-insensitive substring match). An empty title
// never matches.
func TitleLooksSensitive(title string) bool {
	lower := strings.ToLower(strings.TrimSpace(title))
	if lower == "" {
		return false
	}
	for _, needle := range sensitiveWindowTitles {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// FindSensitiveTitle returns the first title in titles that looks
// sensitive, or "" when none does. The returned title is used in
// the refusal message only as the matched category's evidence —
// the refusal names the category ("a password-manager window"),
// never the title itself, so a title that itself contains a
// secret is never echoed back to the model.
func FindSensitiveTitle(titles []string) string {
	for _, t := range titles {
		if TitleLooksSensitive(t) {
			return t
		}
	}
	return ""
}

// SensitiveTitleCategory names the category a sensitive title falls
// into, for refusal messages that must not quote the title itself.
func SensitiveTitleCategory(title string) string {
	lower := strings.ToLower(title)
	for _, needle := range []string{
		"keepass", "bitwarden", "1password", "lastpass", "dashlane",
		"nordpass", "roboform", "enpass", "password manager",
	} {
		if strings.Contains(lower, needle) {
			return "password manager"
		}
	}
	for _, needle := range []string{
		"bank", "banking", "chase", "hsbc", "dbs", "ocbc", "uob",
		"standard chartered", "paypal", "binance", "coinbase", "kraken",
		"ledger", "trezor",
	} {
		if strings.Contains(lower, needle) {
			return "bank / payment app"
		}
	}
	for _, needle := range []string{"otp", "two-factor", "2fa", "authenticator"} {
		if strings.Contains(lower, needle) {
			return "one-time-code / second-factor dialog"
		}
	}
	if strings.Contains(lower, "vault") {
		return "vault"
	}
	return "sensitive app"
}
