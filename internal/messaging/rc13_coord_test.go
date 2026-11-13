// rc13 coordinator regression test: the approval log lines must
// write to the stderr captured when the hook was installed, not to
// whatever os.Stderr points at when the hook fires. Root cause of a
// data race found by -race in TestW3DaemonAndTelegramPollGatePrompts
// (pre-existing on rc12, 1/10 runs): the expiry goroutine's
// Fprintf read the os.Stderr global while the test's capture helper
// restored it. The fix captures os.Stderr once at install time in
// ApprovalPrompter, InstallApprovalExpiry, and (at entry) in
// HandleApprovalCallback. This test pins the install-time capture
// deterministically, without needing -race timing: it installs the
// hook while stderr is pipe A, swaps stderr to pipe B, lets the
// request expire, and asserts the "approval expired" line landed in
// A, not B. On unfixed code the line lands in B and the test fails.
package messaging

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/irfan/orion/internal/permissions"
)

func TestRC13ApprovalExpiryLogsToInstallTimeStderr(t *testing.T) {
	oldStderr := os.Stderr
	defer func() { os.Stderr = oldStderr }()

	rA, wA, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe A: %v", err)
	}
	rB, wB, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe B: %v", err)
	}

	// Install the hook while stderr is pipe A: the fix captures
	// it here; the unfixed code reads the global at fire time.
	os.Stderr = wA
	fake := newQueueFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	gw, err := NewTelegramGateway(testToken, WithBaseURL(srv.URL), WithAllowedChats(77))
	if err != nil {
		t.Fatal(err)
	}
	cards := NewApprovalCards()
	gate := permissions.NewGate(permissions.NewRuleStore(""), ApprovalPrompter(gw, cards, "test"))
	gate.SetTimeout(150 * time.Millisecond)
	InstallApprovalExpiry(gate, gw, cards, "test")

	// Swap stderr to pipe B before the request is even made.
	os.Stderr = wB

	ctx := permissions.WithApprovalContext(context.Background(), 77, 77)
	resultCh := make(chan bool, 1)
	go func() {
		ok, _ := gate.Check(ctx, "shell_exec", `{"command":"echo hi"}`)
		resultCh <- ok
	}()
	var reqID string
	var cardMsgID int64
	waitFor(t, "approval card for chat 77", func() bool {
		reqID, cardMsgID = approvalCardID(fake, 77)
		return reqID != ""
	})
	select {
	case ok := <-resultCh:
		if ok {
			t.Error("Check returned true for an unanswered request, want false (expired)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Check did not return after the approval window")
	}
	waitFor(t, "expired card edited in place", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, e := range fake.edits {
			if e.MsgID == cardMsgID && strings.Contains(e.Text, "Expired") && e.HasKeys {
				return true
			}
		}
		return false
	})

	// Both pipes are drained only now: the hook has fired and the
	// edit completed, so no writer is still active.
	_ = wA.Close()
	_ = wB.Close()
	os.Stderr = oldStderr
	outA, _ := io.ReadAll(rA)
	outB, _ := io.ReadAll(rB)
	if !strings.Contains(string(outA), "approval expired: request "+reqID) {
		t.Errorf("pipe A (install-time stderr) = %q, want it to contain the expiry line for request %s", string(outA), reqID)
	}
	if strings.Contains(string(outB), "approval expired: request "+reqID) {
		t.Errorf("pipe B (stderr at fire time) = %q, want it NOT to contain the expiry line for request %s (the hook must use the install-time writer)", string(outB), reqID)
	}
}
