// Tests for the screenshot tool (computer-use M1). The real
// backends (spectacle, xdotool, loginctl) never run here: every
// seam is stubbed, so the tests run on a headless VM. Live capture
// is verified on the laptop instead (see
// ~/workspace/orion-verification/computer-use-m1/).
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeScreenshotEnv is a stubbed screenshot tool plus its clock.
type fakeScreenshotEnv struct {
	tool     *screenshotTool
	clock    time.Time
	home     string
	png      []byte // 1920x1080 test capture
	captures int
}

func makeTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, image.Black)
			// A little variation so the JPEG is not degenerate.
			if (x+y)%7 == 0 {
				img.Set(x, y, image.White)
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test PNG: %v", err)
	}
	return buf.Bytes()
}

func newFakeScreenshotEnv(t *testing.T) *fakeScreenshotEnv {
	t.Helper()
	t.Setenv("DISPLAY", ":1")
	home := t.TempDir()
	env := &fakeScreenshotEnv{
		tool:  newScreenshotTool(),
		clock: time.Now(),
		home:  home,
		png:   makeTestPNG(t, 1920, 1080),
	}
	tool := env.tool
	tool.homeDir = func() (string, error) { return home, nil }
	tool.now = func() time.Time { return env.clock }
	tool.lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	tool.execCmd = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "spectacle" && len(args) == 1 && args[0] == "--version" {
			return "spectacle 6.6.5\n", nil
		}
		if name == "spectacle" {
			// -b -n -o <path>: write the test capture.
			out := args[len(args)-1]
			env.captures++
			if err := os.WriteFile(out, env.png, 0600); err != nil {
				t.Errorf("fake spectacle write: %v", err)
			}
			return "", nil
		}
		t.Errorf("unexpected exec: %s %v", name, args)
		return "", nil
	}
	tool.windowTitles = func(ctx context.Context) ([]string, error) {
		return []string{"Firefox", "Kate — notes.txt"}, nil
	}
	tool.activeWindow = func(ctx context.Context) (string, error) {
		return "Kate — notes.txt", nil
	}
	tool.cursor = func(ctx context.Context) (cursorPos, bool) {
		return cursorPos{X: 960, Y: 540}, true
	}
	tool.screenLocked = func(ctx context.Context) (bool, bool) { return false, true }
	return env
}

// advance moves the fake clock past the 1s min interval.
func (e *fakeScreenshotEnv) advance() { e.clock = e.clock.Add(2 * time.Second) }

func (e *fakeScreenshotEnv) call(t *testing.T, args string) ToolResult {
	t.Helper()
	res, err := e.tool.ExecuteResult(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("ExecuteResult(%s) = %v, want nil", args, err)
	}
	return res
}

func parseSummary(t *testing.T, res ToolResult) screenshotSummary {
	t.Helper()
	var s screenshotSummary
	if err := json.Unmarshal([]byte(res.Text), &s); err != nil {
		t.Fatalf("summary JSON %q does not parse: %v", res.Text, err)
	}
	return s
}

func TestScreenshotHappyPath(t *testing.T) {
	env := newFakeScreenshotEnv(t)
	res := env.call(t, `{}`)
	if len(res.Images) != 1 {
		t.Fatalf("got %d images, want 1", len(res.Images))
	}
	img := res.Images[0]
	if img.MIMEType != "image/jpeg" {
		t.Errorf("media type = %q, want image/jpeg", img.MIMEType)
	}
	if len(img.Data) == 0 || len(img.Data) > MaxImageBytes {
		t.Errorf("image size = %d, want (0, %d]", len(img.Data), MaxImageBytes)
	}
	s := parseSummary(t, res)
	if s.Backend != "spectacle" {
		t.Errorf("backend = %q, want spectacle", s.Backend)
	}
	// 1920x1080 scaled to the default max_width 1280.
	if s.Width != 1280 || s.Height != 720 {
		t.Errorf("size = %dx%d, want 1280x720", s.Width, s.Height)
	}
	if s.Format != "jpeg" || s.SizeBytes != len(img.Data) {
		t.Errorf("summary format/size = %q/%d, want jpeg/%d", s.Format, s.SizeBytes, len(img.Data))
	}
	if s.FocusedWindow != "Kate — notes.txt" {
		t.Errorf("focused window = %q, want the active window", s.FocusedWindow)
	}
	if s.Cursor == nil || s.Cursor.X != 960 || s.Cursor.Y != 540 {
		t.Errorf("cursor = %+v, want {960 540}", s.Cursor)
	}
	// Bonus finding (M1 W3): screenshots never render the pointer
	// on this laptop — the tool must say so, never imply otherwise.
	if s.CursorVisibleInImage {
		t.Errorf("cursor_visible_in_image = true, want false")
	}
	if s.SensitiveCheck != "ok" {
		t.Errorf("sensitive_check = %q, want ok", s.SensitiveCheck)
	}
	// The file exists, 0600, under ~/.orion/screenshots.
	info, err := os.Stat(s.File)
	if err != nil {
		t.Fatalf("screenshot file %s: %v", s.File, err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("screenshot file mode = %o, want 0600", info.Mode().Perm())
	}
	if dir := filepath.Dir(s.File); dir != filepath.Join(env.home, ".orion", "screenshots") {
		t.Errorf("screenshot dir = %q, want ~/.orion/screenshots", dir)
	}
	if env.captures != 1 {
		t.Errorf("spectacle ran %d captures, want 1", env.captures)
	}
}

func TestScreenshotPNGAndScaleAndRegion(t *testing.T) {
	env := newFakeScreenshotEnv(t)
	res := env.call(t, `{"format":"png"}`)
	if res.Images[0].MIMEType != "image/png" {
		t.Errorf("media type = %q, want image/png", res.Images[0].MIMEType)
	}
	if !strings.HasSuffix(parseSummary(t, res).File, ".png") {
		t.Errorf("png file = %q, want a .png name", parseSummary(t, res).File)
	}

	env.advance()
	res = env.call(t, `{"scale":0.5}`)
	if s := parseSummary(t, res); s.Width != 960 || s.Height != 540 {
		t.Errorf("scale 0.5 size = %dx%d, want 960x540", s.Width, s.Height)
	}

	env.advance()
	res = env.call(t, `{"region":{"x":100,"y":100,"width":200,"height":100}}`)
	if s := parseSummary(t, res); s.Width != 200 || s.Height != 100 {
		t.Errorf("region size = %dx%d, want 200x100", s.Width, s.Height)
	}
}

func TestScreenshotInvalidArgs(t *testing.T) {
	env := newFakeScreenshotEnv(t)
	bad := []string{
		`{"scale":0.1}`,
		`{"scale":1.5}`,
		`{"format":"gif"}`,
		`{"max_width":100}`,
		`{"max_width":5000}`,
		`{"display":"all"}`,
		`{"display":"HDMI-1"}`,
		`{"region":{"x":-1,"y":0,"width":10,"height":10}}`,
		`{"region":{"x":0,"y":0,"width":0,"height":10}}`,
	}
	for _, args := range bad {
		_, err := env.tool.ExecuteResult(context.Background(), json.RawMessage(args))
		if err == nil {
			t.Errorf("ExecuteResult(%s) = nil, want invalid_args", args)
			continue
		}
		terr, ok := err.(*ToolError)
		if !ok || terr.Kind != "invalid_args" {
			t.Errorf("ExecuteResult(%s) error = %v, want kind invalid_args", args, err)
		}
	}
	// A region outside the capture is invalid_args too (detected
	// after capture, once the dimensions are known).
	env.advance()
	_, err := env.tool.ExecuteResult(context.Background(), json.RawMessage(`{"region":{"x":1900,"y":1000,"width":200,"height":200}}`))
	if err == nil {
		t.Errorf("ExecuteResult(out-of-bounds region) = nil, want invalid_args")
	} else if terr, ok := err.(*ToolError); !ok || terr.Kind != "invalid_args" {
		t.Errorf("ExecuteResult(out-of-bounds region) error = %v, want kind invalid_args", err)
	}
}

func TestScreenshotRegistryValidation(t *testing.T) {
	// The schema layer (registry validateArgs) rejects out-of-range
	// values before tool code runs.
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	_, err = reg.ExecuteResult(context.Background(), "screenshot", json.RawMessage(`{"scale":5}`))
	if err == nil {
		t.Errorf("registry ExecuteResult(scale 5) = nil, want invalid_args")
	} else if terr, ok := err.(*ToolError); !ok || terr.Kind != "invalid_args" {
		t.Errorf("registry ExecuteResult(scale 5) error = %v, want kind invalid_args", err)
	}
}

func TestScreenshotMissingBackend(t *testing.T) {
	env := newFakeScreenshotEnv(t)
	env.tool.lookPath = func(name string) (string, error) { return "", os.ErrNotExist }
	_, err := env.tool.ExecuteResult(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatalf("ExecuteResult(no spectacle) = nil, want invalid_args")
	}
	terr, ok := err.(*ToolError)
	if !ok || terr.Kind != "invalid_args" {
		t.Fatalf("ExecuteResult(no spectacle) error = %v, want kind invalid_args", err)
	}
	if !strings.Contains(terr.Err.Error(), "not supported yet") {
		t.Errorf("missing-backend error = %q, want it to say the ScreenCast backend is not supported yet", terr.Err.Error())
	}
	if env.captures != 0 {
		t.Errorf("spectacle ran %d captures after a missing backend, want 0", env.captures)
	}
}

func TestScreenshotRefusesSensitiveWindow(t *testing.T) {
	env := newFakeScreenshotEnv(t)
	// Negative test (plan §5.3 / M1 gate): a password manager open
	// refuses the capture before anything is taken.
	env.tool.windowTitles = func(ctx context.Context) ([]string, error) {
		return []string{"Firefox", "KeePassXC"}, nil
	}
	_, err := env.tool.ExecuteResult(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatalf("ExecuteResult(KeePassXC open) = nil, want a denial")
	}
	terr, ok := err.(*ToolError)
	if !ok || terr.Kind != "denied" {
		t.Fatalf("ExecuteResult(KeePassXC open) error = %v, want kind denied", err)
	}
	// The refusal names the category, never the sensitive title or
	// its content — and nothing was captured or stored.
	if strings.Contains(terr.Err.Error(), "KeePassXC") {
		t.Errorf("refusal quotes the sensitive title: %q", terr.Err.Error())
	}
	if env.captures != 0 {
		t.Errorf("spectacle ran %d captures after a sensitive refusal, want 0", env.captures)
	}
	dir := filepath.Join(env.home, ".orion", "screenshots")
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("screenshots dir has %d files after a refusal, want 0", len(entries))
	}
}

func TestScreenshotRefusesUnattendedTurn(t *testing.T) {
	env := newFakeScreenshotEnv(t)
	ctx := WithUnattendedTurn(context.Background())
	_, err := env.tool.ExecuteResult(ctx, json.RawMessage(`{}`))
	if err == nil {
		t.Fatalf("ExecuteResult(unattended) = nil, want a denial")
	}
	terr, ok := err.(*ToolError)
	if !ok || terr.Kind != "denied" {
		t.Fatalf("ExecuteResult(unattended) error = %v, want kind denied", err)
	}
	if env.captures != 0 {
		t.Errorf("spectacle ran %d captures in an unattended turn, want 0", env.captures)
	}
}

func TestScreenshotRefusesLockedScreen(t *testing.T) {
	env := newFakeScreenshotEnv(t)
	env.tool.screenLocked = func(ctx context.Context) (bool, bool) { return true, true }
	_, err := env.tool.ExecuteResult(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatalf("ExecuteResult(locked) = nil, want an error")
	}
	if terr, ok := err.(*ToolError); !ok || terr.Kind != "execution" {
		t.Fatalf("ExecuteResult(locked) error = %v, want kind execution", err)
	}
	if env.captures != 0 {
		t.Errorf("spectacle ran %d captures on a locked screen, want 0", env.captures)
	}
}

func TestScreenshotRateLimits(t *testing.T) {
	env := newFakeScreenshotEnv(t)
	env.call(t, `{}`)
	// Min interval: an immediate second capture is denied.
	if _, err := env.tool.ExecuteResult(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Errorf("second capture within 1s = nil, want a rate-limit denial")
	} else if terr, ok := err.(*ToolError); !ok || terr.Kind != "denied" {
		t.Errorf("second capture within 1s error = %v, want kind denied", err)
	}

	// Per-turn cap: seed 14 calls, one more is allowed, the 16th
	// is not; BeginTurn resets the counter.
	env.advance()
	env.tool.turnCalls = screenshotMaxPerTurn - 1
	env.call(t, `{}`)
	env.advance()
	if _, err := env.tool.ExecuteResult(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Errorf("16th capture in a turn = nil, want a rate-limit denial")
	}
	env.tool.BeginTurn()
	env.advance()
	env.call(t, `{}`)

	// Per-hour cap: 120 calls in the last hour blocks the next.
	env.tool.hourCalls = make([]time.Time, screenshotMaxPerHour)
	for i := range env.tool.hourCalls {
		env.tool.hourCalls[i] = env.clock.Add(-time.Minute)
	}
	env.advance()
	if _, err := env.tool.ExecuteResult(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Errorf("121st capture in an hour = nil, want a rate-limit denial")
	}
}

func TestScreenshotPrunesOldFiles(t *testing.T) {
	env := newFakeScreenshotEnv(t)
	dir := filepath.Join(env.home, ".orion", "screenshots")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	old := filepath.Join(dir, "screenshot-old.jpg")
	if err := os.WriteFile(old, []byte("old"), 0600); err != nil {
		t.Fatalf("write old file: %v", err)
	}
	if err := os.Chtimes(old, time.Now().Add(-25*time.Hour), time.Now().Add(-25*time.Hour)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	env.call(t, `{}`)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("24h-old screenshot was not pruned")
	}
}

func TestScreenshotExecuteReturnsText(t *testing.T) {
	env := newFakeScreenshotEnv(t)
	text, err := env.tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute = %v, want nil", err)
	}
	var s screenshotSummary
	if err := json.Unmarshal([]byte(text), &s); err != nil {
		t.Fatalf("Execute text %q is not the JSON summary: %v", text, err)
	}
	if s.Backend != "spectacle" {
		t.Errorf("Execute text backend = %q, want spectacle", s.Backend)
	}
}

func TestScreenshotSensitiveCheckUnavailable(t *testing.T) {
	// xdotool missing entirely: the capture proceeds, but the
	// summary says the check could not run — the gap is visible,
	// never silent.
	env := newFakeScreenshotEnv(t)
	env.tool.windowTitles = func(ctx context.Context) ([]string, error) { return nil, os.ErrNotExist }
	env.tool.activeWindow = func(ctx context.Context) (string, error) { return "", os.ErrNotExist }
	res := env.call(t, `{}`)
	if s := parseSummary(t, res); s.SensitiveCheck != "unavailable" {
		t.Errorf("sensitive_check = %q, want unavailable", s.SensitiveCheck)
	}
}

func TestScreenshotEmptyWindowListIsOk(t *testing.T) {
	// Live finding (laptop 2026-11-15): xdotool enumerates 3
	// visible X windows, all with empty titles, and no X window
	// is focused — that is a successful enumeration with no
	// sensitive title, so the check is "ok", not "unavailable".
	// (A nil slice from a stub used to read as failure.)
	env := newFakeScreenshotEnv(t)
	env.tool.windowTitles = func(ctx context.Context) ([]string, error) { return []string{}, nil }
	env.tool.activeWindow = func(ctx context.Context) (string, error) { return "", nil }
	res := env.call(t, `{}`)
	if s := parseSummary(t, res); s.SensitiveCheck != "ok" {
		t.Errorf("sensitive_check = %q, want ok", s.SensitiveCheck)
	}
	if s := parseSummary(t, res); s.FocusedWindow != "" {
		t.Errorf("focused_window = %q, want empty (no X window focused)", s.FocusedWindow)
	}
}
