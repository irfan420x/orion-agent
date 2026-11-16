// Busy-input acknowledgement: what the bot does when a new message
// arrives while a turn is already running for the same chat.
//
// A second message from a chat whose turn is still running used to
// block silently on the per-chat lock until the first turn ended —
// the user saw nothing at all until the first reply landed. Now the
// gateway acknowledges the arrival at once and acts on the chat's
// busy mode (default interrupt, changeable with /busy):
//
//   - interrupt: cancel the running turn and run the new message
//     instead (degraded to queue while a plan decision is pending —
//     cancelling a turn that is waiting on a plan approval would
//     orphan the plan card, so the message queues behind it);
//   - queue: acknowledge and run the message as the next turn, in
//     arrival order behind the per-chat lock;
//   - steer: inject the message into the running turn (the agent
//     loop picks it up at the next step boundary); when the run
//     cannot take it, the message runs as a normal turn instead.
//
// Commands (text starting with "/") never trigger busy handling:
// they flow to the lock wait as before, so /stop and /busy sent
// mid-turn are not mistaken for new input.
//
// One ack per running turn: later arrivals while the same run is
// busy still get their mode action, but no second ack — a busy
// user hammering messages must not stack acks (Telegram
// rate-limits, ~1 message/sec per chat). The ack is best-effort: a
// failed send is logged, never fails the turn.
package messaging

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/irfan/orion/internal/core"
)

// BusyMode is how a chat's new messages are handled while a turn
// runs for that chat.
type BusyMode int

const (
	// BusyModeInterrupt redirects the running turn to the new
	// message: the old turn is cancelled, the new message runs
	// instead. The default.
	BusyModeInterrupt BusyMode = iota
	// BusyModeQueue runs the new message as the next turn, after
	// the running turn finishes.
	BusyModeQueue
	// BusyModeSteer injects the new message into the running
	// turn; it joins after the next step boundary.
	BusyModeSteer
)

// ParseBusyMode parses a mode name (case-insensitive, trimmed).
// "" means the default (interrupt). Anything else is an error
// naming the valid modes.
func ParseBusyMode(s string) (BusyMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "interrupt":
		return BusyModeInterrupt, nil
	case "queue":
		return BusyModeQueue, nil
	case "steer":
		return BusyModeSteer, nil
	default:
		return BusyModeInterrupt, fmt.Errorf("unknown busy mode %q (want interrupt, queue, or steer)", strings.TrimSpace(s))
	}
}

// String renders the mode name (the /busy status text and the
// persisted session value use this).
func (m BusyMode) String() string {
	switch m {
	case BusyModeQueue:
		return "queue"
	case BusyModeSteer:
		return "steer"
	default:
		return "interrupt"
	}
}

// formatBusyElapsed renders a run's age for the ack detail: "1m 5s"
// past a minute, "5s" below it.
func formatBusyElapsed(d time.Duration) string {
	if d >= time.Minute {
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

// busyState is one running turn's busy-input state: when it started,
// what it is doing, whether its ack was sent, and how to cancel it.
// It is registered when the turn starts (after the per-chat lock)
// and unregistered when the turn's handler finishes.
type busyState struct {
	mu sync.Mutex
	// startedAt is when the turn registered (just before the
	// handler ran), for the ack's elapsed detail.
	startedAt time.Time
	// tool is the tool the turn is currently running ("" when
	// between tools), step is the latest provider step number.
	tool string
	step int
	// ackSent is set once the single ack for this turn has gone
	// out; later arrivals get their mode action but no second ack.
	ackSent bool
	// interrupted is set when an arrival cancelled this run.
	interrupted bool
	// ctx is the context the running turn (main or leftover)
	// runs on; cancel cancels it. Both are retargeted together
	// by setCancel when a leftover turn starts (F-A23W3-12), so
	// cancelRun always cancels the turn that is actually running
	// — never a finished main turn while its leftover runs on.
	// ctx is also how cancelRun knows whether there is anything
	// live left to cancel: a Done ctx means the run already
	// finished (or was already cancelled).
	ctx context.Context
	// cancel cancels the turn's context (the handler's ctx).
	cancel context.CancelFunc
}

// noteEvent updates the tool/step detail from a progress event of
// the running turn. Only the kinds the terminal card cares about
// change anything (step starts, tool starts/finishes); the rest
// are ignored.
func (s *busyState) noteEvent(ev core.ProgressEvent) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch ev.Kind {
	case core.EventStepStarted:
		if ev.Step > s.step {
			s.step = ev.Step
		}
	case core.EventToolCallStarted:
		s.tool = ev.ToolName
		if ev.Step > s.step {
			s.step = ev.Step
		}
	case core.EventToolCallFinished:
		if ev.ToolName != "" && ev.ToolName == s.tool {
			s.tool = ""
		}
	}
}

// detail renders the parenthesised live detail for the ack, e.g.
// " (1m 5s in, step 3, running shell_exec)". Elapsed is always
// present; step and tool are omitted when unknown.
func (s *busyState) detail() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.detailLocked()
}

// detailLocked is detail with the mutex already held.
func (s *busyState) detailLocked() string {
	parts := []string{formatBusyElapsed(time.Since(s.startedAt)) + " in"}
	if s.step > 0 {
		parts = append(parts, fmt.Sprintf("step %d", s.step))
	}
	if s.tool != "" {
		parts = append(parts, "running "+s.tool)
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// cancelRun marks the run interrupted and cancels its context.
// It reports whether it actually cancelled a live turn
// (F-A23W3-12): false when there is nothing live left to cancel —
// the cancel func is nil, or the turn's ctx is already Done
// (the run finished, or a previous interrupt already cancelled
// it). In that case interrupted is NOT set: no run was
// interrupted. Cancelling twice is safe (context.CancelFunc is
// idempotent), but only the first cancel of a live run counts.
func (s *busyState) cancelRun() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	if s.cancel == nil || (s.ctx != nil && s.ctx.Err() != nil) {
		s.mu.Unlock()
		return false
	}
	s.interrupted = true
	cancel := s.cancel
	s.mu.Unlock()
	cancel()
	return true
}

// setCancel retargets the state at the currently-running turn
// (F-A23W3-12): a leftover turn runs on its own cancellable ctx,
// so the busy state must point at that ctx (not the finished
// main turn's) for as long as the leftover runs. The caller owns
// the new cancel func's lifetime and calls it when the turn ends.
func (s *busyState) setCancel(ctx context.Context, cancel context.CancelFunc) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctx = ctx
	s.cancel = cancel
}

// composeBusyAck builds the ack text for a mode. degraded is the
// interrupt-degraded-to-queue case (a plan decision is pending, so
// the run cannot be interrupted). The wording is Orion's own —
// not copied from any other agent. detail is the live-detail
// suffix from busyState.detail ("" when unavailable).
func composeBusyAck(mode BusyMode, degraded bool, detail string) string {
	switch {
	case degraded:
		return "⏳ Queued — the current run is waiting on a plan decision, so I'll get to this after it resolves." + detail
	case mode == BusyModeQueue:
		return "⏳ Queued — I'll get to this after the current task finishes." + detail
	case mode == BusyModeSteer:
		return "⏩ Steered into the current run — your message joins it after the next step." + detail
	default:
		return "↪ Redirected the current run — I'll adjust to your new message." + detail
	}
}

// isBusyExemptCommand reports whether text is a slash command:
// commands never trigger busy handling — they flow to the lock
// wait as before, so /stop and /busy sent mid-turn are not
// mistaken for new input.
func isBusyExemptCommand(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), "/")
}

// SetBusyModeFor installs the hook the gateway calls to read a
// chat's busy mode. A nil hook means the default (interrupt) for
// every chat. Set before PollStream starts; it must not race
// polling. The daemon wires it to the session store.
func (g *TelegramGateway) SetBusyModeFor(fn func(chatID int64) BusyMode) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.busyModeHook = fn
}

// SetPlanPendingFor installs the hook the gateway calls to ask
// whether a plan decision is pending for a chat (an interrupt
// degrades to queue while one is). A nil hook means no plan is
// ever pending. Set before PollStream starts; it must not race
// polling. The daemon wires it to the plan-mode tool's
// PlanModeBlocks probe.
func (g *TelegramGateway) SetPlanPendingFor(fn func(chatID int64) bool) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.planPendingHook = fn
}

// SetSteerFor installs the hook the gateway calls to steer a
// message into a chat's running turn. It reports whether the run
// accepted the message; a nil hook never accepts (the message
// then runs as a normal turn). Set before PollStream starts; it
// must not race polling. The daemon wires it to App.SteerTurn.
func (g *TelegramGateway) SetSteerFor(fn func(chatID int64, text string, updateID int64) (accepted bool)) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.steerHook = fn
}

// SetSteerLeftoversFor installs the hook the gateway calls after a
// turn returns, to collect steered messages the run did not
// consume (they arrived during the final provider call, or after
// the step budget ran out). A nil hook means none. Set before
// PollStream starts; it must not race polling. The daemon wires
// it to App.DrainSteeredLeftovers.
func (g *TelegramGateway) SetSteerLeftoversFor(fn func(chatID int64) []core.SteeredMessage) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.steerLeftoversHook = fn
}

// busyModeFor reads a chat's busy mode through the installed hook
// (default interrupt when none is installed).
func (g *TelegramGateway) busyModeFor(chatID int64) BusyMode {
	if g == nil {
		return BusyModeInterrupt
	}
	g.mu.Lock()
	fn := g.busyModeHook
	g.mu.Unlock()
	if fn == nil {
		return BusyModeInterrupt
	}
	return fn(chatID)
}

// planPendingFor reports whether a plan decision is pending for a
// chat through the installed hook (false when none is installed).
func (g *TelegramGateway) planPendingFor(chatID int64) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	fn := g.planPendingHook
	g.mu.Unlock()
	if fn == nil {
		return false
	}
	return fn(chatID)
}

// steerFor offers a message to a chat's running turn through the
// installed hook (false when none is installed).
func (g *TelegramGateway) steerFor(chatID int64, text string, updateID int64) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	fn := g.steerHook
	g.mu.Unlock()
	if fn == nil {
		return false
	}
	return fn(chatID, text, updateID)
}

// steerLeftoversFor collects a chat's unconsumed steered messages
// through the installed hook (nil when none is installed).
func (g *TelegramGateway) steerLeftoversFor(chatID int64) []core.SteeredMessage {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	fn := g.steerLeftoversHook
	g.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(chatID)
}

// dispatchStamp is what the poll loop records for an update at
// dispatch time (see stampDispatch): the busy state the update
// arrived during (nil when the chat was idle), and the FIFO ticket
// that orders this update's turn behind earlier updates from the
// same chat.
type dispatchStamp struct {
	busy   *busyState
	ticket int64
}

// ticketQueue is one chat's FIFO ticket queue for text turns: it
// makes queued messages run in dispatch (arrival) order, which the
// per-chat lock alone cannot guarantee — two handles spawned in the
// same batch race to the lock in goroutine-scheduling order, and
// the later message can win it (observed in
// TestBusyAckCoalescingAndQueueOrdering: [first third second]).
// A ticket is taken at dispatch (in batch order) and released when
// the turn's handler finishes — including on early returns (a
// consumed reply-chain answer, a steered message) and on panic,
// via defer — so the queue can never stall behind a handle that
// left without running a turn.
type ticketQueue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	next    int64 // next ticket to hand out
	serving int64 // ticket currently being served
}

func newTicketQueue() *ticketQueue {
	q := &ticketQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// take hands out the next ticket, in dispatch order.
func (q *ticketQueue) take() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	t := q.next
	q.next++
	return t
}

// wait blocks until ticket t is the one being served.
func (q *ticketQueue) wait(t int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.serving != t {
		q.cond.Wait()
	}
}

// release advances the queue past ticket t (waiting for it first,
// so an early return behind a still-running turn still releases in
// order) and wakes the next waiter.
func (q *ticketQueue) release(t int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.serving != t {
		q.cond.Wait()
	}
	q.serving++
	q.cond.Broadcast()
}

// ticketQueueFor returns the FIFO ticket queue for a chat, creating
// it on first use. One queue per chat: different chats never wait
// on each other.
func (g *TelegramGateway) ticketQueueFor(chatID int64) *ticketQueue {
	g.ticketMu.Lock()
	defer g.ticketMu.Unlock()
	if g.tickets == nil {
		g.tickets = map[int64]*ticketQueue{}
	}
	q, ok := g.tickets[chatID]
	if !ok {
		q = newTicketQueue()
		g.tickets[chatID] = q
	}
	return q
}

// stampDispatch snapshots, at spawn time, the busy state an
// update arrived during (nil when no turn is running for the
// chat) and hands out the FIFO ticket that orders this update's
// turn behind earlier updates from the same chat. Spawn time is
// the moment just before the handler goroutine starts, taken in
// the sequential dispatch loop, so stamps stay in batch order.
// The handler reads the stamp instead of looking the state up
// live: a handler goroutine can start late — after the run it
// arrived during ended and the next turn registered — and a live
// lookup would then ack against the fresh state. A nil busy state
// means the update arrived while the chat was idle (or in the
// microsecond teardown window between one turn unregistering and
// the next registering) and gets no busy handling.
func (g *TelegramGateway) stampDispatch(updateID, chatID int64) dispatchStamp {
	if g == nil {
		return dispatchStamp{}
	}
	st, _ := g.busyFor(chatID)
	ticket := g.ticketQueueFor(chatID).take()
	s := dispatchStamp{busy: st, ticket: ticket}
	g.stampMu.Lock()
	defer g.stampMu.Unlock()
	if g.busyStamps == nil {
		g.busyStamps = map[int64]dispatchStamp{}
	}
	g.busyStamps[updateID] = s
	return s
}

// dispatchStampFor returns the dispatch-time stamp for an update
// (if any) and deletes it.
func (g *TelegramGateway) dispatchStampFor(updateID int64) (dispatchStamp, bool) {
	if g == nil {
		return dispatchStamp{}, false
	}
	g.stampMu.Lock()
	defer g.stampMu.Unlock()
	s, ok := g.busyStamps[updateID]
	if ok {
		delete(g.busyStamps, updateID)
	}
	return s, ok
}

// busyFor returns the running turn's busy state for a chat, if any.
func (g *TelegramGateway) busyFor(chatID int64) (*busyState, bool) {
	if g == nil {
		return nil, false
	}
	g.busyMu.Lock()
	defer g.busyMu.Unlock()
	st, ok := g.busy[chatID]
	return st, ok
}

// registerBusy records a newly started turn's busy state for a
// chat. Called after the per-chat lock is held, just before the
// handler runs — registering earlier would let a later arrival
// cancel a turn that has not started yet, losing that message's
// turn with no reply.
func (g *TelegramGateway) registerBusy(chatID int64, ctx context.Context, cancel context.CancelFunc) *busyState {
	st := &busyState{startedAt: time.Now(), ctx: ctx, cancel: cancel}
	g.busyMu.Lock()
	defer g.busyMu.Unlock()
	if g.busy == nil {
		g.busy = map[int64]*busyState{}
	}
	g.busy[chatID] = st
	return st
}

// unregisterBusy removes a finished turn's busy state, but only
// when it is still the registered one — a newer turn for the chat
// must never lose its state to an older turn's teardown.
func (g *TelegramGateway) unregisterBusy(chatID int64, st *busyState) {
	if g == nil || st == nil {
		return
	}
	g.busyMu.Lock()
	defer g.busyMu.Unlock()
	if cur, ok := g.busy[chatID]; ok && cur == st {
		delete(g.busy, chatID)
	}
}

// sendBusyAck sends the single acknowledgement for the running
// turn described by st, unless one was already sent for this turn
// (coalesced). degraded is the interrupt-degraded-to-queue case.
// Best-effort, on the PollStream ctx (never the turn ctx, which
// an interrupt may have just cancelled): a failed send is logged,
// never fails the turn.
func (g *TelegramGateway) sendBusyAck(ctx context.Context, chatID int64, st *busyState, mode BusyMode, degraded bool) {
	if g == nil || st == nil {
		return
	}
	st.mu.Lock()
	if st.ackSent {
		st.mu.Unlock()
		return
	}
	st.ackSent = true
	text := composeBusyAck(mode, degraded, st.detailLocked())
	st.mu.Unlock()
	if err := g.SendMessage(ctx, chatID, text); err != nil {
		g.logf("telegram poll: busy ack to chat %d failed: %v", chatID, err)
	}
}

// cancelQuestionForChat cancels the pending ask_user_question for
// a chat (if any) with a visible card edit, so an interrupted run
// does not leave a live-looking question behind. It reports
// whether a question was actually cancelled.
func (g *TelegramGateway) cancelQuestionForChat(chatID int64) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	q := g.questions
	g.mu.Unlock()
	if q == nil {
		return false
	}
	return q.CancelForChat(chatID)
}
