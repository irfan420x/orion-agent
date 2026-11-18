// Tests for the input tool (computer-use M3). The real backends
// (ydotoold, KWin, python3/AT-SPI, xdotool) never run here: every
// seam is stubbed and the desktop is simulated (a fake cursor
// position, a fake acceleration factor, a fake KWin oracle), so
// the tests run on a headless VM. Live input is verified on the
// laptop instead (click is untested there too, by design — no
// safe target was established).
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/irfan/orion/internal/permissions"
)

// fakeYdotoolCall is one recorded ydotool invocation: argv plus
// the stdin the type call carried ("" for every other call).
type fakeYdotoolCall struct {
	stdin string
	args  []string
}

// fakeInputEnv is a stubbed input tool over a simulated desktop:
// env.cursor is the true cursor position, env.screen the virtual
// screen, and the fake KWin oracle reports env.cursor (or
// env.oracleFixed when set, to simulate an oracle that
// disagrees with the transport).
type fakeInputEnv struct {
	tool  *inputTool
	clock time.Time
	home  string

	cursor          cursorPos
	screen          inputScreen
	stuck           bool    // ydotool moves never land
	firstMoveFactor float64 // acceleration distortion on the 1st move only (1.0 = exact)
	// moveFactor, when set, replaces firstMoveFactor for every
	// move: the simulated acceleration factor for an issued
	// (dx, dy) residual. A test uses it to model
	// magnitude-dependent distortion (large deltas land far
	// short, small ones nearly exact).
	moveFactor  func(dx, dy int) float64
	oracleFixed *cursorPos // oracle reports this instead of cursor (disagreement)
	oracleErr   bool
	kwinWindows []kwinWindow
	// kwinWindowsAfterClick, when set, replaces kwinWindows the
	// moment the fake ydotool processes a click: the simulated
	// desktop's reaction to the click (e.g. the target window
	// taking focus). Unset means the click changed nothing —
	// the missed-click simulation.
	kwinWindowsAfterClick []kwinWindow
	moveCount             int

	tree string // canned AT-SPI tree JSON ("" = default benign tree)
	// treeAfterClick, when set, replaces tree the moment the
	// fake ydotool processes a click: the simulated desktop's
	// reaction to the click (e.g. another control taking
	// focus). treeErrAfterClick fails the tree walk after a
	// click instead. Unset means the click changed nothing.
	treeAfterClick    string
	treeErrAfterClick bool
	clicked           bool // the fake ydotool processed a click
	treeErr           bool // atspiTree fails -> xdotool fallback
	xdotoolTitles     []string
	xdotoolErr        bool
	locked            bool

	socketExists bool
	socketPath   string
	ydotoolCalls []fakeYdotoolCall
	kwinCalls    int
	kwinFlipTo   string // F-A23W2-10: from the 2nd KWin call on, this class is the active window (simulates a focus change mid-call)
}

func benignInputTreeJSON() string {
	return `{"windows": [
		{"title": "Sign in to GitHub · GitHub — Mozilla Firefox", "app": "Firefox", "role": "frame", "focused": true, "x": 0, "y": 0, "w": 1920, "h": 1080},
		{"title": "Kate — notes.txt", "app": "Kate", "role": "frame", "focused": false, "x": 0, "y": 0, "w": 800, "h": 600}],
	"elements": [],
	"focused_window": {"title": "Sign in to GitHub · GitHub — Mozilla Firefox", "app": "Firefox"},
	"walked_nodes": 140}`
}

func sensitiveInputTreeJSON() string {
	return `{"windows": [
		{"title": "KeePassXC — passwords.kdbx", "app": "KeePassXC", "role": "frame", "focused": true, "x": 100, "y": 100, "w": 800, "h": 600},
		{"title": "Sign in to GitHub · GitHub — Mozilla Firefox", "app": "Firefox", "role": "frame", "focused": false, "x": 0, "y": 0, "w": 1920, "h": 1080}],
	"elements": [],
	"focused_window": {"title": "KeePassXC — passwords.kdbx", "app": "KeePassXC"},
	"walked_nodes": 12}`
}

// contentSensitiveInputTreeJSON (F-M2R-14): a login page whose
// window title is innocuous ("Welcome") but whose body holds a
// password field — the title check alone cannot catch it.
func contentSensitiveInputTreeJSON() string {
	return `{"windows": [
		{"title": "Welcome — Mozilla Firefox", "app": "Firefox", "role": "frame", "focused": true, "x": 0, "y": 0, "w": 1920, "h": 1080}],
	"elements": [
		{"role": "heading", "name": "Welcome", "x": 800, "y": 300, "w": 200, "h": 30, "win": 0, "showing": true},
		{"role": "entry", "name": "Username", "x": 784, "y": 383, "w": 352, "h": 40, "win": 0, "showing": true},
		{"role": "password text", "name": "Password", "x": 784, "y": 464, "w": 352, "h": 40, "win": 0, "showing": true},
		{"role": "push button", "name": "Log in", "x": 784, "y": 540, "w": 100, "h": 40, "win": 0, "showing": true}],
	"focused_window": {"title": "Welcome — Mozilla Firefox", "app": "Firefox"},
	"walked_nodes": 5}`
}

// contentOTPInputTreeJSON (F-M2R-14): the same innocuous title,
// but a one-time-code field instead of a password field.
func contentOTPInputTreeJSON() string {
	return `{"windows": [
		{"title": "Welcome — Mozilla Firefox", "app": "Firefox", "role": "frame", "focused": true, "x": 0, "y": 0, "w": 1920, "h": 1080}],
	"elements": [
		{"role": "heading", "name": "Sign in", "x": 800, "y": 300, "w": 200, "h": 30, "win": 0, "showing": true},
		{"role": "text", "name": "Enter one-time code", "x": 784, "y": 383, "w": 352, "h": 40, "win": 0, "showing": true},
		{"role": "push button", "name": "Verify", "x": 784, "y": 460, "w": 100, "h": 40, "win": 0, "showing": true}],
	"focused_window": {"title": "Welcome — Mozilla Firefox", "app": "Firefox"},
	"walked_nodes": 4}`
}

// contentBenignSignInTreeJSON (F-M2R-14): a sign-in page in
// words only — heading, username field, sign-in button — with
// NO password or OTP field: typing and clicking into it must
// stay allowed.
func contentBenignSignInTreeJSON() string {
	return `{"windows": [
		{"title": "Sign in to GitHub · GitHub — Mozilla Firefox", "app": "Firefox", "role": "frame", "focused": true, "x": 0, "y": 0, "w": 1920, "h": 1080}],
	"elements": [
		{"role": "heading", "name": "Sign in to GitHub", "x": 784, "y": 300, "w": 200, "h": 30, "win": 0, "showing": true},
		{"role": "entry", "name": "Username or email address", "x": 784, "y": 383, "w": 352, "h": 40, "win": 0, "showing": true},
		{"role": "push button", "name": "Sign in", "x": 784, "y": 460, "w": 100, "h": 40, "win": 0, "showing": true},
		{"role": "link", "name": "Forgot password?", "x": 1019, "y": 439, "w": 117, "h": 21, "win": 0, "showing": true}],
	"focused_window": {"title": "Sign in to GitHub · GitHub — Mozilla Firefox", "app": "Firefox"},
	"walked_nodes": 5}`
}

// contentHiddenPasswordTreeJSON (F-M2R-14): the password field
// is hidden — a closed popup's node lingering in the tree —
// so it must not refuse.
func contentHiddenPasswordTreeJSON() string {
	return `{"windows": [
		{"title": "Welcome — Mozilla Firefox", "app": "Firefox", "role": "frame", "focused": true, "x": 0, "y": 0, "w": 1920, "h": 1080}],
	"elements": [
		{"role": "password text", "name": "Password", "x": 784, "y": 464, "w": 352, "h": 40, "win": 0, "showing": false}],
	"focused_window": {"title": "Welcome — Mozilla Firefox", "app": "Firefox"},
	"walked_nodes": 2}`
}

// contentAndTitleTreeJSON (F-M2R-14): BOTH signals at once — a
// title-sensitive KeePassXC window plus a content-sensitive
// Firefox login page. A click inside the Firefox box but
// outside the KeePassXC box must still refuse: the old
// title-only check would have allowed it.
func contentAndTitleTreeJSON() string {
	return `{"windows": [
		{"title": "KeePassXC — passwords.kdbx", "app": "KeePassXC", "role": "frame", "focused": false, "x": 100, "y": 100, "w": 800, "h": 600},
		{"title": "Welcome — Mozilla Firefox", "app": "Firefox", "role": "frame", "focused": true, "x": 0, "y": 0, "w": 1920, "h": 1080}],
	"elements": [
		{"role": "password text", "name": "Password", "x": 784, "y": 464, "w": 352, "h": 40, "win": 1, "showing": true}],
	"focused_window": {"title": "Welcome — Mozilla Firefox", "app": "Firefox"},
	"walked_nodes": 4}`
}

func newFakeInputEnv(t *testing.T) *fakeInputEnv {
	t.Helper()
	t.Setenv("DISPLAY", ":1")
	home := t.TempDir()
	env := &fakeInputEnv{
		tool:            newInputTool(),
		clock:           time.Now(),
		home:            home,
		cursor:          cursorPos{X: 100, Y: 100},
		screen:          inputScreen{X: 0, Y: 0, W: 1920, H: 1080},
		firstMoveFactor: 1.0,
		socketExists:    true,
	}
	env.socketPath = filepath.Join(home, ".ydotool_socket")
	if err := os.WriteFile(env.socketPath, nil, 0600); err != nil {
		t.Fatalf("fake socket write: %v", err)
	}
	tool := env.tool
	tool.homeDir = func() (string, error) { return home, nil }
	tool.now = func() time.Time { return env.clock }
	tool.sleep = func(time.Duration) {}
	tool.lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	tool.socketPath = func() (string, error) { return env.socketPath, nil }
	tool.ydotool = func(ctx context.Context, stdin string, args ...string) (string, error) {
		env.ydotoolCalls = append(env.ydotoolCalls, fakeYdotoolCall{stdin: stdin, args: append([]string{}, args...)})
		if len(args) > 0 && args[0] == "click" {
			env.clicked = true
			if env.kwinWindowsAfterClick != nil {
				// The simulated desktop reacts to the click:
				// from here on the KWin oracle reports the
				// post-click window list (focus included).
				env.kwinWindows = env.kwinWindowsAfterClick
			}
		}
		if len(args) > 0 && args[0] == "mousemove" {
			env.moveCount++
			dx, dy := 0, 0
			for i := 1; i+1 < len(args); i += 2 {
				v, _ := strconv.Atoi(args[i+1])
				if args[i] == "-x" {
					dx = v
				} else if args[i] == "-y" {
					dy = v
				}
			}
			if !env.stuck {
				f := 1.0
				if env.moveFactor != nil {
					f = env.moveFactor(dx, dy)
				} else if env.moveCount == 1 {
					f = env.firstMoveFactor
				}
				env.cursor.X += int(float64(dx) * f)
				env.cursor.Y += int(float64(dy) * f)
			}
			return "", nil
		}
		return "", nil
	}
	tool.runKWinHelper = func(ctx context.Context, helperPath string) ([]byte, error) {
		env.kwinCalls++
		if env.oracleErr {
			return nil, errors.New("fake KWin helper failure")
		}
		cur := env.cursor
		if env.oracleFixed != nil {
			cur = *env.oracleFixed
		}
		wins := env.kwinWindows
		if env.kwinFlipTo != "" && env.kwinCalls > 1 {
			flipped := make([]kwinWindow, len(wins))
			copy(flipped, wins)
			for i := range flipped {
				flipped[i].Active = flipped[i].Class == env.kwinFlipTo
			}
			wins = flipped
		}
		out := inputCursorOutput{Cursor: cur, Screen: &env.screen, Windows: wins}
		raw, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("fake KWin marshal: %v", err)
		}
		return raw, nil
	}
	tool.writeKWinHelper = func() (string, error) { return filepath.Join(home, "input-kwin-helper.py"), nil }
	// The ground tool inside input: stub its AT-SPI seams.
	g := tool.ground
	g.homeDir = func() (string, error) { return home, nil }
	g.lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	g.execCmd = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "python3" {
			return "", nil // import smoke test
		}
		t.Errorf("unexpected ground exec: %s %v", name, args)
		return "", nil
	}
	g.runHelper = func(ctx context.Context, helperPath string) ([]byte, error) {
		if env.clicked && env.treeErrAfterClick {
			return nil, errors.New("fake AT-SPI helper failure after the click")
		}
		if env.clicked && env.treeAfterClick != "" {
			return []byte(env.treeAfterClick), nil
		}
		if env.treeErr {
			return nil, errors.New("fake AT-SPI helper failure")
		}
		if env.tree != "" {
			return []byte(env.tree), nil
		}
		return []byte(benignInputTreeJSON()), nil
	}
	g.writeHelper = func() (string, error) { return filepath.Join(home, "ground-atspi-helper.py"), nil }
	// The screenshot tool inside input: stub its two seams.
	s := tool.shot
	s.homeDir = func() (string, error) { return home, nil }
	s.now = func() time.Time { return env.clock }
	s.windowTitles = func(ctx context.Context) ([]string, error) {
		if env.xdotoolErr {
			return nil, errors.New("fake xdotool failure")
		}
		return env.xdotoolTitles, nil
	}
	s.screenLocked = func(ctx context.Context) (bool, bool) { return env.locked, true }
	return env
}

func (e *fakeInputEnv) advance() { e.clock = e.clock.Add(2 * time.Second) }

func (e *fakeInputEnv) call(t *testing.T, args string) ToolResult {
	t.Helper()
	res, err := e.tool.ExecuteResult(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("ExecuteResult(%s) = %v, want nil", args, err)
	}
	return res
}

func (e *fakeInputEnv) callErr(t *testing.T, args string) error {
	t.Helper()
	_, err := e.tool.ExecuteResult(context.Background(), json.RawMessage(args))
	if err == nil {
		t.Fatalf("ExecuteResult(%s) = nil, want an error", args)
	}
	return err
}

func parseInputSummary(t *testing.T, res ToolResult) inputSummary {
	t.Helper()
	var s inputSummary
	if err := json.Unmarshal([]byte(res.Text), &s); err != nil {
		t.Fatalf("input summary JSON %q does not parse: %v", res.Text, err)
	}
	return s
}

func (e *fakeInputEnv) mousemoveCalls() []fakeYdotoolCall {
	var out []fakeYdotoolCall
	for _, c := range e.ydotoolCalls {
		if len(c.args) > 0 && c.args[0] == "mousemove" {
			out = append(out, c)
		}
	}
	return out
}

func (e *fakeInputEnv) clickCalls() []fakeYdotoolCall {
	var out []fakeYdotoolCall
	for _, c := range e.ydotoolCalls {
		if len(c.args) > 0 && c.args[0] == "click" {
			out = append(out, c)
		}
	}
	return out
}

func TestInputMoveConvergesAfterDistortedLanding(t *testing.T) {
	env := newFakeInputEnv(t)
	// The first relative move lands at 40% (libinput acceleration
	// fighting the landing, as in the live M3 probe); the loop
	// must correct the residual and converge. The (100,100) to
	// (960,540) jump is ~966 px, so it runs staged in 3 legs of
	// <=400 px: the distorted first move lands short of the
	// first waypoint, one exact correction finishes that leg,
	// and the two remaining legs land exactly — 4 moves total.
	env.firstMoveFactor = 0.4
	res := env.call(t, `{"op": "move", "x": 960, "y": 540}`)
	s := parseInputSummary(t, res)
	if s.Op != "move" || !s.Reached {
		t.Errorf("summary = %+v, want op move reached", s)
	}
	if s.Final == nil || *s.Final != (cursorPos{X: 960, Y: 540}) {
		t.Errorf("final = %+v, want {960 540}", s.Final)
	}
	if s.Target == nil || *s.Target != (cursorPos{X: 960, Y: 540}) {
		t.Errorf("target = %+v, want {960 540}", s.Target)
	}
	if s.Iterations != 4 {
		t.Errorf("correction_moves = %d, want 4 (distorted first leg + exact correction + 2 exact legs)", s.Iterations)
	}
	if got := len(env.mousemoveCalls()); got != 4 {
		t.Errorf("got %d mousemove calls, want 4", got)
	}
	// The first call moves toward the first waypoint (387, 247),
	// not the whole residual (860, 440) — the jump is staged.
	first := env.mousemoveCalls()[0]
	if fmt.Sprint(first.args) != fmt.Sprint([]string{"mousemove", "-x", "287", "-y", "147"}) {
		t.Errorf("first mousemove = %v, want the first leg's residual (287, 147)", first.args)
	}
}

// TestInputMoveLongJumpStagedConverges pins F-A23-W4-2 (Audit 23,
// live): the first move to (744, 998) failed — 5 correction moves
// from (1655,176) did not converge and the cursor stuck at
// (970,1023) — while a retry from a nearer start succeeded in 2
// moves. The fake models the cause, magnitude-dependent
// acceleration distortion: a relative move lands at 55% when
// either component exceeds 400 px, at 95% for small steps. Under
// that model a single 5-move loop cannot converge the ~1,227 px
// jump (each full-residual move leaves 45% behind), but staged
// legs of <=400 px converge in 2 moves each — so the move must
// succeed, issuing only small steps, and take more than the old
// 5-move budget to do it.
func TestInputMoveLongJumpStagedConverges(t *testing.T) {
	env := newFakeInputEnv(t)
	env.cursor = cursorPos{X: 1655, Y: 176}
	env.moveFactor = func(dx, dy int) float64 {
		if dx < 0 {
			dx = -dx
		}
		if dy < 0 {
			dy = -dy
		}
		if dx > 400 || dy > 400 {
			return 0.55
		}
		return 0.95
	}
	res := env.call(t, `{"op": "move", "x": 744, "y": 998}`)
	s := parseInputSummary(t, res)
	if s.Op != "move" || !s.Reached {
		t.Errorf("summary = %+v, want op move reached", s)
	}
	if s.Final == nil || !withinTolerance(*s.Final, 744, 998) {
		t.Errorf("final = %+v, want within 5 px of {744 998}", s.Final)
	}
	if s.Iterations <= 5 {
		t.Errorf("correction_moves = %d, want more than 5 (the old single-loop budget, which live failed to converge)", s.Iterations)
	}
	// Every issued step is a small step: no single relative
	// move exceeds the 400 px stage cap on either axis.
	for _, c := range env.mousemoveCalls() {
		for i := 1; i+1 < len(c.args); i += 2 {
			v, _ := strconv.Atoi(c.args[i+1])
			if v < 0 {
				v = -v
			}
			if v > 400 {
				t.Errorf("mousemove %v issues a %d px step, want every step <= %d px", c.args, v, inputMoveStagePx)
			}
		}
	}
}

// TestInputMoveLongJumpStillGivesUpWhenStuck pins the honest
// failure of a staged move: a cursor that never lands fails on
// the FIRST leg (5 correction moves, no infinite loop, no click
// follows) and the error still names the real target and the
// final oracle position.
func TestInputMoveLongJumpStillGivesUpWhenStuck(t *testing.T) {
	env := newFakeInputEnv(t)
	env.stuck = true
	env.cursor = cursorPos{X: 1655, Y: 176}
	err := env.callErr(t, `{"op": "move", "x": 744, "y": 998}`)
	if !strings.Contains(err.Error(), "could not reach (744, 998)") {
		t.Errorf("err = %v, want 'could not reach (744, 998)'", err)
	}
	if !strings.Contains(err.Error(), "leg 1 of 4 failed") {
		t.Errorf("err = %v, want the failed leg named", err)
	}
	if !strings.Contains(err.Error(), "after 5 correction moves") {
		t.Errorf("err = %v, want the per-leg iteration count in the message", err)
	}
	if !strings.Contains(err.Error(), "cursor is at (1655, 176)") {
		t.Errorf("err = %v, want the final oracle position in the message", err)
	}
	if got := len(env.mousemoveCalls()); got != 5 {
		t.Errorf("got %d mousemove calls, want 5 (first leg exhausted, not infinite)", got)
	}
}

func TestInputMoveAlreadyThereIssuesNoCall(t *testing.T) {
	env := newFakeInputEnv(t)
	env.cursor = cursorPos{X: 962, Y: 541}
	res := env.call(t, `{"op": "move", "x": 960, "y": 540}`)
	s := parseInputSummary(t, res)
	if !s.Reached || s.Iterations != 0 {
		t.Errorf("summary = %+v, want reached with 0 correction moves", s)
	}
	if len(env.ydotoolCalls) != 0 {
		t.Errorf("got %d ydotool calls, want 0 (already within tolerance)", len(env.ydotoolCalls))
	}
}

func TestInputMoveGivesUpHonestlyWhenStuck(t *testing.T) {
	env := newFakeInputEnv(t)
	env.stuck = true
	err := env.callErr(t, `{"op": "move", "x": 960, "y": 540}`)
	if !strings.Contains(err.Error(), "could not reach (960, 540)") {
		t.Errorf("err = %v, want 'could not reach (960, 540)'", err)
	}
	if !strings.Contains(err.Error(), "cursor is at (100, 100)") {
		t.Errorf("err = %v, want the final oracle position in the message", err)
	}
	if !strings.Contains(err.Error(), "after 5 correction moves") {
		t.Errorf("err = %v, want the iteration count in the message", err)
	}
	if got := len(env.mousemoveCalls()); got != 5 {
		t.Errorf("got %d mousemove calls, want 5 (exhausted, not infinite)", got)
	}
}

func TestInputMoveNeverClaimsSuccessWhenOracleDisagrees(t *testing.T) {
	env := newFakeInputEnv(t)
	// The transport works (the fake cursor moves exactly) but
	// the oracle insists the cursor is at (0, 0): the tool must
	// trust the oracle and fail, never claim the target.
	env.oracleFixed = &cursorPos{X: 0, Y: 0}
	err := env.callErr(t, `{"op": "move", "x": 960, "y": 540}`)
	if !strings.Contains(err.Error(), "could not reach") {
		t.Errorf("err = %v, want a could-not-reach failure, not success", err)
	}
	if env.cursor == (cursorPos{X: 0, Y: 0}) {
		t.Errorf("fake cursor never moved — the test setup is wrong (transport must work, oracle must disagree)")
	}
}

func TestInputMoveOutOfBoundsAfterOracleReportsScreen(t *testing.T) {
	env := newFakeInputEnv(t)
	err := env.callErr(t, `{"op": "move", "x": 5000, "y": 5000}`)
	if !strings.Contains(err.Error(), "outside the 1920x1080 screen") {
		t.Errorf("err = %v, want the screen-bounds refusal", err)
	}
	if len(env.mousemoveCalls()) != 0 {
		t.Errorf("got %d mousemove calls, want 0 (rejected before moving)", len(env.mousemoveCalls()))
	}
}

func TestInputMoveOracleFailure(t *testing.T) {
	env := newFakeInputEnv(t)
	env.oracleErr = true
	err := env.callErr(t, `{"op": "move", "x": 960, "y": 540}`)
	if !strings.Contains(err.Error(), "KWin oracle") {
		t.Errorf("err = %v, want the KWin-oracle failure, not a move on no proof", err)
	}
	if len(env.ydotoolCalls) != 0 {
		t.Errorf("got %d ydotool calls, want 0 (no oracle, no move)", len(env.ydotoolCalls))
	}
}

func TestInputClickByCoordinates(t *testing.T) {
	resetGroundSnapshots()
	registerFreshGrounding()
	env := newFakeInputEnv(t)
	res := env.call(t, `{"op": "click", "x": 500, "y": 300}`)
	s := parseInputSummary(t, res)
	if s.Op != "click" || !s.Reached || s.Button != "left" || s.Clicks != 1 {
		t.Errorf("summary = %+v, want click left x1 reached", s)
	}
	if s.Final == nil || *s.Final != (cursorPos{X: 500, Y: 300}) {
		t.Errorf("final = %+v, want {500 300}", s.Final)
	}
	clicks := env.clickCalls()
	if len(clicks) != 1 {
		t.Fatalf("got %d click calls, want 1", len(clicks))
	}
	if fmt.Sprint(clicks[0].args) != fmt.Sprint([]string{"click", "--repeat", "1", "0xC0"}) {
		t.Errorf("click call = %v, want left click (0xC0)", clicks[0].args)
	}
}

func TestInputClickButtonsAndDouble(t *testing.T) {
	resetGroundSnapshots()
	registerFreshGrounding()
	env := newFakeInputEnv(t)
	env.call(t, `{"op": "click", "button": "right", "x": 500, "y": 300}`)
	env.advance()
	env.call(t, `{"op": "click", "button": "middle", "clicks": 2, "x": 500, "y": 300}`)
	clicks := env.clickCalls()
	if len(clicks) != 2 {
		t.Fatalf("got %d click calls, want 2", len(clicks))
	}
	if fmt.Sprint(clicks[0].args) != fmt.Sprint([]string{"click", "--repeat", "1", "0xC1"}) {
		t.Errorf("right click = %v, want 0xC1", clicks[0].args)
	}
	if fmt.Sprint(clicks[1].args) != fmt.Sprint([]string{"click", "--repeat", "2", "0xC2"}) {
		t.Errorf("middle double-click = %v, want --repeat 2 0xC2", clicks[1].args)
	}
}

func TestInputClickBySnapshot(t *testing.T) {
	resetGroundSnapshots()
	registerSignInSnapshot()
	env := newFakeInputEnv(t)
	env.kwinWindows = firefoxKWinWindow(0)
	// The fresh tree still shows the button at its grounded
	// centre: the pre-click re-check passes and the click lands.
	env.tree = treeWithButtonJSON(910, 378, 100, 50)
	res := env.call(t, `{"op": "click", "snapshot_id": "g-test-1", "element_id": 1}`)
	s := parseInputSummary(t, res)
	if !s.Reached || s.Target == nil || *s.Target != (cursorPos{X: 960, Y: 403}) {
		t.Errorf("summary = %+v, want the click at the registered element centre (960, 403)", s)
	}
	if s.Element == nil || s.Element.SnapshotID != "g-test-1" || s.Element.ElementID != 1 {
		t.Errorf("element = %+v, want the snapshot binding in the result", s.Element)
	}
	if len(env.clickCalls()) != 1 {
		t.Errorf("got %d click calls, want 1", len(env.clickCalls()))
	}
}

func TestInputClickBySnapshotRefusals(t *testing.T) {
	resetGroundSnapshots()
	groundSnapshots.register("g-test-1", 0, time.Now(), []groundElement{
		{ID: 1, Centre: groundPoint{X: 960, Y: 403}},
	}, nil)
	env := newFakeInputEnv(t)
	// Unknown snapshot: refused before any desktop touch.
	err := env.callErr(t, `{"op": "click", "snapshot_id": "g-nope", "element_id": 1}`)
	if !strings.Contains(err.Error(), "not available") {
		t.Errorf("unknown snapshot err = %v, want 'not available'", err)
	}
	// Unknown element in a known snapshot.
	err = env.callErr(t, `{"op": "click", "snapshot_id": "g-test-1", "element_id": 99}`)
	if !strings.Contains(err.Error(), "not in snapshot") {
		t.Errorf("unknown element err = %v, want 'not in snapshot'", err)
	}
	// Stale snapshot (older than the 60 s TTL).
	resetGroundSnapshots()
	groundSnapshots.register("g-old", 0, time.Now().Add(-61*time.Second), []groundElement{
		{ID: 1, Centre: groundPoint{X: 960, Y: 403}},
	}, nil)
	err = env.callErr(t, `{"op": "click", "snapshot_id": "g-old", "element_id": 1}`)
	if !strings.Contains(err.Error(), "old") {
		t.Errorf("stale snapshot err = %v, want the age refusal", err)
	}
	if len(env.ydotoolCalls) != 0 {
		t.Errorf("got %d ydotool calls, want 0 (refused before touching the desktop)", len(env.ydotoolCalls))
	}
}

func TestInputSnapshotCentreLookup(t *testing.T) {
	resetGroundSnapshots()
	groundSnapshots.register("g-c", 0, time.Now(), []groundElement{
		{ID: 1, Centre: groundPoint{X: 960, Y: 403}},
		{ID: 2, Centre: groundPoint{X: 100, Y: 200}},
	}, nil)
	ctx := context.Background()
	c, err := GroundSnapshotElementCentre(ctx, "g-c", 2)
	if err != nil {
		t.Fatalf("GroundSnapshotElementCentre = %v, want nil", err)
	}
	if c != (groundPoint{X: 100, Y: 200}) {
		t.Errorf("centre = %+v, want {100 200}", c)
	}
	if _, err := GroundSnapshotElementCentre(ctx, "g-c", 99); err == nil {
		t.Errorf("unknown element should be refused")
	}
	if _, err := GroundSnapshotElementCentre(ctx, "g-nope", 1); err == nil {
		t.Errorf("unknown snapshot should be refused")
	}
}

// TestInputClickBySnapshotCrossChat (F-A23W2-19): a snapshot
// grounded in chat 111 cannot be clicked from chat 222 — the
// click refuses before any desktop touch — while the same chat
// clicks it fine. The id is identical in both cases: the chat
// binding, not the id, decides.
func TestInputClickBySnapshotCrossChat(t *testing.T) {
	resetGroundSnapshots()
	groundSnapshots.register("g-cross", 111, time.Now(), []groundElement{
		{ID: 1, Centre: groundPoint{X: 960, Y: 403}},
	}, nil)
	env := newFakeInputEnv(t)
	ctxOther := permissions.WithApprovalContext(context.Background(), 222, 222)
	_, err := env.tool.ExecuteResult(ctxOther, json.RawMessage(`{"op": "click", "snapshot_id": "g-cross", "element_id": 1}`))
	if err == nil || !strings.Contains(err.Error(), "different chat") {
		t.Errorf("cross-chat click err = %v, want 'different chat'", err)
	}
	if len(env.ydotoolCalls) != 0 {
		t.Errorf("got %d ydotool calls, want 0 (refused before touching the desktop)", len(env.ydotoolCalls))
	}
	ctxSame := permissions.WithApprovalContext(context.Background(), 111, 111)
	env.advance()
	res, err := env.tool.ExecuteResult(ctxSame, json.RawMessage(`{"op": "click", "snapshot_id": "g-cross", "element_id": 1}`))
	if err != nil {
		t.Fatalf("same-chat click = %v, want nil", err)
	}
	if s := parseInputSummary(t, res); !s.Reached {
		t.Errorf("same-chat click summary = %+v, want reached", s)
	}
}

func TestInputClickValidation(t *testing.T) {
	env := newFakeInputEnv(t)
	cases := map[string]string{
		`{"op": "click", "button": "side", "x": 1, "y": 1}`: "button must be",
		`{"op": "click", "clicks": 3, "x": 1, "y": 1}`:      "clicks must be 1 or 2",
		`{"op": "click", "clicks": 0, "x": 1, "y": 1}`:      "clicks must be 1 or 2",
		`{"op": "click"}`: "needs either snapshot_id",
		`{"op": "click", "snapshot_id": "g-x", "x": 1, "y": 1}`:       "not both",
		`{"op": "click", "snapshot_id": "g-x"}`:                       "needs both snapshot_id and element_id",
		`{"op": "click", "element_id": 1}`:                            "needs both snapshot_id and element_id",
		`{"op": "click", "x": 1}`:                                     "needs both x and y",
		`{"op": "click", "x": -5, "y": 1}`:                            "non-negative",
		`{"op": "move"}`:                                              "move needs x and y",
		`{"op": "move", "x": -1, "y": 1}`:                             "non-negative",
		`{"op": "drag", "x": 1, "y": 1}`:                              "op must be move, click, type or key",
		`{"op": "type", "text": ""}`:                                  "needs non-empty text",
		`{"op": "key", "key": ""}`:                                    "not in the allowlist",
		`{"op": "key", "key": "a"}`:                                   "not in the allowlist",
		`{"op": "key", "key": "ctrl"}`:                                "not in the allowlist",
		`{"op": "key", "key": "Return+Tab"}`:                          "not in the allowlist",
		`{"op": "type", "text": "a\u0009b"}`:                          "control character",
		`{"op": "type", "text": "a\nb"}`:                              "control character",
		`{"op": "type", "text": "` + strings.Repeat("x", 1001) + `"}`: "over the 1000-rune cap",
	}
	for args, want := range cases {
		if err := env.callErr(t, args); !strings.Contains(err.Error(), want) {
			t.Errorf("ExecuteResult(%s) err = %v, want %q", args, err, want)
		}
		env.advance()
	}
	if len(env.ydotoolCalls) != 0 {
		t.Errorf("got %d ydotool calls, want 0 (validation fails before touching the desktop)", len(env.ydotoolCalls))
	}
}

func TestInputTypeHappyPathNeverEchoesText(t *testing.T) {
	env := newFakeInputEnv(t)
	res := env.call(t, `{"op": "type", "text": "hello world"}`)
	s := parseInputSummary(t, res)
	if s.CharsTyped != 11 {
		t.Errorf("chars_typed = %d, want 11", s.CharsTyped)
	}
	if strings.Contains(res.Text, "hello") {
		t.Errorf("result echoes the typed text %q — typed text (possibly a password) must never appear in a result", res.Text)
	}
	if len(env.ydotoolCalls) != 1 {
		t.Fatalf("got %d ydotool calls, want 1", len(env.ydotoolCalls))
	}
	call := env.ydotoolCalls[0]
	if fmt.Sprint(call.args) != fmt.Sprint([]string{"type", "--file", "-"}) {
		t.Errorf("type call = %v, want type --file - (stdin carries the text)", call.args)
	}
	if call.stdin != "hello world" {
		t.Errorf("type stdin = %q, want the text on stdin, not in argv", call.stdin)
	}
}

func TestInputKeyAllowlist(t *testing.T) {
	cases := map[string]int{
		"Return": 28, "Enter": 28, "Escape": 1, "Esc": 1, "Tab": 15,
		"BackSpace": 14, "Delete": 111, "Home": 102, "End": 107,
		"PageUp": 104, "Page_Down": 109, "PageDown": 109, "Page_Up": 104, "Up": 103, "Down": 108,
		"Left": 105, "Right": 106, "Space": 57,
		"F1": 59, "F2": 60, "F3": 61, "F4": 62, "F5": 63, "F6": 64, "F7": 65, "F8": 66, "F9": 67, "F10": 68, "F11": 87, "F12": 88,
	}
	for key, wantCode := range cases {
		env := newFakeInputEnv(t)
		res := env.call(t, fmt.Sprintf(`{"op": "key", "key": %q}`, key))
		s := parseInputSummary(t, res)
		if s.Key != strings.ToLower(key) {
			t.Errorf("key %q: summary key = %q, want %q", key, s.Key, strings.ToLower(key))
		}
		if len(env.ydotoolCalls) != 1 {
			t.Fatalf("key %q: got %d ydotool calls, want 1", key, len(env.ydotoolCalls))
		}
		want := []string{"key", strconv.Itoa(wantCode) + ":1", strconv.Itoa(wantCode) + ":0"}
		if fmt.Sprint(env.ydotoolCalls[0].args) != fmt.Sprint(want) {
			t.Errorf("key %q: call = %v, want %v (numeric KEYCODE:PRESSED — names are silently delays)", key, env.ydotoolCalls[0].args, want)
		}
	}
	// The allowlist is exactly the map: a name in the map but
	// not in the table above (or vice versa) fails here.
	if len(inputKeyCodes) != len(cases) {
		t.Errorf("inputKeyCodes has %d entries, want the %d tested above — a new key needs a test row", len(inputKeyCodes), len(cases))
	}
	for key, code := range cases {
		if got, ok := inputKeyCodes[strings.ToLower(key)]; !ok || got != code {
			t.Errorf("inputKeyCodes[%q] = %d, want %d", key, got, code)
		}
	}
}

func TestInputRiskClassGateAgreement(t *testing.T) {
	tool := newInputTool()
	if got := tool.RiskClass(); got != permissions.ClassDestructive {
		t.Errorf("RiskClass = %v, want destructive (click/type/key can activate or submit)", got)
	}
	if got := permissions.Classify("input"); got != permissions.ClassDestructive {
		t.Errorf("gate Classify(input) = %v, want destructive", got)
	}
	if _, ok := permissions.ClassifyKnown("input"); !ok {
		t.Errorf("input is not explicitly classified in the gate maps")
	}
	// Register rejects a declaration/gate mismatch, so a
	// successful Register is the agreement proof.
	reg := NewRegistry()
	if err := reg.Register(tool); err != nil {
		t.Errorf("Register(input) = %v, want nil (declaration must agree with the gate maps)", err)
	}
	// And the factory set carries it, so the README/skills/gate
	// pins see it too.
	found := false
	for _, name := range fullBuiltInToolNames(t) {
		if name == "input" {
			found = true
		}
	}
	if !found {
		t.Errorf("input is not in the factory set (RegisterFactory missing?)")
	}
}

func TestInputUnattendedRefused(t *testing.T) {
	env := newFakeInputEnv(t)
	ctx := WithUnattendedTurn(context.Background())
	for _, args := range []string{
		`{"op": "move", "x": 1, "y": 1}`,
		`{"op": "click", "x": 1, "y": 1}`,
		`{"op": "type", "text": "hi"}`,
		`{"op": "key", "key": "Return"}`,
	} {
		_, err := env.tool.ExecuteResult(ctx, json.RawMessage(args))
		if err == nil || !strings.Contains(err.Error(), "unattended") {
			t.Errorf("ExecuteResult(%s) err = %v, want the unattended refusal", args, err)
		}
	}
	if len(env.ydotoolCalls) != 0 || env.kwinCalls != 0 {
		t.Errorf("unattended calls touched the desktop (%d ydotool, %d KWin)", len(env.ydotoolCalls), env.kwinCalls)
	}
}

// TestInputUnattendedRefusedViaGateMarker (F-A23W3-18): input must
// refuse a turn marked unattended ONLY by the permission gate's
// marker, like TestInputUnattendedRefused does for the tools
// marker. It FAILS on the old code.
func TestInputUnattendedRefusedViaGateMarker(t *testing.T) {
	env := newFakeInputEnv(t)
	ctx := permissions.WithUnattendedContext(context.Background())
	for _, args := range []string{
		`{"op": "move", "x": 1, "y": 1}`,
		`{"op": "click", "x": 1, "y": 1}`,
		`{"op": "type", "text": "hi"}`,
		`{"op": "key", "key": "Return"}`,
	} {
		_, err := env.tool.ExecuteResult(ctx, json.RawMessage(args))
		if err == nil || !strings.Contains(err.Error(), "unattended") {
			t.Errorf("ExecuteResult(%s) err = %v, want the unattended refusal", args, err)
		}
	}
	if len(env.ydotoolCalls) != 0 || env.kwinCalls != 0 {
		t.Errorf("gate-marked unattended calls touched the desktop (%d ydotool, %d KWin)", len(env.ydotoolCalls), env.kwinCalls)
	}
}

func TestInputTypeSensitiveRefused(t *testing.T) {
	env := newFakeInputEnv(t)
	env.tree = sensitiveInputTreeJSON()
	for _, args := range []string{
		`{"op": "type", "text": "secret"}`,
		`{"op": "key", "key": "Return"}`,
	} {
		err := env.callErr(t, args)
		if !strings.Contains(err.Error(), "password manager") {
			t.Errorf("ExecuteResult(%s) err = %v, want the password-manager refusal (category, never the title)", args, err)
		}
		env.advance()
	}
	if len(env.ydotoolCalls) != 0 {
		t.Errorf("got %d ydotool calls, want 0 (refused before typing)", len(env.ydotoolCalls))
	}
}

func TestInputTypeSensitiveXdotoolFallback(t *testing.T) {
	env := newFakeInputEnv(t)
	env.treeErr = true // no AT-SPI tree -> xdotool titles
	env.xdotoolTitles = []string{"KeePassXC — passwords.kdbx", "Firefox"}
	err := env.callErr(t, `{"op": "type", "text": "secret"}`)
	if !strings.Contains(err.Error(), "password manager") {
		t.Errorf("err = %v, want the password-manager refusal via the xdotool fallback", err)
	}
}

func TestInputTypeSensitiveFailClosedWhenUnenumerable(t *testing.T) {
	env := newFakeInputEnv(t)
	env.treeErr = true
	env.xdotoolErr = true // nothing can enumerate the windows
	err := env.callErr(t, `{"op": "type", "text": "secret"}`)
	if !strings.Contains(err.Error(), "will not type or press keys blind") {
		t.Errorf("err = %v, want the fail-closed blind-typing refusal", err)
	}
	if len(env.ydotoolCalls) != 0 {
		t.Errorf("got %d ydotool calls, want 0", len(env.ydotoolCalls))
	}
}

func TestInputClickSensitivePreciseBox(t *testing.T) {
	resetGroundSnapshots()
	registerFreshGrounding()
	env := newFakeInputEnv(t)
	env.tree = sensitiveInputTreeJSON()
	// KWin places the KeePassXC window 29 px lower than AT-SPI
	// reports (the M2 F2 shift): the corrected box is
	// (100,129)-(900,729). A click at (500,110) is inside the
	// RAW box but outside the corrected one — allowed only
	// because the correction is applied; a click at (500,200)
	// is inside the corrected box — refused.
	env.kwinWindows = []kwinWindow{
		{Class: "keepassxc", FG: [4]int{100, 129, 800, 600}, CG: [4]int{100, 129, 800, 600}},
	}
	res := env.call(t, `{"op": "click", "x": 500, "y": 110}`)
	if s := parseInputSummary(t, res); !s.Reached {
		t.Errorf("click outside the corrected sensitive box: summary = %+v, want reached", s)
	}
	env.advance()
	err := env.callErr(t, `{"op": "click", "x": 500, "y": 200}`)
	if !strings.Contains(err.Error(), "inside a password manager") {
		t.Errorf("click inside the sensitive box err = %v, want the inside-a-sensitive-window refusal", err)
	}
	// Exactly one click call (the allowed one): the refused click
	// never moved the cursor towards the window either.
	if got := len(env.clickCalls()); got != 1 {
		t.Errorf("got %d click calls, want 1", got)
	}
}

func TestInputClickSensitiveUncorrectedRefused(t *testing.T) {
	resetGroundSnapshots()
	registerFreshGrounding()
	env := newFakeInputEnv(t)
	env.tree = sensitiveInputTreeJSON()
	env.oracleErr = true // no KWin -> boxes uncorrected -> coarse refusal
	err := env.callErr(t, `{"op": "click", "x": 1500, "y": 900}`)
	if !strings.Contains(err.Error(), "will not click while it is visible") {
		t.Errorf("err = %v, want the coarse sensitive-window click refusal", err)
	}
}

// TestInputTypeSensitiveContentRefused (F-M2R-14): a login page
// whose title is innocuous ("Welcome") but whose body holds a
// password field must refuse type/key — and the refusal must
// name the category only, never the field name or the title.
func TestInputTypeSensitiveContentRefused(t *testing.T) {
	env := newFakeInputEnv(t)
	env.tree = contentSensitiveInputTreeJSON()
	for _, args := range []string{
		`{"op": "type", "text": "secret"}`,
		`{"op": "key", "key": "Return"}`,
	} {
		err := env.callErr(t, args)
		if !strings.Contains(err.Error(), "sign-in form") {
			t.Errorf("ExecuteResult(%s) err = %v, want the sign-in-form refusal", args, err)
		}
		if strings.Contains(err.Error(), "Password") || strings.Contains(err.Error(), "Welcome") {
			t.Errorf("refusal %q names element content or the title — refusals name the category only", err.Error())
		}
		env.advance()
	}
	if len(env.ydotoolCalls) != 0 {
		t.Errorf("got %d ydotool calls, want 0 (refused before typing)", len(env.ydotoolCalls))
	}
}

// TestInputTypeSensitiveContentOTPRefused (F-M2R-14): the same
// innocuous title, but a one-time-code field instead of a
// password field, must refuse with the OTP category.
func TestInputTypeSensitiveContentOTPRefused(t *testing.T) {
	env := newFakeInputEnv(t)
	env.tree = contentOTPInputTreeJSON()
	err := env.callErr(t, `{"op": "type", "text": "123456"}`)
	if !strings.Contains(err.Error(), "one-time-code / second-factor dialog") {
		t.Errorf("err = %v, want the one-time-code refusal", err)
	}
	if len(env.ydotoolCalls) != 0 {
		t.Errorf("got %d ydotool calls, want 0", len(env.ydotoolCalls))
	}
}

// TestInputTypeSensitiveContentBenignSignInAllowed (F-M2R-14):
// a sign-in page in words only — "sign in" heading, username
// field, sign-in button, "Forgot password?" link — with NO
// password or OTP field must stay typeable. This is the false
// positive the narrow signal must not introduce.
func TestInputTypeSensitiveContentBenignSignInAllowed(t *testing.T) {
	env := newFakeInputEnv(t)
	env.tree = contentBenignSignInTreeJSON()
	res := env.call(t, `{"op": "type", "text": "hello"}`)
	if s := parseInputSummary(t, res); s.CharsTyped != 5 {
		t.Errorf("chars_typed = %d, want 5 (a sign-in page with no password/OTP field must stay typeable)", s.CharsTyped)
	}
	if got := len(env.ydotoolCalls); got != 1 {
		t.Errorf("got %d ydotool calls, want 1", got)
	}
}

// TestInputTypeSensitiveContentHiddenFieldAllowed (F-M2R-14): a
// password field that is not showing (a closed popup's node
// lingering in the tree) must not refuse typing.
func TestInputTypeSensitiveContentHiddenFieldAllowed(t *testing.T) {
	env := newFakeInputEnv(t)
	env.tree = contentHiddenPasswordTreeJSON()
	res := env.call(t, `{"op": "type", "text": "hello"}`)
	if s := parseInputSummary(t, res); s.CharsTyped != 5 {
		t.Errorf("chars_typed = %d, want 5 (a hidden password field must not refuse)", s.CharsTyped)
	}
}

// TestInputClickSensitiveContentPreciseBox (F-M2R-14): the
// content signal follows the same precise-box rule as titles.
// KWin places the Firefox window 29 px lower than AT-SPI
// reports (the M2 F2 shift): the corrected box is
// (0,29)-(1920,1109). A click at (500,10) is inside the RAW
// box but outside the corrected one — allowed only because the
// correction is applied; a click at (500,200) is inside the
// corrected box — refused.
func TestInputClickSensitiveContentPreciseBox(t *testing.T) {
	resetGroundSnapshots()
	registerFreshGrounding()
	env := newFakeInputEnv(t)
	env.tree = contentSensitiveInputTreeJSON()
	env.kwinWindows = []kwinWindow{
		{Class: "firefox", FG: [4]int{0, 29, 1920, 1080}, CG: [4]int{0, 29, 1920, 1080}},
	}
	res := env.call(t, `{"op": "click", "x": 500, "y": 10}`)
	if s := parseInputSummary(t, res); !s.Reached {
		t.Errorf("click outside the corrected content-sensitive box: summary = %+v, want reached", s)
	}
	env.advance()
	err := env.callErr(t, `{"op": "click", "x": 500, "y": 200}`)
	if !strings.Contains(err.Error(), "inside a sign-in form") {
		t.Errorf("click inside the content-sensitive box err = %v, want the inside-a-sign-in-form refusal", err)
	}
	// Exactly one click call (the allowed one): the refused click
	// never moved the cursor towards the window either.
	if got := len(env.clickCalls()); got != 1 {
		t.Errorf("got %d click calls, want 1", got)
	}
}

// TestInputClickSensitiveContentUncorrectedRefused (F-M2R-14):
// with no KWin correction the content-sensitive window's box
// cannot be trusted, so the click refuses outright.
func TestInputClickSensitiveContentUncorrectedRefused(t *testing.T) {
	resetGroundSnapshots()
	registerFreshGrounding()
	env := newFakeInputEnv(t)
	env.tree = contentSensitiveInputTreeJSON()
	env.oracleErr = true // no KWin -> boxes uncorrected -> coarse refusal
	err := env.callErr(t, `{"op": "click", "x": 1500, "y": 900}`)
	if !strings.Contains(err.Error(), "will not click while it is visible") {
		t.Errorf("err = %v, want the coarse sensitive-window click refusal", err)
	}
	if !strings.Contains(err.Error(), "sign-in form") {
		t.Errorf("err = %v, want the sign-in-form category", err)
	}
}

// TestInputClickSensitiveContentAndTitleUnion (F-M2R-14): with
// BOTH signals — a title-sensitive KeePassXC window plus a
// content-sensitive Firefox login page — a click inside the
// Firefox box but outside the KeePassXC box must still refuse.
// The old title-only check would have allowed it.
func TestInputClickSensitiveContentAndTitleUnion(t *testing.T) {
	resetGroundSnapshots()
	registerFreshGrounding()
	env := newFakeInputEnv(t)
	env.tree = contentAndTitleTreeJSON()
	env.kwinWindows = []kwinWindow{
		{Class: "keepassxc", FG: [4]int{100, 129, 800, 600}, CG: [4]int{100, 129, 800, 600}},
		{Class: "firefox", FG: [4]int{0, 29, 1920, 1080}, CG: [4]int{0, 29, 1920, 1080}},
	}
	err := env.callErr(t, `{"op": "click", "x": 1500, "y": 900}`)
	if !strings.Contains(err.Error(), "inside a sign-in form") {
		t.Errorf("click inside the content-sensitive box err = %v, want the inside-a-sign-in-form refusal", err)
	}
	if got := len(env.clickCalls()); got != 0 {
		t.Errorf("got %d click calls, want 0", got)
	}
}

func TestInputMoveAllowedUnderSensitiveWindow(t *testing.T) {
	env := newFakeInputEnv(t)
	env.tree = sensitiveInputTreeJSON()
	// Moving the cursor cannot activate, type, close or submit
	// anything: position is not an effect, so move is allowed
	// where type/key/click are refused.
	res := env.call(t, `{"op": "move", "x": 960, "y": 540}`)
	if s := parseInputSummary(t, res); !s.Reached {
		t.Errorf("move under a sensitive window: summary = %+v, want reached", s)
	}
}

func TestInputDaemonNotRunning(t *testing.T) {
	env := newFakeInputEnv(t)
	if err := os.Remove(env.socketPath); err != nil {
		t.Fatalf("remove fake socket: %v", err)
	}
	err := env.callErr(t, `{"op": "move", "x": 960, "y": 540}`)
	if !strings.Contains(err.Error(), "daemon is not running") {
		t.Errorf("err = %v, want the daemon-not-running error", err)
	}
	if !strings.Contains(err.Error(), env.socketPath) {
		t.Errorf("err = %v, want the socket path in the message", err)
	}
	if len(env.ydotoolCalls) != 0 || env.kwinCalls != 0 {
		t.Errorf("daemon-down call touched the desktop (%d ydotool, %d KWin)", len(env.ydotoolCalls), env.kwinCalls)
	}
}

func TestInputScreenLockedRefused(t *testing.T) {
	env := newFakeInputEnv(t)
	env.locked = true
	err := env.callErr(t, `{"op": "move", "x": 960, "y": 540}`)
	if !strings.Contains(err.Error(), "screen is locked") {
		t.Errorf("err = %v, want the screen-locked refusal", err)
	}
	if len(env.ydotoolCalls) != 0 {
		t.Errorf("got %d ydotool calls, want 0", len(env.ydotoolCalls))
	}
}

func TestInputRateLimit(t *testing.T) {
	env := newFakeInputEnv(t)
	env.call(t, `{"op": "move", "x": 200, "y": 200}`)
	// Same fake second: the min-interval limit denies.
	err := env.callErr(t, `{"op": "move", "x": 300, "y": 300}`)
	if !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("second call err = %v, want the rate-limit denial", err)
	}
	env.advance()
	res := env.call(t, `{"op": "move", "x": 300, "y": 300}`)
	if s := parseInputSummary(t, res); !s.Reached {
		t.Errorf("after advancing the clock: summary = %+v, want reached", s)
	}
}

func TestInputKWinHelperFile(t *testing.T) {
	tool := newInputTool()
	home := t.TempDir()
	tool.homeDir = func() (string, error) { return home, nil }
	path, err := tool.defaultWriteKWinHelper()
	if err != nil {
		t.Fatalf("defaultWriteKWinHelper = %v", err)
	}
	if path != filepath.Join(home, ".orion", "input-kwin-helper.py") {
		t.Errorf("helper path = %q, want ~/.orion/input-kwin-helper.py", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read helper: %v", err)
	}
	if string(raw) != inputKWinHelperSource {
		t.Errorf("helper file contents differ from the embedded source")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat helper: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("helper mode = %o, want 0600", info.Mode().Perm())
	}
	// Identical existing file is left untouched.
	if path2, err := tool.defaultWriteKWinHelper(); err != nil || path2 != path {
		t.Errorf("second write = (%q, %v), want (%q, nil)", path2, err, path)
	}
}

func TestInputKWinHelperMarkerEntropy(t *testing.T) {
	// F-A23W2-11: the oracle marker was 16-bit random only —
	// two calls from the same pid collided after ~100 calls,
	// and a stale journal line could return the wrong cursor
	// as closed-loop truth. The marker must carry 64-bit+
	// secrets randomness plus a timestamp, with the nonce
	// echoed in the payload and checked on receipt.
	if strings.Contains(inputKWinHelperSource, "randrange(0x10000)") {
		t.Errorf("input KWin helper still uses the 16-bit marker")
	}
	for _, want := range []string{
		"secrets.token_hex(8)", // 64-bit randomness
		"time.time_ns()",       // timestamp/nonce
		`{nonce: "%s"`,         // nonce echoed in the payload
		"nonce mismatch",       // stale line refused, not used
		"def new_marker",
	} {
		if !strings.Contains(inputKWinHelperSource, want) {
			t.Errorf("input KWin helper source lacks %q", want)
		}
	}
}

func TestInputFrameworkIntegration(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	tool, ok := reg.Get("input")
	if !ok {
		t.Fatalf("DefaultRegistry has no input tool")
	}
	def := tool.Definition()
	if def.Name != "input" {
		t.Errorf("definition name = %q, want input", def.Name)
	}
	props, ok := def.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("definition has no properties map")
	}
	op, ok := props["op"].(map[string]any)
	if !ok {
		t.Fatalf("definition has no op property")
	}
	enums, _ := op["enum"].([]string)
	if fmt.Sprint(enums) != fmt.Sprint([]string{"move", "click", "type", "key"}) {
		t.Errorf("op enum = %v, want [move click type key]", enums)
	}
	// Execute (text-only) delegates to ExecuteResult: the same
	// move returns the same JSON text through both entry points.
	env := newFakeInputEnv(t)
	textOut, err := env.tool.Execute(context.Background(), json.RawMessage(`{"op": "move", "x": 200, "y": 200}`))
	if err != nil {
		t.Fatalf("Execute(move) = %v, want nil", err)
	}
	env.advance()
	resOut, err := env.tool.ExecuteResult(context.Background(), json.RawMessage(`{"op": "move", "x": 300, "y": 300}`))
	if err != nil {
		t.Fatalf("ExecuteResult(move) = %v, want nil", err)
	}
	if !strings.Contains(textOut, `"op":"move"`) || !strings.Contains(resOut.Text, `"op":"move"`) {
		t.Errorf("Execute = %q, ExecuteResult = %q, want both to carry the move summary", textOut, resOut.Text)
	}
}

// TestInputScreenCacheInvalidation (F-A23W2-17): the cached
// screen size must stop being trusted once it is old, or a
// resolution change leaves validatePoint checking against a dead
// screen. The fake oracle starts at 800x600, then the screen
// grows to 1920x1080 (monitor plugged in / resolution raised).
func TestInputScreenCacheInvalidation(t *testing.T) {
	env := newFakeInputEnv(t)
	env.screen = inputScreen{X: 0, Y: 0, W: 800, H: 600}
	env.cursor = cursorPos{X: 100, Y: 100}
	env.call(t, `{"op": "move", "x": 400, "y": 300}`)
	if env.tool.screenW != 800 || env.tool.screenH != 600 {
		t.Fatalf("cached screen = %dx%d, want 800x600", env.tool.screenW, env.tool.screenH)
	}

	// The screen grows to 1920x1080. While the cache is still
	// fresh (seconds old) the old bound is enforced fail-fast:
	// (1500, 500) is outside the cached 800x600 screen.
	env.screen = inputScreen{X: 0, Y: 0, W: 1920, H: 1080}
	env.advance()
	err := env.callErr(t, `{"op": "move", "x": 1500, "y": 500}`)
	if !strings.Contains(err.Error(), "outside the 800x600 screen") {
		t.Errorf("fresh-cache move err = %v, want the 800x600 bound", err)
	}

	// Past the TTL the stale bound is no longer enforced: the
	// same target is valid on the real 1920x1080 screen, and
	// the move lands (doMove re-validates against the fresh
	// oracle reading before any ydotool call).
	env.clock = env.clock.Add(inputScreenCacheTTL + time.Second)
	env.advance()
	res := env.call(t, `{"op": "move", "x": 1500, "y": 500}`)
	if s := parseInputSummary(t, res); !s.Reached {
		t.Errorf("stale-cache move summary = %+v, want reached on the grown screen", s)
	}
	// The oracle read refreshed the cache to the new size: a
	// target outside 1920x1080 is refused again, against the
	// fresh bound.
	if env.tool.screenW != 1920 || env.tool.screenH != 1080 {
		t.Errorf("cached screen after growth = %dx%d, want 1920x1080", env.tool.screenW, env.tool.screenH)
	}
	env.advance()
	err = env.callErr(t, `{"op": "move", "x": 2000, "y": 500}`)
	if !strings.Contains(err.Error(), "outside the 1920x1080 screen") {
		t.Errorf("fresh-cache move err = %v, want the 1920x1080 bound", err)
	}

	// Shrink: the cache says 1920x1080 but the screen is back to
	// 800x600 and the cache is stale — the arg-time check passes,
	// but doMove's post-oracle re-validation refuses before any
	// ydotool call, so the cursor never moves toward a dead target.
	env.screen = inputScreen{X: 0, Y: 0, W: 800, H: 600}
	env.clock = env.clock.Add(inputScreenCacheTTL + time.Second)
	env.advance()
	before := len(env.mousemoveCalls())
	err = env.callErr(t, `{"op": "move", "x": 1500, "y": 500}`)
	if !strings.Contains(err.Error(), "outside the 800x600 screen") {
		t.Errorf("shrink move err = %v, want the 800x600 bound from the fresh oracle", err)
	}
	if len(env.mousemoveCalls()) != before {
		t.Errorf("shrink move issued %d mousemove call(s), want 0", len(env.mousemoveCalls())-before)
	}
}

// TestInputFailedCallsDoNotEatRateLimitSlots (F-A23W2-18): a call
// that fails because the screen is locked or the daemon is down
// performed no input, so it must not consume a per-turn or
// per-hour slot — otherwise a locked screen or a down daemon
// burns the whole quota on failures.
func TestInputFailedCallsDoNotEatRateLimitSlots(t *testing.T) {
	env := newFakeInputEnv(t)
	// Screen locked: 20 failures, zero slots consumed.
	env.locked = true
	for i := 0; i < 20; i++ {
		err := env.callErr(t, `{"op": "move", "x": 200, "y": 200}`)
		if !strings.Contains(err.Error(), "screen is locked") {
			t.Fatalf("locked call #%d err = %v, want the locked-screen error", i+1, err)
		}
	}
	if env.tool.budget.TurnCalls() != 0 || env.tool.budget.HourCalls() != 0 {
		t.Errorf("after 20 locked failures: turnCalls = %d, hourCalls = %d, want 0, 0", env.tool.budget.TurnCalls(), env.tool.budget.HourCalls())
	}
	// Daemon down: same — the socket is gone, the call fails
	// before the rate-limit check.
	env.locked = false
	sock := env.socketPath
	env.socketPath = filepath.Join(env.home, "no-such-socket")
	for i := 0; i < 20; i++ {
		err := env.callErr(t, `{"op": "move", "x": 200, "y": 200}`)
		if !strings.Contains(err.Error(), "daemon is not running") {
			t.Fatalf("daemon-down call #%d err = %v, want the daemon-down error", i+1, err)
		}
	}
	if env.tool.budget.TurnCalls() != 0 || env.tool.budget.HourCalls() != 0 {
		t.Errorf("after 20 daemon-down failures: turnCalls = %d, hourCalls = %d, want 0, 0", env.tool.budget.TurnCalls(), env.tool.budget.HourCalls())
	}
	// The quota is still whole: 15 moves succeed, the 16th is
	// rate-limited — the 40 failures above bought nothing.
	env.socketPath = sock
	for i := 0; i < inputMaxPerTurn; i++ {
		env.advance()
		env.call(t, fmt.Sprintf(`{"op": "move", "x": %d, "y": 200}`, 200+i))
	}
	env.advance()
	err := env.callErr(t, `{"op": "move", "x": 300, "y": 300}`)
	if !strings.Contains(err.Error(), "at most 15 input calls per turn") {
		t.Errorf("16th move err = %v, want the per-turn limit", err)
	}
}

// TestInputSensitiveRefusalsDoNotEatRateLimitSlots (F-A23W2-18,
// follow-up): a call charged by the rate check and then REFUSED
// by a sensitive-window check performed no input, so it must not
// consume a per-turn or per-hour slot — otherwise a password
// manager on screen burns the whole quota on refusals, and the
// turn has no input left once the window closes.
func TestInputSensitiveRefusalsDoNotEatRateLimitSlots(t *testing.T) {
	resetGroundSnapshots()
	registerFreshGrounding()
	env := newFakeInputEnv(t)
	env.tree = sensitiveInputTreeJSON()
	// 10 type refusals + 10 click refusals: every one charges
	// first (the rate check runs before the sensitive check)
	// and is then denied — no ydotool call, no slot consumed.
	for i := 0; i < 10; i++ {
		err := env.callErr(t, `{"op": "type", "text": "secret"}`)
		if !strings.Contains(err.Error(), "password manager") {
			t.Fatalf("type refusal #%d err = %v, want the password-manager refusal", i+1, err)
		}
		env.advance()
		err = env.callErr(t, `{"op": "click", "x": 500, "y": 200}`)
		if !strings.Contains(err.Error(), "will not click while it is visible") {
			t.Fatalf("click refusal #%d err = %v, want the sensitive-window click refusal", i+1, err)
		}
		// No advance after the last refusal: the min-interval
		// check below needs a call in the same fake second.
		if i < 9 {
			env.advance()
		}
	}
	if len(env.ydotoolCalls) != 0 {
		t.Errorf("got %d ydotool calls, want 0 (refused before touching the desktop)", len(env.ydotoolCalls))
	}
	if got := env.tool.budget.TurnCalls(); got != 0 {
		t.Errorf("after 20 sensitive refusals: turnCalls = %d, want 0", got)
	}
	if got := env.tool.budget.HourCalls(); got != 0 {
		t.Errorf("after 20 sensitive refusals: hourCalls = %d, want 0", got)
	}
	// The min-interval stamp is NOT refunded: a refusal still
	// counts as the last call, so an immediate retry is
	// throttled rather than re-running the sensitive check.
	err := env.callErr(t, `{"op": "type", "text": "secret"}`)
	if !strings.Contains(err.Error(), "at most one input call per second") {
		t.Errorf("immediate retry err = %v, want the min-interval denial", err)
	}
	// The quota is still whole: with the sensitive window gone,
	// the full 15 moves succeed and the 16th is rate-limited.
	env.tree = ""
	for i := 0; i < inputMaxPerTurn; i++ {
		env.advance()
		env.call(t, fmt.Sprintf(`{"op": "move", "x": %d, "y": 200}`, 200+i))
	}
	if got := env.tool.budget.TurnCalls(); got != inputMaxPerTurn {
		t.Errorf("after 15 moves: turnCalls = %d, want %d", got, inputMaxPerTurn)
	}
	env.advance()
	err = env.callErr(t, `{"op": "move", "x": 300, "y": 300}`)
	if !strings.Contains(err.Error(), "at most 15 input calls per turn") {
		t.Errorf("16th move err = %v, want the per-turn limit", err)
	}
}

// TestInputClickBySnapshotMovedWindow pins F-M2R-8 at the click:
// the snapshot binds element 1 to Firefox at (0,0,1920,1080),
// but the live tree now reports Firefox at (100,0,1920,1080) —
// the window moved after the grounding, so the click is refused
// before the cursor moves anywhere near the stale centre.
func TestInputClickBySnapshotMovedWindow(t *testing.T) {
	resetGroundSnapshots()
	groundSnapshots.register("g-moved", 0, time.Now(), []groundElement{
		{ID: 1, Centre: groundPoint{X: 960, Y: 403}},
	}, map[int]WindowGeometry{
		1: {App: "Firefox", X: 0, Y: 0, W: 1920, H: 1080},
	})
	env := newFakeInputEnv(t)
	env.tree = `{"windows": [
		{"title": "Sign in to GitHub · GitHub — Mozilla Firefox", "app": "Firefox", "role": "frame", "focused": true, "x": 100, "y": 0, "w": 1920, "h": 1080},
		{"title": "Kate — notes.txt", "app": "Kate", "role": "frame", "focused": false, "x": 0, "y": 0, "w": 800, "h": 600}],
	"elements": [],
	"focused_window": {"title": "Sign in to GitHub · GitHub — Mozilla Firefox", "app": "Firefox"},
	"walked_nodes": 140}`
	err := env.callErr(t, `{"op": "click", "snapshot_id": "g-moved", "element_id": 1}`)
	if !strings.Contains(err.Error(), "moved or resized") {
		t.Errorf("moved-window click err = %v, want the moved-or-resized refusal", err)
	}
	if len(env.ydotoolCalls) != 0 {
		t.Errorf("got %d ydotool calls, want 0 (refused before touching the desktop)", len(env.ydotoolCalls))
	}
}

// --- F-A23W2-1 (stale-centre click): the element must be
// re-read fresh right before the click. ---

// treeWithButtonJSON builds a canned AT-SPI tree whose only
// element is one "Sign in" button at (x, y, w, h) in the
// Firefox window, so a test can move the button between the
// grounding and the click.
func treeWithButtonJSON(x, y, w, h int) string {
	return fmt.Sprintf(`{"windows": [
		{"title": "Sign in to GitHub · GitHub — Mozilla Firefox", "app": "Firefox", "role": "frame", "focused": true, "x": 0, "y": 0, "w": 1920, "h": 1080}],
	"elements": [
		{"role": "button", "name": "Sign in", "x": %d, "y": %d, "w": %d, "h": %d, "win": 0}],
	"focused_window": {"title": "Sign in to GitHub · GitHub — Mozilla Firefox", "app": "Firefox"},
	"walked_nodes": 140}`, x, y, w, h)
}

// firefoxKWinWindow is the KWin window matching the Firefox
// window in treeWithButtonJSON: same class and size, so
// the correction is (dx, dy) — (0, 0) unless dy is given.
// (No title: the KWin helper no longer reports captions,
// F-M2R-15, and matching is class+size.)
func firefoxKWinWindow(dy int) []kwinWindow {
	return []kwinWindow{{Class: "firefox-esr", FG: [4]int{0, dy, 1920, 1080}, CG: [4]int{0, dy, 1920, 1080}}}
}

func registerSignInSnapshot() {
	groundSnapshots.register("g-test-1", 0, time.Now(), []groundElement{
		{ID: 1, Role: "button", Name: "Sign in", Box: groundBox{X: 910, Y: 378, W: 100, H: 50}, Centre: groundPoint{X: 960, Y: 403}},
	}, map[int]WindowGeometry{
		1: {App: "Firefox", X: 0, Y: 0, W: 1920, H: 1080},
	})
}

// registerFreshGrounding registers a just-taken grounding for
// tests that click by coordinates (F-A23W2-8): a coordinate
// click needs a grounding from the last 60 s.
func registerFreshGrounding() {
	groundSnapshots.register("g-fake-fresh", 0, time.Now(), []groundElement{
		{ID: 1, Role: "button", Name: "Sign in", Centre: groundPoint{X: 960, Y: 403}},
	}, nil)
}

func TestInputClickRefusesMovedElement(t *testing.T) {
	resetGroundSnapshots()
	registerSignInSnapshot()
	env := newFakeInputEnv(t)
	env.kwinWindows = firefoxKWinWindow(0)
	// The page scrolled after grounding: the same button is now
	// 197 px lower. The snapshot is still fresh (TTL/id pass),
	// so only a fresh re-read can catch this.
	env.tree = treeWithButtonJSON(910, 575, 100, 50)
	err := env.callErr(t, `{"op": "click", "snapshot_id": "g-test-1", "element_id": 1}`)
	if !strings.Contains(err.Error(), "moved") || !strings.Contains(err.Error(), "ground again") {
		t.Errorf("moved-element click err = %v, want the 'has moved, ground again' refusal", err)
	}
	if got := len(env.clickCalls()); got != 0 {
		t.Errorf("got %d click calls, want 0 (a moved element must never be clicked)", got)
	}
}

func TestInputClickRefusesVanishedElement(t *testing.T) {
	resetGroundSnapshots()
	registerSignInSnapshot()
	env := newFakeInputEnv(t)
	env.kwinWindows = firefoxKWinWindow(0)
	// The button is gone from the fresh tree (dialog closed).
	env.tree = benignInputTreeJSON()
	err := env.callErr(t, `{"op": "click", "snapshot_id": "g-test-1", "element_id": 1}`)
	if !strings.Contains(err.Error(), "no longer on screen") || !strings.Contains(err.Error(), "ground again") {
		t.Errorf("vanished-element click err = %v, want the 'no longer on screen, ground again' refusal", err)
	}
	if got := len(env.clickCalls()); got != 0 {
		t.Errorf("got %d click calls, want 0", got)
	}
}

func TestInputClickRefusesWhenElementCannotBeRechecked(t *testing.T) {
	resetGroundSnapshots()
	registerSignInSnapshot()
	env := newFakeInputEnv(t)
	// The accessibility tree is gone at click time: the element
	// cannot be re-checked, so the click is refused rather than
	// trusted on the stale centre (fail closed).
	env.treeErr = true
	env.xdotoolTitles = []string{"Sign in to GitHub · GitHub — Mozilla Firefox"}
	err := env.callErr(t, `{"op": "click", "snapshot_id": "g-test-1", "element_id": 1}`)
	if !strings.Contains(err.Error(), "cannot re-check") {
		t.Errorf("unverifiable-element click err = %v, want the fail-closed 'cannot re-check' refusal", err)
	}
	if got := len(env.clickCalls()); got != 0 {
		t.Errorf("got %d click calls, want 0", got)
	}
}

func TestInputClickProceedsWhenElementStillThere(t *testing.T) {
	resetGroundSnapshots()
	registerSignInSnapshot()
	env := newFakeInputEnv(t)
	env.kwinWindows = firefoxKWinWindow(0)
	// The fresh tree still shows the button at its grounded
	// centre: the click proceeds.
	env.tree = treeWithButtonJSON(910, 378, 100, 50)
	res := env.call(t, `{"op": "click", "snapshot_id": "g-test-1", "element_id": 1}`)
	if s := parseInputSummary(t, res); !s.Reached {
		t.Errorf("summary = %+v, want reached (element confirmed at its centre)", s)
	}
	if got := len(env.clickCalls()); got != 1 {
		t.Errorf("got %d click calls, want 1", got)
	}
}

func TestInputClickRecheckAppliesKWinCorrection(t *testing.T) {
	resetGroundSnapshots()
	registerSignInSnapshot()
	env := newFakeInputEnv(t)
	// KWin places the Firefox window 29 px lower than AT-SPI
	// reports (the M2 F2 shift): the raw tree button at y=349
	// (centre 374) corrects to the grounded centre (960, 403).
	// Without the correction the re-check would refuse every
	// click on a Wayland window.
	env.kwinWindows = firefoxKWinWindow(29)
	env.tree = treeWithButtonJSON(910, 349, 100, 50)
	res := env.call(t, `{"op": "click", "snapshot_id": "g-test-1", "element_id": 1}`)
	if s := parseInputSummary(t, res); !s.Reached {
		t.Errorf("summary = %+v, want reached (KWin-corrected centre matches)", s)
	}
	if got := len(env.clickCalls()); got != 1 {
		t.Errorf("got %d click calls, want 1", got)
	}
}

// --- F-A23W2-8 (coordinate click bypass): a coordinate click
// needs a recent grounding, like a snapshot click does. ---

func TestInputCoordClickRefusedWithoutGrounding(t *testing.T) {
	env := newFakeInputEnv(t)
	resetGroundSnapshots() // the fake env's fresh grounding is gone
	err := env.callErr(t, `{"op": "click", "x": 500, "y": 300}`)
	if !strings.Contains(err.Error(), "ground first") {
		t.Errorf("coord click with no grounding err = %v, want the 'ground first' refusal", err)
	}
	if got := len(env.ydotoolCalls); got != 0 {
		t.Errorf("got %d ydotool calls, want 0 (refused before touching the desktop)", got)
	}
}

func TestInputCoordClickRefusedWhenGroundingStale(t *testing.T) {
	env := newFakeInputEnv(t)
	resetGroundSnapshots()
	groundSnapshots.register("g-old", 0, time.Now().Add(-61*time.Second), []groundElement{
		{ID: 1, Role: "button", Name: "Sign in", Centre: groundPoint{X: 960, Y: 403}},
	}, nil)
	err := env.callErr(t, `{"op": "click", "x": 500, "y": 300}`)
	if !strings.Contains(err.Error(), "ground first") {
		t.Errorf("coord click with a stale grounding err = %v, want the 'ground first' refusal", err)
	}
	if got := len(env.ydotoolCalls); got != 0 {
		t.Errorf("got %d ydotool calls, want 0", got)
	}
}

// --- F-A23W2-9 (no post-click verification): a click must be
// oracle-verified after delivery, and a timeout mid-click must
// say the click may have landed instead of failing (a failure
// makes the model retry into a double-click). ---

func TestInputClickVerifiedAfterSuccess(t *testing.T) {
	resetGroundSnapshots()
	registerFreshGrounding()
	env := newFakeInputEnv(t)
	res := env.call(t, `{"op": "click", "x": 500, "y": 300}`)
	s := parseInputSummary(t, res)
	if s.PostClickCursor == nil || *s.PostClickCursor != (cursorPos{X: 500, Y: 300}) {
		t.Errorf("post_click_cursor = %+v, want {500 300} (oracle re-read after the click)", s.PostClickCursor)
	}
	if !s.ClickVerified {
		t.Errorf("click_verified = false, want true (the oracle re-read agreed the cursor is on target)")
	}
}

func TestInputClickTimeoutReportsMayHaveLanded(t *testing.T) {
	resetGroundSnapshots()
	registerFreshGrounding()
	env := newFakeInputEnv(t)
	orig := env.tool.ydotool
	env.tool.ydotool = func(ctx context.Context, stdin string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "click" {
			// Record the call, then report the timeout: the
			// click was issued once and timed out mid-flight.
			_, _ = orig(ctx, stdin, args...)
			return "", fmt.Errorf("ydotool click timed out: %w", context.DeadlineExceeded)
		}
		return orig(ctx, stdin, args...)
	}
	// ExecuteResult must NOT return an error here — an error
	// would make the model retry and double-click.
	res, err := env.tool.ExecuteResult(context.Background(), json.RawMessage(`{"op": "click", "x": 500, "y": 300}`))
	if err != nil {
		t.Fatalf("timed-out click returned error %v, want a result saying the click may have landed", err)
	}
	s := parseInputSummary(t, res)
	if !s.ClickMayHaveLanded {
		t.Errorf("click_may_have_landed = false, want true")
	}
	if !strings.Contains(s.Warning, "may already have landed") || !strings.Contains(s.Warning, "before retrying") {
		t.Errorf("warning = %q, want 'may already have landed' + 'before retrying'", s.Warning)
	}
	if got := len(env.clickCalls()); got != 1 {
		t.Errorf("got %d click calls, want 1 (no retry inside the tool)", got)
	}
}

func TestInputClickDeliveredButUnverifiedWhenOracleFails(t *testing.T) {
	resetGroundSnapshots()
	registerFreshGrounding()
	env := newFakeInputEnv(t)
	orig := env.tool.ydotool
	env.tool.ydotool = func(ctx context.Context, stdin string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "click" {
			env.oracleErr = true // the post-click oracle re-read fails
		}
		return orig(ctx, stdin, args...)
	}
	res := env.call(t, `{"op": "click", "x": 500, "y": 300}`)
	s := parseInputSummary(t, res)
	if !s.Reached {
		t.Errorf("summary = %+v, want reached (the click was delivered)", s)
	}
	if s.ClickVerified {
		t.Errorf("click_verified = true with a failed oracle re-read, want false")
	}
	if s.PostClickCursor != nil {
		t.Errorf("post_click_cursor = %+v, want nil (no oracle reading)", s.PostClickCursor)
	}
	if !strings.Contains(s.Warning, "verify") {
		t.Errorf("warning = %q, want a verify-before-retrying note", s.Warning)
	}
	if got := len(env.clickCalls()); got != 1 {
		t.Errorf("got %d click calls, want 1", got)
	}
}

// --- F-A23W2-9 follow-up (cursor-only verification): a click
// must also be judged by the focused window. A button press
// that reaches an unfocused window focuses it, so when the
// target window never takes focus although the focus never
// moved, the click demonstrably never reached the window —
// even though the cursor sits on target. ---

// kwinWindowsClickTarget builds a fake KWin list for the focus
// tests: the Firefox window (fullscreen — the click target
// (960, 403) is inside it) and a small Kate window top-left
// (the target is outside it), with the active flag on the named
// class.
func kwinWindowsClickTarget(activeClass string) []kwinWindow {
	wins := []kwinWindow{
		{Class: "firefox-esr", FG: [4]int{0, 0, 1920, 1080}, CG: [4]int{0, 0, 1920, 1080}},
		{Class: "kate", FG: [4]int{0, 0, 800, 600}, CG: [4]int{0, 0, 800, 600}},
	}
	for i := range wins {
		wins[i].Active = wins[i].Class == activeClass
	}
	return wins
}

func TestInputClickUnverifiedWhenTargetWindowNeverTakesFocus(t *testing.T) {
	resetGroundSnapshots()
	registerFreshGrounding()
	env := newFakeInputEnv(t)
	// Kate is focused; the click target (960, 403) is inside
	// the unfocused Firefox window only. The simulated desktop
	// does not react to the click (no kwinWindowsAfterClick):
	// the focus never moves off Kate.
	env.kwinWindows = kwinWindowsClickTarget("kate")
	res := env.call(t, `{"op": "click", "x": 960, "y": 403}`)
	s := parseInputSummary(t, res)
	if got := len(env.clickCalls()); got != 1 {
		t.Errorf("got %d click calls, want 1 (the click was sent)", got)
	}
	if s.PostClickCursor == nil || *s.PostClickCursor != (cursorPos{X: 960, Y: 403}) {
		t.Errorf("post_click_cursor = %+v, want {960 403} (the cursor IS on target — only the focus demotes this)", s.PostClickCursor)
	}
	if s.ClickVerified {
		t.Errorf("click_verified = true with the focus never leaving Kate, want false (the click never reached the Firefox window)")
	}
	if !strings.Contains(s.Warning, "never took focus") {
		t.Errorf("warning = %q, want the 'target window never took focus' note", s.Warning)
	}
}

func TestInputClickVerifiedWhenTargetWindowTakesFocus(t *testing.T) {
	resetGroundSnapshots()
	registerFreshGrounding()
	env := newFakeInputEnv(t)
	env.kwinWindows = kwinWindowsClickTarget("kate")
	// The click lands: Firefox takes focus.
	env.kwinWindowsAfterClick = kwinWindowsClickTarget("firefox-esr")
	res := env.call(t, `{"op": "click", "x": 960, "y": 403}`)
	s := parseInputSummary(t, res)
	if !s.ClickVerified {
		t.Errorf("click_verified = false, want true (the target window took focus: the click landed)")
	}
	if s.Warning != "" {
		t.Errorf("warning = %q, want empty", s.Warning)
	}
	if got := len(env.clickCalls()); got != 1 {
		t.Errorf("got %d click calls, want 1", got)
	}
}

func TestInputClickVerifiedWhenTargetWindowAlreadyFocused(t *testing.T) {
	resetGroundSnapshots()
	registerFreshGrounding()
	env := newFakeInputEnv(t)
	// Firefox is already focused and the click changes nothing
	// visible (no kwinWindowsAfterClick): this is the click
	// that legitimately changes nothing, and it must NOT
	// false-negative — the cursor verdict stands.
	env.kwinWindows = kwinWindowsClickTarget("firefox-esr")
	res := env.call(t, `{"op": "click", "x": 960, "y": 403}`)
	s := parseInputSummary(t, res)
	if !s.ClickVerified {
		t.Errorf("click_verified = false, want true (already-focused window: the focus check has nothing to demote on)")
	}
	if got := len(env.clickCalls()); got != 1 {
		t.Errorf("got %d click calls, want 1", got)
	}
}

func TestInputSnapshotClickUnverifiedWhenTargetWindowNeverTakesFocus(t *testing.T) {
	resetGroundSnapshots()
	registerSignInSnapshot()
	env := newFakeInputEnv(t)
	env.kwinWindows = kwinWindowsClickTarget("kate")
	// The button is still at its grounded centre, so the
	// pre-click re-validation passes and the click is sent —
	// but the focus never leaves Kate: the click never reached
	// the Firefox window holding the button.
	env.tree = treeWithButtonJSON(910, 378, 100, 50)
	res := env.call(t, `{"op": "click", "snapshot_id": "g-test-1", "element_id": 1}`)
	s := parseInputSummary(t, res)
	if got := len(env.clickCalls()); got != 1 {
		t.Errorf("got %d click calls, want 1 (the click was sent)", got)
	}
	if s.ClickVerified {
		t.Errorf("click_verified = true with the focus never leaving Kate, want false (the click never reached the element's window)")
	}
	if !strings.Contains(s.Warning, "never took focus") {
		t.Errorf("warning = %q, want the 'target window never took focus' note", s.Warning)
	}
}

func TestInputSnapshotClickVerifiedWhenTargetWindowTakesFocus(t *testing.T) {
	resetGroundSnapshots()
	registerSignInSnapshot()
	env := newFakeInputEnv(t)
	env.kwinWindows = kwinWindowsClickTarget("kate")
	env.kwinWindowsAfterClick = kwinWindowsClickTarget("firefox-esr")
	env.tree = treeWithButtonJSON(910, 378, 100, 50)
	res := env.call(t, `{"op": "click", "snapshot_id": "g-test-1", "element_id": 1}`)
	s := parseInputSummary(t, res)
	if !s.ClickVerified {
		t.Errorf("click_verified = false, want true (the element's window took focus: the click landed)")
	}
	if got := len(env.clickCalls()); got != 1 {
		t.Errorf("got %d click calls, want 1", got)
	}
}

// --- F-A23W2-9 element re-check (follow-up round): a click
// that reaches the already-focused window but lands on the
// wrong control must NOT verify. The press gives focus to the
// control it lands on, so the post-click focused control is
// the signal — the target sitting there unchanged while
// another control at the click point takes focus proves the
// press hit the other control. ---

// focusTreeElement is one element in a canned tree for the
// element re-check tests.
type focusTreeElement struct {
	role, name string
	x, y, w, h int
}

var (
	// The snapshot target: the "Sign in" button, centre (960, 403).
	focusSignInButton = focusTreeElement{"button", "Sign in", 910, 378, 100, 50}
	// A neighbouring control whose box covers the click point
	// without containing the button's box (or being contained
	// by it): the wrong control the press can land on.
	focusSearchEntry = focusTreeElement{"entry", "Search", 850, 390, 300, 26}
	// A control elsewhere on the page, holding focus before the
	// click in most cases.
	focusUsernameEntry = focusTreeElement{"entry", "Username", 784, 300, 352, 40}
	// A container holding the button: focus moving to it
	// instead of the button is not a miss.
	focusFormPanel = focusTreeElement{"panel", "Form", 900, 370, 120, 70}
)

// focusTreeJSON builds a canned AT-SPI tree (Firefox window at
// (0,0,1920,1080)) from the given elements; focused names the
// element holding focus ("" = none). Every element carries an
// explicit focused flag, so the fake tree exercises the same
// signal the real helper reports.
func focusTreeJSON(focused string, els ...focusTreeElement) string {
	var b strings.Builder
	b.WriteString(`{"windows": [
		{"title": "Sign in to GitHub · GitHub — Mozilla Firefox", "app": "Firefox", "role": "frame", "focused": true, "x": 0, "y": 0, "w": 1920, "h": 1080}],
	"elements": [`)
	for i, el := range els {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"role": %q, "name": %q, "x": %d, "y": %d, "w": %d, "h": %d, "win": 0, "showing": true, "focused": %v}`,
			el.role, el.name, el.x, el.y, el.w, el.h, el.name == focused)
	}
	b.WriteString(`],
	"focused_window": {"title": "Sign in to GitHub · GitHub — Mozilla Firefox", "app": "Firefox"},
	"walked_nodes": 140}`)
	return b.String()
}

// focusClickEnv is a fake env for the element re-check tests:
// Firefox already focused, the button at its grounded centre.
func focusClickEnv(t *testing.T) *fakeInputEnv {
	t.Helper()
	resetGroundSnapshots()
	registerSignInSnapshot()
	env := newFakeInputEnv(t)
	env.kwinWindows = firefoxKWinWindow(0)
	// firefoxKWinWindow leaves Active unset; the element
	// re-check only runs when the KWin active window is known
	// and unchanged, so the focused window must be marked.
	env.kwinWindows[0].Active = true
	return env
}

func TestInputSnapshotClickUnverifiedWhenWrongControlTakesFocus(t *testing.T) {
	env := focusClickEnv(t)
	// Before the click the Username entry holds focus; the
	// press lands on the Search entry instead of the Sign in
	// button — the Search entry takes focus while the button
	// sits there unchanged.
	env.tree = focusTreeJSON("Username", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	env.treeAfterClick = focusTreeJSON("Search", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	res := env.call(t, `{"op": "click", "snapshot_id": "g-test-1", "element_id": 1}`)
	s := parseInputSummary(t, res)
	if got := len(env.clickCalls()); got != 1 {
		t.Errorf("got %d click calls, want 1 (the click was sent)", got)
	}
	if s.PostClickCursor == nil || *s.PostClickCursor != (cursorPos{X: 960, Y: 403}) {
		t.Errorf("post_click_cursor = %+v, want {960 403} (the cursor IS on target — only the element check demotes this)", s.PostClickCursor)
	}
	if s.ClickVerified {
		t.Errorf("click_verified = true with another control at the click point taking focus, want false (the click landed on the wrong control)")
	}
	if !strings.Contains(s.Warning, "wrong control") {
		t.Errorf("warning = %q, want the 'wrong control' note", s.Warning)
	}
}

func TestInputSnapshotClickVerifiedWhenCorrectControlTakesFocus(t *testing.T) {
	env := focusClickEnv(t)
	// The press lands on the button and the button itself takes
	// focus: the click landed, and the element check must not
	// false-negative it.
	env.tree = focusTreeJSON("Username", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	env.treeAfterClick = focusTreeJSON("Sign in", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	res := env.call(t, `{"op": "click", "snapshot_id": "g-test-1", "element_id": 1}`)
	s := parseInputSummary(t, res)
	if !s.ClickVerified {
		t.Errorf("click_verified = false, want true (the target itself took focus: the click landed)")
	}
	if s.Warning != "" {
		t.Errorf("warning = %q, want empty", s.Warning)
	}
	if got := len(env.clickCalls()); got != 1 {
		t.Errorf("got %d click calls, want 1", got)
	}
}

func TestInputSnapshotClickVerifiedWhenFocusStaysOnOtherControl(t *testing.T) {
	env := focusClickEnv(t)
	// The press lands on a control that never takes focus, so
	// focus stays on the Username entry: a landed click that
	// legitimately changes nothing visible must still verify.
	env.tree = focusTreeJSON("Username", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	env.treeAfterClick = focusTreeJSON("Username", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	res := env.call(t, `{"op": "click", "snapshot_id": "g-test-1", "element_id": 1}`)
	s := parseInputSummary(t, res)
	if !s.ClickVerified {
		t.Errorf("click_verified = false, want true (focus staying put is no evidence of a miss)")
	}
	if got := len(env.clickCalls()); got != 1 {
		t.Errorf("got %d click calls, want 1", got)
	}
}

func TestInputSnapshotClickVerifiedWhenWrongControlAlreadyFocused(t *testing.T) {
	env := focusClickEnv(t)
	// The Search entry was ALREADY focused before the click
	// and still is: the focus did not move, so the press
	// landing on it is indistinguishable from a landed click
	// on a control that never takes focus — no demotion.
	env.tree = focusTreeJSON("Search", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	env.treeAfterClick = focusTreeJSON("Search", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	res := env.call(t, `{"op": "click", "snapshot_id": "g-test-1", "element_id": 1}`)
	s := parseInputSummary(t, res)
	if !s.ClickVerified {
		t.Errorf("click_verified = false, want true (an already-focused control is no signal)")
	}
	if got := len(env.clickCalls()); got != 1 {
		t.Errorf("got %d click calls, want 1", got)
	}
}

func TestInputSnapshotClickVerifiedWhenTargetVanishesAfterClick(t *testing.T) {
	env := focusClickEnv(t)
	// The button is gone after the click (its own effect — a
	// dialog closed, a page navigated) while the Search entry
	// took focus: a vanished target is ambiguous, never a
	// demotion.
	env.tree = focusTreeJSON("Username", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	env.treeAfterClick = focusTreeJSON("Search", focusSearchEntry, focusUsernameEntry)
	res := env.call(t, `{"op": "click", "snapshot_id": "g-test-1", "element_id": 1}`)
	s := parseInputSummary(t, res)
	if !s.ClickVerified {
		t.Errorf("click_verified = false, want true (a vanished target may be the click's own effect)")
	}
	if got := len(env.clickCalls()); got != 1 {
		t.Errorf("got %d click calls, want 1", got)
	}
}

func TestInputSnapshotClickVerifiedWhenNewControlTakesFocus(t *testing.T) {
	env := focusClickEnv(t)
	// The Search entry did not exist before the click (a popup
	// the click itself opened) and takes focus after it: a new
	// control is the click's own effect, never a miss.
	env.tree = focusTreeJSON("Username", focusSignInButton, focusUsernameEntry)
	env.treeAfterClick = focusTreeJSON("Search", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	res := env.call(t, `{"op": "click", "snapshot_id": "g-test-1", "element_id": 1}`)
	s := parseInputSummary(t, res)
	if !s.ClickVerified {
		t.Errorf("click_verified = false, want true (a control created by the click is no evidence of a miss)")
	}
	if got := len(env.clickCalls()); got != 1 {
		t.Errorf("got %d click calls, want 1", got)
	}
}

func TestInputSnapshotClickVerifiedWhenContainerTakesFocus(t *testing.T) {
	env := focusClickEnv(t)
	// Focus moves to the Form panel containing the button: a
	// focusable container taking focus instead of its child is
	// not a miss.
	env.tree = focusTreeJSON("Username", focusSignInButton, focusFormPanel, focusUsernameEntry)
	env.treeAfterClick = focusTreeJSON("Form", focusSignInButton, focusFormPanel, focusUsernameEntry)
	res := env.call(t, `{"op": "click", "snapshot_id": "g-test-1", "element_id": 1}`)
	s := parseInputSummary(t, res)
	if !s.ClickVerified {
		t.Errorf("click_verified = false, want true (a container taking focus is not a miss)")
	}
	if got := len(env.clickCalls()); got != 1 {
		t.Errorf("got %d click calls, want 1", got)
	}
}

func TestInputSnapshotClickVerifiedWhenPostClickTreeWalkFails(t *testing.T) {
	env := focusClickEnv(t)
	// The post-click tree walk fails: a missing signal never
	// demotes — the cursor verdict stands.
	env.tree = focusTreeJSON("Username", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	env.treeErrAfterClick = true
	res := env.call(t, `{"op": "click", "snapshot_id": "g-test-1", "element_id": 1}`)
	s := parseInputSummary(t, res)
	if !s.ClickVerified {
		t.Errorf("click_verified = false, want true (a failed post-click walk is no signal)")
	}
	if got := len(env.clickCalls()); got != 1 {
		t.Errorf("got %d click calls, want 1", got)
	}
}

func TestInputCoordClickSkipsElementRecheck(t *testing.T) {
	// A coordinate click has no element identity, so the
	// element re-check cannot run: even with another control
	// taking focus after the click, the cursor verdict stands.
	resetGroundSnapshots()
	registerFreshGrounding()
	env := newFakeInputEnv(t)
	env.kwinWindows = firefoxKWinWindow(0)
	env.tree = focusTreeJSON("Username", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	env.treeAfterClick = focusTreeJSON("Search", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	res := env.call(t, `{"op": "click", "x": 960, "y": 403}`)
	s := parseInputSummary(t, res)
	if !s.ClickVerified {
		t.Errorf("click_verified = false, want true (coordinate clicks have no element to re-check)")
	}
	if got := len(env.clickCalls()); got != 1 {
		t.Errorf("got %d click calls, want 1", got)
	}
}

// TestElementProvesMiss pins the element re-check's guards
// directly: each case is one combination of the pre/post-click
// trees, judged without the fake desktop.
func TestElementProvesMiss(t *testing.T) {
	at := cursorPos{X: 960, Y: 403}
	el := func(role, name string, x, y, w, h, win int, focused bool) atspiElement {
		wv, f, s := win, focused, true
		return atspiElement{Role: role, Name: name, X: x, Y: y, W: w, H: h, Win: &wv, Showing: &s, Focused: &f}
	}
	// The geometry: the button at (910,378,100,50), the Search
	// entry overlapping its centre, the Username entry
	// elsewhere, the Form panel containing the button.
	button := el("button", "Sign in", 910, 378, 100, 50, 0, false)
	search := el("entry", "Search", 850, 390, 300, 26, 0, false)
	searchFocused := el("entry", "Search", 850, 390, 300, 26, 0, true)
	username := el("entry", "Username", 784, 300, 352, 40, 0, false)
	usernameFocused := el("entry", "Username", 784, 300, 352, 40, 0, true)
	buttonFocused := el("button", "Sign in", 910, 378, 100, 50, 0, true)
	panelFocused := el("panel", "Form", 900, 370, 120, 70, 0, true)
	panel := el("panel", "Form", 900, 370, 120, 70, 0, false)
	frameFocused := el("frame", "Firefox", 0, 0, 1920, 1080, 0, true)
	searchOtherWin := el("entry", "Search", 850, 390, 300, 26, 1, true)
	searchOffPoint := el("entry", "Search", 1200, 390, 300, 26, 0, true)
	buttonMoved := el("button", "Sign in", 910, 478, 100, 50, 0, false)

	pre := func(focused *atspiElement, ambiguous bool, els ...atspiElement) *preClickElementState {
		return &preClickElementState{elements: els, target: button, focused: focused, focusAmbiguous: ambiguous}
	}
	cases := []struct {
		name string
		pre  *preClickElementState
		post []atspiElement
		want bool
	}{
		{"wrongControlTakesFocus", pre(&usernameFocused, false, button, search, usernameFocused), []atspiElement{button, searchFocused, username}, true},
		{"targetTakesFocus", pre(&usernameFocused, false, button, search, usernameFocused), []atspiElement{buttonFocused, search, username}, false},
		{"focusStays", pre(&usernameFocused, false, button, search, usernameFocused), []atspiElement{button, search, usernameFocused}, false},
		{"alreadyFocusedWrongControl", pre(&searchFocused, false, button, searchFocused, username), []atspiElement{button, searchFocused, username}, false},
		{"noFocusBefore", pre(nil, false, button, search, username), []atspiElement{button, searchFocused, username}, true},
		{"targetVanished", pre(&usernameFocused, false, button, search, usernameFocused), []atspiElement{searchFocused, username}, false},
		{"targetMoved", pre(&usernameFocused, false, button, search, usernameFocused), []atspiElement{buttonMoved, searchFocused, username}, false},
		{"newControlTakesFocus", pre(&usernameFocused, false, button, usernameFocused), []atspiElement{button, searchFocused, username}, false},
		{"containerTakesFocus", pre(&usernameFocused, false, button, panel, usernameFocused), []atspiElement{button, panelFocused, username}, false},
		{"noFocusedControl", pre(&usernameFocused, false, button, search, usernameFocused), []atspiElement{button, search, username}, false},
		{"twoFocusedControls", pre(&usernameFocused, false, button, search, usernameFocused), []atspiElement{button, searchFocused, usernameFocused}, false},
		{"wrongControlNotAtPoint", pre(&usernameFocused, false, button, search, usernameFocused), []atspiElement{button, searchOffPoint, username}, false},
		{"wrongControlOtherWindow", pre(&usernameFocused, false, button, search, usernameFocused), []atspiElement{button, searchOtherWin, username}, false},
		{"preFocusAmbiguous", pre(nil, true, button, search, usernameFocused), []atspiElement{button, searchFocused, username}, false},
		{"frameFocusIgnored", pre(&usernameFocused, false, button, search, usernameFocused), []atspiElement{button, searchFocused, username, frameFocused}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := elementProvesMiss(tc.pre, tc.post, at); got != tc.want {
				t.Errorf("elementProvesMiss = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- F-A23W2-9 screenshot-diff third signal: when the focus
// and element checks are silent, a click that changed nothing
// visible around its target must NOT verify. The tests below
// drive the diff through the diffCapture seam (synthetic
// before/after images); the real spectacle path is covered by
// the acceptance test in input_diff_acceptance_test.go. ---

// setDiffImages installs the diffCapture seam returning before
// on the first capture and after on the second, and returns a
// counter of captures performed.
func (e *fakeInputEnv) setDiffImages(before, after image.Image) *int {
	calls := 0
	e.tool.diffCapture = func(ctx context.Context) (image.Image, error) {
		calls++
		if calls == 1 {
			return before, nil
		}
		return after, nil
	}
	return &calls
}

// setDiffCaptureErr installs a diffCapture seam that always
// fails: the diff signal is unavailable.
func (e *fakeInputEnv) setDiffCaptureErr() {
	e.tool.diffCapture = func(ctx context.Context) (image.Image, error) {
		return nil, errors.New("fake diff capture failure")
	}
}

func TestInputSnapshotClickVerifiedWhenDiffShowsChange(t *testing.T) {
	env := focusClickEnv(t)
	// Focus stays on the Username entry (element check
	// silent), but the screen visibly changes around the
	// click point: the click had an effect, so it verifies —
	// and both diff captures ran (3 budget slots: the input
	// call plus before + after).
	env.tree = focusTreeJSON("Username", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	env.treeAfterClick = focusTreeJSON("Username", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	calls := env.setDiffImages(diffScreenImage(t, false), diffScreenImage(t, true))
	res := env.call(t, `{"op": "click", "snapshot_id": "g-test-1", "element_id": 1}`)
	s := parseInputSummary(t, res)
	if !s.ClickVerified {
		t.Errorf("click_verified = false, want true (the screen visibly changed: the click had an effect)")
	}
	if s.Warning != "" {
		t.Errorf("warning = %q, want empty", s.Warning)
	}
	if *calls != 2 {
		t.Errorf("diff captures = %d, want 2 (before + after)", *calls)
	}
	if got := env.tool.budget.TurnCalls(); got != 3 {
		t.Errorf("budget TurnCalls = %d, want 3 (input call + 2 diff captures)", got)
	}
}

func TestInputSnapshotClickVerifiedWhenTargetTakesFocusDespiteNoVisibleChange(t *testing.T) {
	env := focusClickEnv(t)
	// The button itself takes focus: positive hit evidence
	// the press reached the control — the diff must not run,
	// or it would demote a click-to-focus whose effect is
	// invisible in a screenshot by construction. Only the
	// before-capture ran (2 budget slots).
	env.tree = focusTreeJSON("Username", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	env.treeAfterClick = focusTreeJSON("Sign in", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	calls := env.setDiffImages(diffScreenImage(t, false), diffScreenImage(t, false))
	res := env.call(t, `{"op": "click", "snapshot_id": "g-test-1", "element_id": 1}`)
	s := parseInputSummary(t, res)
	if !s.ClickVerified {
		t.Errorf("click_verified = false, want true (the target itself took focus: the click landed)")
	}
	if s.Warning != "" {
		t.Errorf("warning = %q, want empty", s.Warning)
	}
	if *calls != 1 {
		t.Errorf("diff captures = %d, want 1 (the after-capture is skipped on a hit)", *calls)
	}
	if got := env.tool.budget.TurnCalls(); got != 2 {
		t.Errorf("budget TurnCalls = %d, want 2 (input call + before-capture only)", got)
	}
}

func TestInputSnapshotClickUnverifiedWhenWrongControlTakesFocusDespiteVisibleChange(t *testing.T) {
	env := focusClickEnv(t)
	// The Search entry takes focus (element miss) and the
	// screen changes — the diff must NOT resurrect a click
	// the element check already demoted: the warning stays
	// the wrong-control one, and the after-capture never runs.
	env.tree = focusTreeJSON("Username", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	env.treeAfterClick = focusTreeJSON("Search", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	calls := env.setDiffImages(diffScreenImage(t, false), diffScreenImage(t, true))
	res := env.call(t, `{"op": "click", "snapshot_id": "g-test-1", "element_id": 1}`)
	s := parseInputSummary(t, res)
	if s.ClickVerified {
		t.Errorf("click_verified = true with another control taking focus, want false (the diff never overrules a proven miss)")
	}
	if !strings.Contains(s.Warning, "wrong control") {
		t.Errorf("warning = %q, want the 'wrong control' note", s.Warning)
	}
	if s.ClickNoVisibleEffect {
		t.Errorf("click_no_visible_effect = true, want false (the demotion came from the element check, not the diff)")
	}
	if *calls != 1 {
		t.Errorf("diff captures = %d, want 1 (the after-capture is skipped after a proven miss)", *calls)
	}
}

func TestInputCoordClickUnverifiedWhenNothingVisibleChanges(t *testing.T) {
	// Coordinate clicks have no element identity, but the
	// diff still applies: the focus check is silent (target
	// inside the already-focused Firefox window) and nothing
	// changes on screen.
	resetGroundSnapshots()
	registerFreshGrounding()
	env := newFakeInputEnv(t)
	env.kwinWindows = kwinWindowsClickTarget("firefox-esr")
	calls := env.setDiffImages(diffScreenImage(t, false), diffScreenImage(t, false))
	res := env.call(t, `{"op": "click", "x": 960, "y": 403}`)
	s := parseInputSummary(t, res)
	if s.ClickVerified {
		t.Errorf("click_verified = true with nothing on screen changing, want false (the click had no visible effect)")
	}
	if !s.ClickNoVisibleEffect {
		t.Errorf("click_no_visible_effect = false, want true (the demotion came from the diff)")
	}
	if !strings.Contains(s.Warning, "no visible effect") {
		t.Errorf("warning = %q, want the 'no visible effect' note", s.Warning)
	}
	if *calls != 2 {
		t.Errorf("diff captures = %d, want 2 (before + after)", *calls)
	}
}

func TestInputClickVerifiedWhenDiffCaptureFails(t *testing.T) {
	env := focusClickEnv(t)
	// The diff signal is unavailable (no backend, capture
	// error): that is NO signal, never a demotion — the cursor
	// verdict stands. The failed capture's budget slot is
	// refunded, so only the input call itself is charged.
	env.tree = focusTreeJSON("Username", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	env.treeAfterClick = focusTreeJSON("Username", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	env.setDiffCaptureErr()
	res := env.call(t, `{"op": "click", "snapshot_id": "g-test-1", "element_id": 1}`)
	s := parseInputSummary(t, res)
	if !s.ClickVerified {
		t.Errorf("click_verified = false, want true (a failed diff capture is no signal)")
	}
	if got := env.tool.budget.TurnCalls(); got != 1 {
		t.Errorf("budget TurnCalls = %d, want 1 (the failed capture's slot is refunded)", got)
	}
}

func TestInputClickVerifiedWhenNewWindowOpensDespiteNoVisibleChange(t *testing.T) {
	env := focusClickEnv(t)
	// The click opens a NEW window (a dialog): the new window
	// is itself the click's effect, so the diff is skipped
	// even though the crop in the old window did not change.
	// Only the before-capture ran.
	env.tree = focusTreeJSON("Username", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	env.treeAfterClick = focusTreeJSON("Username", focusSignInButton, focusSearchEntry, focusUsernameEntry)
	env.kwinWindowsAfterClick = []kwinWindow{
		{Class: "firefox-esr", FG: [4]int{0, 0, 1920, 1080}, CG: [4]int{0, 0, 1920, 1080}},
		{Class: "kdialog", FG: [4]int{100, 100, 400, 300}, CG: [4]int{100, 100, 400, 300}, Active: true},
	}
	calls := env.setDiffImages(diffScreenImage(t, false), diffScreenImage(t, false))
	res := env.call(t, `{"op": "click", "snapshot_id": "g-test-1", "element_id": 1}`)
	s := parseInputSummary(t, res)
	if !s.ClickVerified {
		t.Errorf("click_verified = false, want true (the new window is itself the click's effect)")
	}
	if *calls != 1 {
		t.Errorf("diff captures = %d, want 1 (the after-capture is skipped when a new window opened)", *calls)
	}
}

func TestInputSnapshotClickUnverifiedWhenWrongWindowTakesFocusButNothingChanges(t *testing.T) {
	// Case 2, narrowed: the press lands in a WRONG
	// pre-existing window (a popup opened over the button
	// after grounding) and focus moves there — the focus check
	// has nothing to demote on, since the focus DID move —
	// but nothing changes around the target, so the diff
	// demotes it. (When the wrong window covers the crop, the
	// pixels change and the click still verifies: the diff
	// narrows case 2, it does not close it.)
	env := focusClickEnv(t)
	env.kwinWindows = []kwinWindow{
		{Class: "firefox-esr", FG: [4]int{0, 0, 1920, 1080}, CG: [4]int{0, 0, 1920, 1080}},
		{Class: "kate", FG: [4]int{0, 0, 800, 600}, CG: [4]int{0, 0, 800, 600}, Active: true},
		{Class: "other", FG: [4]int{900, 300, 400, 300}, CG: [4]int{900, 300, 400, 300}},
	}
	env.kwinWindowsAfterClick = []kwinWindow{
		{Class: "firefox-esr", FG: [4]int{0, 0, 1920, 1080}, CG: [4]int{0, 0, 1920, 1080}},
		{Class: "kate", FG: [4]int{0, 0, 800, 600}, CG: [4]int{0, 0, 800, 600}},
		{Class: "other", FG: [4]int{900, 300, 400, 300}, CG: [4]int{900, 300, 400, 300}, Active: true},
	}
	env.tree = treeWithButtonJSON(910, 378, 100, 50)
	env.treeAfterClick = treeWithButtonJSON(910, 378, 100, 50)
	calls := env.setDiffImages(diffScreenImage(t, false), diffScreenImage(t, false))
	res := env.call(t, `{"op": "click", "snapshot_id": "g-test-1", "element_id": 1}`)
	s := parseInputSummary(t, res)
	if s.ClickVerified {
		t.Errorf("click_verified = true with the focus moving to a wrong window and nothing changing, want false (the diff narrows case 2)")
	}
	if !s.ClickNoVisibleEffect {
		t.Errorf("click_no_visible_effect = false, want true (the demotion came from the diff)")
	}
	if !strings.Contains(s.Warning, "no visible effect") {
		t.Errorf("warning = %q, want the 'no visible effect' note", s.Warning)
	}
	if *calls != 2 {
		t.Errorf("diff captures = %d, want 2 (before + after)", *calls)
	}
}

// --- F-A23W2-10 (type/key TOCTOU): the focus must be re-checked
// right before typing — the safety check is ~5 s before the
// keystrokes, and the focus can move in between. ---

func TestInputTypeRefusedWhenFocusChanges(t *testing.T) {
	env := newFakeInputEnv(t)
	env.kwinWindows = []kwinWindow{
		{Class: "firefox-esr", Active: true, FG: [4]int{0, 0, 1920, 1080}, CG: [4]int{0, 0, 1920, 1080}},
		{Class: "kate", Active: false, FG: [4]int{0, 0, 800, 600}, CG: [4]int{0, 0, 800, 600}},
	}
	// Between the safety check and the keystrokes the user
	// Alt-Tabs to Kate: the text must not go there.
	env.kwinFlipTo = "kate"
	err := env.callErr(t, `{"op": "type", "text": "secret"}`)
	if !strings.Contains(err.Error(), "focused window changed") {
		t.Errorf("type after a focus change err = %v, want the 'focused window changed' refusal", err)
	}
	if got := len(env.ydotoolCalls); got != 0 {
		t.Errorf("got %d ydotool calls, want 0 (refused before typing)", got)
	}
}

func TestInputKeyRefusedWhenFocusChanges(t *testing.T) {
	env := newFakeInputEnv(t)
	env.kwinWindows = []kwinWindow{
		{Class: "firefox-esr", Active: true, FG: [4]int{0, 0, 1920, 1080}, CG: [4]int{0, 0, 1920, 1080}},
		{Class: "kate", Active: false, FG: [4]int{0, 0, 800, 600}, CG: [4]int{0, 0, 800, 600}},
	}
	env.kwinFlipTo = "kate"
	err := env.callErr(t, `{"op": "key", "key": "Return"}`)
	if !strings.Contains(err.Error(), "focused window changed") {
		t.Errorf("key after a focus change err = %v, want the 'focused window changed' refusal", err)
	}
	if got := len(env.ydotoolCalls); got != 0 {
		t.Errorf("got %d ydotool calls, want 0", got)
	}
}

func TestInputTypeRefusedWhenSensitiveWindowTakesFocus(t *testing.T) {
	env := newFakeInputEnv(t)
	env.kwinWindows = []kwinWindow{
		{Class: "firefox-esr", Active: true, FG: [4]int{0, 0, 1920, 1080}, CG: [4]int{0, 0, 1920, 1080}},
		// Sens is the pattern the KWin script itself matched
		// against this window's caption ("keepass"): the
		// takeover is refused on the flag, never on a title.
		{Class: "keepassxc", Active: false, FG: [4]int{100, 100, 800, 600}, CG: [4]int{100, 100, 800, 600}, Sens: "keepass"},
	}
	// A password-manager popup takes focus after the safety
	// check (which saw only Firefox): typing into it is refused.
	// The AT-SPI titles do not list KeePassXC — only the KWin
	// focus re-check can catch this.
	env.kwinFlipTo = "keepassxc"
	err := env.callErr(t, `{"op": "type", "text": "secret"}`)
	if !strings.Contains(err.Error(), "password manager") {
		t.Errorf("type after a sensitive window took focus err = %v, want the password-manager refusal", err)
	}
	if got := len(env.ydotoolCalls); got != 0 {
		t.Errorf("got %d ydotool calls, want 0", got)
	}
}

func TestInputTypeRefusedWhenFocusCannotBeConfirmed(t *testing.T) {
	env := newFakeInputEnv(t)
	env.treeErr = true   // no AT-SPI tree for the fallback either
	env.oracleErr = true // KWin oracle down at re-check time
	err := env.callErr(t, `{"op": "type", "text": "secret"}`)
	if !strings.Contains(err.Error(), "cannot confirm which window is focused") {
		t.Errorf("type with no focus authority err = %v, want the fail-closed 'cannot confirm' refusal", err)
	}
	if got := len(env.ydotoolCalls); got != 0 {
		t.Errorf("got %d ydotool calls, want 0", got)
	}
}
