// Tests that run the REAL vendored Python helpers with a stubbed
// `gi` module (F-M2R-23, F-M2R-28): the helpers' control flow —
// retries, parsing, failure paths — is exercised without a live
// desktop. The stub lives in a temp dir on PYTHONPATH and replaces
// gi.repository.Atspi (and, for the KWin helper, Gio/GLib) with
// fake objects whose failures are driven by environment variables.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stubAtspiSource is a fake gi.repository.Atspi: one desktop, one
// app (Firefox), one window ("Test Window") with a button and an
// entry. Failures are injected by environment:
//
//   ATSTUB_FAIL_DESKTOP_FIRST=1 — get_desktop raises once, then works.
//   ATSTUB_FAIL_DESKTOP_ALWAYS=1 — get_desktop always raises.
//   ATSTUB_FAIL_DESKTOP_COUNT_ALWAYS=1 — get_desktop_count always raises.
//   ATSTUB_FAIL_CHILD_COUNT_ALWAYS=1 — the desktop's get_child_count always raises.
//   ATSTUB_FAIL_STATE=1 — the entry's get_state_set raises (showing unknown).
const stubAtspiSource = `
import os

class CoordType:
    SCREEN = 0

class StateType:
    SHOWING = 0
    FOCUSED = 1

class _Extents:
    def __init__(self, x, y, w, h):
        self.x, self.y, self.width, self.height = x, y, w, h

class _StateSet:
    def __init__(self, states):
        self._states = states
    def contains(self, st):
        return st in self._states

class _Component:
    def __init__(self, box):
        self._box = box
    def get_extents(self, _ct):
        return _Extents(*self._box)

class _Node:
    def __init__(self, name, role, box=None, children=None, showing=True, focused=False, fail_state=False):
        self._name = name
        self._role = role
        self._box = box
        self._children = children or []
        self._showing = showing
        self._focused = focused
        self._fail_state = fail_state
    def get_name(self):
        return self._name
    def get_role_name(self):
        return self._role
    def get_component_iface(self):
        if self._box is None:
            return None
        return _Component(self._box)
    def get_state_set(self):
        if self._fail_state:
            raise Exception("flaky D-Bus: get_state_set failed")
        states = set()
        if self._showing:
            states.add(StateType.SHOWING)
        if self._focused:
            states.add(StateType.FOCUSED)
        return _StateSet(states)
    def get_child_count(self):
        return len(self._children)
    def get_child_at_index(self, i):
        return self._children[i]

def _build_tree():
    button = _Node("OK", "push button", (100, 100, 80, 30))
    entry = _Node("Name", "entry", (100, 150, 200, 30),
                  fail_state=(os.environ.get("ATSTUB_FAIL_STATE") == "1"))
    window = _Node("Test Window", "frame", (0, 0, 800, 600),
                   children=[button, entry], focused=True)
    app = _Node("Firefox", "application", None, children=[window])
    return _Node("desktop", "desktop", None, children=[app])

_DESKTOP = _build_tree()
_CALLS = {"desktop": 0}

class Atspi:
    # Real gi nests the enums under the Atspi namespace
    # (Atspi.CoordType.SCREEN, Atspi.StateType.SHOWING).
    CoordType = CoordType
    StateType = StateType

    @staticmethod
    def init():
        pass
    @staticmethod
    def get_desktop_count():
        if os.environ.get("ATSTUB_FAIL_DESKTOP_COUNT_ALWAYS") == "1":
            raise Exception("flaky D-Bus: get_desktop_count failed")
        return 1
    @staticmethod
    def get_desktop(d):
        _CALLS["desktop"] += 1
        if os.environ.get("ATSTUB_FAIL_DESKTOP_ALWAYS") == "1":
            raise Exception("flaky D-Bus: get_desktop failed")
        if os.environ.get("ATSTUB_FAIL_DESKTOP_FIRST") == "1" and _CALLS["desktop"] == 1:
            raise Exception("flaky D-Bus: get_desktop failed once")
        return _DESKTOP

# The desktop's child count is a method on the _Node instance, so
# the always-fail flag is consulted there via a wrapper installed
# on the built tree.
_orig_child_count = _Node.get_child_count
def _child_count(self):
    if self._name == "desktop" and os.environ.get("ATSTUB_FAIL_CHILD_COUNT_ALWAYS") == "1":
        raise Exception("flaky D-Bus: get_child_count failed")
    return _orig_child_count(self)
_Node.get_child_count = _child_count
`

// writeStubGi writes a stub `gi` package (require_version + the
// given repository source) under dir and returns dir for PYTHONPATH.
func writeStubGi(t *testing.T, dir, repositorySource string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "gi", "repository"), 0700); err != nil {
		t.Fatalf("mkdir stub gi: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gi", "__init__.py"), []byte("def require_version(_name, _ver):\n    pass\n"), 0600); err != nil {
		t.Fatalf("write stub gi __init__: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gi", "repository", "__init__.py"), []byte(repositorySource), 0600); err != nil {
		t.Fatalf("write stub gi repository: %v", err)
	}
	return dir
}

// runRealHelper writes helperSource to a temp file and runs it
// with python3 under the stub PYTHONPATH, returning stdout,
// stderr and the run error. extraEnv sets failure-injection flags.
func runRealHelper(t *testing.T, helperSource string, args []string, stubDir string, extraEnv map[string]string) (string, string, error) {
	t.Helper()
	work := t.TempDir()
	helperPath := filepath.Join(work, "helper.py")
	if err := os.WriteFile(helperPath, []byte(helperSource), 0600); err != nil {
		t.Fatalf("write helper: %v", err)
	}
	cmdArgs := append([]string{helperPath}, args...)
	cmd := exec.CommandContext(context.Background(), "python3", cmdArgs...)
	cmd.Env = append(os.Environ(), "PYTHONPATH="+stubDir)
	for k, v := range extraEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// parseAtspiOutput parses the helper's JSON document.
func parseAtspiOutput(t *testing.T, stdout string) atspiOutput {
	t.Helper()
	var out atspiOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("helper stdout %q does not parse: %v", stdout, err)
	}
	return out
}

// TestRealAtspiHelperFlakyDesktopRetry pins F-M2R-23: a get_desktop
// call that fails once (flaky D-Bus) is retried, and the walk
// still returns the whole tree instead of losing it.
func TestRealAtspiHelperFlakyDesktopRetry(t *testing.T) {
	stubDir := writeStubGi(t, t.TempDir(), stubAtspiSource)
	stdout, stderr, err := runRealHelper(t, atspiHelperSource, []string{"12", "1000"}, stubDir,
		map[string]string{"ATSTUB_FAIL_DESKTOP_FIRST": "1"})
	if err != nil {
		t.Fatalf("helper failed on a once-flaky get_desktop: %v (stderr: %s)", err, stderr)
	}
	out := parseAtspiOutput(t, stdout)
	if len(out.Windows) != 1 || out.Windows[0].Title != "Test Window" {
		t.Errorf("windows = %+v, want the Test Window", out.Windows)
	}
	names := map[string]bool{}
	for _, el := range out.Elements {
		names[el.Name] = true
	}
	if !names["OK"] || !names["Name"] {
		t.Errorf("elements = %+v, want the OK button and the Name entry", out.Elements)
	}
}

// TestRealAtspiHelperFlakyChildCountSkip pins the F-M2R-23 skip
// path: a desktop whose child count keeps failing is skipped, and
// the helper still exits 0 with a (partial but valid) document
// instead of dying on the whole tree.
func TestRealAtspiHelperFlakyChildCountSkip(t *testing.T) {
	stubDir := writeStubGi(t, t.TempDir(), stubAtspiSource)
	stdout, stderr, err := runRealHelper(t, atspiHelperSource, []string{"12", "1000"}, stubDir,
		map[string]string{"ATSTUB_FAIL_CHILD_COUNT_ALWAYS": "1"})
	if err != nil {
		t.Fatalf("helper failed on a failing child count: %v (stderr: %s)", err, stderr)
	}
	out := parseAtspiOutput(t, stdout)
	if len(out.Windows) != 0 || len(out.Elements) != 0 {
		t.Errorf("skipped desktop still contributed windows=%d elements=%d, want 0/0", len(out.Windows), len(out.Elements))
	}
}

// TestRealAtspiHelperDesktopCountFails pins the F-M2R-23 honesty
// boundary: when even the desktop count cannot be read, there is
// no tree to salvage — the helper exits non-zero with the error
// on stderr and NO JSON on stdout, so the Go caller degrades to
// vision_only instead of grounding on partial data.
func TestRealAtspiHelperDesktopCountFails(t *testing.T) {
	stubDir := writeStubGi(t, t.TempDir(), stubAtspiSource)
	stdout, stderr, err := runRealHelper(t, atspiHelperSource, []string{"12", "1000"}, stubDir,
		map[string]string{"ATSTUB_FAIL_DESKTOP_COUNT_ALWAYS": "1"})
	if err == nil {
		t.Fatalf("helper succeeded with stdout %q, want a non-zero exit", stdout)
	}
	if !strings.Contains(stderr, "get_desktop_count failed") {
		t.Errorf("stderr = %q, want the get_desktop_count failure", stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout = %q, want empty (no JSON on failure)", stdout)
	}
}

// TestRealAtspiHelperShowingUnknown pins the F-M2R-21 helper side:
// an element whose SHOWING query fails is still listed (without
// a showing field) and counted in showing_unknown.
func TestRealAtspiHelperShowingUnknown(t *testing.T) {
	stubDir := writeStubGi(t, t.TempDir(), stubAtspiSource)
	stdout, stderr, err := runRealHelper(t, atspiHelperSource, []string{"12", "1000"}, stubDir,
		map[string]string{"ATSTUB_FAIL_STATE": "1"})
	if err != nil {
		t.Fatalf("helper failed: %v (stderr: %s)", err, stderr)
	}
	out := parseAtspiOutput(t, stdout)
	if out.ShowingUnknown != 1 {
		t.Errorf("showing_unknown = %d, want 1 (the entry whose state query failed)", out.ShowingUnknown)
	}
	found := false
	for _, el := range out.Elements {
		if el.Name == "Name" {
			found = true
			if el.Showing != nil {
				t.Errorf("entry showing = %v, want nil (query failed)", *el.Showing)
			}
		}
	}
	if !found {
		t.Errorf("entry missing from elements: %+v", out.Elements)
	}
}

// TestRealAtspiHelperCleanRun pins the stub itself: with no
// failure injected the real helper returns the full document, so
// a red test here means the harness (not the helper) broke.
func TestRealAtspiHelperCleanRun(t *testing.T) {
	stubDir := writeStubGi(t, t.TempDir(), stubAtspiSource)
	stdout, stderr, err := runRealHelper(t, atspiHelperSource, []string{"12", "1000"}, stubDir, nil)
	if err != nil {
		t.Fatalf("helper failed on a clean run: %v (stderr: %s)", err, stderr)
	}
	out := parseAtspiOutput(t, stdout)
	if len(out.Windows) != 1 || out.Windows[0].Title != "Test Window" || out.Windows[0].App != "Firefox" {
		t.Errorf("windows = %+v, want the Firefox Test Window", out.Windows)
	}
	if out.FocusedWindow == nil || out.FocusedWindow.Title != "Test Window" {
		t.Errorf("focused_window = %+v, want the Test Window", out.FocusedWindow)
	}
	if out.ShowingUnknown != 0 {
		t.Errorf("showing_unknown = %d, want 0 on a clean run", out.ShowingUnknown)
	}
	if out.WalkedNodes < 3 {
		t.Errorf("walked_nodes = %d, want >= 3 (window + button + entry)", out.WalkedNodes)
	}
}
