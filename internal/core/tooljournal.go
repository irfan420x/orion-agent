package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ToolJournal is a durable per-update record of completed tool
// calls (F-A9-1). PollStream is at-least-once by design: an update
// whose turn was killed mid-execution (SIGTERM, VM reboot — four
// silent reboots happened on 2026-11-13 alone) is re-delivered
// after the restart, and the re-delivered turn used to run its
// tools a second time. The journal closes that: executeTool
// records every executed tool call under the update's id —
// durably, before the result is fed back to the model — and a
// re-delivered turn replays the recorded result instead of
// re-executing the tool.
//
// Records are keyed by (update id, tool name, canonical
// arguments, occurrence): the occurrence index disambiguates a
// repeated identical call inside one turn (the model may
// legitimately call the same tool with the same arguments twice)
// from a re-delivery replay. The index counts only calls that
// reached the journal lookup — gate denials, hook blocks and
// skill-restriction refusals never consume one — so both runs of
// an identical call sequence agree on the indices. A re-delivered
// turn that diverges (the model takes a different path) simply
// misses the journal and executes normally; a lookup can only
// hit for the identical call, so replay can never return a result
// for a call that did not happen.
//
// Storage is one JSONL file per update id under dir
// (<dir>/<updateID>.jsonl), 0600, appended with Sync before the
// result is returned — the record is durable before the model
// sees the result. Files older than journalRetention (24h) are
// pruned: Telegram re-delivers unconfirmed updates only within
// its 24h window, so an older record can never be replayed. A
// failed record is fail-open (executeTool logs it and the turn
// proceeds): the worst case is the pre-journal behaviour — a
// re-delivered turn may re-run that one call.
//
// The journal is advisory, not authoritative: a corrupt line is
// skipped at load, a missing or unwritable dir disables the
// journal (NewToolJournal returns the error and the caller runs
// without one), and a nil *ToolJournal is inert everywhere.
//
// Residual windows, documented not hidden: a kill between the
// tool returning and the record's Sync still double-runs that
// one call (the window is the Sync itself, microseconds —
// Record is called immediately after Execute returns, before
// anything else); and tools whose results reference in-process
// state that does not survive a restart are excluded from the
// journal entirely (see journalExcludedTools in agent.go), so
// they always re-execute against live state.
type ToolJournal struct {
	mu        sync.RWMutex
	dir       string
	byUpdate  map[int64][]toolJournalRecord
	lastPrune time.Time
}

// toolJournalRecord is one completed tool call, as stored (one
// JSON line per record).
type toolJournalRecord struct {
	UpdateID   int64     `json:"update_id"`
	Occurrence int       `json:"occurrence"`
	ToolName   string    `json:"tool"`
	Arguments  string    `json:"arguments"`
	Result     string    `json:"result"`
	RecordedAt time.Time `json:"recorded_at"`
}

// journalRetention bounds the journal on disk: records older
// than this can never be replayed (Telegram's re-delivery window
// is 24h), so they are pruned.
const journalRetention = 24 * time.Hour

// journalPruneInterval is how often Record re-checks for stale
// files, so a long-lived daemon prunes without a restart.
const journalPruneInterval = time.Hour

// NewToolJournal opens (or creates) the journal under dir,
// loading any records already on disk — a restarted daemon
// replays run-1 records from a killed turn — and pruning files
// older than journalRetention. A failure to create or read the
// directory is an error; the caller runs without a journal.
func NewToolJournal(dir string) (*ToolJournal, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("core: tool journal directory must not be empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("core: tool journal: create %s: %w", dir, err)
	}
	j := &ToolJournal{dir: dir, byUpdate: map[int64][]toolJournalRecord{}}
	if err := j.load(); err != nil {
		return nil, err
	}
	j.prune(time.Now())
	j.lastPrune = time.Now()
	return j, nil
}

// load reads every *.jsonl file in the journal dir into the
// in-memory index. A malformed line is skipped (the record can
// never match a lookup anyway); a file that cannot be read at all
// is skipped the same way — the journal is advisory, and a bad
// file must not brick the daemon.
func (j *ToolJournal) load() error {
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		return fmt.Errorf("core: tool journal: read %s: %w", j.dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimSuffix(e.Name(), ".jsonl"), 10, 64)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(j.dir, e.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var rec toolJournalRecord
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				continue
			}
			if rec.UpdateID != id || rec.ToolName == "" {
				continue
			}
			j.byUpdate[id] = append(j.byUpdate[id], rec)
		}
	}
	// Keep each update's records in file order (record order):
	// the file is append-only, so load order is record order.
	return nil
}

// prune deletes per-update files whose records are all older
// than journalRetention, and drops their index entries. It is
// called at open and lazily from Record. Errors are ignored:
// pruning is housekeeping, never load-bearing.
func (j *ToolJournal) prune(now time.Time) {
	cutoff := now.Add(-journalRetention)
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimSuffix(e.Name(), ".jsonl"), 10, 64)
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(j.dir, e.Name()))
		delete(j.byUpdate, id)
	}
}

// Lookup returns the recorded result for the occurrence-th
// identical call of (tool, canonical arguments) in update id's
// turn, and whether one exists. canonicalArgs must already be
// canonicalised with CanonicalToolArguments.
func (j *ToolJournal) Lookup(updateID int64, tool, canonicalArgs string, occurrence int) (string, bool) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	for _, rec := range j.byUpdate[updateID] {
		if rec.ToolName == tool && rec.Arguments == canonicalArgs && rec.Occurrence == occurrence {
			return rec.Result, true
		}
	}
	return "", false
}

// Record durably appends one completed tool call to update id's
// file (Sync before returning) and indexes it. The caller must
// pass the same canonical arguments and occurrence index it
// looked up with, so a later Lookup finds exactly this record.
func (j *ToolJournal) Record(updateID int64, tool, canonicalArgs, result string, occurrence int) error {
	rec := toolJournalRecord{
		UpdateID:   updateID,
		Occurrence: occurrence,
		ToolName:   tool,
		Arguments:  canonicalArgs,
		Result:     result,
		RecordedAt: time.Now(),
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("core: tool journal: encode record: %w", err)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if time.Since(j.lastPrune) > journalPruneInterval {
		j.prune(time.Now())
		j.lastPrune = time.Now()
	}
	path := filepath.Join(j.dir, strconv.FormatInt(updateID, 10)+".jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("core: tool journal: open %s: %w", path, err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("core: tool journal: write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("core: tool journal: sync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("core: tool journal: close %s: %w", path, err)
	}
	j.byUpdate[updateID] = append(j.byUpdate[updateID], rec)
	return nil
}

// Records returns a copy of the recorded calls for update id, in
// record order. It is for tests and diagnostics.
func (j *ToolJournal) Records(updateID int64) []toolJournalRecord {
	j.mu.RLock()
	defer j.mu.RUnlock()
	out := append([]toolJournalRecord(nil), j.byUpdate[updateID]...)
	return out
}

// Dir returns the journal's directory. It is for tests.
func (j *ToolJournal) Dir() string { return j.dir }

// CanonicalToolArguments canonicalises a tool call's JSON
// argument string for journal keying: parsed and re-marshalled
// (encoding/json sorts object keys), so {"b":1,"a":2} and
// {"a":2,"b":1} key identically. Unparseable or non-object
// arguments key on their trimmed raw text — parseToolArguments
// rejects those before the journal is ever consulted, so the
// fallback is unreachable in production, only defensive.
func CanonicalToolArguments(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "{}"
	}
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
		return trimmed
	}
	m, ok := v.(map[string]any)
	if !ok {
		return trimmed
	}
	b, err := json.Marshal(m)
	if err != nil {
		return trimmed
	}
	return string(b)
}

// updateIDKey is the context key for the Telegram update id of
// the turn in flight. It lives in core (not messaging) so the
// agent loop can read it without an import cycle: messaging
// already imports core, core must never import messaging.
type updateIDKey struct{}

// WithUpdateID carries the Telegram update id of the turn in
// flight on ctx. PollStream sets it before invoking the turn
// handler; any other entrypoint leaves it unset (0).
func WithUpdateID(ctx context.Context, id int64) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, updateIDKey{}, id)
}

// UpdateIDFromContext reads the Telegram update id carried by
// ctx, or 0 when ctx carries none (CLI turns, tests, subagent
// child turns — see newSubagentExecutor, which clears it).
func UpdateIDFromContext(ctx context.Context) int64 {
	if ctx == nil {
		return 0
	}
	id, _ := ctx.Value(updateIDKey{}).(int64)
	return id
}
