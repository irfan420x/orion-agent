// Package tools — planning tools: todo_write (a session checklist,
// Claude Code's TodoWrite) and the task store (task_create,
// task_get, task_list, task_update — Claude Code's Task* tools).
//
// The todo list is the agent's working plan for the current turn:
// todo_write replaces the whole checklist (or, with no todos
// argument, reads the current one back). The task store is durable
// planning state for the session: tasks with a title, notes and a
// status, created and updated across turns.
//
// Persistence is per chat/session, under the rc8 per-chat
// directories (<DataDir>/chats/<chatID>/todos.json and tasks.json),
// never in the global memory.json: facts are shared knowledge,
// a plan is one chat's working state. The chat comes from the
// turn's context — the daemon sets it with
// permissions.WithApprovalContext before the agent runs
// (cmd/oriond/main.go), and the agent loop derives every tool
// context from the run context, so the tools see it with no change
// to the tool or model interfaces (the same mechanism the
// permission gate uses). Subagent children inherit the same
// context, and the child registry is built on the PARENT's
// planning store (Registry.SetPlanningStore, wired by
// newChildRegistry in internal/app/subagent.go), so a child
// planning for a chat lands in that chat's files, not another
// chat's — and not in a fresh in-memory store that is discarded
// when the child exits (F-A5-P2-35).
//
// When no chat is in the context (a local CLI run), the state
// lives in <DataDir>/local/ when a data dir is configured, or in
// memory only when no data dir is configured at all (the
// DefaultRegistry default). In-memory state is always bucketed by
// session key — "chat:<id>" or "local" — so two chats can never
// share one checklist even before a data dir is wired.
//
// Files are JSON, 0600, written atomically (temp file + rename, the
// same atomicWriteFile the file tools use). The store never takes a
// path from the model: the directory is derived from config and
// context only, so the file tools' path confinement does not apply
// here and cannot be bypassed through these tools — there is no
// user-supplied path to smuggle in. A corrupt state file is an
// error, never a silent reset: silently dropping the agent's plan
// would hide data loss.
//
// Wiring: DefaultRegistry registers these tools on an in-memory
// store. The daemon (or any host that knows the data dir) points
// the store at it with one call —
// registry.PlanningStore().SetDataDir(cfg.DataDir) — after which
// every turn persists per chat as above. NewPlanningTools takes an
// explicit store for tests and for hosts that want their own.
package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
)

const (
	planningTodosFile = "todos.json"
	planningTasksFile = "tasks.json"
	// maxTodos bounds one checklist; maxTasks bounds one session's
	// task store. Both are small on purpose: a plan with hundreds
	// of entries is not a plan, and the state files stay tiny.
	maxTodos = 50
	maxTasks = 500
	// maxTodoContentRunes / maxTaskFieldRunes bound individual
	// fields so one tool call cannot stuff megabytes into state.
	maxTodoContentRunes = 500
	maxTaskTitleRunes   = 200
	maxTaskFieldRunes   = 4000
	maxTaskIDRunes      = 64
	// maxPlanningStateBytes caps one planning state file on load
	// (F-A5-P2-36): loadTodos/loadTasks used to read the whole
	// file unbounded, so a planted huge tasks.json loaded
	// unbounded into memory. 32 MB is far above any legitimate
	// state — 500 tasks at the maximum field sizes is ~17 MB
	// worst case — and still a bound.
	maxPlanningStateBytes = 32 * 1024 * 1024
	// defaultTaskListLimit / maxTaskListLimit bound one task_list
	// page (F-A5-P2-32): 500 tasks x ~8 KB of fields is ~4 MB of
	// JSON. The default page is 100 tasks; limit/offset page
	// through the rest.
	defaultTaskListLimit = 100
	maxTaskListLimit     = 500
)

// ---------------------------------------------------------------- types

// TodoStatus is one checklist item's state.
type TodoStatus string

const (
	// TodoPending is queued work; TodoInProgress is the item being
	// worked on now; TodoCompleted is done.
	TodoPending    TodoStatus = "pending"
	TodoInProgress TodoStatus = "in_progress"
	TodoCompleted  TodoStatus = "completed"
)

func validTodoStatus(s string) bool {
	switch TodoStatus(s) {
	case TodoPending, TodoInProgress, TodoCompleted:
		return true
	}
	return false
}

// TodoItem is one checklist entry.
type TodoItem struct {
	Content    string     `json:"content"`
	Status     TodoStatus `json:"status"`
	ActiveForm string     `json:"active_form,omitempty"`
}

// TaskStatus is a stored task's state.
type TaskStatus string

const (
	// TaskPending is created but not started; TaskInProgress is
	// being worked on; TaskBlocked is waiting on something else;
	// TaskCompleted is done; TaskCancelled was dropped.
	TaskPending    TaskStatus = "pending"
	TaskInProgress TaskStatus = "in_progress"
	TaskBlocked    TaskStatus = "blocked"
	TaskCompleted  TaskStatus = "completed"
	TaskCancelled  TaskStatus = "cancelled"
)

func validTaskStatus(s string) bool {
	switch TaskStatus(s) {
	case TaskPending, TaskInProgress, TaskBlocked, TaskCompleted, TaskCancelled:
		return true
	}
	return false
}

// Task is one durable planning task.
type Task struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Description string     `json:"description,omitempty"`
	Notes       string     `json:"notes,omitempty"`
	Status      TaskStatus `json:"status"`
	CreatedAt   string     `json:"created_at"`
	UpdatedAt   string     `json:"updated_at"`
}

// ---------------------------------------------------------------- store

// memBucket is one session's in-memory planning state, used when no
// directory resolves for the session (no data dir configured).
// todosSet distinguishes "never written" from "explicitly emptied":
// without it, clearing the checklist would look like "no list yet"
// on the next read — the same distinction the disk files carry by
// existing with an empty array.
type memBucket struct {
	todos    []TodoItem
	todosSet bool
	tasks    []*Task
}

// PlanningStore is the shared state behind the five planning tools.
// A nil *PlanningStore is never valid: NewPlanningTools substitutes
// an in-memory store, so tools always have one.
//
// All methods take s.mu for the whole load-modify-save, so two
// concurrent turns (a parent and a child, two chats in one process)
// serialise instead of losing each other's writes.
type PlanningStore struct {
	mu      sync.Mutex
	dataDir string
	dirFunc func(ctx context.Context) string
	mem     map[string]*memBucket
}

// NewPlanningStore builds a store. dataDir "" means "no persistence
// yet": state stays in memory until SetDataDir (or SetDirFunc)
// points the store at a data directory. A relative dataDir is
// rejected by SetDataDir; the constructor keeps it as-is so a
// zero-value store is still usable in tests that never set one.
func NewPlanningStore(dataDir string) *PlanningStore {
	return &PlanningStore{dataDir: dataDir, mem: map[string]*memBucket{}}
}

// SetDataDir points the store at Orion's data directory: per-chat
// state then persists under <dataDir>/chats/<chatID>/, and
// no-chat state under <dataDir>/local/. A relative path is an
// error — a data dir must be absolute so a later working-directory
// change cannot redirect planning state. An empty path clears the
// data dir (back to in-memory). Safe for concurrent use with tool
// calls.
func (s *PlanningStore) SetDataDir(dir string) error {
	if dir != "" && !filepath.IsAbs(dir) {
		return fmt.Errorf("tools: planning data dir %q must be absolute", dir)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dataDir = dir
	return nil
}

// SetDirFunc installs an override that resolves the session's
// state directory per call (the host may compute it from context
// itself, e.g. from a per-chat dir it already resolved). A
// non-empty result wins over the data-dir/chat derivation; an empty
// result falls through to it. A nil func removes the override.
// Safe for concurrent use with tool calls.
func (s *PlanningStore) SetDirFunc(f func(ctx context.Context) string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirFunc = f
}

// resolveBucket returns the session's state location: dir is the
// directory holding todos.json/tasks.json ("" = in-memory), and key
// is the in-memory bucket name when dir is "". It snapshots the
// data dir / dir func under s.mu, then calls the host-supplied
// dirFunc OUTSIDE the lock: a dirFunc that called back into the
// store would deadlock if it ran under mu. Callers resolve the
// bucket BEFORE taking mu for the load-modify-save; a SetDataDir
// racing the resolve may apply to the next call instead —
// SetDataDir is a wiring-time operation, never a per-turn one, so
// that staleness is harmless.
func (s *PlanningStore) resolveBucket(ctx context.Context) (dir, key string) {
	s.mu.Lock()
	dirFunc := s.dirFunc
	dataDir := s.dataDir
	s.mu.Unlock()
	if dirFunc != nil {
		if d := dirFunc(ctx); d != "" {
			return d, ""
		}
	}
	if chatID, _, ok := permissions.ApprovalContextFrom(ctx); ok && chatID != 0 {
		if dataDir != "" {
			return filepath.Join(dataDir, "chats", strconv.FormatInt(chatID, 10)), ""
		}
		return "", "chat:" + strconv.FormatInt(chatID, 10)
	}
	if dataDir != "" {
		return filepath.Join(dataDir, "local"), ""
	}
	return "", "local"
}

// bucket returns the in-memory bucket for key, creating it.
func (s *PlanningStore) bucket(key string) *memBucket {
	b, ok := s.mem[key]
	if !ok || b == nil {
		b = &memBucket{}
		s.mem[key] = b
	}
	return b
}

// readStateFile reads a planning state file without following
// symlinks and with a size cap. A symlinked state file is refused
// outright (F-A5-P2-34): plain os.ReadFile would follow a link
// planted in chats/<id>/ and let one chat read another chat's
// state. The file is opened O_NOFOLLOW (openNoFollow) so a swap
// between the Lstat and the open cannot redirect the read either,
// and the read is capped at maxPlanningStateBytes+1 so a planted
// huge file cannot load unbounded into memory (F-A5-P2-36). An
// os.IsNotExist error from the Lstat is returned unwrapped so
// callers can treat "never written" as empty state.
func readStateFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("planning state file %q is a symlink; refusing to load", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("planning state file %q is not a regular file", path)
	}
	if info.Size() > maxPlanningStateBytes {
		return nil, fmt.Errorf("planning state file %q is over the %d MB cap", path, maxPlanningStateBytes/(1024*1024))
	}
	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxPlanningStateBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPlanningStateBytes {
		return nil, fmt.Errorf("planning state file %q is over the %d MB cap", path, maxPlanningStateBytes/(1024*1024))
	}
	return data, nil
}

// ensurePlanningDir creates dir (0700) when missing and refuses it
// when it is a symlink or resolves outside the data dir (F-A5-P2-34):
// MkdirAll would silently follow a planted symlinked chats/<id>
// dir on write. The containment check only applies when dir is
// lexically under dataDir — a dirFunc override may legitimately
// point elsewhere, and the host that supplied it is trusted — but
// a dir that claims to be under the data dir yet resolves outside
// it (a symlinked parent) is refused.
//
// A legitimately symlinked data dir itself is NOT a refusal
// (F-A6-P2-12): the data dir is resolved (longest existing
// ancestor, so a not-yet-created data dir under a symlinked
// parent resolves too) before the containment check, and the
// resolved dir is compared against the RESOLVED data dir. Only a
// dir that resolves outside the resolved data dir — e.g. a
// planted symlinked chats/<id> — is refused. Callers must hold
// s.mu: dataDir is read without taking it.
func ensurePlanningDir(dir, dataDir string) error {
	// Resolve the data dir first (F-A6-P2-12): without this, a
	// symlinked data dir makes every resolved planning dir look
	// "outside" the (unresolved) data dir and every write is
	// falsely refused. resolveSymlinks follows the longest
	// existing ancestor, so it works even before the data dir
	// itself exists. If resolution fails (an unreadable
	// ancestor), fall back to the unresolved spelling — the
	// pre-existing behaviour — rather than failing the write.
	resolvedDataDir := dataDir
	if dataDir != "" {
		if resolved, err := resolveSymlinks(dataDir); err == nil {
			resolvedDataDir = resolved
		}
	}
	if info, err := os.Lstat(dir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("planning dir %q is a symlink; refusing", dir)
		}
		if !info.IsDir() {
			return fmt.Errorf("planning dir %q is not a directory", dir)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create planning dir %q: %w", dir, err)
	}
	// Re-check after creation: a parent swapped (or already a
	// symlink) between the Lstat and the MkdirAll must not
	// redirect writes.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("resolve planning dir %q: %w", dir, err)
	}
	if info, err := os.Lstat(resolved); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("planning dir %q is not a real directory", dir)
	}
	if dataDir != "" && within(dir, dataDir) && !within(resolved, resolvedDataDir) {
		return fmt.Errorf("planning dir %q resolves outside the data dir", dir)
	}
	return nil
}

// lockState takes an exclusive flock on the lockfile for one state
// file (<dir>/<file>.lock) and returns a release function. It
// serialises load-modify-save across PROCESSES sharing a data dir
// (F-A5-P2-33): s.mu only serialises in-process, and the atomic
// rename in atomicWriteFile does not fix the read-modify-write
// race — two stores could both load, both append, and the second
// save would silently drop the first's task. The lock is on a
// separate lockfile, never the state file, so the rename does not
// break it, and flock is released automatically if a process dies,
// so a crash can never leave a stale lock behind. Callers must
// hold s.mu (the in-process half) and call this before loading;
// the lock is held until the save completes. The in-memory path
// (dir == "") needs no lock and returns a no-op release.
//
// The flock is taken non-blocking with retries so a stuck holder
// cannot hang the tool call past the caller's context: after 10s
// (or ctx cancellation) the call fails instead of blocking.
func (s *PlanningStore) lockState(ctx context.Context, dir, file string) (func(), error) {
	noop := func() {}
	if dir == "" {
		return noop, nil
	}
	if err := ensurePlanningDir(dir, s.dataDir); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(dir, file+".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open planning lock %q: %w", lockPath, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return func() { _ = f.Close() }, nil
		} else if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("lock planning state %q: %w", file, err)
		}
		if cerr := ctx.Err(); cerr != nil {
			_ = f.Close()
			return nil, cerr
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("timed out waiting for the planning state lock on %q", file)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// loadTodos returns the session's checklist. Callers must hold s.mu.
func (s *PlanningStore) loadTodos(dir, key string) ([]TodoItem, error) {
	if dir == "" {
		b := s.bucket(key)
		if !b.todosSet {
			return nil, nil
		}
		out := make([]TodoItem, len(b.todos))
		copy(out, b.todos)
		return out, nil
	}
	data, err := readStateFile(filepath.Join(dir, planningTodosFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc struct {
		Todos []TodoItem `json:"todos"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("corrupt todos file %q: %w (fix or remove the file to recover)", filepath.Join(dir, planningTodosFile), err)
	}
	if doc.Todos == nil {
		return nil, nil
	}
	// Validate on load, like loadTasks: a hand-edited file with an
	// unknown status must not silently corrupt the store
	// (F-A5-P2-36). A status the write path could never produce is
	// corrupt, not a pending item.
	for _, td := range doc.Todos {
		if !validTodoStatus(string(td.Status)) {
			return nil, fmt.Errorf("corrupt todos file %q: unknown status %q", filepath.Join(dir, planningTodosFile), td.Status)
		}
	}
	return doc.Todos, nil
}

// saveTodos persists the checklist. Callers must hold s.mu.
func (s *PlanningStore) saveTodos(dir, key string, todos []TodoItem) error {
	if dir == "" {
		b := s.bucket(key)
		b.todos = append([]TodoItem(nil), todos...)
		b.todosSet = true
		return nil
	}
	if err := ensurePlanningDir(dir, s.dataDir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(struct {
		Todos []TodoItem `json:"todos"`
	}{Todos: todos}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode todos: %w", err)
	}
	data = append(data, '\n')
	return atomicWriteFile(filepath.Join(dir, planningTodosFile), data, 0o600)
}

// loadTasks returns the session's tasks in creation order. Callers
// must hold s.mu.
func (s *PlanningStore) loadTasks(dir, key string) ([]*Task, error) {
	if dir == "" {
		b := s.bucket(key)
		out := make([]*Task, len(b.tasks))
		for i, t := range b.tasks {
			cp := *t
			out[i] = &cp
		}
		return out, nil
	}
	data, err := readStateFile(filepath.Join(dir, planningTasksFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc struct {
		Tasks []*Task `json:"tasks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("corrupt tasks file %q: %w (fix or remove the file to recover)", filepath.Join(dir, planningTasksFile), err)
	}
	if doc.Tasks == nil {
		return nil, nil
	}
	// Validate on load: a hand-edited file with a duplicate id or
	// an unknown status must not silently corrupt the store.
	seen := make(map[string]bool, len(doc.Tasks))
	for _, t := range doc.Tasks {
		if t == nil || t.ID == "" || seen[t.ID] {
			return nil, fmt.Errorf("corrupt tasks file %q: duplicate or missing task id", filepath.Join(dir, planningTasksFile))
		}
		seen[t.ID] = true
		if !validTaskStatus(string(t.Status)) {
			return nil, fmt.Errorf("corrupt tasks file %q: task %q has unknown status %q", filepath.Join(dir, planningTasksFile), t.ID, t.Status)
		}
	}
	return doc.Tasks, nil
}

// saveTasks persists the tasks in creation order. Callers must hold
// s.mu.
func (s *PlanningStore) saveTasks(dir, key string, tasks []*Task) error {
	if dir == "" {
		b := s.bucket(key)
		b.tasks = tasks
		return nil
	}
	if err := ensurePlanningDir(dir, s.dataDir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(struct {
		Tasks []*Task `json:"tasks"`
	}{Tasks: tasks}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode tasks: %w", err)
	}
	data = append(data, '\n')
	return atomicWriteFile(filepath.Join(dir, planningTasksFile), data, 0o600)
}

// newTaskID mints a task id: "task_" plus 16 hex chars of
// crypto randomness. Collisions are checked by the caller against
// the loaded task list.
//
// The separator is "_" on purpose, not "-": "task-" contains the
// substring "sk-", and the secret scanner's OpenAI pattern
// (sk-[A-Za-z0-9_-]{8,}) would redact every task id to
// "ta[REDACTED]" as tool results pass through Registry.Execute,
// leaving the agent with an id it can never task_get or
// task_update. "task_" + lowercase hex matches no detector
// (verified: RedactSecrets leaves it untouched).
func newTaskID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate task id: %w", err)
	}
	return "task_" + hex.EncodeToString(b[:]), nil
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// renderTodos renders a checklist as text: [x] done, [~] in
// progress, [ ] pending, with a counts header.
func renderTodos(todos []TodoItem) string {
	if len(todos) == 0 {
		return "Todo list is empty."
	}
	done := 0
	for _, t := range todos {
		if t.Status == TodoCompleted {
			done++
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Todo list (%d item(s), %d completed):\n", len(todos), done)
	for _, t := range todos {
		mark := "[ ]"
		switch t.Status {
		case TodoCompleted:
			mark = "[x]"
		case TodoInProgress:
			mark = "[~]"
		}
		fmt.Fprintf(&sb, "%s %s\n", mark, t.Content)
		if t.Status == TodoInProgress && strings.TrimSpace(t.ActiveForm) != "" {
			fmt.Fprintf(&sb, "    (doing: %s)\n", t.ActiveForm)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// ---------------------------------------------------------------- constructor

// NewPlanningTools builds the five planning tools backed by store.
// A nil store gets a fresh in-memory store. The tools share the one
// store, so a todo_write in one turn is visible to task_list in the
// next — that shared, per-session state is the point.
func NewPlanningTools(store *PlanningStore) []Tool {
	if store == nil {
		store = NewPlanningStore("")
	}
	return []Tool{
		&todoWriteTool{store: store},
		&taskCreateTool{store: store},
		&taskGetTool{store: store},
		&taskListTool{store: store},
		&taskUpdateTool{store: store},
	}
}

// PlanningStore returns the planning store behind a registry's
// todo_write tool, or nil when the registry has no planning tools.
// The daemon uses it as the one-line wiring seam:
// registry.PlanningStore().SetDataDir(cfg.DataDir).
func (r *Registry) PlanningStore() *PlanningStore {
	t, ok := r.Get("todo_write")
	if !ok {
		return nil
	}
	tw, ok := t.(*todoWriteTool)
	if !ok || tw.store == nil {
		return nil
	}
	return tw.store
}

// SetPlanningStore re-points the registry's planning tools
// (todo_write, task_create, task_get, task_list, task_update) at
// store, so a child registry can share the parent's planning state
// instead of starting with a fresh in-memory store (F-A5-P2-35).
// A nil store, a nil registry, or a registry without planning tools
// is a no-op. Call before the registry serves turns: the swap
// mutates the tool structs, so it must not race Execute. Sharing
// one store between a parent and a child is safe — PlanningStore
// methods are mutex-guarded in-process and flock-guarded across
// processes, so a parent and a child writing the same chat's state
// serialise instead of losing each other's writes.
func (r *Registry) SetPlanningStore(store *PlanningStore) {
	if r == nil || store == nil {
		return
	}
	for _, name := range []string{"todo_write", "task_create", "task_get", "task_list", "task_update"} {
		t, ok := r.Get(name)
		if !ok {
			continue
		}
		switch pt := t.(type) {
		case *todoWriteTool:
			pt.store = store
		case *taskCreateTool:
			pt.store = store
		case *taskGetTool:
			pt.store = store
		case *taskListTool:
			pt.store = store
		case *taskUpdateTool:
			pt.store = store
		}
	}
}

// ---------------------------------------------------------------- todo_write

type todoWriteTool struct{ store *PlanningStore }

func (t *todoWriteTool) Name() string { return "todo_write" }

func (t *todoWriteTool) Description() string {
	return "Set (or read) the session's plan checklist. Pass todos to REPLACE the whole list: each item has content and a status (pending, in_progress, completed) and an optional active_form describing the work while it is in progress. Call it as the plan changes — when starting work, when an item finishes, when the plan is revised. With no todos argument it just returns the current list. The list is per chat/session and persists across turns."
}

func (t *todoWriteTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"todos": map[string]any{
					"type":        "array",
					"description": "The full new checklist, replacing the old one. Omit to read the current list without changing it; pass an empty array to clear it.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"content":     map[string]any{"type": "string", "description": "What the step is."},
							"status":      map[string]any{"type": "string", "description": "pending, in_progress, or completed."},
							"active_form": map[string]any{"type": "string", "description": "Present-tense form shown while the step is in progress, e.g. \"Writing the tests\"."},
						},
						"required": []string{"content", "status"},
					},
				},
			},
		},
	}
}

type todoArg struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"active_form,omitempty"`
}

type todoWriteArgs struct {
	Todos *[]todoArg `json:"todos,omitempty"`
}

func (t *todoWriteTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a todoWriteArgs
	if err := decodeArgs(t.Name(), args, &a); err != nil {
		return "", err
	}
	dir, key := t.store.resolveBucket(ctx)
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	if a.Todos == nil {
		// Read-only: return the current list without changing it.
		current, err := t.store.loadTodos(dir, key)
		if err != nil {
			return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
		}
		return renderTodos(current), nil
	}
	if len(*a.Todos) > maxTodos {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("todo list is limited to %d items, got %d", maxTodos, len(*a.Todos))}
	}
	todos := make([]TodoItem, 0, len(*a.Todos))
	for i, item := range *a.Todos {
		content := strings.TrimSpace(item.Content)
		if content == "" {
			return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("todos[%d]: content must not be empty", i)}
		}
		if utf8.RuneCountInString(content) > maxTodoContentRunes {
			return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("todos[%d]: content is over %d characters", i, maxTodoContentRunes)}
		}
		status := strings.TrimSpace(item.Status)
		if status == "" {
			status = string(TodoPending)
		}
		if !validTodoStatus(status) {
			return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("todos[%d]: unknown status %q (want pending, in_progress or completed)", i, status)}
		}
		if utf8.RuneCountInString(item.ActiveForm) > maxTodoContentRunes {
			return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("todos[%d]: active_form is over %d characters", i, maxTodoContentRunes)}
		}
		todos = append(todos, TodoItem{Content: content, Status: TodoStatus(status), ActiveForm: strings.TrimSpace(item.ActiveForm)})
	}
	// Exclusive flock around the save (F-A5-P2-33): the read path
	// above is lock-free (the atomic rename makes it consistent),
	// but the write must serialise against another process's
	// load-modify-save. Held until the save completes.
	release, err := t.store.lockState(ctx, dir, planningTodosFile)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	defer release()
	if err := t.store.saveTodos(dir, key, todos); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	return renderTodos(todos), nil
}

// ---------------------------------------------------------------- task_create

type taskCreateTool struct{ store *PlanningStore }

func (t *taskCreateTool) Name() string { return "task_create" }

func (t *taskCreateTool) Description() string {
	return "Create a durable planning task for this session (a unit of work the agent tracks across turns, with a title, optional description/notes, and a status). Returns the task id — use it with task_get, task_list and task_update. Tasks are per chat/session and persist across turns."
}

func (t *taskCreateTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title":       map[string]any{"type": "string", "description": "Short title of the task. Required."},
				"description": map[string]any{"type": "string", "description": "Longer description of what the task covers."},
				"notes":       map[string]any{"type": "string", "description": "Working notes on the task."},
				"status":      map[string]any{"type": "string", "description": "Initial status: pending (default), in_progress, blocked, completed, or cancelled."},
			},
			"required": []string{"title"},
		},
	}
}

type taskCreateArgs struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Notes       string `json:"notes,omitempty"`
	Status      string `json:"status,omitempty"`
}

func validateTaskField(tool, field, value string, maxRunes int) error {
	if utf8.RuneCountInString(value) > maxRunes {
		return &ToolError{Tool: tool, Kind: "invalid_args", Err: fmt.Errorf("%s is over %d characters", field, maxRunes)}
	}
	return nil
}

func (t *taskCreateTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a taskCreateArgs
	if err := decodeArgs(t.Name(), args, &a); err != nil {
		return "", err
	}
	title := strings.TrimSpace(a.Title)
	if title == "" {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("title must not be empty")}
	}
	if err := validateTaskField(t.Name(), "title", title, maxTaskTitleRunes); err != nil {
		return "", err
	}
	if err := validateTaskField(t.Name(), "description", a.Description, maxTaskFieldRunes); err != nil {
		return "", err
	}
	if err := validateTaskField(t.Name(), "notes", a.Notes, maxTaskFieldRunes); err != nil {
		return "", err
	}
	status := strings.TrimSpace(a.Status)
	if status == "" {
		status = string(TaskPending)
	}
	if !validTaskStatus(status) {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("unknown status %q (want pending, in_progress, blocked, completed or cancelled)", status)}
	}
	if err := ctx.Err(); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: err}
	}
	dir, key := t.store.resolveBucket(ctx)
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	// Exclusive flock around load-modify-save (F-A5-P2-33): s.mu
	// serialises only this process, so a daemon and a CLI sharing
	// the data dir would otherwise lose each other's tasks.
	release, err := t.store.lockState(ctx, dir, planningTasksFile)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	defer release()
	tasks, err := t.store.loadTasks(dir, key)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	if len(tasks) >= maxTasks {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("task list is full (%d tasks); complete or cancel old tasks first", maxTasks)}
	}
	// Mint an id that does not collide with an existing task.
	var id string
	for attempts := 0; attempts < 10; attempts++ {
		cand, err := newTaskID()
		if err != nil {
			return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
		}
		collides := false
		for _, existing := range tasks {
			if existing.ID == cand {
				collides = true
				break
			}
		}
		if !collides {
			id = cand
			break
		}
	}
	if id == "" {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: errors.New("could not mint a unique task id")}
	}
	now := nowRFC3339()
	task := &Task{
		ID: id, Title: title,
		Description: strings.TrimSpace(a.Description),
		Notes:       strings.TrimSpace(a.Notes),
		Status:      TaskStatus(status),
		CreatedAt:   now, UpdatedAt: now,
	}
	tasks = append(tasks, task)
	if err := t.store.saveTasks(dir, key, tasks); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	out, err := json.Marshal(task)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("encoding task: %w", err)}
	}
	return string(out), nil
}

// ---------------------------------------------------------------- task_get

type taskGetTool struct{ store *PlanningStore }

func (t *taskGetTool) Name() string { return "task_get" }

func (t *taskGetTool) Description() string {
	return "Fetch one planning task by id (from task_create or task_list), with its title, description, notes, status and timestamps."
}

func (t *taskGetTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string", "description": "Task id, e.g. \"task_...\" from task_create."},
			},
			"required": []string{"id"},
		},
	}
}

type taskIDArgs struct {
	ID string `json:"id"`
}

func cleanTaskID(tool, id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", &ToolError{Tool: tool, Kind: "invalid_args", Err: fmt.Errorf("id must not be empty")}
	}
	if utf8.RuneCountInString(id) > maxTaskIDRunes {
		return "", &ToolError{Tool: tool, Kind: "invalid_args", Err: fmt.Errorf("id is over %d characters", maxTaskIDRunes)}
	}
	return id, nil
}

func (t *taskGetTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a taskIDArgs
	if err := decodeArgs(t.Name(), args, &a); err != nil {
		return "", err
	}
	id, err := cleanTaskID(t.Name(), a.ID)
	if err != nil {
		return "", err
	}
	dir, key := t.store.resolveBucket(ctx)
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	tasks, err := t.store.loadTasks(dir, key)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	for _, task := range tasks {
		if task.ID == id {
			out, err := json.Marshal(task)
			if err != nil {
				return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("encoding task: %w", err)}
			}
			return string(out), nil
		}
	}
	return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("unknown task %q: use task_list to see known tasks", id)}
}

// ---------------------------------------------------------------- task_list

type taskListTool struct{ store *PlanningStore }

func (t *taskListTool) Name() string { return "task_list" }

func (t *taskListTool) Description() string {
	return "List this session's planning tasks in creation order, with id, title, status and timestamps. Optionally filter by one status. Returns at most 100 tasks by default (max 500); pass limit and offset to page through more. The response carries count (tasks on this page), total (all matching tasks), offset and limit."
}

func (t *taskListTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status": map[string]any{"type": "string", "description": "Optional status filter: pending, in_progress, blocked, completed, or cancelled."},
				"limit":  map[string]any{"type": "integer", "description": "Maximum tasks to return on this page. Default 100, max 500."},
				"offset": map[string]any{"type": "integer", "description": "Tasks to skip before this page. Default 0."},
			},
		},
	}
}

type taskListArgs struct {
	Status string `json:"status,omitempty"`
	Limit  *int   `json:"limit,omitempty"`
	Offset *int   `json:"offset,omitempty"`
}

func (t *taskListTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a taskListArgs
	if len(args) > 0 && strings.TrimSpace(string(args)) != "" && strings.TrimSpace(string(args)) != "null" {
		if err := json.Unmarshal(args, &a); err != nil {
			return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("invalid JSON arguments: %w", err)}
		}
	}
	status := strings.TrimSpace(a.Status)
	if status != "" && !validTaskStatus(status) {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("unknown status %q (want pending, in_progress, blocked, completed or cancelled)", status)}
	}
	// Pagination (F-A5-P2-32): an uncapped list is ~4 MB of JSON
	// at 500 tasks. limit clamps to [1, maxTaskListLimit];
	// offset clamps to >= 0.
	limit := defaultTaskListLimit
	if a.Limit != nil {
		limit = *a.Limit
		if limit < 1 {
			limit = 1
		}
		if limit > maxTaskListLimit {
			limit = maxTaskListLimit
		}
	}
	offset := 0
	if a.Offset != nil {
		offset = *a.Offset
		if offset < 0 {
			offset = 0
		}
	}
	dir, key := t.store.resolveBucket(ctx)
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	tasks, err := t.store.loadTasks(dir, key)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	shown := make([]*Task, 0, len(tasks))
	for _, task := range tasks {
		if status != "" && string(task.Status) != status {
			continue
		}
		shown = append(shown, task)
	}
	total := len(shown)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	page := shown[offset:end]
	out, err := json.Marshal(map[string]any{
		"tasks":  page,
		"count":  len(page),
		"total":  total,
		"offset": offset,
		"limit":  limit,
	})
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("encoding task list: %w", err)}
	}
	return string(out), nil
}

// ---------------------------------------------------------------- task_update

type taskUpdateTool struct{ store *PlanningStore }

func (t *taskUpdateTool) Name() string { return "task_update" }

func (t *taskUpdateTool) Description() string {
	return "Update one planning task by id: change its title, description, notes and/or status. At least one field besides id must be given. Returns the updated task."
}

func (t *taskUpdateTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":          map[string]any{"type": "string", "description": "Task id from task_create or task_list."},
				"title":       map[string]any{"type": "string", "description": "New title (must not be empty when given)."},
				"description": map[string]any{"type": "string", "description": "New description."},
				"notes":       map[string]any{"type": "string", "description": "New notes."},
				"status":      map[string]any{"type": "string", "description": "New status: pending, in_progress, blocked, completed, or cancelled."},
			},
			"required": []string{"id"},
		},
	}
}

type taskUpdateArgs struct {
	ID          string  `json:"id"`
	Title       *string `json:"title,omitempty"`
	Description *string `json:"description,omitempty"`
	Notes       *string `json:"notes,omitempty"`
	Status      *string `json:"status,omitempty"`
}

func (t *taskUpdateTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a taskUpdateArgs
	if err := decodeArgs(t.Name(), args, &a); err != nil {
		return "", err
	}
	id, err := cleanTaskID(t.Name(), a.ID)
	if err != nil {
		return "", err
	}
	if a.Title == nil && a.Description == nil && a.Notes == nil && a.Status == nil {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("nothing to update: give at least one of title, description, notes or status")}
	}
	var newTitle, newDesc, newNotes, newStatus string
	if a.Title != nil {
		newTitle = strings.TrimSpace(*a.Title)
		if newTitle == "" {
			return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("title must not be empty")}
		}
		if err := validateTaskField(t.Name(), "title", newTitle, maxTaskTitleRunes); err != nil {
			return "", err
		}
	}
	if a.Description != nil {
		newDesc = strings.TrimSpace(*a.Description)
		if err := validateTaskField(t.Name(), "description", newDesc, maxTaskFieldRunes); err != nil {
			return "", err
		}
	}
	if a.Notes != nil {
		newNotes = strings.TrimSpace(*a.Notes)
		if err := validateTaskField(t.Name(), "notes", newNotes, maxTaskFieldRunes); err != nil {
			return "", err
		}
	}
	if a.Status != nil {
		newStatus = strings.TrimSpace(*a.Status)
		if !validTaskStatus(newStatus) {
			return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("unknown status %q (want pending, in_progress, blocked, completed or cancelled)", newStatus)}
		}
	}
	if err := ctx.Err(); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: err}
	}
	dir, key := t.store.resolveBucket(ctx)
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	// Exclusive flock around load-modify-save (F-A5-P2-33), as in
	// task_create: s.mu alone does not serialise across processes.
	release, err := t.store.lockState(ctx, dir, planningTasksFile)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	defer release()
	tasks, err := t.store.loadTasks(dir, key)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	var updated *Task
	for _, task := range tasks {
		if task.ID == id {
			updated = task
			break
		}
	}
	if updated == nil {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("unknown task %q: use task_list to see known tasks", id)}
	}
	if a.Title != nil {
		updated.Title = newTitle
	}
	if a.Description != nil {
		updated.Description = newDesc
	}
	if a.Notes != nil {
		updated.Notes = newNotes
	}
	if a.Status != nil {
		updated.Status = TaskStatus(newStatus)
	}
	updated.UpdatedAt = nowRFC3339()
	if err := t.store.saveTasks(dir, key, tasks); err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	out, err := json.Marshal(updated)
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("encoding task: %w", err)}
	}
	return string(out), nil
}
