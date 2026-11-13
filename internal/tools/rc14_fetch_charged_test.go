package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestSubagentFetchMarksChargedForEviction (F-A9-P2-8 follow-up)
// proves the fetch path marks a charged task in the Manager:
// a task whose result was fetched (charged via the sink — the
// CLI path's only charger, it has no sweep) and later evicted
// contributes 0 tokens to the dead-letter ledger. Without the
// MarkCharged call at the fetch charge site, the fetched task's
// tokens would be folded into the ledger on top of the fetch
// charge — the same double-count the sweep path already closes.
func TestSubagentFetchMarksChargedForEviction(t *testing.T) {
	run := func(t *testing.T, fetched bool) (tasks, tokens int) {
		t.Helper()
		tools, mgr, sink := buildSubagentTools(t, 256, instantExecutor("done", 100))
		out, err := callTool(t, tools["subagent_spawn"], map[string]any{"brief": "target"})
		if err != nil {
			t.Fatalf("spawn: %v", err)
		}
		id := parseJSON(t, out)["id"].(string)
		if _, err := mgr.Wait(context.Background(), id); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if fetched {
			if _, err := callTool(t, tools["subagent_result"], map[string]any{"id": id}); err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if total, calls := sink.snapshot(); total != 100 || calls != 1 {
				t.Fatalf("sink after fetch: total=%d calls=%d, want 100/1", total, calls)
			}
		}
		// Flood past the cap (subagent maxEntries = 1000): the
		// target is the oldest settled entry, so it is the
		// first eviction. Spawns retry on "at capacity": 256
		// instant tasks can fill the semaphore faster than
		// they drain when spawned in a tight loop.
		deadline := time.Now().Add(60 * time.Second)
		for i := 0; i < 1000; i++ {
			for {
				if _, err := mgr.Spawn(fmt.Sprintf("filler-%d", i), 0); err == nil {
					break
				} else if !strings.Contains(err.Error(), "at capacity") {
					t.Fatalf("spawn filler %d: %v", i, err)
				} else if time.Now().After(deadline) {
					t.Fatalf("spawn filler %d still at capacity after 60s: %v", i, err)
				}
				time.Sleep(time.Millisecond)
			}
		}
		mgr.WaitAll(context.Background())
		return mgr.DeadLetterTasks(), mgr.DeadLetterTokens()
	}

	t.Run("fetched", func(t *testing.T) {
		tasks, tokens := run(t, true)
		if tasks != 1 {
			t.Fatalf("DeadLetterTasks() = %d, want 1 (the fetched target evicted)", tasks)
		}
		if tokens != 0 {
			t.Errorf("DeadLetterTokens() = %d, want 0 (the fetched task was already charged)", tokens)
		}
	})
	t.Run("unfetched", func(t *testing.T) {
		tasks, tokens := run(t, false)
		if tasks != 1 {
			t.Fatalf("DeadLetterTasks() = %d, want 1", tasks)
		}
		if tokens != 100 {
			t.Errorf("DeadLetterTokens() = %d, want 100 (the never-charged target folds into the ledger)", tokens)
		}
	})
}
