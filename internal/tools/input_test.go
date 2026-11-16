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
	stuck           bool       // ydotool moves never land
	firstMoveFactor float64    // acceleration distortion on the 1st move only (1.0 = exact)
	oracleFixed     *cursorPos // oracle reports this instead of cursor (disagreement)
	oracleErr       bool
	kwinWindows     []kwinWindow
	moveCount       int

	tree          string // canned AT-SPI tree JSON ("" = default benign tree)
	treeErr       bool   // atspiTree fails -> xdotool fallback
	xdotoolTitles []string
	xdotoolErr    bool
	locked        bool

	socketExists bool
	socketPath   string
	ydotoolCalls []fakeYdotoolCall
	kwinCalls    int
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
				if env.moveCount == 1 {
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
		out := inputCursorOutput{Cursor: cur, Screen: &env.screen, Windows: env.kwinWindows}
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
	// must correct the residual and converge.
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
	if s.Iterations != 2 {
		t.Errorf("correction_moves = %d, want 2 (distorted landing + exact correction)", s.Iterations)
	}
	if got := len(env.mousemoveCalls()); got != 2 {
		t.Errorf("got %d mousemove calls, want 2", got)
	}
	// The first call moves the exact residual from the oracle
	// reading (860, 440); the second corrects what is left.
	first := env.mousemoveCalls()[0]
	if fmt.Sprint(first.args) != fmt.Sprint([]string{"mousemove", "-x", "860", "-y", "440"}) {
		t.Errorf("first mousemove = %v, want the exact residual (860, 440)", first.args)
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
	groundSnapshots.register("g-test-1", 0, time.Now(), []groundElement{
		{ID: 1, Centre: groundPoint{X: 960, Y: 403}},
		{ID: 2, Centre: groundPoint{X: 960, Y: 484}},
	})
	env := newFakeInputEnv(t)
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
	})
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
	})
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
	})
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
	})
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
	env := newFakeInputEnv(t)
	env.tree = sensitiveInputTreeJSON()
	// KWin places the KeePassXC window 29 px lower than AT-SPI
	// reports (the M2 F2 shift): the corrected box is
	// (100,129)-(900,729). A click at (500,110) is inside the
	// RAW box but outside the corrected one — allowed only
	// because the correction is applied; a click at (500,200)
	// is inside the corrected box — refused.
	env.kwinWindows = []kwinWindow{
		{Caption: "KeePassXC — passwords.kdbx", Class: "keepassxc", FG: [4]int{100, 129, 800, 600}, CG: [4]int{100, 129, 800, 600}},
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
	env := newFakeInputEnv(t)
	env.tree = sensitiveInputTreeJSON()
	env.oracleErr = true // no KWin -> boxes uncorrected -> coarse refusal
	err := env.callErr(t, `{"op": "click", "x": 1500, "y": 900}`)
	if !strings.Contains(err.Error(), "will not click while it is visible") {
		t.Errorf("err = %v, want the coarse sensitive-window click refusal", err)
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
