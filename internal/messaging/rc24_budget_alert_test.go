// rc24: /budget shows the daily usage alert threshold (F-A18-11)
// next to the limit, so the owner can see when they will be
// notified once per day.
package messaging

import (
	"strings"
	"testing"
)

// TestBudgetShowThreshold: /budget with a threshold wired shows
// the threshold line with the limit and today's usage.
func TestBudgetShowThreshold(t *testing.T) {
	s := newStubBudgetEnv()
	s.env.BudgetAlertThreshold = func() int { return 50000 }
	reply := dispatchBudget(t, s, nil, "/budget")
	for _, want := range []string{"100,000", "25,000", "75,000", "Alert threshold: 50,000 tokens", "once per day"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply missing %q:\n%s", want, reply)
		}
	}
}

// TestBudgetShowThresholdOff: a zero threshold (disabled) is
// shown as off, not as a number.
func TestBudgetShowThresholdOff(t *testing.T) {
	s := newStubBudgetEnv()
	s.env.BudgetAlertThreshold = func() int { return 0 }
	reply := dispatchBudget(t, s, nil, "/budget")
	if !strings.Contains(reply, "Alert threshold: off") {
		t.Errorf("reply missing \"Alert threshold: off\":\n%s", reply)
	}
}

// TestBudgetShowThresholdUnlimited: with an unlimited budget the
// threshold line still shows — the alert is what watches usage
// when nothing refuses turns.
func TestBudgetShowThresholdUnlimited(t *testing.T) {
	s := newStubBudgetEnv()
	s.limit = 0
	s.env.BudgetAlertThreshold = func() int { return 50000 }
	reply := dispatchBudget(t, s, nil, "/budget")
	for _, want := range []string{"unlimited", "Alert threshold: 50,000 tokens"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply missing %q:\n%s", want, reply)
		}
	}
}

// TestBudgetShowThresholdUnwired: when the threshold is not
// wired (hand-built envs) /budget omits the line instead of
// inventing a value — the pre-rc24 output is unchanged.
func TestBudgetShowThresholdUnwired(t *testing.T) {
	s := newStubBudgetEnv()
	reply := dispatchBudget(t, s, nil, "/budget")
	if strings.Contains(reply, "Alert threshold") {
		t.Errorf("reply must not mention the alert threshold when unwired:\n%s", reply)
	}
}
