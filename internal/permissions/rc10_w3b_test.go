// W3b (rc10) regression tests for Audit 5 F-A5-P2-20 (Resolve-vs-
// expiry race) and F-A5-P2-22 (prompter-failure logging).
package permissions

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"
)

// TestW3BResolveRecordsOutcomeBeforeDelivery proves the ordering
// guarantee the timer branch of Check depends on (F-A5-P2-20):
// Resolve records the outcome on the pending request under the gate
// mutex BEFORE delivering it to the channel, so a timer branch that
// runs after Resolve can see the user's answer via resolvedOutcome
// and honour it instead of expiring the request.
//
// The wall-clock race itself — a tap landing within microseconds of
// the 90 s deadline — cannot be reproduced deterministically: any
// Resolve that lands clearly before the deadline is delivered via
// the channel, and one that lands after it is a legitimate late tap.
// What this test pins is the invariant that makes the fix work: the
// outcome is recorded before delivery, and the timer branch's
// re-check reads it back.
func TestW3BResolveRecordsOutcomeBeforeDelivery(t *testing.T) {
	reqCh := make(chan ApprovalRequest, 1)
	g := NewGate(NewRuleStore(""), func(ctx context.Context, req ApprovalRequest) error {
		reqCh <- req
		return nil
	})
	g.SetTimeout(30 * time.Second)
	g.SetOnExpire(func(req ApprovalRequest) {
		t.Errorf("OnExpire fired for a request the user answered (%q)", req.ID)
	})
	resultCh := make(chan bool, 1)
	go func() {
		ok, _ := g.Check(ctxWithChat(t), "shell_exec", `{"command":"echo hi"}`)
		resultCh <- ok
	}()
	var req ApprovalRequest
	select {
	case req = <-reqCh:
	case <-time.After(5 * time.Second):
		t.Fatal("gate never prompted")
	}
	g.mu.Lock()
	pr, ok := g.pending[req.ID]
	g.mu.Unlock()
	if !ok || pr == nil {
		t.Fatal("pending request missing while Check is waiting")
	}
	// Before Resolve, the timer branch must see nothing recorded.
	if _, ok := g.resolvedOutcome(pr); ok {
		t.Error("resolvedOutcome reported an outcome before Resolve ran")
	}
	if !g.Resolve(req.ID, true, nil) {
		t.Fatal("Resolve of a pending request returned false")
	}
	// After Resolve, the outcome is visible to the timer branch —
	// even though Check may already have consumed the channel value.
	res, ok := g.resolvedOutcome(pr)
	if !ok {
		t.Fatal("resolvedOutcome reported nothing after Resolve recorded the approval")
	}
	if !res.approved {
		t.Errorf("resolvedOutcome = %+v, want approved=true", res)
	}
	select {
	case ok := <-resultCh:
		if !ok {
			t.Error("Check returned false after the requester approved")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Check did not return after approval")
	}
}

// TestW3BResolveRecordsDenialToo proves the same recording covers
// a denial: a tap that declines the call must be visible to the
// timer branch as declined, not as "no outcome recorded".
func TestW3BResolveRecordsDenialToo(t *testing.T) {
	reqCh := make(chan ApprovalRequest, 1)
	g := NewGate(NewRuleStore(""), func(ctx context.Context, req ApprovalRequest) error {
		reqCh <- req
		return nil
	})
	g.SetTimeout(30 * time.Second)
	resultCh := make(chan bool, 1)
	go func() {
		ok, _ := g.Check(ctxWithChat(t), "shell_exec", `{"command":"echo hi"}`)
		resultCh <- ok
	}()
	var req ApprovalRequest
	select {
	case req = <-reqCh:
	case <-time.After(5 * time.Second):
		t.Fatal("gate never prompted")
	}
	g.mu.Lock()
	pr := g.pending[req.ID]
	g.mu.Unlock()
	if !g.Resolve(req.ID, false, nil) {
		t.Fatal("Resolve of a pending request returned false")
	}
	res, ok := g.resolvedOutcome(pr)
	if !ok {
		t.Fatal("resolvedOutcome reported nothing after Resolve recorded the denial")
	}
	if res.approved {
		t.Errorf("resolvedOutcome = %+v, want approved=false", res)
	}
	select {
	case ok := <-resultCh:
		if ok {
			t.Error("Check returned true after the requester declined")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Check did not return after denial")
	}
}

// TestW3BPrompterFailureIsLogged proves the F-A5-P2-22 logging gap
// is closed: when the prompter fails to send the approval card, the
// denial carries a log line naming the request, the tool and the
// error — instead of being silent and indistinguishable from "the
// user never answered".
func TestW3BPrompterFailureIsLogged(t *testing.T) {
	var buf bytes.Buffer
	g := NewGate(NewRuleStore(""), func(ctx context.Context, req ApprovalRequest) error {
		return errors.New("send failed: network down")
	})
	g.SetLogger(log.New(&buf, "", 0))
	ok, reason := g.Check(ctxWithChat(t), "shell_exec", `{"command":"echo hi"}`)
	if ok {
		t.Error("prompter error allowed the call")
	}
	if !strings.Contains(reason, "could not ask for approval") {
		t.Errorf("denial reason = %q, want it to say the approval could not be asked", reason)
	}
	logged := buf.String()
	for _, want := range []string{"prompter failed", "shell_exec", "send failed"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log = %q, want it to contain %q", logged, want)
		}
	}
	if g.PendingCount() != 0 {
		t.Error("failed prompt left a pending request")
	}
}
