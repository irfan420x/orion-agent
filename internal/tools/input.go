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
	// target. ClickMayHaveLanded is true when the click command
	// itself timed out — the click may already have landed, and
	// Warning says to verify before retrying instead of
	// clicking again blindly.
	PostClickCursor    *cursorPos `json:"post_click_cursor,omitempty"`
	ClickVerified      bool       `json:"click_verified,omitempty"`
	ClickMayHaveLanded bool       `json:"click_may_have_landed,omitempty"`
	Warning            string     `json:"warning,omitempty"`
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
	sensitivePrecise bool          // every sensitive window has a corrected box
	focus            focusIdentity // focused window when enumerated, unknown when neither authority could name it
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
	if err := t.checkRateLimit(); err != nil {
		return denied("%v", err)
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
		if out.uncertain {
			// The click command timed out: the click may
			// already have landed. This is a result, not an
			// error — an error would make the model retry and
			// double-click.
			summary.ClickMayHaveLanded = true
			summary.Warning = "the click command timed out — the click may already have landed. Verify on screen before retrying; do not click again blindly."
		} else if out.postClick == nil {
			summary.Warning = "the click was sent but the post-click check could not run (KWin oracle unavailable): verify on screen before retrying."
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
// a retryable error.
type clickOutcome struct {
	final     cursorPos
	iters     int
	postClick *cursorPos // oracle cursor right after the click, nil when the re-read failed
	verified  bool       // the post-click oracle agreed the cursor is still on target
	uncertain bool       // the click command timed out: it may have landed
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
func (t *inputTool) doClick(ctx context.Context, button string, clicks int, snapshotID string, elementID *int, target cursorPos) (clickOutcome, error) {
	final, iters, err := t.doMove(ctx, target.X, target.Y)
	if err != nil {
		return clickOutcome{}, err
	}
	if snapshotID != "" && elementID != nil {
		if err := t.revalidateSnapshotElement(ctx, snapshotID, *elementID, final); err != nil {
			return clickOutcome{}, err
		}
	} else if err := requireFreshGrounding(); err != nil {
		// Coordinate click: the grounding may have gone stale
		// during the sensitive check and the move.
		return clickOutcome{}, err
	}
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
	// re-read succeeds and the cursor is still on target. A
	// failed re-read does not fail the click — ydotool already
	// reported success — but the result says so, so the model
	// verifies on screen before retrying.
	out := clickOutcome{final: final, iters: iters}
	if post, err := t.readCursor(ctx); err == nil {
		out.postClick = &post.Cursor
		out.verified = withinTolerance(post.Cursor, target.X, target.Y)
	}
	return out, nil
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
func (t *inputTool) revalidateSnapshotElement(ctx context.Context, snapshotID string, elementID int, cursor cursorPos) error {
	snap, err := groundSnapshots.lookup(snapshotID, chatIDFromContext(ctx), elementID, time.Now())
	if err != nil {
		return fmt.Errorf("snapshot went stale before the click: %v (ground again)", err)
	}
	want := snap.details[elementID]
	tree, _, err := t.ground.atspiTree(ctx)
	if err != nil {
		return fmt.Errorf("cannot re-check element %d (%s %q) before the click: the accessibility tree failed (%v) — ground again", elementID, want.Role, want.Name, err)
	}
	if tree == nil {
		return fmt.Errorf("cannot re-check element %d (%s %q) before the click: no accessibility tree — ground again", elementID, want.Role, want.Name)
	}
	cur, err := t.readCursor(ctx)
	if err != nil {
		return fmt.Errorf("cannot re-check element %d (%s %q) before the click: the KWin oracle failed (%v) — ground again", elementID, want.Role, want.Name, err)
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
	fresh, _, _ := filterGroundElements(tree, roleSet, nil, groundMaxMaxElements, 0, 0, viewW, viewH, kwinMinimizedWindowIndexes(tree, cur.Windows))
	var moved *atspiElement // same role+name, centre elsewhere
	for i := range fresh {
		el := &fresh[i]
		if el.Role != want.Role || el.Name != want.Name {
			continue
		}
		cx, cy := el.X+el.W/2, el.Y+el.H/2
		if withinTolerance(cursorPos{X: cx, Y: cy}, cursor.X, cursor.Y) {
			return nil // still at the cursor: click
		}
		if moved == nil {
			moved = el
		}
	}
	if moved != nil {
		return fmt.Errorf("element %d (%s %q) has moved since grounding: it was at (%d, %d), now at (%d, %d) — the click target is stale: ground again", elementID, want.Role, want.Name, want.Centre.X, want.Centre.Y, moved.X+moved.W/2, moved.Y+moved.H/2)
	}
	return fmt.Errorf("element %d (%s %q) is no longer on screen — ground again and pick an element from the new list", elementID, want.Role, want.Name)
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
	return info, nil
}

// checkClickSensitive refuses a click at (x, y) when a sensitive
// window is present — except when every sensitive window's box
// is KWin-corrected and the target lies outside all of them, in
// which case the click is elsewhere on screen. Titles-only
// enumeration, an uncorrected sensitive window, or no
// enumeration at all refuses the click. For a snapshot click it
// also verifies the geometry binding (F-M2R-8): the window that
// held the element at ground time must still be at the same
// app + box in the fresh window list — a moved or resized window
// refuses the click, because the registered centre no longer
// names the element. The fresh list comes from the same walk
// this check already paid for, so the binding costs no second
// walk. (Movement between this check and the click itself is
// still open — doClick re-validates only TTL/supersession —
// see F-A23W2-1.)
func (t *inputTool) checkClickSensitive(ctx context.Context, x, y int, snapshotID string, elementID *int) error {
	info := t.windowInfo(ctx)
	if !info.available {
		return fmt.Errorf("cannot determine the visible windows (no accessibility tree and no window list): I will not click blind")
	}
	hit := FindSensitiveTitle(info.titles)
	if hit == "" {
		return t.checkSnapshotGeometry(ctx, snapshotID, elementID, info.windows)
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
		return t.checkSnapshotGeometry(ctx, snapshotID, elementID, info.windows)
	}
	return fmt.Errorf("this screen shows a %s: I will not click while it is visible", SensitiveTitleCategory(hit))
}

// checkSnapshotGeometry verifies a snapshot click's geometry
// binding against the fresh window list (nil when the click is
// by coordinates — nothing is bound then).
func (t *inputTool) checkSnapshotGeometry(ctx context.Context, snapshotID string, elementID *int, windows []atspiWindow) error {
	if snapshotID == "" || elementID == nil {
		return nil
	}
	current := make([]WindowGeometry, 0, len(windows))
	for _, w := range windows {
		current = append(current, WindowGeometry{App: w.App, X: w.X, Y: w.Y, W: w.W, H: w.H})
	}
	return CheckGroundSnapshotGeometry(ctx, snapshotID, *elementID, current)
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
