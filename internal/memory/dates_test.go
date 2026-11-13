package memory

import (
	"testing"
	"time"
)

// ref is Friday 2026-11-13, the date this package was written.
var refFriday = time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)

func TestConvertRelativeDatesBasic(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Meet today.", "Meet 2026-11-13."},
		{"Meet tomorrow.", "Meet 2026-11-14."},
		{"It happened yesterday.", "It happened 2026-11-12."},
		{"Ship on Thursday.", "Ship on 2026-11-19."},
		{"Ship on Friday.", "Ship on 2026-11-13."}, // today is Friday
		{"Ship on Monday.", "Ship on 2026-11-16."},
		{"Ship next Thursday.", "Ship 2026-11-19."},
		{"Ship last Thursday.", "Ship 2026-11-05."},
		{"Due next week.", "Due 2026-11-16."}, // Monday of next week
		{"Due last week.", "Due 2026-11-02."}, // Monday of last week
		{"In 3 days we deploy.", "In 2026-11-16 we deploy."},
		{"In 2 weeks we deploy.", "In 2026-11-27 we deploy."},
		{"It broke 5 days ago.", "It broke 2026-11-08."},
		{"It broke 1 week ago.", "It broke 2026-11-06."},
		{"Tomorrow and Thursday.", "2026-11-14 and 2026-11-19."},
		{"No dates here at all.", "No dates here at all."},
		{"Already absolute: 2026-03-05.", "Already absolute: 2026-03-05."},
	}
	for _, c := range cases {
		if got := ConvertRelativeDates(c.in, refFriday); got != c.want {
			t.Errorf("ConvertRelativeDates(%q): got %q want %q", c.in, got, c.want)
		}
	}
}

func TestConvertRelativeDatesUnknownLeftUntouched(t *testing.T) {
	// Forms the converter does not know are never guessed.
	cases := []string{
		"Meet sometime soon.",
		"Ship in a few days.", // "a few" is not a number
		"Due next month.",
	}
	for _, c := range cases {
		if got := ConvertRelativeDates(c, refFriday); got != c {
			t.Errorf("ConvertRelativeDates(%q): got %q, want untouched", c, got)
		}
	}
}

func TestConvertRelativeDatesWeekBoundaries(t *testing.T) {
	// Monday 2026-11-16: "next Thursday" is the Thursday of next
	// week (2026-11-26), not this week's (2026-11-19).
	monday := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	if got := ConvertRelativeDates("next Thursday", monday); got != "2026-11-26" {
		t.Errorf("next Thursday from Monday: got %q want 2026-11-26", got)
	}
	// Sunday 2026-11-15: "last Thursday" is 2026-11-05, "next week"
	// starts Monday 2026-11-16.
	sunday := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	if got := ConvertRelativeDates("last Thursday", sunday); got != "2026-11-05" {
		t.Errorf("last Thursday from Sunday: got %q want 2026-11-05", got)
	}
	if got := ConvertRelativeDates("next week", sunday); got != "2026-11-16" {
		t.Errorf("next week from Sunday: got %q want 2026-11-16", got)
	}
	// "Thursday" on a Thursday means today.
	thursday := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	if got := ConvertRelativeDates("Thursday", thursday); got != "2026-11-19" {
		t.Errorf("Thursday on a Thursday: got %q want 2026-11-19", got)
	}
}

func TestConvertRelativeDatesCaseInsensitive(t *testing.T) {
	if got := ConvertRelativeDates("TOMORROW we ship.", refFriday); got != "2026-11-14 we ship." {
		t.Errorf("uppercase: got %q", got)
	}
}
