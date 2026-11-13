// Audit 4 B P1-3 regression tests: drift verification must not
// permanently kill valid memories on external flags/symbols.
package core

import (
	"context"
	"testing"
)

// TestAudit4DriftFalsePositive: the Workstream B probe memories —
// external-tool flags and a prose symbol with no code context —
// must not be stale, and must not be persisted stale.
func TestAudit4DriftFalsePositive(t *testing.T) {
	root := t.TempDir()
	writeTreeFile(t, root, "main.go", "package main\nfunc RunAgent() {}\n")
	cases := []TypedMemory{
		{Name: "deploy", Body: "Deploy with --prod flag and use --verbose output"},
		{Name: "start", Body: "Call Start( to begin the job"},
		{Name: "rm-flag", Body: "Always pass --no-preserve-root when testing"},
	}
	for _, mem := range cases {
		if stale, missing := driftCheck(root, mem); stale {
			t.Errorf("driftCheck(%q) stale=true, missing=%v, want not stale (no code context: external flag/prose symbol, not a tree claim)", mem.Body, missing)
		}
	}
}

// TestAudit4DriftFlagSymbolOnlyNotPersisted: a memory condemned on
// flag/symbol-only evidence (backticked, so it IS a claim) is
// dropped for the turn but never persisted stale, so it can
// resurface; a memory condemned on a missing file IS persisted.
func TestAudit4DriftFlagSymbolOnlyNotPersisted(t *testing.T) {
	a := newV2TestAgent(t)
	root := t.TempDir()
	writeTreeFile(t, root, "main.go", "package main\n")
	a.SetDriftRoot(root)
	src := &fakeTypedSource{memories: []TypedMemory{
		{Name: "flag-only", Description: "external flag", Type: "project", Body: "Pass `--no-such-flag` to enable it."},
		{Name: "file-gone", Description: "deleted file", Type: "reference", Body: "See `internal/core/deleted.go`."},
		{Name: "good-one", Description: "favourite number", Type: "user", Body: "Favourite number is 17."},
	}}
	a.SetTypedMemorySource(src)
	a.SetMemorySelector(func(ctx context.Context, manifest []TypedMemory, input string, recentTools []string, surfaced []string) ([]string, error) {
		return []string{"flag-only", "file-gone", "good-one"}, nil
	})
	got := a.recallTypedFor(context.Background(), "guide", nil)
	if len(got) != 1 || got[0].Name != "good-one" {
		t.Fatalf("recallTypedFor = %v, want only [good-one]", namesOf(got))
	}
	for _, n := range src.stale {
		if n == "flag-only" {
			t.Errorf("flag-only memory was persisted stale (src.stale=%v); flag/symbol-only drift evidence must never persist", src.stale)
		}
	}
	found := false
	for _, n := range src.stale {
		if n == "file-gone" {
			found = true
		}
	}
	if !found {
		t.Errorf("file-gone memory was not persisted stale (src.stale=%v); missing-file evidence must persist", src.stale)
	}
	// The flag-only memory is re-verified on the next turn (it was
	// never marked stale in-memory either), while the file-gone one
	// stays dropped.
	if a.isStaleMemory("flag-only") {
		t.Error("flag-only memory is in the in-memory stale set; flag/symbol-only evidence must not mark it stale")
	}
	if !a.isStaleMemory("file-gone") {
		t.Error("file-gone memory is not in the in-memory stale set")
	}
}
