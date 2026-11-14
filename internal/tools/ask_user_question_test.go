// ask_user_question tests: the tool validates its arguments, reads
// the chat from the turn context, delegates to the installed asker,
// and maps timeout/cancellation to a timeout ToolError.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/permissions"
)

// recordingAsker is a QuestionAskFunc that records its call and
// returns canned values. block, when non-nil, is closed to release
// a blocked call (for timeout/cancel tests the error is returned
// instead).
type recordingAsker struct {
	mu          sync.Mutex
	calls       int
	chatID      int64
	senderID    int64
	question    string
	options     []string
	timeout     time.Duration
	answer      string
	source      string
	optionIndex int
	err         error
}

func (f *recordingAsker) ask(ctx context.Context, chatID, senderID int64, question string, options []string, timeout time.Duration) (string, string, int, error) {
	f.mu.Lock()
	f.calls++
	f.chatID, f.senderID = chatID, senderID
	f.question = question
	f.options = append([]string(nil), options...)
	f.timeout = timeout
	answer, source, idx, err := f.answer, f.source, f.optionIndex, f.err
	f.mu.Unlock()
	return answer, source, idx, err
}

func (f *recordingAsker) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// askRegistry returns a registry with only ask_user_question,
// wired to asker.
func askRegistry(ask QuestionAskFunc) *Registry {
	r := NewRegistry()
	if err := r.Register(&askUserQuestionTool{ask: ask}); err != nil {
		panic(err)
	}
	return r
}

// askCtx returns a context carrying chat 77 / sender 77, as the
// daemon's turn handler sets it.
func askCtx() context.Context {
	return permissions.WithApprovalContext(context.Background(), 77, 77)
}

func TestAskUserQuestionDefinition(t *testing.T) {
	tool := &askUserQuestionTool{}
	def := tool.Definition()
	if def.Name != "ask_user_question" {
		t.Errorf("Definition.Name = %q, want ask_user_question", def.Name)
	}
	props, ok := def.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("Definition has no properties map")
	}
	for _, name := range []string{"question", "options", "timeout_seconds"} {
		if _, ok := props[name]; !ok {
			t.Errorf("Definition is missing parameter %q", name)
		}
	}
	required, _ := def.Parameters["required"].([]string)
	if len(required) != 1 || required[0] != "question" {
		t.Errorf("Definition required = %v, want [question]", required)
	}
	if tool.Description() == "" {
		t.Error("Description is empty")
	}
}

func TestAskUserQuestionValidation(t *testing.T) {
	fake := &recordingAsker{answer: "yes", source: "button", optionIndex: 0}
	r := askRegistry(fake.ask)
	run := func(t *testing.T, args string) (string, error) {
		t.Helper()
		return r.Execute(askCtx(), "ask_user_question", json.RawMessage(args))
	}
	// Valid: question + options + timeout reaches the asker.
	if _, err := run(t, `{"question":"Pick one?","options":["A","B"],"timeout_seconds":60}`); err != nil {
		t.Fatalf("valid call: %v", err)
	}
	if fake.callCount() != 1 || fake.question != "Pick one?" || len(fake.options) != 2 || fake.timeout != 60*time.Second {
		t.Errorf("asker got question=%q options=%v timeout=%v (calls=%d)", fake.question, fake.options, fake.timeout, fake.callCount())
	}
	// Valid: question only (free text, default timeout).
	if _, err := run(t, `{"question":"Why?"}`); err != nil {
		t.Fatalf("question-only call: %v", err)
	}
	if fake.timeout != DefaultQuestionTimeout {
		t.Errorf("default timeout = %v, want %v", fake.timeout, DefaultQuestionTimeout)
	}
	// Invalid: missing question.
	if _, err := run(t, `{"options":["A"]}`); err == nil {
		t.Error("missing question: got no error, want invalid_args")
	} else if !strings.Contains(err.Error(), "invalid_args") {
		t.Errorf("missing question: error %v, want invalid_args", err)
	}
	// Invalid: empty question.
	if _, err := run(t, `{"question":"   "}`); err == nil {
		t.Error("empty question: got no error, want invalid_args")
	} else if !strings.Contains(err.Error(), "invalid_args") {
		t.Errorf("empty question: error %v, want invalid_args", err)
	}
	// Invalid: question too long.
	if _, err := run(t, `{"question":"`+strings.Repeat("x", MaxQuestionRunes+1)+`"}`); err == nil {
		t.Error("over-long question: got no error, want invalid_args")
	} else if !strings.Contains(err.Error(), "invalid_args") {
		t.Errorf("over-long question: error %v, want invalid_args", err)
	}
	// Invalid: too many options.
	opts := make([]string, MaxQuestionOptions+1)
	for i := range opts {
		opts[i] = "o"
	}
	b, _ := json.Marshal(map[string]any{"question": "Q?", "options": opts})
	if _, err := run(t, string(b)); err == nil {
		t.Error("too many options: got no error, want invalid_args")
	} else if !strings.Contains(err.Error(), "invalid_args") {
		t.Errorf("too many options: error %v, want invalid_args", err)
	}
	// Invalid: empty option.
	if _, err := run(t, `{"question":"Q?","options":["A",""]}`); err == nil {
		t.Error("empty option: got no error, want invalid_args")
	} else if !strings.Contains(err.Error(), "invalid_args") {
		t.Errorf("empty option: error %v, want invalid_args", err)
	}
	// Invalid: option too long.
	if _, err := run(t, `{"question":"Q?","options":["`+strings.Repeat("y", MaxOptionRunes+1)+`"]}`); err == nil {
		t.Error("over-long option: got no error, want invalid_args")
	} else if !strings.Contains(err.Error(), "invalid_args") {
		t.Errorf("over-long option: error %v, want invalid_args", err)
	}
	if n := fake.callCount(); n != 2 {
		t.Errorf("asker called %d times, want 2 (only the valid calls reach it)", n)
	}
}

func TestAskUserQuestionTimeoutClamping(t *testing.T) {
	fake := &recordingAsker{answer: "ok", source: "text", optionIndex: -1}
	r := askRegistry(fake.ask)
	cases := []struct {
		seconds int
		want    time.Duration
	}{
		{0, DefaultQuestionTimeout},
		{1, MinQuestionTimeout},
		{60, 60 * time.Second},
		{99999, MaxQuestionTimeout},
	}
	for _, tc := range cases {
		args, _ := json.Marshal(map[string]any{"question": "Q?", "timeout_seconds": tc.seconds})
		if _, err := r.Execute(askCtx(), "ask_user_question", args); err != nil {
			t.Fatalf("timeout_seconds=%d: %v", tc.seconds, err)
		}
		if fake.timeout != tc.want {
			t.Errorf("timeout_seconds=%d: asker got %v, want %v", tc.seconds, fake.timeout, tc.want)
		}
	}
}

func TestAskUserQuestionNoAsker(t *testing.T) {
	r := askRegistry(nil)
	_, err := r.Execute(askCtx(), "ask_user_question", json.RawMessage(`{"question":"Q?"}`))
	if err == nil {
		t.Fatal("nil asker: got no error, want a clear not-available error")
	}
	if !strings.Contains(err.Error(), "not available") {
		t.Errorf("nil asker: error %v, want it to say not available", err)
	}
}

func TestAskUserQuestionNoChat(t *testing.T) {
	fake := &recordingAsker{answer: "ok", source: "text", optionIndex: -1}
	r := askRegistry(fake.ask)
	// A bare context (local CLI run): no chat, so nobody to ask.
	_, err := r.Execute(context.Background(), "ask_user_question", json.RawMessage(`{"question":"Q?"}`))
	if err == nil {
		t.Fatal("no chat in ctx: got no error, want a clear error")
	}
	if !strings.Contains(err.Error(), "needs a Telegram chat") {
		t.Errorf("no chat in ctx: error %v, want it to say it needs a Telegram chat", err)
	}
	if fake.callCount() != 0 {
		t.Error("asker was called with no chat, want it never reached")
	}
}

func TestAskUserQuestionTimeoutAndCancel(t *testing.T) {
	// Timeout from the asker maps to a timeout ToolError.
	fake := &recordingAsker{err: context.DeadlineExceeded}
	r := askRegistry(fake.ask)
	_, err := r.Execute(askCtx(), "ask_user_question", json.RawMessage(`{"question":"Q?"}`))
	if err == nil {
		t.Fatal("DeadlineExceeded: got no error, want a timeout ToolError")
	}
	var toolErr *ToolError
	if !errors.As(err, &toolErr) || toolErr.Kind != "timeout" {
		t.Errorf("DeadlineExceeded: error %v, want kind timeout", err)
	}
	// Cancellation maps the same way (the wait ended without an
	// answer); the message must say cancelled, not timed out.
	fake.err = context.Canceled
	_, err = r.Execute(askCtx(), "ask_user_question", json.RawMessage(`{"question":"Q?"}`))
	if err == nil {
		t.Fatal("Canceled: got no error, want a timeout ToolError")
	}
	if !errors.As(err, &toolErr) || toolErr.Kind != "timeout" {
		t.Errorf("Canceled: error %v, want kind timeout", err)
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("Canceled: error %v, want it to say cancelled", err)
	}
}

func TestAskUserQuestionResultShape(t *testing.T) {
	fake := &recordingAsker{answer: "B", source: "button", optionIndex: 1}
	r := askRegistry(fake.ask)
	out, err := r.Execute(askCtx(), "ask_user_question", json.RawMessage(`{"question":"Pick?","options":["A","B"]}`))
	if err != nil {
		t.Fatalf("button answer: %v", err)
	}
	var res questionAnswer
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("button answer is not JSON: %q (%v)", out, err)
	}
	if res.Answer != "B" || res.Source != "button" || res.OptionIndex != 1 {
		t.Errorf("button answer = %+v, want {B button 1}", res)
	}
	fake.answer, fake.source, fake.optionIndex = "typed it", "text", -1
	out, err = r.Execute(askCtx(), "ask_user_question", json.RawMessage(`{"question":"Why?"}`))
	if err != nil {
		t.Fatalf("text answer: %v", err)
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("text answer is not JSON: %q (%v)", out, err)
	}
	if res.Answer != "typed it" || res.Source != "text" || res.OptionIndex != -1 {
		t.Errorf("text answer = %+v, want {typed it text -1}", res)
	}
}

func TestToolInstallQuestionAsker(t *testing.T) {
	// The framework installs the transport on the tool itself
	// (InstallQuestionAsker), not through the registry: the
	// registry-level SetQuestionAsker/QuestionAsker seam is
	// deleted. Same guarantees, new mechanism.
	// Nil-safe on a nil tool and a nil fn.
	var nilTool *askUserQuestionTool
	nilTool.InstallQuestionAsker(func(context.Context, int64, int64, string, []string, time.Duration) (string, string, int, error) {
		return "", "", -1, nil
	})
	// InstallQuestionAsker(nil) keeps the previous asker.
	fake := &recordingAsker{answer: "kept", source: "button", optionIndex: 1}
	r := askRegistry(fake.ask)
	tool, ok := r.Get("ask_user_question")
	if !ok {
		t.Fatal("askRegistry has no ask_user_question tool")
	}
	tool.(QuestionAskerInstaller).InstallQuestionAsker(nil)
	out, err := r.Execute(askCtx(), "ask_user_question", json.RawMessage(`{"question":"Q?"}`))
	if err != nil {
		t.Fatalf("InstallQuestionAsker(nil) cleared the asker: %v", err)
	}
	if !strings.Contains(out, `"kept"`) {
		t.Errorf("InstallQuestionAsker(nil) cleared the asker, got %s", out)
	}
	// Round-trip: install a fake asker on a tool that had none
	// and the tool uses it.
	fake2 := &recordingAsker{answer: "installed", source: "text", optionIndex: -1}
	r2 := askRegistry(nil)
	tool2, ok := r2.Get("ask_user_question")
	if !ok {
		t.Fatal("askRegistry has no ask_user_question tool")
	}
	tool2.(QuestionAskerInstaller).InstallQuestionAsker(fake2.ask)
	out, err = r2.Execute(askCtx(), "ask_user_question", json.RawMessage(`{"question":"Q?"}`))
	if err != nil {
		t.Fatalf("installed asker: %v", err)
	}
	if !strings.Contains(out, `"installed"`) {
		t.Errorf("installed asker not used, got %s", out)
	}
	// Framework-level: the same install loop WireTelegram runs
	// (install on every tool implementing QuestionAskerInstaller)
	// is a no-op on a registry without the tool, and reaches the
	// tool on a registry that has it.
	empty := NewRegistry()
	fake3 := &recordingAsker{answer: "framework", source: "text", optionIndex: -1}
	for _, tool := range empty.List() {
		if inst, ok := tool.(QuestionAskerInstaller); ok {
			inst.InstallQuestionAsker(fake3.ask)
		}
	}
	r3 := askRegistry(nil)
	for _, tool := range r3.List() {
		if inst, ok := tool.(QuestionAskerInstaller); ok {
			inst.InstallQuestionAsker(fake3.ask)
		}
	}
	out, err = r3.Execute(askCtx(), "ask_user_question", json.RawMessage(`{"question":"Q?"}`))
	if err != nil {
		t.Fatalf("framework install: %v", err)
	}
	if !strings.Contains(out, `"framework"`) {
		t.Errorf("framework install did not reach the tool, got %s", out)
	}
}

func TestDefaultRegistryHasAskUserQuestion(t *testing.T) {
	r, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	tool, ok := r.Get("ask_user_question")
	if !ok {
		t.Fatal("DefaultRegistry has no ask_user_question")
	}
	if tool.Name() != "ask_user_question" {
		t.Errorf("tool Name() = %q", tool.Name())
	}
	// The default registry has no transport: the tool must fail
	// cleanly (never hang) when the model calls it there.
	_, err = r.Execute(askCtx(), "ask_user_question", json.RawMessage(`{"question":"Q?"}`))
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("DefaultRegistry call: error %v, want not-available", err)
	}
	// It is a safe tool: the permission gate must never prompt
	// for approval to ask the user a question.
	if permissions.Classify("ask_user_question") != permissions.ClassSafe {
		t.Error("ask_user_question is not ClassSafe: the gate would prompt before the tool can ask")
	}
}
