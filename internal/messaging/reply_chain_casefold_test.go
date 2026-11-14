// Typed-nil interceptor tests for the reply chain (F-A22-9):
// replyChain.add must reject a typed-nil interceptor the same
// way Register rejects a typed-nil tool, instead of appending
// it and panicking (nil deref) on every later message.
package messaging

import (
	"fmt"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/tools"
)

// TestReplyChainAddRejectsTypedNilInterceptor reproduces the
// Audit 22 F-A22-9 live probe: a typed-nil interceptor
// (a nil *fakeReplyInterceptor stored in the ReplyInterceptor
// interface) must NOT be appended. Before the fix it was
// appended, and every message then panicked inside TryAnswer —
// recovered + logged by tryOne's panic isolation, so no crash
// and no lost update, but one panic-log line per message while
// the interceptor stayed registered.
//
// Assertion shape matters (research §(c) trap): the test asserts
// on the chain length and on the absence of panic log lines —
// a plain interface comparison, not a reflecting NotNil-style
// assertion, which would itself call the typed nil nil and pass
// against unfixed code.
func TestReplyChainAddRejectsTypedNilInterceptor(t *testing.T) {
	var logs []string
	c := newReplyChain()
	c.logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	var nilInterceptor *fakeReplyInterceptor
	var in tools.ReplyInterceptor = nilInterceptor
	// Setup proof: the probe value really is a typed nil — a
	// plain == nil check misses it, which is exactly the bug.
	if in == nil {
		t.Fatal("test setup broken: the typed-nil probe compares nil as an interface, so it would not exercise the guard")
	}
	c.add(in)
	if got := len(c.interceptors); got != 0 {
		t.Fatalf("add(typed nil): chain holds %d interceptor(s), want 0 (rejected, not appended)", got)
	}
	// The rejection itself is logged (the only observable the
	// caller gets, since add returns nothing): exactly one drop
	// line, naming the nil.
	if len(logs) != 1 || !strings.Contains(logs[0], "dropped") {
		t.Fatalf("add(typed nil) logged %v, want exactly one drop line", logs)
	}
	logs = nil
	// A message after the rejected add: unconsumed, and — the
	// audit's observable — ZERO log lines. Before the fix the
	// appended typed nil panicked here and logged one panic line
	// per message.
	if c.tryAnswer(testUpdate(77)) {
		t.Error("tryAnswer after rejected typed-nil add returned true, want false")
	}
	if len(logs) != 0 {
		t.Errorf("tryAnswer logged %d line(s) %v, want none (no panic line from a rejected interceptor)", len(logs), logs)
	}
	// The chain stays usable afterwards: a live interceptor
	// added later still runs and consumes.
	good := &fakeReplyInterceptor{consume: true}
	c.add(good)
	if !c.tryAnswer(testUpdate(77)) {
		t.Error("tryAnswer after adding a live interceptor returned false, want true")
	}
	if good.calls != 1 {
		t.Errorf("live interceptor calls = %d, want 1", good.calls)
	}
}

// TestReplyChainAddUntypedNilStillNoop is the negative control:
// an untyped nil was already a no-op before the fix and must
// stay one (no log line, no entry, chain usable).
func TestReplyChainAddUntypedNilStillNoop(t *testing.T) {
	var logs []string
	c := newReplyChain()
	c.logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	c.add(nil)
	if len(c.interceptors) != 0 {
		t.Fatalf("add(nil): chain holds %d interceptor(s), want 0", len(c.interceptors))
	}
	if c.tryAnswer(testUpdate(77)) {
		t.Error("tryAnswer on an empty chain returned true, want false")
	}
	if len(logs) != 0 {
		t.Errorf("tryAnswer logged %v, want none", logs)
	}
}

// TestGatewayAddReplyInterceptorRejectsTypedNil covers the same
// guard through the gateway's only public entry point: a
// typed-nil interceptor handed to AddReplyInterceptor must not
// reach the chain, so no later message panics inside it.
func TestGatewayAddReplyInterceptorRejectsTypedNil(t *testing.T) {
	var logs []string
	g, err := NewTelegramGateway(testToken, WithLogger(func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}))
	if err != nil {
		t.Fatalf("NewTelegramGateway: %v", err)
	}
	var nilInterceptor *fakeReplyInterceptor
	g.AddReplyInterceptor(nilInterceptor)
	if g.replyChainTryAnswer(testUpdate(77)) {
		t.Error("replyChainTryAnswer after typed-nil AddReplyInterceptor returned true, want false")
	}
	for _, line := range logs {
		if strings.Contains(line, "panicked") {
			t.Errorf("typed-nil interceptor produced a panic log line %q, want none", line)
		}
	}
}
