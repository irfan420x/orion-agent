package tools

import (
	"context"
	"log"
	"sync"
	"testing"
	"time"
)

// cronCountingWriter counts log lines: the scheduler logs one line
// per tick error, so with no runner configured (every ProcessDue
// errors loudly) the line count is the tick count plus the initial
// scan — a way to observe Run's tick cadence without a due job
// (a due job fires once and its NextRun advances, so a runner
// call count cannot measure cadence).
type cronCountingWriter struct {
	mu sync.Mutex
	n  int
}

func (w *cronCountingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.n++
	w.mu.Unlock()
	return len(p), nil
}

func (w *cronCountingWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

// TestCronSetTickIntervalAppliesWhileRunning proves F-C21: a
// SetTickInterval call made after Run has started reschedules
// the pending tick at the new interval, instead of being
// silently ignored until a restart (the old fixed ticker).
//
// The scheduler starts at a 300ms interval: after the initial
// scan (1 log line) no tick may have happened 120ms in —
// timers never fire early, so the count must still be 1. The
// interval is then cut to 30ms: 900ms at 30ms holds ~30 ticks,
// while the stale 300ms interval would hold at most 3 more
// (count 4). Reaching 6 log lines inside 900ms therefore proves
// the running scheduler picked up the new interval.
func TestCronSetTickIntervalAppliesWhileRunning(t *testing.T) {
	store := NewPlanningStore("")
	s := NewCronScheduler(store)
	w := &cronCountingWriter{}
	s.SetLogger(log.New(w, "", 0))
	s.SetTickInterval(300 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()

	// The initial scan logs once (no runner configured).
	waitForCronRunnerCalls(t, w.count, 1)
	time.Sleep(120 * time.Millisecond)
	if got := w.count(); got != 1 {
		cancel()
		t.Fatalf("log lines 120ms after start = %d, want 1 (no tick yet at the 300ms interval)", got)
	}

	s.SetTickInterval(30 * time.Millisecond)
	deadline := time.Now().Add(900 * time.Millisecond)
	for w.count() < 6 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := w.count(); got < 6 {
		cancel()
		t.Fatalf("log lines after cutting the interval to 30ms = %d, want >= 6 within 900ms (stale 300ms interval would give at most 4)", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestCronSetTickIntervalBeforeRun proves the same setter is
// honoured when called before Run starts (the pre-existing
// behaviour, kept by the timer rebuild): at 30ms a fresh Run
// ticks several times within 300ms.
func TestCronSetTickIntervalBeforeRun(t *testing.T) {
	store := NewPlanningStore("")
	s := NewCronScheduler(store)
	w := &cronCountingWriter{}
	s.SetLogger(log.New(w, "", 0))
	s.SetTickInterval(30 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()

	// Initial scan (1) plus ticks at 30ms: 300ms holds ~10.
	waitForCronRunnerCalls(t, w.count, 6)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
