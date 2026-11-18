// Package tools — screenshot-diff verification for input clicks
// (F-A23W2-9, scoped fix 2026-11-18).
//
// The problem: after the narrow round, a click that reaches the
// already-focused window but has NO visible effect still
// verifies true. The focus check demotes only when the target
// sits in a different window than the focused one, and the
// element check demotes only when another control demonstrably
// takes focus — both need positive evidence of a miss, and a
// dead button, an already-selected tab, or a press on a control
// that never takes focus produces no evidence at all.
//
// The third signal: compare a small crop around the click
// target BEFORE the click with the same crop AFTER it. A click
// that changed nothing visible demotes with the distinct reason
// "no visible effect" — never "missed": the press may have
// landed on a control whose effect is invisible (placing the
// text caret, for one — the pointer is NOT rendered in
// screenshots on this machine, so a caret move is invisible
// by construction), and the result must say that, not claim a
// miss. There is no auto-retry: the warning tells the model to
// verify on screen before clicking again.
//
// Scope, exactly as decided (ORION-F-A23W2-9-DECISION.md):
//   - The diff runs ONLY when the focus + element checks are
//     silent (no positive miss evidence either way) AND the
//     target did not itself take focus (a focus-taking target
//     is positive hit evidence, not silence — the click
//     demonstrably reached the control, e.g. click-to-focus a
//     text field, whose effect is invisible in a screenshot).
//     It never resurrects a click the other checks demoted.
//   - A click that opened a NEW window (the post-click active
//     window was absent before the click) skips the diff: the
//     new window is itself the click's effect, and a crop taken
//     in the old window cannot judge it.
//   - Any failure of the signal — budget exhausted, no
//     backend, a capture error — is NO signal: the cursor
//     verdict stands. The diff can only demote, never verify
//     on its own.
//   - Comparison is local, Go stdlib only, no model call, no
//     image sent anywhere. The capture is in-memory only: the
//     image is compared and discarded immediately, never
//     stored, never logged, never transmitted.
//
// What the diff does NOT prove (documented honestly):
//   - Case 2 (cross-window): a click that lands in a wrong
//     pre-existing window is only NARROWED, not closed. The
//     diff demotes it when the wrong window does not cover the
//     crop around the target; when it does cover the crop, the
//     pixels change and the click still verifies. Focus/window
//     identity is still read alongside for exactly this reason.
//   - A click whose target took focus but did nothing visible
//     (a button that takes focus yet opens no dialog) is
//     exempted by the hit rule above — focus is itself the
//     effect evidence, and the diff cannot second-guess it.
//   - False demotion: a legitimate click with no visible
//     effect (already-focused text field, already-selected
//     tab) demotes. That is the honest report — nothing
//     changed — and only a warning, never a refusal.
//   - False pass: unrelated animation inside the crop (a
//     spinner, a progress bar, a clock digit) counts as a
//     visible change. The thresholds below are tuned so small
//     noise stays below them, but animation is animation.
//
// Thresholds (the ONE tuning decision — picked here,
// documented here, pinned by tests):
//   - Per-pixel delta: a pixel counts as changed when the
//     summed absolute channel difference (|dR|+|dG|+|dB|,
//     8-bit channels, max 765) exceeds 32 — about 4% of full
//     scale. Spectacle captures lossless PNG, so there is no
//     compression noise to ignore; 32 ignores sub-perceptual
//     shimmer (anti-aliasing, subpixel rendering) while any
//     real state change (pressed colour, selection highlight,
//     a glyph appearing) shifts channels by far more.
//   - Changed fraction: the crop counts as visibly changed
//     when at least 0.5% of its pixels changed — 288 px of a
//     240x240 crop. A pressed-button recolour or a menu opening
//     changes thousands of pixels; a clock digit (~100-150 px)
//     or a blinking text caret (~5-20 px) stays below it. A
//     small checkbox checkmark (~100 px, no focus ring) sits
//     below it too and false-demotes — accepted, and mitigated
//     in practice by the hit exemption for focusable controls.
//   - Crop: 240x240 px centred on the click target, clamped
//     to the captured screen. Big enough to catch a menu or
//     dialog opening beside the target, small enough that
//     unrelated animation elsewhere on screen cannot pass it.
package tools

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"time"
)

// Screenshot-diff tuning (F-A23W2-9): see the package comment
// above for the reasoning behind each number. Change a number
// only with a test that pins the new boundary.
const (
	diffCropSize        = 240                    // crop edge in px, centred on the click target
	diffPixelDelta      = 32                     // summed |dR|+|dG|+|dB| (8-bit, max 765) that marks one pixel changed
	diffChangedFraction = 0.005                  // fraction of crop pixels changed that counts as a visible change
	diffSettle          = 300 * time.Millisecond // minimum settle after the click before the after-capture
)

// diffCropRect is the crop rectangle around target, clamped to
// the captured screen bounds. A target near the screen edge
// gets a smaller crop, never an out-of-bounds one; a target
// outside the capture entirely gets an empty rectangle.
func diffCropRect(target cursorPos, size int, bounds image.Rectangle) image.Rectangle {
	half := size / 2
	r := image.Rect(target.X-half, target.Y-half, target.X+half, target.Y+half)
	return r.Intersect(bounds)
}

// diffVisibleChange compares the crops around target in the
// before/after captures and reports whether anything visible
// changed, plus the changed fraction (for tests and
// diagnostics). The two captures may differ in size (the
// screen can resize between them): only their overlapping
// crop is compared, and an empty overlap is an error — no
// overlap means no comparison, never a verdict.
func diffVisibleChange(before, after image.Image, target cursorPos) (visible bool, fraction float64, err error) {
	rect := diffCropRect(target, diffCropSize, before.Bounds()).Intersect(
		diffCropRect(target, diffCropSize, after.Bounds()))
	if rect.Empty() {
		return false, 0, fmt.Errorf("no overlapping crop around the click point (%d, %d)", target.X, target.Y)
	}
	total := rect.Dx() * rect.Dy()
	changed := 0
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			if diffPixelChanged(before.At(x, y), after.At(x, y)) {
				changed++
			}
		}
	}
	fraction = float64(changed) / float64(total)
	return fraction >= diffChangedFraction, fraction, nil
}

// diffPixelChanged reports whether one pixel changed enough to
// count: the summed 8-bit channel difference exceeds
// diffPixelDelta. RGBA() returns 16-bit channels; the low byte
// is sub-perceptual noise, so only the high byte is compared.
func diffPixelChanged(before, after color.Color) bool {
	br, bg, bb, _ := before.RGBA()
	ar, ag, ab, _ := after.RGBA()
	d := diffChannelDelta(br, ar) + diffChannelDelta(bg, ag) + diffChannelDelta(bb, ab)
	return d > diffPixelDelta
}

// diffChannelDelta is the absolute difference of one 16-bit
// channel, reduced to 8-bit.
func diffChannelDelta(a, b uint32) int {
	da, db := int(a>>8), int(b>>8)
	if da >= db {
		return da - db
	}
	return db - da
}

// captureScreen captures the desktop with spectacle and returns
// the decoded full-screen image. It is the low-level capture
// behind the input tool's screenshot-diff signal ONLY: unlike
// screenshot's ExecuteResult it stores no file, returns no
// image to any caller outside this process, runs no vision gate
// and no sensitive-window check — the pixels are compared in
// memory and discarded immediately.
//
// The missing sensitive check is deliberate, not an oversight:
// screenshot's check is xdotool titles (XWayland only), and on
// a native-Wayland screen it fails closed to "unavailable",
// which would silently disable the diff wherever there is no
// XWayland window. The diff needs no title check because its
// image never leaves the process — nothing is stored,
// transmitted or logged, only a single changed/unchanged bit
// derived from the whole crop — and the click target itself was
// already sensitivity-checked by checkClickSensitive before the
// click (a sensitive window anywhere on screen refuses the
// click unless every sensitive box is precisely known and the
// target lies outside all of them). A crop may include edge
// pixels of an adjacent sensitive window; those pixels are
// compared in memory and dropped with the image.
//
// It charges NO budget itself: its only caller (input's
// tryDiffCapture) charges the shared capture budget per
// capture, so the quota stays in one place. Pre-flight failures
// (no session, no backend) happen before any charge, so the
// caller only ever pays for a capture it attempted.
func (t *screenshotTool) captureScreen(ctx context.Context) (image.Image, error) {
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return nil, fmt.Errorf("no graphical session available: neither DISPLAY nor WAYLAND_DISPLAY is set")
	}
	spectaclePath, err := t.lookPath("spectacle")
	if err != nil || spectaclePath == "" {
		return nil, fmt.Errorf("spectacle is not installed (no screenshot backend on this machine)")
	}
	if _, err := t.execCmd(ctx, "spectacle", "--version"); err != nil {
		return nil, fmt.Errorf("spectacle is installed but does not run (--version failed: %v)", err)
	}
	dir, err := t.screenshotDir()
	if err != nil {
		return nil, fmt.Errorf("cannot resolve the screenshot directory: %v", err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("cannot create the screenshot directory: %v", err)
	}
	t.pruneOld(dir)
	stamp := t.now().UTC().Format("20060102-150405")
	rawPath := filepath.Join(dir, fmt.Sprintf("capture-%s-%d.png", stamp, rand.Intn(100000)))
	out, err := t.execCmd(ctx, "spectacle", "-b", "-n", "-o", rawPath)
	if err != nil {
		return nil, fmt.Errorf("spectacle failed: %v (output: %s)", err, out)
	}
	defer os.Remove(rawPath)
	f, err := os.Open(rawPath)
	if err != nil {
		return nil, fmt.Errorf("spectacle produced no image file at %s", rawPath)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("cannot decode the captured image: %v", err)
	}
	return img, nil
}
