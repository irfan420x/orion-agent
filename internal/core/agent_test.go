package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/tools"
)

// --- Fake provider -------------------------------------------------------

// fakeProvider is a scriptable model.Provider for tests. chatFn decides
// each Chat reply by call number; streamFn decides each Stream reply
// by call number (its content is emitted as one chunk); when streamFn
// is nil, Stream replays streamChunks and returns their joined text.
type fakeProvider struct {
	mu           sync.Mutex
	chatCalls    int
	chatFn       func(call int, req model.ChatRequest) (*model.ChatResponse, error)
	streamFn     func(call int, req model.ChatRequest) (*model.ChatResponse, error)
	streamChunks []string
	streamErr    error
	streamCalls  int
	lastReq      model.ChatRequest
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Chat(_ context.Context, req model.ChatRequest) (*model.ChatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chatCalls++
	f.lastReq = req
	if f.chatFn != nil {
		return f.chatFn(f.chatCalls, req)
	}
	return &model.ChatResponse{
		Message:      model.Message{Role: "assistant", Content: "canned answer"},
		FinishReason: "stop",
	}, nil
}

func (f *fakeProvider) Stream(_ context.Context, req model.ChatRequest, fn func(chunk string) error) (*model.ChatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.streamCalls++
	f.lastReq = req
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	if f.streamFn != nil {
		resp, err := f.streamFn(f.streamCalls, req)
		if err != nil {
			return nil, err
		}
		if resp != nil && resp.Message.Content != "" {
			if err := fn(resp.Message.Content); err != nil {
				return nil, err
			}
		}
		return resp, nil
	}
	for _, c := range f.streamChunks {
		if err := fn(c); err != nil {
			return nil, err
		}
	}
	return &model.ChatResponse{
		Message:      model.Message{Role: "assistant", Content: strings.Join(f.streamChunks, "")},
		FinishReason: "stop",
	}, nil
}

func (f *fakeProvider) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chatCalls
}

func (f *fakeProvider) streamCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streamCalls
}

// textResponse builds a plain assistant reply.
func textResponse(text string) *model.ChatResponse {
	return &model.ChatResponse{
		Message:      model.Message{Role: "assistant", Content: text},
		FinishReason: "stop",
	}
}

// toolCallResponse builds an assistant reply requesting one tool call.
func toolCallResponse(id, name, args string) *model.ChatResponse {
	return &model.ChatResponse{
		Message: model.Message{
			Role:      "assistant",
			Content:   "",
			ToolCalls: []model.ToolCall{{ID: id, Name: name, Arguments: args}},
		},
		FinishReason: "tool_calls",
	}
}

// --- Fake memory store ---------------------------------------------------
// In-test fake so core tests never depend on another package's FileStore.

// fakeStore is an in-memory memory.Store.
type fakeStore struct {
	mu       sync.Mutex
	facts    map[string]memory.Fact
	messages []model.Message
	saved    bool
	closed   bool
}

func newFakeStore() *fakeStore { return &fakeStore{facts: map[string]memory.Fact{}} }

func (s *fakeStore) AddFact(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.facts[key] = memory.Fact{Key: key, Value: value}
	return nil
}

func (s *fakeStore) GetFact(key string) (memory.Fact, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.facts[key]
	return f, ok, nil
}

func (s *fakeStore) DeleteFact(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.facts, key)
	return nil
}

func (s *fakeStore) SearchFacts(query string) ([]memory.Fact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []memory.Fact
	for _, f := range s.facts {
		if strings.Contains(f.Key, query) || strings.Contains(f.Value, query) {
			out = append(out, f)
		}
	}
	return out, nil
}

func (s *fakeStore) ListFacts() ([]memory.Fact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]memory.Fact, 0, len(s.facts))
	for _, f := range s.facts {
		out = append(out, f)
	}
	return out, nil
}

func (s *fakeStore) AddMessage(msg model.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, msg)
	return nil
}

func (s *fakeStore) RecentMessages(n int) ([]model.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= 0 || n >= len(s.messages) {
		return append([]model.Message(nil), s.messages...), nil
	}
	return append([]model.Message(nil), s.messages[len(s.messages)-n:]...), nil
}

func (s *fakeStore) ClearMessages() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = nil
	return nil
}

func (s *fakeStore) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = true
	return nil
}

func (s *fakeStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *fakeStore) allMessages() []model.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Message(nil), s.messages...)
}

// --- Fake tools ----------------------------------------------------------

// echoTool echoes the "text" argument back, recording each call.
type echoTool struct {
	mu    sync.Mutex
	calls int
	last  string
}

func (t *echoTool) Name() string        { return "echo" }
func (t *echoTool) Description() string { return "Echoes the text argument back." }
func (t *echoTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        "echo",
		Description: t.Description(),
		Parameters:  map[string]any{"type": "object"},
	}
}
func (t *echoTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	var parsed struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(args, &parsed); err != nil {
		return "", err
	}
	t.last = parsed.Text
	return "echo:" + parsed.Text, nil
}

func (t *echoTool) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

// failingTool always fails, to exercise the feed-error-back path.
type failingTool struct{}

func (failingTool) Name() string        { return "boom" }
func (failingTool) Description() string { return "Always fails." }
func (failingTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: "boom", Description: "Always fails.", Parameters: map[string]any{"type": "object"}}
}
func (failingTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	return "", errors.New("boom exploded")
}

// --- Helpers -------------------------------------------------------------

func testConfig(maxSteps int) *config.Config {
	cfg := config.Default()
	cfg.MaxSteps = maxSteps
	cfg.TimeoutSeconds = 30
	return cfg
}

func newTestAgent(t *testing.T, cfg *config.Config, p model.Provider, reg *tools.Registry, mem memory.Store) *Agent {
	t.Helper()
	a, err := NewAgent(cfg, p, reg, mem)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	a.SetLogger(log.New(io.Discard, "", 0))
	return a
}

// --- Tests ---------------------------------------------------------------

func TestNewAgentRejectsNilDeps(t *testing.T) {
	cfg := testConfig(5)
	reg := tools.NewRegistry()
	mem := newFakeStore()
	prov := &fakeProvider{}

	if _, err := NewAgent(cfg, nil, reg, mem); err == nil {
		t.Error("NewAgent(nil provider): expected error, got nil")
	}
	if _, err := NewAgent(cfg, prov, nil, mem); err == nil {
		t.Error("NewAgent(nil tools): expected error, got nil")
	}
	if _, err := NewAgent(cfg, prov, reg, nil); err == nil {
		t.Error("NewAgent(nil memory): expected error, got nil")
	}
	if _, err := NewAgent(cfg, prov, reg, mem); err != nil {
		t.Errorf("NewAgent(valid deps): unexpected error: %v", err)
	}
}

func TestSystemPrompt(t *testing.T) {
	a := newTestAgent(t, testConfig(5), &fakeProvider{}, tools.NewRegistry(), newFakeStore())
	p := a.SystemPrompt()
	for _, want := range []string{"Orion", "local-first", "clarification", "destructive", "blocked"} {
		if !strings.Contains(strings.ToLower(p), strings.ToLower(want)) {
			t.Errorf("SystemPrompt missing %q: %q", want, p)
		}
	}
	// The one-tool-call-per-turn rule exists because small models
	// otherwise issue dependent calls in one turn (e.g. file_write
	// with a value the calculator has not returned yet). It must stay.
	if !strings.Contains(strings.ToLower(p), "at most one tool call per message") {
		t.Errorf("SystemPrompt must state the one-tool-call-per-message rule: %q", p)
	}
}

func TestRunDirectAnswer(t *testing.T) {
	prov := &fakeProvider{}
	mem := newFakeStore()
	a := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), mem)

	out, err := a.Run(context.Background(), "hello there")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "canned answer" {
		t.Errorf("Run = %q, want %q", out, "canned answer")
	}
	if got := prov.calls(); got != 1 {
		t.Errorf("provider Chat calls = %d, want 1", got)
	}

	msgs := mem.allMessages()
	if len(msgs) != 2 {
		t.Fatalf("memory messages = %d, want 2 (user + assistant): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || msgs[0].Content != "hello there" {
		t.Errorf("memory[0] = %+v, want user/hello there", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Content != "canned answer" {
		t.Errorf("memory[1] = %+v, want assistant/canned answer", msgs[1])
	}
}

func TestRunIncludesRecentMemory(t *testing.T) {
	prov := &fakeProvider{}
	mem := newFakeStore()
	if err := mem.AddMessage(model.Message{Role: "user", Content: "earlier question"}); err != nil {
		t.Fatal(err)
	}
	if err := mem.AddMessage(model.Message{Role: "assistant", Content: "earlier answer"}); err != nil {
		t.Fatal(err)
	}
	a := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), mem)

	if _, err := a.Run(context.Background(), "follow up"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sent := prov.lastReq.Messages
	if len(sent) < 4 {
		t.Fatalf("provider got %d messages, want >= 4 (system + 2 history + user): %+v", len(sent), sent)
	}
	if sent[0].Role != "system" {
		t.Errorf("messages[0].Role = %q, want system", sent[0].Role)
	}
	if sent[1].Content != "earlier question" || sent[2].Content != "earlier answer" {
		t.Errorf("history not included in order: %+v", sent)
	}
	if sent[len(sent)-1].Content != "follow up" {
		t.Errorf("last message = %+v, want user/follow up", sent[len(sent)-1])
	}
}

// TestRunPassesTemperatureThrough: a configured temperature reaches
// the provider on every call; unset, the request leaves it nil so the
// provider uses its own default.
func TestRunPassesTemperatureThrough(t *testing.T) {
	temp := 0.0
	cfg := testConfig(5)
	cfg.Temperature = &temp
	prov := &fakeProvider{}
	a := newTestAgent(t, cfg, prov, tools.NewRegistry(), newFakeStore())
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if prov.lastReq.Temperature == nil || *prov.lastReq.Temperature != 0 {
		t.Errorf("request Temperature = %v, want 0", prov.lastReq.Temperature)
	}

	prov2 := &fakeProvider{}
	a2 := newTestAgent(t, testConfig(5), prov2, tools.NewRegistry(), newFakeStore())
	if _, err := a2.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if prov2.lastReq.Temperature != nil {
		t.Errorf("request Temperature = %v, want nil (provider default)", prov2.lastReq.Temperature)
	}
}

func TestRunExecutesToolCall(t *testing.T) {
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
			return textResponse("the tool said echo:hi"), nil
		},
	}
	mem := newFakeStore()
	a := newTestAgent(t, testConfig(5), prov, reg, mem)

	out, err := a.Run(context.Background(), "say hi via echo")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "the tool said echo:hi" {
		t.Errorf("Run = %q, want follow-up answer", out)
	}
	if got := echo.callCount(); got != 1 {
		t.Errorf("echo calls = %d, want 1", got)
	}
	if echo.last != "hi" {
		t.Errorf("echo last arg = %q, want %q", echo.last, "hi")
	}
	if got := prov.calls(); got != 2 {
		t.Errorf("provider Chat calls = %d, want 2", got)
	}
	// The tool result must be in memory as a tool-role message.
	found := false
	for _, m := range mem.allMessages() {
		if m.Role == "tool" && m.ToolCallID == "call-1" && m.Content == "echo:hi" {
			found = true
		}
	}
	if !found {
		t.Errorf("tool result message not in memory: %+v", mem.allMessages())
	}
}

// TestRunExecutesOnlyFirstToolCallPerTurn: a model that issues several
// tool calls in one turn must not have them all executed at once —
// later calls may depend on results that do not exist yet (a small
// model once wrote a hallucinated number to a file instead of the
// calculator's answer). Only the first call runs; the model re-plans
// the rest on the next turn, with the result in context.
func TestRunExecutesOnlyFirstToolCallPerTurn(t *testing.T) {
	echo := &echoTool{}
	reg := tools.NewRegistry()
	if err := reg.Register(echo); err != nil {
		t.Fatalf("Register echo: %v", err)
	}
	prov := &fakeProvider{
		chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
			switch call {
			case 1:
				return &model.ChatResponse{
					Message: model.Message{
						Role:    "assistant",
						Content: "",
						ToolCalls: []model.ToolCall{
							{ID: "call-1", Name: "echo", Arguments: `{"text":"first"}`},
							{ID: "call-2", Name: "echo", Arguments: `{"text":"second"}`},
						},
					},
					FinishReason: "tool_calls",
				}, nil
			case 2:
				// Re-plan the deferred second call now that the
				// first result is in context.
				return toolCallResponse("call-3", "echo", `{"text":"second"}`), nil
			default:
				return textResponse("done"), nil
			}
		},
	}
	mem := newFakeStore()
	a := newTestAgent(t, testConfig(5), prov, reg, mem)

	out, err := a.Run(context.Background(), "echo twice")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "done" {
		t.Errorf("Run = %q, want %q", out, "done")
	}
	if got := prov.calls(); got != 3 {
		t.Errorf("provider Chat calls = %d, want 3 (first call, re-planned call, final answer)", got)
	}
	if got := echo.callCount(); got != 2 {
		t.Errorf("echo calls = %d, want 2", got)
	}
	// The deferred call-2 must never have executed: no tool result
	// may carry its ID, and the turn-1 assistant message must record
	// only the executed call.
	for _, m := range mem.allMessages() {
		if m.Role == "tool" && m.ToolCallID == "call-2" {
			t.Errorf("deferred tool call call-2 was executed: %+v", m)
		}
		if m.Role == "assistant" && len(m.ToolCalls) > 1 {
			t.Errorf("assistant message records %d tool calls, want at most 1: %+v", len(m.ToolCalls), m.ToolCalls)
		}
	}
}

// TestRunRemindsModelOfDeferredToolCalls: if the model issues several
// calls in one turn and then answers directly instead of re-planning
// the deferred ones, the loop reminds it once to complete them before
// finishing. The premature answer is discarded, not recorded.
func TestRunRemindsModelOfDeferredToolCalls(t *testing.T) {
	echo := &echoTool{}
	reg := tools.NewRegistry()
	if err := reg.Register(echo); err != nil {
		t.Fatalf("Register echo: %v", err)
	}
	var thirdReq model.ChatRequest
	prov := &fakeProvider{
		chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
			switch call {
			case 1:
				return &model.ChatResponse{
					Message: model.Message{
						Role: "assistant",
						ToolCalls: []model.ToolCall{
							{ID: "call-1", Name: "echo", Arguments: `{"text":"first"}`},
							{ID: "call-2", Name: "echo", Arguments: `{"text":"second"}`},
						},
					},
					FinishReason: "tool_calls",
				}, nil
			case 2:
				// Model answers directly instead of re-planning
				// the deferred call.
				return textResponse("early answer"), nil
			case 3:
				thirdReq = req
				// After the reminder, it completes the deferred call.
				return toolCallResponse("call-3", "echo", `{"text":"second"}`), nil
			default:
				return textResponse("done"), nil
			}
		},
	}
	mem := newFakeStore()
	a := newTestAgent(t, testConfig(6), prov, reg, mem)

	out, err := a.Run(context.Background(), "echo twice")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "done" {
		t.Errorf("Run = %q, want %q", out, "done")
	}
	if got := prov.calls(); got != 4 {
		t.Errorf("provider Chat calls = %d, want 4 (first call, early answer, reminded call, final answer)", got)
	}
	if got := echo.callCount(); got != 2 {
		t.Errorf("echo calls = %d, want 2", got)
	}
	// The third request must carry the reminder about the pending call.
	reminded := false
	for _, m := range thirdReq.Messages {
		if m.Role == "user" && strings.Contains(m.Content, "pending") && strings.Contains(m.Content, "echo") {
			reminded = true
		}
	}
	if !reminded {
		t.Errorf("no deferred-call reminder in third request: %+v", thirdReq.Messages)
	}
	// The premature answer must not be recorded in memory, and the
	// deferred call-2 must never have executed.
	for _, m := range mem.allMessages() {
		if m.Role == "assistant" && m.Content == "early answer" {
			t.Errorf("premature answer was recorded in memory: %+v", m)
		}
		if m.Role == "tool" && m.ToolCallID == "call-2" {
			t.Errorf("deferred tool call call-2 was executed: %+v", m)
		}
	}
}

func TestRunFeedsToolErrorBack(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(failingTool{}); err != nil {
		t.Fatalf("Register boom: %v", err)
	}
	var secondReq model.ChatRequest
	prov := &fakeProvider{
		chatFn: func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
			if call == 1 {
				return toolCallResponse("call-1", "boom", `{}`), nil
			}
			secondReq = req
			return textResponse("recovered after error"), nil
		},
	}
	a := newTestAgent(t, testConfig(5), prov, reg, newFakeStore())

	out, err := a.Run(context.Background(), "run boom")
	if err != nil {
		t.Fatalf("Run should not abort on tool error, got: %v", err)
	}
	if out != "recovered after error" {
		t.Errorf("Run = %q, want recovery answer", out)
	}
	// The second request must carry the tool error as a tool result.
	fedBack := false
	for _, m := range secondReq.Messages {
		if m.Role == "tool" && strings.Contains(m.Content, "boom exploded") {
			fedBack = true
		}
	}
	if !fedBack {
		t.Errorf("tool error not fed back to model: %+v", secondReq.Messages)
	}
}

func TestRunRespectsMaxSteps(t *testing.T) {
	prov := &fakeProvider{
		chatFn: func(_ int, _ model.ChatRequest) (*model.ChatResponse, error) {
			return toolCallResponse("c1", "echo", `{"text":"x"}`), nil
		},
	}
	reg := tools.NewRegistry()
	if err := reg.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	a := newTestAgent(t, testConfig(1), prov, reg, newFakeStore())

	_, err := a.Run(context.Background(), "loop forever")
	if err == nil {
		t.Fatal("Run with MaxSteps=1 and repeated tool calls: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "max steps exceeded") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "max steps exceeded")
	}
	// The error must carry the partial transcript: the user input and
	// the tool call the model made before the budget ran out.
	var maxErr *MaxStepsError
	if !errors.As(err, &maxErr) {
		t.Fatalf("error = %T, want *MaxStepsError", err)
	}
	transcript := maxErr.Transcript()
	if len(transcript) < 3 {
		t.Fatalf("partial transcript = %d messages, want >= 3 (system + user + assistant tool call): %+v", len(transcript), transcript)
	}
	foundToolCall := false
	for _, m := range transcript {
		if m.Role == "assistant" && len(m.ToolCalls) == 1 && m.ToolCalls[0].Name == "echo" {
			foundToolCall = true
		}
	}
	if !foundToolCall {
		t.Errorf("partial transcript has no assistant tool-call message: %+v", transcript)
	}
}

func TestRunHonoursCancelledContext(t *testing.T) {
	prov := &fakeProvider{}
	a := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), newFakeStore())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.Run(ctx, "hello")
	if err == nil {
		t.Fatal("Run with cancelled ctx: expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
	if got := prov.calls(); got != 0 {
		t.Errorf("provider Chat calls = %d, want 0 (cancelled before start)", got)
	}
}

func TestRunEmptyInput(t *testing.T) {
	a := newTestAgent(t, testConfig(5), &fakeProvider{}, tools.NewRegistry(), newFakeStore())
	if _, err := a.Run(context.Background(), "   "); err == nil {
		t.Error("Run(empty input): expected error, got nil")
	}
}

func TestRunProviderError(t *testing.T) {
	prov := &fakeProvider{
		chatFn: func(_ int, _ model.ChatRequest) (*model.ChatResponse, error) {
			return nil, fmt.Errorf("upstream down")
		},
	}
	a := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), newFakeStore())
	if _, err := a.Run(context.Background(), "hello"); err == nil {
		t.Error("Run(provider error): expected error, got nil")
	} else if !strings.Contains(err.Error(), "upstream down") {
		t.Errorf("error = %q, want wrapped provider error", err.Error())
	}
}

func TestRunStreamDeliversChunks(t *testing.T) {
	prov := &fakeProvider{streamChunks: []string{"Hel", "lo", "!"}}
	a := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), newFakeStore())

	var got []string
	out, err := a.RunStream(context.Background(), "say hello", func(chunk string) error {
		got = append(got, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if out != "Hello!" {
		t.Errorf("RunStream = %q, want %q", out, "Hello!")
	}
	if strings.Join(got, "") != "Hello!" {
		t.Errorf("chunks = %q, want them to join to Hello!", got)
	}
	if len(got) != 3 {
		t.Errorf("got %d chunks, want 3", len(got))
	}
	// Streaming must be one provider call per step: Chat is never used.
	if got := prov.calls(); got != 0 {
		t.Errorf("provider Chat calls = %d, want 0 (RunStream must use Stream only)", got)
	}
	if got := prov.streamCallCount(); got != 1 {
		t.Errorf("provider Stream calls = %d, want 1", got)
	}
}

func TestRunStreamWithToolCallThenStream(t *testing.T) {
	echo := &echoTool{}
	reg := tools.NewRegistry()
	if err := reg.Register(echo); err != nil {
		t.Fatal(err)
	}
	// Stream drives every step: call 1 is a tool-call turn, call 2
	// the final answer. Chat must never be called.
	prov := &fakeProvider{
		streamFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
			if call == 1 {
				return toolCallResponse("call-1", "echo", `{"text":"yo"}`), nil
			}
			return textResponse("the tool said echo:yo"), nil
		},
	}
	mem := newFakeStore()
	a := newTestAgent(t, testConfig(5), prov, reg, mem)

	var got []string
	out, err := a.RunStream(context.Background(), "echo yo", func(chunk string) error {
		got = append(got, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if out != "the tool said echo:yo" {
		t.Errorf("RunStream = %q, want %q", out, "the tool said echo:yo")
	}
	if strings.Join(got, "") != "the tool said echo:yo" {
		t.Errorf("chunks = %q, want the final answer streamed", got)
	}
	if echo.callCount() != 1 {
		t.Errorf("echo calls = %d, want 1", echo.callCount())
	}
	if got := prov.calls(); got != 0 {
		t.Errorf("provider Chat calls = %d, want 0 (RunStream must use Stream only)", got)
	}
	if got := prov.streamCallCount(); got != 2 {
		t.Errorf("provider Stream calls = %d, want 2 (one per step)", got)
	}
	// The streamed final answer must be stored in memory.
	msgs := mem.allMessages()
	if len(msgs) == 0 || msgs[len(msgs)-1].Content != "the tool said echo:yo" {
		t.Errorf("last memory message = %+v, want streamed final answer", msgs)
	}
}

func TestRunStreamNilCallback(t *testing.T) {
	a := newTestAgent(t, testConfig(5), &fakeProvider{}, tools.NewRegistry(), newFakeStore())
	if _, err := a.RunStream(context.Background(), "hi", nil); err == nil {
		t.Error("RunStream(nil fn): expected error, got nil")
	}
}

func TestShutdownSavesAndCloses(t *testing.T) {
	mem := newFakeStore()
	a := newTestAgent(t, testConfig(5), &fakeProvider{}, tools.NewRegistry(), mem)
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := a.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !mem.saved {
		t.Error("Shutdown did not Save memory")
	}
	if !mem.closed {
		t.Error("Shutdown did not Close memory")
	}
}

func TestIsBlockedCommandDelegates(t *testing.T) {
	if !IsBlockedCommand("rm -rf /") {
		t.Error("IsBlockedCommand(rm -rf /) = false, want true")
	}
	if IsBlockedCommand("ls -la") {
		t.Error("IsBlockedCommand(ls -la) = true, want false")
	}
}
