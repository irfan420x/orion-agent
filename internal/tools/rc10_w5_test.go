// Regression tests for the Audit 5 search + planning findings fixed
// in rc10 (worker W5): F-A5-9, F-A5-P2-28 … F-A5-P2-36.
//
// Each test names the finding it pins, so a future regression
// points straight at the audit entry.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/irfan/orion/internal/permissions"
)

// TestRC10GlobExponentialPatternFast (F-A5-9): the audit's proof
// pattern — 20 "**/" segments on a 12-deep tree — took 42.6s for a
// single match with the old recursive matcher. With consecutive
// "**" collapsed and a linear DP matcher it must complete well
// under a second and still find the deep file.
func TestRC10GlobExponentialPatternFast(t *testing.T) {
	root := t.TempDir()
	deep := root
	for i := 0; i < 12; i++ {
		deep = filepath.Join(deep, fmt.Sprintf("d%d", i))
	}
	writeSeedFile(t, filepath.Join(deep, "leaf.go"), "package leaf\n")
	tool := &globTool{roots: []string{root}}
	pattern := strings.Repeat("**/", 20) + "*.go"
	start := time.Now()
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": pattern, "path": root,
	}))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("glob failed: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("glob with 20 ** segments took %v, want well under 1s", elapsed)
	}
	if !strings.Contains(out, "leaf.go") {
		t.Fatalf("deep file not found, got:\n%s", out)
	}
}

// TestRC10GlobPatternBounds (F-A5-9): validateGlobPattern collapses
// consecutive "**" and rejects over-long patterns.
func TestRC10GlobPatternBounds(t *testing.T) {
	segs, err := validateGlobPattern("**/**/**/*.go")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(segs) != 2 || segs[0] != "**" || segs[1] != "*.go" {
		t.Fatalf("**/**/** was not collapsed to one **, got %v", segs)
	}
	if _, err := validateGlobPattern(strings.Repeat("a", maxGlobPatternRunes+1)); err == nil {
		t.Fatalf("pattern over %d runes must be rejected", maxGlobPatternRunes)
	}
	many := strings.Repeat("a/", maxGlobSegments) + "b"
	if _, err := validateGlobPattern(many); err == nil {
		t.Fatalf("pattern over %d segments must be rejected", maxGlobSegments)
	}
}

// TestRC10GrepIncludeExcludeAppliesToSymlinks (F-A5-P2-28): a
// symlinked file must honour include/exclude exactly like a regular
// file — the audit's proof returned the .md file's line for
// include "*.go".
func TestRC10GrepIncludeExcludeAppliesToSymlinks(t *testing.T) {
	root := t.TempDir()
	writeSeedFile(t, filepath.Join(root, "sub", "real.txt"), "NEEDLE_SYM here\n")
	if err := os.Symlink(filepath.Join(root, "sub", "real.txt"), filepath.Join(root, "link.md")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	tool := &grepTool{roots: []string{root}}
	// Control: without a filter the symlinked content is found.
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "NEEDLE_SYM", "path": root,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "NEEDLE_SYM") {
		t.Fatalf("control: symlinked file should match without a filter, got:\n%s", out)
	}
	// With include "*.go" the link named link.md must not match.
	out, err = tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "NEEDLE_SYM", "path": root, "include": "*.go",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "NEEDLE_SYM") {
		t.Fatalf("include *.go must exclude link.md, got:\n%s", out)
	}
	// With exclude "*.md" the same link must not match either.
	out, err = tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "NEEDLE_SYM", "path": root, "exclude": "*.md",
	}))
	if err != nil {
		t.Fatal(err)
	}
	// The regular file sub/real.txt is *.txt, not *.md, so it still
	// matches — but the LINK's copy must not double it: exactly one
	// matching line, from the real file.
	if !strings.Contains(out, "[1 matching line(s)") {
		t.Fatalf("exclude *.md must drop the link copy, got:\n%s", out)
	}
}

// TestRC10GrepSymlinkIntoGitSkipped (F-A5-P2-29): the walk skips
// .git dirs, but a symlink to a file inside .git used to be read.
// The audit's proof returned .git/config's secret line.
func TestRC10GrepSymlinkIntoGitSkipped(t *testing.T) {
	root := t.TempDir()
	writeSeedFile(t, filepath.Join(root, ".git", "config"), "[core]\n\tSECRET_GIT_NEEDLE = yes\n")
	writeSeedFile(t, filepath.Join(root, "ok.txt"), "nothing\n")
	if err := os.Symlink(filepath.Join(root, ".git", "config"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	tool := &grepTool{roots: []string{root}}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "SECRET_GIT_NEEDLE", "path": root,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "SECRET_GIT_NEEDLE") {
		t.Fatalf("symlink into .git must not be read, got:\n%s", out)
	}
	if !strings.Contains(out, "skipped 1 symlinked path(s)") {
		t.Fatalf("expected the .git symlink to be counted as skipped, got:\n%s", out)
	}
}

// TestRC10GrepHardlinkRefused (F-A5-P2-30): a hard link inside the
// root to a secret outside it must not be read — grep returns no
// match and says why, instead of the audit's proof (grep returned
// the secret line).
func TestRC10GrepHardlinkRefused(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	writeSeedFile(t, secret, "HARDLINK_NEEDLE secret\n")
	if err := os.Link(secret, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("cannot create hard link: %v", err)
	}
	tool := &grepTool{roots: []string{root}}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "HARDLINK_NEEDLE", "path": root,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "HARDLINK_NEEDLE") {
		t.Fatalf("hardlinked file must not be read, got:\n%s", out)
	}
	if !strings.Contains(out, "multiple hard links") {
		t.Fatalf("expected a hard-link skip note, got:\n%s", out)
	}
}

// TestRC10FileReadHardlinkRefused (F-A5-P2-30): the same refusal on
// the file_read path, as a clear error rather than a silent skip.
func TestRC10FileReadHardlinkRefused(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	writeSeedFile(t, secret, "HARDLINK_NEEDLE secret\n")
	if err := os.Link(secret, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("cannot create hard link: %v", err)
	}
	tool := &fileReadTool{roots: []string{root}}
	_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"path": filepath.Join(root, "link.txt"),
	}))
	if err == nil {
		t.Fatal("expected an error reading a hardlinked file")
	}
	if !strings.Contains(err.Error(), "more than one hard link") {
		t.Fatalf("expected a hard-link refusal, got: %v", err)
	}
}

// TestRC10GrepTotalBytesCap (F-A5-P2-31): 40 x 1 MB no-match files
// exceed the 32 MB per-call scan cap — the call must stop early and
// say so, instead of reading ~10 GB worst case.
func TestRC10GrepTotalBytesCap(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("x", 1<<20)
	for i := 0; i < 40; i++ {
		writeSeedFile(t, filepath.Join(root, fmt.Sprintf("f%02d.txt", i)), big)
	}
	tool := &grepTool{roots: []string{root}}
	start := time.Now()
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "ZZZ_NO_MATCH_ANYWHERE", "path": root,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatalf("capped scan took too long: %v", time.Since(start))
	}
	if !strings.Contains(out, "(no matches)") {
		t.Fatalf("expected no matches, got:\n%s", out)
	}
	if !strings.Contains(out, "stopped after scanning") || !strings.Contains(out, "cap 32 MB") {
		t.Fatalf("expected a total-bytes cap note, got:\n%s", out)
	}
}

// TestRC10GrepRenderedLinesCap (F-A5-P2-32): 200 matches with 10
// context lines each would render 4200 lines (~10 MB structural
// max with 1000-rune lines). The render cap must cut it at 2000
// lines and say so.
func TestRC10GrepRenderedLinesCap(t *testing.T) {
	root := t.TempDir()
	var sb strings.Builder
	for i := 0; i < 200*25; i++ {
		if i%25 == 0 {
			fmt.Fprintf(&sb, "NEEDLE %d\n", i)
		} else {
			sb.WriteString("filler\n")
		}
	}
	writeSeedFile(t, filepath.Join(root, "many.txt"), sb.String())
	tool := &grepTool{roots: []string{root}}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "NEEDLE", "path": root, "context_lines": 10, "max_matches": 500,
	}))
	if err != nil {
		t.Fatal(err)
	}
	lines := 0
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if !strings.HasPrefix(l, "[") {
			lines++
		}
	}
	if lines > maxGrepRenderedLines {
		t.Fatalf("rendered %d lines, want at most %d", lines, maxGrepRenderedLines)
	}
	if !strings.Contains(out, "output truncated to 2000 rendered lines") {
		t.Fatalf("expected a render-cap note, got:\n%s", out)
	}
}

// TestRC10TaskListPagination (F-A5-P2-32): task_list pages with
// limit/offset and reports the total, instead of returning up to
// 500 tasks x ~8 KB (~4 MB) in one call.
func TestRC10TaskListPagination(t *testing.T) {
	tools := planningToolsFor(t, NewPlanningStore(""))
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		execPlanning(t, tools, ctx, "task_create", map[string]any{"title": fmt.Sprintf("task %d", i)})
	}
	var list struct {
		Tasks  []*Task `json:"tasks"`
		Count  int     `json:"count"`
		Total  int     `json:"total"`
		Offset int     `json:"offset"`
		Limit  int     `json:"limit"`
	}
	decode := func(out string) {
		t.Helper()
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			t.Fatalf("task_list output is not JSON: %v\n%s", err, out)
		}
	}
	// Default page: all 5, total 5.
	decode(execPlanning(t, tools, ctx, "task_list", map[string]any{}))
	if list.Count != 5 || list.Total != 5 || list.Limit != defaultTaskListLimit {
		t.Fatalf("default page wrong: %+v", list)
	}
	// First page of 2.
	decode(execPlanning(t, tools, ctx, "task_list", map[string]any{"limit": 2}))
	if list.Count != 2 || list.Total != 5 || list.Tasks[0].Title != "task 0" {
		t.Fatalf("page 1 wrong: %+v", list)
	}
	// Second page: tasks 2 and 3, in creation order.
	decode(execPlanning(t, tools, ctx, "task_list", map[string]any{"limit": 2, "offset": 2}))
	if list.Count != 2 || list.Tasks[0].Title != "task 2" || list.Tasks[1].Title != "task 3" {
		t.Fatalf("page 2 wrong: %+v", list)
	}
	// Last page: one task; offset past the end: empty, not an error.
	decode(execPlanning(t, tools, ctx, "task_list", map[string]any{"limit": 2, "offset": 4}))
	if list.Count != 1 || list.Tasks[0].Title != "task 4" {
		t.Fatalf("page 3 wrong: %+v", list)
	}
	decode(execPlanning(t, tools, ctx, "task_list", map[string]any{"offset": 99}))
	if list.Count != 0 || list.Total != 5 {
		t.Fatalf("offset past end wrong: %+v", list)
	}
	// limit clamps: 0 -> 1, over-max -> maxTaskListLimit.
	decode(execPlanning(t, tools, ctx, "task_list", map[string]any{"limit": 0}))
	if list.Limit != 1 {
		t.Fatalf("limit 0 must clamp to 1, got %+v", list)
	}
	decode(execPlanning(t, tools, ctx, "task_list", map[string]any{"limit": 9999}))
	if list.Limit != maxTaskListLimit {
		t.Fatalf("limit must clamp to %d, got %+v", maxTaskListLimit, list)
	}
}

// TestRC10PlanningCrossProcessNoLostUpdate (F-A5-P2-33): the audit's
// proof — two PlanningStores on one data dir, overlapped load,
// both append, both save — lost the first task. With the flock
// around load-modify-save, 20 concurrent creates from two stores
// must all survive.
func TestRC10PlanningCrossProcessNoLostUpdate(t *testing.T) {
	dataDir := t.TempDir()
	stores := []*PlanningStore{NewPlanningStore(dataDir), NewPlanningStore(dataDir)}
	const perStore = 10
	var wg sync.WaitGroup
	errs := make(chan error, 2*perStore)
	for si, store := range stores {
		tools := planningToolsFor(t, store)
		for i := 0; i < perStore; i++ {
			wg.Add(1)
			go func(si, i int) {
				defer wg.Done()
				_, err := tools["task_create"].Execute(context.Background(), mustJSON(t, map[string]any{
					"title": fmt.Sprintf("store%d-task%d", si, i),
				}))
				if err != nil {
					errs <- err
				}
			}(si, i)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent task_create failed: %v", err)
	}
	out := execPlanning(t, planningToolsFor(t, stores[0]), context.Background(), "task_list", map[string]any{"limit": 500})
	var list struct {
		Tasks []*Task `json:"tasks"`
		Total int     `json:"total"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatal(err)
	}
	if list.Total != 2*perStore {
		t.Fatalf("lost update: %d of %d tasks survived:\n%s", list.Total, 2*perStore, out)
	}
	for si := 0; si < 2; si++ {
		for i := 0; i < perStore; i++ {
			want := fmt.Sprintf("store%d-task%d", si, i)
			found := false
			for _, task := range list.Tasks {
				if task.Title == want {
					found = true
				}
			}
			if !found {
				t.Errorf("task %q missing after concurrent creates", want)
			}
		}
	}
}

// TestRC10PlanningStateFileSymlinkRefused (F-A5-P2-34): the audit's
// proof — chats/222/tasks.json replaced with a symlink to
// chats/111/tasks.json — let chat B read chat A's "A secret task".
// Loading a symlinked state file is now an error.
func TestRC10PlanningStateFileSymlinkRefused(t *testing.T) {
	dataDir := t.TempDir()
	store := NewPlanningStore(dataDir)
	tools := planningToolsFor(t, store)
	chatA := permissions.WithApprovalContext(context.Background(), 111, 111)
	chatB := permissions.WithApprovalContext(context.Background(), 222, 222)
	execPlanning(t, tools, chatA, "task_create", map[string]any{"title": "A secret task"})
	execPlanning(t, tools, chatB, "task_create", map[string]any{"title": "B task"})
	bFile := filepath.Join(dataDir, "chats", "222", "tasks.json")
	aFile := filepath.Join(dataDir, "chats", "111", "tasks.json")
	if err := os.Remove(bFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(aFile, bFile); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	_, err := tools["task_list"].Execute(chatB, mustJSON(t, map[string]any{}))
	if err == nil {
		t.Fatal("expected an error loading a symlinked tasks file")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected a symlink refusal, got: %v", err)
	}
	// Chat A's own list still works.
	listA := execPlanning(t, tools, chatA, "task_list", map[string]any{})
	if !strings.Contains(listA, "A secret task") {
		t.Fatalf("chat A list broke, got: %s", listA)
	}
}

// TestRC10PlanningSymlinkedChatDirRefused (F-A5-P2-34): a symlinked
// chats/<id> dir, which MkdirAll would follow on write, is refused
// instead of written through.
func TestRC10PlanningSymlinkedChatDirRefused(t *testing.T) {
	dataDir := t.TempDir()
	other := t.TempDir()
	chatsDir := filepath.Join(dataDir, "chats")
	if err := os.MkdirAll(chatsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(chatsDir, "999")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	store := NewPlanningStore(dataDir)
	tools := planningToolsFor(t, store)
	chat := permissions.WithApprovalContext(context.Background(), 999, 999)
	_, err := tools["task_create"].Execute(chat, mustJSON(t, map[string]any{"title": "must not land"}))
	if err == nil {
		t.Fatal("expected an error writing through a symlinked chat dir")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected a symlink refusal, got: %v", err)
	}
	if entries, _ := os.ReadDir(other); len(entries) != 0 {
		t.Fatalf("symlinked target must not receive files, got %v", entries)
	}
}

// TestRC10PlanningTodoStatusValidated (F-A5-P2-36): loadTodos now
// validates statuses like loadTasks does — a hand-edited todos.json
// with status "done" is corrupt, not silently rendered as pending.
func TestRC10PlanningTodoStatusValidated(t *testing.T) {
	dataDir := t.TempDir()
	chat := permissions.WithApprovalContext(context.Background(), 111, 111)
	dir := filepath.Join(dataDir, "chats", "111")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "todos.json"),
		[]byte(`{"todos":[{"content":"x","status":"done"}]}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tools := planningToolsFor(t, NewPlanningStore(dataDir))
	_, err := tools["todo_write"].Execute(chat, mustJSON(t, map[string]any{}))
	if err == nil {
		t.Fatal("expected an error for a todos file with an unknown status")
	}
	if !strings.Contains(err.Error(), "unknown status") {
		t.Fatalf("expected an unknown-status error, got: %v", err)
	}
}

// TestRC10PlanningStateFileSizeCap (F-A5-P2-36): loadTodos/loadTasks
// no longer read the whole file unbounded — a planted 33 MB
// tasks.json is refused at the 32 MB cap.
func TestRC10PlanningStateFileSizeCap(t *testing.T) {
	dataDir := t.TempDir()
	chat := permissions.WithApprovalContext(context.Background(), 111, 111)
	dir := filepath.Join(dataDir, "chats", "111")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, maxPlanningStateBytes+1024*1024)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(filepath.Join(dir, "tasks.json"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	tools := planningToolsFor(t, NewPlanningStore(dataDir))
	_, err := tools["task_list"].Execute(chat, mustJSON(t, map[string]any{}))
	if err == nil {
		t.Fatal("expected an error for an over-cap tasks file")
	}
	if !strings.Contains(err.Error(), "over the 32 MB cap") {
		t.Fatalf("expected a size-cap error, got: %v", err)
	}
}

// TestRC10GrepSingleFileFifoRefused (F-A5-P2-36): a single-file grep
// on a FIFO inside the root used to block up to the 30s
// readFileBounded timeout. The regular-file pre-check (the same one
// file_edit uses) must fail fast with a clear error.
func TestRC10GrepSingleFileFifoRefused(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot create fifo: %v", err)
	}
	tool := &grepTool{roots: []string{root}}
	start := time.Now()
	_, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{
		"pattern": "x", "path": fifo,
	}))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error grepping a fifo")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("fifo grep took %v, want a fast refusal", elapsed)
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("expected a not-a-regular-file error, got: %v", err)
	}
}
