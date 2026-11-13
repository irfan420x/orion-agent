// rc13 W3 regression tests for Audit 8 finding F-A8-P2-2 (the fetch
// path never charges a timed-out task's late usage; on the CLI path,
// which has no sweep, those tokens vanish).
//
// Negative control: these tests were written FIRST and run against
// the unmodified rc12 code, where they FAIL (see the report for the
// fail-before output). Only then was the fix implemented.
package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/irfan/orion/internal/subagent"
)

// rc13w3LateExecutor ignores ctx, blocks on release, then returns
// 700 tokens — the Audit 8 W4 probe shape (timed-out task settling
// with 700 late tokens).
func rc13w3LateExecutor(release chan struct{}) subagent.Executor {
	return func(ctx context.Context, brief string) (string, subagent.Usage, error) {
		<-release
		return "late", subagent.Usage{Tokens: 700}, nil
	}
}

// rc13w3FetchResult fetches a subagent_result payload and returns
// its decoded "tokens" field.
func rc13w3FetchResult(t *testing.T, tools map[string]Tool, id string) (map[string]any, error) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"id": id})
	if err != nil {
		t.Fatalf("marshal fetch args: %v", err)
	}
	out, err := tools["subagent_result"].Execute(context.Background(), raw)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("fetch output %q is not JSON: %v", out, err)
	}
	return m, nil
}

// rc13w3WaitSettled polls GetResult until the task's usage is
// settled with want tokens, or fails after the deadline.
func rc13w3WaitSettled(t *testing.T, mgr *subagent.Manager, id string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, ok := mgr.GetResult(id)
		if ok && got.UsageSettled && got.Usage.Tokens == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s never settled with %d tokens: GetResult = %+v, ok=%v", id, want, got, ok)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestRC13W3FetchChargesTimedOutLateUsage is the Audit 8 W4 probe
// (TestAudit8W4FetchNeverChargesTimedOutLateUsage) inverted into a
// positive test: a timed-out task settles with 700 late tokens; the
// fetch payload reports "tokens": 700, and the usage sink must
// receive those 700 — on the CLI path there is no sweep to save
// them, so a fetch that does not charge them loses them. On
// unmodified rc12 code the payload reports 700 but the sink
// receives 0.
func TestRC13W3FetchChargesTimedOutLateUsage(t *testing.T) {
	release := make(chan struct{})
	tools, mgr, sink := buildSubagentTools(t, 4, rc13w3LateExecutor(release))

	// Spawn directly on the Manager: the spawn tool clamps timeouts
	// to >= 10s, but the probe needs a 50 ms timeout.
	id, err := mgr.Spawn("slow", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if _, err := mgr.Wait(context.Background(), id); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if st, _ := mgr.Status(id); st != subagent.StatusTimeout {
		t.Fatalf("Status = %q, want %q", st, subagent.StatusTimeout)
	}

	// Fetch while the usage is still unsettled: nothing to charge
	// yet, and the fetch must NOT consume the single charge — the
	// late 700 must still be chargeable once it arrives.
	m, err := rc13w3FetchResult(t, tools, id)
	if err != nil {
		t.Fatalf("fetch while unsettled: %v", err)
	}
	if m["tokens"] != float64(0) {
		t.Fatalf("tokens while unsettled = %v, want 0 (provisional)", m["tokens"])
	}
	if total, _ := sink.snapshot(); total != 0 {
		t.Fatalf("sink after unsettled fetch: total=%d, want 0", total)
	}

	// Let the executor return its 700 late tokens, then fetch.
	close(release)
	rc13w3WaitSettled(t, mgr, id, 700)
	m, err = rc13w3FetchResult(t, tools, id)
	if err != nil {
		t.Fatalf("fetch after late return: %v", err)
	}
	if m["tokens"] != float64(700) {
		t.Fatalf("tokens after late return = %v, want 700", m["tokens"])
	}
	if total, calls := sink.snapshot(); total != 700 || calls != 1 {
		t.Fatalf("sink after settled fetch: total=%d calls=%d, want 700/1 (CLI path has no sweep; fetch is the only charger)", total, calls)
	}
}

// TestRC13W3FetchChargesTimedOutLateUsageExactlyOnce proves the
// exactly-once property on the fetch path: fetching the same
// timed-out result again must not charge twice. It also proves the
// fetch side does not burn its single charge while the usage is
// still unsettled (the fetch-before-settling in the test above
// charged nothing, so the settled fetch still charges the full
// 700 exactly once).
func TestRC13W3FetchChargesTimedOutLateUsageExactlyOnce(t *testing.T) {
	release := make(chan struct{})
	tools, mgr, sink := buildSubagentTools(t, 4, rc13w3LateExecutor(release))

	id, err := mgr.Spawn("slow", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if _, err := mgr.Wait(context.Background(), id); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	// Unsettled fetch first: must not consume the charge.
	if _, err := rc13w3FetchResult(t, tools, id); err != nil {
		t.Fatalf("fetch while unsettled: %v", err)
	}
	close(release)
	rc13w3WaitSettled(t, mgr, id, 700)

	// Three settled fetches: exactly one charge of 700.
	for i := 0; i < 3; i++ {
		m, err := rc13w3FetchResult(t, tools, id)
		if err != nil {
			t.Fatalf("settled fetch %d: %v", i+1, err)
		}
		if m["tokens"] != float64(700) {
			t.Fatalf("settled fetch %d: tokens = %v, want 700", i+1, m["tokens"])
		}
	}
	if total, calls := sink.snapshot(); total != 700 || calls != 1 {
		t.Fatalf("sink after 3 settled fetches: total=%d calls=%d, want 700/1 (exactly once)", total, calls)
	}
}

// TestRC13W3FetchAndSweepChargeTimedOutLateUsageOnceEach proves the
// fetch fix does not double-charge on the chat path, where both the
// fetch and the per-chat sweep can see the same timed-out task.
// The two chargers write to SEPARATE buckets — the fetch sink
// resolves to the global agent (app.go:206-214), while the sweep
// charges the chat's stats/budget (chats.go runChatTurn) — exactly
// as Done/Failed tasks already do today. So "exactly once" holds
// per bucket: this test models both consumers (the real fetch tool
// plus a sweep-like consumer mirroring sweepFinishedSubagents'
// guard: chargeable status + UsageSettled + a per-consumer seen
// set) and asserts each charges the 700 exactly once, neither
// more, and neither before the usage settles. The fetch fix must
// not mutate any Manager state the sweep depends on — it only
// touches the tools-local delivered set.
func TestRC13W3FetchAndSweepChargeTimedOutLateUsageOnceEach(t *testing.T) {
	release := make(chan struct{})
	tools, mgr, sink := buildSubagentTools(t, 4, rc13w3LateExecutor(release))
	// Sweep-like second consumer: its own seen set, its own sink
	// (the chat's stats/budget bucket). Mirrors the real sweep's
	// guard: skip unsettled without marking, charge settled once.
	sweepSink := &fakeUsageSink{}
	swept := map[string]bool{}
	sweepOnce := func() {
		for _, ts := range mgr.List() {
			if ts.Status != subagent.StatusDone && ts.Status != subagent.StatusFailed && ts.Status != subagent.StatusTimeout {
				continue
			}
			res, ok := mgr.GetResult(ts.ID)
			if !ok || !res.UsageSettled {
				continue // skip WITHOUT marking, like the real sweep
			}
			if swept[ts.ID] {
				continue
			}
			swept[ts.ID] = true
			sweepSink.AddSubagentUsage(res.Usage.Tokens)
		}
	}

	id, err := mgr.Spawn("slow", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if _, err := mgr.Wait(context.Background(), id); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	// Both consumers see the task while unsettled: neither charges.
	if _, err := rc13w3FetchResult(t, tools, id); err != nil {
		t.Fatalf("fetch while unsettled: %v", err)
	}
	sweepOnce()
	if total, _ := sink.snapshot(); total != 0 {
		t.Fatalf("fetch sink while unsettled: total=%d, want 0", total)
	}
	if total, _ := sweepSink.snapshot(); total != 0 {
		t.Fatalf("sweep sink while unsettled: total=%d, want 0", total)
	}

	// Usage settles at 700. Fetch charges its bucket once; the
	// sweep charges its bucket once; repeats charge nothing.
	close(release)
	rc13w3WaitSettled(t, mgr, id, 700)
	if _, err := rc13w3FetchResult(t, tools, id); err != nil {
		t.Fatalf("settled fetch: %v", err)
	}
	sweepOnce()
	if _, err := rc13w3FetchResult(t, tools, id); err != nil {
		t.Fatalf("second settled fetch: %v", err)
	}
	sweepOnce()
	if total, calls := sink.snapshot(); total != 700 || calls != 1 {
		t.Fatalf("fetch sink: total=%d calls=%d, want 700/1", total, calls)
	}
	if total, calls := sweepSink.snapshot(); total != 700 || calls != 1 {
		t.Fatalf("sweep sink: total=%d calls=%d, want 700/1", total, calls)
	}
}
