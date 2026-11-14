// Package version holds the single shared Orion version string, so
// cmd/orion and cmd/oriond (and any future binary) cannot drift apart.
package version

// Version is the Orion release version, in semantic-version form.
//
// It is a var, not a const, so the build can set it from the git tag
// with -ldflags -X (see the Makefile and install.sh): the tag is the
// source of truth at release time. The value here is the fallback
// for an untagged build — the version of the release this source
// tree is working towards — so a build from a commit between tags
// still reports a sane version, never "dev" and never a stale tag
// from an earlier release. Release checklist (CHANGELOG.md):
// `orion version` must match the tag before any push.
var Version = "0.2.0-rc24"

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
