package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", name, err)
	}
	return path
}

const shellSkill = `# Shell

Run shell commands safely on the host.

## Tools

- exec
- shell_check
`

const filesSkill = `# Files

Read and write files in the workspace.

## Tools

- read
- write
- list
`

func TestLoadDirLoadsSkills(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "shell.md", shellSkill)
	writeFixture(t, dir, "files.md", filesSkill)
	// A non-.md file must be ignored.
	writeFixture(t, dir, "notes.txt", "# Notes\n\nNot a skill.\n")
	writeFixture(t, dir, "README", "# Readme\n\nNot a skill.\n")

	r := NewRegistry()
	n, err := r.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir returned error: %v", err)
	}
	if n != 2 {
		t.Fatalf("LoadDir loaded %d skills, want 2", n)
	}

	shell, ok := r.Get("Shell")
	if !ok {
		t.Fatal("Get(\"Shell\") not found")
	}
	if shell.Description != "Run shell commands safely on the host." {
		t.Errorf("Shell description = %q, want %q", shell.Description, "Run shell commands safely on the host.")
	}
	if len(shell.Tools) != 2 || shell.Tools[0] != "exec" || shell.Tools[1] != "shell_check" {
		t.Errorf("Shell tools = %v, want [exec shell_check]", shell.Tools)
	}
	if shell.Path != filepath.Join(dir, "shell.md") {
		t.Errorf("Shell path = %q, want %q", shell.Path, filepath.Join(dir, "shell.md"))
	}
	if !strings.Contains(shell.Content, "# Shell") {
		t.Errorf("Shell content does not contain the full file text")
	}

	files, ok := r.Get("Files")
	if !ok {
		t.Fatal("Get(\"Files\") not found")
	}
	if files.Description != "Read and write files in the workspace." {
		t.Errorf("Files description = %q", files.Description)
	}
	if len(files.Tools) != 3 || files.Tools[0] != "read" || files.Tools[1] != "write" || files.Tools[2] != "list" {
		t.Errorf("Files tools = %v, want [read write list]", files.Tools)
	}

	// The ignored non-.md files must not appear as skills.
	if _, ok := r.Get("Notes"); ok {
		t.Error("non-.md file was loaded as a skill")
	}
	if _, ok := r.Get("Readme"); ok {
		t.Error("extensionless file was loaded as a skill")
	}
}

func TestLoadDirEmptyFileSkippedButReported(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "good.md", shellSkill)
	writeFixture(t, dir, "empty.md", "")

	r := NewRegistry()
	n, err := r.LoadDir(dir)
	if n != 1 {
		t.Errorf("LoadDir loaded %d skills, want 1", n)
	}
	if err == nil {
		t.Fatal("LoadDir returned nil error, want an error reporting the empty file")
	}
	if !strings.Contains(err.Error(), "empty.md") {
		t.Errorf("LoadDir error %q does not mention empty.md", err.Error())
	}
	if _, ok := r.Get("Shell"); !ok {
		t.Error("good skill was not loaded alongside the bad file")
	}
}

func TestLoadDirMissingDir(t *testing.T) {
	r := NewRegistry()
	n, err := r.LoadDir(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("LoadDir on missing directory returned nil error")
	}
	if n != 0 {
		t.Errorf("LoadDir returned %d, want 0", n)
	}
}

func TestLoadDirNoMarkdownFiles(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "notes.txt", "hello")
	r := NewRegistry()
	if _, err := r.LoadDir(dir); err == nil {
		t.Fatal("LoadDir on a directory with no .md files returned nil error")
	}
}

func TestGetListSearch(t *testing.T) {
	r := NewRegistry()
	for _, s := range []Skill{
		{Name: "Zebra", Description: "Striped animal facts"},
		{Name: "Apple", Description: "Fruit and orchard notes"},
		{Name: "Mango", Description: "Tropical fruit guide"},
	} {
		if err := r.Register(s); err != nil {
			t.Fatalf("Register(%q): %v", s.Name, err)
		}
	}

	// Get: found and missing.
	if _, ok := r.Get("Apple"); !ok {
		t.Error("Get(\"Apple\") not found")
	}
	if _, ok := r.Get("Nope"); ok {
		t.Error("Get(\"Nope\") unexpectedly found")
	}

	// List: sorted by name regardless of registration order.
	list := r.List()
	got := []string{list[0].Name, list[1].Name, list[2].Name}
	want := []string{"Apple", "Mango", "Zebra"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("List()[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// Search: case-insensitive substring over name and description.
	hits := r.Search("FRUIT")
	if len(hits) != 2 {
		t.Fatalf("Search(\"FRUIT\") returned %d skills, want 2", len(hits))
	}
	if hits[0].Name != "Apple" || hits[1].Name != "Mango" {
		t.Errorf("Search(\"FRUIT\") = %v, want [Apple Mango]", []string{hits[0].Name, hits[1].Name})
	}
	if hits := r.Search("zebra"); len(hits) != 1 || hits[0].Name != "Zebra" {
		t.Errorf("Search(\"zebra\") did not match by name: %v", hits)
	}
	if hits := r.Search("nothing-matches"); len(hits) != 0 {
		t.Errorf("Search(\"nothing-matches\") = %d hits, want 0", len(hits))
	}
}

func TestRegisterDuplicateAndEmpty(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(Skill{Name: "Shell"}); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := r.Register(Skill{Name: "Shell"}); err == nil {
		t.Error("duplicate Register returned nil error")
	}
	if err := r.Register(Skill{Name: ""}); err == nil {
		t.Error("Register with empty name returned nil error")
	}
	if err := r.Register(Skill{Name: "   "}); err == nil {
		t.Error("Register with blank name returned nil error")
	}
	if len(r.List()) != 1 {
		t.Errorf("registry holds %d skills after failed registers, want 1", len(r.List()))
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, "shell.md", shellSkill)

	r := NewRegistry()
	s, err := r.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if s.Name != "Shell" {
		t.Errorf("LoadFile name = %q, want %q", s.Name, "Shell")
	}
	if s.Description != "Run shell commands safely on the host." {
		t.Errorf("LoadFile description = %q", s.Description)
	}
	if len(s.Tools) != 2 {
		t.Errorf("LoadFile tools = %v, want 2", s.Tools)
	}

	// Missing file errors.
	if _, err := r.LoadFile(filepath.Join(dir, "missing.md")); err == nil {
		t.Error("LoadFile on missing file returned nil error")
	}
}

func TestParseFallbacks(t *testing.T) {
	dir := t.TempDir()

	// No "# " heading: name falls back to the file name.
	path := writeFixture(t, dir, "plain.md", "Just a paragraph, no heading.\n")
	r := NewRegistry()
	s, err := r.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if s.Name != "plain" {
		t.Errorf("name = %q, want %q (filename fallback)", s.Name, "plain")
	}
	if s.Description != "Just a paragraph, no heading." {
		t.Errorf("description = %q", s.Description)
	}
	if len(s.Tools) != 0 {
		t.Errorf("tools = %v, want none", s.Tools)
	}

	// "## Tools" section ends at the next section heading.
	path = writeFixture(t, dir, "scoped.md", "# Scoped\n\nDoes things.\n\n## Tools\n\n- one\n\n## Other\n\n- not-a-tool\n")
	s, err = r.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if len(s.Tools) != 1 || s.Tools[0] != "one" {
		t.Errorf("tools = %v, want [one]", s.Tools)
	}
}

func TestLoadDirDuplicateNamesReported(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "a.md", "# Same\n\nFirst copy.\n")
	writeFixture(t, dir, "b.md", "# Same\n\nSecond copy.\n")

	r := NewRegistry()
	n, err := r.LoadDir(dir)
	if n != 1 {
		t.Errorf("LoadDir loaded %d skills, want 1", n)
	}
	if err == nil {
		t.Fatal("LoadDir returned nil error for duplicate skill names")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("LoadDir error %q does not mention the duplicate", err.Error())
	}
}

func TestParseToolsBacktickWithDescription(t *testing.T) {
	// Regression test: stripping backticks before the " - description"
	// suffix used to leave a stray backtick on the tool name
	// ("`exec` - runs shell" parsed as "exec`" instead of "exec").
	dir := t.TempDir()
	path := writeFixture(t, dir, "ops.md", "# Ops\n\nDoes ops things.\n\n## Tools\n\n- `exec` - runs shell commands\n- `read`: reads a file\n- `write`\n- plain - no backticks, with a description\n")
	r := NewRegistry()
	s, err := r.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	want := []string{"exec", "read", "write", "plain"}
	if len(s.Tools) != len(want) {
		t.Fatalf("tools = %v, want %v", s.Tools, want)
	}
	for i, w := range want {
		if s.Tools[i] != w {
			t.Errorf("tools[%d] = %q, want %q (full list %v)", i, s.Tools[i], w, s.Tools)
		}
	}
}

func TestParseToolsDeduped(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, "dup.md", "# Dup\n\nDoes things.\n\n## Tools\n\n- exec\n- exec\n- read\n- exec\n")
	r := NewRegistry()
	s, err := r.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	want := []string{"exec", "read"}
	if len(s.Tools) != len(want) || s.Tools[0] != want[0] || s.Tools[1] != want[1] {
		t.Errorf("tools = %v, want deduped %v", s.Tools, want)
	}
}

func TestLoadFileDotMDHasNoName(t *testing.T) {
	// A file literally called ".md" yields an empty filename fallback;
	// with no "# " heading there is no name at all, so LoadFile must
	// fail instead of returning an empty-named skill that Register
	// would then reject further downstream.
	dir := t.TempDir()
	path := writeFixture(t, dir, ".md", "Just a paragraph, no heading.\n")
	r := NewRegistry()
	if _, err := r.LoadFile(path); err == nil {
		t.Error("LoadFile(\".md\" with no heading): expected error, got nil")
	}
}

func TestRegisterAndGetDoNotAliasTools(t *testing.T) {
	// Regression test: Skill is passed by value, but its Tools slice
	// used to be shared with the registry, so mutating the caller's
	// slice (or a slice returned by Get/List) corrupted stored skills.
	r := NewRegistry()
	orig := Skill{Name: "Shell", Tools: []string{"exec", "read"}}
	if err := r.Register(orig); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Mutating the caller's slice must not touch the registry.
	orig.Tools[0] = "MUTATED"
	got, ok := r.Get("Shell")
	if !ok {
		t.Fatal("Get(\"Shell\") not found")
	}
	if got.Tools[0] != "exec" {
		t.Errorf("after mutating caller's slice, registry tool = %q, want %q", got.Tools[0], "exec")
	}
	// Mutating a slice returned by Get must not touch the registry.
	got.Tools[1] = "MUTATED"
	got2, _ := r.Get("Shell")
	if got2.Tools[1] != "read" {
		t.Errorf("after mutating Get() result, registry tool = %q, want %q", got2.Tools[1], "read")
	}
	// Same for List.
	list := r.List()
	list[0].Tools[0] = "MUTATED"
	got3, _ := r.Get("Shell")
	if got3.Tools[0] != "exec" {
		t.Errorf("after mutating List() result, registry tool = %q, want %q", got3.Tools[0], "exec")
	}
}

func TestParseToolsHeadingVariants(t *testing.T) {
	// Audit 5 F-A5-P2-25: "## Tools:", "## Allowed Tools", and
	// "## Available Tools" used to parse to Tools=nil — and nil
	// means unrestricted — so a skill author intending a
	// restriction silently got none. All variants must now parse
	// to the same restriction as the exact heading.
	cases := []struct {
		heading string
	}{
		{"## Tools"},
		{"## Tools:"},
		{"## tools"},
		{"## Allowed Tools"},
		{"## Allowed Tools:"},
		{"## Available Tools"},
		{"## available tools:"},
	}
	for _, tc := range cases {
		t.Run(tc.heading, func(t *testing.T) {
			content := "# Variant\n\nDoes things.\n\n" + tc.heading + "\n\n- echo\n"
			s, warnings := parseSkill("variant.md", content)
			if len(warnings) != 0 {
				t.Errorf("heading %q produced warnings %v, want none", tc.heading, warnings)
			}
			if len(s.Tools) != 1 || s.Tools[0] != "echo" {
				t.Errorf("heading %q: tools = %v, want [echo]", tc.heading, s.Tools)
			}
		})
	}
}

func TestParseToolsLikeHeadingWarns(t *testing.T) {
	// A heading that looks like a Tools section but is not a
	// recognised variant must not silently fail open: it parses
	// to no restriction AND produces a warning, so the author
	// finds out instead of running unrestricted.
	for _, heading := range []string{"## Tool", "## Tools and Setup", "## My Tools"} {
		content := "# Variant\n\nDoes things.\n\n" + heading + "\n\n- echo\n"
		s, warnings := parseSkill("variant.md", content)
		if len(s.Tools) != 0 {
			t.Errorf("heading %q: tools = %v, want none (unrecognised heading)", heading, s.Tools)
		}
		if len(warnings) == 0 {
			t.Errorf("heading %q: no warning produced, want one", heading)
		} else if !strings.Contains(warnings[0], "no tool restriction") {
			t.Errorf("heading %q: warning = %q, want it to say no tool restriction applies", heading, warnings[0])
		}
	}

	// A heading with no "tool" word at all is not Tools-like:
	// no warning, no restriction.
	s, warnings := parseSkill("plain.md", "# Plain\n\nDoes things.\n\n## Notes\n\n- echo\n")
	if len(s.Tools) != 0 || len(warnings) != 0 {
		t.Errorf("non-Tools heading: tools = %v, warnings = %v, want both empty", s.Tools, warnings)
	}
}

func TestLoadDirRefusesSymlinks(t *testing.T) {
	// Audit 5 F-A5-P2-26: a .md symlink inside the skills dir
	// pointing anywhere on disk must not load as a skill.
	dir := t.TempDir()
	outside := t.TempDir()
	writeFixture(t, outside, "outside.md", "# Smuggled\n\nSmuggled body.\n")
	if err := os.Symlink(filepath.Join(outside, "outside.md"), filepath.Join(dir, "evil.md")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	writeFixture(t, dir, "good.md", shellSkill)

	r := NewRegistry()
	n, err := r.LoadDir(dir)
	if n != 1 {
		t.Fatalf("LoadDir loaded %d skills, want 1 (only the regular file)", n)
	}
	if err == nil {
		t.Fatal("LoadDir: expected an error refusing the symlink, got nil")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("LoadDir error = %v, want it to mention the symlink", err)
	}
	if _, ok := r.Get("Smuggled"); ok {
		t.Error("symlinked skill \"Smuggled\" loaded — it must not be")
	}
	if _, ok := r.Get("Shell"); !ok {
		t.Error("regular skill \"Shell\" did not load alongside the refused symlink")
	}
}

func TestLoadFileRefusesSymlink(t *testing.T) {
	// Same guarantee on the single-file path.
	dir := t.TempDir()
	outside := t.TempDir()
	target := writeFixture(t, outside, "outside.md", "# Smuggled\n\nSmuggled body.\n")
	link := filepath.Join(dir, "evil.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	r := NewRegistry()
	if _, err := r.LoadFile(link); err == nil {
		t.Error("LoadFile(symlink): expected error, got nil")
	} else if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("LoadFile(symlink) error = %v, want it to mention the symlink", err)
	}
}
