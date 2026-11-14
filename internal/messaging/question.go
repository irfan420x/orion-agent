// Package messaging — ask_user_question transport.
//
// The agent's ask_user_question tool (internal/tools) blocks waiting
// for the user's answer. This file is the Telegram transport for
// that wait, reusing the approval-callback machinery instead of a
// second parallel callback system:
//
//   - The question goes out as a Telegram message: inline-button
//     options when the model supplies them (callback data
//     "ask:<questionID>:<optionIndex>"), free text otherwise.
//   - A tap resolves through the installed callback handler — the
//     same dispatch as approvals, the same requester-or-owner
//     sender check, the same expiry-edit pattern.
//   - A typed reply is intercepted in PollStream BEFORE the
//     per-chat lock: the turn that asked is blocked waiting for
//     the answer and holds that lock, so routing the reply through
//     a turn would deadlock until the question timed out. A
//     command ("/...") is never an answer — it flows to the normal
//     turn path.
//   - Timeout and turn cancellation settle the question exactly
//     once (the gate's single-winner settle pattern), so a late
//     tap is told "expired" and a waiting tool never hangs
//     forever — including across a daemon restart, which cancels
//     the turn context.
package messaging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// questionCallbackPrefix is the callback_data prefix for question
// option buttons. The format is "ask:<questionID>:<optionIndex>",
// where the index is the 0-based position in the question's
// options. The option TEXT never travels in callback_data (Telegram
// caps it at 64 bytes) — only the index, resolved against the
// pending question.
const questionCallbackPrefix = "ask:"

// questionResult is one answered question: the answer text, how it
// arrived ("button" for an option tap, "text" for a typed reply),
// and the tapped option index (0-based; -1 for a typed reply).
type questionResult struct {
	answer      string
	source      string
	optionIndex int
}

// pendingQuestion is one question waiting on the user.
type pendingQuestion struct {
	id       string
	chatID   int64
	senderID int64
	question string
	options  []string
	// msgID is the sent question message (0 until the send
	// returns), for the expiry edit.
	msgID int64
	// ch delivers the answer (buffered: a tap that lands while
	// Ask is still sending must not block, exactly as the
	// approval gate's pending channel).
	ch chan questionResult
	// timer is the timeout, created after the send returns.
	timer *time.Timer
	// resolved is the outcome Resolve recorded, if any — read by
	// the timeout/cancel branch that lost the race, so a tap
	// landing as the timer fires still wins.
	resolved *questionResult
	// settled is set once, under the registry mutex, by whichever
	// of Resolve / timeout / cancellation wins first.
	settled bool
}

// QuestionRegistry tracks pending ask_user_question questions by
// ID. It is the question analogue of ApprovalCards plus the
// gate's pending map: the asker registers, the callback handler
// and the free-text interception resolve, and timeout/cancel
// settle. All methods are safe for concurrent use; a nil registry
// means "no questions" (every method is nil-safe, like
// ApprovalCards).
type QuestionRegistry struct {
	mu      sync.Mutex
	m       map[string]*pendingQuestion
	ownerID int64
}

// NewQuestionRegistry returns an empty question registry. ownerID
// is the owner (first allowlisted chat): the owner may answer any
// question, like approvals.
func NewQuestionRegistry(ownerID int64) *QuestionRegistry {
	return &QuestionRegistry{m: map[string]*pendingQuestion{}, ownerID: ownerID}
}

// newQuestionID returns a random question ID (16 hex chars of
// randomness, never sequential — the same shape as the gate's
// request IDs, so a question ID can never be guessed or collide
// with an approval ID by construction of the prefix).
func newQuestionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is catastrophic and unrecoverable:
		// an ID made of predictable bytes would let anyone
		// answer anyone else's question.
		panic(fmt.Sprintf("messaging: newQuestionID: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// register adds a pending question. It is called by Ask before
// the question is sent, so a fast tap can resolve it.
func (r *QuestionRegistry) register(p *pendingQuestion) {
	if r == nil || p == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]*pendingQuestion{}
	}
	r.m[p.id] = p
}

// lookup returns the pending question with id, for the callback
// handler to validate the answerer before resolving.
func (r *QuestionRegistry) lookup(id string) (*pendingQuestion, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.m[id]
	return p, ok
}

// msgID returns the sent question message id for p (0 until Ask
// records it after the send returns). Read under the registry
// mutex: Ask writes it after the send, while a fast typed reply
// can read it from the poll loop at the same time.
func (r *QuestionRegistry) msgID(p *pendingQuestion) int64 {
	if r == nil || p == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return p.msgID
}

// any. A chat holds at most one at a time (turns serialise per
// chat), but the oldest wins if several somehow exist — it is the
// one the agent is blocked waiting on.
func (r *QuestionRegistry) PendingForChat(chatID int64) (*pendingQuestion, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var oldest *pendingQuestion
	for _, p := range r.m {
		if p.chatID != chatID {
			continue
		}
		if oldest == nil {
			oldest = p
		}
	}
	return oldest, oldest != nil
}

// PendingCount reports how many questions are currently waiting.
func (r *QuestionRegistry) PendingCount() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.m)
}

// canAnswer reports whether senderID may answer p: the requester
// or the owner. Anyone else must neither answer nor disturb the
// pending question.
func (r *QuestionRegistry) canAnswer(p *pendingQuestion, senderID int64) bool {
	if r == nil || p == nil {
		return false
	}
	return senderID == p.senderID || senderID == r.ownerID
}

// settle is the single state transition for a pending question
// (the gate's settle pattern): Resolve, the timer branch and the
// cancellation branch all settle through here, under the registry
// mutex, so exactly one of them wins. It reports whether this call
// won. Settling removes the question from pending, stops its timer,
// and — for a Resolve — records the outcome before the caller
// delivers it on the channel.
func (r *QuestionRegistry) settle(p *pendingQuestion, res *questionResult) bool {
	if r == nil || p == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.settled {
		return false
	}
	p.settled = true
	if p.timer != nil {
		p.timer.Stop()
	}
	delete(r.m, p.id)
	if res != nil {
		p.resolved = res
	}
	return true
}

// resolvedOutcome returns the outcome Resolve recorded on p, if
// any. Resolve sets it under the registry mutex before delivering
// to the channel, so this read is ordered against it: a non-nil
// result means the user answered before the timer/cancel branch
// ran, even if the select picked the timer case first.
func (r *QuestionRegistry) resolvedOutcome(p *pendingQuestion) (questionResult, bool) {
	if r == nil || p == nil {
		return questionResult{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.resolved == nil {
		return questionResult{}, false
	}
	return *p.resolved, true
}

// Resolve answers the pending question with id, delivering res to
// the waiting Ask. It reports whether the resolve won: false when
// the question is unknown or already answered/expired (a late tap
// must be told "expired", never silently dropped).
func (r *QuestionRegistry) Resolve(id string, res questionResult) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	p, ok := r.m[id]
	r.mu.Unlock()
	if !ok {
		return false
	}
	if !r.settle(p, &res) {
		return false
	}
	select {
	case p.ch <- res:
	default:
	}
	return true
}

// ParseQuestionCallback parses callback data from a question
// option button. It returns the question ID and the tapped option
// index (0-based). ok is false for any other callback data (other
// buttons, other features).
func ParseQuestionCallback(data string) (questionID string, optionIndex int, ok bool) {
	if !strings.HasPrefix(data, questionCallbackPrefix) {
		return "", 0, false
	}
	rest := strings.TrimPrefix(data, questionCallbackPrefix)
	id, idxStr, found := strings.Cut(rest, ":")
	if !found || id == "" || idxStr == "" {
		return "", 0, false
	}
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 {
		return "", 0, false
	}
	return id, idx, true
}

// QuestionKeyboard builds the inline keyboard for a question: one
// button per option, one button per row so labels stay readable.
// The callback data carries only the option index (see
// questionCallbackPrefix).
func QuestionKeyboard(questionID string, options []string) InlineKeyboardMarkup {
	rows := make([][]InlineKeyboardButton, 0, len(options))
	for i, opt := range options {
		rows = append(rows, []InlineKeyboardButton{{
			Text:         opt,
			CallbackData: questionCallbackPrefix + questionID + ":" + strconv.Itoa(i),
		}})
	}
	return InlineKeyboardMarkup{InlineKeyboard: rows}
}

// TelegramQuestionAsker is the Telegram transport for
// ask_user_question: it sends the question through gw and blocks
// until the user answers. Its Ask method has the signature of
// tools.QuestionAskFunc, so the daemon wires it with a method
// value (registry.SetQuestionAsker(asker.Ask)) without either
// package importing the other for the type.
type TelegramQuestionAsker struct {
	gw        *TelegramGateway
	questions *QuestionRegistry
	ownerID   int64
	logPrefix string
}

// NewTelegramQuestionAsker returns an asker sending through gw and
// tracking pending questions in questions. A nil gateway or
// registry makes Ask fail loudly instead of hanging.
func NewTelegramQuestionAsker(gw *TelegramGateway, questions *QuestionRegistry, ownerID int64, logPrefix string) *TelegramQuestionAsker {
	return &TelegramQuestionAsker{gw: gw, questions: questions, ownerID: ownerID, logPrefix: logPrefix}
}

// formatQuestionTimeout renders a wait for the user ("5 minutes",
// "30 seconds"): Telegram shows the limit so the user knows the
// question expires.
func formatQuestionTimeout(d time.Duration) string {
	if d >= time.Minute {
		m := int(d.Minutes())
		if m == 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", m)
	}
	s := int(d.Seconds())
	if s == 1 {
		return "1 second"
	}
	return fmt.Sprintf("%d seconds", s)
}

// Ask sends question to chatID and blocks until the user answers
// (option tap or typed reply), the timeout expires, or ctx is
// cancelled. It returns the answer, the source ("button" or
// "text") and the tapped option index (-1 for a typed reply). On
// timeout it returns an error wrapping context.DeadlineExceeded;
// on cancellation it returns ctx.Err().
func (a *TelegramQuestionAsker) Ask(ctx context.Context, chatID, senderID int64, question string, options []string, timeout time.Duration) (answer, source string, optionIndex int, err error) {
	if a == nil || a.gw == nil || a.questions == nil {
		return "", "", -1, fmt.Errorf("messaging: ask_user_question: no Telegram question transport is configured")
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	id := newQuestionID()
	p := &pendingQuestion{
		id:       id,
		chatID:   chatID,
		senderID: senderID,
		question: question,
		options:  append([]string(nil), options...),
		ch:       make(chan questionResult, 1),
	}
	a.questions.register(p)

	// Send first, time second (the gate's order): the window is
	// the time to answer after the question was delivered, not
	// after Ask was called.
	text := "❓ " + question + "\n\n"
	if len(options) > 0 {
		text += "Tap an option below, or just reply with your answer."
	} else {
		text += "Reply with your answer."
	}
	text += " (You have " + formatQuestionTimeout(timeout) + ".)"
	var msgID int64
	if len(options) > 0 {
		msgID, err = a.gw.SendMessageWithKeyboard(ctx, chatID, text, QuestionKeyboard(id, options))
	} else {
		msgID, err = a.gw.SendMessageReturningID(ctx, chatID, text)
	}
	if err != nil {
		a.questions.settle(p, nil)
		return "", "", -1, fmt.Errorf("messaging: ask_user_question: could not send the question: %w", err)
	}
	a.questions.mu.Lock()
	p.msgID = msgID
	a.questions.mu.Unlock()
	fmt.Fprintf(os.Stderr, "%s: question %s chat %d card %d (timeout %s)\n", a.logPrefix, id, chatID, msgID, timeout)

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	a.questions.mu.Lock()
	p.timer = timer
	a.questions.mu.Unlock()
	select {
	case res := <-p.ch:
		return res.answer, res.source, res.optionIndex, nil
	case <-timer.C:
		// Timer vs Resolve, decided by construction (the gate's
		// settle pattern): exactly one wins. If this branch
		// wins, the expiry edit fires exactly once. If Resolve
		// won, its outcome is recorded and honoured here
		// instead of expiring the question.
		if a.questions.settle(p, nil) {
			go a.expireEdit(p)
			return "", "", -1, fmt.Errorf("messaging: ask_user_question: no answer within %s: %w", timeout, context.DeadlineExceeded)
		}
		if res, ok := a.questions.resolvedOutcome(p); ok {
			return res.answer, res.source, res.optionIndex, nil
		}
		return "", "", -1, fmt.Errorf("messaging: ask_user_question: no answer within %s: %w", timeout, context.DeadlineExceeded)
	case <-ctx.Done():
		// Same single-winner settlement as the timer branch: a
		// tap landing as the turn is cancelled wins over the
		// cancellation when Resolve already settled it.
		if a.questions.settle(p, nil) {
			return "", "", -1, ctx.Err()
		}
		if res, ok := a.questions.resolvedOutcome(p); ok {
			return res.answer, res.source, res.optionIndex, nil
		}
		return "", "", -1, ctx.Err()
	}
}

// expireEdit edits a timed-out question in place — text replaced
// and keyboard REMOVED, so nobody can tap a dead button. It runs
// in a goroutine (the caller must return promptly) with a
// background context — the turn's context may already be
// cancelled, which is exactly why the question expired.
// Best-effort: a failed edit is logged, never fatal.
func (a *TelegramQuestionAsker) expireEdit(p *pendingQuestion) {
	if a == nil || a.gw == nil || p == nil {
		return
	}
	msgID := a.questions.msgID(p)
	if msgID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.gw.EditMessageTextMarkup(ctx, p.chatID, msgID, "❓ Question: ⏰ Expired — no answer within the time limit.", InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{}}); err != nil {
		fmt.Fprintf(os.Stderr, "%s: question %s card %d expiry edit failed: %v\n", a.logPrefix, p.id, msgID, err)
	}
}

// HandleQuestionCallback resolves one question option tap: the
// answer comes from the button's callback data
// (ask:<questionID>:<optionIndex>); the pending question is looked
// up in the registry, and only the user whose turn asked the
// question — or the owner — may answer it. A tap from anyone else
// is refused without touching the pending question, so a stranger
// can neither answer nor disturb someone else's question. The
// question message is edited in place to show the answer, and the
// query is answered to clear the button spinner. A tap on an
// expired question is told so. Every tap is logged: a tap that
// never arrives and a tap on a dead card must be distinguishable
// in the log.
func HandleQuestionCallback(ctx context.Context, gw *TelegramGateway, questions *QuestionRegistry, q TGCallbackQuery, logPrefix string) error {
	stderr := os.Stderr
	id, index, ok := ParseQuestionCallback(q.Data)
	if !ok {
		// The dispatcher only calls this for ask: data, so this
		// is unreachable in production — but a direct caller
		// must still get the same "unknown button" answer as
		// the approval handler gives for unparseable data.
		fmt.Fprintf(stderr, "%s: question tap: unparseable callback data %q (query %s)\n", logPrefix, q.Data, q.ID)
		return gw.AnswerCallbackQuery(ctx, q.ID, "Unknown button.")
	}
	if questions == nil {
		return gw.AnswerCallbackQuery(ctx, q.ID, "This question has expired.")
	}
	senderID := int64(0)
	if q.From != nil {
		senderID = q.From.ID
	}
	fmt.Fprintf(stderr, "%s: question tap: %s option %d from %d\n", logPrefix, id, index, senderID)
	p, found := questions.lookup(id)
	if !found {
		fmt.Fprintf(stderr, "%s: question tap: question %s expired (no longer pending)\n", logPrefix, id)
		return gw.AnswerCallbackQuery(ctx, q.ID, "This question has expired.")
	}
	if !questions.canAnswer(p, senderID) {
		fmt.Fprintf(stderr, "%s: question tap: question %s refused for sender %d (asker %d, owner %d)\n", logPrefix, id, senderID, p.senderID, questions.ownerID)
		return gw.AnswerCallbackQuery(ctx, q.ID, "Only the person who was asked can answer this.")
	}
	if index < 0 || index >= len(p.options) {
		fmt.Fprintf(stderr, "%s: question tap: question %s option %d out of range (%d options)\n", logPrefix, id, index, len(p.options))
		return gw.AnswerCallbackQuery(ctx, q.ID, "Unknown button.")
	}
	answer := p.options[index]
	if !questions.Resolve(id, questionResult{answer: answer, source: "button", optionIndex: index}) {
		fmt.Fprintf(stderr, "%s: question tap: question %s expired during resolve\n", logPrefix, id)
		return gw.AnswerCallbackQuery(ctx, q.ID, "This question has expired.")
	}
	fmt.Fprintf(stderr, "%s: question tap: question %s answered with option %d\n", logPrefix, id, index)
	// Edit the question in place to show the answer. The answer
	// is model-supplied text and EditMessageTextMarkup sends
	// HTML, so it is escaped first — echoing it raw could break
	// the markup (the same reason the approval outcome edit
	// never echoes the request summary).
	if q.Message != nil {
		text := "❓ Question: ✅ " + escapeHTML(answer)
		if err := gw.EditMessageTextMarkup(ctx, q.Message.Chat.ID, q.Message.MessageID, text, InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{}}); err != nil {
			fmt.Fprintf(stderr, "%s: question tap: question %s outcome edit failed: %v\n", logPrefix, id, err)
		}
	}
	return gw.AnswerCallbackQuery(ctx, q.ID, truncateRunes(answer, 200))
}

// tryAnswerQuestion consumes a text message as the free-text
// answer to a pending question in its chat, if there is one. It
// is called from PollStream's message handler BEFORE the per-chat
// lock: the turn that asked is blocked waiting for the answer
// and holds that lock, so routing the reply through a turn would
// deadlock until the question timed out. It reports whether the
// message was consumed (the caller then returns without starting
// a turn). Commands ("/...") are never answers — they flow to
// the normal turn path — and only the asker or the owner may
// answer, so another chat member's message is never consumed.
func (g *TelegramGateway) tryAnswerQuestion(upd Update) bool {
	if g == nil || g.questions == nil || upd.Message == nil {
		return false
	}
	text := strings.TrimSpace(upd.Message.Text)
	if text == "" || strings.HasPrefix(text, "/") {
		return false
	}
	senderID := int64(0)
	if upd.Message.From != nil {
		senderID = upd.Message.From.ID
	}
	p, ok := g.questions.PendingForChat(upd.Message.Chat.ID)
	if !ok {
		return false
	}
	if !g.questions.canAnswer(p, senderID) {
		return false
	}
	if !g.questions.Resolve(p.id, questionResult{answer: text, source: "text", optionIndex: -1}) {
		return false
	}
	fmt.Fprintf(os.Stderr, "question: chat %d question %s answered by typed reply from %d\n", upd.Message.Chat.ID, p.id, senderID)
	// Edit the question in place to show it was answered and
	// remove the keyboard, so nobody taps a dead button. The
	// answer is user text and the edit sends HTML, so it is
	// escaped and truncated first. Best-effort: a failed edit
	// never fails the answer.
	//
	// The message id is recorded by Ask after the send returns,
	// and a fast reply can land before that write: the user has
	// necessarily seen the question (the send completed
	// server-side), so wait briefly for Ask to record the id
	// instead of silently skipping the edit. The wait is bounded
	// (1s) and only ever runs when the id is not yet recorded —
	// in production a human reply always arrives long after.
	msgID := g.questions.msgID(p)
	for i := 0; i < 50 && msgID == 0; i++ {
		time.Sleep(20 * time.Millisecond)
		msgID = g.questions.msgID(p)
	}
	if msgID != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		text := "❓ Question: ✅ " + escapeHTML(truncateRunes(text, 200))
		_ = g.EditMessageTextMarkup(ctx, p.chatID, msgID, text, InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{}})
	}
	return true
}
