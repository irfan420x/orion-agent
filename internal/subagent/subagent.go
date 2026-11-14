// Package subagent manages in-process child agents for Orion.
//
// A Manager runs briefs through an injected Executor — the real agent
// loop is plugged in by the caller, so this package never shells out
// and never imports the core package (which would create an import
// cycle). Each task runs in its own goroutine under a context timeout,
// concurrency is bounded by a semaphore, and every task's status and
// result are recorded in a race-safe, mutex-guarded registry.
//
// Cancellation and timeout are enforced by the Manager itself: the
// executor runs in an inner goroutine and the Manager selects on the
// task context, so a task is recorded as timed out / cancelled promptly
// even if the executor ignores its context. A late executor result's
// OUTPUT is discarded (the task is already terminal), but its USAGE
// is not: when the executor eventually returns, a late-arrival
// waiter records the reported usage on the entry (F-A7-2a), so a
// later GetResult or sweep charges the tokens instead of losing
// them. UsageSettled on the Result reports whether the usage is
// final yet.
package subagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultMaxConcurrent is used when NewManager is given maxConcurrent <= 0.
const DefaultMaxConcurrent = 4

// DefaultTimeout is used when Spawn is given timeout <= 0.
const DefaultTimeout = 5 * time.Minute

// ShutdownTimeout bounds how long Shutdown waits for running tasks
// to finish after cancelling them.
const ShutdownTimeout = 10 * time.Second

// Status is the lifecycle state of a subagent task.
type Status string

// Task statuses.
const (
	StatusPending Status = "pending"
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
	StatusTimeout Status = "timeout"
)

// Task is a unit of work handed to a subagent.
type Task struct {
	ID        string
	Brief     string
	Timeout   time.Duration
	CreatedAt time.Time
	// ChatID is the chat that spawned the task (0 when spawned
	// without a chat context, e.g. the CLI). The per-chat
	// subagent sweep (F-A5-7) charges a task only to its owning
	// chat; without this the shared Manager lets one chat's
	// finished tasks be charged to another chat whose sweep
	// window they happen to fall in.
	ChatID int64
}

// Result is the outcome of a finished subagent task. Err is non-nil
// for failed and timed-out tasks. Usage records what the executor
// reported the task consumed. UsageSettled reports whether Usage is
// final: it is true once the executor's outcome has been consumed
// (at record time on the normal path). For a task recorded as timed
// out or cancelled while its executor was still running, Usage is
// zero and UsageSettled is false until the executor eventually
// returns, at which point the late-arrival waiter records its
// reported usage and sets UsageSettled (F-A7-2a) — a caller that
// charges a task must wait for UsageSettled before treating a zero
// usage as final, or it will charge zero and lose the late tokens.
// An executor that never returns leaves Usage zero and
// UsageSettled false forever; its spend is unknowable, not invented.
type Result struct {
	TaskID string
	Output string
	Err    error
	Usage  Usage
	// UsageSettled reports whether Usage can no longer change.
	UsageSettled bool
	Duration     time.Duration
	FinishedAt   time.Time
}

// Usage is the resource consumption one subagent task reported. Tokens
// is the executor's token count for the whole task (prompt + completion
// across its own agent loop); the parent charges it when it consumes
// the result, never at spawn time.
type Usage struct {
	Tokens int
}

// TaskStatus is a snapshot of one task for List.
type TaskStatus struct {
	ID        string
	Brief     string
	Status    Status
	StartedAt time.Time
	// ChatID is the owning chat (0 when spawned without one).
	ChatID int64
}

// Executor runs one brief and returns its output plus the usage it
// consumed. Implementations must respect ctx cancellation/timeout
// where possible; the Manager enforces the timeout regardless, so an
// executor that ignores ctx is still recorded as timed out on schedule.
// Its late OUTPUT is discarded, but its late usage is recorded on
// the task when it eventually returns (F-A7-2a), so the tokens are
// charged by the next sweep or session close instead of being lost.
// The real agent loop is plugged in by the caller via
// NewManagerWithExecutor or SetExecutor.
type Executor func(ctx context.Context, brief string) (output string, usage Usage, err error)

// outcome is one executor return, carried on runTask's buffered
// channel. Package-level so the late-arrival waiter (spawned by
// runTask's timeout branch) can consume it after runTask returns.
type outcome struct {
	output string
	usage  Usage
	err    error
}

// taskEntry is the Manager's internal record for one task. done is
// closed exactly once, when the task finishes and result is set.
// detached is set when the entry leaves m.entries (evicted or
// forgotten) while a late-arrival waiter may still write to it: the
// waiter then folds the late usage into the dead-letter ledger
// instead of writing onto an entry no reader can reach (F-A8-P2-1).
type taskEntry struct {
	task      Task
	exec      Executor
	status    Status
	startedAt time.Time
	cancel    context.CancelFunc
	done      chan struct{}
	result    Result
	finished  bool
	detached  bool
	// charged is set by MarkCharged once a task's usage has been
	// charged by a consumer (the per-chat sweep, or a fetch that
	// recorded the tokens in its own delivered set). Only a
	// SETTLED usage may be marked: chargers skip unsettled tasks,
	// so an unsettled task is never charged and must never be
	// marked. GetResult deliberately does NOT set this flag — it
	// is also called by non-charging readers (list, /agents,
	// pending-token estimates), and marking on a peek would let
	// an eviction silently drop tokens no consumer ever charged.
	// Eviction folds only uncharged tokens into the dead-letter
	// ledger (F-A9-P2-8); a charged task contributes 0 tokens.
	charged bool
}

// Manager runs subagent tasks with bounded concurrency.
type Manager struct {
	mu            sync.Mutex
	maxConcurrent int
	executor      Executor
	entries       map[string]*taskEntry
	sem           chan struct{}
	wg            sync.WaitGroup
	shutdown      bool
	// storeDir is the persistence directory (F-A17-12): "" means
	// in-memory-only (NewManager / NewManagerWithExecutor).
	// persistMu serialises state-file writes; the snapshot is
	// taken under mu and the file is written outside it, so a
	// slow disk never blocks task bookkeeping.
	storeDir  string
	persistMu sync.Mutex
	// deadLetterTokens / deadLetterTasks are the dead-letter ledger
	// (F-A8-P2-1, semantics fixed F-A9-P2-8): tokens of finished
	// tasks that left the registry through eviction WITHOUT ever
	// being charged, and how many such tasks there were. Eviction
	// is involuntary — unlike Forget, nobody decided these results
	// were consumed — so an evicted task's recorded usage is folded
	// here at eviction time instead of silently voided, but ONLY
	// when the task was never charged (see taskEntry.charged):
	// folding an already-charged task's tokens would double-count
	// them against the charged total. A charged evicted task still
	// counts in deadLetterTasks but contributes 0 tokens. A task
	// evicted while its usage was still unsettled contributes its
	// late usage to the ledger when the executor eventually returns
	// (see awaitLateOutcome); that fold is exact because chargers
	// skip unsettled tasks, so the late usage was never chargeable
	// before eviction. The ledger is therefore exactly "evicted
	// but never charged" and a consumer may add DeadLetterTokens
	// to a charged total without reconciliation — see
	// DrainDeadLetter, which hands the total out once and resets
	// it. Every eviction is logged (see evictLocked). In practice
	// the ledger is nearly unreachable — the per-chat sweep charges
	// finished tasks at the end of every turn, and the cap below
	// is 10x the old one — but no evicted token is ever silently
	// lost again, and no evicted token is ever counted twice.
	deadLetterTokens int
	deadLetterTasks  int
}

// NewManager returns a Manager with the given concurrency limit.
// maxConcurrent <= 0 defaults to DefaultMaxConcurrent (4). No executor
// is configured; set one with SetExecutor (or use
// NewManagerWithExecutor) before calling Spawn.
func NewManager(maxConcurrent int) *Manager {
	if maxConcurrent <= 0 {
		maxConcurrent = DefaultMaxConcurrent
	}
	return &Manager{
		maxConcurrent: maxConcurrent,
		entries:       make(map[string]*taskEntry),
		sem:           make(chan struct{}, maxConcurrent),
	}
}

// NewManagerWithExecutor is NewManager with an executor already plugged in.
func NewManagerWithExecutor(maxConcurrent int, exec Executor) *Manager {
	m := NewManager(maxConcurrent)
	m.executor = exec
	return m
}

// SetExecutor replaces the executor used for subsequently spawned
// tasks. Tasks already running keep the executor they started with.
func (m *Manager) SetExecutor(exec Executor) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.executor = exec
}

// newTaskID returns 16 hex characters from 8 crypto-random bytes.
func newTaskID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("subagent: generating task id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Spawn starts a task and returns its ID. An empty (or whitespace-only)
// brief is an error; timeout <= 0 defaults to DefaultTimeout. If the
// Manager is already at maxConcurrent, Spawn returns an "at capacity"
// error immediately instead of blocking. Spawn fails if no executor
// is configured or the Manager has been shut down.
// Spawn starts a task unattributed to any chat (chatID 0). Prefer
// SpawnForChat on the model path so the per-chat sweep can charge
// the task to its owner; unattributed tasks are never swept (the
// fetch path still charges them to whoever fetches the result).
func (m *Manager) Spawn(brief string, timeout time.Duration) (string, error) {
	return m.SpawnForChat(0, brief, timeout)
}

// SpawnForChat is Spawn with the owning chat recorded on the task
// (Audit 5 F-A5-7 attribution): the per-chat sweep charges a
// finished task only to the chat that spawned it.
func (m *Manager) SpawnForChat(chatID int64, brief string, timeout time.Duration) (string, error) {
	if strings.TrimSpace(brief) == "" {
		return "", errors.New("subagent: brief must not be empty")
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	id, err := newTaskID()
	if err != nil {
		return "", err
	}

	m.mu.Lock()
	if m.shutdown {
		m.mu.Unlock()
		return "", errors.New("subagent: manager shut down")
	}
	exec := m.executor
	if exec == nil {
		m.mu.Unlock()
		return "", errors.New("subagent: no executor configured")
	}
	// Non-blocking semaphore acquire: at capacity is an error, not a wait.
	select {
	case m.sem <- struct{}{}:
	default:
		m.mu.Unlock()
		return "", errors.New("subagent: at capacity")
	}

	now := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	entry := &taskEntry{
		task:      Task{ID: id, Brief: brief, Timeout: timeout, CreatedAt: now, ChatID: chatID},
		exec:      exec,
		status:    StatusRunning,
		startedAt: now,
		cancel:    cancel,
		done:      make(chan struct{}),
	}
	m.entries[id] = entry
	m.wg.Add(1)
	// Bound memory: evict the oldest finished tasks once the
	// registry grows past maxEntries, so a long daemon run cannot
	// grow List / WaitAll without bound. Eviction preserves token
	// accounting (F-A8-P2-1, F-A9-P2-8): it skips unsettled finished tasks
	// while any settled finished task can be evicted instead, and
	// folds an evicted task's recorded usage into the
	// dead-letter ledger only if the task was never charged
	// (see DeadLetterTokens).
	m.evictFinishedLocked()
	m.mu.Unlock()
	// Persist the spawn (F-A17-12): a task that is running when
	// the daemon stops must reload as lost, not vanish.
	m.persistSnapshot()

	go m.runTask(entry, ctx, cancel)
	return id, nil
}

// maxEntries bounds the task registry. Finished tasks beyond this
// are evicted oldest-first; running tasks are never evicted. Raised
// from 100 to 1000 in rc13 (F-A8-P2-1): the old cap was daemon-wide
// while sweeps run per turn, so a single busy turn could flood the
// registry and void finished tasks' tokens before any sweep saw
// them. At 1000 entries the registry is still small (each entry is
// one struct plus its output/brief strings), and eviction below
// preserves the tokens anyway — the cap is a memory bound now, not
// an accounting cliff.
const maxEntries = 1000

// DeadLetterTokens returns the tokens folded into the dead-letter
// ledger by eviction (F-A8-P2-1, semantics F-A9-P2-8): the recorded
// usage of finished tasks that left the registry through eviction
// WITHOUT ever being charged. A task charged before eviction
// contributes 0 tokens (it still counts in DeadLetterTasks). Late
// usage that arrived after an unsettled task was evicted is
// included, because it was never chargeable before eviction. The
// total is therefore exactly "evicted but never charged" and may
// be added to a charged total with no double-count; see
// DrainDeadLetter for the consuming read.
func (m *Manager) DeadLetterTokens() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deadLetterTokens
}

// DeadLetterTasks returns how many finished tasks were evicted from
// the registry (F-A8-P2-1). It counts evicted tasks, not ledger
// contributions: a task evicted while unsettled is counted at
// eviction time, and its late tokens join DeadLetterTokens when
// they arrive; a task evicted after being charged is counted too,
// but contributes 0 tokens (F-A9-P2-8).
func (m *Manager) DeadLetterTasks() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deadLetterTasks
}

// MarkCharged records that a task's usage has been charged by a
// consumer (the per-chat sweep, or a fetch that recorded the tokens
// in its own delivered set). It reports whether this call newly
// marked the task: false for an unknown id, an unfinished task, an
// unsettled task, or a task already marked. Only a SETTLED usage
// can be charged — chargers skip unsettled tasks — so an unsettled
// task is never marked, and its late usage stays recoverable. The
// mark is what keeps eviction from double-counting: evictLocked
// folds only uncharged tokens into the dead-letter ledger. The
// flag is not an exactly-once guard between the fetch and sweep
// chargers — each keeps its own guard (the delivered set, the
// swept set) and they charge separate buckets — it records only
// that at least one consumer charged the usage, so the ledger
// must not count it again.
func (m *Manager) MarkCharged(id string) bool {
	m.mu.Lock()
	entry, ok := m.entries[id]
	if !ok || !entry.finished || !entry.result.UsageSettled || entry.charged {
		m.mu.Unlock()
		return false
	}
	entry.charged = true
	m.mu.Unlock()
	// Persist the charge (F-A17-12): a reboot must not re-fold
	// an already-charged task's tokens into the ledger.
	m.persistSnapshot()
	return true
}

// DrainDeadLetter returns the dead-letter ledger's uncharged token
// total and resets the ledger to zero (tokens and task count).
// It is the ledger's consuming read (F-A9-P2-8): after the
// charged-flag fix the total is exactly "evicted but never
// charged", so a caller — e.g. the app/session-close path — may
// add the drained total to its charged total once, with no
// double-count and no reconciliation. Draining is destructive by
// design: a second call returns 0 until further evictions occur.
// The total is daemon-wide and unattributed to any chat, so it
// must NOT be added to a single chat's per-chat usage line.
//
// The ledger's production consumer for VISIBILITY (F-A10-2) is
// the /usage command (messaging.CommandEnv.DeadLetter, wired in
// cmd/oriond to DeadLetterTokens/DeadLetterTasks): it surfaces
// the ledger as its own daemon-wide line, the charged/uncharged
// split, without draining it — so the counts stay stable across
// /usage calls and DrainDeadLetter remains available for a
// future charge path.
func (m *Manager) DrainDeadLetter() int {
	m.mu.Lock()
	total := m.deadLetterTokens
	m.deadLetterTokens = 0
	m.deadLetterTasks = 0
	m.mu.Unlock()
	// Persist the drain (F-A17-12): a reboot must not resurrect
	// drained tokens.
	m.persistSnapshot()
	return total
}

// evictFinishedLocked deletes the oldest finished entries until the
// registry is at most maxEntries. Callers must hold m.mu.
//
// Token accounting (F-A8-P2-1, semantics F-A9-P2-8) is preserved
// two ways. First, eviction prefers settled finished tasks: an
// unsettled task's usage is not final yet (its executor is still
// running past the timeout), so evicting it would strand the late
// usage the F-A7-2(a) waiter exists to recover — the task is skipped
// while any settled finished task can be evicted instead. Second,
// every evicted task's recorded usage is folded into the
// dead-letter ledger ONLY if the task was never charged (see
// evictLocked): a charged task's tokens are already counted
// elsewhere, and folding them again would double-count. Only when
// every finished entry is unsettled and room is still needed does
// eviction take the oldest unsettled ones — the registry must not
// grow unboundedly — and those entries are marked detached, so
// their late usage still lands in the ledger when it arrives
// (see awaitLateOutcome) instead of on an entry no reader can
// reach.
func (m *Manager) evictFinishedLocked() {
	if len(m.entries) <= maxEntries {
		return
	}
	var finished []*taskEntry
	for _, e := range m.entries {
		if e.finished {
			finished = append(finished, e)
		}
	}
	sort.Slice(finished, func(i, j int) bool {
		if finished[i].startedAt.Equal(finished[j].startedAt) {
			return finished[i].task.ID < finished[j].task.ID
		}
		return finished[i].startedAt.Before(finished[j].startedAt)
	})
	// First pass: settled finished tasks, oldest first.
	for _, e := range finished {
		if len(m.entries) <= maxEntries {
			break
		}
		if !e.result.UsageSettled {
			continue
		}
		m.evictLocked(e)
	}
	// Last resort: all remaining finished entries are unsettled.
	// Evict the oldest anyway — boundedness wins — but the
	// detached mark preserves their late usage (see above).
	for _, e := range finished {
		if len(m.entries) <= maxEntries {
			break
		}
		if e.result.UsageSettled {
			continue
		}
		m.evictLocked(e)
	}
}

// evictLocked removes one finished entry from the registry and
// folds its recorded usage into the dead-letter ledger — but ONLY
// if the task was never charged (F-A9-P2-8): a charged task
// contributes 0 tokens (it still counts in deadLetterTasks), so
// charged + ledger never double-counts. The entry is marked
// detached so a late-arrival waiter that may still hold its
// pointer folds any later usage into the ledger instead of
// writing onto a detached object. Every eviction is logged with
// the token count and whether the task was already charged; the
// log line carries only the task id and counts, never the brief
// or output.
func (m *Manager) evictLocked(e *taskEntry) {
	e.detached = true
	folded := 0
	if !e.charged {
		folded = e.result.Usage.Tokens
		m.deadLetterTokens += folded
	}
	m.deadLetterTasks++
	log.Printf("subagent: evicted task %s: %d tokens folded into dead-letter ledger (already charged: %v)", e.task.ID, folded, e.charged)
	delete(m.entries, e.task.ID)
}

// Forget removes a finished task from the registry, freeing its result.
// It returns false for unknown or still-running tasks. A forgotten
// task's settled usage is NOT folded into the dead-letter ledger:
// forgetting is an explicit discard by the caller, who presumably
// already charged the result or decided to drop it. But if the
// task is still unsettled, nobody could have charged its usage yet
// (both chargers skip unsettled tasks), so the entry is marked
// detached and a late usage that arrives afterwards is folded into
// the ledger by awaitLateOutcome instead of being lost.
func (m *Manager) Forget(id string) bool {
	m.mu.Lock()
	entry, ok := m.entries[id]
	if !ok || !entry.finished {
		m.mu.Unlock()
		return false
	}
	entry.detached = true
	delete(m.entries, id)
	m.mu.Unlock()
	// Persist the removal (F-A17-12): a forgotten task must not
	// come back on restart.
	m.persistSnapshot()
	return true
}

// runTask executes one task's executor and records its outcome. It
// runs in its own goroutine; it always releases the semaphore slot and
// marks the WaitGroup done, and closes entry.done exactly once.
// cancel is the task context's cancel function: it is deferred here so
// the context (and its timer) is released as soon as the task is
// recorded, instead of living until the per-task timeout.
func (m *Manager) runTask(entry *taskEntry, ctx context.Context, cancel context.CancelFunc) {
	defer m.wg.Done()
	defer func() { <-m.sem }()
	if cancel != nil {
		defer cancel()
	}

	ch := make(chan outcome, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- outcome{err: fmt.Errorf("subagent: executor panicked: %v", r)}
			}
		}()
		out, usage, err := entry.exec(ctx, entry.task.Brief)
		ch <- outcome{output: out, usage: usage, err: err}
	}()

	start := entry.startedAt
	var output string
	var usage Usage
	var err error
	var status Status
	// lateUsage is set when the timeout/cancellation branch wins the
	// race: the task is recorded with unsettled (zero) usage and a
	// late-arrival waiter is spawned after the record.
	lateUsage := false

	select {
	case o := <-ch:
		output, usage, err = o.output, o.usage, o.err
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			status = StatusTimeout
			if err == nil {
				err = context.DeadlineExceeded
			}
		case ctx.Err() == context.Canceled:
			status = StatusFailed
			if err == nil {
				err = context.Canceled
			}
		case err != nil:
			status = StatusFailed
		default:
			status = StatusDone
		}
	case <-ctx.Done():
		// Timeout or cancellation won the race. The executor may still
		// be running; the task is recorded promptly with zero usage
		// (UsageSettled false), and a late-arrival waiter records the
		// usage when the executor eventually returns (F-A7-2a) — the
		// buffered channel means the waiter never blocks the executor,
		// and the usage can only be written once (this branch is the
		// only other writer, and it already ran), so a late usage can
		// never be charged twice. The late OUTPUT is discarded: the
		// task is already terminal, only its spend is recovered.
		if ctx.Err() == context.DeadlineExceeded {
			status = StatusTimeout
			err = context.DeadlineExceeded
		} else {
			status = StatusFailed
			err = context.Canceled
		}
		lateUsage = true
	}

	finishedAt := time.Now()
	m.mu.Lock()
	entry.status = status
	entry.result = Result{
		TaskID:       entry.task.ID,
		Output:       output,
		Err:          err,
		Usage:        usage,
		UsageSettled: !lateUsage,
		Duration:     finishedAt.Sub(start),
		FinishedAt:   finishedAt,
	}
	entry.finished = true
	m.mu.Unlock()
	// Persist the finish (F-A17-12): the result and its usage
	// must survive a reboot so the sweep can still charge them.
	// Persist BEFORE closing done: Wait() returning is the
	// durability signal — a waiter that returns must be able to
	// trust the snapshot on disk. Persisting after close(done)
	// let a concurrent file replacement (or a kill in the gap
	// between done and the write) lose the finish: a test that
	// overwrote the file after Wait returned could see its write
	// clobbered by this still-pending persist, and a daemon
	// killed in the gap lost the result from disk.
	m.persistSnapshot()
	m.mu.Lock()
	close(entry.done)
	m.mu.Unlock()
	if lateUsage {
		go m.awaitLateOutcome(entry, ch)
	}
}

// awaitLateOutcome waits for a timed-out or cancelled task's
// executor to return and records its reported usage on the entry
// (F-A7-2a), so GetResult and the per-chat sweep pick it up and the
// next sweep (or session close) charges it. Only the usage is
// recorded — the late output and error are discarded, the task
// stays recorded with its timeout/cancellation status and error.
// The wait ends as soon as the outcome arrives; an executor that
// never returns leaves one goroutine parked on the buffered channel
// for the Manager's lifetime. That is the same order of leak as the
// executor goroutine itself (which is stuck inside user code and
// cannot be interrupted in Go), so no extra bound is imposed: a
// timer on this waiter would forfeit exactly the late usage it
// exists to recover, and the entry is already terminal, so the
// parked waiter holds no semaphore slot and no other resource.
//
// If the entry left the registry while the waiter was parked —
// evicted as a last resort when every finished entry was unsettled
// (F-A8-P2-1), or forgotten — the late usage is folded into the
// dead-letter ledger instead of being written onto a detached
// entry no reader could ever reach. The usage is still unsettled
// at removal time, so no charger could have counted it yet, and
// the fold is exact: the tokens are recorded once, in the ledger.
func (m *Manager) awaitLateOutcome(entry *taskEntry, ch <-chan outcome) {
	o := <-ch
	m.mu.Lock()
	if entry.detached {
		m.deadLetterTokens += o.usage.Tokens
		m.mu.Unlock()
		// Persist the late fold (F-A17-12).
		m.persistSnapshot()
		return
	}
	entry.result.Usage = o.usage
	entry.result.UsageSettled = true
	m.mu.Unlock()
	// Persist the settled usage (F-A17-12): a reboot must not
	// lose tokens that arrived late.
	m.persistSnapshot()
}

// Wait blocks until the named task finishes, or ctx is done. It
// returns the task's Result, or ctx.Err() if the wait itself was
// cancelled/timed out. An unknown id is an error.
func (m *Manager) Wait(ctx context.Context, id string) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	entry, ok := m.entries[id]
	m.mu.Unlock()
	if !ok {
		return Result{}, fmt.Errorf("subagent: unknown task %q", id)
	}
	select {
	case <-entry.done:
		m.mu.Lock()
		res := entry.result
		m.mu.Unlock()
		return res, nil
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// WaitAll waits for all tasks known at call time and returns their
// results in spawn order (oldest first). If ctx is done before all
// tasks finish, it returns the results collected so far. It never
// returns nil: with no tasks it returns an empty slice.
func (m *Manager) WaitAll(ctx context.Context) []Result {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	entries := make([]*taskEntry, 0, len(m.entries))
	for _, e := range m.entries {
		entries = append(entries, e)
	}
	m.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].startedAt.Equal(entries[j].startedAt) {
			return entries[i].task.ID < entries[j].task.ID
		}
		return entries[i].startedAt.Before(entries[j].startedAt)
	})

	results := make([]Result, 0, len(entries))
	for _, entry := range entries {
		select {
		case <-entry.done:
			m.mu.Lock()
			results = append(results, entry.result)
			m.mu.Unlock()
		case <-ctx.Done():
			return results
		}
	}
	return results
}

// Status returns the current status of a task, and whether the id is known.
func (m *Manager) Status(id string) (Status, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[id]
	if !ok {
		return "", false
	}
	return entry.status, true
}

// GetResult returns the Result of a finished task, and whether the
// task is known and finished. For pending/running tasks it returns false.
func (m *Manager) GetResult(id string) (Result, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[id]
	if !ok || !entry.finished {
		return Result{}, false
	}
	return entry.result, true
}

// List returns a snapshot of all known tasks, oldest first.
func (m *Manager) List() []TaskStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TaskStatus, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, TaskStatus{
			ID:        e.task.ID,
			Brief:     e.task.Brief,
			Status:    e.status,
			StartedAt: e.startedAt,
			ChatID:    e.task.ChatID,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}

// Cancel cancels a running task's context. It returns true if the task
// was known and still running; false for unknown or already-finished
// tasks. The task is recorded as failed with context.Canceled once
// its executor returns (promptly, if the executor respects ctx).
func (m *Manager) Cancel(id string) bool {
	m.mu.Lock()
	entry, ok := m.entries[id]
	if !ok || entry.finished {
		m.mu.Unlock()
		return false
	}
	cancel := entry.cancel
	m.mu.Unlock()
	cancel()
	return true
}

// Shutdown cancels all running tasks and waits up to ShutdownTimeout
// (10s) for them to finish. After Shutdown, Spawn fails. Shutdown is
// idempotent and safe to call concurrently; later calls return at once.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	if m.shutdown {
		m.mu.Unlock()
		return
	}
	m.shutdown = true
	cancels := make([]context.CancelFunc, 0, len(m.entries))
	for _, e := range m.entries {
		if !e.finished && e.cancel != nil {
			cancels = append(cancels, e.cancel)
		}
	}
	m.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(ShutdownTimeout):
		// A task whose executor ignores ctx and never returns is
		// abandoned after the bound; its semaphore slot stays held
		// only until its goroutine finally records, and the Manager
		// is shut down either way, so no new work can start.
	}
}
