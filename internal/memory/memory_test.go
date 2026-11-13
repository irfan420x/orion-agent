package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/model"
)

func mustStore(t *testing.T, dir string) *FileStore {
	t.Helper()
	s, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore(%q): %v", dir, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAddGetDeleteFact(t *testing.T) {
	s := mustStore(t, t.TempDir())

	if err := s.AddFact("name", "Irfan"); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	f, ok, err := s.GetFact("name")
	if err != nil {
		t.Fatalf("GetFact: %v", err)
	}
	if !ok {
		t.Fatal("GetFact: expected fact to exist")
	}
	if f.Key != "name" || f.Value != "Irfan" {
		t.Fatalf("GetFact: got %+v, want key=name value=Irfan", f)
	}
	if f.CreatedAt.IsZero() || f.UpdatedAt.IsZero() {
		t.Fatalf("GetFact: timestamps must be set, got %+v", f)
	}

	// Missing key reports not-found without an error.
	if _, ok, err := s.GetFact("nope"); err != nil || ok {
		t.Fatalf("GetFact missing: ok=%v err=%v, want ok=false err=nil", ok, err)
	}

	if err := s.DeleteFact("name"); err != nil {
		t.Fatalf("DeleteFact: %v", err)
	}
	if _, ok, err := s.GetFact("name"); err != nil || ok {
		t.Fatalf("GetFact after delete: ok=%v err=%v, want ok=false err=nil", ok, err)
	}

	// Deleting a missing key is a no-op, not an error.
	if err := s.DeleteFact("name"); err != nil {
		t.Fatalf("DeleteFact missing key: %v", err)
	}
}

func TestAddFactUpsertPreservesCreatedAt(t *testing.T) {
	s := mustStore(t, t.TempDir())

	if err := s.AddFact("lang", "Go"); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	orig, _, err := s.GetFact("lang")
	if err != nil {
		t.Fatalf("GetFact: %v", err)
	}
	time.Sleep(2 * time.Millisecond)

	if err := s.AddFact("lang", "Go + TypeScript"); err != nil {
		t.Fatalf("AddFact upsert: %v", err)
	}
	updated, _, err := s.GetFact("lang")
	if err != nil {
		t.Fatalf("GetFact: %v", err)
	}
	if updated.Value != "Go + TypeScript" {
		t.Fatalf("upsert value: got %q, want %q", updated.Value, "Go + TypeScript")
	}
	if !updated.CreatedAt.Equal(orig.CreatedAt) {
		t.Fatalf("upsert must preserve CreatedAt: got %v, want %v", updated.CreatedAt, orig.CreatedAt)
	}
	if updated.UpdatedAt.Before(orig.UpdatedAt) {
		t.Fatalf("upsert must not move UpdatedAt backwards: got %v, was %v", updated.UpdatedAt, orig.UpdatedAt)
	}
	// Exactly one fact, not two.
	facts, err := s.ListFacts()
	if err != nil {
		t.Fatalf("ListFacts: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("ListFacts after upsert: got %d facts, want 1", len(facts))
	}
}

func TestAddFactEmptyKey(t *testing.T) {
	s := mustStore(t, t.TempDir())
	for _, key := range []string{"", "   "} {
		if err := s.AddFact(key, "v"); !errors.Is(err, ErrEmptyKey) {
			t.Fatalf("AddFact(%q): err=%v, want ErrEmptyKey", key, err)
		}
	}
	if facts, err := s.ListFacts(); err != nil || len(facts) != 0 {
		t.Fatalf("ListFacts after rejected adds: facts=%v err=%v, want empty", facts, err)
	}
}

func TestSearchFactsCaseInsensitive(t *testing.T) {
	s := mustStore(t, t.TempDir())
	for k, v := range map[string]string{
		"language":  "Go",
		"editor":    "VS Code",
		"favourite": "Golang Weekly",
	} {
		if err := s.AddFact(k, v); err != nil {
			t.Fatalf("AddFact(%q): %v", k, err)
		}
	}

	cases := []struct {
		query string
		want  []string // expected keys, any order
	}{
		{"go", []string{"language", "favourite"}},
		{"GO", []string{"language", "favourite"}},
		{"GoLaNg", []string{"favourite"}}, // "golang" is not a substring of "Go"
		{"code", []string{"editor"}},
		{"EDIT", []string{"editor"}},
		{"", []string{"language", "editor", "favourite"}},
	}
	for _, tc := range cases {
		got, err := s.SearchFacts(tc.query)
		if err != nil {
			t.Fatalf("SearchFacts(%q): %v", tc.query, err)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("SearchFacts(%q): got %d facts %v, want keys %v", tc.query, len(got), got, tc.want)
		}
		seen := map[string]bool{}
		for _, f := range got {
			seen[f.Key] = true
		}
		for _, k := range tc.want {
			if !seen[k] {
				t.Fatalf("SearchFacts(%q): missing key %q in %v", tc.query, k, got)
			}
		}
	}

	// No match returns an empty (non-nil) slice and no error.
	got, err := s.SearchFacts("zzz-no-such-thing")
	if err != nil {
		t.Fatalf("SearchFacts no-match: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("SearchFacts no-match: got %v, want empty", got)
	}
}

func TestSearchFactsSortedByUpdatedAtDesc(t *testing.T) {
	s := mustStore(t, t.TempDir())
	if err := s.AddFact("a", "shared"); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := s.AddFact("b", "shared"); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	// Touch "a" so it becomes the most recently updated.
	if err := s.AddFact("a", "shared v2"); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	got, err := s.SearchFacts("shared")
	if err != nil {
		t.Fatalf("SearchFacts: %v", err)
	}
	if len(got) != 2 || got[0].Key != "a" || got[1].Key != "b" {
		t.Fatalf("SearchFacts order: got %v, want [a b] (UpdatedAt desc)", got)
	}
}

func TestListFactsOrdering(t *testing.T) {
	s := mustStore(t, t.TempDir())
	for _, k := range []string{"zeta", "alpha", "mid"} {
		if err := s.AddFact(k, "v"); err != nil {
			t.Fatalf("AddFact(%q): %v", k, err)
		}
	}
	got, err := s.ListFacts()
	if err != nil {
		t.Fatalf("ListFacts: %v", err)
	}
	want := []string{"alpha", "mid", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("ListFacts: got %d facts, want %d", len(got), len(want))
	}
	for i, k := range want {
		if got[i].Key != k {
			t.Fatalf("ListFacts[%d]: got %q, want %q (full: %v)", i, got[i].Key, k, got)
		}
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s := mustStore(t, dir)
	if err := s.AddFact("project", "Orion"); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	if err := s.AddFact("language", "Go"); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	msgs := []model.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
	}
	for _, m := range msgs {
		if err := s.AddMessage(m); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
	}
	// Explicit Save, then close and reopen on the same dir.
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen NewFileStore: %v", err)
	}
	defer func() { _ = s2.Close() }()

	f, ok, err := s2.GetFact("project")
	if err != nil || !ok || f.Value != "Orion" {
		t.Fatalf("reopened GetFact(project): f=%+v ok=%v err=%v", f, ok, err)
	}
	facts, err := s2.ListFacts()
	if err != nil || len(facts) != 2 {
		t.Fatalf("reopened ListFacts: %v err=%v, want 2 facts", facts, err)
	}
	got, err := s2.RecentMessages(0)
	if err != nil {
		t.Fatalf("reopened RecentMessages: %v", err)
	}
	if len(got) != len(msgs) {
		t.Fatalf("reopened messages: got %d, want %d", len(got), len(msgs))
	}
	for i, m := range msgs {
		if !reflect.DeepEqual(got[i], m) {
			t.Fatalf("reopened message[%d]: got %+v, want %+v", i, got[i], m)
		}
	}
}

func TestMessageCap(t *testing.T) {
	s := mustStore(t, t.TempDir())
	for i := 0; i < 210; i++ {
		m := model.Message{Role: "user", Content: fmt.Sprintf("msg %d", i)}
		if err := s.AddMessage(m); err != nil {
			t.Fatalf("AddMessage %d: %v", i, err)
		}
	}
	got, err := s.RecentMessages(0)
	if err != nil {
		t.Fatalf("RecentMessages(0): %v", err)
	}
	if len(got) != MaxMessages {
		t.Fatalf("RecentMessages(0) after 210 adds: got %d, want %d", len(got), MaxMessages)
	}
	// Oldest 10 dropped: first retained message is "msg 10".
	if got[0].Content != "msg 10" {
		t.Fatalf("first retained message: got %q, want %q", got[0].Content, "msg 10")
	}
	if got[len(got)-1].Content != "msg 209" {
		t.Fatalf("last retained message: got %q, want %q", got[len(got)-1].Content, "msg 209")
	}
}

func TestMessageCapSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s := mustStore(t, dir)
	for i := 0; i < 210; i++ {
		if err := s.AddMessage(model.Message{Role: "user", Content: fmt.Sprintf("msg %d", i)}); err != nil {
			t.Fatalf("AddMessage %d: %v", i, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen NewFileStore: %v", err)
	}
	defer func() { _ = s2.Close() }()
	got, err := s2.RecentMessages(0)
	if err != nil {
		t.Fatalf("reopened RecentMessages: %v", err)
	}
	if len(got) != MaxMessages || got[0].Content != "msg 10" {
		t.Fatalf("reopened cap: got %d msgs, first %q, want 200 / %q", len(got), got[0].Content, "msg 10")
	}
}

func TestRecentMessagesN(t *testing.T) {
	s := mustStore(t, t.TempDir())
	for i := 0; i < 5; i++ {
		if err := s.AddMessage(model.Message{Role: "user", Content: fmt.Sprintf("m%d", i)}); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
	}
	got, err := s.RecentMessages(3)
	if err != nil {
		t.Fatalf("RecentMessages(3): %v", err)
	}
	if len(got) != 3 || got[0].Content != "m2" || got[2].Content != "m4" {
		t.Fatalf("RecentMessages(3): got %v, want [m2 m3 m4] in order", got)
	}
	// n <= 0 returns all, in chronological order.
	for _, n := range []int{0, -1} {
		all, err := s.RecentMessages(n)
		if err != nil {
			t.Fatalf("RecentMessages(%d): %v", n, err)
		}
		if len(all) != 5 || all[0].Content != "m0" || all[4].Content != "m4" {
			t.Fatalf("RecentMessages(%d): got %v, want all 5 in order", n, all)
		}
	}
	// n larger than history returns everything.
	all, err := s.RecentMessages(100)
	if err != nil || len(all) != 5 {
		t.Fatalf("RecentMessages(100): got %d err=%v, want 5", len(all), err)
	}
	// Empty history returns an empty slice, no error.
	empty := mustStore(t, t.TempDir())
	msgs, err := empty.RecentMessages(0)
	if err != nil || len(msgs) != 0 {
		t.Fatalf("RecentMessages on empty store: got %v err=%v, want empty", msgs, err)
	}
}

func TestClearMessages(t *testing.T) {
	s := mustStore(t, t.TempDir())
	if err := s.AddMessage(model.Message{Role: "user", Content: "a"}); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	if err := s.AddMessage(model.Message{Role: "assistant", Content: "b"}); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	if err := s.ClearMessages(); err != nil {
		t.Fatalf("ClearMessages: %v", err)
	}
	got, err := s.RecentMessages(0)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("after ClearMessages: got %d messages, want 0", len(got))
	}
	// Facts must survive a message clear.
	if err := s.AddFact("keep", "me"); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	if err := s.ClearMessages(); err != nil {
		t.Fatalf("ClearMessages: %v", err)
	}
	if _, ok, err := s.GetFact("keep"); err != nil || !ok {
		t.Fatalf("fact lost after ClearMessages: ok=%v err=%v", ok, err)
	}
}

func TestOperationsAfterCloseError(t *testing.T) {
	s := mustStore(t, t.TempDir())
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, _, err := s.GetFact("k"); !errors.Is(err, ErrClosed) {
		t.Fatalf("GetFact after Close: err=%v, want ErrClosed", err)
	}
	for name, fn := range map[string]func() error{
		"AddFact":         func() error { return s.AddFact("k", "v") },
		"DeleteFact":      func() error { return s.DeleteFact("k") },
		"SearchFacts":     func() error { _, err := s.SearchFacts("k"); return err },
		"ListFacts":       func() error { _, err := s.ListFacts(); return err },
		"AddMessage":      func() error { return s.AddMessage(model.Message{Role: "user", Content: "x"}) },
		"RecentMessages":  func() error { _, err := s.RecentMessages(1); return err },
		"ReplaceMessages": func() error { return s.ReplaceMessages(nil) },
		"ClearMessages":   func() error { return s.ClearMessages() },
		"Save":            func() error { return s.Save() },
	} {
		if err := fn(); !errors.Is(err, ErrClosed) {
			t.Fatalf("%s after Close: err=%v, want ErrClosed", name, err)
		}
	}
	// Close itself is idempotent.
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v, want nil", err)
	}
}

func TestCorruptJSONReturnsError(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "memory.json")
	if err := os.WriteFile(bad, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	if _, err := NewFileStore(dir); err == nil {
		t.Fatal("NewFileStore on corrupt JSON: got nil error, want error")
	}
	// The corrupt file must not have been silently overwritten/discarded.
	data, err := os.ReadFile(bad)
	if err != nil {
		t.Fatalf("read corrupt file after failed open: %v", err)
	}
	if string(data) != "{not valid json" {
		t.Fatalf("corrupt file was modified: got %q", string(data))
	}
}

func TestEmptyFileIsFresh(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "memory.json"), []byte{}, 0o600); err != nil {
		t.Fatalf("write empty file: %v", err)
	}
	s, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore on empty file: %v", err)
	}
	defer func() { _ = s.Close() }()
	if facts, err := s.ListFacts(); err != nil || len(facts) != 0 {
		t.Fatalf("fresh store facts: %v err=%v, want empty", facts, err)
	}
	if err := s.AddFact("k", "v"); err != nil {
		t.Fatalf("AddFact on fresh store: %v", err)
	}
}

func TestFileAndDirPermissions(t *testing.T) {
	// The store root must not exist yet: NewFileStore creates it
	// (0700). (t.TempDir() itself is pre-existing, so opening a
	// store directly on it would test the "leave pre-existing dirs
	// alone" rule instead — see
	// TestNewFileStoreLeavesPreExistingDirMode.)
	dir := filepath.Join(t.TempDir(), "store")
	s := mustStore(t, dir)
	if err := s.AddFact("k", "v"); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, "memory.json"))
	if err != nil {
		t.Fatalf("stat memory.json: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("memory.json mode: got %o, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat data dir: %v", err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("data dir mode: got %o, want 0700", di.Mode().Perm())
	}
	// No stray temp files left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "memory.json" {
		t.Fatalf("data dir contents: %v, want only memory.json", entries)
	}
}

func TestNewFileStoreCreatesNestedDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", "c")
	s, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore nested dir: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.AddFact("k", "v"); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "memory.json")); err != nil {
		t.Fatalf("memory.json missing in nested dir: %v", err)
	}
}

// TestKillRestartSurvival simulates a kill -9: the first store is
// abandoned WITHOUT Close (no graceful shutdown, no final Save) and a
// fresh store instance is opened over the same on-disk file. Every
// fact and every message recorded before the "kill" must survive
// intact, because each mutation persists synchronously.
func TestKillRestartSurvival(t *testing.T) {
	dir := t.TempDir()

	// Deliberately no Close: this store is "killed", not shut down.
	s1, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	facts := map[string]string{
		"user":     "Irfan",
		"project":  "Orion",
		"language": "Go",
	}
	for k, v := range facts {
		if err := s1.AddFact(k, v); err != nil {
			t.Fatalf("AddFact(%q): %v", k, err)
		}
	}
	msgs := []model.Message{
		{Role: "system", Content: "you are Orion"},
		{Role: "user", Content: "remember this"},
		{Role: "assistant", Content: "noted", ToolCalls: []model.ToolCall{{ID: "call-1", Name: "remember", Arguments: `{"k":"v"}`}}},
		{Role: "tool", Content: "ok", ToolCallID: "call-1"},
	}
	for _, m := range msgs {
		if err := s1.AddMessage(m); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
	}
	// Capture pre-kill timestamps for exact comparison.
	wantFacts := map[string]Fact{}
	for k := range facts {
		f, ok, err := s1.GetFact(k)
		if err != nil || !ok {
			t.Fatalf("pre-kill GetFact(%q): f=%+v ok=%v err=%v", k, f, ok, err)
		}
		wantFacts[k] = f
	}
	// s1 is abandoned here: no Close, no Save. Simulates kill -9.

	s2, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen after kill: NewFileStore: %v", err)
	}
	defer func() { _ = s2.Close() }()

	gotFacts, err := s2.ListFacts()
	if err != nil {
		t.Fatalf("reopened ListFacts: %v", err)
	}
	if len(gotFacts) != len(wantFacts) {
		t.Fatalf("reopened facts: got %d, want %d", len(gotFacts), len(wantFacts))
	}
	for _, f := range gotFacts {
		want, ok := wantFacts[f.Key]
		if !ok {
			t.Fatalf("reopened fact %q was not recorded before the kill", f.Key)
		}
		if !reflect.DeepEqual(f, want) {
			t.Fatalf("reopened fact %q: got %+v, want %+v (timestamps must survive)", f.Key, f, want)
		}
	}
	gotMsgs, err := s2.RecentMessages(0)
	if err != nil {
		t.Fatalf("reopened RecentMessages: %v", err)
	}
	if len(gotMsgs) != len(msgs) {
		t.Fatalf("reopened messages: got %d, want %d", len(gotMsgs), len(msgs))
	}
	for i, m := range msgs {
		if !reflect.DeepEqual(gotMsgs[i], m) {
			t.Fatalf("reopened message[%d]: got %+v, want %+v", i, gotMsgs[i], m)
		}
	}
	// The reopened store must keep working after the "restart".
	if err := s2.AddFact("after-restart", "yes"); err != nil {
		t.Fatalf("AddFact after restart: %v", err)
	}
	if _, ok, err := s2.GetFact("after-restart"); err != nil || !ok {
		t.Fatalf("GetFact after restart: ok=%v err=%v", ok, err)
	}
}

// TestCorruptFileVariants covers the ways a memory file can be damaged
// in practice — garbage, a torn/truncated write, a wrong top-level
// shape, a wrong field type, and a version from a newer Orion. Opening
// must fail with an error (never panic, never silently discard) and
// must leave the damaged file untouched for manual recovery.
func TestCorruptFileVariants(t *testing.T) {
	valid := `{"version": 1, "facts": [{"key": "k", "value": "v", "created_at": "2026-11-13T00:00:00Z", "updated_at": "2026-11-13T00:00:00Z"}], "messages": [{"role": "user", "content": "hi"}]}`
	cases := map[string]string{
		"garbage":             "{not valid json",
		"truncated":           valid[:len(valid)/2], // torn write mid-file
		"truncated-at-brace":  valid[:len(valid)-3],
		"empty-object":        "{}",
		"top-level-array":     `[]`,
		"top-level-string":    `"hello"`,
		"facts-wrong-type":    `{"version": 1, "facts": "nope", "messages": []}`,
		"fact-wrong-type":     `{"version": 1, "facts": [42], "messages": []}`,
		"messages-wrong-type": `{"version": 1, "facts": [], "messages": "nope"}`,
		"version-wrong-type":  `{"version": "x", "facts": [], "messages": []}`,
		"version-too-new":     `{"version": 99, "facts": [], "messages": []}`,
	}
	// "{}" is not corrupt: an empty object is a valid empty store.
	for name, contents := range cases {
		if name == "empty-object" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "memory.json")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatalf("write damaged file: %v", err)
			}
			// Recover from any panic: a damaged file must never crash the process.
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("NewFileStore panicked on %q: %v", name, r)
				}
			}()
			if _, err := NewFileStore(dir); err == nil {
				t.Fatalf("NewFileStore on %s file: got nil error, want error", name)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read damaged file after failed open: %v", err)
			}
			if string(after) != contents {
				t.Fatalf("damaged file was modified: got %q, want %q", string(after), contents)
			}
		})
	}

	// An empty JSON object is a valid, empty store (not corruption).
	t.Run("empty-object", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "memory.json"), []byte("{}"), 0o600); err != nil {
			t.Fatalf("write empty object: %v", err)
		}
		s, err := NewFileStore(dir)
		if err != nil {
			t.Fatalf("NewFileStore on {}: %v", err)
		}
		defer func() { _ = s.Close() }()
		if facts, err := s.ListFacts(); err != nil || len(facts) != 0 {
			t.Fatalf("empty-object store facts: %v err=%v, want empty", facts, err)
		}
	})
}

// TestConcurrentAccess hammers one store from many goroutines —
// concurrent fact writes, message appends, and reads — and then
// reopens the file to prove every committed write survived. Run under
// -race: any unsynchronised access fails the test.
func TestConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	s := mustStore(t, dir)

	const writers = 8
	const perWriter = 25
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				key := fmt.Sprintf("w%d-k%d", w, i)
				if err := s.AddFact(key, "v"); err != nil {
					t.Errorf("AddFact(%q): %v", key, err)
					return
				}
				if err := s.AddMessage(model.Message{Role: "user", Content: key}); err != nil {
					t.Errorf("AddMessage(%q): %v", key, err)
					return
				}
				if _, _, err := s.GetFact(key); err != nil {
					t.Errorf("GetFact(%q): %v", key, err)
					return
				}
				if _, err := s.RecentMessages(5); err != nil {
					t.Errorf("RecentMessages: %v", err)
					return
				}
				if _, err := s.SearchFacts("w"); err != nil {
					t.Errorf("SearchFacts: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	facts, err := s.ListFacts()
	if err != nil {
		t.Fatalf("ListFacts: %v", err)
	}
	if len(facts) != writers*perWriter {
		t.Fatalf("facts after concurrent writes: got %d, want %d", len(facts), writers*perWriter)
	}
	msgs, err := s.RecentMessages(0)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	if len(msgs) != writers*perWriter {
		t.Fatalf("messages after concurrent writes: got %d, want %d", len(msgs), writers*perWriter)
	}

	// Reopen: every concurrent write must be on disk.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()
	facts2, err := s2.ListFacts()
	if err != nil || len(facts2) != writers*perWriter {
		t.Fatalf("reopened facts: got %d err=%v, want %d", len(facts2), err, writers*perWriter)
	}
}

// ---------------------------------------------------------------------
// Follow-up fixes (P1-15, P2-19, P2-20) — 2026-11-13.
// ---------------------------------------------------------------------

// TestNewFileStoreLeavesPreExistingDirMode (P1-15): NewFileStore must
// never chmod a directory it did not create — including when data_dir
// points at an existing directory with relaxed permissions.
func TestNewFileStoreLeavesPreExistingDirMode(t *testing.T) {
	dir := t.TempDir()
	// t.TempDir() creates the dir 0700; loosen it to simulate a
	// shared or user-owned directory.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("Chmod temp dir: %v", err)
	}
	s := mustStore(t, dir)
	if err := s.AddFact("k", "v"); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	if mode := statMode(t, dir); mode != 0o755 {
		t.Errorf("pre-existing data dir mode = %o, want 0755 (unchanged)", mode)
	}
	if mode := statMode(t, filepath.Join(dir, "memory.json")); mode != 0o600 {
		t.Errorf("memory.json mode = %o, want 0600", mode)
	}

	// A pre-existing parent keeps its mode while a newly created
	// child gets 0700.
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatalf("Chmod parent: %v", err)
	}
	nested := filepath.Join(parent, "new", "child")
	s2, err := NewFileStore(nested)
	if err != nil {
		t.Fatalf("NewFileStore nested: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if mode := statMode(t, parent); mode != 0o755 {
		t.Errorf("pre-existing parent mode = %o, want 0755 (unchanged)", mode)
	}
	for _, d := range []string{filepath.Join(parent, "new"), nested} {
		if mode := statMode(t, d); mode != 0o700 {
			t.Errorf("newly created dir %q mode = %o, want 0700", d, mode)
		}
	}
}

func statMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	return fi.Mode().Perm()
}

// TestAddFactTrimsKey (P2-19): a key with surrounding whitespace is
// stored trimmed, so " key " and "key" are the same fact.
func TestAddFactTrimsKey(t *testing.T) {
	s := mustStore(t, t.TempDir())

	if err := s.AddFact("  spaced  ", "v1"); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	// Both spellings find the fact, stored under the trimmed key.
	for _, k := range []string{"spaced", "  spaced  ", "\tspaced\n"} {
		f, ok, err := s.GetFact(k)
		if err != nil || !ok {
			t.Fatalf("GetFact(%q): ok=%v err=%v, want the fact", k, ok, err)
		}
		if f.Key != "spaced" {
			t.Fatalf("GetFact(%q): stored key = %q, want %q", k, f.Key, "spaced")
		}
	}
	// An upsert under the untrimmed spelling updates the same fact,
	// not a second one.
	if err := s.AddFact("spaced", "v2"); err != nil {
		t.Fatalf("AddFact upsert: %v", err)
	}
	facts, err := s.ListFacts()
	if err != nil {
		t.Fatalf("ListFacts: %v", err)
	}
	if len(facts) != 1 || facts[0].Key != "spaced" || facts[0].Value != "v2" {
		t.Fatalf("ListFacts after trimmed upsert: got %v, want one fact spaced=v2", facts)
	}
	// Delete also matches the trimmed key.
	if err := s.DeleteFact("  spaced "); err != nil {
		t.Fatalf("DeleteFact: %v", err)
	}
	if _, ok, err := s.GetFact("spaced"); err != nil || ok {
		t.Fatalf("GetFact after trimmed delete: ok=%v err=%v, want not-found", ok, err)
	}

	// A fact written untrimmed by an older version is normalised on load.
	dir := t.TempDir()
	raw := `{"version": 1, "facts": [{"key": "  legacy  ", "value": "v", "created_at": "2026-11-13T00:00:00Z", "updated_at": "2026-11-13T00:00:00Z"}], "messages": []}`
	if err := os.WriteFile(filepath.Join(dir, "memory.json"), []byte(raw), 0o600); err != nil {
		t.Fatalf("write legacy file: %v", err)
	}
	s2, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore on legacy file: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if _, ok, err := s2.GetFact("legacy"); err != nil || !ok {
		t.Fatalf("GetFact(legacy) after load: ok=%v err=%v, want found", ok, err)
	}
}

// TestMessageTruncationCopiesBackingArray (P2-20): truncating history
// to MaxMessages must copy, so the dropped messages are not kept alive
// by the backing array. The test reads the unexported slice (same
// package) and asserts its capacity is bounded by the cap.
func TestMessageTruncationCopiesBackingArray(t *testing.T) {
	s := mustStore(t, t.TempDir())
	for i := 0; i < MaxMessages+10; i++ {
		if err := s.AddMessage(model.Message{Role: "user", Content: fmt.Sprintf("msg %d", i)}); err != nil {
			t.Fatalf("AddMessage %d: %v", i, err)
		}
	}
	if got := len(s.messages); got != MaxMessages {
		t.Fatalf("len(messages) = %d, want %d", got, MaxMessages)
	}
	if got := cap(s.messages); got > MaxMessages {
		t.Fatalf("cap(messages) = %d, want <= %d: truncation re-sliced without copying, dropped messages stay alive", got, MaxMessages)
	}
	if s.messages[0].Content != "msg 10" {
		t.Fatalf("first retained message = %q, want %q", s.messages[0].Content, "msg 10")
	}

	// The load path must copy on truncate too: a file holding more
	// than MaxMessages (e.g. written by an older version) must load
	// capped without keeping the dropped messages alive.
	dir := t.TempDir()
	var over []model.Message
	for i := 0; i < MaxMessages+10; i++ {
		over = append(over, model.Message{Role: "user", Content: fmt.Sprintf("msg %d", i)})
	}
	raw, err := json.Marshal(persisted{Version: 1, Facts: []Fact{}, Messages: over})
	if err != nil {
		t.Fatalf("marshal over-cap file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.json"), raw, 0o600); err != nil {
		t.Fatalf("write over-cap file: %v", err)
	}
	s3, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen over-cap file: %v", err)
	}
	defer func() { _ = s3.Close() }()
	if got := len(s3.messages); got != MaxMessages {
		t.Fatalf("loaded len(messages) = %d, want %d", got, MaxMessages)
	}
	if got := cap(s3.messages); got > MaxMessages {
		t.Fatalf("loaded cap(messages) = %d, want <= %d: load truncated without copying", got, MaxMessages)
	}
	if s3.messages[0].Content != "msg 10" {
		t.Fatalf("loaded first message = %q, want %q", s3.messages[0].Content, "msg 10")
	}
}
