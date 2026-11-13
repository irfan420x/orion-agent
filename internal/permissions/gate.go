// Package permissions implements Orion's permission/confirmation gate
// (external audit P1-O2).
//
// Before the gate, the only thing standing between the model and a
// destructive tool call was the shell denylist (an absolute block on
// known-bad commands) plus a line in the system prompt asking the
// model to confirm destructive actions — prompt text the model could
// ignore, and no check at all for file writes or network tools. The
// gate is a real enforcement point in the agent loop, before
// Registry.Execute:
//
//   - Every tool is classified (see Classify): safe tools run freely,
//     destructive tools (shell_exec, file_write, file_edit,
//     subagent_spawn — a child agent can run shell tools, so spawning
//     one must be gated or the gate is bypassable) and network tools
//     (web_fetch, web_search) need approval.
//   - A persisted rule store (permissions.json, 0600) holds per-tool
//     allow/deny rules. A deny rule is never prompted; an allow rule
//     never prompts either. Rules are the user's standing decisions.
//   - Anything else gated prompts the user through a Prompter — in
//     the daemon, a Telegram inline-keyboard message with Approve /
//     Deny / Always allow / Never allow buttons. The gate blocks the
//     agent loop until the user answers, the approval times out, or
//     the turn's context is cancelled. A denial is fed back to the
//     model as a "permission denied" tool result; it never crashes
//     the loop, and the model is told (in the system prompt) not to
//     retry a denied call.
//
// The shell denylist stays ABSOLUTE: the gate checks it itself before
// prompting, so a denylisted command is denied without ever asking
// the user — an approval can never authorise a denylisted command,
// and the tool layer still enforces the denylist underneath as a
// second line of defence.
//
// Scope: the gate applies to Telegram-driven turns (remote, higher
// risk), where the daemon installs it on the app. Local CLI runs
// (orion run / orion chat) never install a gate: the operator is
// present at the terminal and the denylist is the only check there.
// A nil *Gate means "no gate" — core.Agent runs ungated, exactly as
// before — so the CLI path needs no code at all.
package permissions

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/irfan/orion/internal/security"
)

// Class is the risk class of a tool.
type Class int

const (
	// ClassSafe tools run without approval (calculator, file_read,
	// subagent list/result/stop, ...).
	ClassSafe Class = iota
	// ClassDestructive tools mutate the machine or spawn an agent
	// that can: shell_exec, file_write, file_edit, subagent_spawn.
	ClassDestructive
	// ClassNetwork tools send data off the machine or fetch remote
	// content the model then acts on: web_fetch, web_search.
	ClassNetwork
)

// String renders the class for prompts and logs.
func (c Class) String() string {
	switch c {
	case ClassDestructive:
		return "destructive"
	case ClassNetwork:
		return "network"
	default:
		return "safe"
	}
}

// destructiveTools are the tools that mutate the machine (or, for
// subagent_spawn, delegate to a child agent that can mutate it).
var destructiveTools = map[string]bool{
	"shell_exec":     true,
	"file_write":     true,
	"file_edit":      true,
	"subagent_spawn": true,
}

// networkTools are the tools that move data off the machine or fetch
// remote content.
var networkTools = map[string]bool{
	"web_fetch":  true,
	"web_search": true,
}

// Classify returns the risk class of a tool by name. Unknown tools
// are ClassSafe: the registry rejects them as unknown_tool before
// they can run, so the gate has nothing to protect there, and
// classifying an unknown tool as gated would only produce approval
// prompts for calls that can never execute. MCP tools (mcp__<server>__<tool>)
// are the exception: they are ClassNetwork — a server is an external
// process with unknown capabilities, so every MCP tool needs
// approval when a gate is installed (rc9, audit P0 #7).
func Classify(tool string) Class {
	tool = strings.TrimSpace(tool)
	if destructiveTools[tool] {
		return ClassDestructive
	}
	if networkTools[tool] {
		return ClassNetwork
	}
	if strings.HasPrefix(tool, "mcp__") {
		return ClassNetwork
	}
	return ClassSafe
}

// IsGated reports whether the tool ever needs approval (i.e. it is
// not ClassSafe).
func IsGated(tool string) bool { return Classify(tool) != ClassSafe }

// Action is a persisted per-tool rule.
type Action string

const (
	// ActionAllow lets the tool run without prompting.
	ActionAllow Action = "allow"
	// ActionDeny refuses the tool without prompting.
	ActionDeny Action = "deny"
)

// Valid reports whether the action is a known rule action.
func (a Action) Valid() bool { return a == ActionAllow || a == ActionDeny }

// Rule is one persisted per-tool decision.
type Rule struct {
	Tool   string `json:"tool"`
	Action Action `json:"action"`
}

// approvalContext carries the chat and sender a gated tool call
// belongs to, through the agent run's context. The agent loop's
// tool context derives from the run context, so the gate sees it in
// Check without any change to the tool or model interfaces.
type approvalContext struct {
	chatID   int64
	senderID int64
}

type approvalContextKey struct{}

// WithApprovalContext returns ctx carrying the chat and sender the
// current turn belongs to. The daemon's turn handler sets it (from
// the Telegram update) before the agent runs; the gate reads it in
// Check to address the approval prompt at the right chat and to
// verify the approver is the requester (or the owner).
func WithApprovalContext(ctx context.Context, chatID, senderID int64) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, approvalContextKey{}, approvalContext{chatID: chatID, senderID: senderID})
}

// ApprovalContextFrom reads the chat/sender carried by ctx. ok is
// false when ctx carries none (e.g. a local CLI run, which never
// installs a gate anyway).
func ApprovalContextFrom(ctx context.Context) (chatID, senderID int64, ok bool) {
	if ctx == nil {
		return 0, 0, false
	}
	v, ok := ctx.Value(approvalContextKey{}).(approvalContext)
	if !ok {
		return 0, 0, false
	}
	return v.chatID, v.senderID, true
}

// ApprovalRequest is one gated tool call waiting on the user.
type ApprovalRequest struct {
	// ID is the opaque pending-request ID the callback data carries
	// (16 hex chars of randomness, never sequential).
	ID string
	// ChatID is the chat the prompt goes to; SenderID is the user
	// whose turn triggered the call (only they — or the owner —
	// may answer it).
	ChatID   int64
	SenderID int64
	// Tool and Arguments are the call as the model made it.
	Tool      string
	Arguments string
}

// Summary renders the call for the approval prompt: the tool class
// plus the human-meaningful argument (command, path, URL, query),
// truncated and secret-redacted, never the raw JSON.
func (r ApprovalRequest) Summary() string {
	return RequestSummary(r.Tool, r.Arguments)
}

// maxSummaryRunes caps the argument shown in an approval prompt.
const maxSummaryRunes = 200

// RequestSummary renders a tool call for an approval prompt: the
// tool name and class plus the human-meaningful argument, truncated
// and secret-redacted. It never returns raw JSON: the prompt is a
// user-facing message, and the raw arguments can carry a secret the
// model pasted into a command.
func RequestSummary(tool, argsJSON string) string {
	class := Classify(tool)
	detail := ""
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(argsJSON), &raw); err == nil {
		detail = summaryArg(tool, raw)
	}
	if detail == "" {
		detail = strings.TrimSpace(argsJSON)
	}
	detail = security.RedactSecrets(detail)
	if r := []rune(detail); len(r) > maxSummaryRunes {
		detail = string(r[:maxSummaryRunes]) + "…"
	}
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return fmt.Sprintf("%s (%s)", tool, class)
	}
	return fmt.Sprintf("%s (%s): %s", tool, class, detail)
}

// summaryArg picks the human-meaningful argument for a tool.
func summaryArg(tool string, raw map[string]json.RawMessage) string {
	key := ""
	switch tool {
	case "shell_exec", "subagent_spawn":
		// shell_exec takes "command"; subagent_spawn takes "brief".
		if tool == "shell_exec" {
			key = "command"
		} else {
			key = "brief"
		}
	case "file_write", "file_edit":
		key = "path"
	case "web_fetch":
		key = "url"
	case "web_search":
		key = "query"
	default:
		return ""
	}
	var s string
	if v, ok := raw[key]; ok {
		if err := json.Unmarshal(v, &s); err == nil {
			return s
		}
		return string(v)
	}
	return ""
}

// pendingRequest is a registered approval wait: the request plus the
// channel Resolve delivers on.
type pendingRequest struct {
	req ApprovalRequest
	ch  chan pendingResult
}

// pendingResult is what Resolve delivers: whether the user approved,
// and an optional rule to persist ("always allow" / "never allow").
type pendingResult struct {
	approved bool
	persist  *Action
}

// DefaultTimeout bounds how long the gate waits for an approval
// before denying. It is deliberately shorter than the default
// per-step tool timeout (120s), so a silent user produces a clean
// "approval timed out" denial instead of a context-cancellation
// error from the tool timeout.
const DefaultTimeout = 90 * time.Second

// Prompter asks the user to approve a request: it sends the prompt
// (in the daemon, a Telegram message with inline Approve/Deny
// buttons) and returns promptly. The gate waits for Resolve —
// invoked by the callback handler when the user taps a button —
// it does not wait inside the prompter. A prompter error denies the
// call (the user was never asked, so nothing may run).
type Prompter func(ctx context.Context, req ApprovalRequest) error

// Gate is the permission/confirmation gate. A nil *Gate means "no
// gate installed" (local CLI). A Gate with a nil Prompter denies
// every gated call: approval was requested but nobody can ask the
// user, so failing closed is the only safe answer.
//
// The gate is safe for concurrent use: several chats' turns may wait
// on approvals at once, and Resolve may arrive from the polling
// goroutine while Check waits.
type Gate struct {
	mu       sync.Mutex
	rules    *RuleStore
	prompter Prompter
	timeout  time.Duration
	pending  map[string]*pendingRequest
	// onExpire, when set via SetOnExpire, is called (once, outside
	// the gate mutex) when a pending request dies unanswered — the
	// approval window elapsed, or the turn was cancelled. The
	// daemon uses it to expire the approval card in place, so a
	// dead card never keeps live-looking buttons (the rc8 incident:
	// a tap on an already-timed-out card looks exactly like a
	// broken button). It is NOT called when Resolve answers the
	// request, nor when the prompter fails (no card was sent).
	// It must return promptly — spawn a goroutine for slow work
	// (a network call) — because Check calls it before returning
	// the denial.
	onExpire func(ApprovalRequest)
}

// NewGate builds a gate over rules (may be nil: no standing rules)
// with prompter (may be nil: every gated call is denied). Use
// SetTimeout to override DefaultTimeout.
func NewGate(rules *RuleStore, prompter Prompter) *Gate {
	return &Gate{
		rules:    rules,
		prompter: prompter,
		timeout:  DefaultTimeout,
		pending:  map[string]*pendingRequest{},
	}
}

// SetTimeout overrides how long Check waits for an approval. A
// non-positive value restores DefaultTimeout.
func (g *Gate) SetTimeout(d time.Duration) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if d <= 0 {
		d = DefaultTimeout
	}
	g.timeout = d
}

// SetOnExpire installs the hook called when a pending request dies
// unanswered (timeout or turn cancellation). A nil hook removes it.
// It is safe for concurrent use with a waiting Check: the hook is
// read once, outside the gate mutex, when the request expires.
func (g *Gate) SetOnExpire(fn func(ApprovalRequest)) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.onExpire = fn
}

// onExpireHook returns the installed expiry hook, or nil.
func (g *Gate) onExpireHook() func(ApprovalRequest) {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.onExpire
}

// Timeout returns the approval wait bound.
func (g *Gate) Timeout() time.Duration {
	if g == nil {
		return DefaultTimeout
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.timeout <= 0 {
		return DefaultTimeout
	}
	return g.timeout
}

// Check decides whether the tool call may run. It returns (true, "")
// when allowed and (false, reason) when denied; reason is a
// human-readable explanation the agent feeds back to the model (it
// never contains secrets: arguments are summarised through
// RequestSummary, never quoted raw). Denial paths, in order:
//
//  1. the shell denylist — checked BEFORE any rule or prompt, so a
//     denylisted command is denied without ever asking the user and
//     no "always allow" rule can rescue it. An approval can never
//     authorise a denylisted command;
//  2. a persisted rule (deny rules are never prompted, allow rules
//     never prompt either);
//  3. safe tools (allowed outright);
//  4. the prompter + the user's answer (or the timeout / turn
//     cancellation, which deny).
//
// A nil gate allows everything: "no gate installed" is the CLI
// configuration, not a denial.
func (g *Gate) Check(ctx context.Context, tool, args string) (bool, string) {
	if g == nil {
		return true, ""
	}
	tool = strings.TrimSpace(tool)
	// The denylist is absolute and is checked FIRST — before any
	// persisted rule, so an "always allow" rule for shell_exec can
	// never rescue a denylisted command. The tool layer enforces
	// the same denylist underneath, so this is the first of two
	// independent checks.
	if tool == "shell_exec" && isDenylistedCommand(args) {
		return false, "denied: command is blocked by the shell denylist and cannot be approved"
	}
	if g.rules != nil {
		if action, ok := g.rules.Lookup(tool); ok {
			if action == ActionAllow {
				return true, ""
			}
			return false, fmt.Sprintf("denied by permission rule for tool %q", tool)
		}
	}
	if Classify(tool) == ClassSafe {
		return true, ""
	}
	if g.prompter == nil {
		return false, fmt.Sprintf("denied: tool %q needs approval but no approver is configured", tool)
	}
	chatID, senderID, _ := ApprovalContextFrom(ctx)
	req := ApprovalRequest{
		ID:        newRequestID(),
		ChatID:    chatID,
		SenderID:  senderID,
		Tool:      tool,
		Arguments: args,
	}
	g.mu.Lock()
	if g.pending == nil {
		g.pending = map[string]*pendingRequest{}
	}
	pr := &pendingRequest{req: req, ch: make(chan pendingResult, 1)}
	g.pending[req.ID] = pr
	timeout := g.timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	g.mu.Unlock()

	if err := g.prompter(ctx, req); err != nil {
		g.removePending(req.ID)
		return false, fmt.Sprintf("denied: could not ask for approval: %v", err)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-pr.ch:
		if res.persist != nil && g.rules != nil {
			// Persisting "always allow" for a denylisted
			// command must be impossible: the check above
			// already refused the call, so this line is only
			// reachable for non-denylisted calls — but
			// re-check here anyway, so a future refactor
			// that reorders Check cannot create an approval
			// bypass through a persisted rule.
			if *res.persist == ActionAllow && tool == "shell_exec" && isDenylistedCommand(args) {
				return false, "denied: command is blocked by the shell denylist and cannot be approved"
			}
			_ = g.rules.Set(tool, *res.persist)
		}
		if res.approved {
			return true, ""
		}
		return false, fmt.Sprintf("denied: the user declined the %s tool call %q", Classify(tool), tool)
	case <-timer.C:
		g.removePending(req.ID)
		if hook := g.onExpireHook(); hook != nil {
			hook(req)
		}
		return false, "denied: approval timed out (no answer within the approval window)"
	case <-ctx.Done():
		g.removePending(req.ID)
		if hook := g.onExpireHook(); hook != nil {
			hook(req)
		}
		return false, "denied: approval cancelled (the turn ended before an answer arrived)"
	}
}

// Pending returns the pending request with id, for the callback
// handler to validate the approver (requester-or-owner) before
// resolving. ok is false when no such request is waiting (unknown
// or already answered/expired).
func (g *Gate) Pending(id string) (ApprovalRequest, bool) {
	if g == nil {
		return ApprovalRequest{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	pr, ok := g.pending[id]
	if !ok {
		return ApprovalRequest{}, false
	}
	return pr.req, true
}

// PendingCount reports how many approvals are currently waiting.
func (g *Gate) PendingCount() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.pending)
}

// Resolve answers the pending request with id. approved is the
// user's answer; persist, when non-nil, is a standing rule to store
// ("always allow" / "never allow"). It reports whether a request
// was waiting: false means the id is unknown, already answered, or
// expired, and the caller should tell the user the request is gone
// instead of acting on the tap. Resolve never blocks: if Check
// already left (timeout/cancel), the answer is dropped.
func (g *Gate) Resolve(id string, approved bool, persist *Action) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	pr, ok := g.pending[id]
	if !ok {
		g.mu.Unlock()
		return false
	}
	delete(g.pending, id)
	g.mu.Unlock()
	select {
	case pr.ch <- pendingResult{approved: approved, persist: persist}:
	default:
	}
	return true
}

// removePending drops a pending request without answering it (the
// prompter failed, or Check left via timeout/cancel).
func (g *Gate) removePending(id string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.pending, id)
}

// isDenylistedCommand reports whether args (a JSON-object arguments
// string for shell_exec) carries a command the security denylist
// blocks. Unparseable arguments are not denylisted here: the agent
// rejects malformed arguments before the gate runs, and the tool
// layer enforces the denylist again on the real command, so a
// malformed call can never slip through by this path.
func isDenylistedCommand(args string) bool {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(args), &raw); err != nil {
		return false
	}
	var cmd string
	if v, ok := raw["command"]; ok {
		if err := json.Unmarshal(v, &cmd); err != nil {
			return false
		}
	}
	if strings.TrimSpace(cmd) == "" {
		return false
	}
	return security.IsBlockedCommand(cmd)
}
