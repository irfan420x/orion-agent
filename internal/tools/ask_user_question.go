// Package tools — ask_user_question: ask the user a question and wait
// for the answer.
//
// The agent calls this tool when it needs clarification mid-task
// instead of guessing: the question goes to the user as a Telegram
// message (inline-button options when the model supplies them, free
// text otherwise) and the tool call BLOCKS until the user answers,
// the timeout expires, or the turn is cancelled. The answer comes
// back as the tool result and the agent continues.
//
// Transport lives outside this package: the tool holds a
// QuestionAskFunc, installed by the daemon after it builds the
// Telegram gateway (Registry.SetQuestionAsker, following the
// PlanningStore seam). The Telegram implementation is in
// internal/messaging (TelegramQuestionAsker), which reuses the
// approval-callback machinery — the same callback_query dispatch,
// the same requester-or-owner sender check, the same expiry-edit
// pattern — instead of a second parallel callback system. A nil
// asker (local CLI runs, which have no Telegram chat) makes the
// tool fail cleanly with "not available" instead of hanging.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
)

// Question timeouts and limits.
//
// The default (5 minutes) gives the user time to read, think and
// answer — longer than the 90s approval window, because a question
// needs a considered answer where an approval needs only a tap.
// The bounds keep a model from asking for an absurd wait (a 24h
// block would hold the turn, the chat lock and a turn slot) or a
// useless one (a 1s window nobody can answer in).
const (
	// DefaultQuestionTimeout is the wait when timeout_seconds is
	// absent or zero.
	DefaultQuestionTimeout = 5 * time.Minute
	// MinQuestionTimeout is the shortest wait the tool accepts
	// (shorter values are clamped up to it).
	MinQuestionTimeout = 10 * time.Second
	// MaxQuestionTimeout is the longest wait the tool accepts
	// (longer values are clamped down to it).
	MaxQuestionTimeout = 30 * time.Minute
	// MaxQuestionOptions is the most options one question may
	// offer. More would make the keyboard unreadable and the
	// question message oversized.
	MaxQuestionOptions = 10
	// MaxQuestionRunes caps the question text (Telegram messages
	// cap at 4096 runes; the header and instructions take ~100).
	MaxQuestionRunes = 3500
	// MaxOptionRunes caps one option's button text.
	MaxOptionRunes = 200
)

// QuestionAskFunc asks the user a question on the given chat and
// blocks until the user answers, the timeout expires, or ctx is
// cancelled. It returns the answer text, the source ("button" when
// the user tapped an option, "text" when they typed a reply), and
// the tapped option index (0-based; -1 for a typed reply). On
// timeout it returns an error wrapping context.DeadlineExceeded;
// on cancellation it returns ctx.Err().
type QuestionAskFunc func(ctx context.Context, chatID, senderID int64, question string, options []string, timeout time.Duration) (answer, source string, optionIndex int, err error)

type askUserQuestionTool struct {
	ask QuestionAskFunc
}

func (t *askUserQuestionTool) Name() string { return "ask_user_question" }

func (t *askUserQuestionTool) Description() string {
	return "Ask the user a clarifying question and wait for their answer instead of guessing. Use it when the task cannot proceed safely without the user's choice or information (ambiguous request, missing detail, a choice between options). The question is sent to the user as a Telegram message; if you supply options they appear as tap buttons, otherwise the user replies with free text. The call blocks until the user answers (default 5 minutes, set timeout_seconds to change it, 10s-30m), then returns the answer as JSON {\"answer\", \"source\" (\"button\" or \"text\"), \"option_index\" (-1 for a typed reply)}. On timeout it returns a timeout error — do not retry the same question immediately; proceed with your best judgement or explain you could not reach the user. Only available in a Telegram chat; in a local run it fails with a clear error."
}

func (t *askUserQuestionTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"question": map[string]any{"type": "string", "description": "The question to ask the user, e.g. \"Which database should I use for the new service?\". Must not be empty."},
				"options": map[string]any{
					"type":        "array",
					"description": "Optional answer options shown as tap buttons (2-10, each a short label). Omit or pass an empty list for a free-text answer.",
					"items":       map[string]any{"type": "string"},
				},
				"timeout_seconds": map[string]any{"type": "integer", "description": "How long to wait for an answer, in seconds (10-1800). 0 or absent means the 5-minute default."},
			},
			"required": []string{"question"},
		},
	}
}

type askUserQuestionArgs struct {
	Question       string   `json:"question"`
	Options        []string `json:"options,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

// questionAnswer is the tool result: the user's answer, how it
// arrived, and which option was tapped (-1 for a typed reply).
type questionAnswer struct {
	Answer      string `json:"answer"`
	Source      string `json:"source"`
	OptionIndex int    `json:"option_index"`
}

func (t *askUserQuestionTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a askUserQuestionArgs
	if err := decodeArgs(t.Name(), args, &a); err != nil {
		return "", err
	}
	question := strings.TrimSpace(a.Question)
	if question == "" {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("question must not be empty")}
	}
	if utf8.RuneCountInString(question) > MaxQuestionRunes {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("question is too long (%d runes, max %d)", utf8.RuneCountInString(question), MaxQuestionRunes)}
	}
	options := make([]string, 0, len(a.Options))
	for i, opt := range a.Options {
		opt = strings.TrimSpace(opt)
		if opt == "" {
			return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("option %d must not be empty", i)}
		}
		if utf8.RuneCountInString(opt) > MaxOptionRunes {
			return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("option %d is too long (%d runes, max %d)", i, utf8.RuneCountInString(opt), MaxOptionRunes)}
		}
		options = append(options, opt)
	}
	if len(options) > MaxQuestionOptions {
		return "", &ToolError{Tool: t.Name(), Kind: "invalid_args", Err: fmt.Errorf("too many options (%d, max %d)", len(options), MaxQuestionOptions)}
	}
	// A single option is a valid question (a confirm button), and
	// zero options means free text — both are accepted as given.
	timeout := DefaultQuestionTimeout
	if a.TimeoutSeconds != 0 {
		timeout = time.Duration(a.TimeoutSeconds) * time.Second
		if timeout < MinQuestionTimeout {
			timeout = MinQuestionTimeout
		}
		if timeout > MaxQuestionTimeout {
			timeout = MaxQuestionTimeout
		}
	}
	if t.ask == nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: errors.New("ask_user_question is not available here: no question transport is configured (it needs a Telegram chat)")}
	}
	// The chat/sender come from the turn's context (set by the
	// daemon's turn handler from the Telegram update — the same
	// context the permission gate reads). A run with no chat in
	// its context (a local CLI run) has nobody to ask.
	chatID, senderID, ok := permissions.ApprovalContextFrom(ctx)
	if !ok || chatID == 0 {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: errors.New("ask_user_question needs a Telegram chat: this run has no chat in its context")}
	}
	answer, source, optionIndex, err := t.ask(ctx, chatID, senderID, question, options, timeout)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: fmt.Errorf("no answer within %s", timeout)}
		}
		if errors.Is(err, context.Canceled) {
			return "", &ToolError{Tool: t.Name(), Kind: "timeout", Err: errors.New("the question was cancelled (the turn ended before an answer arrived)")}
		}
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: err}
	}
	out, err := json.Marshal(questionAnswer{Answer: answer, Source: source, OptionIndex: optionIndex})
	if err != nil {
		return "", &ToolError{Tool: t.Name(), Kind: "execution", Err: fmt.Errorf("could not encode the answer: %w", err)}
	}
	return string(out), nil
}

// SetQuestionAsker installs the transport the registry's
// ask_user_question tool uses to reach the user. A nil fn is a
// no-op (the tool keeps whatever asker it had, if any). Call
// before the registry serves turns: the swap mutates the tool
// struct, so it must not race Execute. The daemon installs the
// Telegram asker after it builds the gateway; local runs never
// install one, so the tool fails cleanly there instead of hanging.
func (r *Registry) SetQuestionAsker(fn QuestionAskFunc) {
	if r == nil || fn == nil {
		return
	}
	t, ok := r.Get("ask_user_question")
	if !ok {
		return
	}
	qt, ok := t.(*askUserQuestionTool)
	if !ok || qt == nil {
		return
	}
	qt.ask = fn
}

// QuestionAsker returns the transport installed on the registry's
// ask_user_question tool, or nil when none is installed (or the
// registry has no such tool).
func (r *Registry) QuestionAsker() QuestionAskFunc {
	if r == nil {
		return nil
	}
	t, ok := r.Get("ask_user_question")
	if !ok {
		return nil
	}
	qt, ok := t.(*askUserQuestionTool)
	if !ok || qt == nil {
		return nil
	}
	return qt.ask
}
