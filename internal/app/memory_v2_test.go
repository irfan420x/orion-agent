// Integration tests for Memory v2 wiring: the typed-store adapter
// (core's recall v2 source), the App-level typed-memory helpers, and
// the /compress variant that draws on session memory.
package app

import (
	"context"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
)

func mustTypedStoreForApp(t *testing.T, dir string) *memory.TypedStore {
	t.Helper()
	s, err := memory.NewTypedStore(dir)
	if err != nil {
		t.Fatalf("NewTypedStore(%q): %v", dir, err)
	}
	return s
}

func TestTypedStoreAdapterFiltersStale(t *testing.T) {
	dir := t.TempDir()
	ts := mustTypedStoreForApp(t, dir)
	live := memory.TypedMemory{
		Name:        "Favourite number",
		Description: "The user's favourite number is 17",
		Type:        memory.TypeProject,
		Body:        "Favourite number is 17.",
	}
	stale := memory.TypedMemory{
		Name:        "Old deploy path",
		Description: "Deploy via the old script",
		Type:        memory.TypeProject,
		Body:        "Run `scripts/deploy.sh` to deploy.",
	}
	if err := ts.Save(live); err != nil {
		t.Fatalf("Save live: %v", err)
	}
	if err := ts.Save(stale); err != nil {
		t.Fatalf("Save stale: %v", err)
	}
	if err := ts.MarkStale("Old deploy path"); err != nil {
		t.Fatalf("MarkStale: %v", err)
	}
	ad := newTypedStoreAdapter(ts)
	listed, err := ad.ListTypedMemories()
	if err != nil {
		t.Fatalf("ListTypedMemories: %v", err)
	}
	if len(listed) != 1 || listed[0].Name != "Favourite number" {
		t.Fatalf("ListTypedMemories: got %+v, want only the live memory", listed)
	}
	if listed[0].Type != "project" {
		t.Fatalf("Type conversion: got %q want %q", listed[0].Type, "project")
	}
	got, err := ad.LoadTypedMemory("Favourite number")
	if err != nil {
		t.Fatalf("LoadTypedMemory: %v", err)
	}
	if got.Body != "Favourite number is 17." {
		t.Fatalf("LoadTypedMemory body: %q", got.Body)
	}
	if _, err := ad.LoadTypedMemory("No such memory"); err == nil {
		t.Fatalf("LoadTypedMemory missing: no error")
	}
	// The persistent stale marker goes through the adapter too.
	if err := ad.MarkTypedMemoryStale("Favourite number"); err != nil {
		t.Fatalf("MarkTypedMemoryStale: %v", err)
	}
	after, found, err := ts.Load("Favourite number")
	if err != nil || !found || !after.Stale {
		t.Fatalf("stale not persisted: found=%v stale=%v err=%v", found, after.Stale, err)
	}
	listed, err = ad.ListTypedMemories()
	if err != nil {
		t.Fatalf("ListTypedMemories after mark: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("ListTypedMemories after mark: got %d, want 0", len(listed))
	}
}

func TestAppTypedMemoryHelpers(t *testing.T) {
	a := testApp(t)
	// Unwired App degrades gracefully: empty listing, "" session
	// memory, and a clear error on forget.
	if got, err := a.TypedMemories(); err != nil || len(got) != 0 {
		t.Fatalf("unwired TypedMemories: %v, %v", got, err)
	}
	if _, err := a.ForgetTypedMemory("x"); err == nil {
		t.Fatalf("unwired ForgetTypedMemory: no error")
	}
	if got, err := a.SessionMemoryText(context.Background()); err != nil || got != "" {
		t.Fatalf("unwired SessionMemoryText: %q, %v", got, err)
	}
	// Wire the store + session log directly (same package).
	dir := a.Config.DataDir
	a.typedStore = mustTypedStoreForApp(t, dir)
	slog, err := memory.NewSessionLog(dir)
	if err != nil {
		t.Fatalf("NewSessionLog: %v", err)
	}
	a.sessionLog = slog
	if err := a.typedStore.Save(memory.TypedMemory{
		Name:        "Favourite number",
		Description: "The user's favourite number is 17",
		Type:        memory.TypeProject,
		Body:        "Favourite number is 17.",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	listed, err := a.TypedMemories()
	if err != nil || len(listed) != 1 || listed[0].Name != "Favourite number" {
		t.Fatalf("TypedMemories: %+v, %v", listed, err)
	}
	if err := slog.Append("Decision: use DeepSeek as the default provider."); err != nil {
		t.Fatalf("SessionLog Append: %v", err)
	}
	text, err := a.SessionMemoryText(context.Background())
	if err != nil || !strings.Contains(text, "DeepSeek") {
		t.Fatalf("SessionMemoryText: %q, %v", text, err)
	}
	name, err := a.ForgetTypedMemory("Favourite number")
	if err != nil || name != "Favourite number" {
		t.Fatalf("ForgetTypedMemory: %q, %v", name, err)
	}
	listed, err = a.TypedMemories()
	if err != nil || len(listed) != 0 {
		t.Fatalf("TypedMemories after forget: %+v, %v", listed, err)
	}
}

func TestCompressWithSessionMemory(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	// 12 messages so compression has something to drop (keep 10).
	for i := 0; i < 12; i++ {
		if err := a.Memory.AddMessage(model.Message{Role: "user", Content: "turn message"}); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
	}
	before, after, _, err := a.CompressWithSessionMemory(ctx, 10, "## 2026-11-13\nDecision: use DeepSeek as the default provider.")
	if err != nil {
		t.Fatalf("CompressWithSessionMemory: %v", err)
	}
	if before != 12 || after != 10 {
		t.Fatalf("CompressWithSessionMemory: before=%d after=%d, want 12/10", before, after)
	}
	fact, found, err := a.Memory.GetFact("conversation-summary")
	if err != nil || !found {
		t.Fatalf("summary fact: found=%v err=%v", found, err)
	}
	if !strings.Contains(fact.Value, "(Drew on session memory.)") {
		t.Fatalf("summary fact lacks the session-memory citation: %q", fact.Value)
	}
	// Plain Compress (empty session memory) has no citation.
	a2 := testApp(t)
	for i := 0; i < 12; i++ {
		if err := a2.Memory.AddMessage(model.Message{Role: "user", Content: "turn message"}); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
	}
	if _, _, _, err := a2.Compress(ctx, 10); err != nil {
		t.Fatalf("Compress: %v", err)
	}
	fact, found, err = a2.Memory.GetFact("conversation-summary")
	if err != nil || !found {
		t.Fatalf("plain summary fact: found=%v err=%v", found, err)
	}
	if strings.Contains(fact.Value, "session memory") {
		t.Fatalf("plain Compress cited session memory: %q", fact.Value)
	}
}
