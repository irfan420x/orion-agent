// rc24 tests: daily budget threshold alert (F-A18-11) — the
// App-layer wiring: a turn whose recorded usage crosses the
// threshold notifies the owner exactly once per chat per day,
// whether the budget is finite or unlimited, and never when the
// alert is disabled. The threshold evaluation and the persisted
// "alerted today" state are covered in internal/budget
// (alert_test.go); these tests prove the turn path calls them.
package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/irfan/orion/internal/budget"
	"github.com/irfan/orion/internal/model"
)

// alertCall is one delivered budget alert: the chat it was about
// and the formatted text the owner would receive.
type alertCall struct {
	chatID int64
	text   string
}

// alertRecorder is a BudgetAlertNotifier that records every
// delivery (and can be told to fail deliveries, to prove the
// retry-on-failure behaviour).
type alertRecorder struct {
	mu       sync.Mutex
	calls    []alertCall
	failNext int // deliveries to fail before succeeding
	failures int
}

func (r *alertRecorder) notify(_ context.Context, chatID int64, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failures < r.failNext {
		r.failures++
		return errors.New("test: simulated Telegram send failure")
	}
	r.calls = append(r.calls, alertCall{chatID: chatID, text: text})
	return nil
}

func (r *alertRecorder) delivered() []alertCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]alertCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// alertTestApp builds a test App whose provider reports
// tokensPerTurn tokens on every turn, with a Budget of limit and
// an alert threshold, and a recording notifier. The returned
// App is closed by the caller (defer a.Close()).
func alertTestApp(t *testing.T, limit, threshold, tokensPerTurn int) (*App, *alertRecorder) {
	t.Helper()
	a := testApp(t)
	a.Budget = budget.New(limit)
	a.Budget.SetAlertThreshold(threshold)
	rec := &alertRecorder{}
	a.SetBudgetAlertNotifier(rec.notify)
	a.Provider = &usageScriptProvider{chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		return finalUsageResponse("ok", model.Usage{PromptTokens: tokensPerTurn, TotalTokens: tokensPerTurn}), nil
	}}
	return a, rec
}

// runAlertTurn runs one turn for chatID and fails the test on an
// unexpected turn error (a budget refusal here would mean the
// test's numbers are wrong, not that the alert works).
func runAlertTurn(t *testing.T, a *App, chatID int64, input string) {
	t.Helper()
	if _, _, err := a.HandleStreamForChat(context.Background(), chatID, input, nil); err != nil {
		t.Fatalf("HandleStreamForChat(%d, %q): %v", chatID, input, err)
	}
}

// TestBudgetAlertBelowThreshold: a chat under the threshold is
// never alerted, however many turns it runs below it.
func TestBudgetAlertBelowThreshold(t *testing.T) {
	a, rec := alertTestApp(t, 100000, 100, 60)
	defer a.Close()
	runAlertTurn(t, a, 111, "first")
	if got := a.Budget.Usage(111); got != 60 {
		t.Fatalf("Usage(111) = %d, want 60", got)
	}
	if calls := rec.delivered(); len(calls) != 0 {
		t.Errorf("delivered alerts = %d, want 0 below the threshold", len(calls))
	}
	if got := a.BudgetAlertThreshold(); got != 100 {
		t.Errorf("BudgetAlertThreshold() = %d, want 100", got)
	}
}

// TestBudgetAlertCrossingFiresOnce: the turn that reaches the
// threshold alerts exactly once; later turns the same day — and
// other chats' crossings — alert only for their own chat, once.
func TestBudgetAlertCrossingFiresOnce(t *testing.T) {
	a, rec := alertTestApp(t, 100000, 100, 60)
	defer a.Close()
	runAlertTurn(t, a, 111, "first") // 60: below
	if calls := rec.delivered(); len(calls) != 0 {
		t.Fatalf("alerts after turn 1 = %d, want 0", len(calls))
	}
	runAlertTurn(t, a, 111, "second") // 120: crosses
	calls := rec.delivered()
	if len(calls) != 1 {
		t.Fatalf("alerts after crossing turn = %d, want exactly 1", len(calls))
	}
	if calls[0].chatID != 111 {
		t.Errorf("alert chatID = %d, want 111", calls[0].chatID)
	}
	for _, want := range []string{"111", "120", "100", "UTC midnight"} {
		if !strings.Contains(calls[0].text, want) {
			t.Errorf("alert text missing %q:\n%s", want, calls[0].text)
		}
	}
	runAlertTurn(t, a, 111, "third") // 180: still above, no repeat
	if calls := rec.delivered(); len(calls) != 1 {
		t.Errorf("alerts after turn 3 = %d, want still exactly 1 (once per chat per day)", len(calls))
	}
	// Another chat crossing alerts for itself, once.
	runAlertTurn(t, a, 222, "other one") // 60: below
	runAlertTurn(t, a, 222, "other two") // 120: crosses
	calls = rec.delivered()
	if len(calls) != 2 {
		t.Fatalf("alerts after chat 222 crosses = %d, want 2 (one per chat)", len(calls))
	}
	if calls[1].chatID != 222 {
		t.Errorf("second alert chatID = %d, want 222", calls[1].chatID)
	}
}

// TestBudgetAlertDisabled: threshold 0 (the default) never
// alerts, whatever the usage.
func TestBudgetAlertDisabled(t *testing.T) {
	a, rec := alertTestApp(t, 100000, 0, 60)
	defer a.Close()
	runAlertTurn(t, a, 111, "first")
	runAlertTurn(t, a, 111, "second")
	runAlertTurn(t, a, 111, "third")
	if calls := rec.delivered(); len(calls) != 0 {
		t.Errorf("delivered alerts = %d with threshold 0, want 0 (disabled)", len(calls))
	}
	if got := a.BudgetAlertThreshold(); got != 0 {
		t.Errorf("BudgetAlertThreshold() = %d, want 0", got)
	}
}

// TestBudgetAlertUnlimitedLimit: the live case — budget unlimited
// (limit 0) with a threshold set — the alert still fires, and the
// message says the budget is unlimited (turns are watched, not
// refused).
func TestBudgetAlertUnlimitedLimit(t *testing.T) {
	a, rec := alertTestApp(t, 0, 100, 60)
	defer a.Close()
	if got := a.Budget.Limit(); got != 0 {
		t.Fatalf("Limit() = %d, want 0 (unlimited)", got)
	}
	runAlertTurn(t, a, 111, "first")  // 60: below
	runAlertTurn(t, a, 111, "second") // 120: crosses
	calls := rec.delivered()
	if len(calls) != 1 {
		t.Fatalf("alerts with unlimited budget = %d, want exactly 1", len(calls))
	}
	if !strings.Contains(calls[0].text, "unlimited") {
		t.Errorf("alert text with unlimited budget missing \"unlimited\":\n%s", calls[0].text)
	}
	runAlertTurn(t, a, 111, "third") // 180: no repeat
	if calls := rec.delivered(); len(calls) != 1 {
		t.Errorf("alerts after turn 3 = %d, want still exactly 1", len(calls))
	}
}

// TestBudgetAlertFailedDeliveryRetries: a delivery that fails is
// NOT marked alerted — the next record retries, and the owner
// still gets exactly one delivered alert.
func TestBudgetAlertFailedDeliveryRetries(t *testing.T) {
	a, rec := alertTestApp(t, 100000, 100, 60)
	defer a.Close()
	rec.failNext = 1
	runAlertTurn(t, a, 111, "first")  // 60: below, no attempt
	runAlertTurn(t, a, 111, "second") // 120: crosses, delivery fails
	if calls := rec.delivered(); len(calls) != 0 {
		t.Fatalf("delivered alerts after failed send = %d, want 0", len(calls))
	}
	if rec.failures != 1 {
		t.Fatalf("failed deliveries = %d, want 1", rec.failures)
	}
	runAlertTurn(t, a, 111, "third") // 180: still above, retries
	calls := rec.delivered()
	if len(calls) != 1 {
		t.Fatalf("delivered alerts after retry = %d, want exactly 1", len(calls))
	}
	runAlertTurn(t, a, 111, "fourth") // no repeat after success
	if calls := rec.delivered(); len(calls) != 1 {
		t.Errorf("delivered alerts after turn 4 = %d, want still exactly 1", len(calls))
	}
}

// TestBudgetAlertNoNotifier: with no notifier installed (the
// CLI / hand-built App case) turns run normally and nothing
// panics — the alert is simply never sent.
func TestBudgetAlertNoNotifier(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(100000)
	a.Budget.SetAlertThreshold(100)
	a.Provider = &usageScriptProvider{chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		return finalUsageResponse("ok", model.Usage{TotalTokens: 150}), nil
	}}
	runAlertTurn(t, a, 111, "first")
	if got := a.Budget.Usage(111); got != 150 {
		t.Errorf("Usage(111) = %d, want 150 (turn still records without a notifier)", got)
	}
	if _, _, crossed := a.Budget.CheckAlert(111); !crossed {
		t.Error("CheckAlert(111) = no crossing without a notifier, want the evaluation to still answer true")
	}
}

// TestFormatBudgetAlertMessage: the alert names the chat, its
// usage and the threshold, and states the budget (unlimited, or
// the finite limit) and the UTC-midnight reset.
func TestFormatBudgetAlertMessage(t *testing.T) {
	unlimited := FormatBudgetAlertMessage(7429947930, 120, 100, 0)
	for _, want := range []string{"7429947930", "120", "100", "unlimited", "UTC midnight"} {
		if !strings.Contains(unlimited, want) {
			t.Errorf("unlimited alert missing %q:\n%s", want, unlimited)
		}
	}
	finite := FormatBudgetAlertMessage(111, 1000, 1000, 100000)
	for _, want := range []string{"111", "1000", "100000", "UTC midnight"} {
		if !strings.Contains(finite, want) {
			t.Errorf("finite alert missing %q:\n%s", want, finite)
		}
	}
	if strings.Contains(finite, "unlimited") {
		t.Errorf("finite alert must not say unlimited:\n%s", finite)
	}
}
