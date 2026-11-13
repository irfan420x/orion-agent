package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/tools"
)

// recordingHandler collects ProgressEvents for assertions. Handlers run
// synchronously on the run goroutine, but the mutex keeps it safe
// when a test sets the handler from another goroutine mid-run.
type recordingHandler struct {
	mu     sync.Mutex
	events []ProgressEvent
}

func (r *recordingHandler) handle(ev ProgressEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recordingHandler) all() []ProgressEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ProgressEvent(nil), r.events...)
}

func (r *recordingHandler) kinds() []EventKind {
	evs := r.all()
	kinds := make([]EventKind, len(evs))
	for i, ev := range evs {
		kinds[i] = ev.Kind
	}
	return kinds
}

func (r *recordingHandler) count(kind EventKind) int {
	n := 0
	for _, ev := range r.all() {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

func TestEventSequenceMultiStepRun(t *testing.T) {
	echo := &echoTool{}
	reg := tools.NewRegistry()
	if err := reg.Register(echo); err != nil {
		t.Fatalf("Register echo: %v", err)
	}
	prov := &fakeProvider{
		chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
			if call == 1 {
				resp := toolCallResponse("call-1", "echo", `{"text":"hi"}`)
				resp.Usage = model.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
				return resp, nil
			}
			resp := textResponse("done")
			resp.Usage = model.Usage{PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30}
			return resp, nil
		},
	}
	rec := &recordingHandler{}
	a := newTestAgent(t, testConfig(5), prov, reg, newFakeStore())
	a.SetEventHandler(rec.handle)

	out, err := a.Run(context.Background(), "say hi via echo")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "done" {
		t.Errorf("Run = %q, want %q", out, "done")
	}

	wantKinds := []EventKind{
		EventStepStarted,
		EventToolCallStarted,
		EventToolCallFinished,
		EventStepStarted,
		EventTurnFinished,
	}
	gotKinds := rec.kinds()
	if len(gotKinds) != len(wantKinds) {
		t.Fatalf("event kinds = %v, want %v", gotKinds, wantKinds)
	}
	for i, want := range wantKinds {
		if gotKinds[i] != want {
			t.Errorf("event %d kind = %q, want %q", i, gotKinds[i], want)
		}
	}
	evs := rec.all()

	// StepStarted step 1: before the first provider call, usage is zero.
	if evs[0].Step != 1 || evs[0].Usage != (model.Usage{}) {
		t.Errorf("event 0 = %+v, want StepStarted step 1 with zero usage", evs[0])
	}
	// ToolCallStarted step 1: name/args exactly as the model sent them,
	// usage accumulated from step 1's provider call.
	if evs[1].Step != 1 || evs[1].ToolName != "echo" || evs[1].Arguments != `{"text":"hi"}` {
		t.Errorf("event 1 = %+v, want ToolCallStarted step 1 echo {\"text\":\"hi\"}", evs[1])
	}
	if evs[1].Usage != (model.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}) {
		t.Errorf("event 1 Usage = %+v, want {10 5 15}", evs[1].Usage)
	}
	// ToolCallFinished step 1: duration > 0, no error, same usage.
	if evs[2].Step != 1 || evs[2].ToolName != "echo" || evs[2].Arguments != `{"text":"hi"}` {
		t.Errorf("event 2 = %+v, want ToolCallFinished step 1 echo {\"text\":\"hi\"}", evs[2])
	}
	if evs[2].Duration <= 0 {
		t.Errorf("event 2 Duration = %v, want > 0", evs[2].Duration)
	}
	if evs[2].Err != nil {
		t.Errorf("event 2 Err = %v, want nil", evs[2].Err)
	}
	// StepStarted step 2: usage accumulated so far (step 1 only).
	if evs[3].Step != 2 || evs[3].Usage != (model.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}) {
		t.Errorf("event 3 = %+v, want StepStarted step 2 with usage {10 5 15}", evs[3])
	}
	// TurnFinished: step 2, duration > 0, no error, total usage.
	if evs[4].Step != 2 || evs[4].Err != nil || evs[4].Duration <= 0 {
		t.Errorf("event 4 = %+v, want TurnFinished step 2, nil error, duration > 0", evs[4])
	}
	if evs[4].Usage != (model.Usage{PromptTokens: 30, CompletionTokens: 15, TotalTokens: 45}) {
		t.Errorf("event 4 Usage = %+v, want {30 15 45}", evs[4].Usage)
	}
}

func TestEventToolErrorSetsFinishedErr(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(failingTool{}); err != nil {
		t.Fatalf("Register boom: %v", err)
	}
	prov := &fakeProvider{
		chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
			if call == 1 {
				return toolCallResponse("call-1", "boom", `{}`), nil
			}
			return textResponse("recovered after error"), nil
		},
	}
	rec := &recordingHandler{}
	a := newTestAgent(t, testConfig(5), prov, reg, newFakeStore())
	a.SetEventHandler(rec.handle)

	out, err := a.Run(context.Background(), "run boom")
	if err != nil {
		t.Fatalf("Run should not abort on tool error, got: %v", err)
	}
	if out != "recovered after error" {
		t.Errorf("Run = %q, want recovery answer", out)
	}

	// The tool "error" is result text starting with "Error", so the
	// ToolCallFinished event must carry it as Err — while the turn
	// itself still finishes successfully.
	var finished *ProgressEvent
	var turn *ProgressEvent
	for _, ev := range rec.all() {
		if ev.Kind == EventToolCallFinished {
			ev := ev
			finished = &ev
		}
		if ev.Kind == EventTurnFinished {
			ev := ev
			turn = &ev
		}
	}
	if finished == nil {
		t.Fatal("no ToolCallFinished event")
	}
	if finished.Err == nil || !strings.Contains(finished.Err.Error(), "boom exploded") {
		t.Errorf("ToolCallFinished Err = %v, want it to contain %q", finished.Err, "boom exploded")
	}
	if finished.Duration <= 0 {
		t.Errorf("ToolCallFinished Duration = %v, want > 0", finished.Duration)
	}
	if turn == nil {
		t.Fatal("no TurnFinished event")
	}
	if turn.Err != nil {
		t.Errorf("TurnFinished Err = %v, want nil (tool error is fed back, turn recovers)", turn.Err)
	}
}

func TestEventHandlerPanicDoesNotCrashRun(t *testing.T) {
	echo := &echoTool{}
	reg := tools.NewRegistry()
	if err := reg.Register(echo); err != nil {
		t.Fatalf("Register echo: %v", err)
	}
	prov := &fakeProvider{
		chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
			if call == 1 {
				return toolCallResponse("call-1", "echo", `{"text":"hi"}`), nil
			}
			return textResponse("done"), nil
		},
	}
	a := newTestAgent(t, testConfig(5), prov, reg, newFakeStore())
	// A handler that panics on every event must not crash the loop:
	// emit recovers and logs, and the run returns its normal result.
	a.SetEventHandler(func(ProgressEvent) { panic("handler boom") })

	out, err := a.Run(context.Background(), "say hi via echo")
	if err != nil {
		t.Fatalf("Run with panicking handler: %v", err)
	}
	if out != "done" {
		t.Errorf("Run = %q, want %q", out, "done")
	}
	if got := echo.callCount(); got != 1 {
		t.Errorf("echo calls = %d, want 1 (tool still ran despite handler panics)", got)
	}
}

func TestSetEventHandlerNilClears(t *testing.T) {
	rec := &recordingHandler{}
	a := newTestAgent(t, testConfig(5), &fakeProvider{}, tools.NewRegistry(), newFakeStore())
	a.SetEventHandler(rec.handle)
	a.SetEventHandler(nil)

	if _, err := a.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(rec.all()); got != 0 {
		t.Errorf("got %d events after SetEventHandler(nil), want 0", got)
	}
}

func TestSetEventHandlerDuringRunDoesNotDeadlock(t *testing.T) {
	unblock := make(chan struct{})
	prov := &fakeProvider{
		chatFn: func(_ int, _ model.ChatRequest) (*model.ChatResponse, error) {
			select {
			case <-unblock:
			case <-time.After(5 * time.Second):
				return nil, errors.New("test provider timed out waiting for unblock")
			}
			return textResponse("done"), nil
		},
	}
	rec := &recordingHandler{}
	a := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), newFakeStore())

	type runResult struct {
		out string
		err error
	}
	done := make(chan runResult, 1)
	go func() {
		out, err := a.Run(context.Background(), "hello")
		done <- runResult{out, err}
	}()

	// Set the handler from another goroutine while the run is in
	// flight (provider blocked). This must not deadlock: the run
	// may or may not observe the handler, but it must finish.
	time.Sleep(100 * time.Millisecond)
	setDone := make(chan struct{})
	go func() {
		a.SetEventHandler(rec.handle)
		close(setDone)
	}()

	select {
	case <-setDone:
	case <-time.After(5 * time.Second):
		t.Fatal("SetEventHandler deadlocked while a run was in flight")
	}
	close(unblock)

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("Run: %v", res.err)
		}
		if res.out != "done" {
			t.Errorf("Run = %q, want %q", res.out, "done")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run deadlocked after SetEventHandler during run")
	}
	// StepStarted was already emitted before the handler was set, so
	// at most the TurnFinished event can have been observed.
	if got := len(rec.all()); got > 1 {
		t.Errorf("got %d events, want at most 1 (TurnFinished)", got)
	}
}

func TestTurnFinishedExactlyOnceOnMaxSteps(t *testing.T) {
	prov := &fakeProvider{
		chatFn: func(_ int, _ model.ChatRequest) (*model.ChatResponse, error) {
			return toolCallResponse("c1", "echo", `{"text":"x"}`), nil
		},
	}
	reg := tools.NewRegistry()
	if err := reg.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	rec := &recordingHandler{}
	a := newTestAgent(t, testConfig(2), prov, reg, newFakeStore())
	a.SetEventHandler(rec.handle)

	_, err := a.Run(context.Background(), "loop forever")
	if err == nil {
		t.Fatal("Run: expected MaxStepsError, got nil")
	}
	var maxErr *MaxStepsError
	if !errors.As(err, &maxErr) {
		t.Fatalf("error = %T, want *MaxStepsError", err)
	}

	if got := rec.count(EventTurnFinished); got != 1 {
		t.Errorf("TurnFinished count = %d, want exactly 1", got)
	}
	if got := rec.count(EventStepStarted); got != 2 {
		t.Errorf("StepStarted count = %d, want 2 (one per step)", got)
	}
	var turn *ProgressEvent
	for _, ev := range rec.all() {
		if ev.Kind == EventTurnFinished {
			ev := ev
			turn = &ev
		}
	}
	if turn == nil {
		t.Fatal("no TurnFinished event")
	}
	if turn.Step != 2 {
		t.Errorf("TurnFinished Step = %d, want 2", turn.Step)
	}
	if turn.Duration <= 0 {
		t.Errorf("TurnFinished Duration = %v, want > 0", turn.Duration)
	}
	if turn.Err == nil || !strings.Contains(turn.Err.Error(), "max steps exceeded") {
		t.Errorf("TurnFinished Err = %v, want the MaxStepsError", turn.Err)
	}
}

func TestTurnFinishedExactlyOnceOnProviderError(t *testing.T) {
	echo := &echoTool{}
	reg := tools.NewRegistry()
	if err := reg.Register(echo); err != nil {
		t.Fatalf("Register echo: %v", err)
	}
	prov := &fakeProvider{
		chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
			if call == 1 {
				resp := toolCallResponse("call-1", "echo", `{"text":"hi"}`)
				resp.Usage = model.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}
				return resp, nil
			}
			return nil, errors.New("upstream down")
		},
	}
	rec := &recordingHandler{}
	a := newTestAgent(t, testConfig(5), prov, reg, newFakeStore())
	a.SetEventHandler(rec.handle)

	if _, err := a.Run(context.Background(), "hello"); err == nil {
		t.Fatal("Run: expected provider error, got nil")
	} else if !strings.Contains(err.Error(), "upstream down") {
		t.Errorf("Run error = %q, want wrapped provider error", err.Error())
	}

	wantKinds := []EventKind{
		EventStepStarted,
		EventToolCallStarted,
		EventToolCallFinished,
		EventStepStarted,
		EventTurnFinished,
	}
	if gotKinds := rec.kinds(); len(gotKinds) != len(wantKinds) {
		t.Fatalf("event kinds = %v, want %v", gotKinds, wantKinds)
	} else {
		for i, want := range wantKinds {
			if gotKinds[i] != want {
				t.Errorf("event %d kind = %q, want %q", i, gotKinds[i], want)
			}
		}
	}
	evs := rec.all()
	turn := evs[len(evs)-1]
	if turn.Step != 2 {
		t.Errorf("TurnFinished Step = %d, want 2 (error on step 2)", turn.Step)
	}
	if turn.Err == nil || !strings.Contains(turn.Err.Error(), "upstream down") {
		t.Errorf("TurnFinished Err = %v, want the provider error", turn.Err)
	}
	if turn.Usage != (model.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}) {
		t.Errorf("TurnFinished Usage = %+v, want step 1's usage {7 3 10}", turn.Usage)
	}
}

func TestNoTurnFinishedOnEarlyValidationErrors(t *testing.T) {
	rec := &recordingHandler{}
	a := newTestAgent(t, testConfig(5), &fakeProvider{}, tools.NewRegistry(), newFakeStore())
	a.SetEventHandler(rec.handle)

	// Empty input: validation error, no turn started.
	if _, err := a.Run(context.Background(), "   "); err == nil {
		t.Error("Run(empty input): expected error, got nil")
	}
	// Cancelled before start: no turn started.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Run(ctx, "hello"); err == nil {
		t.Error("Run(cancelled ctx): expected error, got nil")
	}
	// RunStream with a nil callback: validation error, no turn started.
	if _, err := a.RunStream(context.Background(), "hi", nil); err == nil {
		t.Error("RunStream(nil fn): expected error, got nil")
	}
	// Shut down, then run: refused before the turn starts.
	if err := a.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if _, err := a.Run(context.Background(), "hello"); err == nil {
		t.Error("Run(after shutdown): expected error, got nil")
	}

	if got := len(rec.all()); got != 0 {
		t.Errorf("got %d events for validation errors, want 0: %+v", got, rec.all())
	}
}

func TestEventUsageStaysZeroWhenProviderReportsNone(t *testing.T) {
	// The default fakeProvider reports zero usage; the accumulated
	// field must stay zero (no estimation in the core).
	rec := &recordingHandler{}
	a := newTestAgent(t, testConfig(5), &fakeProvider{}, tools.NewRegistry(), newFakeStore())
	a.SetEventHandler(rec.handle)

	if _, err := a.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	evs := rec.all()
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 (StepStarted + TurnFinished): %v", len(evs), rec.kinds())
	}
	if evs[0].Kind != EventStepStarted || evs[0].Usage != (model.Usage{}) {
		t.Errorf("event 0 = %+v, want StepStarted with zero usage", evs[0])
	}
	if evs[1].Kind != EventTurnFinished || evs[1].Usage != (model.Usage{}) || evs[1].Err != nil {
		t.Errorf("event 1 = %+v, want TurnFinished with zero usage and nil error", evs[1])
	}
}

func TestEventSequenceViaRunStream(t *testing.T) {
	echo := &echoTool{}
	reg := tools.NewRegistry()
	if err := reg.Register(echo); err != nil {
		t.Fatal(err)
	}
	prov := &fakeProvider{
		streamFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
			if call == 1 {
				return toolCallResponse("call-1", "echo", `{"text":"yo"}`), nil
			}
			return textResponse("the tool said echo:yo"), nil
		},
	}
	rec := &recordingHandler{}
	a := newTestAgent(t, testConfig(5), prov, reg, newFakeStore())
	a.SetEventHandler(rec.handle)

	out, err := a.RunStream(context.Background(), "echo yo", func(string) error { return nil })
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if out != "the tool said echo:yo" {
		t.Errorf("RunStream = %q, want %q", out, "the tool said echo:yo")
	}
	wantKinds := []EventKind{
		EventStepStarted,
		EventToolCallStarted,
		EventToolCallFinished,
		EventStepStarted,
		EventTurnFinished,
	}
	if gotKinds := rec.kinds(); len(gotKinds) != len(wantKinds) {
		t.Fatalf("event kinds = %v, want %v", gotKinds, wantKinds)
	} else {
		for i, want := range wantKinds {
			if gotKinds[i] != want {
				t.Errorf("event %d kind = %q, want %q", i, gotKinds[i], want)
			}
		}
	}
}
