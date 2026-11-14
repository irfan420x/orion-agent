//go:build race

package messaging

import (
	"testing"
	"time"
)

// raceTimeoutMultiplier lengthens test-only wait deadlines under -race
// (F-A22-5, F-A22-4). The race detector slows the test binary several-fold
// and full-suite contention on a small VM slows it further, so a wall-clock
// bound that is generous plain is tight under -race. This mirrors
// CockroachDB's verified pattern: DefaultSucceedsSoonDuration 45s,
// RaceSucceedsSoonDuration = 45s x 5, selected by a RaceEnabled flag —
// here the flag is the standard //go:build race tag, so no new dependency
// and no runtime detection. The multiplier applies ONLY to test wait
// deadlines (waitFor/waitForLong/stopPollStream and the result selects in
// the approval/offset tests); poll intervals and every assertion are
// unchanged, so a longer deadline never weakens a test — it only stops
// load from masquerading as a failure. See test_timeout_norace_test.go
// for the plain-build value (1).
const raceTimeoutMultiplier = 5

// TestRaceTimeoutMultiplier pins the -race scaling: the race build must
// select the 5x multiplier, and scaledTimeout must apply it. If this
// fails, the wrong build-tag file was selected and the flake fixes
// below it are silently plain-timed.
func TestRaceTimeoutMultiplier(t *testing.T) {
	if raceTimeoutMultiplier != 5 {
		t.Fatalf("raceTimeoutMultiplier = %d, want 5 under -race", raceTimeoutMultiplier)
	}
	if got := scaledTimeout(5 * time.Second); got != 25*time.Second {
		t.Fatalf("scaledTimeout(5s) = %v, want 25s under -race", got)
	}
}
