package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/permissions"
)

func planningToolsFor(t *testing.T, store *PlanningStore) map[string]Tool {
	t.Helper()
	tools := NewPlanningTools(store)
	m := make(map[string]Tool, len(tools))
	for _, tl := range tools {
		m[tl.Name()] = tl
	}
	return m
}

func execPlanning(t *testing.T, tools map[string]Tool, ctx context.Context, name string, args map[string]any) string {
	t.Helper()
	out, err := tools[name].Execute(ctx, mustJSON(t, args))
	if err != nil {
		t.Fatalf("%s failed: %v", name, err)
	}
	return out
}

func TestTodoWriteRoundTrip(t *testing.T) {
	tools := planningToolsFor(t, NewPlanningStore(""))
	ctx := context.Background()
	out := execPlanning(t, tools, ctx, "todo_write", map[string]any{
		"todos": []map[string]any{
			{"content": "Write the grep tool", "status": "completed"},
			{"content": "Write the tests", "status": "in_progress", "active_form": "Writing the tests"},
			{"content": "Update the docs", "status": "pending"},
		},
	})
	for _, want := range []string{
		"Todo list (3 item(s), 1 completed):",
		"[x] Write the grep tool",
		"[~] Write the tests",
		"(doing: Writing the tests)",
		"[ ] Update the docs",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Read back with no todos argument: same list, unchanged.
	out2 := execPlanning(t, tools, ctx, "todo_write", map[string]any{})
	if out2 != out {
		t.Errorf("read-back differs:\n%q\nvs\n%q", out2, out)
	}
	// Mark all done: replace the whole list.
	out3 := execPlanning(t, tools, ctx, "todo_write", map[string]any{
		"todos": []map[string]any{
			{"content": "Write the grep tool", "status": "completed"},
			{"content": "Write the tests", "status": "completed"},
			{"content": "Update the docs", "status": "completed"},
		},
	})
	if !strings.Contains(out3, "3 completed") {
		t.Errorf("expected all completed, got:\n%s", out3)
	}
	// Clear with an empty array.
	out4 := execPlanning(t, tools, ctx, "todo_write", map[string]any{
		"todos": []map[string]any{},
	})
	if out4 != "Todo list is empty." {
		t.Errorf("expected empty list, got %q", out4)
	}
	// Clearing is sticky: a later read still shows empty, not the
	// old list (todosSet distinguishes cleared from never-set).
	out5 := execPlanning(t, tools, ctx, "todo_write", map[string]any{})
	if out5 != "Todo list is empty." {
		t.Errorf("cleared list came back, got %q", out5)
	}
}

func TestTodoWriteValidation(t *testing.T) {
	tools := planningToolsFor(t, NewPlanningStore(""))
	ctx := context.Background()
	bad := []map[string]any{
		{"todos": []map[string]any{{"content": "x", "status": "done"}}},
		{"todos": []map[string]any{{"content": "   ", "status": "pending"}}},
		{"todos": []map[string]any{{"content": strings.Repeat("x", 501), "status": "pending"}}},
	}
	for _, args := range bad {
		_, err := tools["todo_write"].Execute(ctx, mustJSON(t, args))
		if err == nil {
			t.Fatalf("args %v: expected error", args)
		}
		if kind := toolErrKind(t, err); kind != "invalid_args" {
			t.Fatalf("args %v: expected invalid_args, got %q", args, kind)
		}
	}
	// More than maxTodos items.
	many := make([]map[string]any, 0, maxTodos+1)
	for i := 0; i <= maxTodos; i++ {
		many = append(many, map[string]any{"content": "item", "status": "pending"})
	}
	_, err := tools["todo_write"].Execute(ctx, mustJSON(t, map[string]any{"todos": many}))
	if err == nil {
		t.Fatal("expected error for over-long todo list")
	}
}

func TestTaskCreateGetListUpdateCycle(t *testing.T) {
	store := NewPlanningStore("")
	tools := planningToolsFor(t, store)
	ctx := context.Background()

	created := execPlanning(t, tools, ctx, "task_create", map[string]any{
		"title": "Implement glob", "description": "Recursive matching", "notes": "start with **",
	})
	var task Task
	if err := json.Unmarshal([]byte(created), &task); err != nil {
		t.Fatalf("task_create output is not a task: %v\n%s", err, created)
	}
	if task.ID == "" || !strings.HasPrefix(task.ID, "task_") {
		t.Fatalf("bad task id %q", task.ID)
	}
	if task.Status != TaskPending || task.Title != "Implement glob" {
		t.Fatalf("unexpected created task: %+v", task)
	}
	if task.CreatedAt == "" || task.UpdatedAt == "" || task.CreatedAt != task.UpdatedAt {
		t.Fatalf("timestamps wrong: %+v", task)
	}

	// Get round-trips.
	got := execPlanning(t, tools, ctx, "task_get", map[string]any{"id": task.ID})
	var fetched Task
	if err := json.Unmarshal([]byte(got), &fetched); err != nil {
		t.Fatal(err)
	}
	if fetched.ID != task.ID || fetched.Notes != "start with **" {
		t.Fatalf("task_get mismatch: %+v", fetched)
	}

	// List shows it.
	listed := execPlanning(t, tools, ctx, "task_list", map[string]any{})
	var list struct {
		Tasks []*Task `json:"tasks"`
		Count int     `json:"count"`
	}
	if err := json.Unmarshal([]byte(listed), &list); err != nil {
		t.Fatal(err)
	}
	if list.Count != 1 || list.Tasks[0].ID != task.ID {
		t.Fatalf("task_list wrong: %s", listed)
	}

	// Update status + notes.
	updated := execPlanning(t, tools, ctx, "task_update", map[string]any{
		"id": task.ID, "status": "in_progress", "notes": "matcher done",
	})
	var upd Task
	if err := json.Unmarshal([]byte(updated), &upd); err != nil {
		t.Fatal(err)
	}
	if upd.Status != TaskInProgress || upd.Notes != "matcher done" {
		t.Fatalf("task_update did not apply: %+v", upd)
	}
	if upd.Title != "Implement glob" || upd.Description != "Recursive matching" {
		t.Fatalf("task_update clobbered untouched fields: %+v", upd)
	}
	// Get again: the update stuck.
	got2 := execPlanning(t, tools, ctx, "task_get", map[string]any{"id": task.ID})
	var fetched2 Task
	if err := json.Unmarshal([]byte(got2), &fetched2); err != nil {
		t.Fatal(err)
	}
	if fetched2.Status != TaskInProgress || fetched2.Notes != "matcher done" {
		t.Fatalf("update did not persist: %+v", fetched2)
	}
}

// TestTaskIDSurvivesRegistryRedaction is the regression test for a
// real bug the first live proof caught: task ids minted as
// "task-<hex>" contain the substring "sk-", so the secret
// scanner's OpenAI pattern redacted every id to "ta[REDACTED]"
// as the result passed through Registry.Execute — and the agent
// could never task_get or task_update the task it had just
// created. The direct-tool tests above never saw it because they
// bypass the registry. This test drives the full cycle through
// the registry, the way the agent loop does.
func TestTaskIDSurvivesRegistryRedaction(t *testing.T) {
	r, err := DefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	call := func(name string, args map[string]any) string {
		t.Helper()
		b, _ := json.Marshal(args)
		out, err := r.Execute(ctx, name, b)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return out
	}
	created := call("task_create", map[string]any{"title": "Registry round-trip"})
	var task Task
	if err := json.Unmarshal([]byte(created), &task); err != nil {
		t.Fatalf("create output is not a task: %v\n%s", err, created)
	}
	if strings.Contains(task.ID, "[REDACTED]") || !strings.HasPrefix(task.ID, "task_") {
		t.Fatalf("task id was mangled by redaction: %q (raw: %s)", task.ID, created)
	}
	updated := call("task_update", map[string]any{"id": task.ID, "status": "in_progress", "notes": "via registry"})
	var upd Task
	if err := json.Unmarshal([]byte(updated), &upd); err != nil {
		t.Fatalf("update output is not a task: %v\n%s", err, updated)
	}
	if upd.ID != task.ID || upd.Status != TaskInProgress {
		t.Fatalf("registry task_update did not apply: %+v", upd)
	}
	got := call("task_get", map[string]any{"id": task.ID})
	var fetched Task
	if err := json.Unmarshal([]byte(got), &fetched); err != nil {
		t.Fatalf("get output is not a task: %v\n%s", err, got)
	}
	if fetched.ID != task.ID || fetched.Status != TaskInProgress {
		t.Fatalf("registry task_get mismatch: %+v", fetched)
	}
}

func TestTaskValidationAndUnknownIDs(t *testing.T) {
	tools := planningToolsFor(t, NewPlanningStore(""))
	ctx := context.Background()
	cases := []struct {
		name string
		args map[string]any
	}{
		{"create empty title", map[string]any{"title": "   "}},
		{"create bad status", map[string]any{"title": "x", "status": "done"}},
		{"create long title", map[string]any{"title": strings.Repeat("x", 201)}},
		{"get unknown", map[string]any{"id": "task-nope"}},
		{"get empty id", map[string]any{"id": ""}},
		{"update unknown", map[string]any{"id": "task-nope", "status": "completed"}},
		{"update nothing", map[string]any{"id": "task-nope"}},
		{"list bad status", map[string]any{"status": "done"}},
	}
	for _, tc := range cases {
		tool := "task_create"
		switch {
		case strings.HasPrefix(tc.name, "get"):
			tool = "task_get"
		case strings.HasPrefix(tc.name, "update"):
			tool = "task_update"
		case strings.HasPrefix(tc.name, "list"):
			tool = "task_list"
		}
		_, err := tools[tool].Execute(ctx, mustJSON(t, tc.args))
		if err == nil {
			t.Fatalf("%s: expected error", tc.name)
		}
		if kind := toolErrKind(t, err); kind != "invalid_args" {
			t.Fatalf("%s: expected invalid_args, got %q (%v)", tc.name, kind, err)
		}
	}
	// Update with an empty title is invalid even when the id exists.
	created := execPlanning(t, tools, ctx, "task_create", map[string]any{"title": "real"})
	var task Task
	if err := json.Unmarshal([]byte(created), &task); err != nil {
		t.Fatal(err)
	}
	_, err := tools["task_update"].Execute(ctx, mustJSON(t, map[string]any{"id": task.ID, "title": "  "}))
	if err == nil {
		t.Fatal("expected error for empty title update")
	}
}

func TestTaskListFilter(t *testing.T) {
	tools := planningToolsFor(t, NewPlanningStore(""))
	ctx := context.Background()
	for _, tc := range []struct {
		title, status string
	}{
		{"one", "pending"}, {"two", "in_progress"}, {"three", "completed"},
	} {
		execPlanning(t, tools, ctx, "task_create", map[string]any{"title": tc.title, "status": tc.status})
	}
	out := execPlanning(t, tools, ctx, "task_list", map[string]any{"status": "completed"})
	var list struct {
		Tasks []*Task `json:"tasks"`
		Count int     `json:"count"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatal(err)
	}
	if list.Count != 1 || list.Tasks[0].Title != "three" {
		t.Fatalf("filtered list wrong: %s", out)
	}
	out = execPlanning(t, tools, ctx, "task_list", map[string]any{})
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatal(err)
	}
	if list.Count != 3 {
		t.Fatalf("unfiltered list should have 3, got %s", out)
	}
	// Creation order is preserved.
	if list.Tasks[0].Title != "one" || list.Tasks[2].Title != "three" {
		t.Fatalf("tasks not in creation order: %s", out)
	}
}

func TestPlanningPerChatIsolationOnDisk(t *testing.T) {
	dataDir := t.TempDir()
	store := NewPlanningStore(dataDir)
	tools := planningToolsFor(t, store)
	chatA := permissions.WithApprovalContext(context.Background(), 111, 111)
	chatB := permissions.WithApprovalContext(context.Background(), 222, 222)

	execPlanning(t, tools, chatA, "task_create", map[string]any{"title": "chat A task"})
	execPlanning(t, tools, chatA, "todo_write", map[string]any{
		"todos": []map[string]any{{"content": "A todo", "status": "pending"}},
	})
	execPlanning(t, tools, chatB, "task_create", map[string]any{"title": "chat B task"})

	// Chat B sees only its own task; chat A sees only its own.
	listB := execPlanning(t, tools, chatB, "task_list", map[string]any{})
	if strings.Contains(listB, "chat A task") || !strings.Contains(listB, "chat B task") {
		t.Fatalf("chat B list leaked or missing: %s", listB)
	}
	listA := execPlanning(t, tools, chatA, "task_list", map[string]any{})
	if strings.Contains(listA, "chat B task") || !strings.Contains(listA, "chat A task") {
		t.Fatalf("chat A list leaked or missing: %s", listA)
	}
	todosB := execPlanning(t, tools, chatB, "todo_write", map[string]any{})
	if todosB != "Todo list is empty." {
		t.Fatalf("chat B must have no todos, got %q", todosB)
	}
	todosA := execPlanning(t, tools, chatA, "todo_write", map[string]any{})
	if !strings.Contains(todosA, "A todo") {
		t.Fatalf("chat A todos missing, got %q", todosA)
	}

	// Files land exactly under the rc8 per-chat dirs.
	for _, want := range []string{
		filepath.Join(dataDir, "chats", "111", "tasks.json"),
		filepath.Join(dataDir, "chats", "111", "todos.json"),
		filepath.Join(dataDir, "chats", "222", "tasks.json"),
	} {
		if _, err := os.Stat(want); err != nil {
			t.Errorf("expected state file %s: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, "chats", "222", "todos.json")); !os.IsNotExist(err) {
		t.Errorf("chat B never wrote todos, yet todos.json exists")
	}
	// State files are 0600.
	info, err := os.Stat(filepath.Join(dataDir, "chats", "111", "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("tasks.json mode = %o, want 0600", info.Mode().Perm())
	}
	// No global memory.json was touched: nothing at the data-dir root.
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "chats" {
		t.Errorf("expected only chats/ under data dir, got %v", entries)
	}
}

func TestPlanningLocalBucketWithoutChat(t *testing.T) {
	dataDir := t.TempDir()
	tools := planningToolsFor(t, NewPlanningStore(dataDir))
	execPlanning(t, tools, context.Background(), "task_create", map[string]any{"title": "local task"})
	if _, err := os.Stat(filepath.Join(dataDir, "local", "tasks.json")); err != nil {
		t.Fatalf("expected local/tasks.json: %v", err)
	}
}

func TestPlanningInMemoryBucketsArePerChat(t *testing.T) {
	// No data dir at all: state is in-memory, but still bucketed
	// per chat — two chats must never share one checklist.
	tools := planningToolsFor(t, NewPlanningStore(""))
	chatA := permissions.WithApprovalContext(context.Background(), 111, 111)
	chatB := permissions.WithApprovalContext(context.Background(), 222, 222)
	execPlanning(t, tools, chatA, "todo_write", map[string]any{
		"todos": []map[string]any{{"content": "A only", "status": "pending"}},
	})
	got := execPlanning(t, tools, chatB, "todo_write", map[string]any{})
	if got != "Todo list is empty." {
		t.Fatalf("in-memory todos leaked across chats, got %q", got)
	}
}

func TestPlanningPersistsAcrossStoreInstances(t *testing.T) {
	dataDir := t.TempDir()
	chat := permissions.WithApprovalContext(context.Background(), 111, 111)
	tools1 := planningToolsFor(t, NewPlanningStore(dataDir))
	created := execPlanning(t, tools1, chat, "task_create", map[string]any{"title": "durable"})
	var task Task
	if err := json.Unmarshal([]byte(created), &task); err != nil {
		t.Fatal(err)
	}
	// A fresh store (a daemon restart) reads the same files back.
	tools2 := planningToolsFor(t, NewPlanningStore(dataDir))
	got := execPlanning(t, tools2, chat, "task_get", map[string]any{"id": task.ID})
	var fetched Task
	if err := json.Unmarshal([]byte(got), &fetched); err != nil {
		t.Fatal(err)
	}
	if fetched.Title != "durable" {
		t.Fatalf("task did not survive a store rebuild: %+v", fetched)
	}
}

func TestPlanningCorruptFileIsAnError(t *testing.T) {
	dataDir := t.TempDir()
	chat := permissions.WithApprovalContext(context.Background(), 111, 111)
	dir := filepath.Join(dataDir, "chats", "111")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tasks.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	tools := planningToolsFor(t, NewPlanningStore(dataDir))
	_, err := tools["task_list"].Execute(chat, mustJSON(t, map[string]any{}))
	if err == nil {
		t.Fatal("expected error for corrupt tasks file")
	}
	if kind := toolErrKind(t, err); kind != "execution" {
		t.Fatalf("expected execution, got %q (%v)", kind, err)
	}
}

func TestPlanningStoreSetDataDir(t *testing.T) {
	store := NewPlanningStore("")
	if err := store.SetDataDir("relative/path"); err == nil {
		t.Fatal("expected error for relative data dir")
	}
	if err := store.SetDataDir(t.TempDir()); err != nil {
		t.Fatalf("absolute data dir must be accepted: %v", err)
	}
	if err := store.SetDataDir(""); err != nil {
		t.Fatalf("clearing the data dir must work: %v", err)
	}
}

// TestPlanningSymlinkedDataDirIsAccepted (F-A6-P2-12): a data dir
// that is itself a symlink — a legitimate deployment layout, e.g.
// ~/.orion -> /mnt/data/orion — must not be refused. Before the
// fix, ensurePlanningDir compared the resolved planning dir
// against the UNRESOLVED data dir, so every write under a
// symlinked data dir failed with "resolves outside the data dir".
func TestPlanningSymlinkedDataDirIsAccepted(t *testing.T) {
	real := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// The store is pointed at the symlink; a task_create for a
	// chat must persist under the REAL directory.
	store := NewPlanningStore(link)
	tools := planningToolsFor(t, store)
	chat := permissions.WithApprovalContext(context.Background(), 111, 111)
	execPlanning(t, tools, chat, "task_create", map[string]any{"title": "symlinked data dir task"})
	if _, err := os.Stat(filepath.Join(real, "chats", "111", "tasks.json")); err != nil {
		t.Fatalf("task not persisted under the real data dir: %v", err)
	}
	list := execPlanning(t, tools, chat, "task_list", map[string]any{})
	if !strings.Contains(list, "symlinked data dir task") {
		t.Fatalf("task_list missing the task: %s", list)
	}
}

// TestPlanningSymlinkedDataDirNotYetCreated (F-A6-P2-12): the same
// acceptance must hold when the data dir does not exist yet — the
// symlink is on a PARENT (e.g. the TempDir itself is reached
// through a link) and only the leaf is created by the write.
func TestPlanningSymlinkedDataDirNotYetCreated(t *testing.T) {
	real := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// The data dir itself (under the link) does not exist yet.
	missing := filepath.Join(link, "orion-data")
	if err := ensurePlanningDir(filepath.Join(missing, "chats", "111"), missing); err != nil {
		t.Fatalf("ensurePlanningDir under a not-yet-created symlinked data dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(real, "orion-data", "chats", "111")); err != nil {
		t.Fatalf("planning dir not created under the real data dir: %v", err)
	}
}

// TestPlanningEscapingSymlinkStillRefused (F-A6-P2-12 guard): the
// fix must not weaken the F-A5-P2-34 refusal — a planning dir
// that resolves OUTSIDE the (resolved) data dir, e.g. a planted
// symlinked chats/ dir, is still refused, and a planning dir
// that is itself a symlink is still refused.
func TestPlanningEscapingSymlinkStillRefused(t *testing.T) {
	dataDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	// Plant chats/ as a symlink escaping the data dir.
	if err := os.Symlink(outside, filepath.Join(dataDir, "chats")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := ensurePlanningDir(filepath.Join(dataDir, "chats", "111"), dataDir); err == nil {
		t.Fatal("planning dir escaping the data dir via a symlinked chats/ must be refused")
	}
	// A planning dir that is itself a symlink is refused too.
	real := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(filepath.Join(real, "chats"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(real, "chats", "111")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := ensurePlanningDir(filepath.Join(real, "chats", "111"), real); err == nil {
		t.Fatal("a planning dir that is itself a symlink must be refused")
	}
}

func TestPlanningDirFuncOverride(t *testing.T) {
	dataDir := t.TempDir()
	store := NewPlanningStore(dataDir)
	custom := filepath.Join(dataDir, "custom-session")
	store.SetDirFunc(func(ctx context.Context) string { return custom })
	tools := planningToolsFor(t, store)
	chat := permissions.WithApprovalContext(context.Background(), 111, 111)
	execPlanning(t, tools, chat, "task_create", map[string]any{"title": "custom dir task"})
	if _, err := os.Stat(filepath.Join(custom, "tasks.json")); err != nil {
		t.Fatalf("dirFunc override not honoured: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "chats", "111", "tasks.json")); !os.IsNotExist(err) {
		t.Errorf("dirFunc override must win over the chat dir")
	}
}

func TestRegistryPlanningStoreSeam(t *testing.T) {
	r, err := DefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	store := r.PlanningStore()
	if store == nil {
		t.Fatal("DefaultRegistry must expose its planning store")
	}
	dataDir := t.TempDir()
	if err := store.SetDataDir(dataDir); err != nil {
		t.Fatal(err)
	}
	chat := permissions.WithApprovalContext(context.Background(), 999, 999)
	out, err := r.Execute(chat, "task_create", mustJSON(t, map[string]any{"title": "via registry"}))
	if err != nil {
		t.Fatalf("registry task_create failed: %v", err)
	}
	if !strings.Contains(out, "via registry") {
		t.Fatalf("unexpected output: %s", out)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "chats", "999", "tasks.json")); err != nil {
		t.Fatalf("registry planning tools must persist per chat: %v", err)
	}
	// All seven new tools are reachable through the registry.
	for _, name := range []string{"grep", "glob", "todo_write", "task_create", "task_get", "task_list", "task_update"} {
		if _, ok := r.Get(name); !ok {
			t.Errorf("tool %q not in DefaultRegistry", name)
		}
	}
}

func TestPlanningToolsHaveDefinitions(t *testing.T) {
	for _, tl := range NewPlanningTools(nil) {
		def := tl.Definition()
		if def.Name == "" || def.Description == "" || def.Parameters == nil {
			t.Errorf("tool %+v has an incomplete definition", tl.Name())
		}
	}
	for _, tl := range []Tool{&grepTool{}, &globTool{}} {
		def := tl.Definition()
		if def.Name == "" || def.Description == "" || def.Parameters == nil {
			t.Errorf("tool %q has an incomplete definition", tl.Name())
		}
	}
}
