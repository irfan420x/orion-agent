// rc25 tests: F-A19-6 — a permanent "no owner" budget-alert
// failure is logged once per chat and never retried, instead of
// being retried (and logged) on every record until UTC midnight.
// A transient delivery failure still retries (see
// TestBudgetAlertFailedDeliveryRetries in rc24_alert_test.go).
package app

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"testing"
)

// noOwnerNotifier is a BudgetAlertNotifier that always fails with
// ErrBudgetAlertNoOwner, the way the daemon entrypoints' closures
// do when ownerID == 0. It counts every call.
type noOwnerNotifier struct {
	calls int
}

func (n *noOwnerNotifier) notify(_ context.Context, _ int64, _ string) error {
	n.calls++
	return fmt.Errorf("oriond: %w", ErrBudgetAlertNoOwner)
}

// TestBudgetAlertNoOwnerLoggedOnce: with no owner configured, the
// crossing turn logs once and the notifier is never called again
// for that chat — however many turns run above the threshold.
func TestBudgetAlertNoOwnerLoggedOnce(t *testing.T) {
	a, _ := alertTestApp(t, 100000, 100, 60)
	defer a.Close()
	var logBuf bytes.Buffer
	a.Logger = log.New(&logBuf, "", 0)
	n := &noOwnerNotifier{}
	a.SetBudgetAlertNotifier(n.notify)

	runAlertTurn(t, a, 111, "first")  // 60: below, no attempt
	runAlertTurn(t, a, 111, "second") // 120: crosses, fails (no owner)
	runAlertTurn(t, a, 111, "third")  // 180: still above, must NOT retry
	runAlertTurn(t, a, 111, "fourth") // 240: still above, must NOT retry
	if n.calls != 1 {
		t.Errorf("no-owner notifier calls = %d, want exactly 1 (logged once, never retried)", n.calls)
	}
	if got := strings.Count(logBuf.String(), "no owner configured"); got != 1 {
		t.Errorf("log mentions \"no owner configured\" %d time(s), want exactly 1:\n%s", got, logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "will not retry until restart") {
		t.Errorf("log does not say the alert will not be retried:\n%s", logBuf.String())
	}
	// The alert was never delivered, so it must NOT be marked
	// alerted: a restart with an owner configured must still be
	// able to fire it for this chat today.
	if _, _, crossed := a.Budget.CheckAlert(111); !crossed {
		t.Error("CheckAlert(111) = no crossing after the no-owner failure, want the evaluation to still answer true (the alert was never delivered, so it must not be marked alerted)")
	}
}

// TestBudgetAlertNoOwnerPerChat: the log-once is per chat — a second
// chat crossing is attempted (and logged) once for itself, while the
// first chat stays quiet.
func TestBudgetAlertNoOwnerPerChat(t *testing.T) {
	a, _ := alertTestApp(t, 100000, 100, 60)
	defer a.Close()
	var logBuf bytes.Buffer
	a.Logger = log.New(&logBuf, "", 0)
	n := &noOwnerNotifier{}
	a.SetBudgetAlertNotifier(n.notify)

	runAlertTurn(t, a, 111, "first")       // 60: below
	runAlertTurn(t, a, 111, "second")      // 120: crosses, fails once
	runAlertTurn(t, a, 222, "other one")   // 60: below
	runAlertTurn(t, a, 222, "other two")   // 120: crosses, fails once
	runAlertTurn(t, a, 111, "third")       // 180: first chat stays quiet
	runAlertTurn(t, a, 222, "other three") // 180: second chat stays quiet
	if n.calls != 2 {
		t.Errorf("no-owner notifier calls = %d, want exactly 2 (once per chat)", n.calls)
	}
	if got := strings.Count(logBuf.String(), "no owner configured"); got != 2 {
		t.Errorf("log mentions \"no owner configured\" %d time(s), want exactly 2 (once per chat):\n%s", got, logBuf.String())
	}
}
