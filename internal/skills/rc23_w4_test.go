// rc23 W4: F-A17-14 (unknown ## Tools names warn at load) and
// F-A17-15 (skill hot reload via Registry.Reload).
package skills

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureLog redirects the standard logger for the test and returns
// the buffer it writes to; the caller must restore with the returned
// func. parseFile logs load warnings through the standard logger.
func captureLog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	return &buf, func() { log.SetOutput(old) }
}

func TestParseSkillUnknownToolWarns(t *testing.T) {
	content := "# Fixer\n\nFixes things.\n\n## Tools\n\n- shell_exe\n- file_read\n"
	s, warnings := parseSkill("fixer.md", content)
	// The skill still loads, with the name kept.
	if len(s.Tools) != 2 || s.Tools[0] != "shell_exe" || s.Tools[1] != "file_read" {
		t.Fatalf("Tools = %v, want [shell_exe file_read]", s.Tools)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, `unknown tool "shell_exe"`) &&
			strings.Contains(w, `skill "Fixer"`) &&
			strings.Contains(w, `did you mean "shell_exec"`) {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want an unknown-tool warning naming shell_exe, skill Fixer, and suggesting shell_exec", warnings)
	}
	// The known name must not warn.
	for _, w := range warnings {
		if strings.Contains(w, "file_read") {
			t.Errorf("known tool file_read must not warn: %v", warnings)
		}
	}
}

func TestParseSkillKnownToolsDoNotWarn(t *testing.T) {
	for _, name := range []string{"shell_exec", "file_read", "web_search", "skill", "todo_write", "subagent_spawn", "calculator"} {
		content := "# S\n\nDoes things.\n\n## Tools\n\n- " + name + "\n"
		_, warnings := parseSkill("s.md", content)
		for _, w := range warnings {
			if strings.Contains(w, "unknown tool") {
				t.Errorf("tool %q warned: %v", name, warnings)
			}
		}
	}
}

func TestParseSkillUnknownToolNoCloseMatch(t *testing.T) {
	// "exec" is far from every known name: warn, but suggest nothing.
	_, warnings := parseSkill("s.md", "# S\n\nDoes things.\n\n## Tools\n\n- exec\n")
	found := false
	for _, w := range warnings {
		if strings.Contains(w, `unknown tool "exec"`) {
			found = true
			if strings.Contains(w, "did you mean") {
				t.Errorf("far-off name must not get a suggestion: %q", w)
			}
		}
	}
	if !found {
		t.Errorf("warnings = %v, want an unknown-tool warning for exec", warnings)
	}
}

func TestClosestKnownTool(t *testing.T) {
	for in, want := range map[string]string{
		"shell_exe":  "shell_exec",
		"shell-exec": "shell_exec",
		"file_reed":  "file_read",
		"webfetch":   "web_fetch",
		"exec":       "",
		"banana":     "",
	} {
		if got := closestKnownTool(in, knownBuiltInTools); got != want {
			t.Errorf("closestKnownTool(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadDirUnknownToolWarnsInLogButLoads(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "fixer.md", "# Fixer\n\nFixes things.\n\n## Tools\n\n- shell_exe\n")
	buf, restore := captureLog(t)
	defer restore()
	r := NewRegistry()
	loaded, err := r.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if loaded != 1 {
		t.Fatalf("loaded = %d, want 1 (warning only, never a load failure)", loaded)
	}
	if !strings.Contains(buf.String(), `unknown tool "shell_exe"`) {
		t.Errorf("log = %q, want the unknown-tool warning", buf.String())
	}
	if _, ok := r.Get("Fixer"); !ok {
		t.Error("skill Fixer must be registered despite the warning")
	}
}

func TestSetKnownToolsExtendsValidation(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "mcp.md", "# Mcp\n\nUses MCP.\n\n## Tools\n\n- mcp__srv__do_thing\n")
	// Without the extension the MCP tool name warns.
	_, warnings := parseSkill("mcp.md", "# Mcp\n\nUses MCP.\n\n## Tools\n\n- mcp__srv__do_thing\n")
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "mcp__srv__do_thing") {
			found = true
		}
	}
	if !found {
		t.Fatal("MCP tool name must warn against the built-in set")
	}
	// With the extension it does not — and the built-ins stay known.
	r := NewRegistry()
	r.SetKnownTools([]string{"mcp__srv__do_thing"})
	known := r.knownSet()
	if !known["mcp__srv__do_thing"] || !known["shell_exec"] {
		t.Errorf("knownSet after extension = %v, want MCP name added and shell_exec kept", known)
	}
	buf, restore := captureLog(t)
	defer restore()
	if loaded, err := r.LoadDir(dir); err != nil || loaded != 1 {
		t.Fatalf("LoadDir = (%d, %v), want (1, nil)", loaded, err)
	}
	if strings.Contains(buf.String(), "unknown tool") {
		t.Errorf("log must not warn for the extended name: %q", buf.String())
	}
}

func TestReloadPicksUpAddsChangesAndRemovals(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "a.md", "# Alpha\n\nFirst version.\n")
	r := NewRegistry()
	if loaded, err := r.LoadDir(dir); err != nil || loaded != 1 {
		t.Fatalf("LoadDir = (%d, %v), want (1, nil)", loaded, err)
	}
	// Change A, add B, then reload: both must be visible.
	writeFixture(t, dir, "a.md", "# Alpha\n\nSecond version.\n")
	writeFixture(t, dir, "b.md", "# Beta\n\nBrand new.\n")
	loaded, err := r.Reload(dir)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if loaded != 2 {
		t.Fatalf("Reload loaded = %d, want 2", loaded)
	}
	a, ok := r.Get("Alpha")
	if !ok {
		t.Fatal("Alpha missing after reload")
	}
	if a.Description != "Second version." {
		t.Errorf("Alpha description = %q, want the reloaded text", a.Description)
	}
	if _, ok := r.Get("Beta"); !ok {
		t.Error("Beta missing after reload")
	}
	// Remove A, reload again: A must drop out.
	if err := os.Remove(filepath.Join(dir, "a.md")); err != nil {
		t.Fatalf("remove a.md: %v", err)
	}
	if loaded, err := r.Reload(dir); err != nil || loaded != 1 {
		t.Fatalf("Reload after removal = (%d, %v), want (1, nil)", loaded, err)
	}
	if _, ok := r.Get("Alpha"); ok {
		t.Error("Alpha must be gone after its file was removed")
	}
	if _, ok := r.Get("Beta"); !ok {
		t.Error("Beta must survive the reload")
	}
}

func TestReloadUnreadableDirKeepsOldSkills(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "a.md", "# Alpha\n\nKeeps me.\n")
	r := NewRegistry()
	if _, err := r.LoadDir(dir); err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	loaded, err := r.Reload(filepath.Join(dir, "does-not-exist"))
	if err == nil {
		t.Error("Reload of a missing dir must error")
	}
	if loaded != 0 {
		t.Errorf("Reload loaded = %d, want 0 on total failure", loaded)
	}
	if _, ok := r.Get("Alpha"); !ok {
		t.Error("failed Reload must leave the previously loaded skills live")
	}
}

func TestReloadPartialFailureSwapsGoodSkills(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "good.md", "# Good\n\nWorks.\n")
	r := NewRegistry()
	if _, err := r.LoadDir(dir); err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	// Break the only file (empty) and add a second good one: the
	// reload still swaps (the good files are the dir's truth) and
	// reports the failure alongside the count.
	writeFixture(t, dir, "good.md", "   \n")
	writeFixture(t, dir, "new.md", "# New\n\nAlso works.\n")
	loaded, err := r.Reload(dir)
	if err == nil {
		t.Error("Reload with a broken file must return an error")
	}
	if loaded != 1 {
		t.Errorf("Reload loaded = %d, want 1 (the good file)", loaded)
	}
	if _, ok := r.Get("Good"); ok {
		t.Error("broken Good must drop out of the registry")
	}
	if _, ok := r.Get("New"); !ok {
		t.Error("New must be registered after the partial reload")
	}
}

func TestReloadNilRegistry(t *testing.T) {
	var r *Registry
	if _, err := r.Reload(t.TempDir()); err == nil {
		t.Error("Reload on nil Registry must error, not panic")
	}
}

func TestReloadEmptyDirKeepsOldSkills(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "a.md", "# Alpha\n\nKeeps me.\n")
	r := NewRegistry()
	if _, err := r.LoadDir(dir); err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	empty := t.TempDir()
	if loaded, err := r.Reload(empty); err == nil || loaded != 0 {
		t.Errorf("Reload of an empty dir = (%d, %v), want (0, error)", loaded, err)
	}
	if _, ok := r.Get("Alpha"); !ok {
		t.Error("empty-dir Reload must leave the previously loaded skills live")
	}
}
