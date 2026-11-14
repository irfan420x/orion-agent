//go:build !race

package messaging

import (
	"testing"
	"time"
)

// raceTimeoutMultiplier is 1 on plain builds: test wait deadlines run at
// their written values. Under -race the companion file
// test_timeout_race_test.go sets it to 5 (CockroachDB's verified 5x
// race scaling for SucceedsSoon-style waits, F-A22-5/F-A22-4).
const raceTimeoutMultiplier = 1

// TestRaceTimeoutMultiplier pins the plain-build scaling: no multiplier
// outside -race, so wait deadlines run at their written values.
func TestRaceTimeoutMultiplier(t *testing.T) {
	if raceTimeoutMultiplier != 1 {
		t.Fatalf("raceTimeoutMultiplier = %d, want 1 on a plain build", raceTimeoutMultiplier)
	}
	if got := scaledTimeout(5 * time.Second); got != 5*time.Second {
		t.Fatalf("scaledTimeout(5s) = %v, want 5s on a plain build", got)
	}
}
