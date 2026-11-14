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
//   - A persisted rule store (permissions.json, 0600) holds
//     per-chat allow/deny rules (F-A16-3): an "always allow" tapped
//     in one chat applies to that chat only, never to other chats.
//     Rules written before rc22 carry no chat and load as legacy
//     global rules (see RuleStore); a deny rule is never prompted;
//     an allow rule never prompts either. Rules are the user's
//     standing decisions.
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
	"log"
	"os"
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

// Rule is one persisted per-chat decision. ChatID 0 is a legacy
// global rule: it was written before rules were per-chat (rc22)
// and applies to every chat. Every rule written by rc22 or later
// carries the chat it was tapped in.
type Rule struct {
	ChatID int64  `json:"chat_id,omitempty"`
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
// channel Resolve delivers on. resolved records the outcome Resolve
// delivered (set under the gate mutex before the channel send), so a
// timer/cancel branch that fires in the same instant as a tap can
// prefer the delivered result instead of denying a call the user
// just approved (F-A5-P2-20).
//
// settled + timer close the race by construction (F-A6-P2-8): the
// timer branch, the cancellation branch and Resolve all settle the
// request through one mutex-guarded transition (see settle), so
// exactly one of them wins. The loser observes the winner's
// outcome: a Resolve that loses to the timer returns false (the
// callback handler then reports the request expired, instead of
// telling the user "Approved" for a call the gate denied), and a
// timer branch that loses to Resolve honours the approval. Before
// this, Resolve never stopped the timer and the timer branch only
// re-checked the recorded outcome — a tap landing after that
// re-check but before the expiry hook still produced "approved"
// on the card and "denied" in the loop.
type pendingRequest struct {
	req      ApprovalRequest
	ch       chan pendingResult
	resolved *pendingResult
	// timer is the approval-window timer Check armed for this
	// request (nil until Check creates it); Resolve stops it
	// under the gate mutex as part of settling, so a stopped
	// timer can never fire after the request is answered.
	timer *time.Timer
	// settled is true once exactly one of Resolve, the timer
	// branch or the cancellation branch has claimed the
	// request. It is read and written only under the gate
	// mutex, and settle is the only transition that sets it.
	settled bool
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
	// logger receives gate diagnostics (prompter failures and
	// other silent paths — F-A5-P2-22). It defaults to stderr;
	// SetLogger replaces it. loggerMu guards it.
	logger   *log.Logger
	loggerMu sync.RWMutex
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
		logger:   log.New(os.Stderr, "orion/permissions: ", log.LstdFlags),
	}
}

// SetLogger replaces the gate's diagnostic logger. A nil logger is
// ignored. It never blocks on a waiting Check.
func (g *Gate) SetLogger(l *log.Logger) {
	if g == nil || l == nil {
		return
	}
	g.loggerMu.Lock()
	defer g.loggerMu.Unlock()
	g.logger = l
}

// logf writes a formatted diagnostic line, never panicking on a nil
// logger.
func (g *Gate) logf(format string, args ...any) {
	if g == nil {
		return
	}
	g.loggerMu.RLock()
	l := g.logger
	g.loggerMu.RUnlock()
	if l != nil {
		l.Printf(format, args...)
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
//  2. a persisted rule for this chat (deny rules are never prompted,
//     allow rules never prompt either); a rule from another chat
//     never applies here (F-A16-3 — per-chat isolation at the
//     permission layer, matching the history/model/budget layers);
//     a legacy global rule, written before rules were per-chat,
//     still applies to every chat;
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
	// The chat comes from the turn's context (set by the daemon's
	// turn handler from the Telegram update). The rule lookup is
	// per-chat: another chat's "always allow" must never approve
	// a call here (F-A16-3). A turn with no chat in its context
	// (a local run, which never installs a gate anyway) falls
	// back to the global rules only.
	chatID, senderID, _ := ApprovalContextFrom(ctx)
	if g.rules != nil {
		if action, ok := g.rules.LookupForChat(chatID, tool); ok {
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
		// Log the failure (F-A5-P2-22): a failed card send is
		// otherwise silent and indistinguishable from "the user
		// never answered". The request ID names the prompt that
		// never reached the user.
		g.logf("approval request %s: prompter failed for tool %q (chat %d): %v", req.ID, tool, req.ChatID, err)
		return false, fmt.Sprintf("denied: could not ask for approval: %v", err)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	g.mu.Lock()
	pr.timer = timer
	g.mu.Unlock()
	select {
	case res := <-pr.ch:
		return g.applyResult(tool, args, req.ChatID, res)
	case <-timer.C:
		// Timer vs Resolve, decided by construction
		// (F-A6-P2-8): settle claims the request under the
		// gate mutex, so exactly one of this branch and
		// Resolve wins. If this branch wins, Resolve already
		// returned (or will return) false and the expiry hook
		// fires exactly once. If Resolve won, its outcome is
		// recorded and honoured here instead of expiring the
		// request. (A loss with no recorded outcome means
		// the cancellation branch won instead — the turn is
		// over either way, so the denial below is correct.)
		if g.settle(pr, nil) {
			if hook := g.onExpireHook(); hook != nil {
				hook(req)
			}
			return false, "denied: approval timed out (no answer within the approval window)"
		}
		if res, ok := g.resolvedOutcome(pr); ok {
			return g.applyResult(tool, args, req.ChatID, res)
		}
		return false, "denied: approval timed out (no answer within the approval window)"
	case <-ctx.Done():
		// Same single-winner settlement as the timer branch:
		// a tap landing as the turn is cancelled wins over
		// the cancellation when Resolve already settled it.
		if g.settle(pr, nil) {
			if hook := g.onExpireHook(); hook != nil {
				hook(req)
			}
			return false, "denied: approval cancelled (the turn ended before an answer arrived)"
		}
		if res, ok := g.resolvedOutcome(pr); ok {
			return g.applyResult(tool, args, req.ChatID, res)
		}
		return false, "denied: approval cancelled (the turn ended before an answer arrived)"
	}
}

// resolvedOutcome returns the outcome Resolve recorded on pr, if
// any. Resolve sets it under the gate mutex before delivering to
// the channel, so this read is ordered against it: a non-nil
// result means the user answered before the timer/cancel branch
// ran, even if the select picked the timer case first.
func (g *Gate) resolvedOutcome(pr *pendingRequest) (pendingResult, bool) {
	if g == nil || pr == nil {
		return pendingResult{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if pr.resolved == nil {
		return pendingResult{}, false
	}
	return *pr.resolved, true
}

// applyResult applies a delivered approval outcome: it persists an
// "always allow" / "never allow" rule when one was requested (a
// denylisted command can never be persisted as allowed — see the
// re-check below) and reports whether the call may run. The rule
// is persisted for chatID only (F-A16-3): an "always allow" tapped
// in one chat must never approve the tool in another chat. A
// chatID of 0 (no chat in the turn's context) falls back to a
// global rule, the pre-rc22 behaviour.
func (g *Gate) applyResult(tool, args string, chatID int64, res pendingResult) (bool, string) {
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
		if chatID != 0 {
			_ = g.rules.SetForChat(chatID, tool, *res.persist)
		} else {
			_ = g.rules.Set(tool, *res.persist)
		}
	}
	if res.approved {
		return true, ""
	}
	return false, fmt.Sprintf("denied: the user declined the %s tool call %q", Classify(tool), tool)
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

// settle is the single state transition for a pending request
// (F-A6-P2-8): Resolve, the timer branch and the cancellation
// branch all settle through here, under the gate mutex, so
// exactly one of them wins. It reports whether this call won:
// the winner owns the outcome (a recorded res, or the expiry
// for a nil res), and every loser must observe the winner's
// outcome instead of acting. Settling removes the request from
// pending, stops its timer (so a timer stopped in time never
// fires after an answer), and — for a Resolve — records the
// outcome before the caller delivers it on the channel.
func (g *Gate) settle(pr *pendingRequest, res *pendingResult) bool {
	if g == nil || pr == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.settleLocked(pr, res)
}

// settleLocked is settle with the gate mutex already held.
func (g *Gate) settleLocked(pr *pendingRequest, res *pendingResult) bool {
	if pr.settled {
		return false
	}
	pr.settled = true
	if res != nil {
		pr.resolved = res
	}
	if pr.timer != nil {
		pr.timer.Stop()
	}
	delete(g.pending, pr.req.ID)
	return true
}

// Resolve answers the pending request with id. approved is the
// user's answer; persist, when non-nil, is a standing rule to store
// ("always allow" / "never allow"). It reports whether a request
// was waiting: false means the id is unknown, already answered, or
// expired, and the caller should tell the user the request is gone
// instead of acting on the tap. Resolve never blocks: if Check
// already left (timeout/cancel), the answer is dropped. Exactly
// one of Resolve and the timer/cancel branch wins the settle
// (F-A6-P2-8): a Resolve that returns true is guaranteed to be
// honoured by Check, and one that returns false lost to an expiry
// Check already reported (or is reporting) as a denial.
func (g *Gate) Resolve(id string, approved bool, persist *Action) bool {
	if g == nil {
		return false
	}
	// The lookup and the settle happen under one lock hold:
	// a prompter-failure removal landing between them must not
	// let Resolve report success for a request Check already
	// abandoned.
	g.mu.Lock()
	pr, ok := g.pending[id]
	if !ok {
		g.mu.Unlock()
		return false
	}
	// Record the outcome under the mutex BEFORE delivering it:
	// a timer/cancel branch racing this Resolve re-checks the
	// recorded outcome (see resolvedOutcome) and honours the
	// user's answer instead of expiring the request (F-A5-P2-20).
	// settleLocked makes the win atomic: if the timer branch
	// already settled the request, this Resolve loses and
	// returns false instead of recording an approval Check
	// will deny.
	res := pendingResult{approved: approved, persist: persist}
	won := g.settleLocked(pr, &res)
	g.mu.Unlock()
	if !won {
		return false
	}
	select {
	case pr.ch <- res:
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
