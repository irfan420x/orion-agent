// Package tools — ground (computer-use tool 2 of 5, M2).
//
// ground is the grounding tool from the M2 design
// (~/workspace/orion-verification/computer-use-m2/design/grounding-design.md,
// Irfan's idea 2026-11-15 18:18 SGT): the provider must never guess
// coordinates again. One call returns, from the same instant, (1) a
// screenshot with numbered Set-of-Mark markers and (2) the exact
// name/role/bounding-box of every interactive element on screen.
// The provider picks an element NUMBER; the future M3 mouse tool
// clicks that element's exact centre — pixel guessing is gone.
//
// Sources, in order:
//  1. AT-SPI tree via a vendored Python helper (ground_atspi.py,
//     embedded in the binary, written 0600 to ~/.orion/ at Execute
//     time, run with a timeout). Go's stdlib has no D-Bus client,
//     so this is the only path (design §9 path a). The helper
//     reads name/role/screen-extents ONLY — never text values — so
//     a password field contributes role + box and nothing else.
//  2. Vision-only fallback: when the helper is missing or fails,
//     the screenshot is still returned with an empty element list
//     and coverage "vision_only". Never faked elements.
//
// Bounds (design §2.1): the walk is depth-capped, node-capped and
// helper-timeout-bounded so a heavy app cannot hang the tool.
// Depth cap 12: the design probe walked depth <= 8 and did NOT
// reach GitHub's "Sign in" button — page content sits at depth
// 6-10, chrome at <= 4, so the button sits at depth >= 9; 12
// reaches it with headroom for one more nesting level (dialogs,
// nested frames) while the node cap (~1000) and the 5 s helper
// timeout bound the cost. The live verification must measure the
// real depth that reaches "Sign in" and adjust this cap if it is
// ever observed deeper than 12.
//
// Timing note: the tree is walked BEFORE the screenshot (the
// sensitive-window check must refuse before anything is captured),
// so the tree is NOT the same instant as the image. The old
// comment claimed "~0.4 s apart" — that was measured on a
// 140-node tree (0.38 s); on a real 942-node tree the walk alone
// takes ~5.4 s (D-Bus round-trip bound), so the markers come
// from a tree up to ~6 s old. The summary now reports the real
// numbers (tree_walk_ms, tree_age_ms) instead of the stale
// claim, and a post-capture KWin re-check refuses the grounding
// when a window moved between the walk and the capture —
// in-page movement (scrolling, typing) is still invisible to
// that check, and the reported ages are the honest bound on it.
//
// Safety (design §4.3, enforced in code):
//   - interactive roles only (default set below); degenerate
//     boxes (w<=1 || h<=1 || x<0 || y<0) excluded;
//   - password fields: role + box only, value/text never read,
//     never in results or logs (the helper cannot read values);
//   - data minimisation (F-M2R-17): element names are capped at
//     60 runes, correction entries carry the app (never a window
//     title), snapshots bind windows by app + box (never a
//     title), and the KWin helper never prints a title — the
//     output carries only what a click needs (id, role, short
//     name, box, centre) plus the focused window and the
//     correction;
//   - sensitive-window refusal BEFORE grounding, using the AT-SPI
//     window titles (which see native Wayland windows xdotool
//     cannot) plus the KWin sensitive flag (which sees the Qt
//     windows the tree cannot); with neither source available
//     the grounding is refused, never captured blind (F-M2R-4/5);
//   - empty name -> the role is the name, never a guessed name.
//   - read-only, RiskClass Network (the image leaves the machine
//     to the model provider — F-M2R-6); refused in unattended turns.
//
// Stale-refusal (§5): every grounding registers its snapshot_id +
// element list in the package-level snapshot registry
// (groundSnapshots, max 3 retained — the same last-3 retention as
// M1's screenshot history eviction, so an evicted snapshot's
// elements are no longer clickable). ValidateGroundSnapshot,
// used by the M3 mouse tool, refuses when the snapshot is
// >60 s old, superseded by a newer grounding, evicted, or the
// element id is not in that snapshot. Ground itself never clicks.
// The registry also binds each element to its window's geometry
// at ground time (F-M2R-8): CheckGroundSnapshotGeometry refuses
// the click when that window has moved or resized since.
//
// Markers: Go stdlib image/draw only — no new deps. The stdlib
// has no text rendering, so a tiny 5x7 bitmap digit font is
// embedded below and each marker is a small readable number badge
// drawn at the element's centre, clamped inside the element's box
// when the box is big enough to hold it.
//
// Rate limits: the M1 screenshot discipline (min 1 s between
// captures, max 15 per turn, max 120 per hour), enforced here on
// ground calls — but the per-turn/hour counting is SHARED with
// the screenshot and input tools through one CaptureBudget
// (F-A23W2-14 / F-M2R-16): a ground call charges the shared
// budget once, and the inner screenshot capture it takes is
// already paid for, so it does not charge again. One grounding
// is one capture, not two, and the combined worst case per turn
// is 15 computer-use calls across ground + screenshot + input
// (parent and children together), not 30 captures + 15 inputs.
// The shared budget also fixes F-M2R-1 as a side effect: the
// embedded screenshot's own counter used to never reset (its
// BeginTurn was never called — it is not a registry tool), so
// ground died permanently after 15 groundings; the shared
// budget resets with ground's BeginTurn.
package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg" // registers the JPEG decoder for image.Decode below
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
)

//go:embed ground_atspi.py
var atspiHelperSource string

//go:embed ground_kwin.py
var kwinHelperSource string

func init() {
	RegisterFactory("ground", func(deps ToolDeps) (Tool, error) {
		return newGroundToolWithBudget(deps.CaptureBudget, deps.CaptureBudgetOwner), nil
	})
}

// Grounding bounds (design §2.1 — see the package comment for the
// depth-cap rationale).
const (
	groundMaxDepth = 12
	groundMaxNodes = 1000
	// Helper timeout: the walk is D-Bus round-trip bound (~5 ms per
	// node), and the SHOWING query (one extra call per boxed node,
	// for the hidden-popup filter) pushed the measured walk on a
	// 942-node tree from 4.83 s to 5.44 s — past the old 5 s bound,
	// which silently killed the helper and degraded the tool to
	// vision_only (live 2026-11-15 20:31). 10 s keeps a bound
	// against a hung walk while leaving headroom for heavier pages.
	groundHelperTimeout = 10 * time.Second
	// Ground time budget: 10 s (raised from the 3 s design target,
	// 2026-11-15). Measured floor ~8.1 s: AT-SPI walk 5.44 s
	// (D-Bus round-trip bound, ~5.8 ms/node on a 942-node tree),
	// spectacle ~1.0 s, probe 0.36 s, KWin query ~0.1-0.5 s.
	// Depth caps cannot help — the tree is wide, not deep. The
	// budget is ENFORCED (F-M2R-10): ExecuteResult runs under one
	// context deadline for the whole grounding (probe + walk +
	// KWin + capture), so the worst case is the budget, not the
	// ~25 s+ the sequential per-step timeouts allowed. The
	// enforced bounds inside it are groundHelperTimeout (10 s),
	// the KWin helper timeout (half the budget), and the agent's
	// 120 s step timeout. Grounding is a deliberate
	// once-per-decision action, not a hot path.
	groundDefaultMaxElements = 60
	groundMaxMaxElements     = 200
	groundSnapshotTTL        = 60 * time.Second
	groundMaxSnapshots       = 3 // mirrors M1's last-3 image eviction
	groundHelperFile         = "ground-atspi-helper.py"
	groundKWinHelperFile     = "ground-kwin-helper.py"
)

// groundTimeBudget is the enforced whole-grounding deadline
// (F-M2R-10). It is a var, not a const, so tests can shrink it to
// prove the deadline fires (see TestGroundTimeBudget).
var groundTimeBudget = 10 * time.Second

// groundKWinTimeout bounds one KWin helper run: half the ground
// time budget, so a KWin failure (no KWin, no journal, hung
// script) can never eat the whole budget — the grounding degrades
// to uncorrected instead. The query itself measured ~0.1 s
// (10/10 trials). Computed from the budget, not a const, so a
// test-shrunk budget shrinks it too.
func groundKWinTimeout() time.Duration { return groundTimeBudget / 2 }

// Ground rate limits (M1 screenshot discipline, plan §4.1).
// The per-turn/hour numbers alias the shared capture budget's
// (F-A23W2-14): one limit across ground + screenshot + input,
// not one per tool. The min interval stays per tool instance.
const (
	groundMinInterval = time.Second
	groundMaxPerTurn  = captureMaxPerTurn
	groundMaxPerHour  = captureMaxPerHour
	// groundMaxNameRunes caps element names in the output
	// (F-M2R-17 data minimisation): the model needs a label to
	// tell elements apart, not a full document title or message
	// subject — accessible names can carry content. Names over
	// the cap are cut to 59 runes + "…". The AT-SPI helper
	// enforces the same cap, so the bound holds even if the Go
	// filter is bypassed.
	groundMaxNameRunes = 60
)

// defaultInteractiveRoles is the default `roles` filter: roles a
// user can act on. Headings, frames, panels and document text are
// not clickable, so they never appear — the provider must never be
// offered a target it cannot use.
//
// "button" is the HTML button role: Firefox reports every web
// button (Sign in, Continue with Google, ...) as "button", NOT
// "push button" — the M2 live verification found zero "push
// button" nodes in a 939-node tree, so without "button" the
// page's primary action was missing (F1). "toggle button" and
// "push button menu" are button variants found in the same tree;
// "radio button" is the check-box sibling (same selection-control
// class). "page tab", "list item", "canvas" and "menu" are NOT
// here: tabs are a separate switching affordance, and list
// items / canvases / menus are containers or non-button surfaces
// whose nodes would add noise.
var defaultInteractiveRoles = []string{
	"push button", "button", "link", "entry", "text", "password text",
	"check box", "radio button", "toggle button", "push button menu",
	"combo box", "menu item",
}

// groundArgs is the tool's JSON arguments.
type groundArgs struct {
	Region      *region  `json:"region"`       // zoom crop in screen px, optional
	MaxElements *int     `json:"max_elements"` // default 60, range 1-200
	Roles       []string `json:"roles"`        // default: interactive roles only
}

// groundSummary is the JSON text returned with the marked image.
type groundSummary struct {
	SnapshotID    string            `json:"snapshot_id"`
	TakenAt       string            `json:"taken_at"`
	Screen        groundScreen      `json:"screen"`
	Region        *region           `json:"region,omitempty"`
	FocusedWindow *groundWindowRef  `json:"focused_window,omitempty"`
	Elements      []groundElement   `json:"elements"`
	Coverage      string            `json:"coverage"` // grounded | vision_only
	WalkedNodes   int               `json:"walked_nodes,omitempty"`
	Correction    *groundCorrection `json:"correction,omitempty"`
	// TreeWalkMs is how long the AT-SPI probe + walk took, and
	// TreeAgeMs how long from the end of that walk to the
	// capture (F-M2R-11): the markers are that old, never the
	// "~0.4 s" the old comment claimed. GeometryVerified is
	// true only when the post-capture KWin re-check ran and the
	// window geometries still matched — false when KWin was
	// unavailable for either check.
	TreeWalkMs       int64 `json:"tree_walk_ms,omitempty"`
	TreeAgeMs        int64 `json:"tree_age_ms,omitempty"`
	GeometryVerified bool  `json:"geometry_verified,omitempty"`
}

// groundCorrection records the KWin-measured per-window
// coordinate correction applied to this grounding (F2): each
// entry is a window whose AT-SPI coordinates were shifted by
// (DX, DY) to match the compositor's true geometry. Absent when
// the KWin query was unavailable — then the elements are raw
// AT-SPI coordinates, uncorrected. A present correction with an
// empty Windows list means KWin ran and found nothing to shift.
type groundCorrection struct {
	Source  string              `json:"source"` // always "kwin"
	Windows []groundWindowShift `json:"windows"`
}

type groundWindowShift struct {
	App string `json:"app"`
	DX  int    `json:"dx"`
	DY  int    `json:"dy"`
}

// groundScreen is the grounded view: the returned image's
// dimensions. With no region that is the full screen (the capture
// is taken at full resolution, max_width 1920, scale 1.0, so image
// pixels ARE screen pixels); with a region it is the cropped view
// and centre_norm is relative to that view. Scale is the ratio of
// returned-image pixels to view pixels — 1 unless the image had
// to be shrunk under the 2 MiB cap.
type groundScreen struct {
	Width  int     `json:"width"`
	Height int     `json:"height"`
	Scale  float64 `json:"scale"`
}

type groundWindowRef struct {
	Title string `json:"title"`
	App   string `json:"app"`
}

// groundElement is one grounded element. Box is absolute screen
// pixels (for verification); centre is absolute screen pixels;
// centre_norm is 0-1000 relative to the returned image (the
// grounded view) — the canonical coordinate the M3 mouse tool
// maps back through the snapshot. There is deliberately NO value
// field: a password field's value must never appear here.
type groundElement struct {
	ID         int         `json:"id"`
	Role       string      `json:"role"`
	Name       string      `json:"name"`
	Box        groundBox   `json:"box"`
	Centre     groundPoint `json:"centre"`
	CentreNorm groundPoint `json:"centre_norm"`
}

type groundBox struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

type groundPoint struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// --- AT-SPI helper output ---

type atspiOutput struct {
	Windows       []atspiWindow   `json:"windows"`
	Elements      []atspiElement  `json:"elements"`
	FocusedWindow *atspiWindowRef `json:"focused_window"`
	WalkedNodes   int             `json:"walked_nodes"`
}

type atspiWindow struct {
	Title   string `json:"title"`
	App     string `json:"app"`
	Role    string `json:"role"`
	Focused bool   `json:"focused"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
	W       int    `json:"w"`
	H       int    `json:"h"`
}

type atspiWindowRef struct {
	Title string `json:"title"`
	App   string `json:"app"`
}

type atspiElement struct {
	Role string `json:"role"`
	Name string `json:"name"`
	X    int    `json:"x"`
	Y    int    `json:"y"`
	W    int    `json:"w"`
	H    int    `json:"h"`
	// Win is the index of this element's window in
	// atspiOutput.Windows (set by the helper). Nil means the
	// helper did not tag it (older output): the element is left
	// uncorrected — a missing signal never guesses a window.
	Win *int `json:"win,omitempty"`
	// Showing is the AT-SPI SHOWING state reported by the helper.
	// Nil means the helper did not report it (state query failed):
	// the element is kept — a missing signal never drops an
	// element. False means hidden: the element stays in the tree
	// (closed popups keep their nodes) but is not on screen.
	Showing *bool `json:"showing,omitempty"`
}

// --- KWin helper output ---

// kwinWindow is one window's true screen geometry from the KWin
// compositor (frameGeometry is the outer frame; clientGeometry
// the content area — identical for Wayland apps without
// decorations). Class is the app class (e.g. "firefox-esr").
// There is deliberately NO caption/title field (F-M2R-15): the
// helper's output passes through the systemd journal, and titles
// can carry sensitive text — so the helper never prints them.
// Sens is the sensitive pattern the KWin script itself matched
// against the window's caption ("" when none matched): the
// sensitive check runs inside KWin, where the title is visible,
// and only the matched pattern leaves it.
type kwinWindow struct {
	Class     string `json:"cls"`
	Active    bool   `json:"active"`
	Minimized bool   `json:"minimized"`
	FG        [4]int `json:"fg"` // [x, y, w, h] frame geometry
	CG        [4]int `json:"cg"` // [x, y, w, h] client geometry
	Sens      string `json:"sens,omitempty"`
}

type kwinOutput struct {
	Windows []kwinWindow `json:"kwin_windows"`
}

// matchKWinWindows matches KWin's true geometries to the AT-SPI
// windows and returns the per-window coordinate shift (KWin
// geometry minus AT-SPI box) for each matched window, keyed by the
// AT-SPI window index. A window matches when exactly one KWin
// window has a compatible app class and the same size; the shift
// is then a pure translation (the app reports window-relative
// coordinates — Firefox's window at (0,0) while KWin places it
// at (0,29)). Anything ambiguous or unmatched gets no shift: an
// uncorrected element is never worse than a wrongly-shifted one.
//
// The size match tries the frame geometry first, then the client
// geometry (F-M2R-9: CG was collected but never used). A decorated
// window's frame is bigger than the area AT-SPI reports, so the
// FG size never matches — but the client area does, and the
// offset then comes from the client origin. The offset is taken
// from whichever geometry matched.
//
// Titles are deliberately NOT used for matching (F-M2R-15): the
// KWin helper no longer reports them, so two same-app, same-size
// windows are ambiguous and stay uncorrected rather than guessed.
//
// The focused/active flags are deliberately NOT used for
// matching: AT-SPI reported focused=false for the active Firefox
// window (live 2026-11-15), so they are unreliable here.
func matchKWinWindows(tree *atspiOutput, kwin []kwinWindow) map[int][2]int {
	offsets := map[int][2]int{}
	if tree == nil {
		return offsets
	}
	for i, w := range tree.Windows {
		var matches [][2]int
		for _, kw := range kwin {
			if !kwinClassCompatible(w.App, kw.Class) {
				continue
			}
			if w.W == kw.FG[2] && w.H == kw.FG[3] {
				matches = append(matches, [2]int{kw.FG[0] - w.X, kw.FG[1] - w.Y})
			} else if w.W == kw.CG[2] && w.H == kw.CG[3] {
				matches = append(matches, [2]int{kw.CG[0] - w.X, kw.CG[1] - w.Y})
			}
		}
		if len(matches) != 1 {
			continue
		}
		offsets[i] = matches[0]
	}
	return offsets
}

// kwinClassCompatible reports whether an AT-SPI app name and a
// KWin resource class name the same app: lowercased, equal, or
// one a prefix of the other at a word boundary ("firefox" matches
// "firefox-esr" — the next char is '-'; "fire" never matches
// "firefox", and "firefox" never matches "firefoxesr").
func kwinClassCompatible(app, class string) bool {
	a, c := strings.ToLower(strings.TrimSpace(app)), strings.ToLower(strings.TrimSpace(class))
	if a == "" || c == "" {
		return false
	}
	if a == c {
		return true
	}
	short, long := a, c
	if len(c) < len(a) {
		short, long = c, a
	}
	if !strings.HasPrefix(long, short) {
		return false
	}
	next := long[len(short)]
	return next == '-' || next == '_' || next == '.'
}

// applyWindowOffsets shifts every element (and window box) by its
// window's matched offset, in place. Elements whose window index
// is missing or unmatched are left untouched.
func applyWindowOffsets(tree *atspiOutput, offsets map[int][2]int) {
	if tree == nil || len(offsets) == 0 {
		return
	}
	for i := range tree.Windows {
		if off, ok := offsets[i]; ok {
			tree.Windows[i].X += off[0]
			tree.Windows[i].Y += off[1]
		}
	}
	for i := range tree.Elements {
		el := &tree.Elements[i]
		if el.Win == nil {
			continue
		}
		if off, ok := offsets[*el.Win]; ok {
			el.X += off[0]
			el.Y += off[1]
		}
	}
}

// --- snapshot registry (stale-refusal, design §5) ---

// WindowGeometry is one window's app + box, recorded at ground
// time so a later click can verify the window is still where the
// grounding saw it (F-M2R-8 geometry binding). No title: titles
// can carry sensitive text and are not needed to bind a window —
// app + box is the identity.
type WindowGeometry struct {
	App  string
	X, Y int
	W, H int
}

// groundSnapshot is one grounding's registered state: the element
// ids the M3 input tool may click, their centres (the click
// targets — the registry, not the model, supplies coordinates),
// the chat the grounding belongs to (F-A23W2-19: a snapshot
// registered in one chat must never be clickable from another —
// the registry is process-wide, so without the binding a
// same-second id collision could click the wrong chat's element),
// the window geometry each element was grounded in (the geometry
// binding — a click refuses when that window has moved or
// resized since the grounding), details carries the full element
// (role, name, box) alongside the centre: the input tool's
// pre-click re-check (F-A23W2-1) matches a fresh AT-SPI read
// against the grounded element's identity, not just its id,
// and the sequence number that decides supersession (only the
// newest snapshot is live).
type groundSnapshot struct {
	id             string
	chatID         int64 // 0 when the grounding ran with no chat in context (CLI, tests)
	takenAt        time.Time
	elements       map[int]bool
	centres        map[int]groundPoint
	elementWindows map[int]WindowGeometry
	details        map[int]groundElement
	seq            int64
}

// groundSnapshotRegistry keeps the last groundMaxSnapshots
// groundings. Anything older is evicted and its elements are no
// longer clickable — the registry is the enforcement point for
// the M1 last-3 eviction rule applied to grounding snapshots.
// Retention is global, not per chat: one chat's groundings can
// evict another chat's snapshot, but that only ever fails closed
// (the click is refused and the provider grounds again) — it can
// never retarget a click, because lookup also checks the chat
// binding (F-A23W2-19).
type groundSnapshotRegistry struct {
	mu        sync.Mutex
	snapshots []*groundSnapshot // oldest first
	seq       int64
}

func newGroundSnapshotRegistry() *groundSnapshotRegistry {
	return &groundSnapshotRegistry{}
}

// groundSnapshots is the process-wide registry. It is package
// level (not per-tool-instance) because the M3 mouse tool is a
// different tool instance in the same process and must validate
// against the groundings this tool registered.
var groundSnapshots = newGroundSnapshotRegistry()

// register stores a grounding and evicts the oldest snapshots
// past the retention limit. It returns the snapshot's sequence
// number. Centres are stored alongside the ids: the M3 input
// tool clicks an element's registered centre, never a coordinate
// the model typed (a model-typed coordinate is a guess; the
// registry's centre is the grounded truth). chatID binds the
// snapshot to the chat that grounded it (F-A23W2-19): a lookup
// from any other chat refuses, even on an id collision. windows binds each
// element id to the geometry of the window that held it at
// ground time (nil when the grounding recorded none — e.g. an
// untagged element — in which case that element has no geometry
// binding and only the TTL/supersession checks apply). The full element is
// stored too, with an empty name normalised to the role exactly
// as the grounding summary does, so the pre-click re-check can
// match a fresh tree read against the grounded identity.
func (r *groundSnapshotRegistry) register(id string, chatID int64, takenAt time.Time, elements []groundElement, windows map[int]WindowGeometry) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	els := make(map[int]bool, len(elements))
	cen := make(map[int]groundPoint, len(elements))
	det := make(map[int]groundElement, len(elements))
	for _, e := range elements {
		els[e.ID] = true
		cen[e.ID] = e.Centre
		if strings.TrimSpace(e.Name) == "" {
			e.Name = e.Role
		}
		det[e.ID] = e
	}
	r.snapshots = append(r.snapshots, &groundSnapshot{id: id, chatID: chatID, takenAt: takenAt, elements: els, centres: cen, elementWindows: windows, details: det, seq: r.seq})
	for len(r.snapshots) > groundMaxSnapshots {
		r.snapshots = r.snapshots[1:]
	}
	return r.seq
}

// latest returns the newest snapshot, or nil when none exists.
func (r *groundSnapshotRegistry) latest() *groundSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.snapshots) == 0 {
		return nil
	}
	return r.snapshots[len(r.snapshots)-1]
}

// chatIDFromContext reads the chat id the approval context
// carries (set by runAgentTurn), or 0 when the call runs with no
// chat in context (CLI runs, tests). Snapshot binding treats 0
// as its own chat: no-context callers share chat 0, exactly as
// they shared the unbound registry before — the binding only ever
// separates callers that DO carry different chat ids.
func chatIDFromContext(ctx context.Context) int64 {
	chatID, _, _ := permissions.ApprovalContextFrom(ctx)
	return chatID
}

// latestFresh reports whether the newest snapshot is still
// within its TTL. A coordinate click (F-A23W2-8) is only
// meaningful against a recent grounding: without one the model
// is clicking a remembered number from an old screen.
func (r *groundSnapshotRegistry) latestFresh(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.snapshots) == 0 {
		return false
	}
	return now.Sub(r.snapshots[len(r.snapshots)-1].takenAt) <= groundSnapshotTTL
}

// ValidateGroundSnapshot is the M3 input tool's gate: it refuses a
// click when (a) the snapshot is unknown or evicted, (b) it is
// older than the 60 s TTL, (c) a newer grounding has superseded
// it, (d) the element id is not in that snapshot, or (e) the
// snapshot belongs to a different chat (F-A23W2-19). The error
// always tells the provider to ground again — never a coordinate.
func ValidateGroundSnapshot(ctx context.Context, id string, elementID int) error {
	return groundSnapshots.validate(id, chatIDFromContext(ctx), elementID, time.Now())
}

func (r *groundSnapshotRegistry) validate(id string, chatID int64, elementID int, now time.Time) error {
	_, err := r.lookup(id, chatID, elementID, now)
	return err
}

// GroundSnapshotElementCentre is the M3 input tool's target
// resolver: it validates (snapshot, element) exactly as
// ValidateGroundSnapshot does and returns the element's centre
// in absolute screen pixels. The input tool clicks this centre
// — the registry's grounded truth — never a coordinate from the
// model. The error always tells the provider to ground again.
func GroundSnapshotElementCentre(ctx context.Context, id string, elementID int) (groundPoint, error) {
	snap, err := groundSnapshots.lookup(id, chatIDFromContext(ctx), elementID, time.Now())
	if err != nil {
		return groundPoint{}, err
	}
	return snap.centres[elementID], nil
}

// CheckGroundSnapshotGeometry is the M3 input tool's geometry
// binding (F-M2R-8): it validates (snapshot, element) exactly as
// ValidateGroundSnapshot does — including the chat binding
// (F-A23W2-19), read from ctx — then verifies the window that
// held the element at ground time is still at the same app + box
// in current (the caller's fresh window list, KWin-corrected).
// A window that moved or resized — or is gone — refuses the
// click: the registered centre no longer names the element.
// An element with no recorded window (untagged at ground time)
// has nothing to bind and passes this check; the TTL /
// supersession / membership validation still applies to it.
// current is the live window list; an empty list with a recorded
// binding fails closed (nothing to verify against).
func CheckGroundSnapshotGeometry(ctx context.Context, id string, elementID int, current []WindowGeometry) error {
	snap, err := groundSnapshots.lookup(id, chatIDFromContext(ctx), elementID, time.Now())
	if err != nil {
		return err
	}
	bound, ok := snap.elementWindows[elementID]
	if !ok {
		return nil
	}
	for _, w := range current {
		if w == bound {
			return nil
		}
	}
	return fmt.Errorf("the window holding element %d has moved or resized since the grounding (was %s %dx%d at %d,%d): ground again and use the new snapshot", elementID, bound.App, bound.W, bound.H, bound.X, bound.Y)
}

// lookup is the shared validation for validate and
// GroundSnapshotElementCentre: it returns the snapshot only when
// it is known, fresh, newest, contains the element, and belongs
// to the caller's chat (F-A23W2-19). The chat-mismatch error
// never names the other chat: the id alone is enough for the
// provider to ground again in its own chat.
func (r *groundSnapshotRegistry) lookup(id string, chatID int64, elementID int, now time.Time) (*groundSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var snap *groundSnapshot
	for _, s := range r.snapshots {
		if s.id == id {
			snap = s
			break
		}
	}
	if snap == nil {
		return nil, fmt.Errorf("snapshot %s is not available (unknown or evicted — only the last %d groundings are kept): ground again and use the new snapshot", id, groundMaxSnapshots)
	}
	if snap.chatID != chatID {
		return nil, fmt.Errorf("snapshot %s belongs to a different chat: ground again in this chat and use the new snapshot", id)
	}
	if now.Sub(snap.takenAt) > groundSnapshotTTL {
		return nil, fmt.Errorf("snapshot %s is %v old (max %v): the screen has moved on — ground again and use the new snapshot", id, now.Sub(snap.takenAt).Round(time.Second), groundSnapshotTTL)
	}
	if len(r.snapshots) > 0 && snap.seq != r.snapshots[len(r.snapshots)-1].seq {
		return nil, fmt.Errorf("snapshot %s was superseded by a newer grounding: ground again and use the new snapshot", id)
	}
	if !snap.elements[elementID] {
		return nil, fmt.Errorf("element %d is not in snapshot %s: ground again and pick an element from the new list", elementID, id)
	}
	return snap, nil
}

// groundSnapshotForTest exposes a registered snapshot's element ids
// for tests in this package.
func (r *groundSnapshotRegistry) snapshotIDs(id string) (map[int]bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.snapshots {
		if s.id == id {
			return s.elements, true
		}
	}
	return nil, false
}

// --- the tool ---

type groundTool struct {
	mu       sync.Mutex
	lastCall time.Time // per-tool min-interval only; turn/hour counting is the shared budget's

	// budget is the shared computer-use budget (F-A23W2-14):
	// ground, screenshot and input charge the same per-turn and
	// per-hour counters. budgetOwner says whether this tool's
	// BeginTurn resets it (parent registry only — a child
	// registry's tools share the budget but never reset it).
	budget      *CaptureBudget
	budgetOwner bool

	// shot is the M1 screenshot tool, reused whole: ground takes
	// its own screenshot so image and tree are the same instant,
	// and the capture (spectacle probe, sensitive check, scaling,
	// 0600 storage) is never duplicated here. It shares this
	// tool's budget (and never charges it — the grounding
	// already paid for the capture).
	shot *screenshotTool

	// Seams, overridable in tests. Defaults run the real helpers.
	execCmd         func(ctx context.Context, name string, args ...string) (string, error)
	lookPath        func(name string) (string, error)
	homeDir         func() (string, error)
	now             func() time.Time
	runHelper       func(ctx context.Context, helperPath string) ([]byte, error)
	writeHelper     func() (string, error)
	runKWinHelper   func(ctx context.Context, helperPath string) ([]byte, error)
	writeKWinHelper func() (string, error)
}

func newGroundTool() *groundTool {
	return newGroundToolWithBudget(NewCaptureBudget(), true)
}

// newGroundToolWithBudget builds a ground tool charging budget
// (nil becomes a fresh private budget). owner says whether this
// tool's BeginTurn resets the budget — true for the parent
// registry's tool, false for a child registry's. The embedded
// screenshot tool shares the budget but never owns it: its
// BeginTurn is never called (it is not a registry tool), and
// ground's BeginTurn is the reset point for both.
func newGroundToolWithBudget(budget *CaptureBudget, owner bool) *groundTool {
	if budget == nil {
		budget = NewCaptureBudget()
	}
	t := &groundTool{
		budget:      budget,
		budgetOwner: owner,
		shot:        newScreenshotToolWithBudget(budget, false),
		lookPath:    exec.LookPath,
		homeDir:     os.UserHomeDir,
		now:         time.Now,
	}
	t.execCmd = defaultExecCmd
	t.runHelper = t.defaultRunHelper
	t.writeHelper = t.defaultWriteHelper
	t.runKWinHelper = t.defaultRunKWinHelper
	t.writeKWinHelper = t.defaultWriteKWinHelper
	return t
}

func (t *groundTool) Name() string { return "ground" }

func (t *groundTool) Description() string {
	return "Ground the screen: capture a screenshot with numbered markers and return the exact name, role and bounding box of every interactive element on screen, so a click can target an element number instead of a guessed coordinate. Read-only on the desktop: it takes no input from the screen and changes nothing. DISCLOSURE: the captured image is sent to the model provider (data leaves this machine) — this call is gated and needs approval. Returns a snapshot_id plus the element list; a later click must quote that snapshot_id and an element id from it (stale snapshots are refused). When no accessibility tree is available the screenshot is still returned with an empty element list and coverage \"vision_only\" — never faked elements. Refuses on sensitive windows (password manager / bank / payment / one-time-code) and in unattended (cron) turns."
}

func (t *groundTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"region": map[string]any{
					"type":        "object",
					"description": "Crop rectangle in screen pixels (zoomed grounding of a small target). Only elements whose centre lies inside the region are listed; centre_norm is relative to the cropped view.",
					"properties": map[string]any{
						"x":      map[string]any{"type": "integer"},
						"y":      map[string]any{"type": "integer"},
						"width":  map[string]any{"type": "integer"},
						"height": map[string]any{"type": "integer"},
					},
					"required": []string{"x", "y", "width", "height"},
				},
				"max_elements": map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "description": "Most elements to list (default 60)."},
				"roles": map[string]any{
					"type":        "array",
					"description": "Only list elements with these AT-SPI roles. Default: interactive roles only (push button, button, link, entry, text, password text, check box, radio button, toggle button, push button menu, combo box, menu item).",
					"items":       map[string]any{"type": "string"},
				},
			},
		},
	}
}

// RiskClass declares the tool Network (F-M2R-6): it changes nothing
// on the desktop, but the marked image it returns is sent to the
// model provider — data leaves the machine — so a grounding is a
// gated, disclosed action, never silently auto-approved.
func (t *groundTool) RiskClass() permissions.Class { return permissions.ClassNetwork }

// BeginTurn resets the per-turn capture counter (see
// TurnResetter) — but only when this tool owns the shared
// budget (F-A23W2-14): a child registry's tools share the
// budget and must never reset it mid-parent-turn. Resetting is
// idempotent, so the parent registry's ground, screenshot and
// input tools may all call it.
func (t *groundTool) BeginTurn() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.budgetOwner {
		t.budget.BeginTurn()
	}
}

// Execute is the text-only Tool interface method: the same
// grounding, returning only the JSON summary text.
func (t *groundTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	res, err := t.ExecuteResult(ctx, args)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

// ExecuteResult grounds the screen and returns the JSON summary
// plus the marker image.
func (t *groundTool) ExecuteResult(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	invalid := func(format string, v ...any) (ToolResult, error) {
		return ToolResult{}, &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf(format, v...)}
	}
	denied := func(format string, v ...any) (ToolResult, error) {
		return ToolResult{}, &ToolError{Tool: t.Name(), Kind: "denied", Err: fmt.Errorf(format, v...)}
	}
	failed := func(format string, v ...any) (ToolResult, error) {
		return ToolResult{}, &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf(format, v...)}
	}

	// Time budget (F-M2R-10): the whole grounding — probe, tree
	// walk, KWin query, capture, markers — runs under one
	// deadline, so the worst case is the budget, not the ~25 s+
	// the sequential per-step timeouts allowed. Every subcall
	// below takes this ctx, so a hung helper or a slow capture
	// is cut off at the deadline instead of stacking timeouts.
	ctx, cancel := context.WithTimeout(ctx, groundTimeBudget)
	defer cancel()

	// Computer-use is OFF in unattended turns, no exceptions
	// (plan §11.3) — refused before anything else happens.
	if UnattendedTurnFromContext(ctx) {
		return denied("computer-use is off in unattended (cron) turns: ground refused")
	}

	// Vision gate (F-M2R-3): the grounding's value is the image,
	// and a text-only model can never read it.
	if err := visionGateError(ctx, "ground"); err != nil {
		return denied("%v", err)
	}

	var a groundArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return invalid("arguments must be a JSON object: %v", err)
		}
	}
	maxElements := groundDefaultMaxElements
	if a.MaxElements != nil {
		maxElements = *a.MaxElements
	}
	if maxElements < 1 || maxElements > groundMaxMaxElements {
		return invalid("max_elements must be between 1 and %d, got %d", groundMaxMaxElements, maxElements)
	}
	roles := defaultInteractiveRoles
	if a.Roles != nil {
		if len(a.Roles) == 0 {
			return invalid("roles must not be empty (omit it for the interactive-role default)")
		}
		for _, r := range a.Roles {
			if strings.TrimSpace(r) == "" {
				return invalid("roles must not contain an empty role")
			}
		}
		roles = a.Roles
	}
	roleSet := make(map[string]bool, len(roles))
	for _, r := range roles {
		roleSet[r] = true
	}
	if a.Region != nil {
		r := a.Region
		if r.Width <= 0 || r.Height <= 0 || r.X < 0 || r.Y < 0 {
			return invalid("region must have non-negative x/y and positive width/height, got %+v", *r)
		}
	}
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return failed("no graphical session available: neither DISPLAY nor WAYLAND_DISPLAY is set")
	}

	// Rate limits (M1 discipline): min 1s between groundings, max
	// 15 per turn, max 120 per hour.
	if err := t.checkRateLimit(); err != nil {
		return denied("%v", err)
	}

	// AT-SPI backend probe at Execute start, never cached: python3
	// present + gi.Atspi importable. A missing backend (or any
	// helper failure) is not an error — grounding degrades to
	// vision_only below — UNLESS the budget killed the walk, in
	// which case the degrade would hide the timeout: that is a
	// budget failure, reported as one. walkTime is the real
	// probe + walk duration (F-M2R-11): the markers are that old.
	tree, walkTime, _ := t.atspiTree(ctx)
	walkEnd := time.Now()
	if tree == nil && ctx.Err() == context.DeadlineExceeded {
		return failed("ground time budget of %v exceeded during the accessibility walk", groundTimeBudget)
	}

	// KWin geometry correction (F2) + the KWin sensitive source
	// (F-M2R-4): the compositor knows each window's true screen
	// position AND sees every window — including Qt apps with no
	// accessibility bridge, which are invisible to both the
	// AT-SPI tree and xdotool. The KWin query therefore runs on
	// EVERY grounding, not only when there is a tree: it is the
	// sensitive check's second source, and the only one that
	// sees Qt windows. A KWin failure (no KWin, no journal,
	// timeout) leaves the tree uncorrected: the grounding still
	// returns, with no correction field, rather than failing or
	// faking coordinates.
	var correction *groundCorrection
	var kwinOut *kwinOutput
	if kwinPath, werr := t.writeKWinHelper(); werr == nil {
		if raw, rerr := t.runKWinHelper(ctx, kwinPath); rerr == nil {
			var kout kwinOutput
			if jerr := json.Unmarshal(raw, &kout); jerr == nil {
				kwinOut = &kout
			}
		}
	}
	if tree != nil && kwinOut != nil {
		offsets := matchKWinWindows(tree, kwinOut.Windows)
		if len(offsets) > 0 {
			correction = &groundCorrection{Source: "kwin", Windows: []groundWindowShift{}}
			for i := range tree.Windows {
				if off, ok := offsets[i]; ok && (off[0] != 0 || off[1] != 0) {
					correction.Windows = append(correction.Windows, groundWindowShift{
						App: tree.Windows[i].App, DX: off[0], DY: off[1],
					})
				}
			}
			applyWindowOffsets(tree, offsets)
		}
	}

	// Sensitive-window refusal BEFORE grounding, on the AT-SPI
	// window titles when there is a tree — AT-SPI sees native
	// Wayland windows, which closes the xdotool gap the M1
	// screenshot tool reports as "partial"/"unavailable" — plus
	// the KWin sensitive flag, which sees the Qt windows the
	// tree cannot (F-M2R-4). The KWin script matches titles
	// itself and reports only the matched pattern, never the
	// title (F-M2R-15). The refusal names the category only,
	// never the title.
	if tree != nil {
		titles := make([]string, 0, len(tree.Windows))
		for _, w := range tree.Windows {
			titles = append(titles, w.Title)
		}
		if hit := FindSensitiveTitle(titles); hit != "" {
			return denied("this screen shows a %s: I will not capture, ground or describe it", SensitiveTitleCategory(hit))
		}
	}
	if kwinOut != nil {
		for _, kw := range kwinOut.Windows {
			if kw.Sens != "" {
				return denied("this screen shows a %s: I will not capture, ground or describe it", SensitiveTitleCategory(kw.Sens))
			}
		}
	}
	// Fail closed (F-M2R-5): with no AT-SPI tree AND no KWin
	// list, no source can say whether a sensitive window is
	// visible — a Qt window, a native Wayland window, any window
	// at all could be on screen unseen. The capture does NOT
	// proceed on an unverified screen; the caller must fix the
	// backend (AT-SPI or KWin) and ground again.
	if tree == nil && kwinOut == nil {
		return denied("cannot verify no sensitive window is visible (no accessibility tree and no KWin window list): I will not capture blind")
	}

	// Capture with the M1 screenshot tool at full resolution
	// (max_width 1920, scale 1.0): image pixels must BE screen
	// pixels, or the markers would not land on the boxes. The
	// screenshot tool still enforces its own sensitive check on
	// top — but the capture is NOT charged to the shared budget
	// again: this grounding already charged it once above, and
	// one grounding is one capture (F-A23W2-14). The sensitive
	// check is already done above, on strictly better sources
	// (AT-SPI + KWin) than the screenshot tool's xdotool list —
	// so the run context carries the verified mark and the
	// screenshot tool skips its own check instead of refusing
	// on a pure-Wayland session where xdotool sees nothing (its
	// fail-closed "unavailable" refusal is for standalone
	// captures with no verification at all).
	shotArgs, _ := json.Marshal(map[string]any{
		"format":    "jpeg",
		"scale":     1.0,
		"max_width": 1920,
		"region":    a.Region,
	})
	shotRes, err := t.shot.executeResult(WithSensitiveVerified(ctx), shotArgs, false)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return failed("ground time budget of %v exceeded during capture", groundTimeBudget)
		}
		return ToolResult{}, err
	}
	// Tree age (F-M2R-11): from the end of the walk to the end
	// of the capture — the bound on how stale the markers are.
	treeAge := time.Since(walkEnd)
	// Post-capture movement re-check (F-M2R-11): the walk is
	// seconds old, so a window that moved between the walk and
	// the capture misaligns every marker in it. KWin answers in
	// ~0.1 s, so re-read the window geometries now and refuse
	// the grounding when they differ from the pre-capture list
	// — the provider must ground again for fresh markers. A
	// KWin failure here cannot prove movement either way, so the
	// grounding proceeds with geometry_verified false (the
	// reported tree age is then the honest bound).
	geometryVerified := false
	if kwinOut != nil {
		if kwinPath2, werr := t.writeKWinHelper(); werr == nil {
			if raw2, rerr := t.runKWinHelper(ctx, kwinPath2); rerr == nil {
				var kout2 kwinOutput
				if jerr := json.Unmarshal(raw2, &kout2); jerr == nil {
					if sameKWinGeometry(kwinOut.Windows, kout2.Windows) {
						geometryVerified = true
					} else {
						return failed("the screen moved during grounding (a window changed geometry between the tree walk and the capture): ground again for fresh markers")
					}
				}
			}
		}
	}
	if len(shotRes.Images) != 1 {
		return failed("screenshot returned %d images, want 1", len(shotRes.Images))
	}
	var shotSummary screenshotSummary
	if err := json.Unmarshal([]byte(shotRes.Text), &shotSummary); err != nil {
		return failed("cannot parse the screenshot summary: %v", err)
	}
	img, _, err := image.Decode(bytes.NewReader(shotRes.Images[0].Data))
	if err != nil {
		return failed("cannot decode the captured image: %v", err)
	}
	imgW, imgH := img.Bounds().Dx(), img.Bounds().Dy()

	// The grounded view's origin and size in screen pixels: the
	// region when given, else the whole capture. The screenshot
	// tool may have shrunk the image under the 2 MiB cap, so the
	// image-to-view scale is derived from the actual image
	// dimensions, never assumed to be 1.
	originX, originY := 0, 0
	viewW, viewH := imgW, imgH
	if a.Region != nil {
		originX, originY = a.Region.X, a.Region.Y
		viewW, viewH = a.Region.Width, a.Region.Height
	}
	scaleX := float64(imgW) / float64(viewW)
	scaleY := float64(imgH) / float64(viewH)
	scale := math.Min(scaleX, scaleY)

	// Filter the tree into the element list (design §4.3).
	elements, walked := filterGroundElements(tree, roleSet, a.Region, maxElements)

	// Number the elements (ids from 1, in tree order) and compute
	// centres. Centre_norm is relative to the returned image
	// (0-1000 over the grounded view).
	now := t.now()
	summary := groundSummary{
		SnapshotID:       newSnapshotID(now),
		TakenAt:          now.Format(time.RFC3339),
		Screen:           groundScreen{Width: imgW, Height: imgH, Scale: scale},
		Region:           a.Region,
		Elements:         []groundElement{},
		WalkedNodes:      walked,
		Correction:       correction,
		TreeWalkMs:       walkTime.Milliseconds(),
		TreeAgeMs:        treeAge.Milliseconds(),
		GeometryVerified: geometryVerified,
	}
	if tree != nil {
		summary.Coverage = "grounded"
	} else {
		summary.Coverage = "vision_only"
	}
	// Focused window: the AT-SPI tree's focused window first
	// (it sees native Wayland windows); fall back to the
	// screenshot tool's xdotool value.
	if tree != nil && tree.FocusedWindow != nil && tree.FocusedWindow.Title != "" {
		summary.FocusedWindow = &groundWindowRef{Title: tree.FocusedWindow.Title, App: tree.FocusedWindow.App}
	} else if shotSummary.FocusedWindow != "" {
		summary.FocusedWindow = &groundWindowRef{Title: shotSummary.FocusedWindow}
	}
	for i, el := range elements {
		id := i + 1
		cx, cy := el.X+el.W/2, el.Y+el.H/2
		summary.Elements = append(summary.Elements, groundElement{
			ID:     id,
			Role:   el.Role,
			Name:   el.Name,
			Box:    groundBox{X: el.X, Y: el.Y, W: el.W, H: el.H},
			Centre: groundPoint{X: cx, Y: cy},
			CentreNorm: groundPoint{
				X: (cx - originX) * 1000 / viewW,
				Y: (cy - originY) * 1000 / viewH,
			},
		})
	}

	// Draw the numbered markers on the captured image.
	annotated := drawMarkers(img, summary.Elements, originX, originY, scaleX, scaleY)
	encoded, err := encodeJPEG(annotated, 85)
	if err != nil {
		return failed("cannot encode the marked image: %v", err)
	}
	// Shrink loop (same shape as the screenshot tool's): markers
	// scale with the image, so relative alignment survives.
	for attempt := 0; len(encoded) > MaxImageBytes && attempt < screenshotMaxScaleTries; attempt++ {
		if attempt < 2 {
			encoded, err = encodeJPEG(annotated, 70-15*attempt)
			if err != nil {
				return failed("cannot encode the marked image: %v", err)
			}
			continue
		}
		annotated = scaleImage(annotated, 0.5)
		encoded, err = encodeJPEG(annotated, 85)
		if err != nil {
			return failed("cannot encode the marked image: %v", err)
		}
	}
	if len(encoded) > MaxImageBytes {
		return failed("marked image is %d bytes even after shrinking, over the %d-byte cap", len(encoded), MaxImageBytes)
	}

	// Register the snapshot BEFORE returning: the provider may
	// click an element from this grounding immediately, and the
	// registry is what makes that click valid. Registration is
	// what the last-3 eviction applies to — a 4th grounding
	// evicts the oldest snapshot here. The registered elements
	// carry their centres, so the input tool clicks the
	// registry's centre, never a model-typed coordinate. The
	// snapshot is bound to this turn's chat (F-A23W2-19): a
	// click from any other chat refuses, even on an id collision.
	// They also carry their window's geometry (F-M2R-8): the input
	// tool refuses the click when that window has moved or
	// resized since the grounding. The tree's windows are
	// KWin-corrected by now, so the binding is on compositor
	// truth, not window-relative coordinates.
	elemWindows := map[int]WindowGeometry{}
	if tree != nil {
		for i, el := range elements {
			if el.Win == nil {
				continue
			}
			wi := *el.Win
			if wi < 0 || wi >= len(tree.Windows) {
				continue
			}
			w := tree.Windows[wi]
			elemWindows[i+1] = WindowGeometry{App: w.App, X: w.X, Y: w.Y, W: w.W, H: w.H}
		}
	}
	groundSnapshots.register(summary.SnapshotID, chatIDFromContext(ctx), now, summary.Elements, elemWindows)

	text, err := json.Marshal(summary)
	if err != nil {
		return failed("cannot encode the grounding summary: %v", err)
	}
	return ToolResult{
		Text:   string(text),
		Images: []ToolImage{{Data: encoded, MIMEType: "image/jpeg"}},
	}, nil
}

// probeHelper reports whether the AT-SPI backend is present: a
// python3 binary plus an importable gi.Atspi. It runs at every
// Execute start and is never cached. The import does NOT call
// Atspi.init() — that happens in the helper run itself, under the
// helper timeout — so the probe stays fast and cannot hang on a
// session without a registry.
func (t *groundTool) probeHelper(ctx context.Context) bool {
	if _, err := t.lookPath("python3"); err != nil {
		return false
	}
	pctx, cancel := context.WithTimeout(ctx, groundHelperTimeout)
	defer cancel()
	_, err := t.execCmd(pctx, "python3", "-c", "import gi; gi.require_version('Atspi', '2.0'); from gi.repository import Atspi")
	return err == nil
}

// defaultWriteHelper writes the embedded AT-SPI helper to
// ~/.orion/ground-atspi-helper.py (0600, atomic temp+rename) and
// returns its path. An identical existing file is left untouched.
func (t *groundTool) defaultWriteHelper() (string, error) {
	return installEmbeddedHelper(t.homeDir, groundHelperFile, atspiHelperSource)
}

// defaultWriteKWinHelper writes the embedded KWin helper to
// ~/.orion/ground-kwin-helper.py (0600, atomic temp+rename) and
// returns its path. Same shape as defaultWriteHelper.
func (t *groundTool) defaultWriteKWinHelper() (string, error) {
	return installEmbeddedHelper(t.homeDir, groundKWinHelperFile, kwinHelperSource)
}

// installEmbeddedHelper installs one embedded helper script under
// ~/.orion/ (0600, atomic temp+rename) and returns its path. An
// identical existing file is left untouched. It is the shared
// installer for ground's and input's helpers (input_kwin.py),
// so the install semantics cannot drift between the two tools.
func installEmbeddedHelper(homeDir func() (string, error), file, source string) (string, error) {
	home, err := homeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("cannot determine the home directory: %v", err)
	}
	dir := filepath.Join(home, ".orion")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("cannot create %s: %v", dir, err)
	}
	path := filepath.Join(dir, file)
	if cur, err := os.ReadFile(path); err == nil && string(cur) == source {
		return path, nil
	}
	tmp, err := os.CreateTemp(dir, "ground-helper-*")
	if err != nil {
		return "", fmt.Errorf("cannot stage the helper %s: %v", file, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(source); err != nil {
		tmp.Close()
		return "", fmt.Errorf("cannot write the helper %s: %v", file, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("cannot write the helper %s: %v", file, err)
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		return "", fmt.Errorf("cannot chmod the helper %s: %v", file, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", fmt.Errorf("cannot install the helper %s: %v", file, err)
	}
	return path, nil
}

// atspiTree probes the AT-SPI backend and runs the helper,
// returning the parsed tree and how long the probe + walk took
// (real wall clock — the tree's age is a staleness fact, never a
// fake-clock value). (nil, 0, nil) when the backend is missing —
// the caller degrades (ground: vision_only; input: xdotool
// fallback). Any helper failure (write, run, parse) is an error.
// Extracted from ExecuteResult so the input tool's
// sensitive-window check runs the same tree walk instead of a
// second, drifting copy of it.
func (t *groundTool) atspiTree(ctx context.Context) (*atspiOutput, time.Duration, error) {
	start := time.Now()
	if !t.probeHelper(ctx) {
		return nil, time.Since(start), nil
	}
	helperPath, err := t.writeHelper()
	if err != nil {
		return nil, time.Since(start), err
	}
	raw, err := t.runHelper(ctx, helperPath)
	if err != nil {
		return nil, time.Since(start), err
	}
	var out atspiOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, time.Since(start), fmt.Errorf("AT-SPI helper output does not parse: %v", err)
	}
	return &out, time.Since(start), nil
}

// sameKWinGeometry reports whether two KWin window lists describe
// the same windows at the same geometries: the same multiset of
// (app class, frame geometry, client geometry, minimized). The
// active flag and the sensitive flag are excluded — either can
// change without anything moving. Used by the post-capture
// movement re-check (F-M2R-11): a window that moved, resized,
// opened, closed, minimized or restored between the walk and
// the capture makes the lists differ.
func sameKWinGeometry(a, b []kwinWindow) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(w kwinWindow) string {
		return fmt.Sprintf("%s|%v|%v|%v", w.Class, w.FG, w.CG, w.Minimized)
	}
	counts := make(map[string]int, len(a))
	for _, w := range a {
		counts[key(w)]++
	}
	for _, w := range b {
		k := key(w)
		counts[k]--
		if counts[k] < 0 {
			return false
		}
	}
	return true
}

// defaultRunHelper runs the helper with the depth/node caps and
// the helper timeout, and returns its stdout. Any failure (exit
// code, timeout, empty output) is an error — the caller degrades
// to vision_only.
func (t *groundTool) defaultRunHelper(ctx context.Context, helperPath string) ([]byte, error) {
	hctx, cancel := context.WithTimeout(ctx, groundHelperTimeout)
	defer cancel()
	cmd := exec.CommandContext(hctx, "python3", helperPath,
		strconv.Itoa(groundMaxDepth), strconv.Itoa(groundMaxNodes))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("AT-SPI helper failed: %v (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	if stdout.Len() == 0 {
		return nil, fmt.Errorf("AT-SPI helper produced no output")
	}
	return stdout.Bytes(), nil
}

// defaultRunKWinHelper runs the KWin helper with the KWin timeout
// and returns its stdout. Any failure (exit code, timeout, empty
// output) is an error — the caller grounds uncorrected. The
// sensitive substrings travel as argv so the helper's KWin script
// can run the sensitive check itself (F-M2R-15: titles never
// leave KWin — only the matched pattern does).
func (t *groundTool) defaultRunKWinHelper(ctx context.Context, helperPath string) ([]byte, error) {
	hctx, cancel := context.WithTimeout(ctx, groundKWinTimeout())
	defer cancel()
	args := append([]string{helperPath}, sensitiveWindowTitles...)
	cmd := exec.CommandContext(hctx, "python3", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("KWin helper failed: %v (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	if stdout.Len() == 0 {
		return nil, fmt.Errorf("KWin helper produced no output")
	}
	return stdout.Bytes(), nil
}

// checkRateLimit enforces the min-interval (per tool instance)
// and the per-turn + per-hour limits (the SHARED capture budget,
// F-A23W2-14) and records the call. The budget refusal is
// wrapped in ground's own message, so the model hears the same
// "at most 15 groundings per turn" as before.
func (t *groundTool) checkRateLimit() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if !t.lastCall.IsZero() && now.Sub(t.lastCall) < groundMinInterval {
		return fmt.Errorf("ground rate limit: at most one grounding per second (last grounding %v ago)", now.Sub(t.lastCall).Round(time.Millisecond))
	}
	if err := t.budget.Charge(now, groundMaxPerTurn, groundMaxPerHour); err != nil {
		if errors.Is(err, errCaptureBudgetTurn) {
			return fmt.Errorf("ground rate limit: at most %d groundings per turn", groundMaxPerTurn)
		}
		return fmt.Errorf("ground rate limit: at most %d groundings per hour", groundMaxPerHour)
	}
	t.lastCall = now
	return nil
}

// filterGroundElements applies the design §4.3 filters to the
// tree, in tree order, capped at maxElements. A nil tree yields no
// elements (vision_only). Returns the kept elements and the walked
// node count (0 without a tree).
func filterGroundElements(tree *atspiOutput, roleSet map[string]bool, region *region, maxElements int) ([]atspiElement, int) {
	if tree == nil {
		return nil, 0
	}
	kept := []atspiElement{}
	for _, el := range tree.Elements {
		if len(kept) >= maxElements {
			break
		}
		if !roleSet[el.Role] {
			continue
		}
		// Hidden elements: AT-SPI keeps closed popups' nodes in
		// the tree (calendar days, session buttons), and without
		// this filter they exhaust the max_elements cap before
		// the focused app's elements are reached (live 2026-11-15
		// 20:26: 60/60 listed were hidden plasmashell elements).
		// SHOWING separates visible from hidden cleanly (probe
		// 2026-11-15 20:27: 7/7). A missing signal (nil) is kept.
		if el.Showing != nil && !*el.Showing {
			continue
		}
		// Degenerate box: offscreen (-1,-1) or 1x1 placeholders
		// never reach the provider.
		if el.W <= 1 || el.H <= 1 || el.X < 0 || el.Y < 0 {
			continue
		}
		// Region zoom: only elements whose centre lies inside
		// the region are listed.
		if region != nil {
			cx, cy := el.X+el.W/2, el.Y+el.H/2
			if cx < region.X || cx >= region.X+region.Width ||
				cy < region.Y || cy >= region.Y+region.Height {
				continue
			}
		}
		// Empty name -> the role is the name, never a guess.
		if strings.TrimSpace(el.Name) == "" {
			el.Name = el.Role
		}
		// Data minimisation (F-M2R-17): the model gets a label,
		// not the full accessible name — long names can carry
		// document titles or message text. Over the cap, cut to
		// 59 runes + "…" (runes, never bytes: no split UTF-8).
		if runes := []rune(el.Name); len(runes) > groundMaxNameRunes {
			el.Name = string(runes[:groundMaxNameRunes-1]) + "…"
		}
		kept = append(kept, el)
	}
	return kept, tree.WalkedNodes
}

// newSnapshotID builds a unique snapshot id like
// "g-20261115-182033-7f3a91c4d2e8b506": a timestamp plus 8 random
// bytes (64-bit entropy, F-A23W2-19 — the old 2-byte suffix gave
// only 65,536 possibilities per second, so two chats grounding
// in the same second could collide and the registry's first
// match would serve the wrong chat's snapshot). Two groundings
// in the same second still differ.
func newSnapshotID(now time.Time) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Randomness failing must not fail the grounding: fall
		// back to a nanosecond suffix, still unique per call.
		return fmt.Sprintf("g-%s-%d", now.Format("20060102-150405"), now.Nanosecond())
	}
	return fmt.Sprintf("g-%s-%s", now.Format("20060102-150405"), hex.EncodeToString(b[:]))
}

// --- markers (stdlib image/draw only) ---

// digitFont is a 5x7 bitmap font for the digits 0-9. Each digit is
// 7 rows of 5 bits (most significant bit left). The stdlib has no
// text rendering, so the marker numbers are drawn from this font.
var digitFont = [10][7]uint8{
	{0x0E, 0x11, 0x13, 0x15, 0x19, 0x11, 0x0E}, // 0
	{0x04, 0x0C, 0x04, 0x04, 0x04, 0x04, 0x0E}, // 1
	{0x0E, 0x11, 0x01, 0x06, 0x08, 0x10, 0x1F}, // 2
	{0x1F, 0x02, 0x04, 0x02, 0x01, 0x11, 0x0E}, // 3
	{0x02, 0x06, 0x0A, 0x12, 0x1F, 0x02, 0x02}, // 4
	{0x1F, 0x10, 0x1E, 0x01, 0x01, 0x11, 0x0E}, // 5
	{0x06, 0x08, 0x10, 0x1E, 0x11, 0x11, 0x0E}, // 6
	{0x1F, 0x01, 0x02, 0x04, 0x08, 0x08, 0x08}, // 7
	{0x0E, 0x11, 0x11, 0x0E, 0x11, 0x11, 0x0E}, // 8
	{0x0E, 0x11, 0x11, 0x0F, 0x01, 0x02, 0x0C}, // 9
}

const (
	markerPixel = 2 // each font pixel is a 2x2 block
	markerPad   = 3 // badge padding around the digits
)

var (
	markerBadgeColor  = color.RGBA{255, 255, 255, 255} // white badge
	markerBorderColor = color.RGBA{0, 0, 0, 255}       // black border + digits
)

// markerSize returns the badge width/height for a decimal number.
func markerSize(digits int) (w, h int) {
	w = digits*5*markerPixel + (digits-1)*markerPixel + 2*markerPad
	h = 7*markerPixel + 2*markerPad
	return w, h
}

// markerRect returns where the badge for element el goes in image
// coordinates: centred on the element's centre, clamped inside
// the element's box when the badge fits in it, else clamped to
// the image bounds. The badge's CENTRE is always the element's
// centre when the box holds it, so the number a provider reads
// sits on the element it names.
func markerRect(el groundElement, originX, originY int, scaleX, scaleY float64, imgW, imgH int) image.Rectangle {
	w, h := markerSize(len(strconv.Itoa(el.ID)))
	cx := int(float64(el.Centre.X-originX) * scaleX)
	cy := int(float64(el.Centre.Y-originY) * scaleY)
	x := cx - w/2
	y := cy - h/2
	// Clamp inside the element's box (in image coordinates) when
	// the badge fits — the marker must fall inside the box.
	bx0 := int(float64(el.Box.X-originX) * scaleX)
	by0 := int(float64(el.Box.Y-originY) * scaleY)
	bx1 := int(float64(el.Box.X+el.Box.W-originX) * scaleX)
	by1 := int(float64(el.Box.Y+el.Box.H-originY) * scaleY)
	if bx1-bx0 >= w && by1-by0 >= h {
		if x < bx0 {
			x = bx0
		}
		if x+w > bx1 {
			x = bx1 - w
		}
		if y < by0 {
			y = by0
		}
		if y+h > by1 {
			y = by1 - h
		}
	}
	// Always keep the badge on the image.
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x+w > imgW {
		x = imgW - w
	}
	if y+h > imgH {
		y = imgH - h
	}
	return image.Rect(x, y, x+w, y+h)
}

// drawMarkers draws the numbered badges for elements onto a copy
// of img and returns it. Elements are drawn in id order; later
// badges may overlap earlier ones on crowded screens, but each
// badge's centre stays on its element.
func drawMarkers(img image.Image, elements []groundElement, originX, originY int, scaleX, scaleY float64) image.Image {
	dst := image.NewRGBA(img.Bounds())
	draw.Draw(dst, dst.Bounds(), img, img.Bounds().Min, draw.Src)
	imgW, imgH := dst.Bounds().Dx(), dst.Bounds().Dy()
	for _, el := range elements {
		r := markerRect(el, originX, originY, scaleX, scaleY, imgW, imgH)
		// Badge: white fill, 1px black border.
		draw.Draw(dst, r, &image.Uniform{markerBadgeColor}, image.Point{}, draw.Src)
		for x := r.Min.X; x < r.Max.X; x++ {
			dst.Set(x, r.Min.Y, markerBorderColor)
			dst.Set(x, r.Max.Y-1, markerBorderColor)
		}
		for y := r.Min.Y; y < r.Max.Y; y++ {
			dst.Set(r.Min.X, y, markerBorderColor)
			dst.Set(r.Max.X-1, y, markerBorderColor)
		}
		// Digits, left to right, 2x2 blocks.
		num := strconv.Itoa(el.ID)
		dx := r.Min.X + markerPad
		dy := r.Min.Y + markerPad
		for i, ch := range num {
			d := digitFont[ch-'0']
			ox := dx + i*(5*markerPixel+markerPixel)
			for row := 0; row < 7; row++ {
				for col := 0; col < 5; col++ {
					if d[row]&(1<<(4-col)) != 0 {
						for py := 0; py < markerPixel; py++ {
							for px := 0; px < markerPixel; px++ {
								dst.Set(ox+col*markerPixel+px, dy+row*markerPixel+py, markerBorderColor)
							}
						}
					}
				}
			}
		}
	}
	return dst
}
