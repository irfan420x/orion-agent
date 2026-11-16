// Package tools — screenshot (computer-use tool 1 of 5, M1).
//
// screenshot captures the desktop and returns the image to the model
// as a ToolResult image, plus a JSON summary (focused window, cursor
// position, backend, file). It is read-only on the desktop —
// RiskClass Network, because the image is sent to the model provider
// (F-M2R-6) — and it takes NO input: no click, no key, no window
// control (plan §6.2 M1).
//
// Backends, in order (M0 verdict + M1 probes, all live on the laptop):
//  1. spectacle — proven non-interactive, no portal dialog. This is
//     the routine path and the only implemented backend in M1.
//  2. portal ScreenCast / PipeWire — proven end-to-end by the M1 W2
//     probe (granted node 92, BGRA 1920x1080 frames via the C
//     consumer ~/orion-computer-use/m1/m1-pw-capture.c), but NOT
//     implemented here: there is no usable CLI/GStreamer/ffmpeg
//     shortcut on the stock install, so a ScreenCast backend needs
//     a native PipeWire consumer — Go cgo against libpipewire, or a
//     vendored C helper with a build story for a repo that is
//     otherwise pure stdlib with zero external deps. That
//     consumer-home decision is still open (flagged in the M1
//     report); until it is made, the backend returns clean
//     invalid_args "not supported yet" instead of a broken or
//     faked capture. ScreenCast also costs the user a portal dialog
//     click on EVERY new session (grants do not persist — M1 W1),
//     so it could never be the routine path anyway: it belongs to
//     live-stream/continuous capture, not single screenshots.
//  3. KWin ScreenShot2 D-Bus — dead on this session (UnknownObject),
//     deprioritised, not attempted.
//
// Cursor: screenshots on this laptop do NOT render the pointer
// (proven by the M1 W3 probe for both spectacle and PipeWire
// frames), so the tool reports the cursor position NUMERICALLY
// (xdotool getmouselocation) and marks cursor_visible_in_image
// false — the model must never be told to look for the pointer in
// the image. Focused window comes from xdotool too (XWayland
// windows only; a native Wayland window reports as unknown).
//
// Safety (plan §5.3, enforced in code, not prompt text):
//   - a password-manager / bank / payment / OTP window focused or
//     visible refuses the capture before anything is taken, and
//     the refusal never names or stores the sensitive content;
//   - screenshots stay local: files are 0600 in
//     ~/.orion/screenshots/, auto-deleted after 24h, and never
//     sent anywhere except inside the provider request that asked
//     for them;
//   - cron/unattended turns refuse outright (plan §11.3).
//
// Rate limits (plan §4.1, enforced here): min 1s between captures,
// max 15 per turn, max 120 per hour. The min interval is per tool
// instance; the per-turn/hour counting is the SHARED capture
// budget (F-A23W2-14 / F-M2R-16) — ground, screenshot and input
// charge one counter, parent and children together. The per-turn
// counter resets on BeginTurn, which the agent loop calls once
// per turn — but only for the budget's owner (see
// CaptureBudget): a child registry's tools never reset it.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math/rand"
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

func init() {
	RegisterFactory("screenshot", func(deps ToolDeps) (Tool, error) {
		return newScreenshotToolWithBudget(deps.CaptureBudget, deps.CaptureBudgetOwner), nil
	})
}

// Screenshot limits (plan §4.1). The per-turn/hour numbers alias
// the shared capture budget's (F-A23W2-14): one limit across
// ground + screenshot + input, not one per tool. The min
// interval stays per tool instance.
const (
	screenshotMinInterval   = time.Second
	screenshotMaxPerTurn    = captureMaxPerTurn
	screenshotMaxPerHour    = captureMaxPerHour
	screenshotDefaultWidth  = 1280
	screenshotMinWidth      = 640
	screenshotMaxWidth      = 1920
	screenshotRetention     = 24 * time.Hour
	screenshotMaxScaleTries = 4
	// screenshotRawRetention bounds crash leftovers (F-M2R-33):
	// the raw spectacle PNG is removed by defer on a clean return,
	// but a killed process (or a crash between capture and return)
	// never runs the defer. Raw files therefore get a much shorter
	// retention than finished screenshots: a raw capture older than
	// an hour cannot belong to a live capture (one capture takes
	// seconds), so the next capture's prune sweep deletes it. The
	// "raw file never remains" claim is now true within an hour
	// even for kills, not only on clean returns. Finished
	// screenshot-* files keep the 24 h retention.
	screenshotRawRetention = time.Hour
)

// screenshotArgs is the tool's JSON arguments.
type screenshotArgs struct {
	Display  string   `json:"display"`   // primary | all | <name>
	Region   *region  `json:"region"`    // crop in pixels, optional
	Scale    *float64 `json:"scale"`     // 0.25–1.0, default 1.0
	Format   string   `json:"format"`    // jpeg (default) | png
	MaxWidth *int     `json:"max_width"` // default 1280, range 640–1920
}

// region is a pixel crop rectangle.
type region struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// screenshotSummary is the JSON text returned with the image.
type screenshotSummary struct {
	Backend              string     `json:"backend"`
	File                 string     `json:"file"`
	Width                int        `json:"width"`
	Height               int        `json:"height"`
	Format               string     `json:"format"`
	SizeBytes            int        `json:"size_bytes"`
	Display              string     `json:"display"`
	FocusedWindow        string     `json:"focused_window,omitempty"`
	Cursor               *cursorPos `json:"cursor,omitempty"`
	CursorVisibleInImage bool       `json:"cursor_visible_in_image"`
	SensitiveCheck       string     `json:"sensitive_check"` // ok | unavailable | partial
}

// cursorPos is a numeric cursor position: screenshots on this laptop
// never render the pointer, so this — not the image — is the
// position proof.
type cursorPos struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type screenshotTool struct {
	mu       sync.Mutex
	lastCall time.Time // per-tool min-interval only; turn/hour counting is the shared budget's

	// budget is the shared computer-use budget (F-A23W2-14):
	// ground, screenshot and input charge the same per-turn and
	// per-hour counters. budgetOwner says whether this tool's
	// BeginTurn resets it (parent registry only — a child
	// registry's tools share the budget but never reset it).
	budget      *CaptureBudget
	budgetOwner bool

	// Seams, overridable in tests. Defaults run the real backends.
	execCmd      func(ctx context.Context, name string, args ...string) (string, error)
	lookPath     func(name string) (string, error)
	homeDir      func() (string, error)
	now          func() time.Time
	windowTitles func(ctx context.Context) ([]string, error)
	activeWindow func(ctx context.Context) (string, error)
	cursor       func(ctx context.Context) (cursorPos, bool)
	screenLocked func(ctx context.Context) (locked bool, checked bool)
}

func newScreenshotTool() *screenshotTool {
	return newScreenshotToolWithBudget(NewCaptureBudget(), true)
}

// newScreenshotToolWithBudget builds a screenshot tool charging
// budget (nil becomes a fresh private budget). owner says
// whether this tool's BeginTurn resets the budget — true for the
// parent registry's tool, false for a child registry's (and for
// ground's embedded instance, whose BeginTurn is never called).
func newScreenshotToolWithBudget(budget *CaptureBudget, owner bool) *screenshotTool {
	if budget == nil {
		budget = NewCaptureBudget()
	}
	t := &screenshotTool{
		budget:      budget,
		budgetOwner: owner,
		lookPath:    exec.LookPath,
		homeDir:     os.UserHomeDir,
		now:         time.Now,
	}
	t.execCmd = defaultExecCmd
	t.windowTitles = t.defaultWindowTitles
	t.activeWindow = t.defaultActiveWindow
	t.cursor = t.defaultCursor
	t.screenLocked = t.defaultScreenLocked
	return t
}

// defaultExecCmd runs a command and returns its combined output.
func defaultExecCmd(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func (t *screenshotTool) Name() string { return "screenshot" }

func (t *screenshotTool) Description() string {
	return "Take a screenshot of the desktop and return it as an image the model can see, with a JSON summary (focused window, numeric cursor position, file). Read-only on the desktop: it takes no input and changes nothing on screen. DISCLOSURE: the captured image is sent to the model provider (data leaves this machine) — this call is gated and needs approval. The pointer is NOT visible in the screenshot image on this machine — use the numeric cursor position in the JSON, never look for the pointer in the picture. Refuses when a password manager, bank/payment app or one-time-code dialog is focused or visible, and never runs in unattended (cron) turns. Backends: spectacle (routine, no dialog); portal ScreenCast is not supported yet."
}

func (t *screenshotTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"display": map[string]any{"type": "string", "description": "Which display to capture: \"primary\" (default). \"all\" and a display name are not supported yet."},
				"region": map[string]any{
					"type":        "object",
					"description": "Crop rectangle in screen pixels (for a zoomed view of a small target). Must lie inside the captured screen.",
					"properties": map[string]any{
						"x":      map[string]any{"type": "integer"},
						"y":      map[string]any{"type": "integer"},
						"width":  map[string]any{"type": "integer"},
						"height": map[string]any{"type": "integer"},
					},
					"required": []string{"x", "y", "width", "height"},
				},
				"scale":     map[string]any{"type": "number", "minimum": 0.25, "maximum": 1.0, "description": "Scale factor applied to the capture (0.25–1.0, default 1.0)."},
				"format":    map[string]any{"type": "string", "enum": []string{"jpeg", "png"}, "description": "Image format: \"jpeg\" (default, smaller) or \"png\"."},
				"max_width": map[string]any{"type": "integer", "minimum": 640, "maximum": 1920, "description": "Longest side of the returned image in pixels (default 1280, range 640–1920). The capture is scaled down to fit."},
			},
		},
	}
}

// RiskClass declares the tool Network (F-M2R-6): it only reads the
// screen and writes Orion's own 0600 screenshot file — never user
// files, never the shell, never the network directly — but the image
// it returns is sent to the model provider, so the capture is a
// gated, disclosed action, never silently auto-approved.
func (t *screenshotTool) RiskClass() permissions.Class { return permissions.ClassNetwork }

// BeginTurn resets the per-turn capture counter (see
// TurnResetter) — but only when this tool owns the shared
// budget (F-A23W2-14): a child registry's tools share the
// budget and must never reset it mid-parent-turn. Resetting is
// idempotent, so the parent registry's ground, screenshot and
// input tools may all call it.
func (t *screenshotTool) BeginTurn() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.budgetOwner {
		t.budget.BeginTurn()
	}
}

// Execute is the text-only Tool interface method: the same capture,
// returning only the JSON summary text.
func (t *screenshotTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	res, err := t.ExecuteResult(ctx, args)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

// ExecuteResult captures the screen and returns the JSON summary
// plus the image.
func (t *screenshotTool) ExecuteResult(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	return t.executeResult(ctx, args, true)
}

// executeResult runs the capture. charge=false skips the shared
// budget charge: ground calls this for its embedded capture
// after already charging the shared budget for the grounding —
// one grounding is one capture, not two (F-A23W2-14). Everything
// else (unattended refusal, sensitive check, backend probe,
// min-interval) still runs: the embedded call is a real capture,
// just already paid for.
func (t *screenshotTool) executeResult(ctx context.Context, args json.RawMessage, charge bool) (ToolResult, error) {
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
		return denied("computer-use is off in unattended (cron) turns: screenshot refused")
	}

	// Vision gate (F-M2R-3): the capture's value is the image,
	// and a text-only model can never read it.
	if err := visionGateError(ctx, "screenshot"); err != nil {
		return denied("%v", err)
	}

	var a screenshotArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return invalid("arguments must be a JSON object: %v", err)
		}
	}
	display := a.Display
	if display == "" {
		display = "primary"
	}
	if display != "primary" {
		return invalid("display %q is not supported yet: only \"primary\" is implemented (multi-monitor mapping is still open)", display)
	}
	scale := 1.0
	if a.Scale != nil {
		scale = *a.Scale
	}
	if scale < 0.25 || scale > 1.0 {
		return invalid("scale must be between 0.25 and 1.0, got %v", scale)
	}
	format := a.Format
	if format == "" {
		format = "jpeg"
	}
	if format != "jpeg" && format != "png" {
		return invalid("format must be \"jpeg\" or \"png\", got %q", format)
	}
	maxWidth := screenshotDefaultWidth
	if a.MaxWidth != nil {
		maxWidth = *a.MaxWidth
	}
	if maxWidth < screenshotMinWidth || maxWidth > screenshotMaxWidth {
		return invalid("max_width must be between %d and %d, got %d", screenshotMinWidth, screenshotMaxWidth, maxWidth)
	}
	if a.Region != nil {
		r := a.Region
		if r.Width <= 0 || r.Height <= 0 || r.X < 0 || r.Y < 0 {
			return invalid("region must have non-negative x/y and positive width/height, got %+v", *r)
		}
	}

	// Backend probe at Execute start, never cached (plan §4.7-5):
	// spectacle must be present (LookPath) and runnable (a
	// --version smoke call). Its absence is a clean error with
	// install guidance, not a stub.
	spectaclePath, err := t.lookPath("spectacle")
	if err != nil || spectaclePath == "" {
		return t.screenCastNotSupported("spectacle is not installed (no screenshot backend on this machine)")
	}
	if _, err := t.execCmd(ctx, "spectacle", "--version"); err != nil {
		return t.screenCastNotSupported("spectacle is installed but does not run (--version failed: " + err.Error() + ")")
	}
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return failed("no graphical session available: neither DISPLAY nor WAYLAND_DISPLAY is set")
	}

	// Rate limits (plan §4.1): min 1s between captures, max 15
	// per turn, max 120 per hour on the SHARED capture budget
	// (F-A23W2-14) — unless this is ground's embedded capture,
	// which already paid for the grounding (charge=false).
	if err := t.checkRateLimit(charge); err != nil {
		return denied("%v", err)
	}

	// Sensitive-window check BEFORE capturing anything (plan
	// §5.3): a password manager / bank / OTP window focused or
	// visible refuses the capture outright. Fail closed
	// (F-M2R-5): when no window title can be read at all
	// ("unavailable" — xdotool missing, no XWayland), the capture
	// does NOT proceed on an unverified screen. The one exception
	// is a capture ground already verified on better sources
	// (AT-SPI tree + KWin list): ground marks that context, and
	// the check is skipped — verifying twice would refuse a
	// verified capture on a pure-Wayland session where xdotool
	// sees nothing.
	var sensitiveCheck string
	if SensitiveVerifiedFromContext(ctx) {
		sensitiveCheck = "ok"
	} else {
		var sensitiveErr string
		sensitiveCheck, sensitiveErr = t.checkSensitive(ctx)
		if sensitiveCheck == "unavailable" {
			return denied("cannot determine the visible windows (no window list available): I will not capture blind")
		}
		if sensitiveErr != "" {
			// The refusal names the category only — never the window
			// title, which could itself carry sensitive text.
			return denied("this screen shows a %s: I will not capture or describe it", sensitiveErr)
		}
	}

	// Screen locked / display off: error, no retry (plan §4.1).
	if locked, _ := t.screenLocked(ctx); locked {
		return failed("the screen is locked (or the display is off): unlock it and ask again — I will not retry on my own")
	}

	// Capture with spectacle (background, no notify, no dialog).
	dir, err := t.screenshotDir()
	if err != nil {
		return failed("cannot resolve the screenshot directory: %v", err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return failed("cannot create the screenshot directory: %v", err)
	}
	t.pruneOld(dir)
	stamp := t.now().UTC().Format("20060102-150405")
	rawPath := filepath.Join(dir, fmt.Sprintf("capture-%s-%d.png", stamp, rand.Intn(100000)))
	out, err := t.execCmd(ctx, "spectacle", "-b", "-n", "-o", rawPath)
	if err != nil {
		return failed("spectacle failed: %v (output: %s)", err, strings.TrimSpace(out))
	}
	defer os.Remove(rawPath)
	raw, err := os.ReadFile(rawPath)
	if err != nil || len(raw) == 0 {
		return failed("spectacle produced no image file at %s", rawPath)
	}

	// Decode, crop, scale, encode to the requested format, shrinking
	// until the image fits the per-image cap.
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return failed("cannot decode the captured image: %v", err)
	}
	bounds := img.Bounds()
	if a.Region != nil {
		r := a.Region
		crop := image.Rect(r.X, r.Y, r.X+r.Width, r.Y+r.Height)
		if !crop.In(image.Rect(bounds.Min.X, bounds.Min.Y, bounds.Max.X, bounds.Max.Y)) {
			return invalid("region %+v lies outside the %dx%d capture", *r, bounds.Dx(), bounds.Dy())
		}
		img = cropImage(img, crop)
		bounds = img.Bounds()
	}
	// Effective scale: the requested scale, then capped to max_width.
	scaleW := float64(bounds.Dx()) * scale
	if scaleW > float64(maxWidth) {
		scale = scale * float64(maxWidth) / scaleW
	}
	if scale < 1.0 {
		img = scaleImage(img, scale)
		bounds = img.Bounds()
	}
	encoded, mediaType, err := encodeImage(img, format)
	if err != nil {
		return failed("cannot encode the captured image: %v", err)
	}
	// Shrink loop: lower JPEG quality, then halve the dimensions,
	// until the image fits MaxImageBytes.
	for attempt := 0; len(encoded) > MaxImageBytes && attempt < screenshotMaxScaleTries; attempt++ {
		if format == "jpeg" && attempt < 2 {
			encoded, err = encodeJPEG(img, 70-15*attempt)
			if err != nil {
				return failed("cannot encode the captured image: %v", err)
			}
			continue
		}
		img = scaleImage(img, 0.5)
		encoded, mediaType, err = encodeImage(img, format)
		if err != nil {
			return failed("cannot encode the captured image: %v", err)
		}
	}
	if len(encoded) > MaxImageBytes {
		return failed("captured image is %d bytes even after shrinking, over the %d-byte cap", len(encoded), MaxImageBytes)
	}

	// Store the final file 0600; the raw spectacle PNG is removed
	// by the deferred cleanup above on a clean return, and by the
	// prune sweep (1 h raw retention) on the next capture if this
	// process is killed first (F-M2R-33).
	ext := ".jpg"
	if format == "png" {
		ext = ".png"
	}
	finalPath := filepath.Join(dir, fmt.Sprintf("screenshot-%s-%d%s", stamp, rand.Intn(100000), ext))
	if err := os.WriteFile(finalPath, encoded, 0600); err != nil {
		return failed("cannot write the screenshot file: %v", err)
	}

	// Focused window + numeric cursor position (best effort: the
	// capture stands even when xdotool cannot answer).
	summary := screenshotSummary{
		Backend:              "spectacle",
		File:                 finalPath,
		Width:                img.Bounds().Dx(),
		Height:               img.Bounds().Dy(),
		Format:               format,
		SizeBytes:            len(encoded),
		Display:              display,
		CursorVisibleInImage: false,
		SensitiveCheck:       sensitiveCheck,
	}
	if w, err := t.activeWindow(ctx); err == nil && w != "" {
		summary.FocusedWindow = w
	}
	if cur, ok := t.cursor(ctx); ok {
		summary.Cursor = &cur
	}
	text, err := json.Marshal(summary)
	if err != nil {
		return failed("cannot encode the screenshot summary: %v", err)
	}
	return ToolResult{
		Text:   string(text),
		Images: []ToolImage{{Data: encoded, MIMEType: mediaType}},
	}, nil
}

// screenCastNotSupported is the ScreenCast backend: clean
// invalid_args, never a faked capture. See the package comment for
// why (no stock PipeWire consumer; per-session dialog; the
// consumer-home decision is still open).
func (t *screenshotTool) screenCastNotSupported(reason string) (ToolResult, error) {
	return ToolResult{}, &ToolError{
		Tool: t.Name(), Kind: "invalid_args",
		Err: fmt.Errorf("screencast backend is not supported yet: %s. The PipeWire consumer was proven by the M1 probe (~/orion-computer-use/m1/m1-pw-capture.c on the laptop) but is not vendored into Orion yet — the consumer home (Go cgo vs a small C helper) is still undecided, and every ScreenCast session costs the user a portal dialog click because grants do not persist, so routine screenshots use spectacle instead", reason),
	}
}

// checkRateLimit enforces the min-interval (per tool instance)
// and the per-turn + per-hour limits (the SHARED capture
// budget, F-A23W2-14) and records the call. It must be called
// once per capture, after argument validation. charge=false
// skips the budget charge (ground's embedded capture — the
// grounding already paid) but still enforces the min-interval
// and records the call time.
func (t *screenshotTool) checkRateLimit(charge bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if !t.lastCall.IsZero() && now.Sub(t.lastCall) < screenshotMinInterval {
		return fmt.Errorf("screenshot rate limit: at most one capture per second (last capture %v ago)", now.Sub(t.lastCall).Round(time.Millisecond))
	}
	if charge {
		if err := t.budget.Charge(now, screenshotMaxPerTurn, screenshotMaxPerHour); err != nil {
			if errors.Is(err, errCaptureBudgetTurn) {
				return fmt.Errorf("screenshot rate limit: at most %d captures per turn", screenshotMaxPerTurn)
			}
			return fmt.Errorf("screenshot rate limit: at most %d captures per hour", screenshotMaxPerHour)
		}
	}
	t.lastCall = now
	return nil
}

// checkSensitive inspects the visible window titles before capture.
// It returns ("ok"|"partial"|"unavailable", "") on pass, or
// ("", category) when a sensitive window is present. "partial"
// means the full window list could not be enumerated and only the
// active window was checked; "unavailable" means no title could be
// read at all (xdotool missing — e.g. a session with no XWayland),
// in which case the CALLER must refuse the capture (F-M2R-5: an
// unverified screen is never captured) — the summary field alone
// is not a substitute for refusing. An enumeration that succeeds
// but finds no titled windows is "ok", not "unavailable" — empty
// titles are a result, not a failure.
func (t *screenshotTool) checkSensitive(ctx context.Context) (string, string) {
	titles, err := t.windowTitles(ctx)
	if err != nil {
		// No list: fall back to the active window alone.
		active, aerr := t.activeWindow(ctx)
		if aerr != nil || active == "" {
			return "unavailable", ""
		}
		if TitleLooksSensitive(active) {
			return "", SensitiveTitleCategory(active)
		}
		return "partial", ""
	}
	if hit := FindSensitiveTitle(titles); hit != "" {
		return "", SensitiveTitleCategory(hit)
	}
	return "ok", ""
}

// screenshotDir is ~/.orion/screenshots.
func (t *screenshotTool) screenshotDir() (string, error) {
	home, err := t.homeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("cannot determine the home directory: %v", err)
	}
	return filepath.Join(home, ".orion", "screenshots"), nil
}

// pruneOld deletes screenshots older than the retention window.
// Best effort: a failure to list or delete never fails a capture.
// Raw spectacle captures (capture-*.png) get the short raw
// retention (F-M2R-33): a raw file older than an hour is a
// kill/crash leftover — the deferred cleanup never ran — so it is
// swept here, on the next capture, instead of lingering for the
// full 24 h. Finished screenshot-* files keep the 24 h retention.
func (t *screenshotTool) pruneOld(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := t.now().Add(-screenshotRetention)
	rawCutoff := t.now().Add(-screenshotRawRetention)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if strings.HasPrefix(e.Name(), "capture-") {
			if info.ModTime().Before(rawCutoff) {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// --- xdotool seams (XWayland only) ---

// defaultWindowTitles lists the titles of visible windows via
// xdotool. It returns an error only when xdotool cannot enumerate
// them; a successful enumeration with no titled windows returns an
// empty (non-nil) slice.
func (t *screenshotTool) defaultWindowTitles(ctx context.Context) ([]string, error) {
	out, err := t.execCmd(ctx, "xdotool", "search", "--onlyvisible", "--name", ".*", "getwindowname", "%@")
	if err != nil {
		return nil, err
	}
	titles := []string{}
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			titles = append(titles, line)
		}
	}
	return titles, nil
}

// defaultActiveWindow returns the focused window's title, or ""
// when there is none / xdotool cannot answer.
func (t *screenshotTool) defaultActiveWindow(ctx context.Context) (string, error) {
	out, err := t.execCmd(ctx, "xdotool", "getactivewindow", "getwindowname", "%@")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// defaultCursor returns the numeric cursor position from xdotool
// getmouselocation ("x:960 y:540 screen:0 ..."). ok is false when
// xdotool is missing or its output cannot be parsed.
func (t *screenshotTool) defaultCursor(ctx context.Context) (cursorPos, bool) {
	out, err := t.execCmd(ctx, "xdotool", "getmouselocation")
	if err != nil {
		return cursorPos{}, false
	}
	var cur cursorPos
	for _, field := range strings.Fields(out) {
		kv := strings.SplitN(field, ":", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "x":
			cur.X, _ = strconv.Atoi(kv[1])
		case "y":
			cur.Y, _ = strconv.Atoi(kv[1])
		}
	}
	return cur, true
}

// defaultScreenLocked reports whether the screen is locked, via
// loginctl's LockedHint for the current session. checked is false
// when the session id is unknown or loginctl cannot answer — the
// capture then proceeds, and a locked screen surfaces as a
// spectacle failure instead. A locked screen is never silently
// captured as a black image and handed to the model as content.
func (t *screenshotTool) defaultScreenLocked(ctx context.Context) (bool, bool) {
	session := os.Getenv("XDG_SESSION_ID")
	if session == "" {
		return false, false
	}
	out, err := t.execCmd(ctx, "loginctl", "show-session", "--value", "-p", "LockedHint", session)
	if err != nil {
		return false, false
	}
	return strings.TrimSpace(out) == "yes", true
}

// --- image helpers (pure stdlib) ---

// cropImage returns the sub-image of img inside r.
func cropImage(img image.Image, r image.Rectangle) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(dst, dst.Bounds(), img, r.Min, draw.Src)
	return dst
}

// scaleImage scales img by factor with nearest-neighbour sampling.
// It is deliberately simple (stdlib only — no x/image dep): for
// screenshots, a fast downscale the model reads is worth more than
// a filtered one, and the factor is only ever <= 1 here.
func scaleImage(img image.Image, factor float64) image.Image {
	b := img.Bounds()
	w := int(float64(b.Dx()) * factor)
	h := int(float64(b.Dy()) * factor)
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		sy := b.Min.Y + int(float64(y)/factor)
		if sy >= b.Max.Y {
			sy = b.Max.Y - 1
		}
		for x := 0; x < w; x++ {
			sx := b.Min.X + int(float64(x)/factor)
			if sx >= b.Max.X {
				sx = b.Max.X - 1
			}
			dst.Set(x, y, img.At(sx, sy))
		}
	}
	return dst
}

// encodeImage encodes img in format ("jpeg" default quality 85, or
// "png") and returns the bytes and media type.
func encodeImage(img image.Image, format string) ([]byte, string, error) {
	if format == "png" {
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return nil, "", err
		}
		return buf.Bytes(), "image/png", nil
	}
	data, err := encodeJPEG(img, 85)
	if err != nil {
		return nil, "", err
	}
	return data, "image/jpeg", nil
}

// encodeJPEG encodes img as JPEG at quality.
func encodeJPEG(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
