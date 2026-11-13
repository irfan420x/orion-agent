package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/subagent"
)

// fakeUsageSink records AddSubagentUsage calls for assertions.
type fakeUsageSink struct {
	mu    sync.Mutex
	total int
	calls int
}

func (s *fakeUsageSink) AddSubagentUsage(tokens int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total += tokens
	s.calls++
}

func (s *fakeUsageSink) snapshot() (total, calls int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total, s.calls
}

// instantExecutor returns output + usage immediately, ignoring ctx.
func instantExecutor(output string, tokens int) subagent.Executor {
	return func(ctx context.Context, brief string) (string, subagent.Usage, error) {
		return output, subagent.Usage{Tokens: tokens}, nil
	}
}

// blockingExecutor blocks on release (respecting ctx) until the test
// closes it, so the task is deterministically still running.
func blockingExecutor(release chan struct{}, output string, tokens int) subagent.Executor {
	return func(ctx context.Context, brief string) (string, subagent.Usage, error) {
		select {
		case <-release:
			return output, subagent.Usage{Tokens: tokens}, nil
		case <-ctx.Done():
			return "", subagent.Usage{}, ctx.Err()
		}
	}
}

// buildSubagentTools builds the four tools against a fresh manager
// with the given executor, returning the tools by name.
func buildSubagentTools(t *testing.T, maxConcurrent int, exec subagent.Executor) (map[string]Tool, *subagent.Manager, *fakeUsageSink) {
	t.Helper()
	mgr := subagent.NewManagerWithExecutor(maxConcurrent, exec)
	t.Cleanup(mgr.Shutdown)
	sink := &fakeUsageSink{}
	tools, err := NewSubagentTools(mgr, sink)
	if err != nil {
		t.Fatalf("NewSubagentTools: %v", err)
	}
	byName := make(map[string]Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	return byName, mgr, sink
}

// callTool runs a tool with args marshalled from v, failing the test
// on marshal errors.
func callTool(t *testing.T, tool Tool, v any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return tool.Execute(context.Background(), raw)
}

func parseJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("result %q is not JSON: %v", s, err)
	}
	return m
}

func subagentToolErrKind(t *testing.T, err error) string {
	t.Helper()
	var te *ToolError
	if !errors.As(err, &te) {
		t.Fatalf("err = %v (%T), want *ToolError", err, err)
	}
	return te.Kind
}

func TestNewSubagentToolsNilArgs(t *testing.T) {
	mgr := subagent.NewManager(4)
	defer mgr.Shutdown()
	sink := &fakeUsageSink{}
	if _, err := NewSubagentTools(nil, sink); err == nil {
		t.Fatal("NewSubagentTools(nil manager): want error, got nil")
	}
	if _, err := NewSubagentTools(mgr, nil); err == nil {
		t.Fatal("NewSubagentTools(nil sink): want error, got nil")
	}
}

func TestNewSubagentToolsNamesAndRegistry(t *testing.T) {
	mgr := subagent.NewManager(4)
	defer mgr.Shutdown()
	tools, err := NewSubagentTools(mgr, &fakeUsageSink{})
	if err != nil {
		t.Fatalf("NewSubagentTools: %v", err)
	}
	want := []string{"subagent_spawn", "subagent_list", "subagent_result", "subagent_stop"}
	if len(tools) != len(want) {
		t.Fatalf("got %d tools, want %d", len(tools), len(want))
	}
	r := NewRegistry()
	for i, tool := range tools {
		if tool.Name() != want[i] {
			t.Fatalf("tool %d: name = %q, want %q", i, tool.Name(), want[i])
		}
		if tool.Description() == "" {
			t.Fatalf("tool %q: empty description", tool.Name())
		}
		if err := r.Register(tool); err != nil {
			t.Fatalf("Register %q: %v", tool.Name(), err)
		}
	}
}

func TestSubagentSpawnHappyPath(t *testing.T) {
	tools, _, _ := buildSubagentTools(t, 4, instantExecutor("out", 10))

	out, err := callTool(t, tools["subagent_spawn"], map[string]any{"brief": "research widgets"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	m := parseJSON(t, out)
	id, _ := m["id"].(string)
	if len(id) != 16 {
		t.Fatalf("id = %q, want 16 hex chars", id)
	}
	if m["status"] != "running" {
		t.Fatalf("status = %v, want running", m["status"])
	}
	if m["timeout_seconds"] != float64(300) {
		t.Fatalf("timeout_seconds = %v, want 300 (default)", m["timeout_seconds"])
	}
}

func TestSubagentSpawnTimeoutClamp(t *testing.T) {
	tools, _, _ := buildSubagentTools(t, 4, instantExecutor("out", 0))

	out, err := callTool(t, tools["subagent_spawn"], map[string]any{"brief": "a", "timeout_seconds": 5})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if got := parseJSON(t, out)["timeout_seconds"]; got != float64(10) {
		t.Fatalf("timeout_seconds = %v, want 10 (clamped up)", got)
	}

	out, err = callTool(t, tools["subagent_spawn"], map[string]any{"brief": "b", "timeout_seconds": 99999})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if got := parseJSON(t, out)["timeout_seconds"]; got != float64(3600) {
		t.Fatalf("timeout_seconds = %v, want 3600 (clamped down)", got)
	}

	out, err = callTool(t, tools["subagent_spawn"], map[string]any{"brief": "c", "timeout_seconds": 60})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if got := parseJSON(t, out)["timeout_seconds"]; got != float64(60) {
		t.Fatalf("timeout_seconds = %v, want 60 (in range, unchanged)", got)
	}
}

func TestSubagentSpawnValidation(t *testing.T) {
	tools, _, _ := buildSubagentTools(t, 4, instantExecutor("out", 0))
	spawn := tools["subagent_spawn"]

	for _, tc := range []struct {
		name string
		args string
	}{
		{"empty args", ""},
		{"empty object", "{}"},
		{"empty brief", `{"brief": ""}`},
		{"whitespace brief", `{"brief": "   "}`},
		{"malformed JSON", `{bad`},
		{"wrong type", `{"brief": 42}`},
	} {
		_, err := spawn.Execute(context.Background(), json.RawMessage(tc.args))
		if err == nil {
			t.Fatalf("%s: want error, got nil", tc.name)
		}
		if kind := subagentToolErrKind(t, err); kind != "invalid_args" {
			t.Fatalf("%s: kind = %q, want invalid_args (%v)", tc.name, kind, err)
		}
	}
}

func TestSubagentSpawnAtCapacity(t *testing.T) {
	release := make(chan struct{})
	tools, _, _ := buildSubagentTools(t, 1, blockingExecutor(release, "x", 0))
	defer close(release)

	if _, err := callTool(t, tools["subagent_spawn"], map[string]any{"brief": "first"}); err != nil {
		t.Fatalf("first spawn: %v", err)
	}
	_, err := callTool(t, tools["subagent_spawn"], map[string]any{"brief": "second"})
	if err == nil {
		t.Fatal("spawn at capacity: want error, got nil")
	}
	if kind := subagentToolErrKind(t, err); kind != "denied" {
		t.Fatalf("kind = %q, want denied (%v)", kind, err)
	}
	if !strings.Contains(err.Error(), "at capacity") {
		t.Fatalf("error = %q, want it to mention 'at capacity'", err.Error())
	}
}

func TestSubagentSpawnNoExecutor(t *testing.T) {
	mgr := subagent.NewManager(4)
	defer mgr.Shutdown()
	tools, err := NewSubagentTools(mgr, &fakeUsageSink{})
	if err != nil {
		t.Fatalf("NewSubagentTools: %v", err)
	}
	_, err = callTool(t, tools[0], map[string]any{"brief": "work"})
	if err == nil {
		t.Fatal("spawn without executor: want error, got nil")
	}
	if kind := subagentToolErrKind(t, err); kind != "execution" {
		t.Fatalf("kind = %q, want execution (%v)", kind, err)
	}
	if !strings.Contains(err.Error(), "no executor") {
		t.Fatalf("error = %q, want it to mention 'no executor'", err.Error())
	}
}

func TestSubagentList(t *testing.T) {
	release := make(chan struct{})
	tools, mgr, sink := buildSubagentTools(t, 4, blockingExecutor(release, "blocked-out", 0))

	// One running task (blocked) and one finished task (instant).
	// The finished task uses a second manager executor swap is not
	// possible per-task, so spawn the finished one first via a direct
	// instant manager instead: use mgr with the blocking executor for
	// the running task, and assert the list shows it; then finish it
	// and list again.
	out, err := callTool(t, tools["subagent_spawn"], map[string]any{"brief": strings.Repeat("long-brief-word ", 20)})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	runningID := parseJSON(t, out)["id"].(string)

	listOut, err := callTool(t, tools["subagent_list"], map[string]any{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var list struct {
		Tasks []struct {
			ID              string  `json:"id"`
			Status          string  `json:"status"`
			Brief           string  `json:"brief"`
			DurationSeconds float64 `json:"duration_seconds"`
		} `json:"tasks"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(listOut), &list); err != nil {
		t.Fatalf("list output is not the expected JSON: %v", err)
	}
	if list.Count != 1 || len(list.Tasks) != 1 {
		t.Fatalf("list count = %d, want 1", list.Count)
	}
	entry := list.Tasks[0]
	if entry.ID != runningID {
		t.Fatalf("list id = %q, want %q", entry.ID, runningID)
	}
	if entry.Status != "running" {
		t.Fatalf("list status = %q, want running", entry.Status)
	}
	// Brief is truncated to ~80 runes (original is 320 runes) and the
	// full output never appears in the list.
	if len([]rune(entry.Brief)) > 81 {
		t.Fatalf("list brief is %d runes, want at most ~80", len([]rune(entry.Brief)))
	}
	if strings.Contains(listOut, "blocked-out") {
		t.Fatal("list must never return task outputs")
	}
	if entry.DurationSeconds < 0 {
		t.Fatalf("duration_seconds = %v, want >= 0", entry.DurationSeconds)
	}
	// Listing never counts usage.
	if total, calls := sink.snapshot(); total != 0 || calls != 0 {
		t.Fatalf("sink after list: total=%d calls=%d, want 0/0", total, calls)
	}

	// Finish the task; the list then shows it done with a final duration.
	close(release)
	if _, err := mgr.Wait(context.Background(), runningID); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	listOut, err = callTool(t, tools["subagent_list"], map[string]any{})
	if err != nil {
		t.Fatalf("list after finish: %v", err)
	}
	if err := json.Unmarshal([]byte(listOut), &list); err != nil {
		t.Fatalf("list output is not the expected JSON: %v", err)
	}
	if list.Tasks[0].Status != "done" {
		t.Fatalf("list status after finish = %q, want done", list.Tasks[0].Status)
	}
	if total, calls := sink.snapshot(); total != 0 || calls != 0 {
		t.Fatalf("sink after list of finished task: total=%d calls=%d, want 0/0", total, calls)
	}
}

func TestSubagentListEmpty(t *testing.T) {
	tools, _, _ := buildSubagentTools(t, 4, instantExecutor("out", 0))
	for _, raw := range []string{"", "null", "{}"} {
		out, err := tools["subagent_list"].Execute(context.Background(), json.RawMessage(raw))
		if err != nil {
			t.Fatalf("list(%q): %v", raw, err)
		}
		m := parseJSON(t, out)
		if m["count"] != float64(0) {
			t.Fatalf("list(%q): count = %v, want 0", raw, m["count"])
		}
	}
}

func TestSubagentResultRunningIsNonBlocking(t *testing.T) {
	release := make(chan struct{})
	tools, _, sink := buildSubagentTools(t, 4, blockingExecutor(release, "later", 77))
	defer close(release)

	out, err := callTool(t, tools["subagent_spawn"], map[string]any{"brief": "slow work"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	id := parseJSON(t, out)["id"].(string)

	// Must return promptly with running status, not block for the task.
	start := time.Now()
	resOut, err := callTool(t, tools["subagent_result"], map[string]any{"id": id})
	if err != nil {
		t.Fatalf("result while running: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("result on a running task blocked for %v, want immediate return", elapsed)
	}
	m := parseJSON(t, resOut)
	if m["status"] != "running" {
		t.Fatalf("status = %v, want running", m["status"])
	}
	if _, ok := m["elapsed_seconds"]; !ok {
		t.Fatal("running result must include elapsed_seconds")
	}
	if _, ok := m["output"]; ok {
		t.Fatal("running result must not include output yet")
	}
	if total, calls := sink.snapshot(); total != 0 || calls != 0 {
		t.Fatalf("sink after running fetch: total=%d calls=%d, want 0/0", total, calls)
	}
}

func TestSubagentResultUnknownAndEmptyID(t *testing.T) {
	tools, _, _ := buildSubagentTools(t, 4, instantExecutor("out", 0))
	result := tools["subagent_result"]

	_, err := callTool(t, result, map[string]any{"id": "no-such-id"})
	if err == nil {
		t.Fatal("result unknown id: want error, got nil")
	}
	if kind := subagentToolErrKind(t, err); kind != "invalid_args" {
		t.Fatalf("unknown id kind = %q, want invalid_args (%v)", kind, err)
	}

	_, err = callTool(t, result, map[string]any{"id": "  "})
	if err == nil {
		t.Fatal("result empty id: want error, got nil")
	}
	if kind := subagentToolErrKind(t, err); kind != "invalid_args" {
		t.Fatalf("empty id kind = %q, want invalid_args (%v)", kind, err)
	}
}

// TestSubagentResultEndToEnd is the fake-executor end-to-end: spawn
// with an executor that returns output + usage, fetch the result,
// assert output + tokens, and assert the sink received the tokens
// exactly once across two fetches.
func TestSubagentResultEndToEnd(t *testing.T) {
	tools, mgr, sink := buildSubagentTools(t, 4, instantExecutor("research complete: 42 widgets", 123))

	out, err := callTool(t, tools["subagent_spawn"], map[string]any{"brief": "count widgets"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	id := parseJSON(t, out)["id"].(string)
	if _, err := mgr.Wait(context.Background(), id); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	resOut, err := callTool(t, tools["subagent_result"], map[string]any{"id": id})
	if err != nil {
		t.Fatalf("first result fetch: %v", err)
	}
	m := parseJSON(t, resOut)
	if m["status"] != "done" {
		t.Fatalf("status = %v, want done", m["status"])
	}
	if m["output"] != "research complete: 42 widgets" {
		t.Fatalf("output = %v, want the executor output", m["output"])
	}
	if m["tokens"] != float64(123) {
		t.Fatalf("tokens = %v, want 123", m["tokens"])
	}
	if _, ok := m["duration_seconds"]; !ok {
		t.Fatal("finished result must include duration_seconds")
	}
	if total, calls := sink.snapshot(); total != 123 || calls != 1 {
		t.Fatalf("sink after first fetch: total=%d calls=%d, want 123/1", total, calls)
	}

	// Second fetch returns the same result but must not double-count.
	resOut2, err := callTool(t, tools["subagent_result"], map[string]any{"id": id})
	if err != nil {
		t.Fatalf("second result fetch: %v", err)
	}
	if parseJSON(t, resOut2)["output"] != "research complete: 42 widgets" {
		t.Fatal("second fetch must return the same output")
	}
	if total, calls := sink.snapshot(); total != 123 || calls != 1 {
		t.Fatalf("sink after second fetch: total=%d calls=%d, want 123/1 (exactly once)", total, calls)
	}
}

func TestSubagentResultFailedTaskCharged(t *testing.T) {
	boom := errors.New("executor boom")
	exec := func(ctx context.Context, brief string) (string, subagent.Usage, error) {
		return "", subagent.Usage{Tokens: 50}, boom
	}
	tools, mgr, sink := buildSubagentTools(t, 4, exec)

	out, err := callTool(t, tools["subagent_spawn"], map[string]any{"brief": "fail please"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	id := parseJSON(t, out)["id"].(string)
	if _, err := mgr.Wait(context.Background(), id); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	resOut, err := callTool(t, tools["subagent_result"], map[string]any{"id": id})
	if err != nil {
		t.Fatalf("result fetch: %v", err)
	}
	m := parseJSON(t, resOut)
	if m["status"] != "failed" {
		t.Fatalf("status = %v, want failed", m["status"])
	}
	if m["tokens"] != float64(50) {
		t.Fatalf("tokens = %v, want 50 reported", m["tokens"])
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "executor boom") {
		t.Fatalf("error = %q, want it to mention 'executor boom'", errStr)
	}
	// Failed tasks count too: their tokens were genuinely spent and
	// the parent is told about them, so hiding them would undercount
	// the turn. Fetching again must not double-count.
	if total, calls := sink.snapshot(); total != 50 || calls != 1 {
		t.Fatalf("sink after failed fetch: total=%d calls=%d, want 50/1", total, calls)
	}
	if _, err := callTool(t, tools["subagent_result"], map[string]any{"id": id}); err != nil {
		t.Fatalf("second result fetch: %v", err)
	}
	if total, calls := sink.snapshot(); total != 50 || calls != 1 {
		t.Fatalf("sink after second fetch: total=%d calls=%d, want 50/1", total, calls)
	}
}

func TestUsageSinkFuncResolvesCurrentTarget(t *testing.T) {
	// A UsageSinkFunc resolves its target on every call, so tokens
	// accrue to whichever sink is current (e.g. the live agent after
	// a provider switch) rather than the one the tools were built with.
	sinkA := &fakeUsageSink{}
	sinkB := &fakeUsageSink{}
	current := sinkA
	sink := UsageSinkFunc(func(n int) { current.AddSubagentUsage(n) })
	mgr := subagent.NewManagerWithExecutor(4, instantExecutor("done", 42))
	t.Cleanup(mgr.Shutdown)
	tools, err := NewSubagentTools(mgr, sink)
	if err != nil {
		t.Fatalf("NewSubagentTools: %v", err)
	}
	byName := make(map[string]Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	fetchOnce := func() string {
		out, err := callTool(t, byName["subagent_spawn"], map[string]any{"brief": "work"})
		if err != nil {
			t.Fatalf("spawn: %v", err)
		}
		id := parseJSON(t, out)["id"].(string)
		if _, err := mgr.Wait(context.Background(), id); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		resOut, err := callTool(t, byName["subagent_result"], map[string]any{"id": id})
		if err != nil {
			t.Fatalf("result fetch: %v", err)
		}
		return resOut
	}
	fetchOnce()
	if total, _ := sinkA.snapshot(); total != 42 {
		t.Fatalf("sinkA after first fetch: total=%d, want 42", total)
	}
	current = sinkB
	fetchOnce()
	if total, _ := sinkA.snapshot(); total != 42 {
		t.Fatalf("sinkA after switch: total=%d, want 42 (unchanged)", total)
	}
	if total, calls := sinkB.snapshot(); total != 42 || calls != 1 {
		t.Fatalf("sinkB after second fetch: total=%d calls=%d, want 42/1", total, calls)
	}
}

func TestSubagentStop(t *testing.T) {
	release := make(chan struct{})
	tools, mgr, _ := buildSubagentTools(t, 4, blockingExecutor(release, "x", 0))
	defer close(release)

	out, err := callTool(t, tools["subagent_spawn"], map[string]any{"brief": "to stop"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	id := parseJSON(t, out)["id"].(string)

	stopOut, err := callTool(t, tools["subagent_stop"], map[string]any{"id": id})
	if err != nil {
		t.Fatalf("stop running: %v", err)
	}
	m := parseJSON(t, stopOut)
	if m["cancelled"] != true {
		t.Fatalf("cancelled = %v, want true", m["cancelled"])
	}
	if _, err := mgr.Wait(context.Background(), id); err != nil {
		t.Fatalf("Wait after stop: %v", err)
	}

	// Already finished: not cancelled.
	stopOut, err = callTool(t, tools["subagent_stop"], map[string]any{"id": id})
	if err != nil {
		t.Fatalf("stop finished: %v", err)
	}
	m = parseJSON(t, stopOut)
	if m["cancelled"] != false {
		t.Fatalf("cancelled = %v, want false for a finished task", m["cancelled"])
	}
	if m["status"] != "failed" {
		t.Fatalf("status = %v, want failed (cancelled task)", m["status"])
	}

	// Unknown id: not cancelled, status unknown.
	stopOut, err = callTool(t, tools["subagent_stop"], map[string]any{"id": "no-such-id"})
	if err != nil {
		t.Fatalf("stop unknown: %v", err)
	}
	m = parseJSON(t, stopOut)
	if m["cancelled"] != false {
		t.Fatalf("cancelled = %v, want false for an unknown task", m["cancelled"])
	}
	if m["status"] != "unknown" {
		t.Fatalf("status = %v, want unknown", m["status"])
	}

	// Empty id is a validation error.
	_, err = callTool(t, tools["subagent_stop"], map[string]any{"id": ""})
	if err == nil {
		t.Fatal("stop empty id: want error, got nil")
	}
	if kind := subagentToolErrKind(t, err); kind != "invalid_args" {
		t.Fatalf("empty id kind = %q, want invalid_args (%v)", kind, err)
	}
}
