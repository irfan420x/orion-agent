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
// context, so a child planning for a chat lands in that chat's
// files, not another chat's.
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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	data, err := os.ReadFile(filepath.Join(dir, planningTodosFile))
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
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create planning dir %q: %w", dir, err)
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
	data, err := os.ReadFile(filepath.Join(dir, planningTasksFile))
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
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create planning dir %q: %w", dir, err)
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
	return "List this session's planning tasks in creation order, with id, title, status and timestamps. Optionally filter by one status."
}

func (t *taskListTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status": map[string]any{"type": "string", "description": "Optional status filter: pending, in_progress, blocked, completed, or cancelled."},
			},
		},
	}
}

type taskListArgs struct {
	Status string `json:"status,omitempty"`
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
	out, err := json.Marshal(map[string]any{
		"tasks": shown,
		"count": len(shown),
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
