// Package hooks implements Orion's hook bus (audit P0 #6): lifecycle
// hooks — PreToolUse, PostToolUse, Stop — that run as external
// executables and can observe, and (for PreToolUse) veto, tool calls.
//
// # Hook protocol contract (implemented here, rc9)
//
// A hook is an executable file under exactly one directory:
//
//	<DataDir>/hooks/<Event>/<name>
//
// where <Event> is PreToolUse, PostToolUse or Stop and <DataDir> is
// Orion's user data dir (e.g. ~/.orion). Hooks load ONLY from there.
// They are NEVER loaded from the repository working directory, from
// any project directory, or from any path the model or a tool result
// can influence: a cloned repo must never be able to smuggle a hook
// into the agent (Claude Code's "project files = trust" mistake,
// audit P1-6). There is no opt-out and no second search path.
//
// Invocation: the hook is executed with the event payload as JSON on
// stdin, a scrubbed environment (see below), and its working
// directory set to the directory holding the hook. Its stdout is
// parsed as JSON only when the hook's decision needs a reason; its
// stderr is kept for logs only and never reaches the model.
//
// Payloads (all JSON objects on stdin):
//
//	PreToolUse:  {"event":"PreToolUse","tool_name":"<tool>","arguments":{...}}
//	PostToolUse: {"event":"PostToolUse","tool_name":"<tool>","arguments":{...},"result":"<tool result text, truncated>"}
//	Stop:        {"event":"Stop","reason":"completed"|"max_steps"|"error: ..."}
//
// Exit-code protocol (the exit code is authoritative; the optional
// JSON on stdout supplies the human-readable reason):
//
//	0: continue. Stdout may carry {"decision":"continue"} or
//	   {"decision":"block","reason":"..."} — a block decision in the
//	   JSON is honoured even with exit 0, so a hook that decides in
//	   code does not have to arrange its exit code too. Non-JSON
//	   stdout on exit 0 is ignored (hooks may print diagnostics).
//	2: block. PreToolUse: the tool call DOES NOT execute — the agent
//	   feeds "Error: tool ... blocked by hook: <reason>" back to the
//	   model instead, exactly as if the permission gate had denied
//	   it, and the tool's side effects never happen. PostToolUse /
//	   Stop: there is nothing left to block, so exit 2 is treated
//	   as continue-with-warning (and logged); it never fails the
//	   turn. Stop hooks can NEVER prevent the turn from ending in
//	   rc9 — unlike Claude Code, Orion has no "stop prevented, keep
//	   going" loop re-entry, and silently re-entering the loop from
//	   a hook would be a correctness hazard. A Stop hook that exits
//	   2 is logged loudly so the operator notices the deviation.
//	other (1, 3, ...): continue-with-warning. The warning is logged
//	   and returned to the caller; the turn proceeds. A hook that
//	   crashes must not take the agent down with it.
//
// Fail-safe rule: a hook that cannot be started, times out, or is
// killed (any failure that is NOT a clean process exit) fails CLOSED
// for PreToolUse — the tool call is blocked, because the hook that
// might have vetoed it never got to speak. For PostToolUse and Stop
// the same failure is only a warning: those events cannot authorise
// anything, so there is nothing to deny.
//
// Environment: a hook child NEVER inherits the agent's secrets. Its
// environment is tools.ScrubEnv (the same scrub shell_exec uses: any
// variable whose name mentions a credential — API_KEY, TOKEN, SECRET,
// PASSWORD, KEY, CREDENTIAL, AUTH, BEARER, ORION_* — is dropped, and
// credential-bearing proxy URLs are dropped), plus exactly two
// Orion-set variables, ORION_HOOK_EVENT and ORION_TOOL_NAME, so a
// plain shell hook can branch without parsing JSON. A hook must
// never be able to exfiltrate a secret it was never given, and the
// hook's stdout/stderr are redacted before they reach the model or
// the log, so a hook that prints a guessed secret cannot leak it
// through the block reason either.
//
// Ordering and multiplicity: hooks for one event run in sorted
// filename order, sequentially. The first PreToolUse hook that
// blocks short-circuits the rest (they never run for that call).
// PostToolUse and Stop hooks all run even if an earlier one fails.
//
// Loading rules (all fail-closed, per hook): a hook must be a
// regular file, executable, at most maxHookFileBytes, and must not
// be (or resolve through) a symlink that escapes the hooks
// directory — a symlink pointing outside the data dir is skipped
// and recorded as a load error. A bad hook never stops the bus (or
// the daemon) from starting: it is skipped, the load error is
// retained for the caller to log, and the remaining hooks load.
//
// A nil *Bus behaves as "no hooks": every method continues. A Bus
// with no hooks loaded is the same. Hooks are therefore safe to wire
// unconditionally; an operator with no hooks directory pays nothing.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/irfan/orion/internal/security"
	"github.com/irfan/orion/internal/tools"
)

// Event is one hook event.
type Event string

const (
	// EventPreToolUse fires before a tool executes; it can block the call.
	EventPreToolUse Event = "PreToolUse"
	// EventPostToolUse fires after a tool executes (result or error text); it observes only.
	EventPostToolUse Event = "PostToolUse"
	// EventStop fires once when a turn ends; it observes only and can never prevent the turn ending.
	EventStop Event = "Stop"
)

// allEvents is the load order (and the only accepted directory names).
var allEvents = []Event{EventPreToolUse, EventPostToolUse, EventStop}

// DefaultTimeout bounds one hook invocation. It is deliberately
// short: a hook runs synchronously inside a tool call (PreToolUse)
// or at turn end (Stop), so a hung hook must not stall the agent.
// A hook that exceeds it fails closed for PreToolUse (see the
// package doc) and warns for the other events.
const DefaultTimeout = 10 * time.Second

// maxHookFileBytes caps a hook file: a hook is a small script, and
// an unbounded file is either a mistake or an attempt to smuggle
// data past review. Larger files are skipped at load with an error.
const maxHookFileBytes = 1024 * 1024 // 1 MiB

// maxHooksPerEvent caps how many hooks one event may hold. Beyond
// this the extras are skipped at load with an error: an unbounded
// hook list is a denial-of-service on every tool call.
const maxHooksPerEvent = 32

// maxHookOutputBytes caps hook stdout/stderr captured per invocation
// (64 KiB / 16 KiB): a flooding hook must not be able to exhaust
// memory through its output, and only the first bytes can carry a
// JSON decision or a useful reason anyway.
const (
	maxHookStdoutBytes = 64 * 1024
	maxHookStderrBytes = 16 * 1024
)

// maxHookResultBytes caps the tool-result text handed to a
// PostToolUse hook: the registry already caps results at 256 KiB,
// and a hook does not need more than 64 KiB of it to observe the
// outcome.
const maxHookResultBytes = 64 * 1024

// maxReasonRunes caps a block/warning reason surfaced to the model
// or the log: a hook's reason is operator text, but it is still
// untrusted output and must stay short.
const maxReasonRunes = 500

// Hook is one loaded hook executable.
type Hook struct {
	// Event is the event this hook handles.
	Event Event
	// Name is the hook's file name (used in logs and reasons).
	Name string
	// Path is the hook's absolute file path.
	Path string
	// Dir is the directory holding the hook (its working directory).
	Dir string
}

// Bus is the hook bus: the loaded hooks for every event, plus the
// per-invocation timeout. Create it with NewBus. It is safe for
// concurrent use: the agent loop calls it while turns run, and the
// hooks map is read-only after construction.
type Bus struct {
	mu       sync.RWMutex
	hooks    map[Event][]*Hook
	timeout  time.Duration
	logger   *log.Logger
	loadErrs []error
}

// NewBus loads hooks from <dataDir>/hooks/<Event>/. A missing hooks
// directory (or an empty dataDir) is not an error: it means "no
// hooks", and the returned Bus continues every call. Individual bad
// hooks are skipped and recorded (see LoadErrors); they never fail
// the load. dataDir is the ONLY search root — no cwd, no repo, no
// environment override is ever consulted.
func NewBus(dataDir string) *Bus {
	b := &Bus{
		hooks:   map[Event][]*Hook{},
		timeout: DefaultTimeout,
		logger:  log.New(os.Stderr, "orion/hooks: ", log.LstdFlags),
	}
	if strings.TrimSpace(dataDir) == "" {
		return b
	}
	root := filepath.Join(dataDir, "hooks")
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		if os.IsNotExist(err) {
			return b // no hooks directory: no hooks, no error
		}
		b.loadErrs = append(b.loadErrs, fmt.Errorf("hooks: cannot resolve hooks dir %q: %w", root, err))
		return b
	}
	for _, ev := range allEvents {
		dir := filepath.Join(resolvedRoot, string(ev))
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue // no hooks for this event
			}
			b.loadErrs = append(b.loadErrs, fmt.Errorf("hooks: cannot read %s hooks dir %q: %w", ev, dir, err))
			continue
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		for _, name := range names {
			if len(b.hooks[ev]) >= maxHooksPerEvent {
				b.loadErrs = append(b.loadErrs, fmt.Errorf("hooks: too many %s hooks (max %d); skipping %q", ev, maxHooksPerEvent, name))
				continue
			}
			h, err := loadHook(ev, dir, resolvedRoot, name)
			if err != nil {
				b.loadErrs = append(b.loadErrs, err)
				continue
			}
			b.hooks[ev] = append(b.hooks[ev], h)
		}
	}
	return b
}

// loadHook validates one candidate hook file. The checks are the
// trust gate: regular file, executable, small, and — after resolving
// symlinks — still inside the hooks directory.
func loadHook(ev Event, dir, resolvedRoot, name string) (*Hook, error) {
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("hooks: cannot stat %s hook %q: %w", ev, path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("hooks: %s hook %q is a directory, skipping", ev, path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("hooks: %s hook %q is not a regular file, skipping", ev, path)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return nil, fmt.Errorf("hooks: %s hook %q is not executable, skipping", ev, path)
	}
	if info.Size() > maxHookFileBytes {
		return nil, fmt.Errorf("hooks: %s hook %q is %d bytes (max %d), skipping", ev, path, info.Size(), maxHookFileBytes)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("hooks: cannot resolve %s hook %q: %w", ev, path, err)
	}
	if !pathWithin(resolved, resolvedRoot) {
		return nil, fmt.Errorf("hooks: %s hook %q resolves outside the hooks directory (%q), skipping", ev, path, resolved)
	}
	return &Hook{Event: ev, Name: name, Path: resolved, Dir: filepath.Dir(resolved)}, nil
}

// pathWithin reports whether p is inside (or equal to) dir, after
// both are clean absolute paths. A lexical prefix check would accept
// /hooks-evil as inside /hooks; the separator boundary stops that.
func pathWithin(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && rel != "..")
}

// LoadErrors returns the errors recorded while loading hooks (bad
// files skipped, unreadable directories). Nil when the load was
// clean. Callers log these at startup; a load error never blocks
// anything by itself.
func (b *Bus) LoadErrors() []error {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]error(nil), b.loadErrs...)
}

// Count returns how many hooks are loaded for event. Zero for a nil
// bus or an event with no hooks.
func (b *Bus) Count(ev Event) int {
	if b == nil {
		return 0
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.hooks[ev])
}

// HasHooks reports whether any hook is loaded for any event.
func (b *Bus) HasHooks() bool {
	if b == nil {
		return false
	}
	for _, ev := range allEvents {
		if b.Count(ev) > 0 {
			return true
		}
	}
	return false
}

// Hooks returns the loaded hooks for event, in run order (sorted by
// file name). The returned slice is a copy.
func (b *Bus) Hooks(ev Event) []*Hook {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]*Hook(nil), b.hooks[ev]...)
}

// SetTimeout overrides the per-invocation timeout. A non-positive
// value restores DefaultTimeout.
func (b *Bus) SetTimeout(d time.Duration) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if d <= 0 {
		d = DefaultTimeout
	}
	b.timeout = d
}

// Timeout returns the per-invocation timeout.
func (b *Bus) Timeout() time.Duration {
	if b == nil {
		return DefaultTimeout
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.timeout <= 0 {
		return DefaultTimeout
	}
	return b.timeout
}

// SetLogger replaces the bus logger. A nil logger is ignored.
func (b *Bus) SetLogger(l *log.Logger) {
	if b == nil || l == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.logger = l
}

func (b *Bus) logf(format string, args ...any) {
	if b == nil {
		return
	}
	b.mu.RLock()
	l := b.logger
	b.mu.RUnlock()
	if l != nil {
		l.Printf(format, args...)
	}
}

// PreOutcome is the result of a PreToolUse evaluation.
type PreOutcome struct {
	// Blocked reports whether the tool call must NOT execute.
	Blocked bool
	// Reason explains a block (or, when set with Blocked false,
	// is empty). It is already secret-redacted and length-capped,
	// safe to feed back to the model.
	Reason string
	// Warning carries non-blocking hook trouble (a hook exited
	// non-zero, a Stop hook tried to block): logged by the caller,
	// never fed to the model as a block.
	Warning string
}

// PreToolUse runs the PreToolUse hooks for a tool call, in order.
// The first hook that blocks short-circuits the rest. A nil bus or
// a bus with no PreToolUse hooks continues with a zero outcome. See
// the package doc for the exit-code contract and the fail-safe rule:
// a hook that cannot run at all BLOCKS the call.
func (b *Bus) PreToolUse(ctx context.Context, tool string, args json.RawMessage) PreOutcome {
	if b == nil || b.Count(EventPreToolUse) == 0 {
		return PreOutcome{}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	payload, err := json.Marshal(preToolUsePayload{Event: string(EventPreToolUse), ToolName: tool, Arguments: args})
	if err != nil {
		// Arguments came from the agent as validated JSON; a
		// marshal failure here is near-impossible. Fail closed
		// rather than running a call the hooks could not see.
		return PreOutcome{Blocked: true, Reason: "hook payload could not be built; refusing to run the tool unseen"}
	}
	var warnings []string
	for _, h := range b.Hooks(EventPreToolUse) {
		res := b.runHook(ctx, h, payload, tool)
		switch {
		case res.failed:
			// Fail closed: the hook never got to speak, so the
			// call it might have vetoed must not run.
			return PreOutcome{
				Blocked: true,
				Reason:  fmt.Sprintf("hook %q failed (%s); failing closed", h.Name, res.reason()),
				Warning: strings.Join(warnings, "; "),
			}
		case res.code == 2 || res.decision == "block":
			reason := res.blockReason()
			if reason == "" {
				reason = fmt.Sprintf("hook %q blocked the call", h.Name)
			}
			return PreOutcome{Blocked: true, Reason: reason, Warning: strings.Join(warnings, "; ")}
		case res.code == 0:
			// Continue. A decision other than "continue" (or
			// empty) on exit 0 is a hook bug, not a block: warn
			// and carry on.
			if res.decision != "" && res.decision != "continue" {
				warnings = append(warnings, fmt.Sprintf("hook %q exited 0 with unknown decision %q", h.Name, res.decision))
			}
		default:
			warnings = append(warnings, fmt.Sprintf("hook %q exited with code %d (%s); continuing with warning", h.Name, res.code, res.reason()))
		}
	}
	return PreOutcome{Warning: strings.Join(warnings, "; ")}
}

// PostToolUse runs the PostToolUse hooks for a completed tool call
// (result is the text the model will see, error text included). It
// never blocks and never returns an error: the returned string is a
// warning for the caller to log (empty when every hook ran clean).
// A nil bus or a bus with no PostToolUse hooks returns "".
func (b *Bus) PostToolUse(ctx context.Context, tool string, args json.RawMessage, result string) string {
	if b == nil || b.Count(EventPostToolUse) == 0 {
		return ""
	}
	if ctx == nil {
		ctx = context.Background()
	}
	payload, err := json.Marshal(postToolUsePayload{
		Event: string(EventPostToolUse), ToolName: tool, Arguments: args,
		Result: tools.TruncateToolResult(result, maxHookResultBytes),
	})
	if err != nil {
		return "hook payload could not be built; post-tool hooks skipped"
	}
	var warnings []string
	for _, h := range b.Hooks(EventPostToolUse) {
		res := b.runHook(ctx, h, payload, tool)
		if w := postWarning(h, res); w != "" {
			warnings = append(warnings, w)
		}
	}
	return strings.Join(warnings, "; ")
}

// Stop runs the Stop hooks for a finished turn (reason is
// "completed", "max_steps", or "error: ..."). It never blocks and
// never returns an error: the returned string is a warning for the
// caller to log. A Stop hook that exits 2 — asking to prevent the
// stop — is warned about loudly and ignored (see package doc).
func (b *Bus) Stop(ctx context.Context, reason string) string {
	if b == nil || b.Count(EventStop) == 0 {
		return ""
	}
	if ctx == nil {
		ctx = context.Background()
	}
	payload, err := json.Marshal(stopPayload{Event: string(EventStop), Reason: reason})
	if err != nil {
		return "hook payload could not be built; stop hooks skipped"
	}
	var warnings []string
	for _, h := range b.Hooks(EventStop) {
		res := b.runHook(ctx, h, payload, "")
		if res.code == 2 || res.decision == "block" {
			warnings = append(warnings, fmt.Sprintf("hook %q asked to prevent the turn ending (exit 2); Orion does not support stop prevention — the turn ends anyway", h.Name))
			continue
		}
		if w := postWarning(h, res); w != "" {
			warnings = append(warnings, w)
		}
	}
	return strings.Join(warnings, "; ")
}

// postWarning renders a non-blocking hook result as a warning
// (PostToolUse and Stop share it). Exit 2 is handled by the caller
// for Stop; for PostToolUse it lands here as a warning too, since a
// finished tool call cannot be un-run.
func postWarning(h *Hook, res hookResult) string {
	switch {
	case res.failed:
		return fmt.Sprintf("hook %q failed (%s)", h.Name, res.reason())
	case res.code == 2:
		return fmt.Sprintf("hook %q exited 2 (block) after the tool already ran; nothing to block", h.Name)
	case res.code == 0:
		if res.decision != "" && res.decision != "continue" && res.decision != "block" {
			return fmt.Sprintf("hook %q exited 0 with unknown decision %q", h.Name, res.decision)
		}
		return ""
	default:
		return fmt.Sprintf("hook %q exited with code %d (%s); continuing with warning", h.Name, res.code, res.reason())
	}
}

// preToolUsePayload is the stdin JSON for a PreToolUse hook.
type preToolUsePayload struct {
	Event     string          `json:"event"`
	ToolName  string          `json:"tool_name"`
	Arguments json.RawMessage `json:"arguments"`
}

// postToolUsePayload is the stdin JSON for a PostToolUse hook.
type postToolUsePayload struct {
	Event     string          `json:"event"`
	ToolName  string          `json:"tool_name"`
	Arguments json.RawMessage `json:"arguments"`
	Result    string          `json:"result"`
}

// stopPayload is the stdin JSON for a Stop hook.
type stopPayload struct {
	Event  string `json:"event"`
	Reason string `json:"reason"`
}

// hookOutput is the optional JSON decision a hook prints on stdout.
type hookOutput struct {
	Decision string `json:"decision"` // "continue" | "block"
	Reason   string `json:"reason"`
}

// hookResult is one hook invocation's outcome, already parsed.
type hookResult struct {
	// code is the process exit code (-1 when the process never
	// produced one: start failure, timeout, kill).
	code int
	// failed reports a non-exit failure (could not start, timed
	// out, killed): the fail-closed case for PreToolUse.
	failed bool
	// decision is the parsed stdout JSON decision ("" when the
	// hook printed no JSON decision).
	decision string
	// stdoutReason / stderr are the hook's outputs, redacted and
	// capped, for reasons and warnings.
	stdoutReason string
	stderr       string
}

// reason returns the best one-line explanation of a failed or
// non-zero hook run: the JSON reason first, then stderr, then a
// bare exit-code note.
func (r hookResult) reason() string {
	if r.stdoutReason != "" {
		return r.stdoutReason
	}
	if r.stderr != "" {
		return r.stderr
	}
	if r.failed {
		return "hook did not exit cleanly"
	}
	return fmt.Sprintf("exit code %d", r.code)
}

// blockReason returns the reason to feed back to the model for a
// block: the hook's JSON reason, else its stderr, else "" (the
// caller supplies a default naming the hook).
func (r hookResult) blockReason() string {
	if r.stdoutReason != "" {
		return r.stdoutReason
	}
	return r.stderr
}

// runHook executes one hook with payload on stdin and returns its
// parsed result. The child runs under ctx with the bus timeout, in
// the hook's own directory, with a scrubbed environment plus
// ORION_HOOK_EVENT / ORION_TOOL_NAME. Its outputs are captured
// capped; a timed-out or killed child is reported failed (the
// caller decides what that means for its event).
func (b *Bus) runHook(ctx context.Context, h *Hook, payload []byte, tool string) hookResult {
	timeout := b.Timeout()
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(hctx, h.Path)
	cmd.Dir = h.Dir
	cmd.Env = hookEnv(h.Event, tool)
	cmd.Stdin = bytes.NewReader(payload)
	// Run the hook in its own process group: a hook that spawns
	// children (e.g. `sleep 30`) would otherwise outlive the kill
	// of the hook itself and hold the stdout/stderr pipes open,
	// leaving cmd.Wait blocked until the grandchild exits. On
	// context expiry CommandContext calls Cancel instead of
	// killing just the direct child, so the whole group dies and
	// the pipes close. WaitDelay bounds Wait even if a detached
	// grandchild somehow keeps a pipe open.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr cappedBuffer
	stdout.limit = maxHookStdoutBytes
	stderr.limit = maxHookStderrBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := hookResult{
		stdoutReason: cleanHookText(stdout.String()),
		stderr:       cleanHookText(stderr.String()),
	}
	if err == nil {
		res.code = 0
	} else {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.code = exitErr.ExitCode()
			// A negative exit code means the process died by
			// signal (or by our timeout kill): it never
			// produced a decision, so it is a failure, not an
			// exit-code verdict. The same holds when our own
			// timeout fired, even if an exit code exists.
			if res.code < 0 || hctx.Err() == context.DeadlineExceeded {
				res.failed = true
			}
		} else {
			// Start failure, timeout kill, signal death: no
			// exit code exists, and the hook never spoke.
			res.code = -1
			res.failed = true
			if res.stderr == "" {
				res.stderr = redactAndCap(err.Error())
			}
		}
	}
	// Parse the stdout JSON decision. Only the FIRST line that
	// parses as a hookOutput counts: a hook may print diagnostics
	// before its decision, and a trailing garbage line must not
	// override it. Non-JSON stdout is not an error.
	for _, line := range strings.Split(stdout.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var out hookOutput
		if err := json.Unmarshal([]byte(line), &out); err != nil {
			continue
		}
		res.decision = strings.TrimSpace(out.Decision)
		if out.Reason != "" {
			res.stdoutReason = redactAndCap(out.Reason)
		}
		break
	}
	return res
}

// hookEnv builds a hook child's environment: the scrubbed process
// environment (no secrets, same scrub shell_exec uses) plus the two
// Orion-set variables. The scrubbed base is built fresh on every
// invocation so a secret set after startup is still stripped.
func hookEnv(ev Event, tool string) []string {
	env := tools.ScrubEnv()
	env = append(env, "ORION_HOOK_EVENT="+string(ev))
	if tool != "" {
		env = append(env, "ORION_TOOL_NAME="+tool)
	}
	return env
}

// cappedBuffer collects output up to a limit and discards the rest,
// remembering nothing but the kept prefix: a flooding hook's output
// stays bounded in memory.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (w *cappedBuffer) Write(p []byte) (int, error) {
	if w.limit <= 0 {
		w.limit = maxHookStdoutBytes
	}
	if w.buf.Len() >= w.limit {
		return len(p), nil
	}
	room := w.limit - w.buf.Len()
	if len(p) > room {
		p = p[:room]
	}
	return w.buf.Write(p)
}

func (w *cappedBuffer) String() string { return w.buf.String() }

// cleanHookText redacts secrets and caps hook output text to
// maxReasonRunes runes, backing off to a rune boundary so the
// result stays valid UTF-8. Hook output is untrusted: it is fed to
// the model (block reasons) and the log, so it is scrubbed here,
// not at the call site.
func cleanHookText(s string) string {
	return redactAndCap(s)
}

// redactAndCap redacts detected secrets, then caps to
// maxReasonRunes runes (appending "…" when it cuts).
func redactAndCap(s string) string {
	s = security.RedactSecrets(s)
	r := []rune(s)
	if len(r) > maxReasonRunes {
		return string(r[:maxReasonRunes]) + "…"
	}
	return s
}
