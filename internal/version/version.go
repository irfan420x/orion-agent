// Package version holds the single shared Orion version string, so
// cmd/orion and cmd/oriond (and any future binary) cannot drift apart.
package version

// Version is the Orion release version, in semantic-version form.
const Version = "0.2.0-rc5"

// Commit and BuildDate are set at build time with -ldflags -X
// (install.sh and the Makefile do this). Built without ldflags they
// keep their defaults, and String reports just the version.
var (
	Commit    = "dev"
	BuildDate = "unknown"
)

// String returns the full build description: the version, plus the
// VCS commit and build date when the binary was built with ldflags.
func String() string {
	s := Version
	if Commit != "" && Commit != "dev" && Commit != "unknown" {
		s += " commit " + Commit
	}
	if BuildDate != "" && BuildDate != "unknown" {
		s += " built " + BuildDate
	}
	return s
}
