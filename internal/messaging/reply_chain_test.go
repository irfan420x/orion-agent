// Reply-chain tests (F-A20-10): per-interceptor panic isolation
// and the consumption audit log.
package messaging

import (
	"fmt"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/tools"
)

// fakeReplyInterceptor is a tools.ReplyInterceptor with scripted
// behaviour: it panics when panicVal is set, otherwise it consumes
// the update when consume is true.
type fakeReplyInterceptor struct {
	consume  bool
	panicVal any
	calls    int
}

func (f *fakeReplyInterceptor) TryAnswer(upd tools.ReplyUpdate) bool {
	f.calls++
	if f.panicVal != nil {
		panic(f.panicVal)
	}
	return f.consume
}

func testUpdate(chatID int64) Update {
	return Update{Message: &TGMessage{Chat: TGChat{ID: chatID}, Text: "hi", From: &TGUser{ID: chatID}}}
}

func TestReplyChainPanickingInterceptorIsolated(t *testing.T) {
	var logs []string
	c := newReplyChain()
	c.logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	broken := &fakeReplyInterceptor{panicVal: "boom"}
	good := &fakeReplyInterceptor{consume: true}
	c.add(broken)
	c.add(good)
	// A panicking interceptor must not kill the chain: the next
	// interceptor still runs and consumes the update.
	if !c.tryAnswer(testUpdate(77)) {
		t.Fatal("tryAnswer returned false: the panic swallowed the update instead of isolating")
	}
	if broken.calls != 1 || good.calls != 1 {
		t.Errorf("calls = broken %d, good %d; want 1 and 1", broken.calls, good.calls)
	}
	if len(logs) != 2 {
		t.Fatalf("audit log lines = %d, want 2 (panic + consumption): %v", len(logs), logs)
	}
	if !strings.Contains(logs[0], "panicked") || !strings.Contains(logs[0], "boom") {
		t.Errorf("panic log line = %q, want it to name the panic", logs[0])
	}
	if !strings.Contains(logs[1], "consumed") || !strings.Contains(logs[1], "77") {
		t.Errorf("consumption log line = %q, want it to name the chat", logs[1])
	}
}

func TestReplyChainPanickingInterceptorAloneConsumesNothing(t *testing.T) {
	var logs []string
	c := newReplyChain()
	c.logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	broken := &fakeReplyInterceptor{panicVal: "boom"}
	c.add(broken)
	// A lone panicking interceptor leaves the update unconsumed
	// (the caller then starts a normal turn) instead of crashing.
	if c.tryAnswer(testUpdate(77)) {
		t.Error("tryAnswer returned true for a panicking interceptor, want false")
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "panicked") {
		t.Errorf("audit log = %v, want exactly one panic line", logs)
	}
}

func TestReplyChainConsumptionAuditLog(t *testing.T) {
	var logs []string
	c := newReplyChain()
	c.logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	quiet := &fakeReplyInterceptor{consume: false}
	eater := &fakeReplyInterceptor{consume: true}
	c.add(quiet)
	c.add(eater)
	if !c.tryAnswer(testUpdate(78)) {
		t.Fatal("tryAnswer returned false, want the consuming interceptor to win")
	}
	// A non-consuming interceptor logs nothing; the consumer logs
	// exactly one line naming its type, the chat, and the
	// consumed text (F-A21-2: the text is the audit trail for
	// what the interceptor claimed).
	if len(logs) != 1 {
		t.Fatalf("audit log lines = %d, want 1: %v", len(logs), logs)
	}
	if !strings.Contains(logs[0], "consumed") || !strings.Contains(logs[0], "78") || !strings.Contains(logs[0], "fakeReplyInterceptor") {
		t.Errorf("consumption log line = %q, want interceptor type + chat", logs[0])
	}
	if !strings.Contains(logs[0], "hi") {
		t.Errorf("consumption log line = %q, want it to name the consumed text", logs[0])
	}
	if quiet.calls != 1 || eater.calls != 1 {
		t.Errorf("calls = quiet %d, eater %d; want 1 and 1", quiet.calls, eater.calls)
	}
}

// TestReplyChainNeverConsumesCommands pins the F-A21-2
// enforcement: a greedy interceptor that returns true for every
// update must NEVER consume a command — tryAnswer skips the
// whole chain, the interceptor is not even called, and the
// bypass is audit-logged so the enforcement is provable from
// the daemon log.
func TestReplyChainNeverConsumesCommands(t *testing.T) {
	var logs []string
	c := newReplyChain()
	c.logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	greedy := &fakeReplyInterceptor{consume: true}
	c.add(greedy)
	for _, text := range []string{"/start", "  /help  ", "/plan approve"} {
		upd := Update{Message: &TGMessage{Chat: TGChat{ID: 77}, Text: text, From: &TGUser{ID: 77}}}
		if c.tryAnswer(upd) {
			t.Fatalf("tryAnswer(%q) returned true, want false: commands are never answers", text)
		}
	}
	if greedy.calls != 0 {
		t.Errorf("greedy interceptor calls = %d, want 0: the chain must not call any interceptor for a command", greedy.calls)
	}
	if len(logs) != 3 {
		t.Fatalf("audit log lines = %d, want 3 (one bypass per command): %v", len(logs), logs)
	}
	for i, line := range logs {
		if !strings.Contains(line, "skipped") || !strings.Contains(line, "77") {
			t.Errorf("bypass log line %d = %q, want it to name the skip and the chat", i, line)
		}
	}
	if !strings.Contains(logs[0], "/start") {
		t.Errorf("bypass log line = %q, want it to name the command", logs[0])
	}
}

// TestReplyChainCommandBypassLogsOnlyWithInterceptors pins that
// a command arriving on an empty chain logs nothing: with no
// interceptor to skip there is nothing to audit, and a bare
// command must not emit a line per message.
func TestReplyChainCommandBypassLogsOnlyWithInterceptors(t *testing.T) {
	var logs []string
	c := newReplyChain()
	c.logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	upd := Update{Message: &TGMessage{Chat: TGChat{ID: 77}, Text: "/start", From: &TGUser{ID: 77}}}
	if c.tryAnswer(upd) {
		t.Fatal("tryAnswer on an empty chain returned true for a command, want false")
	}
	if len(logs) != 0 {
		t.Fatalf("audit log lines = %d, want 0 on an empty chain: %v", len(logs), logs)
	}
}

// TestReplyChainQuestionInterceptorFirst pins the F-A21-22
// ordering rule structurally: the question interceptor goes
// FIRST in the chain even when a tool interceptor was added
// before SetQuestionRegistry ran — the guarantee must not depend
// on WireTelegram's call order. Behaviourally the question
// interceptor still runs first: with no pending question it
// declines, and the tool interceptor then consumes.
func TestReplyChainQuestionInterceptorFirst(t *testing.T) {
	c := newReplyChain()
	tool := &fakeReplyInterceptor{consume: true}
	c.add(tool)
	c.setQuestion(&questionReplyInterceptor{gw: nil, questions: NewQuestionRegistry(77)})
	if len(c.interceptors) != 2 {
		t.Fatalf("chain length = %d, want 2", len(c.interceptors))
	}
	if _, ok := c.interceptors[0].(*questionReplyInterceptor); !ok {
		t.Fatalf("interceptors[0] = %T, want *questionReplyInterceptor first", c.interceptors[0])
	}
	if !c.tryAnswer(testUpdate(77)) {
		t.Fatal("tryAnswer returned false, want the tool interceptor to consume after the question interceptor declines")
	}
	if tool.calls != 1 {
		t.Errorf("tool interceptor calls = %d, want 1", tool.calls)
	}
}

// TestGatewayReplyChainEnforcesCommandsAndLogs pins the F-A21-2
// wiring end to end through the gateway: the chain's logf is the
// gateway's logger (live in production — both binaries pass
// WithLogger), so a consumption lands in the daemon log, and a
// command is never handed to an interceptor even through the
// gateway entry point.
func TestGatewayReplyChainEnforcesCommandsAndLogs(t *testing.T) {
	var logs []string
	gw, err := NewTelegramGateway(testToken, WithLogger(func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	greedy := &fakeReplyInterceptor{consume: true}
	gw.AddReplyInterceptor(greedy)
	cmd := Update{Message: &TGMessage{Chat: TGChat{ID: 77}, Text: "/start", From: &TGUser{ID: 77}}}
	if gw.replyChainTryAnswer(cmd) {
		t.Fatal("replyChainTryAnswer consumed /start, want false")
	}
	if greedy.calls != 0 {
		t.Errorf("greedy interceptor calls = %d, want 0 for a command", greedy.calls)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "skipped") {
		t.Fatalf("gateway log lines = %v, want exactly one command-bypass line", logs)
	}
	// A normal text update is consumed, and the consumption is
	// audit-logged through the gateway logger with the text.
	if !gw.replyChainTryAnswer(testUpdate(77)) {
		t.Fatal("replyChainTryAnswer did not consume the text reply, want true")
	}
	if len(logs) != 2 || !strings.Contains(logs[1], "consumed") || !strings.Contains(logs[1], "hi") {
		t.Fatalf("gateway log lines = %v, want a consumption line naming the text", logs)
	}
}

func TestReplyChainFirstConsumerWins(t *testing.T) {
	c := newReplyChain()
	first := &fakeReplyInterceptor{consume: true}
	second := &fakeReplyInterceptor{consume: true}
	c.add(first)
	c.add(second)
	if !c.tryAnswer(testUpdate(77)) {
		t.Fatal("tryAnswer returned false, want the first consumer to win")
	}
	if first.calls != 1 || second.calls != 0 {
		t.Errorf("calls = first %d, second %d; want 1 and 0 (first-true-wins)", first.calls, second.calls)
	}
}

func TestReplyChainNilSafeWithoutLogger(t *testing.T) {
	// No logger installed: a panicking interceptor is still
	// isolated and a consuming one still consumes — the audit
	// log is best-effort, never a crash.
	c := newReplyChain()
	c.add(&fakeReplyInterceptor{panicVal: "boom"})
	c.add(&fakeReplyInterceptor{consume: true})
	if !c.tryAnswer(testUpdate(77)) {
		t.Error("tryAnswer returned false without a logger, want the consumer to win")
	}
	var nilChain *replyChain
	if nilChain.tryAnswer(testUpdate(77)) {
		t.Error("nil chain tryAnswer returned true, want false")
	}
}
