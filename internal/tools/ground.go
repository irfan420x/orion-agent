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
// so image and tree are ~0.4 s apart (probe: 140 nodes / 0.38 s),
// not the same millisecond. A window that moves in that gap
// misaligns its marker by that movement; the snapshot registry
// (§5 stale-refusal) bounds the damage to one grounding.
//
// Safety (design §4.3, enforced in code):
//   - interactive roles only (default set below); degenerate
//     boxes (w<=1 || h<=1 || x<0 || y<0) excluded;
//   - password fields: role + box only, value/text never read,
//     never in results or logs (the helper cannot read values);
//   - sensitive-window refusal BEFORE grounding, using the AT-SPI
//     window titles (which see native Wayland windows xdotool
//     cannot) and falling back to the screenshot tool's xdotool
//     check when there is no tree;
//   - empty name -> the role is the name, never a guessed name.
//   - read-only, RiskClass Safe; refused in unattended turns.
//
// Stale-refusal (§5): every grounding registers its snapshot_id +
// element list in the package-level snapshot registry
// (groundSnapshots, max 3 retained — the same last-3 retention as
// M1's screenshot history eviction, so an evicted snapshot's
// elements are no longer clickable). ValidateGroundSnapshot,
// used by the future M3 mouse tool, refuses when the snapshot is
// >60 s old, superseded by a newer grounding, evicted, or the
// element id is not in that snapshot. Ground itself never clicks.
//
// Markers: Go stdlib image/draw only — no new deps. The stdlib
// has no text rendering, so a tiny 5x7 bitmap digit font is
// embedded below and each marker is a small readable number badge
// drawn at the element's centre, clamped inside the element's box
// when the box is big enough to hold it.
//
// Rate limits: the M1 screenshot discipline (min 1 s between
// captures, max 15 per turn, max 120 per hour), enforced here on
// ground calls; the inner screenshot capture enforces its own
// identical limits on top, so a ground call consumes one slot of
// each. The two counters are separate (one per tool instance), so
// the combined worst case is 30 captures per turn — still bounded.
package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
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

func init() {
	RegisterFactory("ground", func(deps ToolDeps) (Tool, error) {
		return newGroundTool(), nil
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
	// This is the helper's own safety bound, not the tool's 3 s
	// design time budget — the measured whole grounding is ~7.5 s,
	// over that budget; see the fix report for the raise
	// recommendation.
	groundHelperTimeout      = 10 * time.Second
	groundDefaultMaxElements = 60
	groundMaxMaxElements     = 200
	groundSnapshotTTL        = 60 * time.Second
	groundMaxSnapshots       = 3 // mirrors M1's last-3 image eviction
	groundHelperFile         = "ground-atspi-helper.py"
)

// Ground rate limits (M1 screenshot discipline, plan §4.1).
const (
	groundMinInterval = time.Second
	groundMaxPerTurn  = 15
	groundMaxPerHour  = 120
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
	SnapshotID    string           `json:"snapshot_id"`
	TakenAt       string           `json:"taken_at"`
	Screen        groundScreen     `json:"screen"`
	Region        *region          `json:"region,omitempty"`
	FocusedWindow *groundWindowRef `json:"focused_window,omitempty"`
	Elements      []groundElement  `json:"elements"`
	Coverage      string           `json:"coverage"` // grounded | vision_only
	WalkedNodes   int              `json:"walked_nodes,omitempty"`
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
	// Showing is the AT-SPI SHOWING state reported by the helper.
	// Nil means the helper did not report it (state query failed):
	// the element is kept — a missing signal never drops an
	// element. False means hidden: the element stays in the tree
	// (closed popups keep their nodes) but is not on screen.
	Showing *bool `json:"showing,omitempty"`
}

// --- snapshot registry (stale-refusal, design §5) ---

// groundSnapshot is one grounding's registered state: the element
// ids the future M3 mouse tool may click, and the sequence number
// that decides supersession (only the newest snapshot is live).
type groundSnapshot struct {
	id       string
	takenAt  time.Time
	elements map[int]bool
	seq      int64
}

// groundSnapshotRegistry keeps the last groundMaxSnapshots
// groundings. Anything older is evicted and its elements are no
// longer clickable — the registry is the enforcement point for
// the M1 last-3 eviction rule applied to grounding snapshots.
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
// number.
func (r *groundSnapshotRegistry) register(id string, takenAt time.Time, elementIDs []int) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	els := make(map[int]bool, len(elementIDs))
	for _, e := range elementIDs {
		els[e] = true
	}
	r.snapshots = append(r.snapshots, &groundSnapshot{id: id, takenAt: takenAt, elements: els, seq: r.seq})
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

// ValidateGroundSnapshot is the M3 mouse tool's gate: it refuses a
// click when (a) the snapshot is unknown or evicted, (b) it is
// older than the 60 s TTL, (c) a newer grounding has superseded
// it, or (d) the element id is not in that snapshot. The error
// always tells the provider to ground again — never a coordinate.
func ValidateGroundSnapshot(id string, elementID int) error {
	return groundSnapshots.validate(id, elementID, time.Now())
}

func (r *groundSnapshotRegistry) validate(id string, elementID int, now time.Time) error {
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
		return fmt.Errorf("snapshot %s is not available (unknown or evicted — only the last %d groundings are kept): ground again and use the new snapshot", id, groundMaxSnapshots)
	}
	if now.Sub(snap.takenAt) > groundSnapshotTTL {
		return fmt.Errorf("snapshot %s is %v old (max %v): the screen has moved on — ground again and use the new snapshot", id, now.Sub(snap.takenAt).Round(time.Second), groundSnapshotTTL)
	}
	if len(r.snapshots) > 0 && snap.seq != r.snapshots[len(r.snapshots)-1].seq {
		return fmt.Errorf("snapshot %s was superseded by a newer grounding: ground again and use the new snapshot", id)
	}
	if !snap.elements[elementID] {
		return fmt.Errorf("element %d is not in snapshot %s: ground again and pick an element from the new list", elementID, id)
	}
	return nil
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
	mu        sync.Mutex
	lastCall  time.Time
	hourCalls []time.Time
	turnCalls int

	// shot is the M1 screenshot tool, reused whole: ground takes
	// its own screenshot so image and tree are the same instant,
	// and the capture (spectacle probe, sensitive check, scaling,
	// 0600 storage) is never duplicated here.
	shot *screenshotTool

	// Seams, overridable in tests. Defaults run the real helper.
	execCmd     func(ctx context.Context, name string, args ...string) (string, error)
	lookPath    func(name string) (string, error)
	homeDir     func() (string, error)
	now         func() time.Time
	runHelper   func(ctx context.Context, helperPath string) ([]byte, error)
	writeHelper func() (string, error)
}

func newGroundTool() *groundTool {
	t := &groundTool{
		shot:     newScreenshotTool(),
		lookPath: exec.LookPath,
		homeDir:  os.UserHomeDir,
		now:      time.Now,
	}
	t.execCmd = defaultExecCmd
	t.runHelper = t.defaultRunHelper
	t.writeHelper = t.defaultWriteHelper
	return t
}

func (t *groundTool) Name() string { return "ground" }

func (t *groundTool) Description() string {
	return "Ground the screen: capture a screenshot with numbered markers and return the exact name, role and bounding box of every interactive element on screen, so a click can target an element number instead of a guessed coordinate. Read-only: it takes no input from the screen and changes nothing. Returns a snapshot_id plus the element list; a later click must quote that snapshot_id and an element id from it (stale snapshots are refused). When no accessibility tree is available the screenshot is still returned with an empty element list and coverage \"vision_only\" — never faked elements. Refuses on sensitive windows (password manager / bank / payment / one-time-code) and in unattended (cron) turns."
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

// RiskClass declares the tool safe: read-only — it looks at the
// screen and the accessibility tree and changes nothing.
func (t *groundTool) RiskClass() permissions.Class { return permissions.ClassSafe }

// BeginTurn resets the per-turn capture counter (see TurnResetter).
func (t *groundTool) BeginTurn() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.turnCalls = 0
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

	// Computer-use is OFF in unattended turns, no exceptions
	// (plan §11.3) — refused before anything else happens.
	if UnattendedTurnFromContext(ctx) {
		return denied("computer-use is off in unattended (cron) turns: ground refused")
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
	// present + gi.Atspi importable. A missing backend is not an
	// error — grounding degrades to vision_only below.
	helperOK := t.probeHelper(ctx)

	var tree *atspiOutput
	if helperOK {
		helperPath, werr := t.writeHelper()
		if werr == nil {
			raw, rerr := t.runHelper(ctx, helperPath)
			if rerr == nil {
				var out atspiOutput
				if jerr := json.Unmarshal(raw, &out); jerr == nil {
					tree = &out
				}
			}
		}
		// Any helper failure (write, run, parse) leaves tree nil:
		// vision_only, never a partial or faked element list.
	}

	// Sensitive-window refusal BEFORE grounding, on the AT-SPI
	// window titles when there is a tree — AT-SPI sees native
	// Wayland windows, which closes the xdotool gap the M1
	// screenshot tool reports as "partial"/"unavailable". With no
	// tree the screenshot tool's own check (inside ExecuteResult
	// below) still refuses on what xdotool can see. The refusal
	// names the category only, never the title.
	if tree != nil {
		titles := make([]string, 0, len(tree.Windows))
		for _, w := range tree.Windows {
			titles = append(titles, w.Title)
		}
		if hit := FindSensitiveTitle(titles); hit != "" {
			return denied("this screen shows a %s: I will not capture, ground or describe it", SensitiveTitleCategory(hit))
		}
	}

	// Capture with the M1 screenshot tool at full resolution
	// (max_width 1920, scale 1.0): image pixels must BE screen
	// pixels, or the markers would not land on the boxes. The
	// screenshot tool still enforces its own rate limits and its
	// own sensitive check on top.
	shotArgs, _ := json.Marshal(map[string]any{
		"format":    "jpeg",
		"scale":     1.0,
		"max_width": 1920,
		"region":    a.Region,
	})
	shotRes, err := t.shot.ExecuteResult(ctx, shotArgs)
	if err != nil {
		return ToolResult{}, err
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
		SnapshotID:  newSnapshotID(now),
		TakenAt:     now.Format(time.RFC3339),
		Screen:      groundScreen{Width: imgW, Height: imgH, Scale: scale},
		Region:      a.Region,
		Elements:    []groundElement{},
		WalkedNodes: walked,
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
	ids := make([]int, 0, len(elements))
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
		ids = append(ids, id)
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
	// evicts the oldest snapshot here.
	groundSnapshots.register(summary.SnapshotID, now, ids)

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

// defaultWriteHelper writes the embedded helper to
// ~/.orion/ground-atspi-helper.py (0600, atomic temp+rename) and
// returns its path. An identical existing file is left untouched.
func (t *groundTool) defaultWriteHelper() (string, error) {
	home, err := t.homeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("cannot determine the home directory: %v", err)
	}
	dir := filepath.Join(home, ".orion")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("cannot create %s: %v", dir, err)
	}
	path := filepath.Join(dir, groundHelperFile)
	if cur, err := os.ReadFile(path); err == nil && string(cur) == atspiHelperSource {
		return path, nil
	}
	tmp, err := os.CreateTemp(dir, "ground-helper-*")
	if err != nil {
		return "", fmt.Errorf("cannot stage the AT-SPI helper: %v", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(atspiHelperSource); err != nil {
		tmp.Close()
		return "", fmt.Errorf("cannot write the AT-SPI helper: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("cannot write the AT-SPI helper: %v", err)
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		return "", fmt.Errorf("cannot chmod the AT-SPI helper: %v", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", fmt.Errorf("cannot install the AT-SPI helper: %v", err)
	}
	return path, nil
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

// checkRateLimit enforces the min-interval, per-turn and per-hour
// limits and records the call. Same shape as the screenshot
// tool's, with ground's own counters.
func (t *groundTool) checkRateLimit() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if !t.lastCall.IsZero() && now.Sub(t.lastCall) < groundMinInterval {
		return fmt.Errorf("ground rate limit: at most one grounding per second (last grounding %v ago)", now.Sub(t.lastCall).Round(time.Millisecond))
	}
	if t.turnCalls >= groundMaxPerTurn {
		return fmt.Errorf("ground rate limit: at most %d groundings per turn", groundMaxPerTurn)
	}
	cutoff := now.Add(-time.Hour)
	kept := t.hourCalls[:0]
	for _, ts := range t.hourCalls {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	t.hourCalls = kept
	if len(t.hourCalls) >= groundMaxPerHour {
		return fmt.Errorf("ground rate limit: at most %d groundings per hour", groundMaxPerHour)
	}
	t.lastCall = now
	t.turnCalls++
	t.hourCalls = append(t.hourCalls, now)
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
		kept = append(kept, el)
	}
	return kept, tree.WalkedNodes
}

// newSnapshotID builds a unique snapshot id like
// "g-20261115-182033-7f3a": a timestamp plus 2 random bytes, so
// two groundings in the same second still differ.
func newSnapshotID(now time.Time) string {
	var b [2]byte
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
