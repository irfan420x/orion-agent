// Package memory — relative-date conversion (Memory v2).
//
// Project memories convert relative dates to absolute dates on save
// ("Thursday" → 2026-11-19), so a memory stays true after the week
// it was written in has passed. The converter handles the common
// English forms:
//
//   - today, tomorrow, yesterday
//   - weekday names ("Thursday" — the next occurrence, today if today
//     is that weekday)
//   - "next Thursday" / "last Thursday" — that weekday in the next /
//     previous calendar week (Monday–Sunday)
//   - "next week" / "last week" — the Monday of the next / previous week
//   - "in N days", "in N weeks", "N days ago", "N weeks ago"
//
// Dates are rendered as YYYY-MM-DD. Unknown forms are left untouched,
// never guessed. The reference date is the save date.
package memory

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// weekdayNames maps lowercase weekday names to time.Weekday.
var weekdayNames = map[string]time.Weekday{
	"monday":    time.Monday,
	"tuesday":   time.Tuesday,
	"wednesday": time.Wednesday,
	"thursday":  time.Thursday,
	"friday":    time.Friday,
	"saturday":  time.Saturday,
	"sunday":    time.Sunday,
}

// relativeDateRe matches the relative-date forms ConvertRelativeDates
// understands. Alternatives are ordered most specific first, so "next
// Thursday" matches as a qualified weekday before "Thursday" could
// match on its own, and "next week" is tried before a bare weekday.
var relativeDateRe = regexp.MustCompile(`(?i)\b(?:` +
	`(?P<rel>next|last)\s+(?P<relDay>monday|tuesday|wednesday|thursday|friday|saturday|sunday)` +
	`|in\s+(?P<inN>\d+)\s+(?P<inUnit>days?|weeks?)` +
	`|(?P<agoN>\d+)\s+(?P<agoUnit>days?|weeks?)\s+ago` +
	`|(?P<day>monday|tuesday|wednesday|thursday|friday|saturday|sunday)` +
	`|(?P<today>today|tomorrow|yesterday)` +
	`|(?P<relWeek>next|last)\s+week` +
	`)\b`)

// ConvertRelativeDates rewrites relative dates in body as absolute
// dates (YYYY-MM-DD) relative to ref. The time of day of ref is
// ignored; only its calendar date matters. Forms the converter does
// not recognise are left untouched.
func ConvertRelativeDates(body string, ref time.Time) string {
	base := time.Date(ref.Year(), ref.Month(), ref.Day(), 0, 0, 0, 0, ref.Location())
	return relativeDateRe.ReplaceAllStringFunc(body, func(match string) string {
		sub := relativeDateRe.FindStringSubmatch(match)
		groups := make(map[string]string)
		for i, name := range relativeDateRe.SubexpNames() {
			if name != "" && i < len(sub) {
				groups[name] = sub[i]
			}
		}
		var resolved time.Time
		var ok bool
		var prefix string
		switch {
		case groups["rel"] != "" && groups["relDay"] != "":
			resolved, ok = qualifiedWeekday(base, groups["rel"], weekdayNames[strings.ToLower(groups["relDay"])])
		case groups["inN"] != "":
			resolved, ok = shiftDays(base, groups["inN"], groups["inUnit"], +1)
			// The match includes the leading "in" ("In 3 days"):
			// keep it so "In 3 days we deploy" becomes
			// "In 2026-11-16 we deploy", not "2026-11-16 we deploy".
			if idx := strings.Index(match, groups["inN"]); idx > 0 {
				prefix = strings.TrimSpace(match[:idx]) + " "
			}
		case groups["agoN"] != "":
			resolved, ok = shiftDays(base, groups["agoN"], groups["agoUnit"], -1)
		case groups["day"] != "":
			resolved, ok = nextWeekdayOnOrAfter(base, weekdayNames[strings.ToLower(groups["day"])])
		case groups["today"] != "":
			resolved, ok = relativeDay(base, strings.ToLower(groups["today"]))
		case groups["relWeek"] != "":
			resolved, ok = weekMonday(base, strings.ToLower(groups["relWeek"]))
		}
		if !ok {
			return match // never guess: leave unknown forms untouched
		}
		return prefix + resolved.Format("2006-01-02")
	})
}

// nextWeekdayOnOrAfter returns the next occurrence of target on or
// after base (today itself when base already is that weekday).
func nextWeekdayOnOrAfter(base time.Time, target time.Weekday) (time.Time, bool) {
	delta := (int(target) - int(base.Weekday()) + 7) % 7
	return base.AddDate(0, 0, delta), true
}

// qualifiedWeekday resolves "next <weekday>" / "last <weekday>" to
// that weekday in the next / previous calendar week (Monday–Sunday).
// So from Friday 2026-11-13, "next Thursday" is 2026-11-19 (next
// week's Thursday), not the Thursday that just passed.
func qualifiedWeekday(base time.Time, rel string, target time.Weekday) (time.Time, bool) {
	// ISO weekday: Monday=1 .. Sunday=7.
	iso := func(w time.Weekday) int {
		if w == time.Sunday {
			return 7
		}
		return int(w)
	}
	monday := base.AddDate(0, 0, -(iso(base.Weekday()) - 1))
	switch strings.ToLower(rel) {
	case "next":
		return monday.AddDate(0, 0, 7+(iso(target)-1)), true
	case "last":
		return monday.AddDate(0, 0, -7+(iso(target)-1)), true
	}
	return time.Time{}, false
}

// weekMonday resolves "next week" / "last week" to the Monday of the
// next / previous calendar week.
func weekMonday(base time.Time, rel string) (time.Time, bool) {
	iso := func(w time.Weekday) int {
		if w == time.Sunday {
			return 7
		}
		return int(w)
	}
	monday := base.AddDate(0, 0, -(iso(base.Weekday()) - 1))
	switch rel {
	case "next":
		return monday.AddDate(0, 0, 7), true
	case "last":
		return monday.AddDate(0, 0, -7), true
	}
	return time.Time{}, false
}

// relativeDay resolves today / tomorrow / yesterday.
func relativeDay(base time.Time, word string) (time.Time, bool) {
	switch word {
	case "today":
		return base, true
	case "tomorrow":
		return base.AddDate(0, 0, 1), true
	case "yesterday":
		return base.AddDate(0, 0, -1), true
	}
	return time.Time{}, false
}

// shiftDays resolves "in N days/weeks" (dir +1) and "N days/weeks ago"
// (dir -1).
func shiftDays(base time.Time, nStr, unit string, dir int) (time.Time, bool) {
	n, err := strconv.Atoi(nStr)
	if err != nil {
		return time.Time{}, false
	}
	days := n
	if strings.HasPrefix(strings.ToLower(unit), "week") {
		days = n * 7
	}
	return base.AddDate(0, 0, dir*days), true
}
