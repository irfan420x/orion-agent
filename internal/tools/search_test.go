package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSeedFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedGrepTree builds a nested tree with a known needle string and
// returns the root.
func seedGrepTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeSeedFile(t, filepath.Join(root, "a.go"), "package a\n\n// NEEDLE_ALPHA one\nfunc A() {}\n")
	writeSeedFile(t, filepath.Join(root, "sub", "b.go"), "package b\n\n// NEEDLE_ALPHA two\n// needle_beta lower\nfunc B() {}\n")
	writeSeedFile(t, filepath.Join(root, "sub", "deep", "c.txt"), "plain text\nNEEDLE_ALPHA three\nmore\n")
	writeSeedFile(t, filepath.Join(root, "sub", "deep", "d.md"), "# docs\nnothing here\n")
	return root
}

func TestGrepFindsSeededStringNested(t *testing.T) {
	root := seedGrepTree(t)
	tool := &grepTool{roots: []string{root}}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "NEEDLE_ALPHA", "path": root,
	}))
	if err != nil {
		t.Fatalf("grep failed: %v", err)
	}
	for _, want := range []string{"a.go:3:", "b.go:3:", "c.txt:2:"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected match %q in output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "d.md") {
		t.Errorf("d.md must not match, output:\n%s", out)
	}
	if !strings.Contains(out, "[3 matching line(s) in 3 file(s)]") {
		t.Errorf("expected footer counts, got:\n%s", out)
	}
}

func TestGrepSingleFile(t *testing.T) {
	root := seedGrepTree(t)
	tool := &grepTool{roots: []string{root}}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "NEEDLE_ALPHA", "path": filepath.Join(root, "sub", "b.go"),
	}))
	if err != nil {
		t.Fatalf("grep failed: %v", err)
	}
	if !strings.Contains(out, "b.go:3:") || strings.Contains(out, "a.go") {
		t.Errorf("single-file grep wrong, got:\n%s", out)
	}
}

func TestGrepLiteralAndCaseInsensitive(t *testing.T) {
	root := seedGrepTree(t)
	tool := &grepTool{roots: []string{root}}
	// Regex "." matches everything; literal "." must match only a
	// literal dot.
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "A()", "path": root, "literal": true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.go:4:") {
		t.Errorf("literal A() should match func A() line, got:\n%s", out)
	}
	// Case-insensitive literal finds the lower-case line.
	out, err = tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "NEEDLE_BETA", "path": root, "literal": true, "case_insensitive": true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "b.go:4:") {
		t.Errorf("case-insensitive literal should match needle_beta, got:\n%s", out)
	}
	// Same pattern case-sensitive must not match.
	out, err = tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "NEEDLE_BETA", "path": root, "literal": true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if out != "(no matches)" {
		t.Errorf("case-sensitive literal should not match, got:\n%s", out)
	}
	// Case-insensitive regex.
	out, err = tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "needle_alpha", "path": root, "case_insensitive": true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[3 matching line(s)") {
		t.Errorf("case-insensitive regex should find 3, got:\n%s", out)
	}
}

func TestGrepInvalidRegex(t *testing.T) {
	tool := &grepTool{roots: []string{t.TempDir()}}
	_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "([", "path": ".",
	}))
	if err == nil {
		t.Fatal("expected error for invalid regex")
	}
	if kind := toolErrKind(t, err); kind != "invalid_args" {
		t.Fatalf("expected invalid_args, got %q", kind)
	}
	_, err = tool.Execute(context.Background(), mustJSON(t, map[string]any{"pattern": ""}))
	if err == nil {
		t.Fatal("expected error for empty pattern")
	}
}

func TestGrepContextLines(t *testing.T) {
	root := t.TempDir()
	writeSeedFile(t, filepath.Join(root, "f.txt"), "one\ntwo\nNEEDLE here\nfour\nfive\n")
	tool := &grepTool{roots: []string{root}}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "NEEDLE", "path": root, "context_lines": 1,
	}))
	if err != nil {
		t.Fatal(err)
	}
	// Match line uses ":", context lines use "-".
	if !strings.Contains(out, "f.txt:3: NEEDLE here") {
		t.Errorf("missing match line, got:\n%s", out)
	}
	if !strings.Contains(out, "f.txt-2- two") || !strings.Contains(out, "f.txt-4- four") {
		t.Errorf("missing context lines, got:\n%s", out)
	}
	if strings.Contains(out, "f.txt-1-") || strings.Contains(out, "f.txt-5-") {
		t.Errorf("context must be exactly 1 line each side, got:\n%s", out)
	}
}

func TestGrepIncludeExclude(t *testing.T) {
	root := seedGrepTree(t)
	tool := &grepTool{roots: []string{root}}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "NEEDLE_ALPHA", "path": root, "include": "*.go",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[2 matching line(s)") || strings.Contains(out, "c.txt") {
		t.Errorf("include *.go should find only .go matches, got:\n%s", out)
	}
	out, err = tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "NEEDLE_ALPHA", "path": root, "exclude": "*.go",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "c.txt:2:") || strings.Contains(out, "a.go") {
		t.Errorf("exclude *.go should find only c.txt, got:\n%s", out)
	}
	_, err = tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "x", "path": root, "include": "[bad",
	}))
	if err == nil {
		t.Fatal("expected error for invalid include glob")
	}
}

func TestGrepMaxMatchesCap(t *testing.T) {
	root := t.TempDir()
	var sb strings.Builder
	for i := 0; i < 20; i++ {
		sb.WriteString("NEEDLE line\n")
	}
	writeSeedFile(t, filepath.Join(root, "many.txt"), sb.String())
	tool := &grepTool{roots: []string{root}}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "NEEDLE", "path": root, "max_matches": 5,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[5 matching line(s)") || !strings.Contains(out, "capped at 5 matches") {
		t.Errorf("expected cap at 5, got:\n%s", out)
	}
}

func TestGrepOutsideRootRefused(t *testing.T) {
	root := t.TempDir()
	tool := &grepTool{roots: []string{root}}
	for _, p := range []string{"/etc", filepath.Join(root, "..", "etc")} {
		_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
			"pattern": "root", "path": p,
		}))
		if err == nil {
			t.Fatalf("path %q: expected refusal", p)
		}
		if kind := toolErrKind(t, err); kind != "invalid_args" {
			t.Fatalf("path %q: expected invalid_args, got %q", p, kind)
		}
	}
}

func TestGrepSymlinkEscapeSkipped(t *testing.T) {
	root := seedGrepTree(t)
	// A secret outside the root, linked from inside it.
	outside := t.TempDir()
	writeSeedFile(t, filepath.Join(outside, "secret.txt"), "NEEDLE_SECRET outside\n")
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	// A symlink to a directory outside the root.
	linkDir := filepath.Join(root, "linkdir")
	if err := os.Symlink(outside, linkDir); err != nil {
		t.Skipf("cannot create dir symlink: %v", err)
	}
	tool := &grepTool{roots: []string{root}}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "NEEDLE_SECRET", "path": root,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if out != "(no matches)\n[skipped 2 symlinked path(s) outside the searched directory]" {
		t.Errorf("escaping symlinks must be skipped, not read, got:\n%s", out)
	}
}

func TestGrepBinarySkipped(t *testing.T) {
	root := t.TempDir()
	writeSeedFile(t, filepath.Join(root, "bin.dat"), "NEEDLE\x00binary\x00junk\n")
	writeSeedFile(t, filepath.Join(root, "ok.txt"), "NEEDLE text\n")
	tool := &grepTool{roots: []string{root}}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "NEEDLE", "path": root,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "bin.dat") || !strings.Contains(out, "ok.txt") {
		t.Errorf("binary file must be skipped, got:\n%s", out)
	}
	if !strings.Contains(out, "skipped 1 binary file(s)") {
		t.Errorf("expected binary skip note, got:\n%s", out)
	}
}

// ---------------- glob ----------------

func seedGlobTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, p := range []string{
		"a.go", "b.txt",
		"sub/c.go", "sub/d.ts",
		"sub/deep/e.go", "sub/deep/f.md",
		"other/g.go",
	} {
		writeSeedFile(t, filepath.Join(root, p), "x\n")
	}
	return root
}

func globLines(t *testing.T, out string) []string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var kept []string
	for _, l := range lines {
		if strings.HasPrefix(l, "[") {
			continue
		}
		kept = append(kept, l)
	}
	return kept
}

func TestGlobRecursivePattern(t *testing.T) {
	root := seedGlobTree(t)
	tool := &globTool{roots: []string{root}}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "**/*.go", "path": root,
	}))
	if err != nil {
		t.Fatal(err)
	}
	lines := globLines(t, out)
	if len(lines) != 4 {
		t.Fatalf("**/*.go should match 4 files, got %d:\n%s", len(lines), out)
	}
	for _, want := range []string{"a.go", "sub/c.go", "sub/deep/e.go", "other/g.go"} {
		found := false
		for _, l := range lines {
			if strings.HasSuffix(l, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// "*.go" is top-level only.
	out, err = tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "*.go", "path": root,
	}))
	if err != nil {
		t.Fatal(err)
	}
	lines = globLines(t, out)
	if len(lines) != 1 || !strings.HasSuffix(lines[0], "a.go") {
		t.Errorf("*.go should match only top-level a.go, got:\n%s", out)
	}
}

func TestGlobSegmentPatterns(t *testing.T) {
	root := seedGlobTree(t)
	tool := &globTool{roots: []string{root}}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "sub/**/*.go", "path": root,
	}))
	if err != nil {
		t.Fatal(err)
	}
	lines := globLines(t, out)
	if len(lines) != 2 {
		t.Fatalf("sub/**/*.go should match 2, got:\n%s", out)
	}
	out, err = tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "sub/*.ts", "path": root,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if lines := globLines(t, out); len(lines) != 1 || !strings.HasSuffix(lines[0], "d.ts") {
		t.Errorf("sub/*.ts should match d.ts, got:\n%s", out)
	}
	// "?" and character classes.
	out, err = tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "?.go", "path": root,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if lines := globLines(t, out); len(lines) != 1 {
		t.Errorf("?.go should match 1 top-level file, got:\n%s", out)
	}
	out, err = tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "**/[ab].txt", "path": root,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if lines := globLines(t, out); len(lines) != 1 || !strings.HasSuffix(lines[0], "b.txt") {
		t.Errorf("**/[ab].txt should match b.txt, got:\n%s", out)
	}
	// No matches is a result, not an error.
	out, err = tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "**/*.py", "path": root,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if out != "(no matches)" {
		t.Errorf("expected (no matches), got %q", out)
	}
}

func TestMatchGlobSegments(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		{"**/*.go", "a.go", true},       // ** matches zero segments
		{"**/*.go", "x/y/z.go", true},   // ** matches several
		{"**/*.go", "x/y/z.txt", false}, // wrong extension
		{"*.go", "x/a.go", false},       // * stays in one segment
		{"a/**/b", "a/b", true},         // ** matches zero in the middle
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/x/y/c", false},
		{"**", "a/b/c", true},
		{"a", "a", true},
		{"a", "b", false},
	}
	for _, tc := range cases {
		pat, err := validateGlobPattern(tc.pat)
		if err != nil {
			t.Fatalf("validate %q: %v", tc.pat, err)
		}
		got := matchGlobSegments(pat, strings.Split(tc.name, "/"))
		if got != tc.want {
			t.Errorf("match %q vs %q: got %v, want %v", tc.pat, tc.name, got, tc.want)
		}
	}
}

func TestGlobInvalidPatterns(t *testing.T) {
	tool := &globTool{roots: []string{t.TempDir()}}
	for _, pat := range []string{
		"", "/abs/*.go", "../*.go", "a/../b", "a//b", "**/[bad",
	} {
		_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
			"pattern": pat, "path": ".",
		}))
		if err == nil {
			t.Fatalf("pattern %q: expected error", pat)
		}
		if kind := toolErrKind(t, err); kind != "invalid_args" {
			t.Fatalf("pattern %q: expected invalid_args, got %q", pat, kind)
		}
	}
}

func TestGlobOutsideRootRefused(t *testing.T) {
	root := t.TempDir()
	tool := &globTool{roots: []string{root}}
	_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "**/*.go", "path": "/etc",
	}))
	if err == nil {
		t.Fatal("expected refusal for /etc")
	}
	if kind := toolErrKind(t, err); kind != "invalid_args" {
		t.Fatalf("expected invalid_args, got %q", kind)
	}
}

func TestGlobMaxResultsCap(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 10; i++ {
		writeSeedFile(t, filepath.Join(root, filepath.FromSlash("d/f"+string(rune('a'+i))+".go")), "x\n")
	}
	tool := &globTool{roots: []string{root}}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "**/*.go", "path": root, "max_results": 3,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if lines := globLines(t, out); len(lines) != 3 {
		t.Errorf("expected 3 results, got:\n%s", out)
	}
	if !strings.Contains(out, "showing first 3 matches") {
		t.Errorf("expected cap note, got:\n%s", out)
	}
}

func TestGlobSymlinkEscapeSkipped(t *testing.T) {
	root := seedGlobTree(t)
	outside := t.TempDir()
	writeSeedFile(t, filepath.Join(outside, "secret.go"), "x\n")
	if err := os.Symlink(filepath.Join(outside, "secret.go"), filepath.Join(root, "leak.go")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	tool := &globTool{roots: []string{root}}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "**/*.go", "path": root,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "leak.go") {
		t.Errorf("escaping symlink must not match, got:\n%s", out)
	}
	if !strings.Contains(out, "skipped 1 symlinked path(s)") {
		t.Errorf("expected symlink skip note, got:\n%s", out)
	}
}

func TestGlobMatchesDirectories(t *testing.T) {
	root := seedGlobTree(t)
	tool := &globTool{roots: []string{root}}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "sub", "path": root,
	}))
	if err != nil {
		t.Fatal(err)
	}
	lines := globLines(t, out)
	if len(lines) != 1 || !strings.HasSuffix(lines[0], "sub/") {
		t.Errorf("pattern \"sub\" should match the directory with a trailing slash, got:\n%s", out)
	}
}

// jsonRoundTrip is a compile-time check that the grep/glob argument
// structs survive a JSON round trip through mustJSON.
func TestSearchArgsJSON(t *testing.T) {
	for _, v := range []any{
		grepArgs{Pattern: "x", ContextLines: func() *int { i := 2; return &i }()},
		globArgs{Pattern: "**/*.go"},
	} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var back map[string]any
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
	}
}
