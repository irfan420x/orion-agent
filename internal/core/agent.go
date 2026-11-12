// Package core implements Orion's agent loop: the model-agnostic,
// tool-using conversation loop that sits between a model.Provider,
// the tool registry, and durable memory.
//
// The loop is provider-neutral: it only ever talks to the
// model.Provider interface, so any provider (OpenAI-compatible,
// Anthropic, local Ollama, Hermes-class open models, ...) can drive it.
// Tools are executed through the tools.Registry, which enforces the
// security denylist; this package never executes shell commands itself.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/security"
	"github.com/irfan/orion/internal/tools"
)

// Defaults applied when the config omits a limit.
const (
	defaultMaxSteps       = 20
	defaultTimeoutSeconds = 120
	// maxRecentMessages is how many recent conversation messages from
	// memory are included as context for each run.
	maxRecentMessages = 10
)

// systemPrompt is Orion's identity and operating rules.
const systemPrompt = `You are Orion, a model-agnostic OS agent running locally on the user's machine. ` +
	`You are local-first: you prefer local tools and data, and you never send private data anywhere the user did not ask you to. ` +
	`You accomplish tasks by calling the tools available to you, one step at a time, and you report what you did in plain language. ` +
	`Make at most ONE tool call per message. After each tool call, wait for its result before deciding the next step. ` +
	`Never issue a tool call whose arguments depend on the result of another tool call you have not seen yet — for example, do not write a computed value to a file in the same message as the calculation; first call the calculator, read its result, and only then call file_write with that number. ` +
	`When a request is ambiguous, ask for clarification before acting. ` +
	`When a request is destructive (deleting, overwriting, shutting down, or otherwise irreversible), confirm exactly what will happen and wait for the user to confirm before doing it. ` +
	`Never run a blocked or destructive command. If a tool refuses an action, explain why and offer a safe alternative. ` +
	`If you do not know something, say so instead of inventing an answer.`

// Agent is the core agent loop. Run/RunStream calls are serialised with
// runMu so at most one run executes at a time; SetLogger and Shutdown
// use their own locks and never block on a running run. Shutdown does
// not interrupt a run already in flight — it only prevents new runs
// from starting and persists memory. Create it with NewAgent.
type Agent struct {
	provider model.Provider
	tools    *tools.Registry
	memory   memory.Store
	cfg      *config.Config
	logger   *log.Logger
	runMu    sync.Mutex   // serialises Run/RunStream
	loggerMu sync.RWMutex // guards logger
	shutMu   sync.Mutex   // guards shutDown
	shutDown bool
}

// MaxStepsError is returned by Run/RunStream when the step budget is
// exhausted before the model produces a final answer. It carries the
// partial transcript built so far (system prompt, user input, tool
// calls and tool results), so callers can inspect or resume from the
// partial progress instead of losing it. The error string stays
// "core: max steps exceeded" for callers that match on it.
type MaxStepsError struct {
	Messages []model.Message
}

func (e *MaxStepsError) Error() string { return "core: max steps exceeded" }

// Transcript returns the partial transcript carried by the error.
func (e *MaxStepsError) Transcript() []model.Message { return e.Messages }

// NewAgent builds an Agent. provider, tool registry and memory store must
// all be non-nil; a nil config is replaced with config.Default().
// Limits of zero in the config are replaced with safe defaults.
func NewAgent(cfg *config.Config, provider model.Provider, registry *tools.Registry, mem memory.Store) (*Agent, error) {
	if provider == nil {
		return nil, errors.New("core: provider must not be nil")
	}
	if registry == nil {
		return nil, errors.New("core: tools registry must not be nil")
	}
	if mem == nil {
		return nil, errors.New("core: memory store must not be nil")
	}
	if cfg == nil {
		cfg = config.Default()
	} else {
		// Copy: filling defaults below must not mutate the caller's
		// config (a shared config object would change under the
		// caller and under App.Config).
		cfgCopy := *cfg
		cfg = &cfgCopy
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = defaultMaxSteps
	}
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = defaultTimeoutSeconds
	}
	return &Agent{
		provider: provider,
		tools:    registry,
		memory:   mem,
		cfg:      cfg,
		logger:   log.New(os.Stderr, "orion/core: ", log.LstdFlags),
	}, nil
}

// SystemPrompt returns Orion's identity and operating rules.
func (a *Agent) SystemPrompt() string { return systemPrompt }

// SetLogger replaces the agent's logger. A nil logger is ignored.
// It never blocks on a running run.
func (a *Agent) SetLogger(l *log.Logger) {
	if l == nil {
		return
	}
	a.loggerMu.Lock()
	defer a.loggerMu.Unlock()
	a.logger = l
}

// logf writes a formatted log line, never panicking on a nil logger.
func (a *Agent) logf(format string, args ...any) {
	a.loggerMu.RLock()
	l := a.logger
	a.loggerMu.RUnlock()
	if l != nil {
		l.Printf(format, args...)
	}
}

// timeout returns the per-step timeout for provider calls and tool runs.
func (a *Agent) timeout() time.Duration {
	secs := a.cfg.TimeoutSeconds
	if secs <= 0 {
		secs = defaultTimeoutSeconds
	}
	return time.Duration(secs) * time.Second
}

// maxSteps returns the configured step budget for one run.
func (a *Agent) maxSteps() int {
	if a.cfg.MaxSteps <= 0 {
		return defaultMaxSteps
	}
	return a.cfg.MaxSteps
}

// Run executes one user turn: it sends the user input (with system prompt
// and recent memory) to the provider, executes any requested tool calls,
// feeds the results back to the model, and repeats until the model answers
// directly or the step budget is exhausted. A tool failure is fed back to
// the model as the tool result; it never aborts the loop on its own.
func (a *Agent) Run(ctx context.Context, input string) (string, error) {
	return a.run(ctx, input, false, nil)
}

// RunStream is Run, except every step is produced through
// provider.Stream: each content chunk is passed to fn as it arrives
// and the final step's joined chunks are returned. Streaming is one
// provider call per step — there is no preceding Chat call for any
// turn, so the model is neither charged twice nor able to contradict
// itself between two calls. When a streamed turn contains tool calls,
// the calls are executed and the loop continues, streaming the next
// step too. Text streamed on a tool-call turn is delivered to fn live
// (it is the model's own preamble) and stored in memory as that
// step's assistant message; only the final, tool-free step's content
// is returned.
func (a *Agent) RunStream(ctx context.Context, input string, fn func(chunk string) error) (string, error) {
	if fn == nil {
		return "", errors.New("core: stream callback must not be nil")
	}
	return a.run(ctx, input, true, fn)
}

// run is the shared loop for Run and RunStream. When stream is true
// every step goes through provider.Stream (exactly one provider call
// per step); otherwise every step goes through provider.Chat.
func (a *Agent) run(ctx context.Context, input string, stream bool, streamFn func(chunk string) error) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(input) == "" {
		return "", errors.New("core: input must not be empty")
	}

	a.runMu.Lock()
	defer a.runMu.Unlock()

	a.shutMu.Lock()
	shutDown := a.shutDown
	a.shutMu.Unlock()
	if shutDown {
		return "", errors.New("core: agent is shut down")
	}

	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("core: run cancelled before start: %w", err)
	}

	// Build the conversation: system prompt, recent memory, user input.
	messages := []model.Message{{Role: "system", Content: systemPrompt}}
	if recent, err := a.memory.RecentMessages(maxRecentMessages); err != nil {
		a.logf("memory RecentMessages failed (continuing without history): %v", err)
	} else {
		messages = append(messages, recent...)
	}
	userMsg := model.Message{Role: "user", Content: input}
	messages = append(messages, userMsg)
	if err := a.memory.AddMessage(userMsg); err != nil {
		a.logf("memory AddMessage (user) failed: %v", err)
	}

	// deferred holds tool calls the model requested in a turn where
	// only the first was executed (see below). If the model then
	// answers directly instead of re-planning them, the loop reminds
	// it once to complete them before finishing.
	var deferred []model.ToolCall

	for step := 0; step < a.maxSteps(); step++ {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("core: run cancelled at step %d: %w", step+1, err)
		}

		// One provider call per step, under its own timeout. The
		// step timeout is released as soon as the provider call
		// returns: tool execution below gets a fresh timeout
		// derived from the parent ctx, so a slow provider call
		// cannot starve the tools.
		stepCtx, cancel := context.WithTimeout(ctx, a.timeout())
		req := model.ChatRequest{
			Messages:    append([]model.Message(nil), messages...),
			Tools:       a.tools.Definitions(),
			Temperature: a.cfg.Temperature,
		}
		var resp *model.ChatResponse
		var err error
		if stream {
			resp, err = a.provider.Stream(stepCtx, req, streamFn)
		} else {
			resp, err = a.provider.Chat(stepCtx, req)
		}
		cancel()
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return "", fmt.Errorf("core: provider call cancelled at step %d: %w", step+1, err)
			}
			return "", fmt.Errorf("core: provider call at step %d: %w", step+1, err)
		}
		if resp == nil {
			return "", fmt.Errorf("core: provider returned nil response at step %d", step+1)
		}

		if len(resp.Message.ToolCalls) == 0 {
			// Final answer — unless tool calls were deferred
			// from an earlier turn and the model never re-planned
			// them. A small model often answers after the first
			// tool result and drops the rest of its own plan; nudge
			// it once to finish the pending calls first. The
			// premature answer is discarded, not recorded, so the
			// history stays clean. The reminder lists tool names
			// only — the dropped arguments may be stale (they were
			// planned before the results existed), so the model
			// must re-derive them from the results so far.
			if len(deferred) > 0 {
				names := make([]string, 0, len(deferred))
				for _, tc := range deferred {
					names = append(names, tc.Name)
				}
				a.logf("step %d: model answered with %d deferred tool call(s) pending (%s); reminding it to complete them", step+1, len(deferred), strings.Join(names, ", "))
				deferred = nil
				messages = append(messages, model.Message{
					Role: "user",
					Content: fmt.Sprintf("You still have pending tool calls from your previous message: %s. "+
						"Call them, with correct arguments based on the tool results so far, before giving your final answer. "+
						"If you no longer need one, say so and then answer.", strings.Join(names, ", ")),
				})
				continue
			}
			assistantMsg := model.Message{Role: "assistant", Content: resp.Message.Content}
			messages = append(messages, assistantMsg)
			if err := a.memory.AddMessage(assistantMsg); err != nil {
				a.logf("memory AddMessage (assistant) failed: %v", err)
			}
			return resp.Message.Content, nil
		}

		// Tool-call turn: record the assistant message, then execute
		// each requested tool and feed the results back. Tool-call
		// IDs must never be empty: a provider that omits them (Ollama
		// returns none) would otherwise break tool-result linkage
		// when history is converted back for OpenAI/Anthropic.
		// Synthesise stable IDs here, centrally, so every provider
		// is covered even if it forgets to set one.
		toolCalls := append([]model.ToolCall(nil), resp.Message.ToolCalls...)
		for i := range toolCalls {
			if strings.TrimSpace(toolCalls[i].ID) == "" {
				toolCalls[i].ID = fmt.Sprintf("call-%d-%d", step+1, i+1)
			}
		}
		// One tool call per turn. A model that issues several calls
		// at once has planned them before seeing any result, so a
		// later call's arguments can depend on a result that does
		// not exist yet — a small model once wrote a hallucinated
		// number to a file instead of the calculator's answer.
		// Execute only the first call; the model re-plans the rest
		// on the next turn, with the result in context. The
		// assistant message records only the executed call, so
		// tool-result linkage stays complete for every provider.
		if len(toolCalls) > 1 {
			a.logf("step %d: model requested %d tool calls; executing only %q this turn, the rest will be re-planned", step+1, len(toolCalls), toolCalls[0].Name)
			deferred = append([]model.ToolCall(nil), toolCalls[1:]...)
			toolCalls = toolCalls[:1]
		} else {
			deferred = nil
		}
		assistantMsg := model.Message{
			Role:      "assistant",
			Content:   resp.Message.Content,
			ToolCalls: toolCalls,
		}
		messages = append(messages, assistantMsg)
		if err := a.memory.AddMessage(assistantMsg); err != nil {
			a.logf("memory AddMessage (assistant tool call) failed: %v", err)
		}

		for _, tc := range toolCalls {
			if err := ctx.Err(); err != nil {
				return "", fmt.Errorf("core: run cancelled during tool calls at step %d: %w", step+1, err)
			}
			// Each tool run gets its own timeout from the parent
			// ctx, not the step ctx that also covered the provider
			// call, so a slow provider reply cannot starve the tools.
			toolCtx, toolCancel := context.WithTimeout(ctx, a.timeout())
			result := a.executeTool(toolCtx, tc)
			toolCancel()
			toolMsg := model.Message{Role: "tool", Content: result, ToolCallID: tc.ID}
			messages = append(messages, toolMsg)
			if err := a.memory.AddMessage(toolMsg); err != nil {
				a.logf("memory AddMessage (tool result) failed: %v", err)
			}
		}
	}

	return "", &MaxStepsError{Messages: append([]model.Message(nil), messages...)}
}

// executeTool runs one tool call. Arguments must be a JSON object string;
// invalid JSON and tool failures are returned as error text so the caller
// can feed them back to the model instead of aborting the loop.
func (a *Agent) executeTool(ctx context.Context, tc model.ToolCall) string {
	args, ok := parseToolArguments(tc)
	if !ok {
		a.logf("tool %q: invalid JSON arguments, feeding error back to model", tc.Name)
		return fmt.Sprintf("Error: invalid arguments for tool %q: %q is not a JSON object", tc.Name, tc.Arguments)
	}
	// The registry enforces the security denylist for shell tools; log
	// denials here so they are visible in the agent log.
	result, err := a.tools.Execute(ctx, tc.Name, args)
	if err != nil {
		var toolErr *tools.ToolError
		if errors.As(err, &toolErr) && toolErr.Kind == "denied" {
			a.logf("tool %q denied: %v", tc.Name, err)
		} else {
			a.logf("tool %q failed: %v", tc.Name, err)
		}
		return fmt.Sprintf("Error executing tool %q: %v", tc.Name, err)
	}
	return result
}

// parseToolArguments validates a tool call's argument string and returns
// it as JSON. An empty argument string (or a JSON null) means "no
// arguments" ({}). The top level must be a JSON object: arrays, numbers,
// strings and other scalars are rejected, because tools decode the
// arguments into a struct/map and a non-object fails confusingly
// inside the tool instead of here.
func parseToolArguments(tc model.ToolCall) (json.RawMessage, bool) {
	trimmed := strings.TrimSpace(tc.Arguments)
	if trimmed == "" || trimmed == "null" {
		return json.RawMessage("{}"), true
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(trimmed))
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	// Reject trailing data after the first JSON value.
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, false
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, false
	}
	return json.RawMessage(trimmed), true
}

// Shutdown persists memory and closes it. A Save failure does not skip
// Close: Close re-attempts the save, so a transient failure can still
// persist on the second try. But the same underlying save failure must
// not be reported twice — Save and Close both attempt the same persist,
// so when Close fails with the same failure Shutdown reports it once.
// Distinct failures (Save fails one way, Close another) are still both
// reported. It never blocks on a running run (a run in flight is not
// interrupted, and new runs are refused after Shutdown); calling
// Shutdown twice is a no-op.
func (a *Agent) Shutdown() error {
	a.shutMu.Lock()
	if a.shutDown {
		a.shutMu.Unlock()
		return nil
	}
	a.shutDown = true
	a.shutMu.Unlock()
	saveErr := a.memory.Save()
	closeErr := a.memory.Close()
	if saveErr != nil && closeErr != nil && sameSaveFailure(saveErr, closeErr) {
		// One underlying persist failure, seen twice: surface it once.
		return fmt.Errorf("core: shutdown save memory: %w", saveErr)
	}
	var errs []error
	if saveErr != nil {
		errs = append(errs, fmt.Errorf("core: shutdown save memory: %w", saveErr))
	}
	if closeErr != nil {
		errs = append(errs, fmt.Errorf("core: shutdown close memory: %w", closeErr))
	}
	return errors.Join(errs...)
}

// sameSaveFailure reports whether two errors describe the same underlying
// failure: the identical error value, or two distinct errors with the
// same message — as the memory store produces when the same persist
// fails on two separate attempts (once from Save, once from Close).
func sameSaveFailure(a, b error) bool {
	if errors.Is(a, b) || errors.Is(b, a) {
		return true
	}
	return a.Error() == b.Error()
}

// IsBlockedCommand reports whether a shell command string must be refused.
// It delegates to the canonical security check; tools enforce it, this is
// for logging/inspection at the agent layer.
func IsBlockedCommand(cmd string) bool { return security.IsBlockedCommand(cmd) }
