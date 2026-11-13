package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustTypedStore(t *testing.T, dir string) *TypedStore {
	t.Helper()
	s, err := NewTypedStore(dir)
	if err != nil {
		t.Fatalf("NewTypedStore(%q): %v", dir, err)
	}
	return s
}

func TestParseTypedMemoryRoundTrip(t *testing.T) {
	m := TypedMemory{
		Name:        "Favourite editor",
		Description: "Irfan prefers Zed over VS Code for Go work",
		Type:        TypeUser,
		Body:        "Prefers Zed.\n\nUses VS Code only for debugging.",
	}
	data, err := MarshalTypedMemory(m)
	if err != nil {
		t.Fatalf("MarshalTypedMemory: %v", err)
	}
	parsed, err := ParseTypedMemory(data)
	if err != nil {
		t.Fatalf("ParseTypedMemory: %v", err)
	}
	if parsed.Name != m.Name || parsed.Description != m.Description || parsed.Type != m.Type || parsed.Body != m.Body {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", parsed, m)
	}
}

func TestParseTypedMemoryQuotedValues(t *testing.T) {
	// A description containing a colon must survive the round trip.
	m := TypedMemory{
		Name:        "Deploy checklist",
		Description: "Steps: build, test, then deploy to prod",
		Type:        TypeProject,
		Body:        "Build first.",
	}
	data, err := MarshalTypedMemory(m)
	if err != nil {
		t.Fatalf("MarshalTypedMemory: %v", err)
	}
	parsed, err := ParseTypedMemory(data)
	if err != nil {
		t.Fatalf("ParseTypedMemory: %v", err)
	}
	if parsed.Description != m.Description {
		t.Fatalf("description: got %q want %q", parsed.Description, m.Description)
	}
}

func TestParseTypedMemoryBadInput(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"no frontmatter", "Just a body, no frontmatter at all.\n"},
		{"unclosed frontmatter", "---\nname: X\ndescription: Y\ntype: user\n"},
		{"missing name", "---\ndescription: Y\ntype: user\n---\nBody.\n"},
		{"missing description", "---\nname: X\ntype: user\n---\nBody.\n"},
		{"missing type", "---\nname: X\ndescription: Y\n---\nBody.\n"},
		{"unknown type", "---\nname: X\ndescription: Y\ntype: secret\n---\nBody.\n"},
		{"empty type", "---\nname: X\ndescription: Y\ntype: \n---\nBody.\n"},
		{"empty body", "---\nname: X\ndescription: Y\ntype: user\n---\n"},
		{"whitespace body", "---\nname: X\ndescription: Y\ntype: user\n---\n   \n\n"},
		{"malformed line", "---\nname X\ndescription: Y\ntype: user\n---\nBody.\n"},
		{"empty name", "---\nname:   \ndescription: Y\ntype: user\n---\nBody.\n"},
	}
	for _, c := range cases {
		if _, err := ParseTypedMemory([]byte(c.text)); err == nil {
			t.Errorf("%s: expected an error, got none", c.name)
		}
	}
}

func TestParseMemoryType(t *testing.T) {
	for _, typ := range AllMemoryTypes {
		if got, err := ParseMemoryType(string(typ)); err != nil || got != typ {
			t.Errorf("ParseMemoryType(%q): got %q err %v", typ, got, err)
		}
	}
	if _, err := ParseMemoryType("team"); err == nil {
		t.Error("ParseMemoryType(team): expected an error, got none")
	}
	if _, err := ParseMemoryType(""); err == nil {
		t.Error("ParseMemoryType(\"\"): expected an error, got none")
	}
	// Case-insensitive, but never silently defaulted.
	if got, err := ParseMemoryType("User"); err != nil || got != TypeUser {
		t.Errorf("ParseMemoryType(User): got %q err %v", got, err)
	}
}

func TestSlugify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Favourite editor", "favourite-editor"},
		{"  Spaces  Everywhere  ", "spaces-everywhere"},
		{"Deploy: checklist v2!", "deploy-checklist-v2"},
		{"a/b\\c", "a-b-c"},
		{"!!!", "memory"},
		{"", "memory"},
		{"Ünïcode name", "ünïcode-name"},
		{"already-slugified", "already-slugified"},
		{"a--b___c", "a-b-c"},
	}
	for _, c := range cases {
		if got := Slugify(c.in); got != c.want {
			t.Errorf("Slugify(%q): got %q want %q", c.in, got, c.want)
		}
	}
	long := strings.Repeat("a", 150)
	if got := Slugify(long); len(got) > 100 {
		t.Errorf("Slugify(long): length %d, want <= 100", len(got))
	}
}

func TestTypedStoreSaveLoadListDelete(t *testing.T) {
	dir := t.TempDir()
	s := mustTypedStore(t, dir)

	m := TypedMemory{Name: "Favourite editor", Description: "Prefers Zed", Type: TypeUser, Body: "Zed for Go."}
	if err := s.Save(m); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// The file is slugified, atomic (0600) and readable.
	path := filepath.Join(dir, "memories", "favourite-editor.md")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode: got %o want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "type: user") {
		t.Errorf("file missing frontmatter type:\n%s", data)
	}

	got, found, err := s.Load("Favourite editor")
	if err != nil || !found {
		t.Fatalf("Load: found=%v err=%v", found, err)
	}
	if got.Body != m.Body || got.Type != m.Type || got.UpdatedAt.IsZero() {
		t.Fatalf("Load: got %+v", got)
	}
	if _, found, err := s.Load("does not exist"); err != nil || found {
		t.Fatalf("Load missing: found=%v err=%v", found, err)
	}

	if err := s.Save(TypedMemory{Name: "Deploy checklist", Description: "Steps", Type: TypeProject, Body: "Build, then deploy on Thursday."}); err != nil {
		t.Fatalf("Save project: %v", err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List: got %d memories, want 2", len(list))
	}
	if list[0].Name != "Deploy checklist" || list[1].Name != "Favourite editor" {
		t.Fatalf("List order: got %q, %q", list[0].Name, list[1].Name)
	}

	if err := s.Delete("Favourite editor"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, found, err := s.Load("Favourite editor"); err != nil || found {
		t.Fatalf("Load after delete: found=%v err=%v", found, err)
	}
	// Deleting a missing memory is a no-op, not an error.
	if err := s.Delete("Favourite editor"); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
}

func TestTypedStoreSaveValidation(t *testing.T) {
	s := mustTypedStore(t, t.TempDir())
	cases := []TypedMemory{
		{Name: "", Description: "d", Type: TypeUser, Body: "b"},
		{Name: "n", Description: "", Type: TypeUser, Body: "b"},
		{Name: "n", Description: "d", Type: "secret", Body: "b"},
		{Name: "n", Description: "d", Type: TypeUser, Body: "  "},
		// Feedback without the required structure must not save.
		{Name: "n", Description: "d", Type: TypeFeedback, Body: "just a rule, no Why or How"},
	}
	for i, m := range cases {
		if err := s.Save(m); err == nil {
			t.Errorf("case %d (%+v): expected Save to fail", i, m)
		}
	}
}

func TestTypedStoreProjectDateConversionOnSave(t *testing.T) {
	s := mustTypedStore(t, t.TempDir())
	// Next Thursday on or after today, computed independently of the
	// converter so the test stays valid on any run date.
	now := time.Now()
	delta := (int(time.Thursday) - int(now.Weekday()) + 7) % 7
	wantThursday := now.AddDate(0, 0, delta).Format("2006-01-02")
	if err := s.Save(TypedMemory{Name: "Release", Description: "Release plan", Type: TypeProject, Body: "Ship on Thursday."}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, _, err := s.Load("Release")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.Contains(got.Body, "Thursday") {
		t.Errorf("project save should convert relative dates, got body %q", got.Body)
	}
	if !strings.Contains(got.Body, wantThursday) {
		t.Errorf("expected next Thursday %s in body, got %q", wantThursday, got.Body)
	}
	// Non-project memories are NOT rewritten on save.
	if err := s.Save(TypedMemory{Name: "Note", Description: "A note", Type: TypeUser, Body: "Meet tomorrow."}); err != nil {
		t.Fatalf("Save user: %v", err)
	}
	got, _, err = s.Load("Note")
	if err != nil {
		t.Fatalf("Load user: %v", err)
	}
	if !strings.Contains(got.Body, "tomorrow") {
		t.Errorf("user memory body should be left untouched, got %q", got.Body)
	}
}

func TestTypedStoreHeaders(t *testing.T) {
	s := mustTypedStore(t, t.TempDir())
	for _, m := range []TypedMemory{
		{Name: "Alpha", Description: "first", Type: TypeUser, Body: "A"},
		{Name: "Beta", Description: "second", Type: TypeProject, Body: "B"},
	} {
		if err := s.Save(m); err != nil {
			t.Fatalf("Save %q: %v", m.Name, err)
		}
		// Ensure distinct mtimes so newest-first ordering is stable.
		time.Sleep(10 * time.Millisecond)
	}
	headers, err := s.Headers()
	if err != nil {
		t.Fatalf("Headers: %v", err)
	}
	if len(headers) != 2 {
		t.Fatalf("Headers: got %d, want 2", len(headers))
	}
	if headers[0].Name != "Beta" {
		t.Errorf("Headers: newest first, got %q first, want Beta", headers[0].Name)
	}
	if headers[1].Description != "first" || headers[1].Type != TypeUser || headers[1].FileName != "alpha.md" {
		t.Errorf("Headers[1]: got %+v", headers[1])
	}
}

func TestTypedStoreCorruptFileIsAnError(t *testing.T) {
	s := mustTypedStore(t, t.TempDir())
	if err := os.WriteFile(filepath.Join(s.MemoriesDir(), "bad.md"), []byte("no frontmatter here\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := s.List(); err == nil {
		t.Error("List with a corrupt file: expected an error, got none")
	}
	if _, _, err := s.Load("bad"); err == nil {
		t.Error("Load of a corrupt file: expected an error, got none")
	}
}

func TestFileStoreTypedStore(t *testing.T) {
	dir := t.TempDir()
	fs := mustStore(t, dir)
	ts, err := fs.TypedStore()
	if err != nil {
		t.Fatalf("FileStore.TypedStore: %v", err)
	}
	if ts.DataDir() != dir {
		t.Errorf("TypedStore.DataDir: got %q want %q", ts.DataDir(), dir)
	}
	if err := ts.Save(TypedMemory{Name: "X", Description: "x", Type: TypeUser, Body: "x"}); err != nil {
		t.Fatalf("Save via TypedStore: %v", err)
	}
	// The JSON store is untouched by the typed layer.
	if facts, err := fs.ListFacts(); err != nil || len(facts) != 0 {
		t.Fatalf("ListFacts after typed save: %v facts, err %v", len(facts), err)
	}
}

func TestTypedStoreMarkStale(t *testing.T) {
	dir := t.TempDir()
	s := mustTypedStore(t, dir)
	m := TypedMemory{
		Name:        "Old deploy path",
		Description: "Deploy via the old script",
		Type:        TypeProject,
		Body:        "Run `scripts/deploy.sh` to deploy.",
	}
	if err := s.Save(m); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Round-trip without the marker: not stale.
	loaded, found, err := s.Load("Old deploy path")
	if err != nil || !found {
		t.Fatalf("Load: found=%v err=%v", found, err)
	}
	if loaded.Stale {
		t.Fatalf("fresh memory is stale")
	}
	// Marking a missing memory is a no-op, not an error.
	if err := s.MarkStale("No such memory"); err != nil {
		t.Fatalf("MarkStale missing: %v", err)
	}
	// Mark stale: persists across reloads and survives a
	// Marshal/Parse round trip.
	if err := s.MarkStale("Old deploy path"); err != nil {
		t.Fatalf("MarkStale: %v", err)
	}
	loaded, found, err = s.Load("Old deploy path")
	if err != nil || !found {
		t.Fatalf("Load after mark: found=%v err=%v", found, err)
	}
	if !loaded.Stale {
		t.Fatalf("memory not marked stale after MarkStale")
	}
	data, err := MarshalTypedMemory(loaded)
	if err != nil {
		t.Fatalf("Marshal stale: %v", err)
	}
	if !strings.Contains(string(data), "stale: true") {
		t.Fatalf("marshalled stale memory lacks 'stale: true':\n%s", data)
	}
	parsed, err := ParseTypedMemory(data)
	if err != nil {
		t.Fatalf("Parse stale: %v", err)
	}
	if !parsed.Stale {
		t.Fatalf("parsed stale memory lost the marker")
	}
	// Marking an already-stale memory is a no-op.
	if err := s.MarkStale("Old deploy path"); err != nil {
		t.Fatalf("MarkStale twice: %v", err)
	}
	// List still shows the stale memory (recall filters it, the
	// listing does not hide it).
	listed, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || !listed[0].Stale {
		t.Fatalf("List after mark: %+v", listed)
	}
	// A bad stale value is an error, not silently ignored.
	bad := "---\nname: Bad\ntype: project\ndescription: Bad stale value\nstale: maybe\n---\nBody.\n"
	if _, err := ParseTypedMemory([]byte(bad)); err == nil {
		t.Fatalf("Parse accepted stale: maybe")
	}
}
