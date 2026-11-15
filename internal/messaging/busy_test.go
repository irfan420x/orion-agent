// Busy-input acknowledgement tests (internal/messaging): the
// gateway's mid-turn ack (interrupt / queue / steer), its
// coalescing, the question-cancel edit, the plan-pending degrade,
// the steer accept/decline paths, the command exemption, and the
// leftover-turn drain. The harness is a live PollStream over a
// queueFake, modelled on the question harness in question_test.go.
package messaging

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/core"
)

// steerCall is one SteerFor hook call the harness recorded.
type steerCall struct {
	chatID   int64
	text     string
	updateID int64
}

// busyHarness is a live PollStream over a queueFake with the
// busy-input hooks installed. The mode, plan-pending flag, steer
// acceptance and leftover list are fields tests set before
// enqueueing updates; the turn handler is a dispatch field, like
// the question harness's.
type busyHarness struct {
	t         *testing.T
	fake      *queueFake
	gw        *TelegramGateway
	questions *QuestionRegistry
	asker     *TelegramQuestionAsker
	mu        sync.Mutex
	handler   TurnFunc
	turns     []turnCall
	turnIDs   []int64
	cancel    context.CancelFunc
	errCh     chan error

	mode        BusyMode
	planPending bool
	steerAccept bool
	steerCalls  []steerCall
	leftovers   []core.SteeredMessage
}

func newBusyHarness(t *testing.T) *busyHarness {
	t.Helper()
	fake := newQueueFake()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	gw, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithAllowedChats(77))
	if err != nil {
		t.Fatal(err)
	}
	h := &busyHarness{t: t, fake: fake, gw: gw, mode: BusyModeInterrupt}
	h.questions = NewQuestionRegistry(77)
	gw.SetQuestionRegistry(h.questions)
	h.asker = NewTelegramQuestionAsker(gw, h.questions, 77, "test")
	gw.SetBusyModeFor(func(chatID int64) BusyMode {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.mode
	})
	gw.SetPlanPendingFor(func(chatID int64) bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.planPending
	})
	gw.SetSteerFor(func(chatID int64, text string, updateID int64) bool {
		h.mu.Lock()
		h.steerCalls = append(h.steerCalls, steerCall{chatID, text, updateID})
		accept := h.steerAccept
		h.mu.Unlock()
		return accept
	})
	gw.SetSteerLeftoversFor(func(chatID int64) []core.SteeredMessage {
		h.mu.Lock()
		defer h.mu.Unlock()
		out := h.leftovers
		h.leftovers = nil
		return out
	})
	h.handler = h.recordingTurn
	dispatch := func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		h.mu.Lock()
		fn := h.handler
		h.mu.Unlock()
		return fn(ctx, chatID, text, emit)
	}
	h.cancel, h.errCh = runPollStream(t, gw, dispatch)
	t.Cleanup(func() { stopPollStream(t, h.cancel, h.errCh) })
	return h
}

func (h *busyHarness) recordingTurn(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
	h.noteTurn(ctx, chatID, text)
	return "reply-" + text, nil, nil
}

// noteTurn records one turn call (text + the update id its ctx
// carries, for the leftover-turn identity check).
func (h *busyHarness) noteTurn(ctx context.Context, chatID int64, text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.turns = append(h.turns, turnCall{chatID, text})
	h.turnIDs = append(h.turnIDs, core.UpdateIDFromContext(ctx))
}

// setHandler replaces the dispatch handler.
func (h *busyHarness) setHandler(fn TurnFunc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handler = fn
}

func (h *busyHarness) setMode(m BusyMode) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mode = m
}

func (h *busyHarness) setPlanPending(b bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.planPending = b
}

func (h *busyHarness) setSteerAccept(b bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.steerAccept = b
}

func (h *busyHarness) setLeftovers(l []core.SteeredMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.leftovers = l
}

// turnsSnapshot returns the turn calls recorded so far.
func (h *busyHarness) turnsSnapshot() []turnCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]turnCall(nil), h.turns...)
}

// turnIDsSnapshot returns the update ids the recorded turns ran with.
func (h *busyHarness) turnIDsSnapshot() []int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int64(nil), h.turnIDs...)
}

// steerCallsSnapshot returns the SteerFor calls recorded so far.
func (h *busyHarness) steerCallsSnapshot() []steerCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]steerCall(nil), h.steerCalls...)
}

// sentTexts returns every sendMessage text recorded so far, in order.
func (h *busyHarness) sentTexts() []string {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	out := make([]string, 0, len(h.fake.sent))
	for _, s := range h.fake.sent {
		out = append(out, s.Text)
	}
	return out
}

// sentContaining reports whether any sent text contains substr.
func (h *busyHarness) sentContaining(substr string) bool {
	for _, s := range h.sentTexts() {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

// countSentContaining counts sent texts containing substr.
func (h *busyHarness) countSentContaining(substr string) int {
	n := 0
	for _, s := range h.sentTexts() {
		if strings.Contains(s, substr) {
			n++
		}
	}
	return n
}

// editFor returns the edit recorded for message msgID, if any.
func (h *busyHarness) editFor(msgID int64) (queueEdit, bool) {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	for _, e := range h.fake.edits {
		if e.MsgID == msgID {
			return e, true
		}
	}
	return queueEdit{}, false
}

// waitTurns waits until at least n turns have been recorded.
func (h *busyHarness) waitTurns(n int) {
	h.t.Helper()
	waitFor(h.t, "turns", func() bool { return len(h.turnsSnapshot()) >= n })
}

// waitSent waits until a sent text contains substr.
func (h *busyHarness) waitSent(substr string) {
	h.t.Helper()
	waitFor(h.t, "sent text containing "+substr, func() bool { return h.sentContaining(substr) })
}

// waitOffset waits until the gateway's offset advances past id
// (all updates up to id decided exactly once).
func (h *busyHarness) waitOffset(want int64) {
	h.t.Helper()
	waitFor(h.t, "offset", func() bool { return h.gw.Offset() >= want })
}

// busyMsg builds a text-message update for the harness chat.
func busyMsg(id int64, text string) Update {
	return Update{UpdateID: id, Message: &TGMessage{
		MessageID: id, Chat: TGChat{ID: 77}, From: &TGUser{ID: 77}, Text: text,
	}}
}

// firstTurnBlocker is a handler for "first" that emits a tool event
// (so the ack detail names the running tool), signals started, and
// then blocks until release or ctx cancellation. Any other text
// returns a prompt reply. The ctx.Done branch returns ("", nil,
// nil) exactly like the daemon's turn handler does for a cancelled
// turn, so the old turn sends no reply.
func firstTurnBlocker(h *busyHarness, startedCh, release, cancelled chan struct{}) TurnFunc {
	return func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		h.noteTurn(ctx, chatID, text)
		if text != "first" {
			return "reply-" + text, nil, nil
		}
		emit(started("shell_exec", mustJSONArgsForTest("command", "sleep 60")))
		select {
		case startedCh <- struct{}{}:
		default:
		}
		select {
		case <-ctx.Done():
			select {
			case cancelled <- struct{}{}:
			default:
			}
			return "", nil, nil
		case <-release:
			return "first-done", nil, nil
		}
	}
}

func TestParseBusyMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want BusyMode
	}{
		{"", BusyModeInterrupt},
		{"interrupt", BusyModeInterrupt},
		{"INTERRUPT", BusyModeInterrupt},
		{"  interrupt  ", BusyModeInterrupt},
		{"queue", BusyModeQueue},
		{"Queue", BusyModeQueue},
		{"  queue ", BusyModeQueue},
		{"steer", BusyModeSteer},
		{"Steer", BusyModeSteer},
	} {
		got, err := ParseBusyMode(tc.in)
		if err != nil {
			t.Errorf("ParseBusyMode(%q) error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseBusyMode(%q) = %v, want %v", tc.in, got, tc.want)
		}
		if got.String() != tc.want.String() {
			t.Errorf("ParseBusyMode(%q).String() = %q, want %q", tc.in, got.String(), tc.want.String())
		}
	}
	for _, bad := range []string{"bogus", "queued", "interrupted", "steering", "123"} {
		if _, err := ParseBusyMode(bad); err == nil {
			t.Errorf("ParseBusyMode(%q) = no error, want an error naming the valid modes", bad)
		}
	}
	if got := BusyModeInterrupt.String(); got != "interrupt" {
		t.Errorf("BusyModeInterrupt.String() = %q, want interrupt", got)
	}
}

func TestComposeBusyAckModes(t *testing.T) {
	cases := []struct {
		mode     BusyMode
		degraded bool
		want     string
	}{
		{BusyModeInterrupt, false, "↪ Redirected the current run — I'll adjust to your new message."},
		{BusyModeQueue, false, "⏳ Queued — I'll get to this after the current task finishes."},
		{BusyModeSteer, false, "⏩ Steered into the current run — your message joins it after the next step."},
		{BusyModeInterrupt, true, "⏳ Queued — the current run is waiting on a plan decision, so I'll get to this after it resolves."},
	}
	for _, tc := range cases {
		if got := composeBusyAck(tc.mode, tc.degraded, ""); got != tc.want {
			t.Errorf("composeBusyAck(%v, degraded=%v) = %q, want %q", tc.mode, tc.degraded, got, tc.want)
		}
		// The detail suffix appends verbatim.
		if got := composeBusyAck(tc.mode, tc.degraded, " (5s in)"); got != tc.want+" (5s in)" {
			t.Errorf("composeBusyAck(%v, degraded=%v, detail) = %q, want %q", tc.mode, tc.degraded, got, tc.want+" (5s in)")
		}
	}
}

func TestBusyStateDetail(t *testing.T) {
	st := &busyState{startedAt: time.Now().Add(-65 * time.Second), step: 3, tool: "shell_exec"}
	if got := st.detail(); got != " (1m 5s in, step 3, running shell_exec)" {
		t.Errorf("detail() = %q, want %q", got, " (1m 5s in, step 3, running shell_exec)")
	}
	// No step and no tool: only the elapsed part survives.
	st2 := &busyState{startedAt: time.Now().Add(-5 * time.Second)}
	if got := st2.detail(); got != " (5s in)" {
		t.Errorf("detail() with no step/tool = %q, want %q", got, " (5s in)")
	}
	// noteEvent tracks step and tool, and clears the tool on finish.
	st3 := &busyState{startedAt: time.Now()}
	st3.noteEvent(core.ProgressEvent{Kind: core.EventStepStarted, Step: 2})
	st3.noteEvent(core.ProgressEvent{Kind: core.EventToolCallStarted, Step: 2, ToolName: "shell_exec"})
	if got := st3.detail(); !strings.Contains(got, "step 2") || !strings.Contains(got, "running shell_exec") {
		t.Errorf("detail() after tool start = %q, want step 2 + running shell_exec", got)
	}
	st3.noteEvent(core.ProgressEvent{Kind: core.EventToolCallFinished, Step: 2, ToolName: "shell_exec"})
	if got := st3.detail(); strings.Contains(got, "running") {
		t.Errorf("detail() after tool finish = %q, want no running tool", got)
	}
	if st3.detail() == "" {
		t.Error("detail() must never be empty (elapsed is always present)")
	}
}

func TestBusyInterruptCancelsRunningTurn(t *testing.T) {
	h := newBusyHarness(t)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	h.setHandler(firstTurnBlocker(h, started, release, cancelled))

	h.fake.enqueue(busyMsg(1, "first"))
	select {
	case <-started:
	case <-time.After(scaledTimeout(5 * time.Second)):
		t.Fatal("first turn did not start")
	}
	h.fake.enqueue(busyMsg(2, "second"))
	// The ack names the redirect and the running tool.
	h.waitSent("Redirected the current run")
	waitFor(t, "ack names the running tool", func() bool {
		for _, s := range h.sentTexts() {
			if strings.Contains(s, "Redirected") && strings.Contains(s, "running shell_exec") {
				return true
			}
		}
		return false
	})
	// The old turn's ctx is cancelled, and its handler returns
	// suppressed — it sends no reply of its own.
	select {
	case <-cancelled:
	case <-time.After(scaledTimeout(5 * time.Second)):
		t.Fatal("first turn was not cancelled by the interrupt")
	}
	// The new turn runs, and both updates are decided exactly once:
	// the watermark advances past both.
	h.waitTurns(2)
	h.waitSent("reply-second")
	h.waitOffset(3)
	if got := h.turnsSnapshot(); len(got) != 2 || got[0].text != "first" || got[1].text != "second" {
		t.Errorf("turns = %v, want [first second]", got)
	}
	for _, s := range h.sentTexts() {
		if strings.Contains(s, "first-done") {
			t.Errorf("interrupted turn sent a reply %q, want none", s)
		}
	}
	if got := h.countSentContaining("Redirected"); got != 1 {
		t.Errorf("redirect acks sent = %d, want exactly 1", got)
	}
}

func TestBusyAckCoalescingAndQueueOrdering(t *testing.T) {
	h := newBusyHarness(t)
	h.setMode(BusyModeQueue)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	h.setHandler(firstTurnBlocker(h, started, release, cancelled))

	h.fake.enqueue(busyMsg(1, "first"))
	select {
	case <-started:
	case <-time.After(scaledTimeout(5 * time.Second)):
		t.Fatal("first turn did not start")
	}
	// Two arrivals while the same run is busy: exactly one ack.
	// Both are enqueued together, so the poll loop observes them
	// in one batch while the first turn is still blocked; the
	// dispatch-time stamp pins them both to that run even if a
	// handler goroutine starts late (see stampDispatch).
	h.fake.enqueue(busyMsg(2, "second"))
	h.fake.enqueue(busyMsg(3, "third"))
	h.waitSent("Queued — I'll get to this after the current task finishes")
	// Both arrivals' busy paths have run by now (the ack proves
	// one did, the stamp pins the other to the same run): a
	// moment more must not produce a second ack.
	time.Sleep(300 * time.Millisecond)
	if got := h.countSentContaining("Queued"); got != 1 {
		t.Errorf("queued acks sent = %d, want exactly 1 (coalesced)", got)
	}
	select {
	case <-cancelled:
		t.Fatal("queue mode must not cancel the running turn")
	default:
	}
	close(release)
	// Both queued messages run after the running turn finishes, in
	// arrival order.
	h.waitTurns(3)
	h.waitOffset(4)
	if got := h.turnsSnapshot(); len(got) != 3 || got[0].text != "first" || got[1].text != "second" || got[2].text != "third" {
		t.Errorf("turns = %v, want [first second third] in order", got)
	}
	if got := h.sentTexts(); len(got) != 5 {
		t.Errorf("sent = %q, want [card queue-ack first-done reply-second reply-third]", got)
	} else {
		// got[0] is the terminal card the blocking turn's tool
		// event created (emit feeds the card); got[1] is the
		// single coalesced queue ack.
		if !strings.Contains(got[0], "terminal") || !strings.Contains(got[1], "Queued") || got[2] != "first-done" || got[3] != "reply-second" || got[4] != "reply-third" {
			t.Errorf("sent = %q, want [card queue-ack first-done reply-second reply-third]", got)
		}
	}
}

func TestBusyInterruptWithPendingPlanDegradesToQueue(t *testing.T) {
	h := newBusyHarness(t)
	h.setPlanPending(true)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	h.setHandler(firstTurnBlocker(h, started, release, cancelled))

	h.fake.enqueue(busyMsg(1, "first"))
	select {
	case <-started:
	case <-time.After(scaledTimeout(5 * time.Second)):
		t.Fatal("first turn did not start")
	}
	h.fake.enqueue(busyMsg(2, "second"))
	h.waitSent("waiting on a plan decision")
	// The run is NOT cancelled: the degraded ack went out, and the
	// first turn is still blocked (the cancel fires in the same
	// code path as the ack, so 300ms is ample to catch it).
	time.Sleep(300 * time.Millisecond)
	select {
	case <-cancelled:
		t.Fatal("interrupt with a pending plan must degrade to queue, not cancel the run")
	default:
	}
	if got := h.countSentContaining("Redirected"); got != 0 {
		t.Errorf("redirect acks sent = %d, want 0 (degraded to queue)", got)
	}
	close(release)
	h.waitTurns(2)
	h.waitSent("reply-second")
	h.waitOffset(3)
	if got := h.turnsSnapshot(); len(got) != 2 || got[1].text != "second" {
		t.Errorf("turns = %v, want [first second]", got)
	}
}

func TestBusyInterruptWithPendingQuestion(t *testing.T) {
	h := newBusyHarness(t)
	askDone := make(chan error, 1)
	h.setHandler(func(ctx context.Context, chatID int64, text string, emit func(core.ProgressEvent)) (string, *core.TurnStats, error) {
		h.noteTurn(ctx, chatID, text)
		if text != "first" {
			return "reply-" + text, nil, nil
		}
		_, _, _, err := h.asker.Ask(ctx, chatID, 77, "pick one?", []string{"yes", "no"}, 5*time.Minute)
		askDone <- err
		if err != nil {
			// Cancelled like the daemon's turn handler: no reply.
			if errors.Is(err, context.Canceled) {
				return "", nil, nil
			}
			return "", nil, err
		}
		return "answered", nil, nil
	})

	h.fake.enqueue(busyMsg(1, "first"))
	// Wait for the question card to be sent, and note its message id.
	h.waitSent("❓ pick one?")
	var qMsgID int64
	waitFor(t, "question message id", func() bool {
		h.fake.mu.Lock()
		defer h.fake.mu.Unlock()
		for _, s := range h.fake.sent {
			if strings.HasPrefix(s.Text, "❓") {
				qMsgID = s.MsgID
				return true
			}
		}
		return false
	})
	h.fake.enqueue(busyMsg(2, "something else"))
	h.waitSent("Redirected the current run")
	// The question card is edited in place to the cancelled text,
	// with the keyboard removed (empty inline_keyboard).
	waitFor(t, "question cancel edit", func() bool {
		e, ok := h.editFor(qMsgID)
		return ok && e.Text == questionCancelledText && e.Keyboard == `{"inline_keyboard":[]}`
	})
	// The Ask wakes with the cancellation, and the old turn sends
	// no reply; the new message runs as a normal turn.
	select {
	case err := <-askDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Ask error = %v, want context.Canceled", err)
		}
	case <-time.After(scaledTimeout(5 * time.Second)):
		t.Fatal("Ask did not return after the interrupt")
	}
	h.waitTurns(2)
	h.waitSent("reply-something else")
	h.waitOffset(3)
	for _, s := range h.sentTexts() {
		if strings.Contains(s, "answered") {
			t.Errorf("interrupted question turn sent a reply %q, want none", s)
		}
	}
}

func TestBusySteerAcceptedRunsNoTurn(t *testing.T) {
	h := newBusyHarness(t)
	h.setMode(BusyModeSteer)
	h.setSteerAccept(true)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	h.setHandler(firstTurnBlocker(h, started, release, cancelled))

	h.fake.enqueue(busyMsg(1, "first"))
	select {
	case <-started:
	case <-time.After(scaledTimeout(5 * time.Second)):
		t.Fatal("first turn did not start")
	}
	h.fake.enqueue(busyMsg(2, "steered text"))
	h.waitSent("Steered into the current run")
	// The steered update runs no turn of its own.
	time.Sleep(300 * time.Millisecond)
	if got := h.turnsSnapshot(); len(got) != 1 {
		t.Fatalf("turns = %v, want only [first] (steered update runs no turn)", got)
	}
	if got := h.steerCallsSnapshot(); len(got) != 1 || got[0].text != "steered text" || got[0].updateID != 2 {
		t.Errorf("steer calls = %v, want one call (77, steered text, 2)", got)
	}
	select {
	case <-cancelled:
		t.Fatal("steer must not cancel the running turn")
	default:
	}
	close(release)
	h.waitSent("first-done")
	h.waitOffset(3)
	if got := h.countSentContaining("Steered"); got != 1 {
		t.Errorf("steer acks sent = %d, want exactly 1", got)
	}
}

func TestBusySteerNotAcceptedRunsNormalTurn(t *testing.T) {
	h := newBusyHarness(t)
	h.setMode(BusyModeSteer)
	h.setSteerAccept(false)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	h.setHandler(firstTurnBlocker(h, started, release, cancelled))

	h.fake.enqueue(busyMsg(1, "first"))
	select {
	case <-started:
	case <-time.After(scaledTimeout(5 * time.Second)):
		t.Fatal("first turn did not start")
	}
	h.fake.enqueue(busyMsg(2, "second"))
	// Declined: the message queues behind the lock, so it gets the
	// queue ack — never the steer ack, which would claim it joined
	// the run.
	h.waitSent("Queued — I'll get to this after the current task finishes")
	if got := h.countSentContaining("Steered"); got != 0 {
		t.Errorf("steer acks sent = %d, want 0 (steer was not accepted)", got)
	}
	close(release)
	h.waitTurns(2)
	h.waitSent("reply-second")
	h.waitOffset(3)
	if got := h.turnsSnapshot(); len(got) != 2 || got[1].text != "second" {
		t.Errorf("turns = %v, want [first second]", got)
	}
}

func TestBusyCommandsExempt(t *testing.T) {
	h := newBusyHarness(t)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	h.setHandler(firstTurnBlocker(h, started, release, cancelled))

	h.fake.enqueue(busyMsg(1, "first"))
	select {
	case <-started:
	case <-time.After(scaledTimeout(5 * time.Second)):
		t.Fatal("first turn did not start")
	}
	// A command mid-turn produces no ack and no interrupt: it waits
	// on the lock and runs as a turn afterwards, like any command.
	h.fake.enqueue(busyMsg(2, "/busy"))
	time.Sleep(300 * time.Millisecond)
	for _, s := range h.sentTexts() {
		if strings.Contains(s, "Redirected") || strings.Contains(s, "Queued") || strings.Contains(s, "Steered") {
			t.Errorf("command mid-turn got a busy ack %q, want none", s)
		}
	}
	select {
	case <-cancelled:
		t.Fatal("a command must not cancel the running turn")
	default:
	}
	if got := h.turnsSnapshot(); len(got) != 1 {
		t.Errorf("turns = %v, want only [first] while the command waits on the lock", got)
	}
	close(release)
	h.waitTurns(2)
	h.waitSent("reply-/busy")
	h.waitOffset(3)
	if got := h.turnsSnapshot(); len(got) != 2 || got[1].text != "/busy" {
		t.Errorf("turns = %v, want [first /busy]", got)
	}
}

func TestBusySteerLeftoversRunAsTurns(t *testing.T) {
	h := newBusyHarness(t)
	h.setLeftovers([]core.SteeredMessage{{Text: "leftover text", UpdateID: 99}})
	h.fake.enqueue(busyMsg(1, "first"))
	h.waitTurns(2)
	// The leftover runs through the same handler (with its own
	// update id in ctx) and its reply lands before the main reply.
	h.waitSent("reply-leftover text")
	h.waitSent("reply-first")
	h.waitOffset(2)
	if got := h.turnsSnapshot(); len(got) != 2 || got[0].text != "first" || got[1].text != "leftover text" {
		t.Errorf("turns = %v, want [first 'leftover text']", got)
	}
	if got := h.turnIDsSnapshot(); len(got) != 2 || got[0] != 1 || got[1] != 99 {
		t.Errorf("turn update ids = %v, want [1 99]", got)
	}
	if got := h.sentTexts(); len(got) != 2 || got[0] != "reply-leftover text" || got[1] != "reply-first" {
		t.Errorf("sent = %q, want [reply-leftover text reply-first] (leftover reply before the main reply)", got)
	}
}

// TestBusyCommand exercises /busy through Dispatch with a stub env:
// status with no args, set to each mode, and a bad argument.
func TestBusyCommand(t *testing.T) {
	newEnv := func() (*CommandEnv, *string, *BusyMode, *error) {
		var stored string
		var setMode BusyMode
		var setErr error
		env := &CommandEnv{
			ChatID: 7,
			BusyMode: func(chatID int64) BusyMode {
				m, _ := ParseBusyMode(stored)
				return m
			},
			SetBusyMode: func(chatID int64, mode BusyMode) error {
				if setErr != nil {
					return setErr
				}
				stored = mode.String()
				setMode = mode
				return nil
			},
		}
		return env, &stored, &setMode, &setErr
	}
	dispatch := func(t *testing.T, env *CommandEnv, text string) string {
		t.Helper()
		r := DefaultRegistry(env)
		reply, handled, err := r.Dispatch(context.Background(), env, text)
		if err != nil {
			t.Fatalf("Dispatch(%q) error: %v", text, err)
		}
		if !handled {
			t.Fatalf("Dispatch(%q) not handled", text)
		}
		return reply
	}
	// Status: current mode + one line per mode + the change hint.
	env, _, _, _ := newEnv()
	reply := dispatch(t, env, "/busy")
	for _, want := range []string{"Busy input mode: interrupt", "interrupt", "queue", "steer", "Change with /busy queue, /busy steer, or /busy interrupt."} {
		if !strings.Contains(reply, want) {
			t.Errorf("status reply missing %q:\n%s", want, reply)
		}
	}
	// Set: each mode persists and is confirmed by name.
	for _, tc := range []struct {
		text string
		mode BusyMode
	}{
		{"/busy queue", BusyModeQueue},
		{"/busy steer", BusyModeSteer},
		{"/busy interrupt", BusyModeInterrupt},
		{"/busy QUEUE", BusyModeQueue},
	} {
		env, stored, setMode, _ := newEnv()
		reply := dispatch(t, env, tc.text)
		if !strings.Contains(reply, "Busy input mode set to "+tc.mode.String()) {
			t.Errorf("Dispatch(%q) reply = %q, want the set confirmation naming %s", tc.text, reply, tc.mode)
		}
		if *setMode != tc.mode || *stored != tc.mode.String() {
			t.Errorf("Dispatch(%q) set mode = %v (stored %q), want %v", tc.text, *setMode, *stored, tc.mode)
		}
		// The next status shows the new mode.
		if reply := dispatch(t, env, "/busy"); !strings.Contains(reply, "Busy input mode: "+tc.mode.String()) {
			t.Errorf("status after Dispatch(%q) = %q, want mode %s", tc.text, reply, tc.mode)
		}
	}
	// Bad argument: usage reply, not an error, and nothing is set.
	env, stored, _, _ := newEnv()
	reply = dispatch(t, env, "/busy sideways")
	if !strings.Contains(reply, "Usage: /busy") {
		t.Errorf("bad-arg reply = %q, want the usage text", reply)
	}
	if *stored != "" {
		t.Errorf("bad argument set the mode to %q, want unchanged", *stored)
	}
	// Unwired SetBusyMode: the set reports unavailability.
	env, _, _, _ = newEnv()
	env.SetBusyMode = nil
	reply = dispatch(t, env, "/busy queue")
	if !strings.Contains(reply, "unavailable") {
		t.Errorf("unwired set reply = %q, want the unavailable text", reply)
	}
	// A SetBusyMode error is reported, not swallowed.
	env, _, _, setErr := newEnv()
	*setErr = errors.New("disk full")
	reply = dispatch(t, env, "/busy queue")
	if !strings.Contains(reply, "Could not set the busy mode") {
		t.Errorf("failing set reply = %q, want the failure text", reply)
	}
}
