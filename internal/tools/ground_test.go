// Tests for the ground tool (computer-use M2). The real backends
// (spectacle, xdotool, python3/AT-SPI) never run here: every seam
// is stubbed, so the tests run on a headless VM. Live grounding is
// verified on the laptop instead (see
// ~/workspace/orion-verification/computer-use-m2/).
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/irfan/orion/internal/permissions"
)

// fakeGroundEnv is a stubbed ground tool plus its clock and a
// canned AT-SPI tree (the GitHub login page from the M2 design
// probe: username entry, password field, two links, plus the
// degenerate/offscreen/heading traps the filters must catch).
type fakeGroundEnv struct {
	tool     *groundTool
	clock    time.Time
	home     string
	png      []byte // 1920x1080 test capture
	captures int
	helperOK bool   // runHelper succeeds
	helper   string // canned helper JSON ("" = default tree)
	kwinOK   bool   // runKWinHelper succeeds
	kwin     string // canned KWin JSON
}

func groundTreeJSON() string {
	return `{"windows": [
		{"title": "Sign in to GitHub · GitHub — Mozilla Firefox", "app": "Firefox", "role": "frame", "focused": true, "x": 0, "y": 0, "w": 1920, "h": 1080},
		{"title": "Kate — notes.txt", "app": "Kate", "role": "frame", "focused": false, "x": 0, "y": 0, "w": 800, "h": 600}],
	"elements": [
		{"role": "entry", "name": "Username or email address", "x": 784, "y": 383, "w": 352, "h": 40},
		{"role": "password text", "name": "Password", "x": 784, "y": 464, "w": 352, "h": 40},
		{"role": "link", "name": "Forgot password?", "x": 1019, "y": 439, "w": 117, "h": 21},
		{"role": "link", "name": "Create an account", "x": 954, "y": 730, "w": 119, "h": 20},
		{"role": "link", "name": "Skip to content", "x": -1, "y": -1, "w": -1, "h": -1},
		{"role": "push button", "name": "Sign in", "x": 500, "y": 500, "w": 1, "h": 1},
		{"role": "entry", "name": "Offscreen field", "x": -5, "y": 100, "w": 100, "h": 20},
		{"role": "heading", "name": "Sign in to GitHub", "x": 784, "y": 300, "w": 200, "h": 30},
		{"role": "check box", "name": "", "x": 784, "y": 540, "w": 20, "h": 20},
		{"role": "combo box", "name": "Language", "x": 100, "y": 100, "w": 120, "h": 30}],
	"focused_window": {"title": "Sign in to GitHub · GitHub — Mozilla Firefox", "app": "Firefox"},
	"walked_nodes": 140}`
}

func newFakeGroundEnv(t *testing.T) *fakeGroundEnv {
	t.Helper()
	t.Setenv("DISPLAY", ":1")
	home := t.TempDir()
	env := &fakeGroundEnv{
		tool:     newGroundTool(),
		clock:    time.Now(),
		home:     home,
		png:      makeTestPNG(t, 1920, 1080),
		helperOK: true,
	}
	tool := env.tool
	tool.homeDir = func() (string, error) { return home, nil }
	tool.now = func() time.Time { return env.clock }
	tool.lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	tool.execCmd = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "python3" {
			return "", nil // import smoke test
		}
		t.Errorf("unexpected ground exec: %s %v", name, args)
		return "", nil
	}
	tool.runHelper = func(ctx context.Context, helperPath string) ([]byte, error) {
		if !env.helperOK {
			return nil, errors.New("fake helper failure")
		}
		if env.helper != "" {
			return []byte(env.helper), nil
		}
		return []byte(groundTreeJSON()), nil
	}
	tool.runKWinHelper = func(ctx context.Context, helperPath string) ([]byte, error) {
		if !env.kwinOK {
			return nil, errors.New("fake KWin helper failure")
		}
		return []byte(env.kwin), nil
	}
	// The inner screenshot tool gets the same seams as the
	// screenshot tests (same fake clock, same capture).
	shot := tool.shot
	shot.homeDir = func() (string, error) { return home, nil }
	shot.now = func() time.Time { return env.clock }
	shot.lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	shot.execCmd = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "spectacle" && len(args) == 1 && args[0] == "--version" {
			return "spectacle 6.6.5\n", nil
		}
		if name == "spectacle" {
			out := args[len(args)-1]
			env.captures++
			if err := os.WriteFile(out, env.png, 0600); err != nil {
				t.Errorf("fake spectacle write: %v", err)
			}
			return "", nil
		}
		t.Errorf("unexpected shot exec: %s %v", name, args)
		return "", nil
	}
	shot.windowTitles = func(ctx context.Context) ([]string, error) {
		return []string{"Firefox", "Kate — notes.txt"}, nil
	}
	shot.activeWindow = func(ctx context.Context) (string, error) {
		return "Kate — notes.txt", nil
	}
	shot.cursor = func(ctx context.Context) (cursorPos, bool) {
		return cursorPos{X: 960, Y: 540}, true
	}
	shot.screenLocked = func(ctx context.Context) (bool, bool) { return false, true }
	return env
}

var errFakeLookPath = errors.New("not found")

func (e *fakeGroundEnv) advance() { e.clock = e.clock.Add(2 * time.Second) }

func (e *fakeGroundEnv) call(t *testing.T, args string) ToolResult {
	t.Helper()
	res, err := e.tool.ExecuteResult(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("ExecuteResult(%s) = %v, want nil", args, err)
	}
	return res
}

func parseGroundSummary(t *testing.T, res ToolResult) groundSummary {
	t.Helper()
	var s groundSummary
	if err := json.Unmarshal([]byte(res.Text), &s); err != nil {
		t.Fatalf("ground summary JSON %q does not parse: %v", res.Text, err)
	}
	return s
}

func resetGroundSnapshots() { groundSnapshots = newGroundSnapshotRegistry() }

func TestGroundHappyPath(t *testing.T) {
	resetGroundSnapshots()
	env := newFakeGroundEnv(t)
	res := env.call(t, `{}`)
	if len(res.Images) != 1 {
		t.Fatalf("got %d images, want 1", len(res.Images))
	}
	if res.Images[0].MIMEType != "image/jpeg" {
		t.Errorf("media type = %q, want image/jpeg", res.Images[0].MIMEType)
	}
	if len(res.Images[0].Data) == 0 || len(res.Images[0].Data) > MaxImageBytes {
		t.Errorf("image size = %d, want (0, %d]", len(res.Images[0].Data), MaxImageBytes)
	}
	s := parseGroundSummary(t, res)
	if s.Coverage != "grounded" {
		t.Errorf("coverage = %q, want grounded", s.Coverage)
	}
	if s.WalkedNodes != 140 {
		t.Errorf("walked_nodes = %d, want 140", s.WalkedNodes)
	}
	// Full-resolution capture: image pixels are screen pixels.
	if s.Screen.Width != 1920 || s.Screen.Height != 1080 || s.Screen.Scale != 1 {
		t.Errorf("screen = %+v, want 1920x1080 scale 1", s.Screen)
	}
	if s.FocusedWindow == nil || s.FocusedWindow.Title != "Sign in to GitHub · GitHub — Mozilla Firefox" || s.FocusedWindow.App != "Firefox" {
		t.Errorf("focused_window = %+v, want the AT-SPI focused window", s.FocusedWindow)
	}
	// snapshot_id shape: g-YYYYMMDD-HHMMSS-<16 hex> (64-bit
	// random suffix, F-A23W2-19).
	if ok, _ := regexp.MatchString(`^g-\d{8}-\d{6}-[0-9a-f]{16}$`, s.SnapshotID); !ok {
		t.Errorf("snapshot_id = %q, want g-YYYYMMDD-HHMMSS-<16 hex>", s.SnapshotID)
	}
	if _, err := time.Parse(time.RFC3339, s.TakenAt); err != nil {
		t.Errorf("taken_at = %q does not parse as RFC3339: %v", s.TakenAt, err)
	}
	// The design probe's elements, in tree order, minus the traps:
	// entry, password, 2 links, check box (empty name -> role),
	// combo box = 6. Degenerate (-1,-1 / 1x1), offscreen and the
	// heading are filtered out.
	if len(s.Elements) != 6 {
		t.Fatalf("got %d elements, want 6: %+v", len(s.Elements), s.Elements)
	}
	// Element 1: username entry — the design §4.2 example numbers.
	username := s.Elements[0]
	if username.ID != 1 || username.Role != "entry" || username.Name != "Username or email address" {
		t.Errorf("element 1 = %+v, want entry \"Username or email address\"", username)
	}
	if username.Box != (groundBox{X: 784, Y: 383, W: 352, H: 40}) {
		t.Errorf("element 1 box = %+v, want {784 383 352 40}", username.Box)
	}
	if username.Centre != (groundPoint{X: 960, Y: 403}) {
		t.Errorf("element 1 centre = %+v, want {960 403}", username.Centre)
	}
	if username.CentreNorm != (groundPoint{X: 500, Y: 373}) {
		t.Errorf("element 1 centre_norm = %+v, want {500 373}", username.CentreNorm)
	}
	// Element 2: password — role + box only, no value anywhere.
	pw := s.Elements[1]
	if pw.Role != "password text" || pw.Name != "Password" {
		t.Errorf("element 2 = %+v, want password text \"Password\"", pw)
	}
	if pw.Box != (groundBox{X: 784, Y: 464, W: 352, H: 40}) {
		t.Errorf("element 2 box = %+v, want {784 464 352 40}", pw.Box)
	}
	if strings.Contains(res.Text, `"value"`) {
		t.Errorf("summary contains a \"value\" field — a password value must never appear: %s", res.Text)
	}
	// Empty name falls back to the role, never a guessed name.
	checkbox := s.Elements[4]
	if checkbox.Role != "check box" || checkbox.Name != "check box" {
		t.Errorf("check box element = %+v, want name = role", checkbox)
	}
	// The snapshot is registered and validates for its elements.
	if err := ValidateGroundSnapshot(context.Background(), s.SnapshotID, 1); err != nil {
		t.Errorf("fresh snapshot should validate: %v", err)
	}
	if err := ValidateGroundSnapshot(context.Background(), s.SnapshotID, 99); err == nil {
		t.Errorf("unknown element id should be refused")
	}
}

func TestGroundElementFiltering(t *testing.T) {
	resetGroundSnapshots()
	env := newFakeGroundEnv(t)
	res := env.call(t, `{}`)
	s := parseGroundSummary(t, res)
	for _, el := range s.Elements {
		if el.Role == "heading" {
			t.Errorf("heading element leaked into the list: %+v", el)
		}
		if el.Box.W <= 1 || el.Box.H <= 1 || el.Box.X < 0 || el.Box.Y < 0 {
			t.Errorf("degenerate/offscreen element leaked: %+v", el)
		}
		if el.Centre.X < el.Box.X || el.Centre.X >= el.Box.X+el.Box.W ||
			el.Centre.Y < el.Box.Y || el.Centre.Y >= el.Box.Y+el.Box.H {
			t.Errorf("centre %+v lies outside box %+v", el.Centre, el.Box)
		}
		if el.CentreNorm.X < 0 || el.CentreNorm.X > 1000 || el.CentreNorm.Y < 0 || el.CentreNorm.Y > 1000 {
			t.Errorf("centre_norm %+v outside 0-1000", el.CentreNorm)
		}
	}
}

func TestGroundRegionZoom(t *testing.T) {
	resetGroundSnapshots()
	env := newFakeGroundEnv(t)
	res := env.call(t, `{"region": {"x": 700, "y": 300, "width": 600, "height": 300}}`)
	s := parseGroundSummary(t, res)
	// Inside the region (centre test): username (960,403), password
	// (960,484), "Forgot password?" (1077,449) and the check box
	// (794,550). "Create an account" (1013,740) and the combo box
	// (160,115) are outside.
	if len(s.Elements) != 4 {
		t.Fatalf("got %d elements in region, want 4: %+v", len(s.Elements), s.Elements)
	}
	if s.Region == nil || *s.Region != (region{X: 700, Y: 300, Width: 600, Height: 300}) {
		t.Errorf("region = %+v, want the requested region echoed", s.Region)
	}
	// centre_norm is relative to the cropped view: username
	// (960-700)*1000/600 = 433, (403-300)*1000/300 = 343.
	u := s.Elements[0]
	if u.CentreNorm != (groundPoint{X: 433, Y: 343}) {
		t.Errorf("region centre_norm = %+v, want {433 343}", u.CentreNorm)
	}
	// Box stays absolute screen pixels even in a region.
	if u.Box != (groundBox{X: 784, Y: 383, W: 352, H: 40}) {
		t.Errorf("region box = %+v, want absolute screen pixels", u.Box)
	}
	// The image is the cropped view.
	if s.Screen.Width != 600 || s.Screen.Height != 300 {
		t.Errorf("region screen = %+v, want 600x300", s.Screen)
	}
}

func TestGroundRolesAndMaxElements(t *testing.T) {
	resetGroundSnapshots()
	env := newFakeGroundEnv(t)
	res := env.call(t, `{"roles": ["link"]}`)
	s := parseGroundSummary(t, res)
	if len(s.Elements) != 2 {
		t.Fatalf("roles=[link] gave %d elements, want 2: %+v", len(s.Elements), s.Elements)
	}
	for _, el := range s.Elements {
		if el.Role != "link" {
			t.Errorf("roles=[link] leaked role %q", el.Role)
		}
	}
	env.advance()
	res = env.call(t, `{"max_elements": 2}`)
	s = parseGroundSummary(t, res)
	if len(s.Elements) != 2 {
		t.Errorf("max_elements=2 gave %d elements, want 2", len(s.Elements))
	}
	// Argument validation.
	for _, args := range []string{
		`{"max_elements": 0}`, `{"max_elements": 201}`,
		`{"roles": []}`, `{"roles": [""]}`,
		`{"region": {"x": -1, "y": 0, "width": 10, "height": 10}}`,
	} {
		env.advance()
		if _, err := env.tool.ExecuteResult(context.Background(), json.RawMessage(args)); err == nil {
			t.Errorf("ExecuteResult(%s) = nil, want invalid_args", args)
		}
	}
}

// TestGroundButtonRoleIncluded pins the F1 live bug (M2 live
// verification 19:58–20:03 SGT): Firefox reports HTML buttons as
// role "button", not "push button" — the 939-node tree had zero
// "push button" nodes, so the page's primary action ("Sign in",
// depth 7, box 784,520 352x40) and five more interactive buttons
// were missing from the element list. The default roles must
// include "button" (and the other button variants), and all six
// must appear.
func TestGroundButtonRoleIncluded(t *testing.T) {
	resetGroundSnapshots()
	env := newFakeGroundEnv(t)
	env.helper = `{"windows": [
		{"title": "Sign in to GitHub · GitHub — Mozilla Firefox", "app": "Firefox", "role": "frame", "focused": true, "x": 0, "y": 0, "w": 1920, "h": 993}],
	"elements": [
		{"role": "entry", "name": "Username or email address", "x": 784, "y": 383, "w": 352, "h": 40},
		{"role": "button", "name": "Sign in", "x": 784, "y": 520, "w": 352, "h": 40},
		{"role": "button", "name": "Continue with Google", "x": 784, "y": 621, "w": 352, "h": 40},
		{"role": "button", "name": "Continue with Apple", "x": 784, "y": 669, "w": 352, "h": 40},
		{"role": "button", "name": "Sign in with a passkey", "x": 887, "y": 766, "w": 146, "h": 21},
		{"role": "button", "name": "Manage cookies", "x": 965, "y": 959, "w": 91, "h": 18},
		{"role": "button", "name": "Do not share my personal information", "x": 1104, "y": 959, "w": 216, "h": 18},
		{"role": "toggle button", "name": "Firefox View", "x": 0, "y": 0, "w": 40, "h": 44},
		{"role": "push button menu", "name": "Session", "x": 631, "y": 508, "w": 30, "h": 30},
		{"role": "radio button", "name": "Option A", "x": 100, "y": 200, "w": 20, "h": 20},
		{"role": "heading", "name": "Sign in to GitHub", "x": 784, "y": 300, "w": 200, "h": 30},
		{"role": "label", "name": "Some text", "x": 100, "y": 100, "w": 100, "h": 20},
		{"role": "list item", "name": "Item", "x": 100, "y": 300, "w": 100, "h": 20}],
	"focused_window": {"title": "Sign in to GitHub · GitHub — Mozilla Firefox", "app": "Firefox"},
	"walked_nodes": 939}`
	res := env.call(t, `{}`)
	s := parseGroundSummary(t, res)
	// entry + 6 buttons + toggle + push-button-menu + radio = 10.
	// heading, label and list item are not interactive roles.
	if len(s.Elements) != 10 {
		t.Fatalf("got %d elements, want 10: %+v", len(s.Elements), s.Elements)
	}
	byName := make(map[string]groundElement, len(s.Elements))
	for _, el := range s.Elements {
		byName[el.Name] = el
	}
	for _, name := range []string{"Sign in", "Continue with Google", "Continue with Apple", "Sign in with a passkey", "Manage cookies", "Do not share my personal information"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("button %q missing from the element list", name)
		}
	}
	signin := byName["Sign in"]
	if signin.Role != "button" {
		t.Errorf("Sign in role = %q, want button", signin.Role)
	}
	if signin.Box != (groundBox{X: 784, Y: 520, W: 352, H: 40}) {
		t.Errorf("Sign in box = %+v, want {784 520 352 40}", signin.Box)
	}
	if signin.Centre != (groundPoint{X: 960, Y: 540}) {
		t.Errorf("Sign in centre = %+v, want {960 540}", signin.Centre)
	}
	if signin.CentreNorm != (groundPoint{X: 500, Y: 500}) {
		t.Errorf("Sign in centre_norm = %+v, want {500 500}", signin.CentreNorm)
	}
	if _, ok := byName["Firefox View"]; !ok {
		t.Errorf("toggle button %q missing", "Firefox View")
	}
	if _, ok := byName["Session"]; !ok {
		t.Errorf("push button menu %q missing", "Session")
	}
	if _, ok := byName["Option A"]; !ok {
		t.Errorf("radio button %q missing", "Option A")
	}
}

// TestGroundHiddenElementsExcluded pins the F7 live bug (M2 live
// verification 2026-11-15 20:26 SGT): a default ground run listed
// 60/60 hidden plasmashell elements (calendar days, Sleep /
// Hibernate / Restart / Shut Down session buttons) and none of
// Firefox's — the closed popups' nodes stay in the AT-SPI tree.
// The helper reports the SHOWING state per element; the tool must
// drop showing=false elements, keep showing=true ones, and keep
// elements with no showing field (back-compat: a failed state
// query must never drop an element).
func TestGroundHiddenElementsExcluded(t *testing.T) {
	resetGroundSnapshots()
	env := newFakeGroundEnv(t)
	env.helper = `{"windows": [
		{"title": "Sign in to GitHub · GitHub — Mozilla Firefox", "app": "Firefox", "role": "frame", "focused": true, "x": 0, "y": 0, "w": 1920, "h": 993}],
	"elements": [
		{"role": "button", "name": "Sign in", "x": 784, "y": 520, "w": 352, "h": 40, "showing": true},
		{"role": "entry", "name": "Username or email address", "x": 784, "y": 383, "w": 352, "h": 40},
		{"role": "button", "name": "Sleep", "x": 269, "y": 508, "w": 68, "h": 30, "showing": false},
		{"role": "button", "name": "Sunday, 25 October 2026", "x": 803, "y": 133, "w": 55, "h": 40, "showing": false},
		{"role": "button", "name": "Hibernate", "x": 341, "y": 508, "w": 68, "h": 30, "showing": false},
		{"role": "heading", "name": "Sunday, 15 November 2026", "x": 12, "y": 5, "w": 50, "h": 25, "showing": false}],
	"focused_window": {"title": "Sign in to GitHub · GitHub — Mozilla Firefox", "app": "Firefox"},
	"walked_nodes": 942}`
	res := env.call(t, `{}`)
	s := parseGroundSummary(t, res)
	// Only the visible Sign in button and the entry with no
	// showing field (kept by back-compat) survive: 2 elements.
	if len(s.Elements) != 2 {
		t.Fatalf("got %d elements, want 2: %+v", len(s.Elements), s.Elements)
	}
	byName := make(map[string]groundElement, len(s.Elements))
	for _, el := range s.Elements {
		byName[el.Name] = el
	}
	if _, ok := byName["Sign in"]; !ok {
		t.Errorf("visible button %q missing from the element list", "Sign in")
	}
	if _, ok := byName["Username or email address"]; !ok {
		t.Errorf("element with no showing field %q missing (must be kept)", "Username or email address")
	}
	for _, name := range []string{"Sleep", "Sunday, 25 October 2026", "Hibernate"} {
		if _, ok := byName[name]; ok {
			t.Errorf("hidden element %q listed, want excluded", name)
		}
	}
}

func TestGroundVisionOnly(t *testing.T) {
	resetGroundSnapshots()
	// Helper run fails: screenshot still returned, empty list.
	// Fail-closed (F-M2R-5) still applies with no tree: the
	// grounding proceeds only because KWin verified the screen
	// (empty window list, nothing sensitive).
	env := newFakeGroundEnv(t)
	env.helperOK = false
	env.kwinOK = true
	env.kwin = `{"kwin_windows": []}`
	res := env.call(t, `{}`)
	s := parseGroundSummary(t, res)
	if s.Coverage != "vision_only" {
		t.Errorf("coverage = %q, want vision_only", s.Coverage)
	}
	if len(s.Elements) != 0 {
		t.Errorf("vision_only gave %d elements, want 0 (never faked)", len(s.Elements))
	}
	if len(res.Images) != 1 {
		t.Errorf("vision_only gave %d images, want 1", len(res.Images))
	}
	// Helper JSON unparseable: same clean degrade.
	env2 := newFakeGroundEnv(t)
	env2.helper = `{"windows": [ broken`
	env2.kwinOK = true
	env2.kwin = `{"kwin_windows": []}`
	res = env2.call(t, `{}`)
	if s := parseGroundSummary(t, res); s.Coverage != "vision_only" || len(s.Elements) != 0 {
		t.Errorf("bad helper JSON gave coverage %q with %d elements, want vision_only/0", s.Coverage, len(s.Elements))
	}
	// python3 missing entirely: same degrade, no error.
	env3 := newFakeGroundEnv(t)
	env3.tool.lookPath = func(name string) (string, error) { return "", errFakeLookPath }
	env3.kwinOK = true
	env3.kwin = `{"kwin_windows": []}`
	res = env3.call(t, `{}`)
	if s := parseGroundSummary(t, res); s.Coverage != "vision_only" {
		t.Errorf("no python3 gave coverage %q, want vision_only", s.Coverage)
	}
}

func TestGroundSensitiveRefusal(t *testing.T) {
	resetGroundSnapshots()
	env := newFakeGroundEnv(t)
	env.helper = `{"windows": [
		{"title": "Bitwarden — Vault", "app": "Bitwarden", "role": "frame", "focused": true, "x": 0, "y": 0, "w": 800, "h": 600}],
	"elements": [], "focused_window": {"title": "Bitwarden — Vault", "app": "Bitwarden"}, "walked_nodes": 3}`
	_, err := env.tool.ExecuteResult(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("sensitive window should refuse grounding")
	}
	var terr *ToolError
	if !errors.As(err, &terr) || terr.Kind != "denied" {
		t.Errorf("error = %v, want denied ToolError", err)
	}
	if !strings.Contains(err.Error(), "password manager") {
		t.Errorf("refusal = %q, want the category (never the title)", err.Error())
	}
	if strings.Contains(err.Error(), "Bitwarden") {
		t.Errorf("refusal names the window title %q — titles can carry secrets", err.Error())
	}
	if env.captures != 0 {
		t.Errorf("sensitive refusal still captured %d screenshot(s)", env.captures)
	}
}

func TestGroundUnattendedRefused(t *testing.T) {
	resetGroundSnapshots()
	env := newFakeGroundEnv(t)
	ctx := WithUnattendedTurn(context.Background())
	_, err := env.tool.ExecuteResult(ctx, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("unattended turn should refuse grounding")
	}
	var terr *ToolError
	if !errors.As(err, &terr) || terr.Kind != "denied" {
		t.Errorf("error = %v, want denied ToolError", err)
	}
	if env.captures != 0 {
		t.Errorf("unattended refusal still captured %d screenshot(s)", env.captures)
	}
}

// TestGroundUnattendedRefusedViaGateMarker (F-A23W3-18): grounding
// must refuse a turn marked unattended ONLY by the permission
// gate's marker, like TestGroundUnattendedRefused does for the
// tools marker. It FAILS on the old code.
func TestGroundUnattendedRefusedViaGateMarker(t *testing.T) {
	resetGroundSnapshots()
	env := newFakeGroundEnv(t)
	ctx := permissions.WithUnattendedContext(context.Background())
	_, err := env.tool.ExecuteResult(ctx, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("gate-marked unattended turn should refuse grounding")
	}
	var terr *ToolError
	if !errors.As(err, &terr) || terr.Kind != "denied" {
		t.Errorf("error = %v, want denied ToolError", err)
	}
	if env.captures != 0 {
		t.Errorf("gate-marked unattended refusal still captured %d screenshot(s)", env.captures)
	}
}

func TestGroundRateLimit(t *testing.T) {
	resetGroundSnapshots()
	env := newFakeGroundEnv(t)
	env.call(t, `{}`)
	// Same fake second: the min-interval limit fires.
	_, err := env.tool.ExecuteResult(context.Background(), json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "at most one grounding per second") {
		t.Errorf("second immediate call err = %v, want the per-second limit", err)
	}
	// Per-turn cap, exercised directly on the limiter (15 captures
	// would be slow; the limiter is the enforced code either way).
	tool := newGroundTool()
	tool.now = func() time.Time { return env.clock }
	for i := 0; i < groundMaxPerTurn; i++ {
		if err := tool.checkRateLimit(); err != nil {
			t.Fatalf("checkRateLimit #%d = %v, want nil", i+1, err)
		}
		env.clock = env.clock.Add(2 * time.Second)
	}
	if err := tool.checkRateLimit(); err == nil || !strings.Contains(err.Error(), "at most 15 groundings per turn") {
		t.Errorf("16th grounding err = %v, want the per-turn limit", err)
	}
	tool.BeginTurn()
	env.clock = env.clock.Add(2 * time.Second)
	if err := tool.checkRateLimit(); err != nil {
		t.Errorf("checkRateLimit after BeginTurn = %v, want nil", err)
	}
}

func TestGroundSnapshotRegistry(t *testing.T) {
	resetGroundSnapshots()
	r := groundSnapshots
	now := time.Now()
	const chatA, chatB int64 = 111, 222
	id1 := "g-test-1"
	r.register(id1, chatA, now, []groundElement{{ID: 1, Centre: groundPoint{X: 10, Y: 10}}, {ID: 2, Centre: groundPoint{X: 20, Y: 20}}, {ID: 3, Centre: groundPoint{X: 30, Y: 30}}})
	if err := r.validate(id1, chatA, 2, now); err != nil {
		t.Errorf("fresh snapshot validate = %v, want nil", err)
	}
	if err := r.validate(id1, chatA, 99, now); err == nil || !strings.Contains(err.Error(), "not in snapshot") {
		t.Errorf("unknown element err = %v, want 'not in snapshot'", err)
	}
	if err := r.validate("g-nope", chatA, 1, now); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("unknown snapshot err = %v, want 'not available'", err)
	}
	// F-A23W2-19: the same snapshot id from another chat refuses
	// — even though the id is known and fresh, the binding, not
	// the id, decides. The refusal must not name the other chat.
	crossErr := r.validate(id1, chatB, 1, now)
	if crossErr == nil || !strings.Contains(crossErr.Error(), "different chat") {
		t.Errorf("cross-chat snapshot err = %v, want 'different chat'", crossErr)
	} else if strings.Contains(crossErr.Error(), "222") || strings.Contains(crossErr.Error(), "111") {
		t.Errorf("cross-chat refusal names a chat id — it must not: %v", crossErr)
	}
	// The ctx-aware entry points read the chat from the approval
	// context: same chat validates, another chat refuses, and a
	// context with no chat (chat 0) is its own binding.
	ctxA := permissions.WithApprovalContext(context.Background(), chatA, chatA)
	ctxB := permissions.WithApprovalContext(context.Background(), chatB, chatB)
	if err := ValidateGroundSnapshot(ctxA, id1, 1); err != nil {
		t.Errorf("ValidateGroundSnapshot(same chat) = %v, want nil", err)
	}
	if err := ValidateGroundSnapshot(ctxB, id1, 1); err == nil || !strings.Contains(err.Error(), "different chat") {
		t.Errorf("ValidateGroundSnapshot(other chat) err = %v, want 'different chat'", err)
	}
	if _, err := GroundSnapshotElementCentre(ctxA, id1, 2); err != nil {
		t.Errorf("GroundSnapshotElementCentre(same chat) = %v, want nil", err)
	}
	if _, err := GroundSnapshotElementCentre(ctxB, id1, 2); err == nil {
		t.Errorf("GroundSnapshotElementCentre(other chat) = nil, want a refusal")
	}
	// Older than the 60 s TTL: refused even though it is newest.
	if err := r.validate(id1, chatA, 1, now.Add(61*time.Second)); err == nil || !strings.Contains(err.Error(), "old") {
		t.Errorf("61s-old snapshot err = %v, want stale-age refusal", err)
	}
	// A newer grounding supersedes the old one.
	id2 := "g-test-2"
	r.register(id2, chatA, now.Add(time.Second), []groundElement{{ID: 1, Centre: groundPoint{X: 10, Y: 10}}})
	if err := r.validate(id1, chatA, 1, now.Add(2*time.Second)); err == nil || !strings.Contains(err.Error(), "superseded") {
		t.Errorf("superseded snapshot err = %v, want 'superseded'", err)
	}
	if err := r.validate(id2, chatA, 1, now.Add(2*time.Second)); err != nil {
		t.Errorf("newest snapshot validate = %v, want nil", err)
	}
	// Retention: only the last 3 snapshots are kept — the 4th
	// grounding evicts the oldest, and its elements stop working.
	resetGroundSnapshots()
	r = groundSnapshots
	base := time.Now()
	ids := []string{"g-a", "g-b", "g-c", "g-d"}
	for i, id := range ids {
		r.register(id, chatA, base.Add(time.Duration(i)*time.Second), []groundElement{{ID: 1, Centre: groundPoint{X: 10, Y: 10}}})
	}
	if err := groundSnapshots.validate("g-a", chatA, 1, base.Add(4*time.Second)); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("evicted snapshot err = %v, want 'not available'", err)
	}
	if err := groundSnapshots.validate("g-d", chatA, 1, base.Add(4*time.Second)); err != nil {
		t.Errorf("newest of 4 validate = %v, want nil", err)
	}
}

func TestGroundSnapshotIDsUnique(t *testing.T) {
	resetGroundSnapshots()
	env := newFakeGroundEnv(t)
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		if i > 0 {
			env.advance()
		}
		s := parseGroundSummary(t, env.call(t, `{}`))
		if seen[s.SnapshotID] {
			t.Errorf("duplicate snapshot_id %q", s.SnapshotID)
		}
		seen[s.SnapshotID] = true
	}
	// Two groundings in the same second still differ (random suffix).
	if a, b := newSnapshotID(env.clock), newSnapshotID(env.clock); a == b {
		t.Errorf("same-second snapshot ids collide: %q", a)
	}
	// F-A23W2-19: the random suffix is 8 bytes (64-bit entropy),
	// not 2 — 65,536 possibilities per second could collide
	// across two chats grounding in the same second.
	id := newSnapshotID(env.clock)
	parts := strings.Split(id, "-")
	suffix := parts[len(parts)-1]
	if len(suffix) != 16 {
		t.Errorf("snapshot id %q has a %d-char random suffix, want 16 hex chars (64-bit)", id, len(suffix))
	}
	for _, c := range suffix {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			t.Errorf("snapshot id %q suffix %q is not lowercase hex", id, suffix)
			break
		}
	}
}

func TestGroundMarkerInsideBox(t *testing.T) {
	resetGroundSnapshots()
	env := newFakeGroundEnv(t)
	res := env.call(t, `{}`)
	s := parseGroundSummary(t, res)
	img, err := jpeg.Decode(bytes.NewReader(res.Images[0].Data))
	if err != nil {
		t.Fatalf("decode marked image: %v", err)
	}
	imgW, imgH := img.Bounds().Dx(), img.Bounds().Dy()
	if imgW != 1920 || imgH != 1080 {
		t.Fatalf("marked image = %dx%d, want 1920x1080", imgW, imgH)
	}
	for _, el := range s.Elements {
		r := markerRect(el, 0, 0, 1.0, 1.0, imgW, imgH)
		box := image.Rect(el.Box.X, el.Box.Y, el.Box.X+el.Box.W, el.Box.Y+el.Box.H)
		if !r.In(box) {
			t.Errorf("marker %v for element %d is outside its box %v", r, el.ID, box)
		}
		// The badge corner (just inside the border) is white in
		// the annotated image — the marker is really drawn there.
		px := img.At(r.Min.X+1, r.Min.Y+1)
		rr, gg, bb, _ := px.RGBA()
		if rr < 0xf000 || gg < 0xf000 || bb < 0xf000 {
			t.Errorf("marker %d badge corner pixel = (%d,%d,%d), want white", el.ID, rr>>8, gg>>8, bb>>8)
		}
		// And the badge centre sits on the element centre.
		cx, cy := (r.Min.X+r.Max.X)/2, (r.Min.Y+r.Max.Y)/2
		if cx != el.Centre.X && abs(cx-el.Centre.X) > 1 {
			t.Errorf("marker %d centre x = %d, want element centre %d", el.ID, cx, el.Centre.X)
		}
		_ = cy
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func TestGroundHelperFile(t *testing.T) {
	env := newFakeGroundEnv(t)
	path, err := env.tool.writeHelper()
	if err != nil {
		t.Fatalf("writeHelper = %v", err)
	}
	if want := filepath.Join(env.home, ".orion", groundHelperFile); path != want {
		t.Errorf("helper path = %q, want %q", path, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat helper: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("helper mode = %o, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read helper: %v", err)
	}
	if string(data) != atspiHelperSource {
		t.Errorf("helper file differs from the embedded source")
	}
	before := info.ModTime()
	path2, err := env.tool.writeHelper()
	if err != nil || path2 != path {
		t.Fatalf("second writeHelper = %q, %v, want same path", path2, err)
	}
	if after, _ := os.Stat(path); after.ModTime() != before {
		t.Errorf("identical helper was rewritten (mtime changed)")
	}
	// The helper never reads text values: no get_text_iface call
	// anywhere (the docstring names it only to forbid it), and it
	// only asks for name/role/extents.
	if strings.Contains(atspiHelperSource, "get_text_iface(") {
		t.Errorf("helper source calls get_text_iface — values must never be read")
	}
	if !strings.Contains(atspiHelperSource, "get_extents") || !strings.Contains(atspiHelperSource, "get_role_name") {
		t.Errorf("helper source is missing the name/role/extents reads")
	}
}

func TestGroundFrameworkIntegration(t *testing.T) {
	resetGroundSnapshots()
	r, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry failed: %v", err)
	}
	tool, ok := r.Get("ground")
	if !ok {
		t.Fatal("ground is not in the default registry")
	}
	if tool.Name() != "ground" || tool.Definition().Name != "ground" {
		t.Errorf("tool definition = %+v, want name ground", tool.Definition())
	}
	rc, ok := tool.(RiskClasser)
	if !ok {
		t.Fatal("ground does not implement RiskClasser")
	}
	// F-M2R-6: ground is Network, not Safe — the marked image is
	// sent to the model provider, so the call is gated and
	// disclosed, never silently auto-approved.
	if rc.RiskClass() != permissions.ClassNetwork {
		t.Errorf("ground risk class = %v, want Network", rc.RiskClass())
	}
	// The gate's maps must agree with the factory set (the same
	// pin TestClassifyMapsMatchFactorySet enforces): a Network
	// declaration with no gate entry would fail registration.
	if got, want := permissions.Classify("ground"), permissions.ClassNetwork; got != want {
		t.Errorf("gate classifies ground as %v, want Network", got)
	}
	if !permissions.IsGated("ground") {
		t.Errorf("ground is not gated, want gated (F-M2R-6)")
	}
	if !strings.Contains(tool.Description(), "DISCLOSURE") || !strings.Contains(tool.Description(), "model provider") {
		t.Errorf("ground description %q does not disclose that the image is sent to the model provider", tool.Description())
	}
}

func TestGroundExecuteTextMatches(t *testing.T) {
	resetGroundSnapshots()
	env := newFakeGroundEnv(t)
	res, err := env.tool.ExecuteResult(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("ExecuteResult = %v", err)
	}
	env.advance()
	text, err := env.tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute = %v", err)
	}
	// Same shape (snapshot ids differ — each grounding is unique).
	var a, b groundSummary
	if err := json.Unmarshal([]byte(res.Text), &a); err != nil {
		t.Fatalf("ExecuteResult text: %v", err)
	}
	if err := json.Unmarshal([]byte(text), &b); err != nil {
		t.Fatalf("Execute text: %v", err)
	}
	if len(a.Elements) != len(b.Elements) || a.Coverage != b.Coverage {
		t.Errorf("Execute vs ExecuteResult differ: %+v vs %+v", a, b)
	}
}

// --- KWin geometry correction (F2) ---

// groundTreeWithWinJSON is the GitHub login page with "win" tags:
// window 0 is Firefox (0,0,1920,993), window 1 is Kate
// (0,0,800,600). The Sign in button's AT-SPI centre is (960,540);
// the visible centre is (960,569) — the +29 KWin correction.
func groundTreeWithWinJSON() string {
	return `{"windows": [
		{"title": "Sign in to GitHub · GitHub — Mozilla Firefox", "app": "Firefox", "role": "frame", "focused": true, "x": 0, "y": 0, "w": 1920, "h": 993},
		{"title": "Kate — notes.txt", "app": "Kate", "role": "frame", "focused": false, "x": 0, "y": 0, "w": 800, "h": 600}],
	"elements": [
		{"role": "button", "name": "Sign in", "x": 784, "y": 520, "w": 352, "h": 40, "win": 0, "showing": true},
		{"role": "entry", "name": "Username or email address", "x": 784, "y": 383, "w": 352, "h": 40, "win": 0, "showing": true},
		{"role": "entry", "name": "Notes field", "x": 100, "y": 100, "w": 200, "h": 30, "win": 1, "showing": true},
		{"role": "entry", "name": "Untagged field", "x": 500, "y": 500, "w": 100, "h": 20, "showing": true}],
	"focused_window": {"title": "Sign in to GitHub · GitHub — Mozilla Firefox", "app": "Firefox"},
	"walked_nodes": 942}`
}

// kwinFirefoxJSON is KWin's true geometry for the two windows
// above, as measured live (2026-11-15): Firefox's frame starts at
// y=29, Kate's at y=0.
func kwinFirefoxJSON() string {
	return `{"kwin_windows": [
		{"cls": "firefox-esr", "active": true, "minimized": false, "fg": [0, 29, 1920, 993], "cg": [0, 29, 1920, 993], "sens": ""},
		{"cls": "kate", "active": false, "minimized": false, "fg": [0, 0, 800, 600], "cg": [0, 0, 800, 600], "sens": ""}]}`
}

func findGroundElement(t *testing.T, s groundSummary, name string) groundElement {
	t.Helper()
	for _, e := range s.Elements {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("element %q not in summary: %+v", name, s.Elements)
	return groundElement{}
}

func TestGroundKWinCorrectionFirefox(t *testing.T) {
	resetGroundSnapshots()
	env := newFakeGroundEnv(t)
	env.helper = groundTreeWithWinJSON()
	env.kwinOK = true
	env.kwin = kwinFirefoxJSON()
	s := parseGroundSummary(t, env.call(t, `{}`))
	// The Firefox window shifts +29 in Y; Kate stays put.
	if got := findGroundElement(t, s, "Sign in").Centre; got.X != 960 || got.Y != 569 {
		t.Errorf("Sign in centre = %+v, want (960,569)", got)
	}
	if got := findGroundElement(t, s, "Username or email address").Centre; got.X != 960 || got.Y != 432 {
		t.Errorf("Username centre = %+v, want (960,432)", got)
	}
	if got := findGroundElement(t, s, "Notes field").Centre; got.X != 200 || got.Y != 115 {
		t.Errorf("Notes field centre = %+v, want (200,115) (Kate, unshifted)", got)
	}
	if got := findGroundElement(t, s, "Untagged field").Centre; got.X != 550 || got.Y != 510 {
		t.Errorf("Untagged field centre = %+v, want (550,510) (no win tag, unshifted)", got)
	}
	if s.Correction == nil {
		t.Fatal("summary has no correction, want kwin +29")
	}
	if s.Correction.Source != "kwin" {
		t.Errorf("correction source = %q, want kwin", s.Correction.Source)
	}
	if len(s.Correction.Windows) != 1 {
		t.Fatalf("correction windows = %+v, want one Firefox entry", s.Correction.Windows)
	}
	w := s.Correction.Windows[0]
	if w.App != "Firefox" || w.DX != 0 || w.DY != 29 {
		t.Errorf("correction window = %+v, want Firefox (0,29)", w)
	}
}

func TestGroundKWinNoMatch(t *testing.T) {
	resetGroundSnapshots()
	env := newFakeGroundEnv(t)
	env.helper = groundTreeWithWinJSON()
	env.kwinOK = true
	// Same app class and origin, different size (neither FG nor
	// CG matches the AT-SPI window): no correction. Titles are
	// no longer part of the match (F-M2R-15 — the helper never
	// reports them), so size is what distinguishes windows here.
	env.kwin = `{"kwin_windows": [{"cls": "firefox-esr", "active": true, "minimized": false, "fg": [0, 29, 1920, 1050], "cg": [0, 29, 1920, 1050]}]}`
	s := parseGroundSummary(t, env.call(t, `{}`))
	if got := findGroundElement(t, s, "Sign in").Centre; got.X != 960 || got.Y != 540 {
		t.Errorf("Sign in centre = %+v, want uncorrected (960,540)", got)
	}
	if s.Correction != nil {
		t.Errorf("summary correction = %+v, want nil (no match)", s.Correction)
	}
}

func TestGroundKWinAmbiguous(t *testing.T) {
	resetGroundSnapshots()
	env := newFakeGroundEnv(t)
	env.helper = groundTreeWithWinJSON()
	env.kwinOK = true
	// Two KWin windows match the Firefox class+size: fail
	// safe — no correction rather than a guessed one.
	env.kwin = `{"kwin_windows": [
		{"cls": "firefox-esr", "active": true, "minimized": false, "fg": [0, 29, 1920, 993], "cg": [0, 29, 1920, 993]},
		{"cls": "firefox-esr", "active": false, "minimized": false, "fg": [0, 29, 1920, 993], "cg": [0, 29, 1920, 993]}]}`
	s := parseGroundSummary(t, env.call(t, `{}`))
	if got := findGroundElement(t, s, "Sign in").Centre; got.X != 960 || got.Y != 540 {
		t.Errorf("Sign in centre = %+v, want uncorrected (960,540)", got)
	}
	if s.Correction != nil {
		t.Errorf("summary correction = %+v, want nil (ambiguous)", s.Correction)
	}
}

func TestGroundKWinClassMismatch(t *testing.T) {
	resetGroundSnapshots()
	env := newFakeGroundEnv(t)
	env.helper = groundTreeWithWinJSON()
	env.kwinOK = true
	// Same size, different app class: no correction.
	env.kwin = `{"kwin_windows": [{"cls": "chromium", "active": true, "minimized": false, "fg": [0, 29, 1920, 993], "cg": [0, 29, 1920, 993]}]}`
	s := parseGroundSummary(t, env.call(t, `{}`))
	if got := findGroundElement(t, s, "Sign in").Centre; got.X != 960 || got.Y != 540 {
		t.Errorf("Sign in centre = %+v, want uncorrected (960,540)", got)
	}
	if s.Correction != nil {
		t.Errorf("summary correction = %+v, want nil (class mismatch)", s.Correction)
	}
}

func TestGroundKWinFailure(t *testing.T) {
	resetGroundSnapshots()
	env := newFakeGroundEnv(t)
	env.helper = groundTreeWithWinJSON()
	// kwinOK stays false: the KWin helper is unavailable. The
	// grounding still returns, uncorrected — never a failure.
	s := parseGroundSummary(t, env.call(t, `{}`))
	if got := findGroundElement(t, s, "Sign in").Centre; got.X != 960 || got.Y != 540 {
		t.Errorf("Sign in centre = %+v, want uncorrected (960,540)", got)
	}
	if s.Correction != nil {
		t.Errorf("summary correction = %+v, want nil (KWin unavailable)", s.Correction)
	}
	if s.Coverage != "grounded" {
		t.Errorf("coverage = %q, want grounded", s.Coverage)
	}
}

func TestGroundKWinHelperFile(t *testing.T) {
	env := newFakeGroundEnv(t)
	path, err := env.tool.writeKWinHelper()
	if err != nil {
		t.Fatalf("writeKWinHelper = %v", err)
	}
	if want := filepath.Join(env.home, ".orion", groundKWinHelperFile); path != want {
		t.Errorf("helper path = %q, want %q", path, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat helper: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("helper mode = %o, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read helper: %v", err)
	}
	if string(data) != kwinHelperSource {
		t.Errorf("helper file differs from the embedded source")
	}
	// The KWin helper only reads geometry: it must never ask for
	// window contents, and it must print the kwin_windows key.
	if !strings.Contains(kwinHelperSource, "kwin_windows") {
		t.Errorf("helper source does not print kwin_windows")
	}
	if !strings.Contains(kwinHelperSource, "frameGeometry") {
		t.Errorf("helper source does not read frameGeometry")
	}
}

func TestMatchKWinWindows(t *testing.T) {
	tree := &atspiOutput{
		Windows: []atspiWindow{
			{Title: "Sign in to GitHub · GitHub — Mozilla Firefox", App: "Firefox", X: 0, Y: 0, W: 1920, H: 993},
			{Title: "Kate — notes.txt", App: "Kate", X: 0, Y: 0, W: 800, H: 600},
			{Title: "Hidden panel", App: "plasmashell", X: 0, Y: 0, W: 1920, H: 45},
			// Decorated window: AT-SPI reports the client area
			// (800x570 at y=30); the frame is taller (800x600).
			{Title: "Decorated app", App: "decorated", X: 100, Y: 30, W: 800, H: 570},
		},
	}
	kwin := []kwinWindow{
		{Class: "firefox-esr", FG: [4]int{0, 29, 1920, 993}},
		{Class: "kate", FG: [4]int{0, 0, 800, 600}},
		{Class: "plasmashell", FG: [4]int{0, 0, 1920, 45}},
		{Class: "decorated", FG: [4]int{100, 0, 800, 600}, CG: [4]int{100, 30, 800, 570}},
	}
	offsets := matchKWinWindows(tree, kwin)
	if got, want := offsets[0], [2]int{0, 29}; got != want {
		t.Errorf("Firefox offset = %v, want %v", got, want)
	}
	if got, want := offsets[1], [2]int{0, 0}; got != want {
		t.Errorf("Kate offset = %v, want %v", got, want)
	}
	if got, want := offsets[2], [2]int{0, 0}; got != want {
		t.Errorf("plasmashell offset = %v, want %v", got, want)
	}
	// F-M2R-9: the decorated window matches on CG, not FG — the
	// frame (800x600) never matches the AT-SPI box (800x570),
	// but the client area does, and the offset comes from the
	// client origin (100,30) - (100,30) = (0,0).
	if got, want := offsets[3], [2]int{0, 0}; got != want {
		t.Errorf("decorated-window offset = %v, want %v (CG match)", got, want)
	}
	// Size mismatch: same class, neither FG nor CG fits.
	kwin[0].FG = [4]int{0, 29, 1920, 1050}
	kwin[0].CG = [4]int{0, 29, 1920, 1050}
	if offsets := matchKWinWindows(tree, kwin); len(offsets) != 3 {
		t.Errorf("size mismatch still matched: %v", offsets)
	}
	// CG origin shift: a decorated window whose client area sits
	// below the frame origin gets the CG offset.
	tree2 := &atspiOutput{Windows: []atspiWindow{
		{Title: "Decorated app", App: "decorated", X: 100, Y: 0, W: 800, H: 570},
	}}
	kwin2 := []kwinWindow{
		{Class: "decorated", FG: [4]int{100, 0, 800, 600}, CG: [4]int{100, 30, 800, 570}},
	}
	if offsets := matchKWinWindows(tree2, kwin2); offsets[0] != [2]int{0, 30} {
		t.Errorf("CG-origin offset = %v, want {0 30}", offsets[0])
	}
}

func TestKWinClassCompatible(t *testing.T) {
	cases := []struct {
		app, class string
		want       bool
	}{
		{"Firefox", "firefox-esr", true}, // prefix at '-', live case
		{"firefox", "Firefox", true},     // case-insensitive
		{"kate", "kate", true},           // equal
		{"kate", "kate-wayland", true},   // prefix at '-'
		{"Firefox", "chromium", false},   // different app
		{"fire", "firefox", false},       // prefix, no word boundary
		{"firefox", "firefoxesr", false}, // prefix, no separator
		{"", "firefox", false},           // empty app
		{"Firefox", "", false},           // empty class
		{"plasmashell", "plasmashell", true},
	}
	for _, c := range cases {
		if got := kwinClassCompatible(c.app, c.class); got != c.want {
			t.Errorf("kwinClassCompatible(%q, %q) = %v, want %v", c.app, c.class, got, c.want)
		}
	}
}

func TestApplyWindowOffsets(t *testing.T) {
	win0, win1 := 0, 1
	tree := &atspiOutput{
		Windows: []atspiWindow{
			{Title: "Firefox", X: 0, Y: 0, W: 1920, H: 993},
			{Title: "Kate", X: 0, Y: 0, W: 800, H: 600},
		},
		Elements: []atspiElement{
			{Name: "Firefox element", X: 100, Y: 100, Win: &win0},
			{Name: "Kate element", X: 100, Y: 100, Win: &win1},
			{Name: "Untagged element", X: 100, Y: 100},
		},
	}
	applyWindowOffsets(tree, map[int][2]int{0: {0, 29}})
	if tree.Windows[0].Y != 29 {
		t.Errorf("Firefox window Y = %d, want 29", tree.Windows[0].Y)
	}
	if tree.Windows[1].Y != 0 {
		t.Errorf("Kate window Y = %d, want 0", tree.Windows[1].Y)
	}
	if tree.Elements[0].Y != 129 {
		t.Errorf("Firefox element Y = %d, want 129", tree.Elements[0].Y)
	}
	if tree.Elements[1].Y != 100 {
		t.Errorf("Kate element Y = %d, want 100", tree.Elements[1].Y)
	}
	if tree.Elements[2].Y != 100 {
		t.Errorf("Untagged element Y = %d, want 100", tree.Elements[2].Y)
	}
	// An out-of-range window index is left alone, never panics.
	win9 := 9
	tree.Elements[0].Win = &win9
	applyWindowOffsets(tree, map[int][2]int{0: {0, 29}})
	if tree.Elements[0].Y != 129 {
		t.Errorf("out-of-range win element Y = %d, want 129", tree.Elements[0].Y)
	}
}

// --- F-M2R-4/5: sensitive-window guard fail-open ---

// TestGroundNoTreeNoKWinRefuses pins F-M2R-5 (fail closed): with
// no AT-SPI tree AND no KWin list, no source can say whether a
// sensitive window is visible, so the capture must NOT proceed
// on an unverified screen — not even as vision_only.
func TestGroundNoTreeNoKWinRefuses(t *testing.T) {
	resetGroundSnapshots()
	env := newFakeGroundEnv(t)
	env.helperOK = false // no tree
	// kwinOK stays false: no KWin list either.
	_, err := env.tool.ExecuteResult(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("ground with no tree and no KWin = nil, want a denial")
	}
	var terr *ToolError
	if !errors.As(err, &terr) || terr.Kind != "denied" {
		t.Errorf("error = %v, want denied ToolError", err)
	}
	if !strings.Contains(err.Error(), "cannot verify") {
		t.Errorf("refusal = %q, want the cannot-verify explanation", err.Error())
	}
	if env.captures != 0 {
		t.Errorf("fail-closed refusal still captured %d screenshot(s)", env.captures)
	}
}

// TestGroundKWinSensitiveRefusal pins F-M2R-4: a Qt-style window
// the AT-SPI tree cannot see (no title match in the tree) is
// still caught by the KWin sensitive flag — the KWin script
// matched the caption itself and reported only the pattern.
func TestGroundKWinSensitiveRefusal(t *testing.T) {
	resetGroundSnapshots()
	env := newFakeGroundEnv(t)
	env.kwinOK = true
	env.kwin = `{"kwin_windows": [{"cls": "keepassxc", "active": true, "minimized": false, "fg": [0, 0, 800, 600], "cg": [0, 0, 800, 600], "sens": "keepass"}]}`
	_, err := env.tool.ExecuteResult(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("KWin-flagged sensitive window = nil, want a denial")
	}
	var terr *ToolError
	if !errors.As(err, &terr) || terr.Kind != "denied" {
		t.Errorf("error = %v, want denied ToolError", err)
	}
	if !strings.Contains(err.Error(), "password manager") {
		t.Errorf("refusal = %q, want the category (never the title)", err.Error())
	}
	if strings.Contains(err.Error(), "KeePass") || strings.Contains(err.Error(), "keepassxc") {
		t.Errorf("refusal leaks window identity %q", err.Error())
	}
	if env.captures != 0 {
		t.Errorf("KWin sensitive refusal still captured %d screenshot(s)", env.captures)
	}
}

// TestGroundKWinHelperNoTitles pins F-M2R-15: the KWin helper's
// output passes through the systemd journal, so it must never
// carry a window caption — only geometries, the app class and
// the matched sensitive pattern.
func TestGroundKWinHelperNoTitles(t *testing.T) {
	if strings.Contains(kwinHelperSource, `"caption"`) || strings.Contains(kwinHelperSource, "caption:") {
		t.Errorf("KWin helper source prints a caption — titles must never reach the journal (F-M2R-15)")
	}
	if !strings.Contains(kwinHelperSource, "sens") {
		t.Errorf("KWin helper source has no sensitive flag — the check must run inside KWin")
	}
	// The script reads the caption only to match it locally.
	if !strings.Contains(kwinHelperSource, "w.caption") {
		t.Errorf("KWin helper source does not read w.caption for the in-script sensitive match")
	}
}
