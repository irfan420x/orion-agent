package version

import (
	"regexp"
	"testing"
)

var semverPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

func TestVersionIsSemver(t *testing.T) {
	if Version == "" {
		t.Fatal("Version must not be empty")
	}
	if !semverPattern.MatchString(Version) {
		t.Fatalf("Version %q is not semantic-version shaped (want e.g. 0.1.0)", Version)
	}
}

// TestStringWithoutLdflags checks that a binary built without ldflags
// (as in `go test` and `go build` with no -X flags) reports just the
// version, with no placeholder commit or date.
func TestStringWithoutLdflags(t *testing.T) {
	if got := String(); got != Version {
		t.Fatalf("String() = %q, want %q (no ldflags in tests)", got, Version)
	}
}

// TestStringWithLdflags checks the full build description a binary
// built by install.sh or the Makefile reports.
func TestStringWithLdflags(t *testing.T) {
	oldCommit, oldDate := Commit, BuildDate
	defer func() { Commit, BuildDate = oldCommit, oldDate }()
	Commit, BuildDate = "abc1234", "2026-11-13T05:00:00Z"
	want := "0.1.0 commit abc1234 built 2026-11-13T05:00:00Z"
	if got := String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
