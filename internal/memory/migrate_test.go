package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateFacts(t *testing.T) {
	dir := t.TempDir()
	fs := mustStore(t, dir)
	if err := fs.AddFact("favourite editor", "Zed, not VS Code"); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	if err := fs.AddFact("deploy day", "Never deploy on Fridays.\nSecond line here."); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	ts := mustTypedStore(t, dir)

	res, err := ts.MigrateFacts(fs)
	if err != nil {
		t.Fatalf("MigrateFacts: %v", err)
	}
	if res.Created != 2 || res.AlreadyPresent != 0 || res.Skipped != 0 {
		t.Fatalf("result: %+v, want Created=2 AlreadyPresent=0 Skipped=0", res)
	}
	if len(res.Names) != 2 || res.Names[0] != "deploy day" || res.Names[1] != "favourite editor" {
		t.Fatalf("Names: %v", res.Names)
	}

	// The migrated files are typed project memories with the note.
	m, found, err := ts.Load("favourite editor")
	if err != nil || !found {
		t.Fatalf("Load migrated: found=%v err=%v", found, err)
	}
	if m.Type != TypeProject {
		t.Errorf("migrated type: got %q want project", m.Type)
	}
	if !strings.Contains(m.Body, "Zed, not VS Code") {
		t.Errorf("migrated body lost the value: %q", m.Body)
	}
	if !strings.Contains(m.Body, "Migrated from JSON facts.") {
		t.Errorf("migrated body missing the note: %q", m.Body)
	}
	if m.Description != "Zed, not VS Code" {
		t.Errorf("migrated description: got %q", m.Description)
	}
	m2, _, err := ts.Load("deploy day")
	if err != nil {
		t.Fatalf("Load deploy day: %v", err)
	}
	if m2.Description != "Never deploy on Fridays." {
		t.Errorf("multi-line description: got %q", m2.Description)
	}

	// The JSON facts are untouched — the JSON store stays
	// authoritative for the Store interface.
	facts, err := fs.ListFacts()
	if err != nil {
		t.Fatalf("ListFacts: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("JSON facts after migration: got %d, want 2", len(facts))
	}
	for _, f := range facts {
		if f.Value == "" || strings.Contains(f.Value, "Migrated from JSON facts.") {
			t.Errorf("JSON fact %q was modified by migration: %q", f.Key, f.Value)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "memory.json"))
	if err != nil {
		t.Fatalf("read memory.json: %v", err)
	}
	if strings.Contains(string(data), "Migrated from JSON facts.") {
		t.Error("memory.json was modified by migration")
	}
}

func TestMigrateFactsIdempotent(t *testing.T) {
	dir := t.TempDir()
	fs := mustStore(t, dir)
	if err := fs.AddFact("key one", "value one"); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	ts := mustTypedStore(t, dir)

	res1, err := ts.MigrateFacts(fs)
	if err != nil {
		t.Fatalf("first MigrateFacts: %v", err)
	}
	if res1.Created != 1 || res1.AlreadyPresent != 0 {
		t.Fatalf("first run: %+v", res1)
	}
	// A hand edit to the typed file must survive a re-run.
	path := filepath.Join(dir, "memories", "key-one.md")
	edited := "---\nname: key one\ndescription: hand-edited description\ntype: project\n---\nHand-edited body.\n"
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	res2, err := ts.MigrateFacts(fs)
	if err != nil {
		t.Fatalf("second MigrateFacts: %v", err)
	}
	if res2.Created != 0 || res2.AlreadyPresent != 1 {
		t.Fatalf("second run: %+v, want Created=0 AlreadyPresent=1", res2)
	}
	list, err := ts.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List after two migrations: got %d files, want 1", len(list))
	}
	if list[0].Body != "Hand-edited body." {
		t.Errorf("re-run overwrote the hand edit: body %q", list[0].Body)
	}
}

func TestMigrateFactsSkipsEmptyValue(t *testing.T) {
	dir := t.TempDir()
	fs := mustStore(t, dir)
	if err := fs.AddFact("empty one", ""); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	if err := fs.AddFact("good one", "has a value"); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	ts := mustTypedStore(t, dir)
	res, err := ts.MigrateFacts(fs)
	if err != nil {
		t.Fatalf("MigrateFacts: %v", err)
	}
	if res.Created != 1 || res.Skipped != 1 {
		t.Fatalf("result: %+v, want Created=1 Skipped=1", res)
	}
}

func TestMigrateFactsNilStore(t *testing.T) {
	ts := mustTypedStore(t, t.TempDir())
	if _, err := ts.MigrateFacts(nil); err == nil {
		t.Error("MigrateFacts(nil): expected an error, got none")
	}
}

func TestNewTypedStoreWithMigrateOnOpen(t *testing.T) {
	dir := t.TempDir()
	fs := mustStore(t, dir)
	if err := fs.AddFact("open migrated", "migrated at open"); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	ts, err := NewTypedStore(dir, WithMigrateOnOpen(fs))
	if err != nil {
		t.Fatalf("NewTypedStore with migrate: %v", err)
	}
	if _, found, err := ts.Load("open migrated"); err != nil || !found {
		t.Fatalf("Load after open migration: found=%v err=%v", found, err)
	}
	// Without the option, NewTypedStore does not migrate.
	dir2 := t.TempDir()
	fs2 := mustStore(t, dir2)
	if err := fs2.AddFact("not migrated", "stays in JSON only"); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	ts2, err := NewTypedStore(dir2)
	if err != nil {
		t.Fatalf("NewTypedStore: %v", err)
	}
	if _, found, err := ts2.Load("not migrated"); err != nil || found {
		t.Fatalf("Load without migrate option: found=%v err=%v, want not found", found, err)
	}
}
