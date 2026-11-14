// Daily budget threshold alert (F-A18-11, rc24): when a chat's
// recorded usage for the UTC day reaches the configured threshold
// (config.DailyBudgetAlertThresholdTokens), the owner is notified
// once per chat per day via Telegram.
//
// The Budget type owns the threshold evaluation and the
// "alerted today" state (persisted in budget.json, so a restart
// never re-alerts for the same day); this file owns the Telegram
// side. The App itself has no gateway — the daemon entrypoints
// (oriond, `orion daemon`, `orion telegram poll`) install a
// notifier that sends to the owner (the first allowlisted chat)
// through their gateway. The injection follows the permission
// gate's Prompter pattern (app.NewPermissionGate): a function
// installed at startup, nil when unwired (hand-built Apps, the
// CLI), in which case no alert is ever sent — the usage is still
// recorded and CheckAlert still answers, so the wiring can be
// tested without Telegram.
//
// Flow (maybeAlertBudget, called after every per-chat record):
// CheckAlert -> if crossed, send via the notifier -> only on a
// successful send, MarkAlerted. A failed send is logged and NOT
// marked: the next record retries, so a transient Telegram outage
// delays the alert instead of losing it for the day. A send that
// succeeded but whose MarkAlerted persist fails is also logged:
// the in-memory flag is already set, so this process still alerts
// at most once for the day.
package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrBudgetAlertNoOwner is returned by a budget-alert notifier when
// there is no owner to notify (F-A19-6). It is permanent for the
// current process — the owner is fixed at startup, so no retry can
// succeed until the daemon restarts with an owner configured —
// unlike a transient Telegram delivery failure, which maybeAlertBudget
// retries on the next record. Notifiers return it (wrapped) when
// ownerID == 0 so the App can tell the two apart.
var ErrBudgetAlertNoOwner = errors.New("budget alert: no owner configured")

// BudgetAlertNotifier sends one budget-threshold alert to the
// owner. chatID is the chat whose usage crossed the threshold;
// text is the preformatted alert (see
// FormatBudgetAlertMessage). The daemon entrypoints wire it to
// send text to the owner chat via their Telegram gateway. A
// returned error means "not delivered" — the caller logs it and
// retries on the next record instead of marking the chat alerted.
// A notifier with no owner configured returns ErrBudgetAlertNoOwner
// (wrapped); that failure is permanent for the process, so the
// caller logs it once per chat and stays quiet instead of retrying
// (and logging) on every record until UTC midnight.
type BudgetAlertNotifier func(ctx context.Context, chatID int64, text string) error

// SetBudgetAlertNotifier installs the owner-alert sender for daily
// budget threshold crossings. It is setup-time only (the daemon
// entrypoints call it before polling starts, like
// NewPermissionGate); a nil notifier means "no alerts" (the
// default for hand-built Apps and the CLI). Calling it again
// replaces the notifier.
func (a *App) SetBudgetAlertNotifier(fn BudgetAlertNotifier) {
	if a == nil {
		return
	}
	a.budgetAlertNotifier = fn
}

// BudgetAlertThreshold returns the configured per-chat daily usage
// alert threshold in tokens (0 means the alert is disabled), or 0
// when the app has no Budget. It is the /budget display's source
// for the threshold line.
func (a *App) BudgetAlertThreshold() int {
	if a == nil || a.Budget == nil {
		return 0
	}
	return a.Budget.AlertThreshold()
}

// FormatBudgetAlertMessage renders the owner alert for a chat
// whose usage today crossed the alert threshold. It names the
// chat, its usage and the threshold, and states the daily budget
// (unlimited, or the finite limit) and the UTC-midnight reset, so
// the owner can tell at a glance whether turns are still being
// refused (finite limit) or only being watched (unlimited).
func FormatBudgetAlertMessage(chatID int64, used, threshold, limit int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "⚠️ Daily token budget alert\nChat %d has used %d tokens today, crossing the alert threshold of %d tokens.", chatID, used, threshold)
	if limit <= 0 {
		b.WriteString("\nDaily budget: unlimited.")
	} else {
		fmt.Fprintf(&b, "\nDaily budget: %d tokens per day.", limit)
	}
	b.WriteString("\nUsage resets at UTC midnight.")
	return b.String()
}

// maybeAlertBudget checks whether chatID's usage today crossed
// the alert threshold and, if so, notifies the owner exactly once
// for the day. It is called after every per-chat usage record
// (turn end, /compress, session-close subagent sweep). It is
// nil-safe and silent when there is no Budget, no notifier, or no
// crossing — an alert is a notification, never a reason to fail
// the turn that just completed. A delivery failure is logged and
// retried on the next record; a successful delivery is marked
// (persisted) so the chat is not alerted again today. The one
// exception is a permanent "no owner" failure
// (ErrBudgetAlertNoOwner): it is logged once per chat for this
// process and never retried — retrying (and logging) it on every
// record until UTC midnight would spam the log for a failure that
// cannot succeed until the daemon restarts with an owner. It is
// deliberately NOT marked alerted: the owner was never notified,
// so a restart with an owner configured must still be able to
// fire the alert.
func (a *App) maybeAlertBudget(ctx context.Context, chatID int64) {
	if a == nil || a.Budget == nil || a.budgetAlertNotifier == nil {
		return
	}
	used, threshold, crossed := a.Budget.CheckAlert(chatID)
	if !crossed {
		return
	}
	// F-A19-6: a permanent "no owner" failure is logged once per
	// chat and never retried — if this chat already hit it, stay
	// quiet instead of calling the notifier (and logging) again.
	a.budgetAlertNoOwnerMu.Lock()
	alreadyLogged := a.budgetAlertNoOwnerLogged[chatID]
	a.budgetAlertNoOwnerMu.Unlock()
	if alreadyLogged {
		return
	}
	text := FormatBudgetAlertMessage(chatID, used, threshold, a.Budget.Limit())
	if err := a.budgetAlertNotifier(ctx, chatID, text); err != nil {
		if errors.Is(err, ErrBudgetAlertNoOwner) {
			a.budgetAlertNoOwnerMu.Lock()
			if a.budgetAlertNoOwnerLogged == nil {
				a.budgetAlertNoOwnerLogged = make(map[int64]bool)
			}
			a.budgetAlertNoOwnerLogged[chatID] = true
			a.budgetAlertNoOwnerMu.Unlock()
			if a.Logger != nil {
				a.Logger.Printf("app: chat %d: budget alert not delivered (used %d, threshold %d): no owner configured; will not retry until restart", chatID, used, threshold)
			}
			return
		}
		if a.Logger != nil {
			a.Logger.Printf("app: chat %d: budget alert not delivered (used %d, threshold %d): %v", chatID, used, threshold, err)
		}
		return
	}
	if err := a.Budget.MarkAlerted(chatID); err != nil && a.Logger != nil {
		a.Logger.Printf("app: chat %d: budget alert delivered but could not persist the alerted flag: %v", chatID, err)
	}
}
