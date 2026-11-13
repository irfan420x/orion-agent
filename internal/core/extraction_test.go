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

// fakeSessionAppender records appended session-memory entries.
type fakeSessionAppender struct {
	mu      sync.Mutex
	entries []string
	err     error
}

func (f *fakeSessionAppender) Append(entry string) error {
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, entry)
	return nil
}

func (f *fakeSessionAppender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.entries)
}

func testTranscript() []model.Message {
	return []model.Message{
		{Role: "user", Content: "My favourite number is 17."},
		{Role: "assistant", Content: "Noted, 17."},
		{Role: "user", Content: "Remember I prefer short answers."},
		{Role: "assistant", Content: "Will do."},
	}
}

func TestExtractionThresholds(t *testing.T) {
	// Default thresholds are 10 tool calls / 5 turns.
	a := newV2TestAgent(t)
	toolsThreshold, turnsThreshold := a.extractionThresholds()
	if toolsThreshold != defaultExtractEveryToolCalls || turnsThreshold != defaultExtractEveryTurns {
		t.Fatalf("defaults = (%d, %d), want (%d, %d)", toolsThreshold, turnsThreshold, defaultExtractEveryToolCalls, defaultExtractEveryTurns)
	}
	// Custom thresholds apply.
	a.SetExtractionThresholds(3, 2)
	toolsThreshold, turnsThreshold = a.extractionThresholds()
	if toolsThreshold != 3 || turnsThreshold != 2 {
		t.Fatalf("custom = (%d, %d), want (3, 2)", toolsThreshold, turnsThreshold)
	}
	// Non-positive restores defaults.
	a.SetExtractionThresholds(0, -1)
	toolsThreshold, turnsThreshold = a.extractionThresholds()
	if toolsThreshold != defaultExtractEveryToolCalls || turnsThreshold != defaultExtractEveryTurns {
		t.Fatalf("reset = (%d, %d), want defaults", toolsThreshold, turnsThreshold)
	}
}

func TestExtractionFiresOnTurnThreshold(t *testing.T) {
	a := newV2TestAgent(t)
	app := &fakeSessionAppender{}
	a.SetSessionMemoryAppender(app)
	a.SetExtractionThresholds(100, 2) // only the turn leg can fire
	a.SetSessionExtractor(func(ctx context.Context, turns []model.Message) (string, error) {
		return "- favourite number is 17", nil
	})

	a.noteTurnCompleted(context.Background(), testTranscript())
	if n := app.count(); n != 0 {
		t.Fatalf("after 1 turn: %d appends, want 0", n)
	}
	a.noteTurnCompleted(context.Background(), testTranscript())
	// Background goroutine: wait briefly for the append.
	deadline := time.Now().Add(2 * time.Second)
	for app.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := app.count(); n != 1 {
		t.Fatalf("after 2 turns: %d appends, want 1", n)
	}
	if !strings.Contains(app.entries[0], "favourite number") {
		t.Errorf("appended entry = %q, want the summary", app.entries[0])
	}
	// Counters reset: one more turn must not fire again.
	a.noteTurnCompleted(context.Background(), testTranscript())
	time.Sleep(100 * time.Millisecond)
	if n := app.count(); n != 1 {
		t.Errorf("after reset + 1 turn: %d appends, want still 1", n)
	}
}

func TestExtractionFiresOnToolCallThreshold(t *testing.T) {
	a := newV2TestAgent(t)
	app := &fakeSessionAppender{}
	a.SetSessionMemoryAppender(app)
	a.SetExtractionThresholds(3, 100) // only the tool-call leg can fire
	a.SetSessionExtractor(func(ctx context.Context, turns []model.Message) (string, error) {
		return "- tool summary", nil
	})

	a.noteToolCallExecuted("calculator")
	a.noteToolCallExecuted("calculator")
	a.noteTurnCompleted(context.Background(), testTranscript())
	if n := app.count(); n != 0 {
		t.Fatalf("after 2 tool calls: %d appends, want 0", n)
	}
	a.noteToolCallExecuted("calculator")
	a.noteTurnCompleted(context.Background(), testTranscript())
	deadline := time.Now().Add(2 * time.Second)
	for app.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := app.count(); n != 1 {
		t.Fatalf("after 3 tool calls: %d appends, want 1", n)
	}
}

func TestExtractionSingleFlight(t *testing.T) {
	a := newV2TestAgent(t)
	app := &fakeSessionAppender{}
	a.SetSessionMemoryAppender(app)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls int
	var mu sync.Mutex
	a.SetSessionExtractor(func(ctx context.Context, turns []model.Message) (string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		close(started)
		<-release // hold the extraction open
		return "- summary", nil
	})
	// First trigger starts an extraction; second trigger while it
	// runs must be skipped, not queued.
	a.maybeExtractSessionMemory(context.Background(), testTranscript())
	<-started
	a.maybeExtractSessionMemory(context.Background(), testTranscript())
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 1 {
		t.Fatalf("concurrent extractions = %d, want 1 (single-flight)", n)
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for app.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := app.count(); n != 1 {
		t.Errorf("appends = %d, want 1", n)
	}
}

func TestExtractionBestEffort(t *testing.T) {
	// Extractor error: logged, nothing appended, no panic.
	a := newV2TestAgent(t)
	app := &fakeSessionAppender{}
	a.SetSessionMemoryAppender(app)
	a.SetSessionExtractor(func(ctx context.Context, turns []model.Message) (string, error) {
		return "", errors.New("provider down")
	})
	a.runSessionExtraction(context.Background(), testTranscript())
	if n := app.count(); n != 0 {
		t.Errorf("appends after extractor error = %d, want 0", n)
	}
	// Empty summary: nothing appended.
	a.SetSessionExtractor(func(ctx context.Context, turns []model.Message) (string, error) {
		return "   ", nil
	})
	a.runSessionExtraction(context.Background(), testTranscript())
	if n := app.count(); n != 0 {
		t.Errorf("appends after empty summary = %d, want 0", n)
	}
	// Append error: logged, no panic.
	app.err = errors.New("disk full")
	a.SetSessionExtractor(func(ctx context.Context, turns []model.Message) (string, error) {
		return "- something", nil
	})
	a.runSessionExtraction(context.Background(), testTranscript())
	// No appender wired at all: maybeExtract is a silent no-op.
	a2 := newV2TestAgent(t)
	a2.maybeExtractSessionMemory(context.Background(), testTranscript())
}

func TestExtractionPrompt(t *testing.T) {
	p := extractionPrompt([]model.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi", ToolCalls: []model.ToolCall{{Name: "calculator"}}},
		{Role: "tool", Content: "42", ToolCallID: "call-1-1"},
	})
	for _, want := range []string{"user: hello", "called calculator", "[tool result for call-1-1]: 42"} {
		if !strings.Contains(p, want) {
			t.Errorf("extraction prompt missing %q:\n%s", want, p)
		}
	}
	// Long messages are truncated, not inlined whole.
	long := strings.Repeat("x", maxExtractionMessageRunes+500)
	p = extractionPrompt([]model.Message{{Role: "user", Content: long}})
	if strings.Contains(p, long) {
		t.Errorf("extraction prompt inlined a %d-rune message whole", len(long))
	}
	if !strings.Contains(p, "…[truncated]") && !strings.Contains(p, "truncated") {
		t.Errorf("extraction prompt has no truncation marker")
	}
}

func TestExtractionViaRunCountsToolCalls(t *testing.T) {
	// End-to-end through Run: a tool-call turn counts toward the
	// threshold, and the extraction runs in the background without
	// breaking the turn.
	prov := &fakeProvider{chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		// First call requests a tool; second answers. The
		// extraction summariser also goes through Chat, so
		// distinguish by MaxTokens: the selector/extraction calls
		// carry a small MaxTokens, the loop calls carry none.
		if req.MaxTokens == 400 {
			return &model.ChatResponse{Message: model.Message{Role: "assistant", Content: "- tool was used"}, FinishReason: "stop"}, nil
		}
		if call == 1 {
			return &model.ChatResponse{
				Message:      model.Message{Role: "assistant", Content: "", ToolCalls: []model.ToolCall{{ID: "c1", Name: "echo", Arguments: `{"text":"hi"}`}}},
				FinishReason: "tool_calls",
			}, nil
		}
		return &model.ChatResponse{Message: model.Message{Role: "assistant", Content: "done"}, FinishReason: "stop"}, nil
	}}
	reg := tools.NewRegistry()
	_ = reg.Register(&echoTool{})
	a := newTestAgent(t, testConfig(5), prov, reg, newFakeStore())
	app := &fakeSessionAppender{}
	a.SetSessionMemoryAppender(app)
	a.SetExtractionThresholds(1, 100) // one tool call triggers
	out, err := a.Run(context.Background(), "say hi via echo")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "done" {
		t.Errorf("Run = %q, want %q", out, "done")
	}
	deadline := time.Now().Add(2 * time.Second)
	for app.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := app.count(); n != 1 {
		t.Errorf("appends after tool-call turn = %d, want 1", n)
	}
}
