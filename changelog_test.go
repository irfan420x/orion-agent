// TestChangelogFixBatchEntries (F-A22-20): the CHANGELOG is the last
// unpinned manual step — no test or make target read it, so the F1
// section shipped without F-A21-6 (split-proposal refresh) and
// F-A21-16 (subagent-gating note), the same silent-staleness failure
// mode F-A21-5 closed for the README list. This test fails if a
// finding ID fixed in this batch is missing from the Unreleased
// section.
//
// Deliberately light, in the shape the open-source research found
// for generated-doc pinning (§3: enumerate-and-compare, not README
// parsing — there is no established pattern for parsing hand-written
// prose counts in a test, and this gate does not parse prose at
// all): it matches literal finding IDs (e.g. "F-A22-3") as
// substrings of the Unreleased section only, so the wording around
// an entry can change freely and a stale historical mention
// elsewhere in the file cannot satisfy it.
//
// Scope and limits, stated plainly:
//   - Only the DOCS findings of this batch are pinned here
//     (F-A22-3, F-A22-10, F-A22-11, F-A22-12, F-A22-17, F-A22-20),
//     plus the two F1 omissions this batch adds (F-A21-6,
//     F-A21-16). Findings fixed by other workers are their
//     sections' responsibility — this test must not fail on work
//     it does not own.
//   - Presence, not prose: the test proves the ID is recorded, not
//     that the entry describes the fix correctly. A wrong-but-present
//     entry still needs a human reader.
//   - Unreleased-scoped: when Unreleased is released (moved into a
//     versioned section), this test's section must move with the
//     entries, or it will fail on entries that are present but
//     no longer under Unreleased.
package orion_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChangelogFixBatchEntries(t *testing.T) {
	repoRoot := installTestRepoRoot(t)
	raw, err := os.ReadFile(filepath.Join(repoRoot, "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("read CHANGELOG.md: %v", err)
	}
	text := string(raw)

	// The Unreleased section: from "## [Unreleased]" to the next
	// "## " heading (or end of file). The F1 and fix-batch
	// sections both live here.
	start := strings.Index(text, "## [Unreleased]")
	if start < 0 {
		t.Fatal("CHANGELOG.md: no \"## [Unreleased]\" section found")
	}
	rest := text[start+len("## [Unreleased]"):]
	end := strings.Index(rest, "\n## ")
	unreleased := rest
	if end >= 0 {
		unreleased = rest[:end]
	}

	// Finding IDs this batch adds to the CHANGELOG: the two F1
	// omissions (F-A22-20) and the six DOCS findings of the
	// Audit 22 fix batch itself.
	want := []string{
		"F-A21-6", "F-A21-16",
		"F-A22-3", "F-A22-10", "F-A22-11",
		"F-A22-12", "F-A22-17", "F-A22-20",
	}
	var missing []string
	for _, id := range want {
		if !strings.Contains(unreleased, id) {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		t.Errorf("CHANGELOG.md Unreleased section is missing finding IDs %q (a fixed finding with no entry ships silently — add the entry, F-A22-20)", missing)
	}
}
