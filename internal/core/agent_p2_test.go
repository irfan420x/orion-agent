package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/tools"
)

// --- P2-1: NewAgent must not mutate the caller's config -----------------

func TestNewAgentDoesNotMutateCallerConfig(t *testing.T) {
	cfg := config.Default()
	cfg.MaxSteps = 0
	cfg.TimeoutSeconds = 0
	if _, err := NewAgent(cfg, &fakeProvider{}, tools.NewRegistry(), newFakeStore()); err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if cfg.MaxSteps != 0 {
		t.Fatalf("caller MaxSteps mutated to %d, want 0", cfg.MaxSteps)
	}
	if cfg.TimeoutSeconds != 0 {
		t.Fatalf("caller TimeoutSeconds mutated to %d, want 0", cfg.TimeoutSeconds)
	}

	// Explicit values are left alone too.
	cfg.MaxSteps = 3
	cfg.TimeoutSeconds = 7
	a, err := NewAgent(cfg, &fakeProvider{}, tools.NewRegistry(), newFakeStore())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if a.maxSteps() != 3 || a.timeout() != 7*time.Second {
		t.Fatalf("agent limits = %d steps / %v, want 3 / 7s", a.maxSteps(), a.timeout())
	}
	if cfg.MaxSteps != 3 || cfg.TimeoutSeconds != 7 {
		t.Fatalf("caller config changed: %+v", cfg)
	}
	// ... and the zero case still gets working defaults internally.
	a2, err := NewAgent(config.Default(), &fakeProvider{}, tools.NewRegistry(), newFakeStore())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if a2.maxSteps() != defaultMaxSteps || a2.timeout() != defaultTimeoutSeconds*time.Second {
		t.Fatalf("defaults = %d steps / %v, want %d / %ds", a2.maxSteps(), a2.timeout(), defaultMaxSteps, defaultTimeoutSeconds)
	}
}

// --- P2-2: parseToolArguments requires a JSON object --------------------

func TestParseToolArgumentsRequiresObject(t *testing.T) {
	cases := []struct {
		name string
		args string
		want bool
	}{
		{"empty", "", true},
		{"whitespace", "   ", true},
		{"null", "null", true},
		{"object", `{"text":"hi"}`, true},
		{"empty object", "{}", true},
		{"array", "[1,2]", false},
		{"number", "123", false},
		{"string", `"hello"`, false},
		{"bool", "true", false},
		{"invalid json", "{bad", false},
		{"trailing data", `{"a":1} {"b":2}`, false},
		{"trailing garbage", `{} garbage`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, ok := parseToolArguments(model.ToolCall{Name: "echo", Arguments: tc.args})
			if ok != tc.want {
				t.Fatalf("parseToolArguments(%q) ok = %v, want %v", tc.args, ok, tc.want)
			}
			if tc.want {
				var m map[string]any
				if err := json.Unmarshal(args, &m); err != nil {
					t.Fatalf("returned args %q are not a JSON object: %v", string(args), err)
				}
			}
		})
	}
}

// TestRunFeedsNonObjectArgumentsAsError: a tool call whose arguments
// are valid JSON but not an object is fed back as an error, not
// passed to the tool.
func TestRunFeedsNonObjectArgumentsAsError(t *testing.T) {
	echo := &echoTool{}
	reg := tools.NewRegistry()
	if err := reg.Register(echo); err != nil {
		t.Fatal(err)
	}
	prov := &fakeProvider{
		chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
			if call == 1 {
				return toolCallResponse("call-1", "echo", `[1,2]`), nil
			}
			return textResponse("noted"), nil
		},
	}
	a := newTestAgent(t, testConfig(5), prov, reg, newFakeStore())
	if _, err := a.Run(context.Background(), "echo array"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if echo.callCount() != 0 {
		t.Fatalf("echo calls = %d, want 0 (non-object args must not reach the tool)", echo.callCount())
	}
}

// --- P2-3: tool execution gets its own timeout --------------------------

// deadlineTool records how much time its ctx has left when it runs.
type deadlineTool struct {
	remaining time.Duration
}

func (deadlineTool) Name() string        { return "deadline" }
func (deadlineTool) Description() string { return "Reports its remaining context deadline." }

// RiskClass declares the tool Safe (Register requires a class
// matching permissions.Classify; "deadline" is not in the gate's maps).
func (deadlineTool) RiskClass() permissions.Class { return permissions.ClassSafe }
func (deadlineTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: "deadline", Description: "Reports deadline.", Parameters: map[string]any{"type": "object"}}
}
func (t *deadlineTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	if dl, ok := ctx.Deadline(); ok {
		t.remaining = time.Until(dl)
	}
	return "ok", nil
}

func TestToolExecutionGetsOwnTimeout(t *testing.T) {
	tool := &deadlineTool{}
	reg := tools.NewRegistry()
	if err := reg.Register(tool); err != nil {
		t.Fatal(err)
	}
	// The provider burns 800ms of the 1s step budget before the tool
	// runs. If tools shared the step ctx, the tool would see ~200ms
	// left; with its own timeout it must see close to a full second.
	prov := &fakeProvider{
		chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
			if call == 1 {
				time.Sleep(800 * time.Millisecond)
				return toolCallResponse("call-1", "deadline", `{}`), nil
			}
			return textResponse("done"), nil
		},
	}
	cfg := testConfig(5)
	cfg.TimeoutSeconds = 1
	a := newTestAgent(t, cfg, prov, reg, newFakeStore())
	if _, err := a.Run(context.Background(), "check deadline"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tool.remaining < 500*time.Millisecond {
		t.Fatalf("tool ctx had %v left, want >= 500ms (its own fresh timeout, not the step ctx remainder)", tool.remaining)
	}
}

// --- P2-4: SetLogger/Shutdown don't block on a running run -------------

func TestSetLoggerAndShutdownDoNotBlockDuringRun(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	prov := &fakeProvider{
		chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
			close(entered)
			<-release
			return textResponse("finished"), nil
		},
	}
	mem := newFakeStore()
	a := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), mem)

	runDone := make(chan error, 1)
	go func() {
		_, err := a.Run(context.Background(), "block please")
		runDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("provider Chat was never called")
	}

	// SetLogger and Shutdown must return promptly while the run is
	// still blocked inside the provider call.
	done := make(chan struct{})
	go func() {
		a.SetLogger(log.New(io.Discard, "", 0))
		if err := a.Shutdown(); err != nil {
			t.Errorf("Shutdown during run: %v", err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SetLogger/Shutdown blocked for over 2s while a run was in flight")
	}

	close(release)
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("blocked Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not finish after the provider was released")
	}

	// New runs are refused after Shutdown.
	if _, err := a.Run(context.Background(), "again"); err == nil {
		t.Error("Run after Shutdown: expected error, got nil")
	} else if !strings.Contains(err.Error(), "shut down") {
		t.Errorf("Run after Shutdown error = %q, want it to mention shut down", err.Error())
	}
	// Shutdown is idempotent.
	if err := a.Shutdown(); err != nil {
		t.Errorf("second Shutdown: %v, want nil", err)
	}
}

// TestRunsAreSerialised: two concurrent runs execute one after the
// other, not interleaved.
func TestRunsAreSerialised(t *testing.T) {
	prov := &fakeProvider{
		chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
			return textResponse("ok"), nil
		},
	}
	a := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), newFakeStore())
	errs := make(chan error, 2)
	go func() { _, err := a.Run(context.Background(), "one"); errs <- err }()
	go func() { _, err := a.Run(context.Background(), "two"); errs <- err }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("concurrent Run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent runs did not finish")
		}
	}
	if got := prov.calls(); got != 2 {
		t.Fatalf("provider Chat calls = %d, want 2 (both runs completed)", got)
	}
}

// --- P2-22: Shutdown must not report one save failure twice -------------

// failingPersistStore is a memory.Store whose Save and Close fail with
// the same underlying persist failure, mirroring the real FileStore:
// Close re-attempts the save, so both calls fail identically. It
// embeds fakeStore for the rest of the Store interface.
type failingPersistStore struct {
	*fakeStore
	saveErr  error
	closeErr error
	saves    int
	closes   int
}

func (s *failingPersistStore) Save() error {
	s.saves++
	return s.saveErr
}

func (s *failingPersistStore) Close() error {
	s.closes++
	return s.closeErr
}

func TestShutdownReportsOneSaveFailureOnce(t *testing.T) {
	// Distinct error values with the identical message — exactly what
	// the real store produces when the same persist fails twice.
	const msg = "memory: create temp file: disk full"
	mem := &failingPersistStore{
		fakeStore: newFakeStore(),
		saveErr:   errors.New(msg),
		closeErr:  errors.New(msg),
	}
	a := newTestAgent(t, testConfig(5), &fakeProvider{}, tools.NewRegistry(), mem)
	err := a.Shutdown()
	if err == nil {
		t.Fatal("Shutdown: expected the save failure, got nil")
	}
	if got := strings.Count(err.Error(), msg); got != 1 {
		t.Errorf("Shutdown error %q mentions the failure %d times, want exactly 1", err.Error(), got)
	}
	// Close must still have been attempted after the failed Save: it
	// is the retry that can still persist on a transient failure.
	if mem.saves != 1 || mem.closes != 1 {
		t.Errorf("Save calls = %d, Close calls = %d, want 1 and 1", mem.saves, mem.closes)
	}
}

func TestShutdownReportsOneSaveFailureOnceSameErrorValue(t *testing.T) {
	// Same error value returned by both calls (a store that reuses one
	// sentinel) must also dedupe to a single report.
	sentinel := errors.New("memory: encode state: boom")
	mem := &failingPersistStore{
		fakeStore: newFakeStore(),
		saveErr:   sentinel,
		closeErr:  sentinel,
	}
	a := newTestAgent(t, testConfig(5), &fakeProvider{}, tools.NewRegistry(), mem)
	err := a.Shutdown()
	if err == nil {
		t.Fatal("Shutdown: expected the save failure, got nil")
	}
	if got := strings.Count(err.Error(), sentinel.Error()); got != 1 {
		t.Errorf("Shutdown error %q mentions the failure %d times, want exactly 1", err.Error(), got)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("Shutdown error %v does not wrap the underlying failure", err)
	}
}

func TestShutdownReportsDistinctSaveAndCloseFailures(t *testing.T) {
	// Dedupe must not swallow genuinely different failures.
	mem := &failingPersistStore{
		fakeStore: newFakeStore(),
		saveErr:   errors.New("memory: save failed"),
		closeErr:  errors.New("memory: close failed differently"),
	}
	a := newTestAgent(t, testConfig(5), &fakeProvider{}, tools.NewRegistry(), mem)
	err := a.Shutdown()
	if err == nil {
		t.Fatal("Shutdown: expected both failures, got nil")
	}
	for _, want := range []string{"memory: save failed", "memory: close failed differently"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Shutdown error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestShutdownReportsSaveFailureWhenCloseSucceeds(t *testing.T) {
	// Save fails but Close's retry succeeds: the failure is still
	// surfaced, exactly once.
	mem := &failingPersistStore{
		fakeStore: newFakeStore(),
		saveErr:   errors.New("memory: create temp file: disk full"),
		closeErr:  nil,
	}
	a := newTestAgent(t, testConfig(5), &fakeProvider{}, tools.NewRegistry(), mem)
	err := a.Shutdown()
	if err == nil {
		t.Fatal("Shutdown: expected the save failure, got nil")
	}
	if got := strings.Count(err.Error(), "memory: create temp file: disk full"); got != 1 {
		t.Errorf("Shutdown error %q mentions the failure %d times, want exactly 1", err.Error(), got)
	}
}
