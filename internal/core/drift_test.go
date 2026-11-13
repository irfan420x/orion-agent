package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTreeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExtractReferences(t *testing.T) {
	body := "See `internal/core/agent.go` and use `Run(` after checking " +
		"`--verbose`. Also internal/memory/memory.go, call NewAgent( for setup, " +
		"and pass --max-steps 5. Plain words (like this) are not references."
	got := extractReferences(body)
	byKind := map[refKind]map[string]bool{}
	for _, r := range got {
		if byKind[r.Kind] == nil {
			byKind[r.Kind] = map[string]bool{}
		}
		byKind[r.Kind][r.Text] = true
	}
	for _, want := range []string{"internal/core/agent.go", "internal/memory/memory.go"} {
		if !byKind[refFile][want] {
			t.Errorf("missing file ref %q in %v", want, got)
		}
	}
	for _, want := range []string{"Run", "NewAgent"} {
		if !byKind[refSymbol][want] {
			t.Errorf("missing symbol ref %q in %v", want, got)
		}
	}
	for _, want := range []string{"--verbose", "--max-steps"} {
		if !byKind[refFlag][want] {
			t.Errorf("missing flag ref %q in %v", want, got)
		}
	}
	if len(got) == 0 {
		t.Fatal("no references extracted")
	}
	// Deduped.
	seen := map[codeReference]int{}
	for _, r := range got {
		seen[r]++
		if seen[r] > 1 {
			t.Errorf("duplicate reference %v", r)
		}
	}
	// No references in plain prose.
	if got := extractReferences("Hello, how are you today?"); len(got) != 0 {
		t.Errorf("extractReferences(plain prose) = %v, want none", got)
	}
}

func TestDriftCheckExistingVsMissing(t *testing.T) {
	root := t.TempDir()
	writeTreeFile(t, root, "internal/core/agent.go", "package core\nfunc Run() {}\n")
	writeTreeFile(t, root, "internal/core/recall.go", "package core\n")

	// Everything exists: not stale.
	mem := TypedMemory{Name: "good", Body: "See `internal/core/agent.go` and call `Run(` with --verbose after adding it to agent.go."}
	// --verbose does not exist anywhere in the tree, so craft a
	// body without the flag for the clean case.
	mem.Body = "See `internal/core/agent.go` and call `Run(`."
	if stale, missing := driftCheck(root, mem); stale {
		t.Errorf("driftCheck(existing refs) stale=true, missing=%v, want not stale", missing)
	}

	// Missing file: stale.
	mem = TypedMemory{Name: "gone-file", Body: "See `internal/core/deleted.go` for details."}
	stale, missing := driftCheck(root, mem)
	if !stale || len(missing) != 1 || missing[0].Text != "internal/core/deleted.go" {
		t.Errorf("driftCheck(missing file) stale=%v missing=%v, want stale with deleted.go", stale, missing)
	}

	// Missing symbol: stale (no file in the tree mentions NoSuchFunc).
	mem = TypedMemory{Name: "gone-symbol", Body: "Call `NoSuchFunc(` to start."}
	stale, missing = driftCheck(root, mem)
	if !stale {
		t.Errorf("driftCheck(missing symbol) stale=false, want true (missing=%v)", missing)
	}

	// Missing flag: stale.
	mem = TypedMemory{Name: "gone-flag", Body: "Pass --no-such-flag to enable it."}
	stale, missing = driftCheck(root, mem)
	if !stale {
		t.Errorf("driftCheck(missing flag) stale=false, want true (missing=%v)", missing)
	}

	// Flag that does exist in the tree: not stale.
	writeTreeFile(t, root, "cmd/orion/main.go", "// --max-steps is the step budget\n")
	mem = TypedMemory{Name: "real-flag", Body: "Pass --max-steps to raise the budget."}
	if stale, missing := driftCheck(root, mem); stale {
		t.Errorf("driftCheck(existing flag) stale=true, missing=%v, want not stale", missing)
	}

	// No references at all: never stale.
	mem = TypedMemory{Name: "plain", Body: "User prefers short answers."}
	if stale, _ := driftCheck(root, mem); stale {
		t.Errorf("driftCheck(no refs) stale=true, want false")
	}
}

func TestDriftCheckMarksStaleAndDrops(t *testing.T) {
	a := newV2TestAgent(t)
	root := t.TempDir()
	writeTreeFile(t, root, "internal/core/agent.go", "package core\n")
	a.SetDriftRoot(root)
	src := &fakeTypedSource{memories: []TypedMemory{
		{Name: "stale-one", Description: "deleted file guide", Type: "reference", Body: "See `internal/core/deleted.go`."},
		{Name: "good-one", Description: "favourite number", Type: "user", Body: "Favourite number is 17."},
	}}
	// Wire the source that records stale marks.
	a.SetTypedMemorySource(src)
	a.SetMemorySelector(func(ctx context.Context, manifest []TypedMemory, input string, recentTools []string, surfaced []string) ([]string, error) {
		return []string{"stale-one", "good-one"}, nil
	})
	got := a.recallTypedFor(context.Background(), "guide", nil)
	if len(got) != 1 || got[0].Name != "good-one" {
		t.Fatalf("recallTypedFor = %v, want only [good-one] (stale dropped)", got)
	}
	if !a.isStaleMemory("stale-one") {
		t.Errorf("stale-one not in in-memory stale set")
	}
	found := false
	for _, n := range src.stale {
		if n == "stale-one" {
			found = true
		}
	}
	if !found {
		t.Errorf("stale-one not reported to the store's stale marker (got %v)", src.stale)
	}
	// The stale memory stays dropped on the next turn even if the
	// selector returns it again.
	got = a.recallTypedFor(context.Background(), "guide", nil)
	for _, m := range got {
		if m.Name == "stale-one" {
			t.Errorf("stale memory re-injected on second turn: %v", got)
		}
	}
}

func TestDriftCheckUnreadableRootIsNotStale(t *testing.T) {
	// A root that does not exist degrades to "not stale" rather
	// than blocking recall: verification cannot run, so the memory
	// is injected (current state unknown, not proven stale).
	mem := TypedMemory{Name: "x", Body: "See `internal/core/agent.go`."}
	if stale, _ := driftCheck(filepath.Join(t.TempDir(), "no-such-dir"), mem); stale {
		t.Errorf("driftCheck(unreadable root) stale=true, want false (degrade, do not block)")
	}
}

func TestGrepTreeSkipsHiddenDirs(t *testing.T) {
	root := t.TempDir()
	writeTreeFile(t, root, ".git/config", "HiddenSymbolHere\n")
	writeTreeFile(t, root, "main.go", "package main\n")
	// A symbol that only exists under .git must NOT count as found.
	got := grepTree(root, []codeReference{{Kind: refSymbol, Text: "HiddenSymbolHere"}})
	if got[codeReference{Kind: refSymbol, Text: "HiddenSymbolHere"}] {
		t.Errorf("grepTree found a symbol that only exists under .git")
	}
	// A symbol in a normal file is found.
	writeTreeFile(t, root, "main.go", "package main\n// HiddenSymbolHere is documented\n")
	got = grepTree(root, []codeReference{{Kind: refSymbol, Text: "HiddenSymbolHere"}})
	if !got[codeReference{Kind: refSymbol, Text: "HiddenSymbolHere"}] {
		t.Errorf("grepTree missed a symbol present in main.go")
	}
}

func TestFormatMissingRefs(t *testing.T) {
	s := formatMissingRefs([]codeReference{{Kind: refFile, Text: "a.go"}, {Kind: refFlag, Text: "--x"}})
	if !strings.Contains(s, "file:a.go") || !strings.Contains(s, "flag:--x") {
		t.Errorf("formatMissingRefs = %q", s)
	}
}
