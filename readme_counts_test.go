// TestReadmeCounts (F-A21-10): the README test counts used to be
// pinned only by `make check-readme-counts`, which runs only from
// `make release-check` — a normal `go test ./...` never failed on
// stale counts. This test runs in the normal suite and pins both
// README count statements (the banner and the ## Tests section)
// to the suite itself:
//
//   - the total comes from `go test -count=1 -list '.*' ./...`,
//     which lists every test, example and fuzz target without
//     running any of them. The count matches the `-v` recount
//     exactly: `-list` names only top-level tests (subtests are
//     indented `--- PASS` lines in -v output, which the Makefile
//     grep never counted), and the one fuzz target reports a
//     `--- PASS` line like any test.
//   - fail must be 0 (the suite this test runs in is expected
//     green — a red suite fails `go test` anyway, so a README
//     claiming failures would be stale by definition).
//   - skip is pinned through the known-skip set: the four tests
//     that skip by design (no ollama binary, opt-in live E2E,
//     the fake-MCP-server helper entry point). Each must still
//     be listed, and the README skip count must equal the set's
//     size — adding a new skip means updating the set here AND
//     the README, and either half alone fails loudly.
//   - pass is then total - fail - skip, so all four numbers in
//     both README statements are pinned, and the two statements
//     must agree with each other.
//
// Residual hole, stated plainly: if a known-skip test stops
// skipping (e.g. ollama gets installed, so TestOllamaSmokeIfAvailable
// passes), this test still sees its name in the list and cannot
// tell — only a full run (`make check-readme-counts`, which runs
// the suite and compares the real split) catches that. This test
// catches the common staleness — adding, removing or renaming a
// test without bumping the README — at suite time.
package orion_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// readmeCounts holds one "N total (P pass, F fail, S skip)" statement.
type readmeCounts struct {
	total, pass, fail, skip int
}

func parseReadmeCounts(t *testing.T, text, label, pattern string) readmeCounts {
	t.Helper()
	re := regexp.MustCompile(pattern)
	m := re.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("README.md: could not parse the %s test-count statement (format changed?)", label)
	}
	nums := make([]int, 4)
	for i, s := range m[1:] {
		s = strings.ReplaceAll(s, ",", "")
		n, err := strconv.Atoi(s)
		if err != nil {
			t.Fatalf("README.md %s count %q is not a number: %v", label, s, err)
		}
		nums[i] = n
	}
	return readmeCounts{total: nums[0], pass: nums[1], fail: nums[2], skip: nums[3]}
}

func TestReadmeCounts(t *testing.T) {
	repoRoot := installTestRepoRoot(t)
	raw, err := os.ReadFile(filepath.Join(repoRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	text := string(raw)

	// Both statements, in the exact shapes `make
	// check-readme-counts` parses (em-dash in the banner,
	// "passing/failing/skipped" under ## Tests).
	banner := parseReadmeCounts(t, text, "banner", `\(([0-9][0-9,]*) tests total — ([0-9][0-9,]*) pass, ([0-9][0-9,]*) fail, ([0-9][0-9,]*) skip`)
	section := parseReadmeCounts(t, text, "## Tests section", `([0-9][0-9,]*) tests total \(([0-9][0-9,]*) passing, ([0-9][0-9,]*) failing, ([0-9][0-9,]*) skipped\)`)
	if banner != section {
		t.Fatalf("README test counts disagree: banner says %+v, ## Tests says %+v (update both)", banner, section)
	}

	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH: cannot list the suite")
	}
	cmd := exec.Command(goPath, "test", "-count=1", "-list", ".*", "./...")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go test -list ./...: %v\n%s", err, out)
	}
	listed := map[string]bool{}
	total := 0
	nameRe := regexp.MustCompile(`^(Test|Example|Fuzz)[A-Za-z0-9_]*$`)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if nameRe.MatchString(line) {
			listed[line] = true
			total++
		}
	}
	if total == 0 {
		t.Fatalf("go test -list ./... listed no tests (output:\n%s)", out)
	}

	// The tests that skip by design (see Audit 21 §4 for the
	// inventory): each must still exist in the listed set, and
	// the README skip count must equal exactly this set.
	knownSkips := []string{
		"TestOllamaSmokeIfAvailable", // no ollama binary on this host
		"TestLiveOllamaAgentE2E",     // opt-in live E2E (ORION_LIVE_OLLAMA_E2E=1)
		"TestLiveOllamaMultiTurnE2E", // opt-in live E2E (ORION_LIVE_OLLAMA_E2E=1)
		"TestFakeMCPServerProcess",   // helper entry point, skips unless ORION_TEST_FAKE_MCP_SERVER=1
	}
	for _, name := range knownSkips {
		if !listed[name] {
			t.Errorf("known-skip test %q is no longer listed by go test -list (renamed/removed?): update the known-skip set in TestReadmeCounts and the README counts", name)
		}
	}
	if banner.total != total {
		t.Errorf("README says %d tests total, but go test -list counts %d (a test was added/removed/renamed without bumping the README)", banner.total, total)
	}
	if banner.fail != 0 {
		t.Errorf("README says %d failing tests, want 0 (this suite is expected green)", banner.fail)
	}
	if banner.skip != len(knownSkips) {
		t.Errorf("README says %d skipped, want %d (the known-skip set in TestReadmeCounts)", banner.skip, len(knownSkips))
	}
	if wantPass := total - banner.fail - banner.skip; banner.pass != wantPass {
		t.Errorf("README says %d passing, want %d (%d total - %d fail - %d skip)", banner.pass, wantPass, total, banner.fail, banner.skip)
	}
}
