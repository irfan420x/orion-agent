package subagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// sleepExecutor returns an executor that waits for d (respecting ctx)
// and then returns output, or ctx.Err() if the context ends first.
func sleepExecutor(d time.Duration, output string) Executor {
	return func(ctx context.Context, brief string) (string, Usage, error) {
		select {
		case <-time.After(d):
			return output + ":" + brief, Usage{}, nil
		case <-ctx.Done():
			return "", Usage{}, ctx.Err()
		}
	}
}

// waitForStatus polls until the task reaches want or the deadline hits.
func waitForStatus(t *testing.T, m *Manager, id string, want Status, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st, ok := m.Status(id); ok && st == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	st, _ := m.Status(id)
	t.Fatalf("task %s: status = %q, want %q", id, st, want)
}

func TestNewManagerDefaults(t *testing.T) {
	m := NewManager(0)
	if m.maxConcurrent != DefaultMaxConcurrent {
		t.Fatalf("maxConcurrent = %d, want %d", m.maxConcurrent, DefaultMaxConcurrent)
	}
	m = NewManager(-3)
	if m.maxConcurrent != DefaultMaxConcurrent {
		t.Fatalf("maxConcurrent = %d, want %d", m.maxConcurrent, DefaultMaxConcurrent)
	}
}

func TestSpawnAndWaitReturnsOutput(t *testing.T) {
	m := NewManagerWithExecutor(4, sleepExecutor(10*time.Millisecond, "done"))
	defer m.Shutdown()

	id, err := m.Spawn("hello", time.Minute)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if id == "" {
		t.Fatal("Spawn returned empty id")
	}
	if len(id) != 16 {
		t.Fatalf("id = %q (%d chars), want 16 hex chars", id, len(id))
	}

	res, err := m.Wait(context.Background(), id)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.TaskID != id {
		t.Fatalf("TaskID = %q, want %q", res.TaskID, id)
	}
	if res.Output != "done:hello" {
		t.Fatalf("Output = %q, want %q", res.Output, "done:hello")
	}
	if res.Err != nil {
		t.Fatalf("Err = %v, want nil", res.Err)
	}
	if res.Duration <= 0 || res.FinishedAt.IsZero() {
		t.Fatalf("Duration = %v, FinishedAt = %v, want both set", res.Duration, res.FinishedAt)
	}

	st, ok := m.Status(id)
	if !ok || st != StatusDone {
		t.Fatalf("Status = %q, %v, want %q, true", st, ok, StatusDone)
	}
	got, ok := m.GetResult(id)
	if !ok || got.Output != "done:hello" {
		t.Fatalf("GetResult = %+v, %v, want output done:hello", got, ok)
	}
}

func TestSpawnEmptyBrief(t *testing.T) {
	m := NewManagerWithExecutor(4, sleepExecutor(time.Millisecond, "x"))
	defer m.Shutdown()
	for _, brief := range []string{"", "   ", "\n\t"} {
		if _, err := m.Spawn(brief, time.Minute); err == nil {
			t.Fatalf("Spawn(%q): want error, got nil", brief)
		}
	}
}

func TestSpawnNoExecutor(t *testing.T) {
	m := NewManager(4)
	defer m.Shutdown()
	if _, err := m.Spawn("hi", time.Minute); err == nil {
		t.Fatal("Spawn without executor: want error, got nil")
	}
}

func TestWaitUnknownTask(t *testing.T) {
	m := NewManagerWithExecutor(4, sleepExecutor(time.Millisecond, "x"))
	defer m.Shutdown()
	if _, err := m.Wait(context.Background(), "no-such-id"); err == nil {
		t.Fatal("Wait unknown id: want error, got nil")
	}
	if _, ok := m.Status("no-such-id"); ok {
		t.Fatal("Status unknown id: want false")
	}
	if _, ok := m.GetResult("no-such-id"); ok {
		t.Fatal("GetResult unknown id: want false")
	}
	if m.Cancel("no-such-id") {
		t.Fatal("Cancel unknown id: want false")
	}
}

func TestTimeoutTask(t *testing.T) {
	// Executor would take 500ms; the task timeout is 20ms.
	m := NewManagerWithExecutor(4, sleepExecutor(500*time.Millisecond, "late"))
	defer m.Shutdown()

	id, err := m.Spawn("slow", 20*time.Millisecond)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	start := time.Now()
	res, err := m.Wait(context.Background(), id)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Wait took %v, want prompt timeout (~20ms)", elapsed)
	}
	if res.Err == nil {
		t.Fatal("timeout task: Err = nil, want non-nil")
	}
	if !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Fatalf("Err = %v, want context.DeadlineExceeded", res.Err)
	}
	st, _ := m.Status(id)
	if st != StatusTimeout {
		t.Fatalf("Status = %q, want %q", st, StatusTimeout)
	}
}

func TestTimeoutWithUncooperativeExecutor(t *testing.T) {
	// Executor ignores ctx entirely; the Manager must still time out.
	m := NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, Usage, error) {
		time.Sleep(500 * time.Millisecond)
		return "too late", Usage{}, nil
	})
	defer m.Shutdown()

	id, err := m.Spawn("stubborn", 20*time.Millisecond)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	res, err := m.Wait(context.Background(), id)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Fatalf("Err = %v, want context.DeadlineExceeded", res.Err)
	}
	if st, _ := m.Status(id); st != StatusTimeout {
		t.Fatalf("Status = %q, want %q", st, StatusTimeout)
	}
}

func TestFailingExecutor(t *testing.T) {
	boom := errors.New("boom")
	m := NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, Usage, error) {
		return "", Usage{}, boom
	})
	defer m.Shutdown()

	id, err := m.Spawn("fail", time.Minute)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	res, err := m.Wait(context.Background(), id)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !errors.Is(res.Err, boom) {
		t.Fatalf("Err = %v, want %v", res.Err, boom)
	}
	if st, _ := m.Status(id); st != StatusFailed {
		t.Fatalf("Status = %q, want %q", st, StatusFailed)
	}
}

func TestAtCapacity(t *testing.T) {
	release := make(chan struct{})
	blocking := func(ctx context.Context, brief string) (string, Usage, error) {
		select {
		case <-release:
			return "released", Usage{}, nil
		case <-ctx.Done():
			return "", Usage{}, ctx.Err()
		}
	}
	m := NewManagerWithExecutor(1, blocking)
	defer m.Shutdown()

	id, err := m.Spawn("first", time.Minute)
	if err != nil {
		t.Fatalf("Spawn first: %v", err)
	}
	waitForStatus(t, m, id, StatusRunning, time.Second)

	// Second spawn must fail fast, not block, while the slot is held.
	start := time.Now()
	_, err = m.Spawn("second", time.Minute)
	if err == nil {
		t.Fatal("Spawn at capacity: want error, got nil")
	}
	if !strings.Contains(err.Error(), "at capacity") {
		t.Fatalf("error = %q, want it to mention 'at capacity'", err.Error())
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Spawn at capacity blocked for %v, want immediate error", elapsed)
	}

	// Freeing the slot lets a new spawn succeed.
	close(release)
	if _, err := m.Wait(context.Background(), id); err != nil {
		t.Fatalf("Wait first: %v", err)
	}
	if _, err := m.Spawn("third", time.Minute); err != nil {
		t.Fatalf("Spawn after slot freed: %v", err)
	}
}

func TestCancelStopsRunningTask(t *testing.T) {
	m := NewManagerWithExecutor(4, sleepExecutor(time.Minute, "never"))
	defer m.Shutdown()

	id, err := m.Spawn("long", time.Minute)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, m, id, StatusRunning, time.Second)

	if !m.Cancel(id) {
		t.Fatal("Cancel running task: want true")
	}
	res, err := m.Wait(context.Background(), id)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("Err = %v, want context.Canceled", res.Err)
	}
	if st, _ := m.Status(id); st != StatusFailed {
		t.Fatalf("Status = %q, want %q", st, StatusFailed)
	}
	// Cancelling a finished task is a no-op.
	if m.Cancel(id) {
		t.Fatal("Cancel finished task: want false")
	}
}

func TestCancelUncooperativeExecutor(t *testing.T) {
	// Executor ignores ctx; Cancel must still end the task promptly.
	m := NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, Usage, error) {
		time.Sleep(30 * time.Second)
		return "too late", Usage{}, nil
	})
	defer m.Shutdown()

	id, err := m.Spawn("stubborn", time.Minute)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, m, id, StatusRunning, time.Second)
	if !m.Cancel(id) {
		t.Fatal("Cancel: want true")
	}
	res, err := m.Wait(context.Background(), id)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("Err = %v, want context.Canceled", res.Err)
	}
}

func TestWaitAllCollectsMultiple(t *testing.T) {
	m := NewManagerWithExecutor(4, sleepExecutor(10*time.Millisecond, "ok"))
	defer m.Shutdown()

	ids := make([]string, 0, 3)
	for _, brief := range []string{"a", "b", "c"} {
		id, err := m.Spawn(brief, time.Minute)
		if err != nil {
			t.Fatalf("Spawn %q: %v", brief, err)
		}
		ids = append(ids, id)
	}
	results := m.WaitAll(context.Background())
	if len(results) != 3 {
		t.Fatalf("WaitAll returned %d results, want 3", len(results))
	}
	seen := map[string]bool{}
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("result %+v: unexpected Err %v", r, r.Err)
		}
		seen[r.TaskID] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Fatalf("WaitAll missing task %s", id)
		}
	}
}

func TestWaitAllEmpty(t *testing.T) {
	m := NewManagerWithExecutor(4, sleepExecutor(time.Millisecond, "x"))
	defer m.Shutdown()
	if got := m.WaitAll(context.Background()); got == nil || len(got) != 0 {
		t.Fatalf("WaitAll empty = %v, want empty non-nil slice", got)
	}
}

func TestWaitContextCancelled(t *testing.T) {
	m := NewManagerWithExecutor(4, sleepExecutor(time.Minute, "never"))
	defer m.Shutdown()
	id, err := m.Spawn("long", time.Minute)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := m.Wait(ctx, id); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait with expired ctx: err = %v, want context.DeadlineExceeded", err)
	}
	// The task itself is untouched by the waiter's ctx expiring.
	if st, _ := m.Status(id); st != StatusRunning {
		t.Fatalf("Status after waiter timeout = %q, want %q", st, StatusRunning)
	}
	m.Cancel(id)
}

func TestListReflectsStatuses(t *testing.T) {
	m := NewManagerWithExecutor(4, sleepExecutor(10*time.Millisecond, "ok"))
	defer m.Shutdown()

	id1, err := m.Spawn("first", time.Minute)
	if err != nil {
		t.Fatalf("Spawn first: %v", err)
	}
	id2, err := m.Spawn("second", time.Minute)
	if err != nil {
		t.Fatalf("Spawn second: %v", err)
	}
	list := m.List()
	if len(list) != 2 {
		t.Fatalf("List = %d entries, want 2", len(list))
	}
	byID := map[string]TaskStatus{}
	for _, ts := range list {
		byID[ts.ID] = ts
		if ts.StartedAt.IsZero() {
			t.Fatalf("List entry %s: StartedAt is zero", ts.ID)
		}
	}
	if byID[id1].Brief != "first" || byID[id2].Brief != "second" {
		t.Fatalf("List briefs = %q, %q, want first, second", byID[id1].Brief, byID[id2].Brief)
	}

	// After both finish, List shows them done.
	m.WaitAll(context.Background())
	for _, ts := range m.List() {
		if ts.Status != StatusDone {
			t.Fatalf("List after finish: task %s status = %q, want %q", ts.ID, ts.Status, StatusDone)
		}
	}

	// GetResult is false while a task is still running.
	m2 := NewManagerWithExecutor(4, sleepExecutor(200*time.Millisecond, "slow"))
	defer m2.Shutdown()
	id3, err := m2.Spawn("running", time.Minute)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if _, ok := m2.GetResult(id3); ok {
		t.Fatal("GetResult running task: want false")
	}
	m2.Cancel(id3)
}

func TestShutdownCancelsRunningTasks(t *testing.T) {
	m := NewManagerWithExecutor(4, sleepExecutor(time.Minute, "never"))
	id, err := m.Spawn("long", time.Minute)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, m, id, StatusRunning, time.Second)

	start := time.Now()
	m.Shutdown()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Shutdown took %v, want prompt return", elapsed)
	}
	if st, _ := m.Status(id); st != StatusFailed {
		t.Fatalf("Status after Shutdown = %q, want %q", st, StatusFailed)
	}
	if res, ok := m.GetResult(id); !ok || !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("GetResult after Shutdown = %+v, %v, want context.Canceled", res, ok)
	}
	// Spawn after shutdown fails.
	if _, err := m.Spawn("after", time.Minute); err == nil {
		t.Fatal("Spawn after Shutdown: want error, got nil")
	}
	// Shutdown is idempotent.
	m.Shutdown()
}

func TestExecutorPanicBecomesFailure(t *testing.T) {
	m := NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, Usage, error) {
		panic("boom")
	})
	defer m.Shutdown()
	id, err := m.Spawn("panicky", time.Minute)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	res, err := m.Wait(context.Background(), id)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Err == nil {
		t.Fatal("panicking executor: Err = nil, want non-nil")
	}
	if st, _ := m.Status(id); st != StatusFailed {
		t.Fatalf("Status = %q, want %q", st, StatusFailed)
	}
}

func TestSetExecutor(t *testing.T) {
	m := NewManager(4)
	defer m.Shutdown()
	m.SetExecutor(sleepExecutor(time.Millisecond, "swapped"))
	id, err := m.Spawn("hi", time.Minute)
	if err != nil {
		t.Fatalf("Spawn after SetExecutor: %v", err)
	}
	res, err := m.Wait(context.Background(), id)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Output != "swapped:hi" {
		t.Fatalf("Output = %q, want %q", res.Output, "swapped:hi")
	}
}

// TestResultRecordsUsage: an executor that reports usage must have it
// recorded on the Result, so the tools layer can charge it when the
// parent consumes the result.
func TestResultRecordsUsage(t *testing.T) {
	m := NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, Usage, error) {
		return "used:" + brief, Usage{Tokens: 42}, nil
	})
	defer m.Shutdown()
	id, err := m.Spawn("work", time.Minute)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	res, err := m.Wait(context.Background(), id)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Usage.Tokens != 42 {
		t.Fatalf("Usage.Tokens = %d, want 42", res.Usage.Tokens)
	}
	if got, ok := m.GetResult(id); !ok || got.Usage.Tokens != 42 {
		t.Fatalf("GetResult usage = %+v, %v, want 42 tokens", got, ok)
	}
}

// TestResultUsageZeroWhenExecutorNeverCompletes: a task that times out
// reports zero usage to the waiter — Wait returns promptly, before
// the executor's late return. (F-A7-2a: the late usage IS recorded
// on the entry afterwards, when the executor eventually returns;
// see TestLateUsageFromIgnoringExecutorIsCharged. What stays zero
// here is only the prompt timeout result, never a later GetResult.)
func TestResultUsageZeroWhenExecutorNeverCompletes(t *testing.T) {
	m := NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, Usage, error) {
		time.Sleep(500 * time.Millisecond)
		return "late", Usage{Tokens: 99}, nil
	})
	defer m.Shutdown()
	id, err := m.Spawn("slow", 20*time.Millisecond)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	res, err := m.Wait(context.Background(), id)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Usage.Tokens != 0 {
		t.Fatalf("Usage.Tokens = %d, want 0 (late result discarded)", res.Usage.Tokens)
	}
}

// TestConcurrentSpawnAndCollect spawns many tasks from many goroutines
// at once and collects every result. Under -race any unsynchronised
// access to the Manager's registry, semaphore, or task entries fails.
func TestConcurrentSpawnAndCollect(t *testing.T) {
	m := NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, Usage, error) {
		// Vary the delay by brief so tasks finish out of order.
		time.Sleep(time.Duration(len(brief)%5+1) * time.Millisecond)
		return "out:" + brief, Usage{}, nil
	})
	defer m.Shutdown()

	const goroutines = 8
	const perGoroutine = 5
	ids := make(chan string, goroutines*perGoroutine)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				brief := fmt.Sprintf("g%d-t%d", g, i)
				// At capacity is possible under contention; retry
				// briefly rather than failing the test for it.
				var id string
				var err error
				for attempt := 0; attempt < 200; attempt++ {
					id, err = m.Spawn(brief, time.Minute)
					if err == nil {
						break
					}
					if !strings.Contains(err.Error(), "at capacity") {
						t.Errorf("Spawn(%q): unexpected error %v", brief, err)
						return
					}
					time.Sleep(time.Millisecond)
				}
				if err != nil {
					t.Errorf("Spawn(%q): never got a slot: %v", brief, err)
					return
				}
				ids <- id
			}
		}(g)
	}
	wg.Wait()
	close(ids)

	want := map[string]bool{}
	count := 0
	for id := range ids {
		want[id] = true
		count++
	}
	if count != goroutines*perGoroutine {
		t.Fatalf("spawned %d tasks, want %d", count, goroutines*perGoroutine)
	}

	results := m.WaitAll(context.Background())
	if len(results) != count {
		t.Fatalf("WaitAll returned %d results, want %d", len(results), count)
	}
	seen := map[string]bool{}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("task %s: unexpected Err %v", r.TaskID, r.Err)
		}
		if !strings.HasPrefix(r.Output, "out:") {
			t.Errorf("task %s: Output %q, want prefix %q", r.TaskID, r.Output, "out:")
		}
		if !want[r.TaskID] {
			t.Errorf("WaitAll returned unknown task %s", r.TaskID)
		}
		if seen[r.TaskID] {
			t.Errorf("WaitAll returned task %s twice", r.TaskID)
		}
		seen[r.TaskID] = true
		if st, ok := m.Status(r.TaskID); !ok || st != StatusDone {
			t.Errorf("task %s: Status = %q, %v, want %q, true", r.TaskID, st, ok, StatusDone)
		}
	}
	if len(seen) != count {
		t.Fatalf("collected %d distinct results, want %d", len(seen), count)
	}
}

// TestConcurrentObservers hammers the read paths (Status, List,
// GetResult) and Cancel from many goroutines while tasks are running.
// The registry must stay consistent and -race must stay silent.
func TestConcurrentObservers(t *testing.T) {
	release := make(chan struct{})
	m := NewManagerWithExecutor(4, func(ctx context.Context, brief string) (string, Usage, error) {
		select {
		case <-release:
			return "released:" + brief, Usage{}, nil
		case <-ctx.Done():
			return "", Usage{}, ctx.Err()
		}
	})
	defer m.Shutdown()

	ids := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		id, err := m.Spawn(fmt.Sprintf("task-%d", i), time.Minute)
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		ids = append(ids, id)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, id := range ids {
					if _, ok := m.Status(id); !ok {
						t.Errorf("worker %d: Status(%s) unknown", w, id)
						return
					}
					m.GetResult(id)
				}
				if list := m.List(); len(list) != 4 {
					t.Errorf("worker %d: List() = %d entries, want 4", w, len(list))
					return
				}
				// One worker also cancels tasks it finds still running;
				// the rest only observe.
				if w == 0 {
					m.Cancel(ids[0])
				}
			}
		}(w)
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	close(stop)
	wg.Wait()

	results := m.WaitAll(context.Background())
	if len(results) != 4 {
		t.Fatalf("WaitAll returned %d results, want 4", len(results))
	}
	// task-0 may have been cancelled (failed) or finished (done);
	// the other three must be done.
	for _, r := range results {
		if r.TaskID == ids[0] {
			continue
		}
		if r.Err != nil {
			t.Errorf("task %s: unexpected Err %v", r.TaskID, r.Err)
		}
	}
}

// TestConcurrentShutdown calls Shutdown from several goroutines at
// once while tasks run. It must be idempotent, return promptly, and
// leave the Manager shut down afterwards.
func TestConcurrentShutdown(t *testing.T) {
	m := NewManagerWithExecutor(4, sleepExecutor(50*time.Millisecond, "ok"))
	id, err := m.Spawn("work", time.Minute)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Shutdown()
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Shutdown did not return within 5s")
	}

	if _, err := m.Spawn("after", time.Minute); err == nil {
		t.Fatal("Spawn after concurrent Shutdown: want error, got nil")
	}
	// The task either finished or was cancelled by shutdown; either
	// way it must have a recorded outcome.
	if _, ok := m.GetResult(id); !ok {
		t.Fatal("task has no recorded result after Shutdown")
	}
}
