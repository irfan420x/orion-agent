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
	"time"
)

// stubAtspiSource is a fake gi.repository.Atspi: one desktop, one
// app (Firefox), one window ("Test Window") with a button and an
// entry. Failures are injected by environment:
//
//	ATSTUB_FAIL_DESKTOP_FIRST=1 — get_desktop raises once, then works.
//	ATSTUB_FAIL_DESKTOP_ALWAYS=1 — get_desktop always raises.
//	ATSTUB_FAIL_DESKTOP_COUNT_ALWAYS=1 — get_desktop_count always raises.
//	ATSTUB_FAIL_CHILD_COUNT_ALWAYS=1 — the desktop's get_child_count always raises.
//	ATSTUB_FAIL_STATE=1 — the entry's get_state_set raises (showing unknown).
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

// --- KWin helper stub (F-M2R-24, F-M2R-28) ---

// stubKwinSource is a fake gi.repository with Gio/GLib for the KWin
// helper: a fake D-Bus proxy that records every call_sync in
// $KWINSTUB_RECORD (one method name per line), captures the
// geometry marker from the loaded JS into $KWINSTUB_DIR/marker,
// and fails on demand:
//
//	KWINSTUB_FAIL=start — the start call raises.
//	KWINSTUB_FAIL=proxy — creating the proxy raises.
const stubKwinSource = `
import json, os, re

class _Flags:
    NONE = 0

class _BusType:
    SESSION = 0

class GLib:
    @staticmethod
    def Variant(spec, value):
        return value

class _Proxy:
    def call_sync(self, method, *args):
        record = os.environ.get("KWINSTUB_RECORD", "")
        if record:
            with open(record, "a") as f:
                f.write(method + "\n")
        if method == "loadScript":
            variant = args[0]
            js_path, name = variant[0], variant[1]
            with open(js_path) as f:
                js = f.read()
            m = re.search(r"ORION-KWIN-GEOM-[0-9]+-[0-9a-f]+-[0-9]+:", js)
            marker = m.group(0) if m else ""
            d = os.environ.get("KWINSTUB_DIR", "")
            if d:
                with open(os.path.join(d, "marker"), "w") as f:
                    f.write(marker)
            return None
        if method == "start":
            if os.environ.get("KWINSTUB_FAIL") == "start":
                raise Exception("flaky D-Bus: start failed")
            return None
        if method == "unloadScript":
            return None
        return None

class _DBusProxy:
    @staticmethod
    def new_for_bus_sync(*args):
        if os.environ.get("KWINSTUB_FAIL") == "proxy":
            raise Exception("flaky D-Bus: cannot reach KWin scripting")
        return _Proxy()

class Gio:
    BusType = _BusType
    DBusProxyFlags = _Flags
    DBusCallFlags = _Flags
    DBusProxy = _DBusProxy
`

// fakeJournalctl installs a fake `journalctl` on PATH that prints
// one journal line carrying the marker recorded by the stub proxy
// plus the payload from $KWINSTUB_DIR/payload (a JSON window list).
// With an empty payload it prints an unrelated line (no marker),
// so the helper's "no geometry in the journal" path is exercised.
// It returns the temp dir holding the stub state (KWINSTUB_DIR)
// and the record file path (KWINSTUB_RECORD).
func fakeJournalctl(t *testing.T, payload string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatalf("mkdir fake bin: %v", err)
	}
	var script string
	if payload == "" {
		script = "#!/bin/sh\n" +
			"echo \"Nov 16 12:00:00 host plasma-kwin_wayland[123]: some other log line\"\n"
	} else {
		// The journal line carries the marker plus the payload
		// wrapped in the helper's current protocol (F-A23W2-11):
		// a dict with the run's nonce and kwin_windows — the
		// nonce is the marker minus its prefix and trailing
		// colon. The payload file holds just the window list.
		script = "#!/bin/sh\n" +
			"marker=$(cat \"$KWINSTUB_DIR/marker\" 2>/dev/null)\n" +
			"payload=$(cat \"$KWINSTUB_DIR/payload\" 2>/dev/null)\n" +
			"nonce=${marker#ORION-KWIN-GEOM-}\n" +
			"nonce=${nonce%:}\n" +
			"echo \"Nov 16 12:00:00 host plasma-kwin_wayland[123]: ${marker}{\\\"nonce\\\": \\\"${nonce}\\\", \\\"kwin_windows\\\": ${payload}}\"\n"
	}
	if err := os.WriteFile(filepath.Join(bin, "journalctl"), []byte(script), 0755); err != nil {
		t.Fatalf("write fake journalctl: %v", err)
	}
	if payload != "" {
		if err := os.WriteFile(filepath.Join(dir, "payload"), []byte(payload), 0600); err != nil {
			t.Fatalf("write fake payload: %v", err)
		}
	}
	return dir, filepath.Join(dir, "record")
}

// kwinStubEnv builds the environment for a real-KWin-helper run:
// stub gi on PYTHONPATH, fake journalctl first on PATH, stub state
// in dir, plus failure flags from extra.
func kwinStubEnv(t *testing.T, stubDir, stateDir, record string, extra map[string]string) map[string]string {
	t.Helper()
	env := map[string]string{
		"KWINSTUB_DIR":    stateDir,
		"KWINSTUB_RECORD": record,
		"PATH":            filepath.Join(stateDir, "bin") + ":" + os.Getenv("PATH"),
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// TestRealKWinHelperUnloadOnStartFailure pins F-M2R-24 (a): when
// start() throws, the loaded-never-started script must be
// unloaded before the helper exits — the record must show
// unloadScript after the failed start, and the helper must exit
// non-zero with no JSON on stdout.
func TestRealKWinHelperUnloadOnStartFailure(t *testing.T) {
	stubDir := writeStubGi(t, t.TempDir(), stubKwinSource)
	stateDir, record := fakeJournalctl(t, "")
	stdout, stderr, err := runRealHelper(t, kwinHelperSource, nil, stubDir,
		kwinStubEnv(t, stubDir, stateDir, record, map[string]string{"KWINSTUB_FAIL": "start"}))
	if err == nil {
		t.Fatalf("helper succeeded with stdout %q, want a non-zero exit on start failure", stdout)
	}
	if !strings.Contains(stderr, "loadScript/start failed") {
		t.Errorf("stderr = %q, want the loadScript/start failure", stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout = %q, want empty (no JSON on failure)", stdout)
	}
	calls, rerr := os.ReadFile(record)
	if rerr != nil {
		t.Fatalf("read call record: %v", rerr)
	}
	want := "loadScript\nstart\nunloadScript\n"
	if string(calls) != want {
		t.Errorf("D-Bus calls = %q, want %q (unload must follow the failed start)", string(calls), want)
	}
}

// TestRunHelperProcessKillsProcessGroup pins F-M2R-24 (c): when
// the helper times out, the whole process GROUP is killed — a
// spawned child (the journalctl stand-in) must not survive as an
// orphan.
func TestRunHelperProcessKillsProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not available — cannot prove the child is gone")
	}
	unique := "w-e-groupkill-" + filepath.Base(t.TempDir())
	script := "#!/bin/sh\npython3 -c \"import time; time.sleep(60)\" \"" + unique + "\" &\nsleep 60\n"
	work := t.TempDir()
	path := filepath.Join(work, "helper.sh")
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("write helper script: %v", err)
	}
	start := time.Now()
	_, err := runHelperProcess(context.Background(), path, nil, 200*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("runHelperProcess err = %v, want a timeout error", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("runHelperProcess took %v, want a prompt timeout kill", elapsed)
	}
	out, _ := exec.Command("pgrep", "-f", unique).Output()
	if len(bytes.TrimSpace(out)) > 0 {
		t.Errorf("orphaned helper child still running (pids %s) — the process group was not killed", strings.TrimSpace(string(out)))
	}
}

// TestRunHelperProcessSuccessAndFailure pins the shared runner's
// basic contract: stdout on success, stderr text on failure.
func TestRunHelperProcessSuccessAndFailure(t *testing.T) {
	out, err := runHelperProcess(context.Background(), "sh", []string{"-c", "echo hello"}, 5*time.Second)
	if err != nil {
		t.Fatalf("runHelperProcess(echo) = %v, want nil", err)
	}
	if string(out) != "hello\n" {
		t.Errorf("stdout = %q, want \"hello\\n\"", string(out))
	}
	_, err = runHelperProcess(context.Background(), "sh", []string{"-c", "echo oops >&2; exit 3"}, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "oops") {
		t.Errorf("failing helper err = %v, want the stderr text", err)
	}
}

// TestRealKWinHelperSuccessPath pins F-M2R-28: the real helper,
// driven by a stubbed KWin proxy and a fake journalctl carrying
// the marker + a geometry payload, prints the parsed window list
// — exercising the real script's load/start/unload sequencing,
// journal polling, and output parsing end to end.
func TestRealKWinHelperSuccessPath(t *testing.T) {
	stubDir := writeStubGi(t, t.TempDir(), stubKwinSource)
	payload := `[{"caption": "Sign in to GitHub · GitHub — Mozilla Firefox", "cls": "firefox-esr", "active": true, "minimized": false, "fg": [0, 29, 1920, 993], "cg": [0, 29, 1920, 993]}, {"caption": "Kate — notes.txt", "cls": "kate", "active": false, "minimized": false, "fg": [0, 0, 800, 600], "cg": [0, 0, 800, 600]}]`
	stateDir, record := fakeJournalctl(t, payload)
	stdout, stderr, err := runRealHelper(t, kwinHelperSource, nil, stubDir,
		kwinStubEnv(t, stubDir, stateDir, record, nil))
	if err != nil {
		t.Fatalf("helper failed on the success path: %v (stderr: %s)", err, stderr)
	}
	var out kwinOutput
	if jerr := json.Unmarshal([]byte(stdout), &out); jerr != nil {
		t.Fatalf("helper stdout %q does not parse: %v", stdout, jerr)
	}
	if len(out.Windows) != 2 {
		t.Fatalf("kwin_windows = %+v, want the 2 payload windows", out.Windows)
	}
	fx := out.Windows[0]
	// No caption assertion: the helper's JSON carries no titles
	// (F-M2R-15) — the payload's caption keys are ignored by the
	// parser, so the parsed window is checked on class, active
	// flag and geometry.
	if fx.Class != "firefox-esr" || !fx.Active || fx.FG != [4]int{0, 29, 1920, 993} || fx.CG != [4]int{0, 29, 1920, 993} {
		t.Errorf("firefox window = %+v, want the payload geometry", fx)
	}
	if out.Windows[1].Class != "kate" || out.Windows[1].FG != [4]int{0, 0, 800, 600} {
		t.Errorf("kate window = %+v, want the payload geometry", out.Windows[1])
	}
	calls, rerr := os.ReadFile(record)
	if rerr != nil {
		t.Fatalf("read call record: %v", rerr)
	}
	if want := "loadScript\nstart\nunloadScript\n"; string(calls) != want {
		t.Errorf("D-Bus calls = %q, want %q", string(calls), want)
	}
}

// TestRealKWinHelperMarkerUnique pins F-M2R-28: two runs must use
// different markers — the marker is what keeps one run's journal
// poll from reading another run's (stale) geometry.
func TestRealKWinHelperMarkerUnique(t *testing.T) {
	payload := `[{"caption": "Firefox", "cls": "firefox", "active": true, "minimized": false, "fg": [0, 0, 1920, 1080], "cg": [0, 0, 1920, 1080]}]`
	markers := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		stubDir := writeStubGi(t, t.TempDir(), stubKwinSource)
		stateDir, record := fakeJournalctl(t, payload)
		if _, _, err := runRealHelper(t, kwinHelperSource, nil, stubDir,
			kwinStubEnv(t, stubDir, stateDir, record, nil)); err != nil {
			t.Fatalf("run %d failed: %v", i+1, err)
		}
		marker, err := os.ReadFile(filepath.Join(stateDir, "marker"))
		if err != nil {
			t.Fatalf("run %d: read marker: %v", i+1, err)
		}
		markers = append(markers, string(marker))
	}
	if markers[0] == "" || markers[0] == markers[1] {
		t.Errorf("markers = %q, want two distinct non-empty markers", markers)
	}
}

// TestRealKWinHelperJournalPolls pins the journal-poll loop: the
// fake journalctl prints nothing until its third invocation, so
// the helper must keep polling instead of failing on the first
// empty read.
func TestRealKWinHelperJournalPolls(t *testing.T) {
	stubDir := writeStubGi(t, t.TempDir(), stubKwinSource)
	payload := `[{"caption": "Firefox", "cls": "firefox", "active": true, "minimized": false, "fg": [0, 0, 1920, 1080], "cg": [0, 0, 1920, 1080]}]`
	stateDir, record := fakeJournalctl(t, payload)
	// Replace the fake journalctl with a counting one: silent
	// for the first two calls, marker line from the third.
	script := "#!/bin/sh\n" +
		"n=$(cat \"$KWINSTUB_DIR/count\" 2>/dev/null || echo 0)\n" +
		"n=$((n + 1))\n" +
		"echo \"$n\" > \"$KWINSTUB_DIR/count\"\n" +
		"if [ \"$n\" -lt 3 ]; then exit 0; fi\n" +
		"marker=$(cat \"$KWINSTUB_DIR/marker\" 2>/dev/null)\n" +
		"payload=$(cat \"$KWINSTUB_DIR/payload\" 2>/dev/null)\n" +
		"nonce=${marker#ORION-KWIN-GEOM-}\n" +
		"nonce=${nonce%:}\n" +
		"echo \"Nov 16 12:00:00 host plasma-kwin_wayland[123]: ${marker}{\\\"nonce\\\": \\\"${nonce}\\\", \\\"kwin_windows\\\": ${payload}}\"\n"
	if err := os.WriteFile(filepath.Join(stateDir, "bin", "journalctl"), []byte(script), 0755); err != nil {
		t.Fatalf("write counting journalctl: %v", err)
	}
	stdout, stderr, err := runRealHelper(t, kwinHelperSource, nil, stubDir,
		kwinStubEnv(t, stubDir, stateDir, record, nil))
	if err != nil {
		t.Fatalf("helper failed despite the marker arriving on poll 3: %v (stderr: %s)", err, stderr)
	}
	var out kwinOutput
	if jerr := json.Unmarshal([]byte(stdout), &out); jerr != nil || len(out.Windows) != 1 {
		t.Fatalf("helper stdout %q, want the 1 payload window: %v", stdout, jerr)
	}
	count, _ := os.ReadFile(filepath.Join(stateDir, "count"))
	if strings.TrimSpace(string(count)) != "3" {
		t.Errorf("journalctl invocations = %q, want 3 (marker on the third poll)", strings.TrimSpace(string(count)))
	}
}

// TestRealKWinHelperJournalFailures pins the failure paths of
// the journal parse: no marker in the journal, malformed JSON,
// and a non-list payload all exit non-zero with no JSON.
func TestRealKWinHelperJournalFailures(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		wantErr string
	}{
		{"no marker in journal", "", "no KWin geometry output"},
		{"malformed payload", "not json at all", "cannot parse KWin geometry output"},
		{"non-list payload", `{"caption": "Firefox"}`, "not a list"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubDir := writeStubGi(t, t.TempDir(), stubKwinSource)
			stateDir, record := fakeJournalctl(t, tc.payload)
			stdout, stderr, err := runRealHelper(t, kwinHelperSource, nil, stubDir,
				kwinStubEnv(t, stubDir, stateDir, record, nil))
			if err == nil {
				t.Fatalf("helper succeeded with stdout %q, want a non-zero exit", stdout)
			}
			if !strings.Contains(stderr, tc.wantErr) {
				t.Errorf("stderr = %q, want %q", stderr, tc.wantErr)
			}
			if strings.TrimSpace(stdout) != "" {
				t.Errorf("stdout = %q, want empty (no JSON on failure)", stdout)
			}
		})
	}
}

// TestRealKWinHelperProxyUnreachable pins the earliest failure:
// when the KWin proxy cannot even be created, the helper fails
// cleanly with no D-Bus calls at all (nothing was loaded, so
// nothing needs unloading).
func TestRealKWinHelperProxyUnreachable(t *testing.T) {
	stubDir := writeStubGi(t, t.TempDir(), stubKwinSource)
	stateDir, record := fakeJournalctl(t, "")
	stdout, stderr, err := runRealHelper(t, kwinHelperSource, nil, stubDir,
		kwinStubEnv(t, stubDir, stateDir, record, map[string]string{"KWINSTUB_FAIL": "proxy"}))
	if err == nil {
		t.Fatalf("helper succeeded with stdout %q, want a non-zero exit", stdout)
	}
	if !strings.Contains(stderr, "cannot reach KWin scripting") {
		t.Errorf("stderr = %q, want the proxy failure", stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if calls, _ := os.ReadFile(record); len(calls) != 0 {
		t.Errorf("D-Bus calls = %q, want none (proxy never created)", string(calls))
	}
}

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
