// Package tools — input (computer-use tool 3 of 5, M3).
//
// input is the first tool that ACTS on the desktop: it moves the
// cursor, clicks, types and presses keys, driven by ydotool
// (ydotoold + uinput; M3 decision 2026-11-15 19:22, Irfan's
// delegation) through the Wayland-native path. X11 is fallback
// only — there is no xdotool input path here, because xdotool
// speaks to the diverged XWayland pointer, not the Wayland
// cursor the user sees.
//
// Operations (one tool, four ops — the op selects the effect):
//   - move {x, y}: absolute move with CLOSED-LOOP correction.
//   - click {button, clicks, x, y} or {button, clicks,
//     snapshot_id, element_id}: move to the target (closed-loop)
//     then click. The snapshot form is the point of the M2+M3
//     integration: the agent clicks a grounded element number,
//     never a guessed coordinate.
//   - type {text}: type printable text into the focused window.
//   - key {key}: press one key from an explicit allowlist.
//
// Closed-loop moves (the acceleration answer): the ydotool
// device is RELATIVE-only (EV_REL, no EV_ABS — live M3 probe),
// so `mousemove --absolute` is implemented inside ydotool as
// relative deltas from a start position it cannot know, and
// libinput pointer acceleration scales every relative move
// timing-dependently (a +20 raw move reported as 8.00
// accelerated, a +10 as 17.50). A single call can therefore
// never promise a pixel-exact landing. The loop is the
// mechanism instead: read the true position from the KWin
// oracle, issue the exact residual as a RELATIVE ydotool move
// (relative deltas are exact arithmetic from a known start —
// the oracle reading — unlike --absolute, which trusts an
// assumed start), re-read, correct again, up to
// inputMaxMoveIterations times, within inputMoveTolerancePx.
// Disabling acceleration instead would need a libinput
// device-config change and would alter the user's own pointer
// behaviour for every app — a system change for one tool's
// convenience, rejected: the loop needs no config change, no
// root, and no side effect on the user's pointer. The loop is
// pinned by tests (a distorted first landing is corrected; a
// stuck cursor gives up honestly).
//
// The oracle: KWin workspace.cursorPos via a vendored helper
// (input_kwin.py, embedded, written 0600 — a SEPARATE helper
// from ground_kwin.py, not an extension of it: ground's helper
// output contract is pinned by M2's tests and live verification,
// and the cursor query has a different shape (cursor + virtual
// screen geometry + window list) and a different cadence (called
// up to ~6 times per move, so it must stay cheap). The two
// helpers share the invocation pattern — unique marker,
// loadScript/start, journal poll, unload — which measured 10/10
// reliable for ground. xdotool is never an oracle here: it
// reads the diverged XWayland pointer (stuck at 960,672 while
// KWin reported 134,36).
//
// ydotool invocation: /usr/bin/ydotool via exec with a per-call
// timeout; the client talks to $XDG_RUNTIME_DIR/.ydotool_socket
// (or $YDOTOOL_SOCKET). The tool checks the socket exists
// before doing anything and returns a clear "daemon not
// running" error when it does not — it NEVER starts the daemon
// itself (starting it needs the input group / a user-manager
// login, which is deployment, not a tool call).
//
// Risk class: ClassDestructive — ONE tool, ONE class, and the
// class covers the riskiest operation. move alone would be Safe
// (position is not an effect), but click can activate, close or
// submit things, and type/key can enter text or submit a form
// in whatever window is focused — so the tool is gated like the
// other action tools, and plan mode blocks it while a plan is
// pending. The per-operation reasoning is in the RiskClass
// comment below; the gate map (destructiveTools) is the
// authority and must agree (pinned by
// TestClassifyMapsMatchFactorySet).
//
// Safety (same shape as ground/screenshot, plus the action-tool
// additions):
//   - refused outright in unattended (cron) turns, no exceptions;
//   - refused when the screen is locked (same loginctl seam as
//     screenshot — injecting into a locked screen is meaningless);
//   - type/key refuse when a sensitive window (password manager /
//     bank / payment / OTP, the computeruse.go lists) is focused
//     or visible, and refuse when the window titles cannot be
//     enumerated at all (fail closed — typing blind is the
//     highest-risk op: text typed into the wrong field lands in
//     history, a log, or a submitted form). The titles come from
//     ground's AT-SPI tree — the M2 safety layer that sees native
//     Wayland windows xdotool cannot — with the screenshot
//     tool's xdotool titles as fallback when there is no tree;
//   - click refuses when a sensitive window is present, EXCEPT
//     when every sensitive window's box is known precisely
//     (AT-SPI box corrected by the KWin geometry, the M2 F2
//     correction) and the target point lies outside all of them
//     — then the click is elsewhere on screen and safe. An
//     uncorrected sensitive window, a titles-only enumeration,
//     or no enumeration at all refuses the click;
//   - move is allowed under a sensitive window: moving the cursor
//     cannot activate, type, close or submit anything — position
//     is not an effect. The click/type/key guards are what carry
//     the safety, and a move is always followed by a guarded
//     click, never a bare effect;
//   - type never echoes the typed text in its result (only the
//     character count): the text could be a password, and the
//     result is stored in history.
//   - the tool journal never records input (see
//     journalExcludedTools in internal/core/agent.go): replaying
//     a turn would re-execute clicks and keystrokes.
//
// Rate limits: the M1 screenshot discipline (1 s min interval,
// 15 per turn, 120 per hour), same numbers as ground/screenshot.
package tools

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
)

//go:embed input_kwin.py
var inputKWinHelperSource string

func init() {
	RegisterFactory("input", func(deps ToolDeps) (Tool, error) {
		return newInputTool(), nil
	})
}

// Input limits (M1 screenshot discipline + M3 move bounds).
const (
	inputMinInterval       = time.Second
	inputMaxPerTurn        = 15
	inputMaxPerHour        = 120
	inputMaxTypeRunes      = 1000 // longest text one type call may enter
	inputMoveTolerancePx   = 5    // closed-loop success: both axes within 5 px
	inputMaxMoveIterations = 5    // correction moves before giving up honestly
	inputMoveSettle        = 100 * time.Millisecond
	inputYdotoolTimeout    = 10 * time.Second
	inputKWinTimeout       = 5 * time.Second // same as ground's KWin timeout
	inputKWinHelperFile    = "input-kwin-helper.py"
	// inputScreenCacheTTL bounds how long the cached screen size
	// may be trusted for the fail-fast coordinate check
	// (F-A23W2-17): the oracle re-reads on every move/click, so
	// in active use the cache is seconds old; past this age the
	// size is treated as unknown and the post-oracle check in
	// doMove is the enforcement point.
	inputScreenCacheTTL = 60 * time.Second
)

// inputClickCodes maps button names to the ydotool click hex
// values (Debian ydotool(1) man page: the button number OR'd
// with 0x40 down + 0x80 up — 0xC0 is a left click, 0xC1 right,
// 0xC2 middle). Names, not raw codes, are the tool's interface:
// the model never sees a hex value.
var inputClickCodes = map[string]int{
	"left":   0xC0,
	"right":  0xC1,
	"middle": 0xC2,
}

// inputKeyCodes maps the allowed key names to Linux input event
// codes (input-event-codes.h, verified against the header on
// this machine: KEY_ESC 1, KEY_BACKSPACE 14, KEY_TAB 15,
// KEY_ENTER 28, KEY_SPACE 57, KEY_F1 59 … KEY_F12 88,
// KEY_HOME 102, KEY_UP 103, KEY_PAGEUP 104, KEY_LEFT 105,
// KEY_RIGHT 106, KEY_END 107, KEY_DOWN 108, KEY_PAGEDOWN 109,
// KEY_DELETE 111).
//
// `ydotool key` takes numeric KEYCODE:PRESSED pairs only
// (28:1 28:0 is Enter) — a non-numeric value is silently
// treated as a delay, so the tool translates names to codes
// itself and never passes a name through. The allowlist is
// single keys only: NO modifiers, NO combinations (no Ctrl/Alt/
// Super chords — those need multi-key sequences and their own
// safety review; a future op can add them). "return" and
// "enter" are the same key; "esc" is an alias for "escape".
var inputKeyCodes = map[string]int{
	"return": 28, "enter": 28,
	"escape": 1, "esc": 1,
	"tab":       15,
	"backspace": 14,
	"delete":    111,
	"home":      102,
	"end":       107,
	"pageup":    104, "page_up": 104,
	"pagedown": 109, "page_down": 109,
	"up": 103, "down": 108, "left": 105, "right": 106,
	"space": 57,
	"f1":    59, "f2": 60, "f3": 61, "f4": 62, "f5": 63, "f6": 64,
	"f7": 65, "f8": 66, "f9": 67, "f10": 68, "f11": 87, "f12": 88,
}

// inputArgs is the tool's JSON arguments.
type inputArgs struct {
	Op         string `json:"op"`          // move | click | type | key
	X          *int   `json:"x"`           // move / click by coordinates
	Y          *int   `json:"y"`           // move / click by coordinates
	Button     string `json:"button"`      // click: left | right | middle
	Clicks     *int   `json:"clicks"`      // click: 1 | 2
	SnapshotID string `json:"snapshot_id"` // click by element
	ElementID  *int   `json:"element_id"`  // click by element
	Text       string `json:"text"`        // type
	Key        string `json:"key"`         // key
}

// inputSummary is the JSON text returned on success.
type inputSummary struct {
	Op         string        `json:"op"`
	Target     *cursorPos    `json:"target,omitempty"`
	Final      *cursorPos    `json:"final_cursor,omitempty"`
	Iterations int           `json:"correction_moves,omitempty"` // move/click: ydotool moves issued
	Reached    bool          `json:"reached,omitempty"`          // move/click: oracle agrees
	Button     string        `json:"button,omitempty"`
	Clicks     int           `json:"clicks,omitempty"`
	Element    *inputElement `json:"element,omitempty"`     // click by snapshot
	CharsTyped int           `json:"chars_typed,omitempty"` // type: count only, never the text
	Key        string        `json:"key,omitempty"`
}

type inputElement struct {
	SnapshotID string `json:"snapshot_id"`
	ElementID  int    `json:"element_id"`
}

// inputCursorOutput is the KWin helper's JSON document.
type inputCursorOutput struct {
	Cursor  cursorPos    `json:"cursor"`
	Screen  *inputScreen `json:"screen"` // nil when KWin has no virtualScreenGeometry
	Windows []kwinWindow `json:"kwin_windows"`
}

type inputScreen struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// inputWindowInfo is the sensitive-window enumeration: titles
// (AT-SPI first, xdotool fallback), window boxes when the
// AT-SPI tree was available, and whether every sensitive
// window's box is KWin-corrected (the precise-click condition).
type inputWindowInfo struct {
	available        bool
	titles           []string
	windows          []atspiWindow // AT-SPI boxes, KWin-corrected where matched
	sensitivePrecise bool          // every sensitive window has a corrected box
}

type inputTool struct {
	mu        sync.Mutex
	lastCall  time.Time
	hourCalls []time.Time
	turnCalls int
	// screenW/H is the virtual screen size from the KWin oracle,
	// cached on first sight (0 = unknown yet), with the time it
	// was read. Coordinate validation uses it when known AND
	// fresh (F-A23W2-17: the old cache never invalidated, so a
	// monitor plug/unplug or resolution change left
	// validatePoint checking against a dead screen — a valid
	// point on a bigger screen was rejected, and a point outside
	// a shrunken screen was accepted until the move re-checked).
	// A cache older than inputScreenCacheTTL is treated as
	// unknown: only non-negativity is checked up front, and
	// doMove re-validates against the fresh oracle reading
	// before any ydotool call, so the stale bound can never
	// cause a wrong move — only a fail-fast rejection while the
	// cache is fresh.
	screenW, screenH int
	screenCachedAt time.Time

	// ground supplies the AT-SPI tree for the sensitive-window
	// check (the M2 safety layer); shot supplies the xdotool
	// title fallback and the screen-locked check.
	ground *groundTool
	shot   *screenshotTool

	// Seams, overridable in tests. Defaults run the real backends.
	ydotool         func(ctx context.Context, stdin string, args ...string) (string, error)
	lookPath        func(name string) (string, error)
	homeDir         func() (string, error)
	now             func() time.Time
	sleep           func(time.Duration)
	runKWinHelper   func(ctx context.Context, helperPath string) ([]byte, error)
	writeKWinHelper func() (string, error)
	socketPath      func() (string, error)
}

func newInputTool() *inputTool {
	t := &inputTool{
		ground:   newGroundTool(),
		shot:     newScreenshotTool(),
		lookPath: exec.LookPath,
		homeDir:  os.UserHomeDir,
		now:      time.Now,
		sleep:    time.Sleep,
	}
	t.ydotool = t.defaultYdotool
	t.runKWinHelper = t.defaultRunKWinHelper
	t.writeKWinHelper = t.defaultWriteKWinHelper
	t.socketPath = defaultYdotoolSocketPath
	return t
}

func (t *inputTool) Name() string { return "input" }

func (t *inputTool) Description() string {
	return "Act on the desktop: move the cursor, click, type text or press a single key (ydotool). move takes absolute {x, y} and corrects itself against the compositor's cursor position until it lands within a few pixels or gives up honestly; click takes {button, clicks} plus either {x, y} or {snapshot_id, element_id} from a ground call (the snapshot form is preferred — it clicks a grounded element, never a guessed coordinate); type takes printable {text} (no control characters — Tab/Return only via key); key takes one key from the allowlist (Return, Escape, Tab, BackSpace, Delete, Home, End, PageUp, PageDown, arrows, Space, F1–F12 — no modifiers or combinations). Gated (destructive): click/type/key can activate, close or submit things. Refuses on sensitive windows (password manager / bank / payment / one-time-code) and in unattended (cron) turns, and when the ydotool daemon is not running."
}

func (t *inputTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"op":          map[string]any{"type": "string", "enum": []string{"move", "click", "type", "key"}, "description": "Which input action to perform."},
				"x":           map[string]any{"type": "integer", "description": "Target x in screen pixels (move, or click by coordinates)."},
				"y":           map[string]any{"type": "integer", "description": "Target y in screen pixels (move, or click by coordinates)."},
				"button":      map[string]any{"type": "string", "enum": []string{"left", "right", "middle"}, "description": "Click button (default left)."},
				"clicks":      map[string]any{"type": "integer", "minimum": 1, "maximum": 2, "description": "Number of clicks: 1 (default) or 2 for a double-click."},
				"snapshot_id": map[string]any{"type": "string", "description": "Click by element: the snapshot_id from a ground call (with element_id). The snapshot must be the newest grounding and under 60 s old, or the click is refused — ground again."},
				"element_id":  map[string]any{"type": "integer", "description": "Click by element: the element id from that grounding's element list."},
				"text":        map[string]any{"type": "string", "description": "Text to type (type only): printable characters, at most 1000 runes, no control characters — a newline or tab in the text is refused; use key Return / Tab instead. The typed text is never echoed back in the result."},
				"key":         map[string]any{"type": "string", "description": "Single key to press (key only): Return, Escape, Tab, BackSpace, Delete, Home, End, PageUp, PageDown, Up, Down, Left, Right, Space, F1–F12. No modifiers or combinations."},
			},
			"required": []string{"op"},
		},
	}
}

// RiskClass declares the tool destructive — ONE tool, ONE class,
// and the class covers the riskiest operation, not the safest:
//   - move alone would be Safe: cursor position is not an
//     effect (it cannot activate, type, close or submit
//     anything), and move is allowed under a sensitive window
//     for exactly that reason;
//   - click can activate, close or submit whatever is under the
//     target — a destructive effect on the machine;
//   - type/key send keystrokes to whatever window is focused,
//     which can enter text into the wrong field or submit a form.
//
// The class is therefore the max over the ops (destructive),
// the tool is gated like the other action tools, and plan mode
// blocks it while a plan is pending. Splitting move into its
// own Safe tool was considered and rejected: the four ops share
// one transport, one oracle and one rate limit, and a Safe move
// tool next to a gated click tool would invite move-then-click
// sequences that dodge the gate's per-call approval (the gate
// approves the whole input call, move included).
func (t *inputTool) RiskClass() permissions.Class { return permissions.ClassDestructive }

// BeginTurn resets the per-turn call counter (see TurnResetter).
func (t *inputTool) BeginTurn() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.turnCalls = 0
}

// Execute is the text-only Tool interface method.
func (t *inputTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	res, err := t.ExecuteResult(ctx, args)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

// ExecuteResult runs one input operation and returns its JSON summary.
func (t *inputTool) ExecuteResult(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	invalid := func(format string, v ...any) (ToolResult, error) {
		return ToolResult{}, &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf(format, v...)}
	}
	denied := func(format string, v ...any) (ToolResult, error) {
		return ToolResult{}, &ToolError{Tool: t.Name(), Kind: "denied", Err: fmt.Errorf(format, v...)}
	}
	failed := func(format string, v ...any) (ToolResult, error) {
		return ToolResult{}, &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf(format, v...)}
	}

	// Computer-use is OFF in unattended turns, no exceptions
	// (plan §11.3) — refused before anything else happens. The
	// gate also denies gated tools in unattended turns, so this
	// is the second line of defence, like ground/screenshot.
	if UnattendedTurnFromContext(ctx) {
		return denied("computer-use is off in unattended (cron) turns: input refused")
	}

	var a inputArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return invalid("arguments must be a JSON object: %v", err)
		}
	}

	// Per-op validation — no side effects in this block, so a
	// bad call fails before touching the desktop. The click
	// target (coordinates or snapshot element centre) is
	// resolved here, so an unknown/stale snapshot fails fast.
	var target *cursorPos
	var keyCode int
	button := a.Button
	clicks := 1
	switch strings.ToLower(strings.TrimSpace(a.Op)) {
	case "move":
		if a.X == nil || a.Y == nil {
			return invalid("move needs x and y (absolute screen pixels)")
		}
		if err := t.validatePoint(*a.X, *a.Y); err != nil {
			return invalid("%v", err)
		}
		target = &cursorPos{X: *a.X, Y: *a.Y}
	case "click":
		if button == "" {
			button = "left"
		}
		if _, ok := inputClickCodes[button]; !ok {
			return invalid("button must be left, right or middle, got %q", a.Button)
		}
		if a.Clicks != nil {
			clicks = *a.Clicks
		}
		if clicks < 1 || clicks > 2 {
			return invalid("clicks must be 1 or 2, got %d", clicks)
		}
		bySnapshot := a.SnapshotID != "" || a.ElementID != nil
		byCoords := a.X != nil || a.Y != nil
		if bySnapshot && byCoords {
			return invalid("click takes either snapshot_id + element_id or x + y, not both")
		}
		if !bySnapshot && !byCoords {
			return invalid("click needs either snapshot_id + element_id or x + y")
		}
		if bySnapshot {
			if a.SnapshotID == "" || a.ElementID == nil {
				return invalid("click by element needs both snapshot_id and element_id")
			}
			centre, err := GroundSnapshotElementCentre(ctx, a.SnapshotID, *a.ElementID)
			if err != nil {
				return invalid("cannot click element %d in snapshot %s: %v", *a.ElementID, a.SnapshotID, err)
			}
			target = &cursorPos{X: centre.X, Y: centre.Y}
		} else {
			if a.X == nil || a.Y == nil {
				return invalid("click by coordinates needs both x and y")
			}
			if err := t.validatePoint(*a.X, *a.Y); err != nil {
				return invalid("%v", err)
			}
			target = &cursorPos{X: *a.X, Y: *a.Y}
		}
	case "type":
		if a.Text == "" {
			return invalid("type needs non-empty text")
		}
		if n := utf8.RuneCountInString(a.Text); n > inputMaxTypeRunes {
			return invalid("text is %d runes, over the %d-rune cap for one type call: split it across calls", n, inputMaxTypeRunes)
		}
		for _, r := range a.Text {
			if r < 0x20 || r == 0x7F {
				return invalid("text contains a control character (U+%04X): type takes printable text only — Tab/Return only via the key op", r)
			}
		}
	case "key":
		code, ok := inputKeyCodes[strings.ToLower(strings.TrimSpace(a.Key))]
		if !ok {
			return invalid("key %q is not in the allowlist (Return, Escape, Tab, BackSpace, Delete, Home, End, PageUp, PageDown, Up, Down, Left, Right, Space, F1–F12): no modifiers or combinations", a.Key)
		}
		keyCode = code
	default:
		return invalid("op must be move, click, type or key, got %q", a.Op)
	}
	op := strings.ToLower(strings.TrimSpace(a.Op))

	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return failed("no graphical session available: neither DISPLAY nor WAYLAND_DISPLAY is set")
	}

	// Rate limits (M1 discipline): min 1s between calls, max 15
	// per turn, max 120 per hour.
	if err := t.checkRateLimit(); err != nil {
		return denied("%v", err)
	}

	// Screen locked: injecting into a locked screen is
	// meaningless — refuse, no retry (same seam as screenshot).
	if locked, _ := t.shot.screenLocked(ctx); locked {
		return failed("the screen is locked (or the display is off): unlock it and ask again — I will not retry on my own")
	}

	// The daemon must be running: check the socket (and the
	// client binary) before any helper runs, so a down daemon
	// fails with a clear error instead of a confusing ydotool
	// failure halfway through a move. The tool never starts the
	// daemon itself.
	if err := t.checkDaemon(); err != nil {
		return failed("%v", err)
	}

	var summary inputSummary
	summary.Op = op
	switch op {
	case "move":
		final, iters, err := t.doMove(ctx, target.X, target.Y)
		if err != nil {
			return failed("%v", err)
		}
		summary.Target = target
		summary.Final = &final
		summary.Iterations = iters
		summary.Reached = true
	case "click":
		// The sensitive check is on the CLICK target, before
		// the move: a refused click never moves the cursor
		// towards a sensitive window.
		if err := t.checkClickSensitive(ctx, target.X, target.Y); err != nil {
			return denied("%v", err)
		}
		final, iters, err := t.doClick(ctx, button, clicks, a.SnapshotID, a.ElementID, *target)
		if err != nil {
			return failed("%v", err)
		}
		summary.Target = target
		summary.Final = &final
		summary.Iterations = iters
		summary.Reached = true
		summary.Button = button
		summary.Clicks = clicks
		if a.SnapshotID != "" {
			summary.Element = &inputElement{SnapshotID: a.SnapshotID, ElementID: *a.ElementID}
		}
	case "type":
		if err := t.checkTypeSensitive(ctx); err != nil {
			return denied("%v", err)
		}
		// Type via stdin (ydotool type --file -): the text
		// never appears in an argv (no leading-dash parsing,
		// no argv-length limit) and never in the result.
		if _, err := t.ydotool(ctx, a.Text, "type", "--file", "-"); err != nil {
			return failed("ydotool type failed: %v", err)
		}
		summary.CharsTyped = utf8.RuneCountInString(a.Text)
	case "key":
		if err := t.checkTypeSensitive(ctx); err != nil {
			return denied("%v", err)
		}
		code := strconv.Itoa(keyCode)
		if _, err := t.ydotool(ctx, "", "key", code+":1", code+":0"); err != nil {
			return failed("ydotool key failed: %v", err)
		}
		summary.Key = strings.ToLower(strings.TrimSpace(a.Key))
	}

	text, err := json.Marshal(summary)
	if err != nil {
		return failed("cannot encode the input summary: %v", err)
	}
	return ToolResult{Text: string(text)}, nil
}

// validatePoint checks a target against the known screen size:
// always non-negative, and inside the virtual screen when the
// oracle has reported it RECENTLY (F-A23W2-17). A stale cached
// size is not enforced here — the screen may have changed since
// it was read — and doMove re-validates against the fresh oracle
// reading before any ydotool call. A target outside the screen
// is an argument error, not a move to attempt — the compositor
// would clamp it and the loop would give up after 5 wasted moves.
func (t *inputTool) validatePoint(x, y int) error {
	if x < 0 || y < 0 {
		return fmt.Errorf("coordinates must be non-negative, got (%d, %d)", x, y)
	}
	t.mu.Lock()
	w, h, at := t.screenW, t.screenH, t.screenCachedAt
	t.mu.Unlock()
	if w > 0 && h > 0 && t.now().Sub(at) <= inputScreenCacheTTL && (x >= w || y >= h) {
		return fmt.Errorf("coordinates (%d, %d) are outside the %dx%d screen", x, y, w, h)
	}
	return nil
}

// checkDaemon verifies the ydotool client exists and its daemon
// socket exists. A missing socket means the daemon is not
// running: a clear error with the remedy, never a daemon start.
func (t *inputTool) checkDaemon() error {
	if _, err := t.lookPath("ydotool"); err != nil {
		return fmt.Errorf("ydotool is not installed (no ydotool binary on PATH): the input tool needs the ydotool client and a running ydotoold daemon")
	}
	sock, err := t.socketPath()
	if err != nil {
		return fmt.Errorf("cannot resolve the ydotool socket: %v", err)
	}
	if _, err := os.Stat(sock); err != nil {
		return fmt.Errorf("the ydotool daemon is not running (no socket at %s): start it (systemctl --user start ydotoold) — the input tool never starts the daemon itself", sock)
	}
	return nil
}

// defaultYdotool runs the ydotool client with a per-call timeout.
// stdin is passed through only for `type --file -`; every other
// call passes "".
func (t *inputTool) defaultYdotool(ctx context.Context, stdin string, args ...string) (string, error) {
	path, err := t.lookPath("ydotool")
	if err != nil || path == "" {
		return "", fmt.Errorf("ydotool is not installed: %v", err)
	}
	cctx, cancel := context.WithTimeout(ctx, inputYdotoolTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, path, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.String(), fmt.Errorf("ydotool %s failed: %v (output: %s)", args[0], err, strings.TrimSpace(buf.String()))
	}
	return buf.String(), nil
}

// defaultYdotoolSocketPath resolves the daemon socket the way
// the ydotool client does: $YDOTOOL_SOCKET first, then the
// default $XDG_RUNTIME_DIR/.ydotool_socket, then
// /run/user/<uid>/.ydotool_socket when the runtime dir is unset.
func defaultYdotoolSocketPath() (string, error) {
	if p := os.Getenv("YDOTOOL_SOCKET"); p != "" {
		return p, nil
	}
	if r := os.Getenv("XDG_RUNTIME_DIR"); r != "" {
		return filepath.Join(r, ".ydotool_socket"), nil
	}
	return filepath.Join("/run/user", strconv.Itoa(os.Getuid()), ".ydotool_socket"), nil
}

// defaultWriteKWinHelper writes the embedded KWin cursor helper
// to ~/.orion/input-kwin-helper.py (0600, atomic temp+rename)
// and returns its path. Same shape as ground's helper install.
func (t *inputTool) defaultWriteKWinHelper() (string, error) {
	return installEmbeddedHelper(t.homeDir, inputKWinHelperFile, inputKWinHelperSource)
}

// defaultRunKWinHelper runs the KWin cursor helper with the KWin
// timeout and returns its stdout. Any failure (exit code,
// timeout, empty output) is an error — the caller fails the
// move instead of moving on a missing oracle.
func (t *inputTool) defaultRunKWinHelper(ctx context.Context, helperPath string) ([]byte, error) {
	hctx, cancel := context.WithTimeout(ctx, inputKWinTimeout)
	defer cancel()
	cmd := exec.CommandContext(hctx, "python3", helperPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("KWin cursor helper failed: %v (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	if stdout.Len() == 0 {
		return nil, fmt.Errorf("KWin cursor helper produced no output")
	}
	return stdout.Bytes(), nil
}

// readCursor asks KWin for the true cursor position (plus screen
// size and window geometries). It caches the screen size on
// first sight and refreshes the cache (with its timestamp) on
// every oracle read — the cache is only ever as old as the last
// move/click, and validatePoint stops trusting it past
// inputScreenCacheTTL (F-A23W2-17). Any oracle failure is an
// error: a move without a position proof cannot run closed-loop,
// so it does not run at all.
func (t *inputTool) readCursor(ctx context.Context) (inputCursorOutput, error) {
	helperPath, err := t.writeKWinHelper()
	if err != nil {
		return inputCursorOutput{}, fmt.Errorf("cannot install the KWin cursor helper: %v", err)
	}
	raw, err := t.runKWinHelper(ctx, helperPath)
	if err != nil {
		return inputCursorOutput{}, fmt.Errorf("cannot read the cursor position (KWin oracle): %v", err)
	}
	var out inputCursorOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return inputCursorOutput{}, fmt.Errorf("cannot parse the KWin cursor output: %v", err)
	}
	if out.Screen != nil && out.Screen.W > 0 && out.Screen.H > 0 {
		t.mu.Lock()
		t.screenW, t.screenH = out.Screen.W, out.Screen.H
		t.screenCachedAt = t.now()
		t.mu.Unlock()
	}
	return out, nil
}

// withinTolerance reports whether cur is close enough to the
// target: both axes within inputMoveTolerancePx (Chebyshev, not
// Euclidean — a click target 5 px off in x and 5 px off in y is
// still on a normal-sized control, and the bound is checkable
// per axis in the result).
func withinTolerance(cur cursorPos, x, y int) bool {
	dx := cur.X - x
	if dx < 0 {
		dx = -dx
	}
	dy := cur.Y - y
	if dy < 0 {
		dy = -dy
	}
	return dx <= inputMoveTolerancePx && dy <= inputMoveTolerancePx
}

// doMove moves the cursor to (x, y) with closed-loop correction
// and returns the oracle-confirmed final position plus the
// number of ydotool moves issued. A move that is already within
// tolerance issues no ydotool call at all. A move that does not
// converge within inputMaxMoveIterations returns an error that
// says so — with the final oracle position — and NEVER claims
// success: the caller reports the failure, the model re-grounds
// or retries, and no click ever follows a failed move.
func (t *inputTool) doMove(ctx context.Context, x, y int) (cursorPos, int, error) {
	cur, err := t.readCursor(ctx)
	if err != nil {
		return cursorPos{}, 0, err
	}
	// The oracle just reported the screen size (cached by
	// readCursor): re-validate the target against it now,
	// before any ydotool call. The argument check in
	// ExecuteResult could only test non-negativity on the first
	// call in a process, when no screen size is cached yet —
	// without this, a target outside the screen would burn all
	// 5 correction moves against the compositor's clamp before
	// failing. A target rejected here is an argument error in
	// substance (same message as the arg-time check), reported
	// as an execution failure because it was only knowable
	// after the oracle ran.
	if err := t.validatePoint(x, y); err != nil {
		return cursorPos{}, 0, err
	}
	if withinTolerance(cur.Cursor, x, y) {
		return cur.Cursor, 0, nil
	}
	for i := 1; i <= inputMaxMoveIterations; i++ {
		// The residual as a RELATIVE move: exact arithmetic
		// from the oracle's known start. --absolute is not
		// used: on a relative-only device it is relative
		// deltas from a start position ydotool cannot know.
		dx, dy := x-cur.Cursor.X, y-cur.Cursor.Y
		if _, err := t.ydotool(ctx, "", "mousemove", "-x", strconv.Itoa(dx), "-y", strconv.Itoa(dy)); err != nil {
			return cursorPos{}, i, fmt.Errorf("ydotool mousemove failed: %v", err)
		}
		// Let the compositor apply the motion before the
		// oracle re-reads, or the loop corrects a stale
		// position and oscillates.
		t.sleep(inputMoveSettle)
		cur, err = t.readCursor(ctx)
		if err != nil {
			return cursorPos{}, i, err
		}
		if withinTolerance(cur.Cursor, x, y) {
			return cur.Cursor, i, nil
		}
	}
	return cur.Cursor, inputMaxMoveIterations,
		fmt.Errorf("could not reach (%d, %d) within %d px after %d correction moves: the cursor is at (%d, %d) — the screen may have moved, or pointer acceleration is fighting the correction; ground again and retry, or move in smaller steps", x, y, inputMoveTolerancePx, inputMaxMoveIterations, cur.Cursor.X, cur.Cursor.Y)
}

// doClick moves to the target (closed-loop — a failed move
// aborts before any click, so a click never lands in the wrong
// place) and then clicks. For a snapshot click the snapshot is
// re-validated immediately before the click: the element could
// have gone stale during the sensitive check and the move, and
// a stale element must never be clicked.
func (t *inputTool) doClick(ctx context.Context, button string, clicks int, snapshotID string, elementID *int, target cursorPos) (cursorPos, int, error) {
	final, iters, err := t.doMove(ctx, target.X, target.Y)
	if err != nil {
		return cursorPos{}, iters, err
	}
	if snapshotID != "" && elementID != nil {
		if _, err := GroundSnapshotElementCentre(ctx, snapshotID, *elementID); err != nil {
			return cursorPos{}, iters, fmt.Errorf("snapshot went stale before the click: %v (ground again)", err)
		}
	}
	code := inputClickCodes[button]
	if _, err := t.ydotool(ctx, "", "click", "--repeat", strconv.Itoa(clicks), fmt.Sprintf("0x%02X", code)); err != nil {
		return cursorPos{}, iters, fmt.Errorf("ydotool click failed: %v", err)
	}
	return final, iters, nil
}

// windowInfo enumerates the visible windows for the sensitive
// checks: AT-SPI titles + boxes first (the M2 safety layer —
// it sees native Wayland windows xdotool cannot), KWin
// geometries to correct those boxes (the M2 F2 correction),
// xdotool titles as fallback when there is no AT-SPI tree.
// available is false only when nothing could enumerate the
// windows at all — the type/key/click checks fail closed then.
//
// Cost, stated plainly: the AT-SPI walk is D-Bus round-trip
// bound (~5 s on a heavy page), so a guarded type/key/click
// pays a tree walk before it acts. That is the price of the
// full-coverage check; a future optimisation could read the
// window titles from the KWin list alone (the cursor helper
// already returns captions), which is ~0.1 s — flagged for M4,
// not done here, because the brief pins the AT-SPI check.
func (t *inputTool) windowInfo(ctx context.Context) inputWindowInfo {
	tree, _ := t.ground.atspiTree(ctx)
	if tree == nil {
		titles, err := t.shot.windowTitles(ctx)
		if err != nil {
			return inputWindowInfo{available: false}
		}
		return inputWindowInfo{available: true, titles: titles}
	}
	info := inputWindowInfo{available: true}
	for _, w := range tree.Windows {
		info.titles = append(info.titles, w.Title)
	}
	info.windows = tree.Windows
	// Correct the AT-SPI boxes with KWin's true geometries, so
	// the click point-in-box check is on compositor truth, not
	// on window-relative coordinates ~29 px off. A KWin failure
	// leaves the boxes uncorrected and sensitivePrecise false:
	// the click check then denies on any sensitive title
	// instead of trusting an uncorrected box.
	if cur, err := t.readCursor(ctx); err == nil && len(cur.Windows) > 0 {
		offsets := matchKWinWindows(tree, cur.Windows)
		if len(offsets) > 0 {
			applyWindowOffsets(tree, offsets)
			info.windows = tree.Windows
		}
		precise := true
		for i, w := range tree.Windows {
			if !TitleLooksSensitive(w.Title) {
				continue
			}
			if _, ok := offsets[i]; !ok {
				precise = false
				break
			}
		}
		info.sensitivePrecise = precise
	}
	return info
}

// checkTypeSensitive refuses type/key when a sensitive window is
// focused or visible, or when the windows cannot be enumerated
// at all (fail closed — typing blind is the highest-risk op).
// The refusal names the category only, never the title.
func (t *inputTool) checkTypeSensitive(ctx context.Context) error {
	info := t.windowInfo(ctx)
	if !info.available {
		return fmt.Errorf("cannot determine the visible windows (no accessibility tree and no window list): I will not type or press keys blind")
	}
	if hit := FindSensitiveTitle(info.titles); hit != "" {
		return fmt.Errorf("this screen shows a %s: I will not type or press keys into it", SensitiveTitleCategory(hit))
	}
	return nil
}

// checkClickSensitive refuses a click at (x, y) when a sensitive
// window is present — except when every sensitive window's box
// is KWin-corrected and the target lies outside all of them, in
// which case the click is elsewhere on screen. Titles-only
// enumeration, an uncorrected sensitive window, or no
// enumeration at all refuses the click.
func (t *inputTool) checkClickSensitive(ctx context.Context, x, y int) error {
	info := t.windowInfo(ctx)
	if !info.available {
		return fmt.Errorf("cannot determine the visible windows (no accessibility tree and no window list): I will not click blind")
	}
	hit := FindSensitiveTitle(info.titles)
	if hit == "" {
		return nil
	}
	if info.sensitivePrecise {
		for _, w := range info.windows {
			if !TitleLooksSensitive(w.Title) {
				continue
			}
			if x >= w.X && x < w.X+w.W && y >= w.Y && y < w.Y+w.H {
				return fmt.Errorf("the click target (%d, %d) is inside a %s: I will not click there", x, y, SensitiveTitleCategory(w.Title))
			}
		}
		return nil
	}
	return fmt.Errorf("this screen shows a %s: I will not click while it is visible", SensitiveTitleCategory(hit))
}

// checkRateLimit enforces the min-interval, per-turn and per-hour
// limits and records the call. Same shape as the ground and
// screenshot tools' checks, with input's own counters.
func (t *inputTool) checkRateLimit() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if !t.lastCall.IsZero() && now.Sub(t.lastCall) < inputMinInterval {
		return fmt.Errorf("input rate limit: at most one input call per second (last call %v ago)", now.Sub(t.lastCall).Round(time.Millisecond))
	}
	if t.turnCalls >= inputMaxPerTurn {
		return fmt.Errorf("input rate limit: at most %d input calls per turn", inputMaxPerTurn)
	}
	cutoff := now.Add(-time.Hour)
	kept := t.hourCalls[:0]
	for _, ts := range t.hourCalls {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	t.hourCalls = kept
	if len(t.hourCalls) >= inputMaxPerHour {
		return fmt.Errorf("input rate limit: at most %d input calls per hour", inputMaxPerHour)
	}
	t.lastCall = now
	t.turnCalls++
	t.hourCalls = append(t.hourCalls, now)
	return nil
}
