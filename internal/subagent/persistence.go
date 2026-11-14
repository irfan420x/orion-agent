// Subagent state persistence (F-A17-12, rc23).
//
// Before this file the Manager's task registry and dead-letter
// ledger lived entirely in memory: a daemon reboot lost running
// tasks, finished results, and the evicted-but-never-charged token
// ledger, and a re-delivered brief re-spawned the task — the same
// work paid for twice. A Manager built with a store directory
// (NewManagerWithStore / NewManagerWithExecutorAndStore) persists
// a snapshot to <dir>/subagent-state.json (0600, atomic temp +
// rename + dir fsync, the same pattern the rules store uses) on
// every state change — spawn, finish, eviction, forget, charge,
// drain, late usage — and reloads it at construction.
//
// What survives a restart: finished tasks (id, brief, status,
// output, usage, settled/charged flags, owning chat, timings)
// and the dead-letter ledger. What does not: a task that was
// still running when the daemon stopped — its executor died with
// the process, so its result is unknowable. Such a task reloads
// as failed with an explicit "lost on restart" error (never as
// done, never silently dropped), and the startup log line says
// how many tasks were lost, so the loss is visible, not silent.
// A corrupt or unreadable state file logs and starts empty — a
// bad state file must never stop the daemon starting, and must
// never invent a result.
//
// A Manager with no store directory (NewManager /
// NewManagerWithExecutor, "" dir) behaves exactly as before:
// in-memory only, no file, no log line. The daemon wires the
// store directory to cfg.DataDir (see the wiring note on
// NewManagerWithExecutorAndStore); until it does, persistence
// is dormant, not half-on.
package subagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// subagentStateFileName is the persisted snapshot's file name
// inside the store directory.
const subagentStateFileName = "subagent-state.json"

// persistedStateVersion is the only on-disk version this code
// reads or writes. A file with another version is treated as
// corrupt (log + start empty) rather than guessed at.
const persistedStateVersion = 1

// maxPersistedTasks bounds the snapshot: the newest finished
// tasks are kept, oldest first out. The in-memory registry caps
// at maxEntries (1000); the file keeps a smaller tail because
// outputs are strings and the file is rewritten on every state
// change — 200 tasks is plenty for the sweep/charge path, which
// consumes results within a turn, and the dead-letter ledger
// (not the task list) is what preserves evicted tokens.
const maxPersistedTasks = 200

// maxPersistedOutputRunes / maxPersistedBriefRunes bound the
// strings stored per task, so one huge output cannot bloat the
// state file. Truncated values carry a marker.
const maxPersistedOutputRunes = 16 * 1024
const maxPersistedBriefRunes = 4 * 1024

// persistedTask is one task's on-disk record.
type persistedTask struct {
	ID           string        `json:"id"`
	Brief        string        `json:"brief"`
	Status       Status        `json:"status"`
	Output       string        `json:"output,omitempty"`
	Err          string        `json:"err,omitempty"`
	Tokens       int           `json:"tokens"`
	UsageSettled bool          `json:"usage_settled"`
	Charged      bool          `json:"charged"`
	ChatID       int64         `json:"chat_id"`
	StartedAt    time.Time     `json:"started_at"`
	FinishedAt   time.Time     `json:"finished_at,omitempty"`
	Duration     time.Duration `json:"duration_ns"`
	Timeout      time.Duration `json:"timeout_ns"`
}

// persistedState is the on-disk snapshot.
type persistedState struct {
	Version          int             `json:"version"`
	SavedAt          time.Time       `json:"saved_at"`
	DeadLetterTokens int             `json:"dead_letter_tokens"`
	DeadLetterTasks  int             `json:"dead_letter_tasks"`
	Tasks            []persistedTask `json:"tasks"`
}

// NewManagerWithStore is NewManager with a persistence directory:
// the Manager loads any snapshot found there at construction and
// persists on every state change. An empty dir is in-memory-only
// (identical to NewManager).
func NewManagerWithStore(maxConcurrent int, dir string) *Manager {
	m := NewManager(maxConcurrent)
	m.storeDir = dir
	m.loadPersistedState()
	return m
}

// NewManagerWithExecutorAndStore is NewManagerWithExecutor with a
// persistence directory. WIRING (coordinator, internal/app/app.go):
// replace the subagent construction with
//
//	mgr := subagent.NewManagerWithExecutorAndStore(4, newSubagentExecutor(provider, cfg, typedAdapter, logger, hookBus, planningStore), cfg.DataDir)
//
// where planningStore is the planning store Build creates and
// passes to the registry in ToolDeps. cfg.DataDir is "" when
// unset, and "" means in-memory-only, so the change is safe for
// hand-built Apps with no data dir.
func NewManagerWithExecutorAndStore(maxConcurrent int, exec Executor, dir string) *Manager {
	m := NewManagerWithExecutor(maxConcurrent, exec)
	m.storeDir = dir
	m.loadPersistedState()
	return m
}

// StoreDir returns the Manager's persistence directory ("" when
// in-memory-only).
func (m *Manager) StoreDir() string {
	return m.storeDir
}

// loadPersistedState rebuilds finished tasks and the dead-letter
// ledger from the snapshot in m.storeDir, if any. It runs once,
// at construction, before any task can spawn. A missing file is
// a first run (silent). A present file logs what it recovered
// and what was lost; a corrupt file logs and starts empty.
func (m *Manager) loadPersistedState() {
	if m.storeDir == "" {
		return
	}
	path := filepath.Join(m.storeDir, subagentStateFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		log.Printf("subagent: persisted state %s unreadable (%v); starting with empty registry", path, err)
		return
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return
	}
	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		log.Printf("subagent: persisted state %s corrupt (%v); starting with empty registry", path, err)
		return
	}
	if st.Version != persistedStateVersion {
		log.Printf("subagent: persisted state %s has unsupported version %d (want %d); starting with empty registry", path, st.Version, persistedStateVersion)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deadLetterTokens = st.DeadLetterTokens
	m.deadLetterTasks = st.DeadLetterTasks
	recovered, lost := 0, 0
	for _, pt := range st.Tasks {
		entry := &taskEntry{
			task:      Task{ID: pt.ID, Brief: pt.Brief, Timeout: pt.Timeout, CreatedAt: pt.StartedAt, ChatID: pt.ChatID},
			status:    pt.Status,
			startedAt: pt.StartedAt,
			done:      make(chan struct{}),
			finished:  true,
			charged:   pt.Charged,
		}
		if pt.Status == StatusRunning || pt.Status == StatusPending {
			// The executor died with the old process: the
			// result is unknowable. Record the loss
			// explicitly — failed, never done — so a later
			// GetResult or sweep sees the task and its
			// loss instead of the task silently vanishing.
			// Its usage was never settled, so nothing is
			// folded into the ledger (there is nothing to
			// fold: the tokens were never reported).
			lost++
			entry.status = StatusFailed
			entry.result = Result{
				TaskID:       pt.ID,
				Err:          errors.New("subagent: task was running when the daemon stopped; result lost on restart"),
				UsageSettled: false,
				Duration:     0,
				FinishedAt:   pt.StartedAt,
			}
		} else {
			recovered++
			var resErr error
			if pt.Err != "" {
				resErr = errors.New(pt.Err)
			}
			entry.result = Result{
				TaskID:       pt.ID,
				Output:       pt.Output,
				Err:          resErr,
				Usage:        Usage{Tokens: pt.Tokens},
				UsageSettled: pt.UsageSettled,
				Duration:     pt.Duration,
				FinishedAt:   pt.FinishedAt,
			}
		}
		close(entry.done)
		m.entries[pt.ID] = entry
	}
	if recovered > 0 || lost > 0 || st.DeadLetterTasks > 0 {
		log.Printf("subagent: recovered %d finished task(s) and dead-letter ledger (%d tokens, %d tasks) from %s; %d running task(s) were lost on restart", recovered, st.DeadLetterTokens, st.DeadLetterTasks, path, lost)
	}
}

// persistSnapshot writes the current registry + ledger to the
// state file. It is a no-op (and never logs) for an in-memory-only
// Manager. Callers must NOT hold m.mu: the snapshot is taken
// under m.mu, the file write happens outside it, and writes are
// serialised by m.persistMu. A write failure is logged and
// dropped — persistence must never fail a spawn, a finish, or a
// charge.
func (m *Manager) persistSnapshot() {
	if m.storeDir == "" {
		return
	}
	m.mu.Lock()
	st := m.snapshotLocked()
	m.mu.Unlock()
	m.persistMu.Lock()
	defer m.persistMu.Unlock()
	if err := writePersistedState(m.storeDir, st); err != nil {
		log.Printf("subagent: persisting state: %v", err)
	}
}

// snapshotLocked renders the registry + ledger as a persisted
// snapshot. Callers must hold m.mu. Finished tasks are newest
// first and capped at maxPersistedTasks; running tasks are
// always included (bounded by maxConcurrent), so a restart can
// report exactly which tasks were lost.
func (m *Manager) snapshotLocked() persistedState {
	st := persistedState{
		Version:          persistedStateVersion,
		SavedAt:          time.Now(),
		DeadLetterTokens: m.deadLetterTokens,
		DeadLetterTasks:  m.deadLetterTasks,
	}
	var finished []*taskEntry
	for _, e := range m.entries {
		if e.finished {
			finished = append(finished, e)
		} else {
			st.Tasks = append(st.Tasks, persistedTask{
				ID:        e.task.ID,
				Brief:     truncateRunes(e.task.Brief, maxPersistedBriefRunes),
				Status:    e.status,
				ChatID:    e.task.ChatID,
				StartedAt: e.startedAt,
				Timeout:   e.task.Timeout,
			})
		}
	}
	sort.Slice(finished, func(i, j int) bool {
		return finished[i].result.FinishedAt.After(finished[j].result.FinishedAt)
	})
	if len(finished) > maxPersistedTasks {
		finished = finished[:maxPersistedTasks]
	}
	for _, e := range finished {
		pt := persistedTask{
			ID:           e.task.ID,
			Brief:        truncateRunes(e.task.Brief, maxPersistedBriefRunes),
			Status:       e.status,
			Output:       truncateRunes(e.result.Output, maxPersistedOutputRunes),
			Tokens:       e.result.Usage.Tokens,
			UsageSettled: e.result.UsageSettled,
			Charged:      e.charged,
			ChatID:       e.task.ChatID,
			StartedAt:    e.startedAt,
			FinishedAt:   e.result.FinishedAt,
			Duration:     e.result.Duration,
			Timeout:      e.task.Timeout,
		}
		if e.result.Err != nil {
			pt.Err = e.result.Err.Error()
		}
		st.Tasks = append(st.Tasks, pt)
	}
	return st
}

// writePersistedState writes st atomically (temp file 0600 in the
// same directory + rename + dir fsync).
func writePersistedState(dir string, st persistedState) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("subagent: create state dir: %w", err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("subagent: encode state: %w", err)
	}
	data = append(data, '\n')
	path := filepath.Join(dir, subagentStateFileName)
	tmp, err := os.CreateTemp(dir, ".subagent-state-*.tmp")
	if err != nil {
		return fmt.Errorf("subagent: create temp state file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("subagent: chmod temp state file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("subagent: write temp state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("subagent: sync temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("subagent: close temp state file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("subagent: rename temp state file: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// truncateRunes caps s at max runes, appending a marker when it
// truncates. An empty max means no cap (defensive; callers pass
// a positive cap).
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…[truncated]"
}
