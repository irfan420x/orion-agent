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
// inputMaxMoveIterations times per leg, within
// inputMoveTolerancePx. Long jumps are staged into legs of at
// most inputMoveStagePx (F-A23-W4-2): acceleration distorts
// large deltas far more than small ones, and a ~1,230 px jump
// live never converged in 5 moves while a retry from nearer
// converged in 2.
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
// 15 per turn, 120 per hour) — but the per-turn/hour counting is
// SHARED with ground and screenshot through one CaptureBudget
// (F-A23W2-14 / F-M2R-16): an input call and a capture draw on
// the same 15/turn pool, parent and children together. The
// min interval stays per tool instance.
package tools

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"math"
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
		return newInputToolWithBudget(deps.CaptureBudget, deps.CaptureBudgetOwner), nil
	})
}

// Input limits (M1 screenshot discipline + M3 move bounds). The
// per-turn/hour numbers alias the shared capture budget's
// (F-A23W2-14): one limit across ground + screenshot + input,
// not one per tool. The min interval stays per tool instance.
const (
	inputMinInterval       = time.Second
	inputMaxPerTurn        = captureMaxPerTurn
	inputMaxPerHour        = captureMaxPerHour
	inputMaxTypeRunes      = 1000 // longest text one type call may enter
	inputMoveTolerancePx   = 5    // closed-loop success: both axes within 5 px
	inputMaxMoveIterations = 5    // correction moves per leg before giving up honestly
	// inputMoveStagePx caps one leg of a staged move (F-A23-W4-2):
	// pointer acceleration distorts large relative deltas far
	// more than small ones, so a long jump is split into legs
	// of at most this many pixels, each closed-loop corrected
	// on its own. 400 px keeps a leg well inside the range
	// where a correction converges in 1-2 moves (live: a retry
	// from a nearer start converged in 2 moves, while the
	// ~1,230 px jump never converged in 5).
	inputMoveStagePx    = 400
	inputMoveSettle     = 100 * time.Millisecond
	inputYdotoolTimeout = 10 * time.Second
	inputKWinTimeout    = 5 * time.Second // same as ground's KWin timeout
	inputKWinHelperFile = "input-kwin-helper.py"
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
	// Click verification (F-A23W2-9): the oracle re-read after
	// the click. PostClickCursor is the cursor right after the
	// click (nil when the re-read failed); ClickVerified is true
	// only when that re-read agreed the cursor is still on
	// target AND neither the focus check (the target window
	// never took focus although the focus never moved — the
	// click never reached the window) nor the element check
	// (another control at the click point took focus — the
	// click landed on the wrong control inside the already
	// focused window) proved a miss, AND — the scoped
	// screenshot-diff third signal — the screen visibly changed
	// around the click point, when the focus and element
	// checks were silent (see screenshot_diff.go). A click
	// demoted by the diff alone sets ClickNoVisibleEffect: the
	// press may have landed on a control whose effect is
	// invisible (placing the text caret, for one), so the
	// result says "no visible effect", never "missed".
	// ClickMayHaveLanded is true when the click command
	// itself timed out — the click may already have landed, and
	// Warning says to verify before retrying instead of
	// clicking again blindly.
	PostClickCursor      *cursorPos `json:"post_click_cursor,omitempty"`
	ClickVerified        bool       `json:"click_verified,omitempty"`
	ClickNoVisibleEffect bool       `json:"click_no_visible_effect,omitempty"`
	ClickMayHaveLanded   bool       `json:"click_may_have_landed,omitempty"`
	Warning              string     `json:"warning,omitempty"`
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
// focus is the focused window's identity at enumeration time
// (KWin active window first, AT-SPI focused window second):
// the type/key TOCTOU guard re-checks it right before typing.
// The identity never carries a title out of KWin (F-M2R-15) —
// it is class + frame geometry + the sensitive pattern KWin
// itself matched — and the AT-SPI fallback title inside it is
// compare-only, never logged.
type inputWindowInfo struct {
	available        bool
	titles           []string
	windows          []atspiWindow // AT-SPI boxes, KWin-corrected where matched
	windowsRaw       []atspiWindow // the same boxes BEFORE the KWin correction (nil when no tree)
	windowsListed    bool          // true when an AT-SPI tree enumerated the windows at all
	sensitivePrecise bool          // every sensitive window has a corrected box
	focus            focusIdentity // focused window when enumerated, unknown when neither authority could name it
	// F-M2R-14: content-based sensitive signal — a password or
	// one-time-code field in the visible elements, even when
	// every title is innocuous. contentHit is the refusal
	// category ("" when none); contentWindows are the windows
	// holding a sensitive element (nil when one could not be
	// placed, so the click check must refuse coarsely);
	// contentPrecise is true only when every such window has a
	// KWin-corrected box.
	contentHit     string
	contentWindows map[int]bool
	contentPrecise bool
}

type inputTool struct {
	mu       sync.Mutex
	lastCall time.Time // per-tool min-interval only; turn/hour counting is the shared budget's

	// budget is the shared computer-use budget (F-A23W2-14):
	// ground, screenshot and input charge the same per-turn and
	// per-hour counters. budgetOwner says whether this tool's
	// BeginTurn resets it (parent registry only — a child
	// registry's tools share the budget but never reset it).
	budget      *CaptureBudget
	budgetOwner bool
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
	screenCachedAt   time.Time

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
	// diffCapture, when set, replaces the real spectacle
	// capture for the screenshot-diff signal (F-A23W2-9): the
	// test seam for the before/after images. Nil means the real
	// capture through the embedded screenshot tool.
	diffCapture func(ctx context.Context) (image.Image, error)
}

func newInputTool() *inputTool {
	return newInputToolWithBudget(NewCaptureBudget(), true)
}

// newInputToolWithBudget builds an input tool charging budget
// (nil becomes a fresh private budget). owner says whether this
// tool's BeginTurn resets the budget — true for the parent
// registry's tool, false for a child registry's. The internal
// ground/shot instances keep private budgets: they are only
// ever used for the AT-SPI tree, window titles and the
// screen-locked check — never for a charged capture — so they
// never touch the shared budget.
func newInputToolWithBudget(budget *CaptureBudget, owner bool) *inputTool {
	if budget == nil {
		budget = NewCaptureBudget()
	}
	t := &inputTool{
		budget:      budget,
		budgetOwner: owner,
		ground:      newGroundTool(),
		shot:        newScreenshotTool(),
		lookPath:    exec.LookPath,
		homeDir:     os.UserHomeDir,
		now:         time.Now,
		sleep:       time.Sleep,
	}
	t.ydotool = t.defaultYdotool
	t.runKWinHelper = t.defaultRunKWinHelper
	t.writeKWinHelper = t.defaultWriteKWinHelper
	t.socketPath = defaultYdotoolSocketPath
	return t
}

func (t *inputTool) Name() string { return "input" }

func (t *inputTool) Description() string {
	return "Act on the desktop: move the cursor, click, type text or press a single key (ydotool). move takes absolute {x, y} and corrects itself against the compositor's cursor position until it lands within a few pixels or gives up honestly; click takes {button, clicks} plus either {x, y} or {snapshot_id, element_id} from a ground call (the snapshot form is preferred — it clicks a grounded element, never a guessed coordinate; a coordinate click needs a grounding from the last 60 s, or it is refused — ground first); type takes printable {text} (no control characters — Tab/Return only via key); key takes one key from the allowlist (Return, Escape, Tab, BackSpace, Delete, Home, End, PageUp, PageDown, arrows, Space, F1–F12 — no modifiers or combinations). Gated (destructive): click/type/key can activate, close or submit things. Refuses on sensitive windows (password manager / bank / payment / one-time-code) and in unattended (cron) turns, and when the ydotool daemon is not running."
}

func (t *inputTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"op":          map[string]any{"type": "string", "enum": []string{"move", "click", "type", "key"}, "description": "Which input action to perform."},
				"x":           map[string]any{"type": "integer", "description": "Target x in screen pixels (move, or click by coordinates — a coordinate click needs a grounding from the last 60 s)."},
				"y":           map[string]any{"type": "integer", "description": "Target y in screen pixels (move, or click by coordinates — a coordinate click needs a grounding from the last 60 s)."},
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

// BeginTurn resets the per-turn call counter (see TurnResetter)
// — but only when this tool owns the shared budget (F-A23W2-14):
// a child registry's tools share the budget and must never
// reset it mid-parent-turn. Resetting is idempotent, so the
// parent registry's ground, screenshot and input tools may all
// call it.
func (t *inputTool) BeginTurn() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.budgetOwner {
		t.budget.BeginTurn()
	}
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
//
// F-A23W2-18: a call charged by checkRateLimit and then REFUSED
// (denied) — the sensitive-window checks, the focus re-check —
// performed no input, so the slot is refunded on the way out
// (see the defer below). A FAILED call keeps its slot: it
// attempted input, and a timed-out click or a half-moved cursor
// may already have taken effect — refunding that would invite
// a retry into a double-click. The min-interval lastCall stamp
// is separate and is never refunded: it throttles attempts,
// not quota, so a refused call still counts as the last call.
func (t *inputTool) ExecuteResult(ctx context.Context, args json.RawMessage) (result ToolResult, err error) {
	invalid := func(format string, v ...any) (ToolResult, error) {
		return ToolResult{}, &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf(format, v...)}
	}
	denied := func(format string, v ...any) (ToolResult, error) {
		return ToolResult{}, &ToolError{Tool: t.Name(), Kind: "denied", Err: fmt.Errorf(format, v...)}
	}
	failed := func(format string, v ...any) (ToolResult, error) {
		return ToolResult{}, &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf(format, v...)}
	}

	// Refund the budget slot when a charged call is refused
	// after the rate check. Only Kind "denied" refunds: the
	// rate-limit denial itself returns before charged is set,
	// and invalid/failed returns never reach this with a
	// charged slot they should give back (invalid returns
	// before the charge; failed calls attempted input).
	charged := false
	defer func() {
		if !charged {
			return
		}
		var te *ToolError
		if errors.As(err, &te) && te.Kind == "denied" {
			t.budget.Refund()
		}
	}()

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
			// A coordinate is only meaningful against a recent
			// grounding (F-A23W2-8): without one the model is
			// clicking a remembered number from an old screen,
			// with no TTL, supersession or eviction applying.
			// Checked again in doClick right before the click —
			// the sensitive check and the move take seconds.
			if err := requireFreshGrounding(); err != nil {
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

	// Rate limits (M1 discipline): min 1s between calls, max 15
	// per turn, max 120 per hour. The check runs AFTER the
	// screen-locked and daemon checks (F-A23W2-18): a call that
	// fails there performed no input, so it must not eat a
	// per-turn or per-hour slot — otherwise a locked screen or
	// a down daemon burns the whole quota on failures. Argument
	// errors above never reach this check either, for the same
	// reason: only a call that is about to act consumes a slot.
	// A call charged here and then REFUSED (denied) by the
	// sensitive/focus checks below gets its slot back from the
	// defer at the top of ExecuteResult — a refusal performed
	// no input, so it must not burn quota either.
	if err := t.checkRateLimit(); err != nil {
		return denied("%v", err)
	}
	charged = true

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
		if err := t.checkClickSensitive(ctx, target.X, target.Y, a.SnapshotID, a.ElementID); err != nil {
			return denied("%v", err)
		}
		out, err := t.doClick(ctx, button, clicks, a.SnapshotID, a.ElementID, *target)
		if err != nil {
			return failed("%v", err)
		}
		summary.Target = target
		summary.Final = &out.final
		summary.Iterations = out.iters
		summary.Reached = true
		summary.Button = button
		summary.Clicks = clicks
		summary.PostClickCursor = out.postClick
		summary.ClickVerified = out.verified
		summary.ClickNoVisibleEffect = out.noVisibleEffect
		if out.uncertain {
			// The click command timed out: the click may
			// already have landed. This is a result, not an
			// error — an error would make the model retry and
			// double-click.
			summary.ClickMayHaveLanded = true
			summary.Warning = "the click command timed out — the click may already have landed. Verify on screen before retrying; do not click again blindly."
		} else if out.postClick == nil {
			summary.Warning = "the click was sent but the post-click check could not run (KWin oracle unavailable): verify on screen before retrying."
		} else if out.focusMiss {
			summary.Warning = "the click was sent and the cursor is on the target, but the target window never took focus — the click may not have landed on it. Verify on screen before retrying."
		} else if out.elementMiss {
			summary.Warning = "the click was sent and the cursor is on the target, but another control at the click point took focus — the click may have landed on the wrong control. Verify on screen before retrying."
		} else if out.noVisibleEffect {
			summary.Warning = "the click was sent and the cursor is on the target, but nothing on screen changed around the click point — the click may have had no visible effect. Verify on screen before retrying; do not click again blindly."
		} else if !out.verified {
			summary.Warning = "the click was sent but the cursor is no longer on the target — verify on screen before retrying."
		}
		if a.SnapshotID != "" {
			summary.Element = &inputElement{SnapshotID: a.SnapshotID, ElementID: *a.ElementID}
		}
	case "type":
		info, err := t.checkTypeSensitive(ctx)
		if err != nil {
			return denied("%v", err)
		}
		// The focus must be the window just checked: re-check
		// it right before the keystrokes (F-A23W2-10).
		if err := t.recheckFocus(ctx, info.focus); err != nil {
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
		info, err := t.checkTypeSensitive(ctx)
		if err != nil {
			return denied("%v", err)
		}
		if err := t.recheckFocus(ctx, info.focus); err != nil {
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
		// A timeout (or a cancelled step) is NOT a plain
		// failure: the command may already have taken effect
		// when the deadline fired. Wrap the context error so
		// the caller can tell the two apart (F-A23W2-9) — a
		// timed-out click must report "may have landed",
		// never a retryable error.
		if cctx.Err() != nil {
			return buf.String(), fmt.Errorf("ydotool %s timed out: %w (output: %s)", args[0], cctx.Err(), strings.TrimSpace(buf.String()))
		}
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
// tolerance issues no ydotool call at all.
//
// Long jumps are staged (F-A23-W4-2): the path is split into
// legs of at most inputMoveStagePx, each corrected on its own.
// One long jump does not converge — live, 5 correction moves
// from (1655,176) to (744,998) left the cursor stuck at
// (970,1023), while a retry from a nearer start converged in 2
// moves — because pointer acceleration distorts large relative
// deltas far more than small ones. A leg that does not converge
// fails the move honestly (with the final oracle position) and
// NEVER claims success: the caller reports the failure, the
// model re-grounds or retries, and no click ever follows a
// failed move.
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
	start := cur.Cursor
	dx, dy := x-start.X, y-start.Y
	legs := int(math.Ceil(math.Hypot(float64(dx), float64(dy)) / inputMoveStagePx))
	if legs < 1 {
		legs = 1
	}
	total := 0
	pos := start
	for leg := 1; leg <= legs; leg++ {
		// Waypoint leg/legs along the straight start->target
		// line; the last waypoint is the exact target (the
		// float arithmetic lands on it, and Round keeps it).
		wx := int(math.Round(float64(start.X) + float64(dx)*float64(leg)/float64(legs)))
		wy := int(math.Round(float64(start.Y) + float64(dy)*float64(leg)/float64(legs)))
		var iters int
		pos, iters, err = t.correctTo(ctx, wx, wy, pos)
		total += iters
		if err != nil {
			return pos, total, fmt.Errorf("could not reach (%d, %d) within %d px: leg %d of %d failed: %v", x, y, inputMoveTolerancePx, leg, legs, err)
		}
	}
	return pos, total, nil
}

// correctTo corrects the cursor onto one waypoint (tx, ty) from
// the oracle-confirmed position cur, issuing at most
// inputMaxMoveIterations relative ydotool moves. Each move sends
// the exact residual as a RELATIVE move: exact arithmetic from
// the oracle's known start. --absolute is not used: on a
// relative-only device it is relative deltas from a start
// position ydotool cannot know. A waypoint that does not
// converge within the iteration budget returns an error that
// says so — with the final oracle position — and NEVER claims
// success.
func (t *inputTool) correctTo(ctx context.Context, tx, ty int, cur cursorPos) (cursorPos, int, error) {
	if withinTolerance(cur, tx, ty) {
		return cur, 0, nil
	}
	for i := 1; i <= inputMaxMoveIterations; i++ {
		dx, dy := tx-cur.X, ty-cur.Y
		if _, err := t.ydotool(ctx, "", "mousemove", "-x", strconv.Itoa(dx), "-y", strconv.Itoa(dy)); err != nil {
			return cursorPos{}, i, fmt.Errorf("ydotool mousemove failed: %v", err)
		}
		// Let the compositor apply the motion before the
		// oracle re-reads, or the loop corrects a stale
		// position and oscillates.
		t.sleep(inputMoveSettle)
		out, err := t.readCursor(ctx)
		if err != nil {
			return cursorPos{}, i, err
		}
		cur = out.Cursor
		if withinTolerance(cur, tx, ty) {
			return cur, i, nil
		}
	}
	return cur, inputMaxMoveIterations,
		fmt.Errorf("could not reach (%d, %d) within %d px after %d correction moves: the cursor is at (%d, %d) — the screen may have moved, or pointer acceleration is fighting the correction; ground again and retry from nearer the target", tx, ty, inputMoveTolerancePx, inputMaxMoveIterations, cur.X, cur.Y)
}

// clickOutcome is what a click returns: the oracle-confirmed
// cursor after the move, the correction moves issued, and the
// post-click verification (F-A23W2-9). Uncertain is true when
// the click command itself timed out: the click may already
// have landed, and the caller must say so instead of returning
// a retryable error. focusMiss is true when the cursor is on
// target but the focus check positively showed the click never
// reached the target window (see focusProvesMiss) — the
// warning then says so instead of blaming the cursor.
// elementMiss is true when the cursor is on target but the
// element check positively showed the click landed on a
// different control inside the already-focused window (see
// elementProvesMiss).
type clickOutcome struct {
	final       cursorPos
	iters       int
	postClick   *cursorPos // oracle cursor right after the click, nil when the re-read failed
	verified    bool       // the post-click oracle agreed the cursor is still on target AND neither check proved a miss
	uncertain   bool       // the click command timed out: it may have landed
	focusMiss   bool       // the click never reached the target window (focus never moved)
	elementMiss bool       // the click landed on a different control in the focused window (focus moved to it)
	// noVisibleEffect is the screenshot-diff demotion
	// (F-A23W2-9): the focus and element checks were silent
	// and nothing on screen changed around the click point.
	// Distinct from a miss — the press may have landed on a
	// control whose effect is invisible.
	noVisibleEffect bool
}

// doClick moves to the target (closed-loop — a failed move
// aborts before any click, so a click never lands in the wrong
// place), clicks, and then verifies with an oracle re-read.
// For a snapshot click the element is re-validated fresh
// immediately before the click: the registry check alone
// (TTL/id) cannot see that the element moved since grounding,
// and a stale centre can land on a different control.
// A coordinate click re-checks the freshness gate instead (the
// grounding may have gone stale during the sensitive check and
// the move).
//
// Verification is three signals, in order: the cursor re-read,
// the focus check, the element check — and, when all three are
// silent (verified, no miss proven, and the target did not
// itself take focus), the screenshot-diff third signal
// (F-A23W2-9): a crop around the target before the click is
// compared with the same crop after it, and a click that
// changed nothing visible demotes with the distinct reason "no
// visible effect". A click that opened a new window skips the
// diff (the new window is itself the effect). Any failure of
// the diff signal is no signal: the cursor verdict stands.
//
// Post-click verification (F-A23W2-9) is the cursor AND the
// focused window, not the cursor alone: the move already put
// the cursor on target, so a re-read that agrees only re-proves
// the move — a click that never landed still reports verified.
// The check that can actually catch a missed click is the
// focus: a button press that reaches an unfocused window
// focuses it (KWin click-to-focus). The focus is therefore read
// just before the click and again right after (the same oracle
// reading that carries the post-click cursor); when the focus
// never moved off a different window than the target's, the
// press demonstrably never reached the target window and the
// click is NOT verified, even though the cursor sits on target.
// Every other outcome leaves the cursor verdict standing: a
// click on the already-focused window need not move the focus,
// a landed click may open a dialog that takes the focus
// instead, and when KWin names no active window (or no window
// contains the target) there is no focus signal to judge by —
// so a click that legitimately changes nothing visible still
// verifies on the cursor. The focus check can only demote on
// positive evidence of a miss, never on the absence of a
// visible effect.
//
// The focus check cannot see inside the already-focused
// window, so a snapshot click gets a second check after it:
// the element re-check (elementProvesMiss). A press gives
// keyboard focus to the control it lands on, so when the
// focused control AFTER the click is a different, pre-existing
// control sitting on the click point — while the target sits
// there unchanged and the window never changed — the press
// demonstrably landed on that other control, not the target.
// Geometry alone could not prove this (the flat AT-SPI list
// carries no z-order: two overlapping elements at the click
// point say nothing about which one received the press); the
// focused element is the disambiguator, and it comes from the
// helper's per-element FOCUSED state, read off the same state
// set the helper already queries for SHOWING. Coordinate
// clicks have no element identity and get no element re-check.
//
// What the element re-check still cannot catch, stated
// plainly: a miss onto a control that was ALREADY focused
// before the click (the focus does not move, so it is
// indistinguishable from a landed click on a control that
// never takes focus); a miss onto a non-focusable control or
// area (no focus signal is produced at all); a miss where the
// target moved or vanished (that may be the click's own
// effect); and any click whose toolkit never reports element
// focus — then the check silently never fires, and the cursor
// verdict stands. In every one of those cases the click still
// verifies on the cursor rather than false-negative: the
// element check, like the focus check, can only demote on
// positive evidence of a miss.
func (t *inputTool) doClick(ctx context.Context, button string, clicks int, snapshotID string, elementID *int, target cursorPos) (clickOutcome, error) {
	final, iters, err := t.doMove(ctx, target.X, target.Y)
	if err != nil {
		return clickOutcome{}, err
	}
	// The pre-click element state, for the post-click element
	// re-check below: the revalidation walk already read the
	// fresh tree, so capturing the state here costs no second
	// walk. Nil for coordinate clicks (no element identity)
	// and for snapshot entries without one.
	var pre *preClickElementState
	if snapshotID != "" && elementID != nil {
		var err error
		pre, err = t.revalidateSnapshotElement(ctx, snapshotID, *elementID, final)
		if err != nil {
			return clickOutcome{}, err
		}
	} else if err := requireFreshGrounding(); err != nil {
		// Coordinate click: the grounding may have gone stale
		// during the sensitive check and the move.
		return clickOutcome{}, err
	}
	// The focus just before the click, for the post-click
	// verification below: one cheap KWin read. A failed read
	// means no focus signal — the cursor verdict stands on its
	// own (the post-click read failing already reports the
	// click unverified; a pre-click failure alone must not).
	var beforeWindows []kwinWindow
	var beforeFocus *focusIdentity
	if cur, err := t.readCursor(ctx); err == nil {
		beforeWindows = cur.Windows
		if w := kwinActiveWindow(cur.Windows); w != nil {
			f := kwinFocusIdentity(*w)
			beforeFocus = &f
		}
	}
	// The before-capture for the screenshot-diff third signal
	// (F-A23W2-9): taken after every pre-click gate has passed
	// and immediately before the click, so the crop shows the
	// screen the press is about to land on. A failed capture
	// is no signal — tryDiffCapture returns nil and the click
	// proceeds exactly as before, unverified by the diff.
	// beforeAt stamps the capture for the min-interval wait
	// before the after-capture.
	beforeAt := t.now()
	beforeImg := t.tryDiffCapture(ctx)
	code := inputClickCodes[button]
	if _, err := t.ydotool(ctx, "", "click", "--repeat", strconv.Itoa(clicks), fmt.Sprintf("0x%02X", code)); err != nil {
		// The click command timed out (or the step was
		// cancelled) mid-click: the click may already have
		// landed. Report that, never a plain failure — a
		// failure makes the model retry into a double-click
		// (double-submit / double-purchase).
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return clickOutcome{final: final, iters: iters, uncertain: true}, nil
		}
		return clickOutcome{}, fmt.Errorf("ydotool click failed: %v", err)
	}
	// Post-click verification: the oracle re-reads the cursor
	// right after the click. A click is only verified when the
	// re-read succeeds and the cursor is still on target — and
	// neither the focus check nor the element check proves a
	// miss. A failed re-read does not fail the click — ydotool
	// already reported success — but the result says so, so
	// the model verifies on screen before retrying.
	// clickAt stamps the click for the diff's settle delay.
	clickAt := t.now()
	out := clickOutcome{final: final, iters: iters}
	// elementHit is set by the element re-check below when the
	// target itself took focus (positive hit evidence — the
	// diff must not run then).
	elementHit := false
	if post, err := t.readCursor(ctx); err == nil {
		out.postClick = &post.Cursor
		out.verified = withinTolerance(post.Cursor, target.X, target.Y)
		if out.verified && focusProvesMiss(beforeFocus, beforeWindows, post.Windows, target) {
			// The cursor is on target but the click never
			// reached the target window: the focus never
			// moved off the window that was active before
			// the click.
			out.verified = false
			out.focusMiss = true
		}
		if out.verified && pre != nil && sameWindowFocus(beforeFocus, post.Windows) {
			// Snapshot click, cursor on target, window
			// unchanged: the element re-check runs one
			// more AT-SPI walk and demotes only when
			// another control demonstrably took the press
			// (see elementProvesMiss). A failed walk is
			// no signal, not a miss — the cursor verdict
			// stands. The re-check also reports a HIT —
			// the target itself took focus — which is
			// positive evidence the press reached the
			// control, not silence: the diff below must
			// not second-guess it.
			switch t.postClickElementCheck(ctx, pre, post.Windows, post.Cursor) {
			case elementCheckMiss:
				out.verified = false
				out.elementMiss = true
			case elementCheckHit:
				elementHit = true
			}
		}
		if out.verified && beforeImg != nil && !elementHit && !focusMovedToNewWindow(beforeWindows, post.Windows) {
			// Screenshot-diff third signal (F-A23W2-9):
			// cursor, focus and element checks all silent,
			// the target did not take focus, and the click
			// opened no new window — compare the crop
			// around the target before and after the click.
			// The after-capture waits out the settle delay
			// and the 1 s min interval since the
			// before-capture (the interval subsumes the
			// settle; on snapshot clicks the AT-SPI walk
			// above usually already spent it). A failed
			// after-capture is no signal, not a miss.
			wait := time.Until(beforeAt.Add(inputMinInterval))
			if clickSettle := time.Until(clickAt.Add(diffSettle)); clickSettle > wait {
				wait = clickSettle
			}
			if wait > 0 {
				t.sleep(wait)
			}
			if afterImg := t.tryDiffCapture(ctx); afterImg != nil {
				if visible, _, err := diffVisibleChange(beforeImg, afterImg, target); err == nil && !visible {
					out.verified = false
					out.noVisibleEffect = true
				}
			}
		}
	}
	return out, nil
}

// focusProvesMiss reports whether the pre/post-click focus
// readings positively show the click never reached the target
// window: KWin named an active window before and after the
// click, it is the SAME window in both, and the target point
// lies inside a non-minimized KWin window that is NOT the
// focused one. A button press that reaches an unfocused window
// focuses it — when the focus never moved, the press never
// reached the window. Any unknown is no signal, not a miss:
// no active window before or after, or no window containing
// the target (the click may be on the desktop or on a window
// KWin does not list). Overlapping windows are judged
// conservatively: when ANY window containing the target is the
// focused one, there is no demotion — the press may have
// landed on the focused window underneath an inactive one on
// top, and the check must not false-negative that. Minimized
// windows are skipped: a minimized window cannot receive the
// press, so it is never the target window.
func focusProvesMiss(before *focusIdentity, beforeWindows []kwinWindow, afterWindows []kwinWindow, target cursorPos) bool {
	if before == nil || !before.known || !before.kwin {
		return false
	}
	after := kwinActiveWindow(afterWindows)
	if after == nil {
		return false
	}
	if !before.equal(kwinFocusIdentity(*after)) {
		return false // the focus moved: the click (or its effect) moved it
	}
	// The focus did not move. Demote only when the target
	// point is inside some visible window other than the
	// focused one.
	targetInWindow := false
	for _, w := range beforeWindows {
		if w.Minimized {
			continue
		}
		if target.X >= w.FG[0] && target.X < w.FG[0]+w.FG[2] && target.Y >= w.FG[1] && target.Y < w.FG[1]+w.FG[3] {
			targetInWindow = true
			if w.Class == before.class && w.FG == before.fg {
				return false // the target is in the focused window: no signal
			}
		}
	}
	return targetInWindow
}

// requireFreshGrounding is the F-A23W2-8 gate for coordinate
// clicks: a coordinate click is refused unless the newest
// grounding is still within its 60 s TTL. The snapshot form is
// preferred — it clicks a grounded element, never a guessed
// coordinate — but coordinates stay available for targets the
// grounding could not list (the W4 live click was one), as long
// as the screen they came from is still fresh.
func requireFreshGrounding() error {
	if !groundSnapshots.latestFresh(time.Now()) {
		return fmt.Errorf("coordinate click needs a grounding from the last %v: ground first, then click — or click by snapshot_id + element_id", groundSnapshotTTL)
	}
	return nil
}

// revalidateSnapshotElement re-reads the live accessibility tree
// right before a snapshot click and checks the element is still
// at the cursor: an element with the grounded role and name must
// be present with its centre within tolerance of the
// oracle-confirmed cursor position (the position the click is
// about to land on). The check runs after the move, so it covers
// the whole gap since grounding — the page scrolling, loading or
// reflowing, or the window moving, in between. A moved, vanished
// or unverifiable element refuses the click: the provider must
// ground again. The fresh boxes get the same KWin correction the
// grounding applied, or a ~29 px Wayland shift would refuse
// every click; a KWin failure therefore refuses too, rather than
// comparing uncorrected boxes against corrected centres.
//
// Cost, stated plainly: one more AT-SPI walk (~5 s on a heavy
// page) per snapshot click, on top of the sensitive check's
// walk. That is the price of clicking a live element instead of
// a remembered coordinate.
//
// On success it also returns the pre-click element state for
// the post-click element re-check (F-A23W2-9 follow-up): the
// corrected fresh tree, the matched target, and which control
// held focus just before the press. Capturing it here costs no
// second walk — this is the same tree the re-check just read.
func (t *inputTool) revalidateSnapshotElement(ctx context.Context, snapshotID string, elementID int, cursor cursorPos) (*preClickElementState, error) {
	snap, err := groundSnapshots.lookup(snapshotID, chatIDFromContext(ctx), elementID, time.Now())
	if err != nil {
		return nil, fmt.Errorf("snapshot went stale before the click: %v (ground again)", err)
	}
	want := snap.details[elementID]
	// No recorded identity, nothing to re-check. The re-check
	// matches a fresh element by the grounded role + name, and
	// a snapshot entry that records neither has no identity to
	// match — only a minimal registration can do that: a real
	// grounding always records both (register() normalises an
	// empty name to the role), so this path is unreachable for
	// real snapshots and weakens nothing in F-A23W2-1. The
	// remaining gates still apply: the lookup above enforced
	// TTL/supersession/chat/membership, the geometry binding
	// ran in the sensitive check when a window was recorded,
	// and a snapshot whose element HAS an identity always
	// goes through the fresh-tree match below.
	if want.Role == "" && want.Name == "" {
		return nil, nil
	}
	tree, _, err := t.ground.atspiTree(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot re-check element %d (%s %q) before the click: the accessibility tree failed (%v) — ground again", elementID, want.Role, want.Name, err)
	}
	if tree == nil {
		return nil, fmt.Errorf("cannot re-check element %d (%s %q) before the click: no accessibility tree — ground again", elementID, want.Role, want.Name)
	}
	cur, err := t.readCursor(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot re-check element %d (%s %q) before the click: the KWin oracle failed (%v) — ground again", elementID, want.Role, want.Name, err)
	}
	if len(cur.Windows) > 0 {
		applyWindowOffsets(tree, matchKWinWindows(tree, cur.Windows))
	}
	// The role set is the snapshot's own roles: the grounding
	// may have used a custom roles filter, and the fresh walk
	// must see the same elements it saw.
	roleSet := make(map[string]bool, len(snap.details))
	for _, e := range snap.details {
		roleSet[e.Role] = true
	}
	// The re-check's view is the full screen (F-M2R-21): the
	// oracle's screen size bounds the off-screen-centre drop,
	// and a minimized window's elements are dropped — a click
	// target in a minimized window is not on screen. A missing
	// screen size disables the centre check (0x0 view).
	viewW, viewH := 0, 0
	if cur.Screen != nil {
		viewW, viewH = cur.Screen.W, cur.Screen.H
	}
	minimized := kwinMinimizedWindowIndexes(tree, cur.Windows)
	fresh, _, _, _ := filterGroundElements(tree, roleSet, nil, groundMaxMaxElements, 0, 0, viewW, viewH, minimized)
	var moved *atspiElement // same role+name, centre elsewhere
	for i := range fresh {
		el := &fresh[i]
		if el.Role != want.Role || el.Name != want.Name {
			continue
		}
		cx, cy := el.X+el.W/2, el.Y+el.H/2
		if withinTolerance(cursorPos{X: cx, Y: cy}, cursor.X, cursor.Y) {
			// Still at the cursor: click — and hand the
			// post-click check the state it needs: the
			// corrected fresh tree (any role — the
			// control that takes focus need not share
			// the target's role), the matched target,
			// and the control that held focus before
			// the press.
			view := elementCheckView(tree, minimized)
			focused, ambiguous := focusedControl(view)
			return &preClickElementState{
				elements:       view,
				target:         *el,
				focused:        focused,
				focusAmbiguous: ambiguous,
			}, nil
		}
		if moved == nil {
			moved = el
		}
	}
	if moved != nil {
		return nil, fmt.Errorf("element %d (%s %q) has moved since grounding: it was at (%d, %d), now at (%d, %d) — the click target is stale: ground again", elementID, want.Role, want.Name, want.Centre.X, want.Centre.Y, moved.X+moved.W/2, moved.Y+moved.H/2)
	}
	return nil, fmt.Errorf("element %d (%s %q) is no longer on screen — ground again and pick an element from the new list", elementID, want.Role, want.Name)
}

// preClickElementState is what the post-click element re-check
// needs from the pre-click revalidation walk: the corrected
// fresh tree (every role — the control that takes focus need
// not share the target's role), the matched target, and the
// control that held focus just before the press.
type preClickElementState struct {
	// elements is the corrected pre-click tree, visibility
	// filtered (hidden, degenerate and minimized-window
	// elements removed): the set a post-click focus-taker
	// must already be in, so a popup the click itself
	// opened can never demote.
	elements []atspiElement
	// target is the fresh element the pre-click revalidation
	// matched to the snapshot element.
	target atspiElement
	// focused is the unique control holding focus just before
	// the click, or nil when none held focus;
	// focusAmbiguous is true when more than one control
	// reported focused (the signal is muddy: no demotion).
	focused        *atspiElement
	focusAmbiguous bool
}

// elementBoxTolerance is how far a post-click box may differ
// from the pre-click box per coordinate and still count as the
// same element in place (a pixel of KWin-correction jitter
// between the two walks must not read as movement).
const elementBoxTolerance = 2

// elementCheckView is the corrected, visibility-filtered view of
// a tree for the post-click element check: every element the
// check may reason about, regardless of role (the focused
// control can be any role). Hidden, degenerate and
// minimized-window elements are removed, like the grounding
// filter — a hidden node keeps stale state and can never have
// taken focus from the press. Names are normalised exactly as
// the grounding filter does (empty → role, over-long truncated),
// so pre- and post-click elements compare equal.
func elementCheckView(tree *atspiOutput, minimized map[int]bool) []atspiElement {
	if tree == nil {
		return nil
	}
	view := make([]atspiElement, 0, len(tree.Elements))
	for _, el := range tree.Elements {
		if el.Showing != nil && !*el.Showing {
			continue
		}
		if el.Win != nil && minimized[*el.Win] {
			continue
		}
		if el.W <= 1 || el.H <= 1 || el.X < 0 || el.Y < 0 {
			continue
		}
		if strings.TrimSpace(el.Name) == "" {
			el.Name = el.Role
		}
		if runes := []rune(el.Name); len(runes) > groundMaxNameRunes {
			el.Name = string(runes[:groundMaxNameRunes-1]) + "…"
		}
		view = append(view, el)
	}
	return view
}

// focusedControl returns the unique control holding focus in
// the view, or nil when none does; ambiguous is true when more
// than one control reports focused (stale flags — the signal
// is muddy and must never demote). The window frame itself is
// skipped: it reports FOCUSED whenever its window is active,
// which duplicates the KWin window signal the focus check
// already reads — the control that took the press is what
// matters here.
func focusedControl(view []atspiElement) (focused *atspiElement, ambiguous bool) {
	for i := range view {
		el := &view[i]
		if el.Role == "frame" {
			continue
		}
		if el.Focused == nil || !*el.Focused {
			continue
		}
		if focused != nil {
			return nil, true
		}
		focused = el
	}
	return focused, false
}

// sameWindowFocus reports whether the KWin active window is
// the same before and after the click (and known on both
// sides): the element re-check only runs when the window never
// changed — a landed click that opens a dialog or navigates
// moves the focus as its own effect, and that must never
// demote.
func sameWindowFocus(before *focusIdentity, afterWindows []kwinWindow) bool {
	if before == nil || !before.known || !before.kwin {
		return false
	}
	after := kwinActiveWindow(afterWindows)
	if after == nil {
		return false
	}
	return before.equal(kwinFocusIdentity(*after))
}

// elementCheck is the post-click element re-check's verdict:
// silent (no signal either way), miss (another control
// demonstrably took the press), or hit (the target itself took
// focus — positive evidence the press reached the control, not
// silence: the screenshot-diff third signal must not run on a
// hit, or it would demote a click-to-focus whose effect is
// invisible in a screenshot by construction).
type elementCheck int

const (
	elementCheckSilent elementCheck = iota
	elementCheckMiss
	elementCheckHit
)

// postClickElementCheck runs the post-click element re-check:
// one more AT-SPI walk, KWin-corrected with the post-click
// windows, judged by elementProvesMiss against the pre-click
// state — and, when that proves no miss, by whether the target
// itself took focus. A failed or empty walk is silent, never a
// miss or a hit — the cursor verdict stands. The hit needs the
// same guards as the miss side: the target still there
// unchanged, exactly one focused control, it IS the target, and
// the focus MOVED to it (it was not already focused before the
// press — an already-focused target is no movement, no signal).
func (t *inputTool) postClickElementCheck(ctx context.Context, pre *preClickElementState, postWindows []kwinWindow, at cursorPos) elementCheck {
	tree, _, err := t.ground.atspiTree(ctx)
	if err != nil || tree == nil {
		return elementCheckSilent
	}
	if len(postWindows) > 0 {
		applyWindowOffsets(tree, matchKWinWindows(tree, postWindows))
	}
	post := elementCheckView(tree, kwinMinimizedWindowIndexes(tree, postWindows))
	if elementProvesMiss(pre, post, at) {
		return elementCheckMiss
	}
	if _, ok := findTarget(post, pre.target, at); !ok {
		return elementCheckSilent
	}
	focused, ambiguous := focusedControl(post)
	if ambiguous || focused == nil {
		return elementCheckSilent
	}
	if !sameElement(*focused, pre.target) {
		return elementCheckSilent
	}
	if pre.focusAmbiguous {
		return elementCheckSilent
	}
	if pre.focused != nil && sameElement(*pre.focused, *focused) {
		return elementCheckSilent
	}
	return elementCheckHit
}

// focusMovedToNewWindow reports whether the post-click active
// window is NEW — absent from the pre-click window list. A
// click that opened a dialog or a new window has already
// proven its effect by opening it, so the screenshot-diff must
// not judge it from a crop in the old window. Identity is the
// same class + frame geometry the focus check trusts. An
// unknown pre-click list (the pre-click read failed) makes
// every post-click window look new, so the diff is skipped —
// an unknown is no signal, and skipping fails safe (the
// verdict stands) where running would false-demote a
// dialog-opening click.
func focusMovedToNewWindow(before, after []kwinWindow) bool {
	active := kwinActiveWindow(after)
	if active == nil {
		return false
	}
	id := kwinFocusIdentity(*active)
	for _, w := range before {
		if kwinFocusIdentity(w).equal(id) {
			return false
		}
	}
	return true
}

// tryDiffCapture charges one shared-budget slot and captures
// the screen for the screenshot-diff signal. It never errors:
// a refused charge (budget exhausted) or a failed capture is
// NO signal — the click verdict stands — so the caller gets
// nil and carries on. A charge whose capture then produced no
// image is refunded: nothing was captured, so no quota was
// spent (the F-A23W2-18 rule: a call refused before acting
// must not burn quota).
func (t *inputTool) tryDiffCapture(ctx context.Context) image.Image {
	if err := t.budget.Charge(t.now(), inputMaxPerTurn, inputMaxPerHour); err != nil {
		return nil
	}
	img, err := t.diffCaptureImage(ctx)
	if err != nil || img == nil {
		t.budget.Refund()
		return nil
	}
	return img
}

// diffCaptureImage is the diff capture itself: the diffCapture
// seam when a test sets it, else the real spectacle capture
// through the embedded screenshot tool (in-memory only — the
// image is compared and discarded, never stored or sent).
func (t *inputTool) diffCaptureImage(ctx context.Context) (image.Image, error) {
	if t.diffCapture != nil {
		return t.diffCapture(ctx)
	}
	return t.shot.captureScreen(ctx)
}

// elementProvesMiss reports whether the post-click tree
// positively shows a snapshot click landed on a different
// control than its target — the F-A23W2-9 element re-check.
// A press gives keyboard focus to the control it lands on, so
// the demotion needs ALL of these at once:
//
//   - the target is still there, unchanged (same role+name,
//     centre on the click point, box unchanged): a moved or
//     vanished target may be the click's own effect;
//   - exactly one control holds focus after the click, and it
//     is not the target;
//   - that control's box contains the click point, in the same
//     window as the target;
//   - neither box contains the other: a focusable container
//     taking focus instead of its child is not a miss;
//   - the control pre-existed with the same box: a popup or
//     menu item the click itself opened is not a miss;
//   - the focus MOVED to it: the pre-click focus was nothing,
//     or a different control — a control that was already
//     focused before the press is indistinguishable from a
//     landed click on a control that never takes focus.
//
// Any unknown is no signal, not a miss: no focused control,
// several focused controls, an ambiguous pre-click focus, or
// a missing tree all leave the cursor verdict standing. The
// check therefore never demotes a click that legitimately
// changes nothing visible, and never demotes on the absence of
// a visible effect — only on a focus movement that positively
// places the press on another control.
func elementProvesMiss(pre *preClickElementState, post []atspiElement, at cursorPos) bool {
	if pre == nil {
		return false
	}
	// The target must still be there, unchanged.
	target, ok := findTarget(post, pre.target, at)
	if !ok {
		return false
	}
	// Exactly one control holds focus after the click.
	focused, ambiguous := focusedControl(post)
	if ambiguous || focused == nil {
		return false
	}
	b := *focused
	// ... and it is not the target itself.
	if sameElement(b, pre.target) {
		return false
	}
	// ... its box contains the click point ...
	if !containsPoint(b, at) {
		return false
	}
	// ... in the same window as the target ...
	if b.Win != nil && target.Win != nil && *b.Win != *target.Win {
		return false
	}
	// ... and it is neither a container of the target nor a
	// child of it.
	if boxContains(b, pre.target) || boxContains(pre.target, b) {
		return false
	}
	// ... it pre-existed with the same box (not a popup the
	// click itself opened) ...
	if !hasSameBox(pre.elements, b) {
		return false
	}
	// ... and the focus moved to it.
	if pre.focusAmbiguous {
		return false
	}
	if pre.focused != nil && sameElement(*pre.focused, b) {
		return false // already focused: no movement, no signal
	}
	return true
}

// findTarget finds the target in the post-click view: an
// element with the target's role+name whose centre is on the
// click point and whose box is unchanged. A same-named element
// with a changed box is not the target in place — it may have
// moved as the click's own effect — so it does not match.
func findTarget(post []atspiElement, want atspiElement, at cursorPos) (atspiElement, bool) {
	for _, el := range post {
		if el.Role != want.Role || el.Name != want.Name {
			continue
		}
		if !withinTolerance(cursorPos{X: el.X + el.W/2, Y: el.Y + el.H/2}, at.X, at.Y) {
			continue
		}
		if !sameBox(el, want) {
			continue
		}
		return el, true
	}
	return atspiElement{}, false
}

// sameBox reports whether two boxes agree within
// elementBoxTolerance per coordinate (a pixel of
// KWin-correction jitter between the two walks is not
// movement).
func sameBox(a, b atspiElement) bool {
	diff := func(x, y int) int {
		if x > y {
			return x - y
		}
		return y - x
	}
	return diff(a.X, b.X) <= elementBoxTolerance &&
		diff(a.Y, b.Y) <= elementBoxTolerance &&
		diff(a.W, b.W) <= elementBoxTolerance &&
		diff(a.H, b.H) <= elementBoxTolerance
}

// sameElement reports whether two elements are the same
// control: same role+name and the same box.
func sameElement(a, b atspiElement) bool {
	return a.Role == b.Role && a.Name == b.Name && sameBox(a, b)
}

// hasSameBox reports whether the view holds an element with
// the same role+name and box (the pre-existence check for a
// post-click focus-taker).
func hasSameBox(view []atspiElement, want atspiElement) bool {
	for _, el := range view {
		if sameElement(el, want) {
			return true
		}
	}
	return false
}

// containsPoint reports whether the point lies inside the
// element's box (half-open, like the window containment in
// focusProvesMiss).
func containsPoint(el atspiElement, p cursorPos) bool {
	return p.X >= el.X && p.X < el.X+el.W && p.Y >= el.Y && p.Y < el.Y+el.H
}

// boxContains reports whether outer's box fully contains
// inner's box (equal boxes contain each other).
func boxContains(outer, inner atspiElement) bool {
	return outer.X <= inner.X && outer.Y <= inner.Y &&
		outer.X+outer.W >= inner.X+inner.W &&
		outer.Y+outer.H >= inner.Y+inner.H
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
// full-coverage check; the KWin list alone cannot replace it
// for titles — the cursor helper returns class, geometry and
// the sensitive flag only, never captions (F-M2R-15) — but the
// cheap KWin query does supply the focus identity for the
// type/key re-check, so that re-check costs no second walk.
func (t *inputTool) windowInfo(ctx context.Context) inputWindowInfo {
	tree, _, _ := t.ground.atspiTree(ctx)
	if tree == nil {
		titles, err := t.shot.windowTitles(ctx)
		if err != nil {
			return inputWindowInfo{available: false}
		}
		info := inputWindowInfo{available: true, titles: titles}
		// Best effort: the KWin active window is the focus the
		// type/key re-check compares against. Unavailable here
		// leaves focus unknown — the re-check then only
		// verifies the current focus is known and non-sensitive.
		if cur, err := t.readCursor(ctx); err == nil {
			if w := kwinActiveWindow(cur.Windows); w != nil {
				info.focus = kwinFocusIdentity(*w)
			}
		}
		return info
	}
	info := inputWindowInfo{available: true}
	for _, w := range tree.Windows {
		info.titles = append(info.titles, w.Title)
	}
	info.windows = tree.Windows
	info.windowsListed = true
	// F-M2R-14: content-based signal — a password or
	// one-time-code field in the visible elements, even when
	// every title is innocuous. Computed here, on the same walk
	// the title check already paid for, so the content check
	// costs no second walk. The category names the kind of form
	// only, never an element name or title (F-M2R-15).
	if cat, wins := FindSensitiveContent(tree.Elements); cat != "" {
		info.contentHit = cat
		info.contentWindows = wins
	}
	// Keep the uncorrected boxes alongside the corrected ones:
	// a snapshot's recorded window geometry may be in either
	// frame — a grounding stores the corrected box when KWin
	// answered, the raw box when it did not (F-M2R-8 vs F2) —
	// and the geometry binding compares like-for-like in both
	// frames (see checkSnapshotGeometry). Copied before the
	// correction below, which mutates the windows in place.
	info.windowsRaw = make([]atspiWindow, len(tree.Windows))
	copy(info.windowsRaw, tree.Windows)
	if tree.FocusedWindow != nil {
		info.focus = focusIdentity{known: true, title: tree.FocusedWindow.Title}
	}
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
		// The KWin active window is compositor truth for the
		// focus: prefer it over the AT-SPI focused window
		// (which has reported focused=false for the active
		// window live).
		if w := kwinActiveWindow(cur.Windows); w != nil {
			info.focus = kwinFocusIdentity(*w)
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
		// F-M2R-14: the same correction condition for the
		// content-sensitive windows — a click may only be
		// allowed outside them when every such window's box
		// is KWin-corrected. An unplaced sensitive element
		// (contentWindows nil) is never precise: no box is
		// trustworthy, so the click refuses outright.
		if info.contentHit != "" {
			contentPrecise := info.contentWindows != nil
			for i := range info.contentWindows {
				if _, ok := offsets[i]; !ok {
					contentPrecise = false
					break
				}
			}
			info.contentPrecise = contentPrecise
		}
	}
	return info
}

// focusIdentity identifies the focused window for the type/key
// TOCTOU guard without ever naming it in output. The KWin form
// is the active window's app class + frame geometry + the
// sensitive pattern KWin itself matched ("" when none): KWin
// reports no titles (F-M2R-15), so class+geometry is the
// identity a focus move can change, and Sens is the
// sensitive-takeover signal. The AT-SPI form is the focused
// window's title, used only when KWin names no active window:
// it is compared, never logged anywhere — no title in the
// journal, no title in tool output.
type focusIdentity struct {
	known bool
	kwin  bool   // true: class+fg identity; false: AT-SPI title fallback
	class string // KWin active window class
	fg    [4]int // KWin active window frame geometry [x, y, w, h]
	sens  string // sensitive pattern KWin matched, "" when none
	title string // AT-SPI focused title, compare-only
}

// kwinFocusIdentity builds the KWin form of a focus identity
// from one active KWin window.
func kwinFocusIdentity(w kwinWindow) focusIdentity {
	return focusIdentity{known: true, kwin: true, class: w.Class, fg: w.FG, sens: w.Sens}
}

// equal reports whether two focus identities name the same
// focused window. Identities from different authorities never
// match: a focus that can only be confirmed by a different
// source than the one that checked it is treated as changed
// (fail closed — typing blind is the highest-risk op).
func (f focusIdentity) equal(o focusIdentity) bool {
	if !f.known || !o.known || f.kwin != o.kwin {
		return false
	}
	if f.kwin {
		return f.class == o.class && f.fg == o.fg
	}
	return f.title == o.title
}

// kwinActiveWindow returns the active window in a KWin window
// list, or nil when none is marked active.
func kwinActiveWindow(wins []kwinWindow) *kwinWindow {
	for i := range wins {
		if wins[i].Active {
			return &wins[i]
		}
	}
	return nil
}

// focusIdentity returns the currently focused window's identity:
// the KWin active window first (compositor truth, one cheap
// query), the AT-SPI focused window as fallback. An error means
// neither could identify the focused window.
func (t *inputTool) focusIdentity(ctx context.Context) (focusIdentity, error) {
	if cur, err := t.readCursor(ctx); err == nil {
		if w := kwinActiveWindow(cur.Windows); w != nil {
			return kwinFocusIdentity(*w), nil
		}
	}
	tree, _, err := t.ground.atspiTree(ctx)
	if err != nil || tree == nil || tree.FocusedWindow == nil || tree.FocusedWindow.Title == "" {
		return focusIdentity{}, fmt.Errorf("KWin oracle unavailable and no accessibility tree names a focused window")
	}
	return focusIdentity{known: true, title: tree.FocusedWindow.Title}, nil
}

// recheckFocus is the F-A23W2-10 TOCTOU guard: right before
// typing or pressing a key, re-read the focused window and
// compare its identity with the one the safety check saw. The
// check and the keystrokes are seconds apart (the AT-SPI walk
// alone is ~5 s), and in between the focus can move — a popup,
// a notification, the user Alt-Tabbing — sending the text into
// the wrong field. The re-read is a cheap KWin query, with the
// AT-SPI focused window as fallback. A changed focus, a newly
// sensitive focused window, or an undeterminable focus all
// refuse: typing blind is the highest-risk op. No refusal ever
// names a window title — a sensitive takeover names the
// category only.
func (t *inputTool) recheckFocus(ctx context.Context, before focusIdentity) error {
	after, err := t.focusIdentity(ctx)
	if err != nil {
		return fmt.Errorf("cannot confirm which window is focused (%v): I will not type or press keys blind", err)
	}
	if after.kwin {
		if after.sens != "" {
			return fmt.Errorf("the focused window is now a %s: I will not type or press keys into it", SensitiveTitleCategory(after.sens))
		}
	} else if hit := FindSensitiveTitle([]string{after.title}); hit != "" {
		return fmt.Errorf("the focused window is now a %s: I will not type or press keys into it", SensitiveTitleCategory(hit))
	}
	if before.known && !before.equal(after) {
		return fmt.Errorf("the focused window changed since the safety check: I will not type or press keys into a window I have not checked — look at the screen and retry")
	}
	return nil
}

// checkTypeSensitive refuses type/key when a sensitive window is
// focused or visible, or when the windows cannot be enumerated
// at all (fail closed — typing blind is the highest-risk op).
// A window holding a password or one-time-code field is
// sensitive too, even when its title is innocuous (F-M2R-14).
// The refusal names the category only, never the title. It
// returns the enumeration so the caller can re-check the focus
// right before typing (F-A23W2-10).
func (t *inputTool) checkTypeSensitive(ctx context.Context) (inputWindowInfo, error) {
	info := t.windowInfo(ctx)
	if !info.available {
		return info, fmt.Errorf("cannot determine the visible windows (no accessibility tree and no window list): I will not type or press keys blind")
	}
	if hit := FindSensitiveTitle(info.titles); hit != "" {
		return info, fmt.Errorf("this screen shows a %s: I will not type or press keys into it", SensitiveTitleCategory(hit))
	}
	// The KWin active window's own sensitive flag catches a
	// focused sensitive window the AT-SPI enumeration cannot
	// see (the same signal the focus re-check uses): the
	// refusal names the category only, never the title.
	if info.focus.kwin && info.focus.sens != "" {
		return info, fmt.Errorf("the focused window is a %s: I will not type or press keys into it", SensitiveTitleCategory(info.focus.sens))
	}
	// F-M2R-14: content-based refusal — a password or
	// one-time-code field visible anywhere on screen, even
	// when every window title is innocuous. Typing goes to
	// the focused window, but a credential form anywhere
	// visible means the keystrokes could land in it, so the
	// refusal is screen-wide, like the title check. The
	// refusal names the category only, never any element
	// name or title.
	if info.contentHit != "" {
		return info, fmt.Errorf("this screen shows a %s: I will not type or press keys into it", info.contentHit)
	}
	return info, nil
}

// checkClickSensitive refuses a click at (x, y) when a sensitive
// window is present — except when every sensitive window's box
// is KWin-corrected and the target lies outside all of them, in
// which case the click is elsewhere on screen. Titles-only
// enumeration, an uncorrected sensitive window, or no
// enumeration at all refuses the click. The same applies to the
// F-M2R-14 content signal: a window holding a password or
// one-time-code field is sensitive even when its title is
// innocuous, and the click must clear its corrected box too.
// For a snapshot click it also verifies the geometry binding
// (F-M2R-8): the window that held the element at ground time
// must still be at the same app + box in the fresh window list —
// a moved or resized window refuses the click, because the
// registered centre no longer names the element. The fresh list
// comes from the same walk this check already paid for, so the
// binding costs no second walk. (Movement between this check
// and the click itself is still open — doClick re-validates the
// element fresh right before the click — see F-A23W2-1.)
func (t *inputTool) checkClickSensitive(ctx context.Context, x, y int, snapshotID string, elementID *int) error {
	info := t.windowInfo(ctx)
	if !info.available {
		return fmt.Errorf("cannot determine the visible windows (no accessibility tree and no window list): I will not click blind")
	}
	hit := FindSensitiveTitle(info.titles)
	if hit == "" && info.contentHit == "" {
		return t.checkSnapshotGeometry(ctx, snapshotID, elementID, info)
	}
	// Precise path: every sensitive window — title-sensitive,
	// content-sensitive, or both — has a KWin-corrected box, so
	// a click outside all of them is allowed. An uncorrected
	// box on either signal refuses the click outright, naming
	// the category of the signal that cannot be placed.
	if hit != "" && !info.sensitivePrecise {
		return fmt.Errorf("this screen shows a %s: I will not click while it is visible", SensitiveTitleCategory(hit))
	}
	if info.contentHit != "" && !info.contentPrecise {
		return fmt.Errorf("this screen shows a %s: I will not click while it is visible", info.contentHit)
	}
	for _, w := range info.windows {
		if !TitleLooksSensitive(w.Title) {
			continue
		}
		if x >= w.X && x < w.X+w.W && y >= w.Y && y < w.Y+w.H {
			return fmt.Errorf("the click target (%d, %d) is inside a %s: I will not click there", x, y, SensitiveTitleCategory(w.Title))
		}
	}
	// F-M2R-14: the same point-in-box test for the windows
	// holding a sensitive element. A click inside one refuses
	// with the content category; a click outside every
	// sensitive window — title or content — falls through to
	// the snapshot geometry check.
	for i := range info.contentWindows {
		w := info.windows[i]
		if x >= w.X && x < w.X+w.W && y >= w.Y && y < w.Y+w.H {
			return fmt.Errorf("the click target (%d, %d) is inside a %s: I will not click there", x, y, info.contentHit)
		}
	}
	return t.checkSnapshotGeometry(ctx, snapshotID, elementID, info)
}

// checkSnapshotGeometry verifies a snapshot click's geometry
// binding against the fresh window list (nil when the click is
// by coordinates — nothing is bound then).
//
// Two frame problems, both fixed here rather than in
// CheckGroundSnapshotGeometry (whose contract — current is the
// KWin-corrected list — the ground tests pin):
//
// (1) No enumeration, no binding. When no AT-SPI tree was
// available the window list is empty because nothing was
// enumerated, not because the window is gone — the xdotool
// fallback names titles only, no boxes. Treating that empty
// list as "the window moved" would mask the real failure:
// doClick's re-validation, which needs the same tree, fails
// closed with 'cannot re-check' (F-A23W2-1). So the binding
// is skipped when no windows were listed, and the re-check
// owns the refusal. The click still refuses — only the
// message changes, to the check that actually failed.
//
// (2) Either frame. The snapshot's recorded box may be
// KWin-corrected (a grounding with KWin up) or raw (a
// grounding with KWin down, or a minimal registration) —
// the registry records no frame — so the binding compares
// like-for-like in BOTH frames and passes when either
// matches. A window that genuinely moved fails in both
// frames and still refuses (F-M2R-8); the ~29 px F2 shift
// itself matches the raw frame and never trips a false
// refusal. The one case both frames agree on wrongly is a
// genuine move of exactly the KWin offset — and that move
// is still caught downstream, before any click, by
// revalidateSnapshotElement, which checks the element's
// centre against the cursor (F-A23W2-1). A lookup error
// (unknown/stale snapshot, wrong chat) is returned as-is:
// it is the same in either frame, so no raw retry runs.
func (t *inputTool) checkSnapshotGeometry(ctx context.Context, snapshotID string, elementID *int, info inputWindowInfo) error {
	if snapshotID == "" || elementID == nil {
		return nil
	}
	if !info.windowsListed {
		return nil
	}
	toGeometries := func(windows []atspiWindow) []WindowGeometry {
		current := make([]WindowGeometry, 0, len(windows))
		for _, w := range windows {
			current = append(current, WindowGeometry{App: w.App, X: w.X, Y: w.Y, W: w.W, H: w.H})
		}
		return current
	}
	err := CheckGroundSnapshotGeometry(ctx, snapshotID, *elementID, toGeometries(info.windows))
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "moved or resized") {
		if err := CheckGroundSnapshotGeometry(ctx, snapshotID, *elementID, toGeometries(info.windowsRaw)); err == nil {
			return nil
		}
	}
	return err
}

// checkRateLimit enforces the min-interval (per tool instance)
// and the per-turn + per-hour limits (the SHARED capture budget,
// F-A23W2-14) and records the call. The budget refusal is
// wrapped in input's own message, so the model hears the same
// "at most 15 input calls per turn" as before.
func (t *inputTool) checkRateLimit() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if !t.lastCall.IsZero() && now.Sub(t.lastCall) < inputMinInterval {
		return fmt.Errorf("input rate limit: at most one input call per second (last call %v ago)", now.Sub(t.lastCall).Round(time.Millisecond))
	}
	if err := t.budget.Charge(now, inputMaxPerTurn, inputMaxPerHour); err != nil {
		if errors.Is(err, errCaptureBudgetTurn) {
			return fmt.Errorf("input rate limit: at most %d input calls per turn", inputMaxPerTurn)
		}
		return fmt.Errorf("input rate limit: at most %d input calls per hour", inputMaxPerHour)
	}
	t.lastCall = now
	return nil
}
