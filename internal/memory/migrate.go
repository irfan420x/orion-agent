// Package memory — migration from JSON facts to typed files (Memory v2).
//
// Migration COPIES existing JSON facts into typed memory files. It
// never deletes or modifies the JSON facts: the JSON store stays
// authoritative for the Store interface, and the typed files are an
// additional layer. Migration is idempotent — re-running it does not
// duplicate files — and reports how many memories were created vs
// already present.
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MigrationResult reports what one MigrateFacts run did.
type MigrationResult struct {
	// Created is the number of typed files written by this run.
	Created int
	// AlreadyPresent is the number of facts that already had a typed
	// file (same slug) and were left untouched.
	AlreadyPresent int
	// Skipped is the number of facts that could not become a typed
	// memory (an empty value has no body) and were left in the JSON
	// store untouched.
	Skipped int
	// Names lists the fact keys that were migrated (created), in
	// key-sorted order.
	Names []string
}

// migrationNote is appended to every migrated body so the provenance
// is visible in the file itself.
const migrationNote = "Migrated from JSON facts."

// MigrateFacts copies every fact from the JSON store into a typed
// memory file of type project. The body is the fact value plus a
// "migrated from JSON facts" note; the description is the value's
// first line, truncated. A fact whose slug file already exists is
// counted as AlreadyPresent and left untouched — the existing file
// wins, so hand-edited typed memories are never overwritten by a
// re-run. The JSON facts are only read, never written or deleted.
//
// Provenance and gate (Audit 4 A P1-3): migrated memories are
// marked ProvenanceMigrated and are ALWAYS type project — model-
// derived text (notably the "conversation-summary" fact, which is a
// model summary) must never become feedback. Each fact value also
// passes the what-not-to-save gate: a derivable fact is skipped
// (counted in Skipped) rather than promoted into typed recall.
func (s *TypedStore) MigrateFacts(store *FileStore) (MigrationResult, error) {
	if store == nil {
		return MigrationResult{}, fmt.Errorf("memory: MigrateFacts needs a non-nil JSON store")
	}
	facts, err := store.ListFacts()
	if err != nil {
		return MigrationResult{}, fmt.Errorf("memory: migrate: list JSON facts: %w", err)
	}
	var result MigrationResult
	for _, f := range facts {
		fileName := Slugify(f.Key) + ".md"
		path := filepath.Join(s.memoriesDir, fileName)
		if _, err := os.Stat(path); err == nil {
			result.AlreadyPresent++
			continue
		} else if !os.IsNotExist(err) {
			return result, fmt.Errorf("memory: migrate: stat %q: %w", path, err)
		}
		if strings.TrimSpace(f.Value) == "" {
			// A fact with an empty value has no body to migrate:
			// the result would be a file holding only the
			// migration note. Skip it rather than failing the
			// whole migration — the JSON fact is untouched and
			// stays listed in the JSON store.
			result.Skipped++
			continue
		}
		if d := Gate(f.Value); !d.Save {
			// Derivable content (code patterns, file structure,
			// git history) is not worth a typed memory.
			result.Skipped++
			continue
		}
		m := TypedMemory{
			Name:        f.Key,
			Description: migrateDescription(f.Value),
			Type:        TypeProject,
			Body:        strings.TrimSpace(f.Value) + "\n\n_" + migrationNote + "_\n",
			Provenance:  ProvenanceMigrated,
		}
		if err := m.Validate(); err != nil {
			// Defensive: with the empty-value check above this
			// should not trigger, but a fact that cannot become
			// a valid typed memory must not fail the whole
			// migration — the JSON fact is untouched.
			result.Skipped++
			continue
		}
		data, err := MarshalTypedMemory(m)
		if err != nil {
			return result, fmt.Errorf("memory: migrate: marshal fact %q: %w", f.Key, err)
		}
		s.mu.Lock()
		err = s.writeFileLocked(fileName, data)
		s.mu.Unlock()
		if err != nil {
			return result, fmt.Errorf("memory: migrate: write fact %q: %w", f.Key, err)
		}
		result.Created++
		result.Names = append(result.Names, f.Key)
	}
	return result, nil
}

// migrateDescription derives a recall description from a fact value:
// its first non-empty line, truncated to 120 characters. The
// description is what recall matches on, so a bare key would be a
// poor one.
func migrateDescription(value string) string {
	for _, line := range strings.Split(strings.TrimSpace(value), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 120 {
			return line[:117] + "..."
		}
		return line
	}
	return "(migrated fact)"
}
