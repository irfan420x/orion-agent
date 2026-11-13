package skills

// Audit2 tests (fresh-eyes audit 2026-11-13): heading handling in the
// Tools section, code-fence / numbered-list descriptions, numbered
// tool items, and nil-Registry guards.

import (
	"testing"
)

func TestParseToolsSubheadingEndsSection(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, "scoped.md", "# Scoped\n\nDoes things.\n\n## Tools\n\n- one\n\n### Notes\n\n- not-a-tool\n\n## Other\n\n- also-not-a-tool\n")
	r := NewRegistry()
	s, err := r.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if len(s.Tools) != 1 || s.Tools[0] != "one" {
		t.Errorf("tools = %v, want [one] (a ### sub-heading must end the Tools section)", s.Tools)
	}
}

func TestParseDescriptionSkipsCodeFence(t *testing.T) {
	dir := t.TempDir()
	// A file that opens with a fenced code block must not take the
	// fence marker ("```go") as its description.
	path := writeFixture(t, dir, "fenced.md", "# Fenced\n\n```go\npackage main\n```\n\nReal description here.\n\n## Tools\n\n- exec\n")
	r := NewRegistry()
	s, err := r.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if s.Description != "Real description here." {
		t.Errorf("description = %q, want %q", s.Description, "Real description here.")
	}

	// A file with nothing but a code fence after the heading has no
	// description at all — not the fence contents.
	path = writeFixture(t, dir, "onlyfence.md", "# OnlyFence\n\n```\nsome code\n```\n")
	s, err = r.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if s.Description != "" {
		t.Errorf("description = %q, want empty (only a code fence follows the heading)", s.Description)
	}
}

func TestParseDescriptionSkipsNumberedList(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, "steps.md", "# Steps\n\n1. First step\n2. Second step\n\nActual description.\n")
	r := NewRegistry()
	s, err := r.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if s.Description != "Actual description." {
		t.Errorf("description = %q, want %q (numbered list items are not prose)", s.Description, "Actual description.")
	}
}

func TestParseToolsNumberedItems(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, "num.md", "# Num\n\nDesc.\n\n## Tools\n\n1. exec\n2) read\n- write\n")
	r := NewRegistry()
	s, err := r.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	want := []string{"exec", "read", "write"}
	if len(s.Tools) != len(want) {
		t.Fatalf("tools = %v, want %v", s.Tools, want)
	}
	for i, w := range want {
		if s.Tools[i] != w {
			t.Errorf("tools[%d] = %q, want %q", i, s.Tools[i], w)
		}
	}
}

func TestNilRegistryDoesNotPanic(t *testing.T) {
	var r *Registry
	if err := r.Register(Skill{Name: "X"}); err == nil {
		t.Error("Register on a nil Registry returned nil error, want an error")
	}
	if _, ok := r.Get("X"); ok {
		t.Error("Get on a nil Registry found a skill, want not-found")
	}
	if got := r.List(); len(got) != 0 {
		t.Errorf("List on a nil Registry = %v, want empty", got)
	}
	if got := r.Search("x"); len(got) != 0 {
		t.Errorf("Search on a nil Registry = %v, want empty", got)
	}
	// LoadDir on a nil Registry must report errors, not panic: the
	// skill cannot be registered anywhere.
	dir := t.TempDir()
	writeFixture(t, dir, "a.md", "# A\n\nDesc.\n")
	if _, err := r.LoadDir(dir); err == nil {
		t.Error("LoadDir on a nil Registry returned nil error, want an error")
	}
}

func TestHeadingLevel(t *testing.T) {
	cases := map[string]int{
		"# Top":            1,
		"## Section":       2,
		"###### Deep":      6,
		"#":                1,
		"####### Too many": 0,
		"#hashtag":         0,
		"##nospace":        0,
		"":                 0,
		"plain text":       0,
		" ## indented":     0, // not trimmed here; callers trim first
	}
	for line, want := range cases {
		if got := headingLevel(line); got != want {
			t.Errorf("headingLevel(%q) = %d, want %d", line, got, want)
		}
	}
}

func TestNumberedItem(t *testing.T) {
	cases := []struct {
		line string
		item string
		ok   bool
	}{
		{"1. exec", "exec", true},
		{"12) read", "read", true},
		{"1.exec", "", false},
		{"1. ", "", true}, // empty item: parses, caller skips empties
		{"a. exec", "", false},
		{"- exec", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		item, ok := numberedItem(c.line)
		if ok != c.ok || item != c.item {
			t.Errorf("numberedItem(%q) = (%q, %v), want (%q, %v)", c.line, item, ok, c.item, c.ok)
		}
	}
}
