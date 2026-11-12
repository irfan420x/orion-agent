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
// even if the executor ignores its context. A late executor result is
// discarded (the result channel is buffered).
package subagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
}

// Result is the outcome of a finished subagent task. Err is non-nil
// for failed and timed-out tasks.
type Result struct {
	TaskID     string
	Output     string
	Err        error
	Duration   time.Duration
	FinishedAt time.Time
}

// TaskStatus is a snapshot of one task for List.
type TaskStatus struct {
	ID        string
	Brief     string
	Status    Status
	StartedAt time.Time
}

// Executor runs one brief and returns its output. Implementations must
// respect ctx cancellation/timeout where possible; the Manager enforces
// the timeout regardless, so an executor that ignores ctx is still
// recorded as timed out on schedule (its late result is discarded).
// The real agent loop is plugged in by the caller via
// NewManagerWithExecutor or SetExecutor.
type Executor func(ctx context.Context, brief string) (string, error)

// taskEntry is the Manager's internal record for one task. done is
// closed exactly once, when the task finishes and result is set.
type taskEntry struct {
	task      Task
	exec      Executor
	status    Status
	startedAt time.Time
	cancel    context.CancelFunc
	done      chan struct{}
	result    Result
	finished  bool
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
func (m *Manager) Spawn(brief string, timeout time.Duration) (string, error) {
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
		task:      Task{ID: id, Brief: brief, Timeout: timeout, CreatedAt: now},
		exec:      exec,
		status:    StatusRunning,
		startedAt: now,
		cancel:    cancel,
		done:      make(chan struct{}),
	}
	m.entries[id] = entry
	m.wg.Add(1)
	// Bound memory: evict the oldest finished tasks once the registry
	// grows past maxEntries, so a long daemon run cannot grow List /
	// WaitAll without bound.
	m.evictFinishedLocked()
	m.mu.Unlock()

	go m.runTask(entry, ctx, cancel)
	return id, nil
}

// maxEntries bounds the task registry. Finished tasks beyond this are
// evicted oldest-first; running tasks are never evicted.
const maxEntries = 100

// evictFinishedLocked deletes the oldest finished entries until the
// registry is at most maxEntries. Callers must hold m.mu.
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
	for _, e := range finished {
		if len(m.entries) <= maxEntries {
			break
		}
		delete(m.entries, e.task.ID)
	}
}

// Forget removes a finished task from the registry, freeing its result.
// It returns false for unknown or still-running tasks.
func (m *Manager) Forget(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[id]
	if !ok || !entry.finished {
		return false
	}
	delete(m.entries, id)
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

	type outcome struct {
		output string
		err    error
	}
	ch := make(chan outcome, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- outcome{err: fmt.Errorf("subagent: executor panicked: %v", r)}
			}
		}()
		out, err := entry.exec(ctx, entry.task.Brief)
		ch <- outcome{output: out, err: err}
	}()

	start := entry.startedAt
	var output string
	var err error
	var status Status

	select {
	case o := <-ch:
		output, err = o.output, o.err
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
		// be running; its late result is discarded via the buffered
		// channel, so this task is recorded promptly.
		if ctx.Err() == context.DeadlineExceeded {
			status = StatusTimeout
			err = context.DeadlineExceeded
		} else {
			status = StatusFailed
			err = context.Canceled
		}
	}

	finishedAt := time.Now()
	m.mu.Lock()
	entry.status = status
	entry.result = Result{
		TaskID:     entry.task.ID,
		Output:     output,
		Err:        err,
		Duration:   finishedAt.Sub(start),
		FinishedAt: finishedAt,
	}
	entry.finished = true
	close(entry.done)
	m.mu.Unlock()
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
