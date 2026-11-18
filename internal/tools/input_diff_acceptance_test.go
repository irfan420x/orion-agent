// The screenshot-diff acceptance test (F-A23W2-9): a snapshot
// click on the already-focused window whose focus and element
// checks are both silent and whose screen does not change must
// NOT verify.
//
// This file uses only pre-existing seams — the fake spectacle
// below runs through the embedded screenshot tool's own
// execCmd/lookPath, and the desktop simulation is the same
// fakeInputEnv the older click tests use — so it also compiles
// and runs against the old code, where it FAILS: that failure
// is the proof the fix detects the bug (the old-code check in
// the evidence directory copies this file alone into a clean
// worktree at 57eae59). It asserts only fields the old summary
// already had (click_verified, warning): the new
// click_no_visible_effect field is pinned by the seam-based
// tests in input_test.go instead.
package tools

import (
	"bytes"
	"context"
	"image"
	"image/draw"
	"image/png"
	"os"
	"strings"
	"testing"
)

// diffScreenImage builds a 1920x1080 screen: white throughout,
// or with a black block inside the click crop (900,350)-
// (1000,450) when changed — a visible effect the diff must see.
// (The crop around the snapshot target (960,403) is
// (840,283)-(1080,523), so the block sits inside it.)
func diffScreenImage(t *testing.T, changed bool) *image.RGBA {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	if changed {
		draw.Draw(img, image.Rect(900, 350, 1000, 450), image.Black, image.Point{}, draw.Src)
	}
	return img
}

// fakeDiffSpectacle installs a fake spectacle backend on the
// embedded screenshot tool: --version succeeds, and every
// capture writes the next image from images (in order) as a
// PNG to the -o path. images must cover every capture the test
// performs (before + after).
func fakeDiffSpectacle(t *testing.T, env *fakeInputEnv, images ...*image.RGBA) {
	t.Helper()
	calls := 0
	env.tool.shot.lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	env.tool.shot.execCmd = func(ctx context.Context, name string, args ...string) (string, error) {
		if name != "spectacle" {
			t.Errorf("unexpected shot exec: %s %v", name, args)
			return "", nil
		}
		if len(args) == 1 && args[0] == "--version" {
			return "spectacle 1.0", nil
		}
		var out string
		for i, a := range args {
			if a == "-o" && i+1 < len(args) {
				out = args[i+1]
			}
		}
		if out == "" {
			t.Errorf("spectacle capture without -o: %v", args)
			return "", nil
		}
		if calls >= len(images) {
			t.Errorf("spectacle capture %d, want at most %d", calls+1, len(images))
			return "", nil
		}
		img := images[calls]
		calls++
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatalf("fake spectacle png encode: %v", err)
		}
		if err := os.WriteFile(out, buf.Bytes(), 0600); err != nil {
			t.Fatalf("fake spectacle write %s: %v", out, err)
		}
		return "", nil
	}
}

func TestInputSnapshotClickUnverifiedWhenNothingVisibleChanges(t *testing.T) {
	env := focusClickEnv(t)
	// Before the click the Username entry holds focus; the
	// press lands on a control that never takes focus, so the
	// focus stays put and the element check is silent — and
	// the screen does not change at all. On the old code this
	// click verifies (the focus/element checks have nothing
	// to demote on); with the diff it must not.
	env.tree = focusTreeJSON("Username", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	env.treeAfterClick = focusTreeJSON("Username", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	fakeDiffSpectacle(t, env, diffScreenImage(t, false), diffScreenImage(t, false))
	res := env.call(t, `{"op": "click", "snapshot_id": "g-test-1", "element_id": 1}`)
	s := parseInputSummary(t, res)
	if got := len(env.clickCalls()); got != 1 {
		t.Errorf("got %d click calls, want 1 (the click was sent)", got)
	}
	if s.ClickVerified {
		t.Errorf("click_verified = true with nothing on screen changing, want false (the click had no visible effect)")
	}
	if !strings.Contains(s.Warning, "no visible effect") {
		t.Errorf("warning = %q, want the 'no visible effect' note", s.Warning)
	}
}
