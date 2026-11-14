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
	// exactly one line naming its type and the chat.
	if len(logs) != 1 {
		t.Fatalf("audit log lines = %d, want 1: %v", len(logs), logs)
	}
	if !strings.Contains(logs[0], "consumed") || !strings.Contains(logs[0], "78") || !strings.Contains(logs[0], "fakeReplyInterceptor") {
		t.Errorf("consumption log line = %q, want interceptor type + chat", logs[0])
	}
	if quiet.calls != 1 || eater.calls != 1 {
		t.Errorf("calls = quiet %d, eater %d; want 1 and 1", quiet.calls, eater.calls)
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
