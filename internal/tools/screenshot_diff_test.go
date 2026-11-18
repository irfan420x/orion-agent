// Tests for the screenshot-diff signal (F-A23W2-9): the pixel
// comparison and its thresholds, the crop, the new-window
// guard, and the raw capture's failure modes. The end-to-end
// wiring (doClick demotes on no visible change) lives in
// input_test.go and in input_diff_acceptance_test.go — the
// latter uses only pre-existing seams, so it also runs against
// the old code as the acceptance check.
package tools

import (
	"context"
	"errors"
	"image"
	"image/color"
	"testing"
)

// diffTestImage builds a w×h image filled with fill.
func diffTestImage(w, h int, fill color.Color) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, fill)
		}
	}
	return img
}

// diffFillRect paints r in img with c, clipped to the image.
func diffFillRect(img *image.RGBA, r image.Rectangle, c color.Color) {
	r = r.Intersect(img.Bounds())
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			img.Set(x, y, c)
		}
	}
}

var (
	diffWhite = color.RGBA{255, 255, 255, 255}
	diffBlack = color.RGBA{0, 0, 0, 255}
)

func TestDiffVisibleChangeIdenticalImages(t *testing.T) {
	before := diffTestImage(100, 100, diffWhite)
	after := diffTestImage(100, 100, diffWhite)
	visible, fraction, err := diffVisibleChange(before, after, cursorPos{X: 50, Y: 50})
	if err != nil {
		t.Fatalf("diffVisibleChange = %v, want nil", err)
	}
	if visible || fraction != 0 {
		t.Errorf("visible = %v fraction = %v, want false/0 (identical images)", visible, fraction)
	}
}

func TestDiffVisibleChangeSinglePixel(t *testing.T) {
	before := diffTestImage(100, 100, diffWhite)
	after := diffTestImage(100, 100, diffWhite)
	after.Set(50, 50, diffBlack)
	visible, fraction, err := diffVisibleChange(before, after, cursorPos{X: 50, Y: 50})
	if err != nil {
		t.Fatalf("diffVisibleChange = %v, want nil", err)
	}
	if visible {
		t.Errorf("visible = true fraction = %v, want false (one pixel is noise, not an effect)", fraction)
	}
}

func TestDiffVisibleChangeBelowThreshold(t *testing.T) {
	// 40 px of a 100x100 crop (0.4%) is below the 0.5%
	// threshold: a clock digit or a caret blink must not
	// count as a visible change.
	before := diffTestImage(100, 100, diffWhite)
	after := diffTestImage(100, 100, diffWhite)
	diffFillRect(after, image.Rect(10, 10, 50, 11), diffBlack)
	visible, fraction, err := diffVisibleChange(before, after, cursorPos{X: 50, Y: 50})
	if err != nil {
		t.Fatalf("diffVisibleChange = %v, want nil", err)
	}
	if visible {
		t.Errorf("visible = true fraction = %v, want false (0.4%% changed is below the threshold)", fraction)
	}
}

func TestDiffVisibleChangeAboveThreshold(t *testing.T) {
	// 60 px of a 100x100 crop (0.6%) is above the 0.5%
	// threshold: a real state change counts.
	before := diffTestImage(100, 100, diffWhite)
	after := diffTestImage(100, 100, diffWhite)
	diffFillRect(after, image.Rect(10, 10, 70, 11), diffBlack)
	visible, fraction, err := diffVisibleChange(before, after, cursorPos{X: 50, Y: 50})
	if err != nil {
		t.Fatalf("diffVisibleChange = %v, want nil", err)
	}
	if !visible {
		t.Errorf("visible = false fraction = %v, want true (0.6%% changed is above the threshold)", fraction)
	}
}

func TestDiffVisibleChangeIgnoresTinyPerPixelDelta(t *testing.T) {
	// Every pixel shifts by 10 per channel (summed delta 30,
	// under the 32 threshold): sub-perceptual shimmer over the
	// whole crop must not count as a change.
	before := diffTestImage(100, 100, color.RGBA{100, 100, 100, 255})
	after := diffTestImage(100, 100, color.RGBA{110, 110, 110, 255})
	visible, fraction, err := diffVisibleChange(before, after, cursorPos{X: 50, Y: 50})
	if err != nil {
		t.Fatalf("diffVisibleChange = %v, want nil", err)
	}
	if visible || fraction != 0 {
		t.Errorf("visible = %v fraction = %v, want false/0 (a 10-per-channel shift is noise)", visible, fraction)
	}
}

func TestDiffVisibleChangeCatchesLargeDelta(t *testing.T) {
	// The same 60 px, but black-to-white (summed delta 765):
	// a real colour change counts even at the same size.
	before := diffTestImage(100, 100, diffBlack)
	after := diffTestImage(100, 100, diffBlack)
	diffFillRect(after, image.Rect(10, 10, 70, 11), diffWhite)
	visible, fraction, err := diffVisibleChange(before, after, cursorPos{X: 50, Y: 50})
	if err != nil {
		t.Fatalf("diffVisibleChange = %v, want nil", err)
	}
	if !visible {
		t.Errorf("visible = false fraction = %v, want true (a full black-to-white block is a real change)", fraction)
	}
}

func TestDiffVisibleChangeIgnoresChangeOutsideCrop(t *testing.T) {
	// A big change far from the target — outside the 240x240
	// crop around it — must not count: unrelated animation
	// elsewhere on screen is not the click's effect.
	before := diffTestImage(500, 500, diffWhite)
	after := diffTestImage(500, 500, diffWhite)
	diffFillRect(after, image.Rect(400, 400, 500, 500), diffBlack)
	visible, fraction, err := diffVisibleChange(before, after, cursorPos{X: 50, Y: 50})
	if err != nil {
		t.Fatalf("diffVisibleChange = %v, want nil", err)
	}
	if visible || fraction != 0 {
		t.Errorf("visible = %v fraction = %v, want false/0 (the change is outside the crop)", visible, fraction)
	}
}

func TestDiffVisibleChangeNearScreenEdge(t *testing.T) {
	// Target at (5, 5): the crop clamps to the screen instead
	// of going out of bounds, and a change inside the clamped
	// crop still counts.
	before := diffTestImage(200, 200, diffWhite)
	after := diffTestImage(200, 200, diffWhite)
	diffFillRect(after, image.Rect(0, 0, 30, 30), diffBlack)
	visible, _, err := diffVisibleChange(before, after, cursorPos{X: 5, Y: 5})
	if err != nil {
		t.Fatalf("diffVisibleChange = %v, want nil", err)
	}
	if !visible {
		t.Errorf("visible = false, want true (a change inside the edge-clamped crop counts)")
	}
	if visible2, _, err := diffVisibleChange(before, before, cursorPos{X: 5, Y: 5}); err != nil || visible2 {
		t.Errorf("identical edge images: visible = %v err = %v, want false/nil", visible2, err)
	}
}

func TestDiffVisibleChangeDifferentSizes(t *testing.T) {
	// The screen resized between captures: only the
	// overlapping crop is compared.
	before := diffTestImage(200, 200, diffWhite)
	after := diffTestImage(100, 100, diffWhite)
	diffFillRect(after, image.Rect(10, 10, 70, 11), diffBlack)
	visible, _, err := diffVisibleChange(before, after, cursorPos{X: 50, Y: 50})
	if err != nil {
		t.Fatalf("diffVisibleChange = %v, want nil", err)
	}
	if !visible {
		t.Errorf("visible = false, want true (the change is inside the overlapping crop)")
	}
}

func TestDiffVisibleChangeEmptyOverlapIsError(t *testing.T) {
	// The target is outside both captures: there is no crop
	// to compare, so the diff reports an error — no overlap
	// means no comparison, never a verdict.
	before := diffTestImage(10, 10, diffWhite)
	after := diffTestImage(10, 10, diffWhite)
	if _, _, err := diffVisibleChange(before, after, cursorPos{X: 1000, Y: 1000}); err == nil {
		t.Errorf("diffVisibleChange = nil error, want an error (no overlapping crop)")
	}
}

func TestDiffCropRectClampsToBounds(t *testing.T) {
	bounds := image.Rect(0, 0, 200, 200)
	if got := diffCropRect(cursorPos{X: 100, Y: 100}, 240, bounds); got != image.Rect(0, 0, 200, 200) {
		t.Errorf("centre crop = %v, want the whole 200x200 image (crop bigger than the screen clamps to it)", got)
	}
	if got := diffCropRect(cursorPos{X: 5, Y: 5}, 240, bounds); got != image.Rect(0, 0, 125, 125) {
		t.Errorf("edge crop = %v, want (0,0)-(125,125)", got)
	}
	if got := diffCropRect(cursorPos{X: 1000, Y: 1000}, 240, bounds); !got.Empty() {
		t.Errorf("outside crop = %v, want empty", got)
	}
}

func TestFocusMovedToNewWindow(t *testing.T) {
	before := []kwinWindow{
		{Class: "firefox-esr", FG: [4]int{0, 0, 1920, 1080}, Active: true},
		{Class: "kate", FG: [4]int{0, 0, 800, 600}},
	}
	newWin := kwinWindow{Class: "kdialog", FG: [4]int{100, 100, 400, 300}, Active: true}
	firefox := kwinWindow{Class: "firefox-esr", FG: [4]int{0, 0, 1920, 1080}, Active: true}
	movedWin := kwinWindow{Class: "firefox-esr", FG: [4]int{0, 0, 1920, 900}, Active: true}
	cases := []struct {
		name  string
		after []kwinWindow
		want  bool
	}{
		{"same window stays active", []kwinWindow{firefox, {Class: "kate", FG: [4]int{0, 0, 800, 600}}}, false},
		{"pre-existing window takes focus", []kwinWindow{{Class: "firefox-esr", FG: [4]int{0, 0, 1920, 1080}}, {Class: "kate", FG: [4]int{0, 0, 800, 600}, Active: true}}, false},
		{"new window takes focus", []kwinWindow{{Class: "firefox-esr", FG: [4]int{0, 0, 1920, 1080}}, {Class: "kate", FG: [4]int{0, 0, 800, 600}}, newWin}, true},
		{"no active window after", []kwinWindow{{Class: "firefox-esr", FG: [4]int{0, 0, 1920, 1080}}, {Class: "kate", FG: [4]int{0, 0, 800, 600}}}, false},
		{"unknown before list", []kwinWindow{firefox}, true}, // before is nilled below: any window looks new
		{"same class, resized frame", []kwinWindow{movedWin, {Class: "kate", FG: [4]int{0, 0, 800, 600}}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := before
			if tc.name == "unknown before list" {
				b = nil
			}
			if got := focusMovedToNewWindow(b, tc.after); got != tc.want {
				t.Errorf("focusMovedToNewWindow = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestScreenshotDiffCaptureScreenNoBackend(t *testing.T) {
	t.Setenv("DISPLAY", ":1")
	tool := newScreenshotTool()
	tool.lookPath = func(name string) (string, error) { return "", errors.New("fake lookPath failure") }
	if _, err := tool.captureScreen(context.Background()); err == nil {
		t.Errorf("captureScreen = nil error, want an error (no spectacle)")
	}
}

func TestScreenshotDiffCaptureScreenNoDisplay(t *testing.T) {
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	tool := newScreenshotTool()
	if _, err := tool.captureScreen(context.Background()); err == nil {
		t.Errorf("captureScreen = nil error, want an error (no graphical session)")
	}
}
