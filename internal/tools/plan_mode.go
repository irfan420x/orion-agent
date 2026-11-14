// Package tools — plan_mode: EnterPlanMode / ExitPlanMode.
//
// The agent enters a planning state, presents a plan to the user,
// and works ONLY after the user approves it.
//
// Design (blocking wait, mirroring ask_user_question):
//
//   - EnterPlanMode validates the plan (goal, steps, files, risks),
//     stores it per-chat (persisted via the PlanningStore, so a
//     daemon restart does not lose the record — only the live wait),
//     sends it as a Telegram message with Approve / Reject / Revise
//     buttons, and BLOCKS the tool call until the user taps a
//     button, types approve / reject / revise, or the timeout
//     (default 5 minutes, max 30) fires. It returns
//     {"decision":"approved"|"rejected"|"revise",...} and the turn
//     continues: on approval the agent executes the plan in the
//     SAME turn; on revise the stored plan is discarded and the
//     tool returns the user's feedback so the model can revise and
//     re-enter; on rejection the plan is discarded.
//   - ExitPlanMode clears the stored plan for the chat (the escape
//     hatch, and the way to unstick a plan orphaned by a restart).
//     It is idempotent: with no plan pending it reports
//     {"exited":false}.
//
// Approval boundary — reusing the approval-callback machinery:
// the plan is presented through the callback ROUTER (prefix
// "plan:", dispatched by WireTelegram's F0 auto-discovery, which
// wires CallbackHandler and ReplyInterceptor tools
// automatically) with the same patterns as approvals and
// questions: prefix dispatch, sender check (the requester or the
// owner), a single-winner settle, and the message edited in place
// on decision. No second callback system is built, and no
// synthetic gate approval request is minted — the gate's
// approve / deny / always / never semantics do not map to plan
// approve / reject / revise.
//
// No execution while planning: the enter tool implements
// PlanModeChecker, which the agent loop consults (via
// Registry.PlanModeBlocks) before running any tool. While a plan
// is pending for the chat, ClassDestructive tools are refused
// (Safe and Network tools stay allowed, so the agent can research
// while planning). The check is per-chat, expiry-bounded, and
// consults the PERSISTED plan, so it holds across a restart
// (until the plan expires). A refused call never executed, so it
// is not journalled — a re-delivered turn re-decides it.
//
// Restart orphaning: the stored plan survives a restart but its
// waiter (the in-memory channel) does not. The block is
// expiry-bounded (the plan stops blocking at ExpiresAt), and
// ExitPlanMode can always clear it, so a restart can never leave
// a chat stuck in plan mode. A tap on a plan message from before
// a restart is answered with the no-longer-pending notice (the
// F-A20-9 pattern), not silently ignored.
//
// One plan per chat: a second EnterPlanMode while one is pending
// for the same chat is an error. Different chats are isolated —
// each has its own stored plan and its own waiter.
//
// Waiter / store split: the waiter (the live channel) lives on
// the enter tool; the stored plan lives in the shared
// PlanningStore (the same per-chat state the todo/task tools
// use). ExitPlanMode only touches the STORE: it cannot resolve a
// live waiter in the same process (in the daemon the per-chat
// lock makes that overlap impossible — a second turn for the
// chat cannot start while the first is blocked in EnterPlanMode),
// and cross-process the blocked call's timeout bounds the
// window. Every stored-plan clear from the enter tool is
// ID-scoped (clear only if the stored plan is still THIS plan),
// so an orphaned waiter's timeout can never delete a newer plan
// stored after an external ExitPlanMode.
//
// The plan message send is plain text (SendKeyboard sets no
// parse mode); the outcome EDIT goes through
// EditTextRemoveKeyboard, which sends HTML — outcome text is
// escaped here (planEscapeHTML), never passed through raw.
//
// Known wart (shared with the skill restriction): the permission
// gate runs BEFORE the plan-mode check in the agent loop, so a
// destructive call without an allow rule may prompt the user
// first and then be refused by plan mode. Irfan's Always rule
// for shell_exec means no prompt in the common case.
package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
)

// planCallbackPrefix is the callback-router prefix for plan
// decisions. The router reserves it (like "perm:"); callback data
// is "plan:<decision>:<planID>".
const planCallbackPrefix = "plan:"

// planNoLongerPendingNotice is the toast for a tap on a plan
// message whose wait is gone (answered, expired, or lost to a
// restart). One deterministic notice for all three cases — the
// handler cannot tell them apart (F-A20-9).
const planNoLongerPendingNotice = "This plan is no longer pending — it may have been approved, rejected, or lost when the bot restarted."

const (
	// planDefaultTimeout mirrors ask_user_question: long enough
	// for a human to read a plan, short enough that a stuck plan
	// never blocks a chat for long.
	planDefaultTimeout = 5 * time.Minute
	planMinTimeout     = 10 * time.Second
	planMaxTimeout     = 30 * time.Minute

	planMaxGoalRunes     = 1000
	planMaxSteps         = 20
	planMaxStepRunes     = 300
	planMaxFiles         = 20
	planMaxFileRunes     = 200
	planMaxRisks         = 10
	planMaxRiskRunes     = 300
	planMaxMessageRunes  = 3500 // Telegram caps a message at 4096 runes
	planMaxFeedbackRunes = 500  // revision feedback echoed in the outcome edit
)

// planningPlansFile is the per-chat plans file, next to todos.json
// and tasks.json.
const planningPlansFile = "plans.json"

// planState is the persisted pending plan for one chat. There is
// at most one: a second EnterPlanMode while one is pending is an
// error.
type planState struct {
	ID        string   `json:"id"`
	Goal      string   `json:"goal"`
	Steps     []string `json:"steps"`
	Files     []string `json:"files,omitempty"`
	Risks     []string `json:"risks,omitempty"`
	Requester int64    `json:"requester"`
	CreatedAt string   `json:"created_at"`
	ExpiresAt string   `json:"expires_at"`
}

// planDecision is one settled outcome of a plan wait.
type planDecision struct {
	decision string // approved | rejected | revise | timeout
	feedback string // typed revision feedback (revise only)
}

// planWaiter is the live wait for one chat's pending plan: the
// channel EnterPlanMode blocks on. Single-winner: the first
// settle (callback, typed reply, timeout, cancel) wins.
type planWaiter struct {
	planID   string
	chatID   int64
	senderID int64
	msgID    int64 // plan message id, set after the send
	ch       chan planDecision
	settled  bool
}

func newPlanID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate plan id: %w", err)
	}
	return "plan_" + hex.EncodeToString(b[:]), nil
}

func planExpiredAt(p *planState) (expired bool, err error) {
	exp, err := time.Parse(time.RFC3339, p.ExpiresAt)
	if err != nil {
		return false, fmt.Errorf("plan %q has an unparsable expiry %q: %w", p.ID, p.ExpiresAt, err)
	}
	return time.Now().UTC().After(exp), nil
}

// --- PlanningStore persistence ---------------------------------------
// The helpers below follow the todo/task pattern exactly: the
// caller holds s.mu (resolveBucket first, then the lock), the
// file path is plans.json in the resolved bucket dir, the
// in-memory path is memBucket.plan, and writes take the flock
// (lockState) while holding s.mu — the established mu-then-flock
// order. Reads are lock-free apart from s.mu (the atomic rename
// makes a torn read impossible).

// loadStoredPlan returns the pending plan for the resolved bucket
// (dir, key). An expired stored plan is reported as expired=true
// with a nil plan: it no longer blocks anything, and the caller
// overwrites or discards it. Call with s.mu held.
func loadStoredPlan(s *PlanningStore, dir, key string) (plan *planState, expired bool, err error) {
	if dir == "" {
		b := s.bucket(key)
		if b.plan == nil {
			return nil, false, nil
		}
		p := *b.plan
		exp, err := planExpiredAt(&p)
		if err != nil {
			return nil, false, err
		}
		if exp {
			return nil, true, nil
		}
		return &p, false, nil
	}
	data, err := readStateFile(filepath.Join(dir, planningPlansFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var doc struct {
		Plan *planState `json:"plan"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, false, fmt.Errorf("corrupt plans file %q: %w (fix or remove the file to recover)", filepath.Join(dir, planningPlansFile), err)
	}
	if doc.Plan == nil {
		return nil, false, nil
	}
	exp, err := planExpiredAt(doc.Plan)
	if err != nil {
		return nil, false, err
	}
	if exp {
		return nil, true, nil
	}
	return doc.Plan, false, nil
}

// saveStoredPlan persists plan for the resolved bucket. Call with
// s.mu held; the flock is taken here (mu-then-flock, the
// established order).
func saveStoredPlan(s *PlanningStore, ctx context.Context, dir, key string, plan *planState) error {
	release, err := s.lockState(ctx, dir, planningPlansFile)
	if err != nil {
		return err
	}
	defer release()
	if dir == "" {
		b := s.bucket(key)
		cp := *plan
		b.plan = &cp
		return nil
	}
	if err := ensurePlanningDir(dir, s.dataDir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(struct {
		Plan *planState `json:"plan"`
	}{Plan: plan}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode plan: %w", err)
	}
	data = append(data, '\n')
	return atomicWriteFile(filepath.Join(dir, planningPlansFile), data, 0o600)
}

// clearStoredPlan removes whatever plan is stored for the resolved
// bucket (no-op when none). Call with s.mu held; the flock is
// taken here (mu-then-flock).
func clearStoredPlan(s *PlanningStore, ctx context.Context, dir, key string) error {
	release, err := s.lockState(ctx, dir, planningPlansFile)
	if err != nil {
		return err
	}
	defer release()
	if dir == "" {
		b := s.bucket(key)
		b.plan = nil
		return nil
	}
	path := filepath.Join(dir, planningPlansFile)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// clearStoredPlanIf removes the stored plan only when it is still
// THIS plan (same id). An orphaned waiter's timeout must never
// delete a newer plan stored after an external ExitPlanMode —
// see the header comment. Call with s.mu held.
func clearStoredPlanIf(s *PlanningStore, ctx context.Context, dir, key, planID string) error {
	current, _, err := loadStoredPlan(s, dir, key)
	if err != nil {
		return err
	}
	if current == nil || current.ID != planID {
		return nil
	}
	return clearStoredPlan(s, ctx, dir, key)
}

// --- text helpers ------------------------------------------------------

// planEscapeHTML escapes text for the outcome EDIT, which goes
// through EditTextRemoveKeyboard (HTML). The plan message SEND is
// plain text and needs no escaping.
func planEscapeHTML(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func planTruncate(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes]) + "…"
}

// formatPlanTimeout renders a timeout for the plan message
// ("5 minutes", "30 seconds").
func formatPlanTimeout(d time.Duration) string {
	if d >= time.Minute {
		m := int(d.Minutes() + 0.5)
		if m == 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", m)
	}
	s := int(d.Seconds() + 0.5)
	if s == 1 {
		return "1 second"
	}
	return fmt.Sprintf("%d seconds", s)
}

// renderPlanMessage renders the plan for the Telegram message
// (plain text — SendKeyboard sets no parse mode).
func renderPlanMessage(plan *planState, timeout time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📋 Plan: %s\n", plan.Goal)
	b.WriteString("\nSteps:\n")
	for i, s := range plan.Steps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, s)
	}
	if len(plan.Files) > 0 {
		b.WriteString("\nFiles:\n")
		for _, f := range plan.Files {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	if len(plan.Risks) > 0 {
		b.WriteString("\nRisks:\n")
		for _, r := range plan.Risks {
			fmt.Fprintf(&b, "- %s\n", r)
		}
	}
	fmt.Fprintf(&b, "\nApprove to let me run it, Reject to drop it, or Revise to change it.\n(You have %s.)", formatPlanTimeout(timeout))
	return b.String()
}

// renderPlanOutcome renders the terminal state of the plan message
// (HTML — it goes through EditTextRemoveKeyboard).
func renderPlanOutcome(plan *planState, d planDecision) string {
	goal := planEscapeHTML(plan.Goal)
	switch d.decision {
	case "approved":
		return fmt.Sprintf("📋 Plan: %s\n\n✅ Approved — running the plan now.", goal)
	case "rejected":
		return fmt.Sprintf("📋 Plan: %s\n\n❌ Rejected — the plan was dropped.", goal)
	case "revise":
		if fb := strings.TrimSpace(d.feedback); fb != "" {
			return fmt.Sprintf("📋 Plan: %s\n\n✏️ Revision requested: %s", goal, planEscapeHTML(planTruncate(fb, planMaxFeedbackRunes)))
		}
		return fmt.Sprintf("📋 Plan: %s\n\n✏️ Revision requested — the plan was dropped; I will revise it.", goal)
	case "timeout":
		return fmt.Sprintf("📋 Plan: %s\n\n⏰ No decision in time — the plan was dropped.", goal)
	default:
		return fmt.Sprintf("📋 Plan: %s\n\nThe plan was dropped.", goal)
	}
}

func planKeyboard(planID string) InlineKeyboard {
	return InlineKeyboard{Rows: [][]InlineButton{
		{{Text: "✅ Approve plan", CallbackData: planCallbackPrefix + "approve:" + planID}},
		{{Text: "❌ Reject", CallbackData: planCallbackPrefix + "reject:" + planID}},
		{{Text: "✏️ Revise", CallbackData: planCallbackPrefix + "revise:" + planID}},
	}}
}

// parsePlanCallback parses "plan:<decision>:<planID>".
func parsePlanCallback(data string) (decision, planID string, ok bool) {
	if !strings.HasPrefix(data, planCallbackPrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(data, planCallbackPrefix)
	d, id, found := strings.Cut(rest, ":")
	if !found || id == "" {
		return "", "", false
	}
	switch d {
	case "approve", "reject", "revise":
		return d, id, true
	default:
		return "", "", false
	}
}

// parsePlanReply maps a typed reply to a decision. The match is
// EXACT (after trim, lowercase, and stripping trailing "." / "!"):
// "yes, but change X" is not an approval — it is a revision with
// the whole text as feedback. Anything that is not an exact
// approve/reject word is a revision: while a plan is pending, a
// typed message is most plausibly feedback on the plan.
func parsePlanReply(text string) planDecision {
	t := strings.ToLower(strings.TrimSpace(text))
	t = strings.Trim(t, ".!")
	switch t {
	case "approve", "approved", "yes", "ok", "okay", "go", "go ahead", "do it", "run it", "ship it", "lgtm", "👍", "✅":
		return planDecision{decision: "approved"}
	case "reject", "rejected", "no", "deny", "denied", "cancel", "stop", "👎", "❌", "don't", "do not", "dont":
		return planDecision{decision: "rejected"}
	default:
		return planDecision{decision: "revise", feedback: text}
	}
}

func decisionResult(plan *planState, d planDecision) (string, error) {
	out := map[string]any{
		"decision": d.decision,
		"plan_id":  plan.ID,
		"goal":     plan.Goal,
	}
	if d.feedback != "" {
		out["feedback"] = d.feedback
	}
	data, err := json.Marshal(out)
	if err != nil {
		return "", &ToolError{Tool: "enter_plan_mode", Kind: "execution", Err: err}
	}
	return string(data), nil
}

// --- enter_plan_mode -----------------------------------------------------

// enterPlanModeTool presents a plan to the user and waits for
// their decision. It owns the live waiters (in-memory) and
// implements the callback handler, the reply interceptor, the
// transport install, and the plan-mode checker. The persisted
// plan lives in the shared PlanningStore (see the store helpers
// above), so the exit tool — which only touches the store —
// sees the same plan.
type enterPlanModeTool struct {
	store *PlanningStore

	mu      sync.Mutex
	waiters map[int64]*planWaiter // by chat id; one plan per chat

	gw      CallbackGateway
	ownerID int64
}

func (t *enterPlanModeTool) Name() string { return "enter_plan_mode" }

func (t *enterPlanModeTool) Description() string {
	return "Enter plan mode: present a plan (goal, steps, files, risks) to the user and wait for their approval before doing anything mutating. The tool blocks until the user approves, rejects, or asks for a revision (button tap or typed reply), or the timeout fires. While a plan is pending, destructive tools are blocked for the chat. Returns {\"decision\":\"approved\"|\"rejected\"|\"revise\",...}. Telegram only: needs a chat in the run context."
}

func (t *enterPlanModeTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"goal": map[string]any{
					"type":        "string",
					"description": "What the plan will achieve, in one or two sentences.",
				},
				"steps": map[string]any{
					"type":        "array",
					"description": "The ordered steps of the plan (1-20, each a short sentence).",
					"items":       map[string]any{"type": "string"},
				},
				"files": map[string]any{
					"type":        "array",
					"description": "Files the plan will read, create, or modify (0-20, each a path).",
					"items":       map[string]any{"type": "string"},
				},
				"risks": map[string]any{
					"type":        "array",
					"description": "Risks and what could go wrong (0-10, each a short sentence).",
					"items":       map[string]any{"type": "string"},
				},
				"timeout_seconds": map[string]any{
					"type":        "number",
					"description": "How long to wait for the user's decision, in seconds (10-1800; default 300).",
				},
			},
			"required": []string{"goal", "steps"},
		},
	}
}

func (t *enterPlanModeTool) RiskClass() permissions.Class { return permissions.ClassSafe }

// InstallTransport receives the Telegram gateway and the owner id.
// A nil host (or a nil gateway) means "no transport": the tool
// records that and fails cleanly at Execute time instead of
// failing the install.
func (t *enterPlanModeTool) InstallTransport(host *ToolHost) error {
	if host == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.gw = host.Gateway
	t.ownerID = host.OwnerID
	return nil
}

// PlanModeBlocks is the PlanModeChecker the agent loop consults
// before running any tool. Only ClassDestructive tools are
// blocked, and only while a non-expired plan is stored for the
// chat in ctx. Safe and Network tools stay allowed (research
// while planning), and a chat with no plan never blocks. A
// corrupt or unreadable plans file fails OPEN here (returns
// false): failing closed would block the chat forever, because
// the plan tools themselves also fail on a corrupt file and
// could not clear it — the corruption then surfaces as an error
// at the next plan-tool call, which tells the user to fix or
// remove the file (the same rule as a corrupt todos file).
func (t *enterPlanModeTool) PlanModeBlocks(ctx context.Context, tool string) (bool, string) {
	if permissions.Classify(tool) != permissions.ClassDestructive {
		return false, ""
	}
	if t == nil || t.store == nil {
		return false, ""
	}
	dir, key := t.store.resolveBucket(ctx)
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	plan, _, err := loadStoredPlan(t.store, dir, key)
	if err != nil || plan == nil {
		return false, ""
	}
	return true, fmt.Sprintf("plan %q (%s) is pending approval for this chat", plan.ID, planTruncate(plan.Goal, 80))
}

// CallbackPrefix is the router prefix for plan decisions.
func (t *enterPlanModeTool) CallbackPrefix() string { return planCallbackPrefix }

// HandleCallback settles the wait for the tapped plan. It only
// settles: the blocked EnterPlanMode call does the cleanup (clear
// the stored plan, edit the message) when it wakes, so the
// outcome edit lives in exactly one place. The tap is answered
// with a short toast either way — a failed answer must never
// fail the decision it acknowledges.
func (t *enterPlanModeTool) HandleCallback(ctx context.Context, gw CallbackGateway, q CallbackQuery) error {
	decision, planID, ok := parsePlanCallback(q.CallbackData())
	if !ok {
		// Not our shape (the router only routes our prefix here,
		// so this is a malformed payload): acknowledge it and
		// leave the plan alone.
		_ = gw.AnswerCallback(ctx, q.CallbackID(), planNoLongerPendingNotice)
		return nil
	}
	// Normalize to the result vocabulary: the button says
	// "approve", the result says "approved".
	switch decision {
	case "approve":
		decision = "approved"
	case "reject":
		decision = "rejected"
	}
	senderID := q.SenderID()
	chatID := q.MessageChatID()
	toast := ""
	if chatID != 0 && senderID != 0 {
		if t.settleWaiter(chatID, planID, senderID, planDecision{decision: decision}) {
			switch decision {
			case "approved":
				toast = "Plan approved."
			case "rejected":
				toast = "Plan rejected."
			case "revise":
				toast = "Revision requested."
			}
		} else {
			// No live waiter for this chat, a stale plan id, or
			// a sender who may not decide this plan: the tap
			// changes nothing. A stranger gets the honest
			// "not yours" toast, not the no-longer-pending one.
			switch w := t.waiterForChat(chatID); {
			case w == nil:
				toast = planNoLongerPendingNotice
			case w.planID != planID:
				toast = planNoLongerPendingNotice
			default:
				toast = "This plan can only be decided by the person who asked for it."
			}
		}
	} else {
		toast = planNoLongerPendingNotice
	}
	_ = gw.AnswerCallback(ctx, q.CallbackID(), toast)
	return nil
}

// TryAnswer consumes a typed reply while a plan wait is live for
// the chat. It settles the waiter only — the blocked
// EnterPlanMode call does the cleanup when it wakes. It returns
// false unless the update is definitely this plan's: same chat,
// authorised sender (requester or owner), not a command, non-empty
// text. A greedy interceptor that eats other tools' updates or
// normal turns is a correctness bug.
func (t *enterPlanModeTool) TryAnswer(upd ReplyUpdate) bool {
	if upd == nil || upd.IsCommand() {
		return false
	}
	chatID := upd.ChatID()
	if chatID == 0 {
		return false
	}
	text := strings.TrimSpace(upd.Text())
	if text == "" {
		return false
	}
	senderID := upd.SenderID()
	if senderID == 0 {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	w, ok := t.waiters[chatID]
	if !ok || w.settled {
		return false
	}
	if senderID != w.senderID && senderID != t.ownerID {
		return false
	}
	w.settled = true
	delete(t.waiters, chatID)
	w.ch <- parsePlanReply(text)
	return true
}

// waiterForChat returns the live waiter for a chat, or nil. For
// diagnostics and the callback handler's toast choice; the
// caller must not mutate the waiter.
func (t *enterPlanModeTool) waiterForChat(chatID int64) *planWaiter {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.waiters[chatID]
}

// settleWaiter settles the live waiter for chatID when it is the
// wait for planID and senderID may decide it (requester or
// owner). Single-winner: the first settle wins; a later one is a
// no-op. Returns whether this call settled the wait.
func (t *enterPlanModeTool) settleWaiter(chatID int64, planID string, senderID int64, d planDecision) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	w, ok := t.waiters[chatID]
	if !ok || w.settled {
		return false
	}
	if w.planID != planID {
		return false
	}
	if senderID != w.senderID && senderID != t.ownerID {
		return false
	}
	w.settled = true
	delete(t.waiters, chatID)
	w.ch <- d
	return true
}

// removeWaiter removes waiter w from the map, but only when the
// map still holds w (it may have been replaced by a newer wait
// after an external ExitPlanMode). Used by the timeout / cancel
// paths, which are the only ones that can still own a waiter
// nobody settled.
func (t *enterPlanModeTool) removeWaiter(w *planWaiter) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if cur, ok := t.waiters[w.chatID]; ok && cur == w {
		delete(t.waiters, w.chatID)
	}
}

// clearStoredPlanIfBestEffort clears the stored plan when it is
// still this plan (ID-scoped, see the header comment). Errors
// are ignored: the plan is expiry-bounded, and ExitPlanMode can
// always clear it — a failed clear must never fail the decision
// it follows.
func (t *enterPlanModeTool) clearStoredPlanIfBestEffort(ctx context.Context, planID string) {
	if t.store == nil {
		return
	}
	dir, key := t.store.resolveBucket(ctx)
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	_ = clearStoredPlanIf(t.store, ctx, dir, key, planID)
}

// editOutcomeBestEffort edits the plan message to its terminal
// state. Errors are ignored (a failed edit must never fail the
// decision), and a cancelled context simply skips the edit.
func (t *enterPlanModeTool) editOutcomeBestEffort(ctx context.Context, gw CallbackGateway, plan *planState, w *planWaiter, d planDecision) {
	if gw == nil || w.msgID == 0 {
		return
	}
	_ = gw.EditTextRemoveKeyboard(ctx, w.chatID, w.msgID, renderPlanOutcome(plan, d))
}

// finishWait is the single cleanup site for a decided wait: clear
// the stored plan (ID-scoped), edit the plan message to its
// terminal state, and return the decision result.
func (t *enterPlanModeTool) finishWait(ctx context.Context, gw CallbackGateway, plan *planState, w *planWaiter, d planDecision) (string, error) {
	t.clearStoredPlanIfBestEffort(ctx, plan.ID)
	t.editOutcomeBestEffort(ctx, gw, plan, w, d)
	return decisionResult(plan, d)
}

type enterPlanModeArgs struct {
	Goal           string   `json:"goal"`
	Steps          []string `json:"steps"`
	Files          []string `json:"files"`
	Risks          []string `json:"risks"`
	TimeoutSeconds float64  `json:"timeout_seconds,omitempty"`
}

func (t *enterPlanModeTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a enterPlanModeArgs
	if err := decodeArgs(t.Name(), args, &a); err != nil {
		return "", err
	}
	invalid := func(format string, v ...any) (string, error) {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf(format, v...)}
	}
	goal := strings.TrimSpace(a.Goal)
	if goal == "" {
		return invalid("goal must not be empty")
	}
	if utf8.RuneCountInString(goal) > planMaxGoalRunes {
		return invalid("goal is too long (%d runes, max %d)", utf8.RuneCountInString(goal), planMaxGoalRunes)
	}
	if len(a.Steps) == 0 {
		return invalid("steps must not be empty")
	}
	if len(a.Steps) > planMaxSteps {
		return invalid("too many steps (%d, max %d)", len(a.Steps), planMaxSteps)
	}
	steps := make([]string, 0, len(a.Steps))
	for i, s := range a.Steps {
		s = strings.TrimSpace(s)
		if s == "" {
			return invalid("steps[%d] must not be empty", i)
		}
		if utf8.RuneCountInString(s) > planMaxStepRunes {
			return invalid("steps[%d] is too long (%d runes, max %d)", i, utf8.RuneCountInString(s), planMaxStepRunes)
		}
		steps = append(steps, s)
	}
	if len(a.Files) > planMaxFiles {
		return invalid("too many files (%d, max %d)", len(a.Files), planMaxFiles)
	}
	files := make([]string, 0, len(a.Files))
	for i, f := range a.Files {
		f = strings.TrimSpace(f)
		if f == "" {
			return invalid("files[%d] must not be empty", i)
		}
		if utf8.RuneCountInString(f) > planMaxFileRunes {
			return invalid("files[%d] is too long (%d runes, max %d)", i, utf8.RuneCountInString(f), planMaxFileRunes)
		}
		files = append(files, f)
	}
	if len(a.Risks) > planMaxRisks {
		return invalid("too many risks (%d, max %d)", len(a.Risks), planMaxRisks)
	}
	risks := make([]string, 0, len(a.Risks))
	for i, r := range a.Risks {
		r = strings.TrimSpace(r)
		if r == "" {
			return invalid("risks[%d] must not be empty", i)
		}
		if utf8.RuneCountInString(r) > planMaxRiskRunes {
			return invalid("risks[%d] is too long (%d runes, max %d)", i, utf8.RuneCountInString(r), planMaxRiskRunes)
		}
		risks = append(risks, r)
	}
	timeout := planDefaultTimeout
	if a.TimeoutSeconds != 0 {
		if a.TimeoutSeconds < float64(planMinTimeout/time.Second) || a.TimeoutSeconds > float64(planMaxTimeout/time.Second) {
			return invalid("timeout_seconds must be between %d and %d, got %v", int(planMinTimeout/time.Second), int(planMaxTimeout/time.Second), a.TimeoutSeconds)
		}
		timeout = time.Duration(a.TimeoutSeconds * float64(time.Second))
	}

	chatID, senderID, ok := permissions.ApprovalContextFrom(ctx)
	if !ok || chatID == 0 {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: errors.New("enter_plan_mode needs a Telegram chat: this run has no chat in its context")}
	}
	if senderID == 0 {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: errors.New("enter_plan_mode needs a sender: this run has no sender in its context")}
	}
	t.mu.Lock()
	gw := t.gw
	t.mu.Unlock()
	if gw == nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: errors.New("enter_plan_mode is not available here: no Telegram transport is configured (it needs a Telegram chat)")}
	}
	if t.store == nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: errors.New("enter_plan_mode: no planning store configured")}
	}

	id, err := newPlanID()
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	now := time.Now().UTC()
	plan := &planState{
		ID:        id,
		Goal:      goal,
		Steps:     steps,
		Files:     files,
		Risks:     risks,
		Requester: senderID,
		CreatedAt: now.Format(time.RFC3339),
		ExpiresAt: now.Add(timeout).Format(time.RFC3339),
	}
	msg := renderPlanMessage(plan, timeout)
	if utf8.RuneCountInString(msg) > planMaxMessageRunes {
		return invalid("the rendered plan message is too long (%d runes, max %d): shorten the goal, steps, files, or risks", utf8.RuneCountInString(msg), planMaxMessageRunes)
	}

	// One plan per chat: refuse when one is already pending. An
	// expired stored plan is overwritten below.
	dir, key := t.store.resolveBucket(ctx)
	t.store.mu.Lock()
	existing, _, err := loadStoredPlan(t.store, dir, key)
	if err != nil {
		t.store.mu.Unlock()
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	if existing != nil {
		t.store.mu.Unlock()
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("already in plan mode for this chat: plan %q is pending approval; wait for the decision or call exit_plan_mode", existing.ID)}
	}
	if err := saveStoredPlan(t.store, ctx, dir, key, plan); err != nil {
		t.store.mu.Unlock()
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	t.store.mu.Unlock()

	// Register the waiter BEFORE sending: a tap can only arrive
	// after the message exists, and the waiter must already be
	// there when it does. (If an orphaned waiter from an
	// externally-cleared plan is still in the map, it is replaced
	// here; its timeout cleanup is ID-scoped, so it cannot delete
	// this plan.)
	w := &planWaiter{planID: id, chatID: chatID, senderID: senderID, ch: make(chan planDecision, 1)}
	t.mu.Lock()
	t.waiters[chatID] = w
	t.mu.Unlock()

	msgID, err := gw.SendKeyboard(ctx, chatID, msg, planKeyboard(id))
	if err != nil {
		t.removeWaiter(w)
		t.clearStoredPlanIfBestEffort(ctx, id)
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("send plan message: %w", err)}
	}
	t.mu.Lock()
	w.msgID = msgID
	t.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case d := <-w.ch:
		return t.finishWait(ctx, gw, plan, w, d)
	case <-timer.C:
		// A decision that arrived at the same instant wins:
		// drain the channel before calling it a timeout.
		select {
		case d := <-w.ch:
			return t.finishWait(ctx, gw, plan, w, d)
		default:
		}
		t.removeWaiter(w)
		t.clearStoredPlanIfBestEffort(ctx, id)
		t.editOutcomeBestEffort(ctx, gw, plan, w, planDecision{decision: "timeout"})
		return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: fmt.Errorf("no decision within %s; the plan was dropped", formatPlanTimeout(timeout))}
	case <-ctx.Done():
		select {
		case d := <-w.ch:
			return t.finishWait(ctx, gw, plan, w, d)
		default:
		}
		t.removeWaiter(w)
		t.clearStoredPlanIfBestEffort(ctx, id)
		return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: fmt.Errorf("plan wait cancelled: %v", ctx.Err())}
	}
}

// --- exit_plan_mode ------------------------------------------------------

// exitPlanModeTool clears the pending plan for the chat: the
// escape hatch, and the way to unstick a plan orphaned by a
// restart (the stored plan survives a restart; its waiter does
// not). It only touches the store — it never resolves a live
// waiter (in the daemon the per-chat lock makes that overlap
// impossible; cross-process the blocked call's timeout bounds
// the window). Idempotent: with no plan pending it reports
// {"exited":false}.
type exitPlanModeTool struct {
	store *PlanningStore
}

func (t *exitPlanModeTool) Name() string { return "exit_plan_mode" }

func (t *exitPlanModeTool) Description() string {
	return "Exit plan mode: abandon the pending plan for this chat, if any, so destructive tools are unblocked again. Use it when the plan is no longer wanted, or to clear a plan orphaned by a restart. Idempotent: with no plan pending it reports {\"exited\":false}. Takes no arguments."
}

func (t *exitPlanModeTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}
}

func (t *exitPlanModeTool) RiskClass() permissions.Class { return permissions.ClassSafe }

func (t *exitPlanModeTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	// No arguments: args are ignored (the agent passes {}).
	if t.store == nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: errors.New("exit_plan_mode: no planning store configured")}
	}
	dir, key := t.store.resolveBucket(ctx)
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	plan, expired, err := loadStoredPlan(t.store, dir, key)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	if plan == nil && !expired {
		return `{"exited":false,"note":"no plan is pending for this chat"}`, nil
	}
	if err := clearStoredPlan(t.store, ctx, dir, key); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	if expired {
		return `{"exited":true,"note":"the pending plan had already expired and was discarded"}`, nil
	}
	out, err := json.Marshal(map[string]any{"exited": true, "plan_id": plan.ID})
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	return string(out), nil
}

func init() {
	RegisterFactory("enter_plan_mode", func(deps ToolDeps) (Tool, error) {
		store := deps.PlanningStore
		if store == nil {
			store = NewPlanningStore("")
		}
		return &enterPlanModeTool{store: store, waiters: map[int64]*planWaiter{}}, nil
	})
	RegisterFactory("exit_plan_mode", func(deps ToolDeps) (Tool, error) {
		store := deps.PlanningStore
		if store == nil {
			store = NewPlanningStore("")
		}
		return &exitPlanModeTool{store: store}, nil
	})
}
