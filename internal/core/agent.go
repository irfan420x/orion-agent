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
	`If you do not know something, say so instead of inventing an answer.` +
	`

Memory — saving what you learn (typed memories):
- Types: user (who the user is), feedback (how to work — corrections AND confirmations; a "yes exactly" counts), project (ongoing work, goals, bugs), reference (pointers to external systems).
- Save only what was surprising or non-obvious. If the user explicitly asks you to save something, keep just the surprising part of it.
- Never save code patterns, architecture, git history, or file structure — those are derivable from the tree.
- Before acting on a saved memory that names a file, function, or flag, verify it still exists; if it does not, trust the current state, not the memory.`

// Agent is the core agent loop. Run/RunStream calls are serialised with
// runMu so at most one run executes at a time; SetLogger,
// SetEventHandler and Shutdown use their own locks and never block on
// a running run. Shutdown does not interrupt a run already in flight —
// it only prevents new runs from starting and persists memory.
// Progress events (see events.go) are emitted synchronously while
// runMu is held. Create it with NewAgent.
type Agent struct {
	provider model.Provider
	tools    *tools.Registry
	memory   memory.Store
	cfg      *config.Config
	logger   *log.Logger
	runMu    sync.Mutex   // serialises Run/RunStream
	loggerMu sync.RWMutex // guards logger
	// eventHandler is read at each emission and set via
	// SetEventHandler; it has its own lock like logger/loggerMu
	// and never blocks on a running run.
	eventHandler   EventHandler
	eventHandlerMu sync.RWMutex // guards eventHandler
	// runHandler is the per-run handler installed by
	// RunWithEventHandler for the run currently holding runMu
	// (nil when the run came through Run/RunStream). emit prefers
	// it over eventHandler, so two callers sharing one Agent (two
	// Telegram chats via HandleStream) each receive exactly their
	// own run's events instead of racing on one shared handler
	// (Audit 4 B P1-2). It is set and cleared inside run() while
	// runMu is held, so at most one run's handler is live.
	runHandler   EventHandler
	runHandlerMu sync.RWMutex // guards runHandler
	shutMu       sync.Mutex   // guards shutDown
	shutDown     bool
	// Recall v2 / drift / extraction state (see recall_v2.go,
	// drift.go, extraction.go). typedMu guards the wired sources
	// and roots; surfaced/stale/recentTools have their own locks.
	typedMu               sync.RWMutex
	typedMem              typedMemorySource
	sessionMem            sessionMemoryAppender
	driftRoot             string
	memorySelector        memorySelectorFunc
	surfacedMu            sync.Mutex
	surfaced              map[string]struct{}
	staleMu               sync.Mutex
	stale                 map[string]struct{}
	recentToolsMu         sync.Mutex
	recentTools           []string
	extractMu             sync.Mutex
	extractRunning        bool
	extractThresholdTools int
	extractThresholdTurns int
	toolCallsSinceExtract int
	turnsSinceExtract     int
	sessionExtractor      sessionExtractorFunc
	// subagentMu guards subagentTokens: tokens of subagent results
	// the parent consumed during the current turn (reported via
	// AddSubagentUsage by the subagent_result tool). The turn's
	// finishTurn adds the accumulated total to the turn's
	// Usage.TotalTokens exactly once; a new turn starts at zero.
	subagentMu     sync.Mutex
	subagentTokens int
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
	if cfg.MaxToolResultBytes <= 0 {
		cfg.MaxToolResultBytes = config.DefaultMaxToolResultBytes
	}
	if cfg.MaxHistoryTokens <= 0 {
		cfg.MaxHistoryTokens = config.DefaultMaxHistoryTokens
	}
	// Push the tool-result cap into the registry: Registry.Execute
	// is the central choke point for result size, and the agent is
	// where config meets the registry. A registry shared by several
	// agents takes the cap of the most recently created one.
	registry.SetMaxResultBytes(cfg.MaxToolResultBytes)
	return &Agent{
		provider: provider,
		tools:    registry,
		memory:   mem,
		cfg:      cfg,
		logger:   log.New(os.Stderr, "orion/core: ", log.LstdFlags),
		surfaced: map[string]struct{}{},
		stale:    map[string]struct{}{},
	}, nil
}

// AddSubagentUsage adds tokens from a subagent result the parent
// consumed (first fetch of a finished result) to the current turn's
// subagent total. It implements tools.SubagentUsageSink. It is safe
// for concurrent use: subagent completions race each other and the
// parent turn, so the accumulator is mutex-guarded. Non-positive
// values are ignored. Tokens added after the current turn finished
// (a subagent fetched late, or never fetched during a turn) are
// dropped by the next turn's reset — usage is attributed to the turn
// that consumed the result, never carried across turns.
func (a *Agent) AddSubagentUsage(tokens int) {
	if tokens <= 0 {
		return
	}
	a.subagentMu.Lock()
	defer a.subagentMu.Unlock()
	a.subagentTokens += tokens
}

// takeSubagentUsage returns the accumulated subagent tokens for the
// current turn and resets the accumulator to zero. Called exactly
// once per turn, from finishTurn.
func (a *Agent) takeSubagentUsage() int {
	a.subagentMu.Lock()
	defer a.subagentMu.Unlock()
	tokens := a.subagentTokens
	a.subagentTokens = 0
	return tokens
}

// resetSubagentUsage drops any subagent tokens accumulated outside a
// turn (e.g. a late subagent completion after the previous turn
// ended) so a new turn starts at zero.
func (a *Agent) resetSubagentUsage() {
	a.subagentMu.Lock()
	defer a.subagentMu.Unlock()
	a.subagentTokens = 0
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

// maxHistoryTokens returns the token budget for the history fed to
// the provider on each step.
func (a *Agent) maxHistoryTokens() int {
	if a.cfg.MaxHistoryTokens <= 0 {
		return config.DefaultMaxHistoryTokens
	}
	return a.cfg.MaxHistoryTokens
}

// Run executes one user turn: it sends the user input (with system prompt
// and recent memory) to the provider, executes any requested tool calls,
// feeds the results back to the model, and repeats until the model answers
// directly or the step budget is exhausted. A tool failure is fed back to
// the model as the tool result; it never aborts the loop on its own.
func (a *Agent) Run(ctx context.Context, input string) (string, error) {
	return a.run(ctx, input, false, nil, nil)
}

// RunWithEventHandler is Run with a per-run progress-event handler:
// events from this run (and only this run) go to h, even when other
// callers share the Agent. A nil h behaves exactly like Run (events
// go to the agent-wide handler set with SetEventHandler, if any).
// This is what HandleStream uses: two concurrent chats on one Agent
// must not steal each other's TurnFinished events (Audit 4 B P1-2).
func (a *Agent) RunWithEventHandler(ctx context.Context, input string, h EventHandler) (string, error) {
	return a.run(ctx, input, false, nil, h)
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
	return a.run(ctx, input, true, fn, nil)
}

// run is the shared loop for Run and RunStream. When stream is true
// every step goes through provider.Stream (exactly one provider call
// per step); otherwise every step goes through provider.Chat.
// handler, when non-nil, is the per-run event handler for this run
// only (see RunWithEventHandler); nil means "use the agent-wide
// handler".
func (a *Agent) run(ctx context.Context, input string, stream bool, streamFn func(chunk string) error, handler EventHandler) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(input) == "" {
		return "", errors.New("core: input must not be empty")
	}

	a.runMu.Lock()
	defer a.runMu.Unlock()

	// Install the per-run handler for this run only; it is cleared
	// before runMu is released (defer order: this runs before the
	// unlock above), so it can never leak into the next run.
	a.setRunHandler(handler)
	defer a.setRunHandler(nil)

	a.shutMu.Lock()
	shutDown := a.shutDown
	a.shutMu.Unlock()
	if shutDown {
		return "", errors.New("core: agent is shut down")
	}

	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("core: run cancelled before start: %w", err)
	}

	// A new turn starts with a zero subagent total: tokens from a
	// subagent that finished after the previous turn ended must not
	// leak into this turn's accounting (see AddSubagentUsage).
	a.resetSubagentUsage()

	// Progress-event bookkeeping. Everything below this point is a
	// real turn, so every return from here on emits exactly one
	// TurnFinished (guarded by turnFinished); the early validation
	// returns above emit nothing. usage accumulates resp.Usage
	// across the turn's provider calls, verbatim — a provider that
	// reports zero usage leaves it zero, it is never estimated here.
	// lastStep tracks the most recent step for TurnFinished.
	// memoriesUsed counts the typed memories injected this turn
	// (recall v2); it is assigned at the recall site below and
	// carried on the TurnFinished event for TurnStats.
	turnStart := time.Now()
	var usage model.Usage
	var memoriesUsed int
	lastStep := 0
	turnFinished := false
	finishTurn := func(err error) {
		if turnFinished {
			return
		}
		turnFinished = true
		// Subagent usage is a single total: add the tokens of
		// subagent results consumed during this turn to
		// TotalTokens only (Prompt/Completion split is unknown
		// for delegated work — the child reports one total).
		// takeSubagentUsage also resets the accumulator, so a
		// second finishTurn (impossible by the guard above)
		// could never double-count.
		usage.TotalTokens += a.takeSubagentUsage()
		a.emit(ProgressEvent{
			Kind:         EventTurnFinished,
			Step:         lastStep,
			Duration:     time.Since(turnStart),
			Err:          err,
			Usage:        usage,
			MemoriesUsed: memoriesUsed,
		})
	}

	// Build the conversation: system prompt, recent memory, user input.
	messages := []model.Message{{Role: "system", Content: systemPrompt}}
	var recent []model.Message
	if r, err := a.memory.RecentMessages(maxRecentMessages); err != nil {
		a.logf("memory RecentMessages failed (continuing without history): %v", err)
	} else {
		recent = r
		// Repair the window before it is sent: an earlier run that
		// ended after storing an assistant tool-call message but
		// before its tool result (cancelled in between, crashed,
		// or a failed persist) leaves a dangling tool call, and a
		// tool result whose assistant message aged out of the
		// window is an orphan. OpenAI and Anthropic both reject
		// such a transcript with a 400, on every run until the
		// entry ages out — so strip the calls (or drop the message
		// when nothing is left to say) here. The raw window is
		// still what recall uses below.
		history, repaired := repairHistory(recent)
		if repaired > 0 {
			a.logf("repaired recent history: %d dangling message(s) dropped or stripped", repaired)
		}
		messages = append(messages, history...)
	}
	userMsg := model.Message{Role: "user", Content: input}
	messages = append(messages, userMsg)
	if err := a.memory.AddMessage(userMsg); err != nil {
		a.logf("memory AddMessage (user) failed: %v", err)
	}

	// Fact-aware recall: facts and statements from older turns that
	// the recent window no longer covers are injected into the system
	// message. The section is appended only when non-empty, so the
	// system prompt is byte-identical when nothing is relevant.
	// The injected section counts against the history budget like
	// any other message (trimHistoryToBudget measures the augmented
	// system message), so recall + history together stay within
	// budget: when the window is tight, the oldest turns are
	// dropped to pay for the recall, never the system message.
	if items := a.recallFor(input, recent); len(items) > 0 {
		messages[0].Content += formatRecallSection(items)
	}
	// Recall v2: typed memories are injected alongside the v1
	// items, under their own section header (at most 5, drift
	// checked, never failing the run). Their count is the turn's
	// MemoriesUsed, carried on TurnFinished for TurnStats.
	if typed := a.recallTypedFor(ctx, input, recent); len(typed) > 0 {
		memoriesUsed = len(typed)
		messages[0].Content += formatTypedMemorySection(typed)
	}

	// Session-memory extraction is counted at the end of this turn
	// (deferred: it launches a background goroutine and never
	// blocks the turn). The transcript snapshot is taken when the
	// deferred call runs, i.e. after the turn's messages are final.
	defer func() { a.noteTurnCompleted(ctx, messages) }()

	// deferred holds tool calls the model requested in a turn where
	// only the first was executed (see below). If the model then
	// answers directly instead of re-planning them, the loop reminds
	// it once to complete them before finishing.
	var deferred []model.ToolCall

	for step := 0; step < a.maxSteps(); step++ {
		lastStep = step + 1
		a.emit(ProgressEvent{Kind: EventStepStarted, Step: step + 1, Usage: usage})
		if err := ctx.Err(); err != nil {
			rerr := fmt.Errorf("core: run cancelled at step %d: %w", step+1, err)
			finishTurn(rerr)
			return "", rerr
		}

		// One provider call per step, under its own timeout. The
		// step timeout is released as soon as the provider call
		// returns: tool execution below gets a fresh timeout
		// derived from the parent ctx, so a slow provider call
		// cannot starve the tools.
		//
		// The request window is trimmed to the history budget
		// first (see trimHistoryToBudget): the in-flight messages
		// grow with every tool result, and an unbounded request
		// would blow the provider's context window mid-run. Only
		// the request copy is trimmed — the agent's transcript
		// (and the MaxStepsError transcript) keeps the full
		// history, and memory already holds every message.
		stepCtx, cancel := context.WithTimeout(ctx, a.timeout())
		reqMessages := messages
		if trimmed, stats := trimHistoryToBudget(messages, a.maxHistoryTokens()); stats.dropped > 0 || stats.truncated > 0 {
			a.logf("step %d: history trimmed to %d-token budget: dropped %d message(s), truncated %d bytes", step+1, a.maxHistoryTokens(), stats.dropped, stats.truncated)
			reqMessages = trimmed
		}
		req := model.ChatRequest{
			Messages:    append([]model.Message(nil), reqMessages...),
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
			var perr error
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				perr = fmt.Errorf("core: provider call cancelled at step %d: %w", step+1, err)
			} else {
				perr = fmt.Errorf("core: provider call at step %d: %w", step+1, err)
			}
			finishTurn(perr)
			return "", perr
		}
		if resp == nil {
			nerr := fmt.Errorf("core: provider returned nil response at step %d", step+1)
			finishTurn(nerr)
			return "", nerr
		}
		// Accumulate this step's reported usage verbatim; a zero
		// report leaves the totals unchanged.
		usage.PromptTokens += resp.Usage.PromptTokens
		usage.CompletionTokens += resp.Usage.CompletionTokens
		usage.TotalTokens += resp.Usage.TotalTokens

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
			// An empty final answer — no content and no tool calls
			// — is an error, not a silent success: the caller
			// (Telegram, CLI) would otherwise send the user
			// nothing at all, with no indication anything went
			// wrong. The provider layer already guards its own
			// paths (no choices is an error; an empty stream falls
			// back to Chat), but a choice with empty content and
			// finish "stop" can still reach the loop, so the loop
			// enforces the same contract here: the caller never
			// gets an empty reply with a nil error. The empty
			// message is not stored, so the next turn starts from
			// clean history. (Audit 3, Workstream 1.)
			if strings.TrimSpace(resp.Message.Content) == "" {
				nerr := fmt.Errorf("core: provider returned an empty response at step %d", step+1)
				finishTurn(nerr)
				return "", nerr
			}
			assistantMsg := model.Message{Role: "assistant", Content: resp.Message.Content}
			messages = append(messages, assistantMsg)
			if err := a.memory.AddMessage(assistantMsg); err != nil {
				a.logf("memory AddMessage (assistant) failed: %v", err)
			}
			finishTurn(nil)
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
			// Append, never overwrite: calls deferred on an earlier
			// turn are still pending, and a later multi-call turn
			// must not silently drop them. A call the model
			// re-issues on a later turn is collapsed by name and
			// arguments instead of being listed twice.
			deferred = appendDeferred(deferred, toolCalls[1:]...)
			toolCalls = toolCalls[:1]
		}
		// The executed call may be one the model re-planned from an
		// earlier turn: it is no longer pending. Anything else stays
		// deferred until the model answers directly, when the loop
		// reminds it once (see above) — a single-call turn must not
		// clear the list, or a pending call the model never
		// re-planned is silently lost.
		deferred = removeDeferred(deferred, toolCalls[0])
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
				rerr := fmt.Errorf("core: run cancelled during tool calls at step %d: %w", step+1, err)
				finishTurn(rerr)
				return "", rerr
			}
			// Each tool run gets its own timeout from the parent
			// ctx, not the step ctx that also covered the provider
			// call, so a slow provider reply cannot starve the tools.
			toolCtx, toolCancel := context.WithTimeout(ctx, a.timeout())
			toolStart := time.Now()
			a.emit(ProgressEvent{Kind: EventToolCallStarted, Step: step + 1, ToolName: tc.Name, Arguments: tc.Arguments, Usage: usage})
			result := a.executeTool(toolCtx, tc)
			toolCancel()
			// Record the execution for recall v2 (recently-used
			// tools) and the session-extraction tool-call counter.
			a.noteToolCallExecuted(tc.Name)
			// Tool "errors" returned as result text (executeTool
			// returns "Error: ..." strings) count as Err here too.
			var toolErr error
			if strings.HasPrefix(result, "Error") {
				toolErr = errors.New(result)
			}
			a.emit(ProgressEvent{Kind: EventToolCallFinished, Step: step + 1, ToolName: tc.Name, Arguments: tc.Arguments, Duration: time.Since(toolStart), Err: toolErr, Usage: usage})
			toolMsg := model.Message{Role: "tool", Content: result, ToolCallID: tc.ID}
			messages = append(messages, toolMsg)
			if err := a.memory.AddMessage(toolMsg); err != nil {
				a.logf("memory AddMessage (tool result) failed: %v", err)
			}
		}
	}

	merr := &MaxStepsError{Messages: append([]model.Message(nil), messages...)}
	finishTurn(merr)
	return "", merr
}

// appendDeferred adds calls to the pending list, skipping any call
// already pending with the same name and arguments (the model
// re-issuing the same plan on a later turn).
func appendDeferred(deferred []model.ToolCall, calls ...model.ToolCall) []model.ToolCall {
	for _, c := range calls {
		dup := false
		for _, d := range deferred {
			if d.Name == c.Name && d.Arguments == c.Arguments {
				dup = true
				break
			}
		}
		if !dup {
			deferred = append(deferred, c)
		}
	}
	return deferred
}

// removeDeferred drops the pending call the model just executed
// (matched by name and arguments), if it was pending.
func removeDeferred(deferred []model.ToolCall, executed model.ToolCall) []model.ToolCall {
	for i, d := range deferred {
		if d.Name == executed.Name && d.Arguments == executed.Arguments {
			return append(deferred[:i], deferred[i+1:]...)
		}
	}
	return deferred
}

// repairHistory removes history entries that would make the next
// provider call fail outright, and returns how many messages were
// dropped or stripped:
//
//   - an assistant message whose tool calls have no tool result
//     immediately following them (a dangling call, left by a run
//     that ended between storing the assistant message and storing
//     its result) has those calls stripped; the message itself is
//     dropped when stripping leaves it empty;
//   - a tool result whose ID no assistant message in the window
//     requested — or a tool message with no assistant message before
//     it at all (its turn aged out of the window, or the assistant
//     message was never stored) — is dropped.
//
// Satisfaction is positional (a result only satisfies the assistant
// message it immediately follows), because IDs are not globally
// unique: Ollama synthesises call-1, call-2, ... per turn, so the
// same ID can appear in several turns. Well-formed history passes
// through byte-identical.
func repairHistory(msgs []model.Message) ([]model.Message, int) {
	out := make([]model.Message, 0, len(msgs))
	repaired := 0
	for i := 0; i < len(msgs); {
		m := msgs[i]
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			// Tool results immediately following this message.
			j := i + 1
			following := map[string]bool{}
			for j < len(msgs) && msgs[j].Role == "tool" {
				following[msgs[j].ToolCallID] = true
				j++
			}
			requested := map[string]bool{}
			for _, tc := range m.ToolCalls {
				requested[tc.ID] = true
			}
			var kept []model.ToolCall
			for _, tc := range m.ToolCalls {
				if following[tc.ID] {
					kept = append(kept, tc)
				}
			}
			switch {
			case len(kept) == len(m.ToolCalls):
				out = append(out, m)
			case len(kept) > 0 || strings.TrimSpace(m.Content) != "":
				repaired++
				m2 := m
				m2.ToolCalls = kept
				out = append(out, m2)
			default:
				// Dropped: an empty assistant message whose tool
				// calls never got results.
				repaired++
			}
			// Keep only the following results this message
			// requested; anything else is an orphan.
			for k := i + 1; k < j; k++ {
				if requested[msgs[k].ToolCallID] {
					out = append(out, msgs[k])
				} else {
					repaired++
				}
			}
			i = j
			continue
		}
		if m.Role == "tool" {
			// A tool result with no assistant message before it
			// in this window.
			repaired++
			i++
			continue
		}
		out = append(out, m)
		i++
	}
	return out, repaired
}

// messageTokens estimates the tokens one message costs in a
// provider request: its content plus any tool-call names and
// arguments, plus a small per-message allowance for the role and
// framing the provider adds. It uses model.CountTokens — the same
// ~4-runes-per-token estimate the model layer uses when a provider
// does not report usage — so the budget is measured in the units
// providers bill and context windows are sized in. A byte budget
// would mis-measure dense non-ASCII text (Bengali in Telegram is
// 3 bytes per rune but roughly one token per few runes), and the
// estimate only has to keep the request comfortably inside the
// window, not predict the provider's exact count.
func messageTokens(m model.Message) int {
	n := model.CountTokens(m.Content) + 4
	for _, tc := range m.ToolCalls {
		n += model.CountTokens(tc.Name) + model.CountTokens(tc.Arguments) + 4
	}
	return n
}

// trimStats reports what trimHistoryToBudget removed from the
// request window.
type trimStats struct {
	dropped   int // messages dropped outright
	truncated int // bytes cut from message contents
}

// historyUnits splits msgs into drop-safe units: an assistant message
// plus the tool results that immediately follow it is one unit — a
// tool call must never be separated from its results, or the next
// provider call is rejected for a dangling call — and every other
// message is its own unit. The system message is always unit 0.
// Units are returned in order, oldest first.
func historyUnits(msgs []model.Message) [][2]int {
	var units [][2]int
	for i := 0; i < len(msgs); {
		end := i + 1
		if msgs[i].Role == "assistant" && len(msgs[i].ToolCalls) > 0 {
			for end < len(msgs) && msgs[end].Role == "tool" {
				end++
			}
		}
		units = append(units, [2]int{i, end})
		i = end
	}
	return units
}

// trimHistoryToBudget returns a copy of msgs whose estimated token
// cost fits within budget, degrading gracefully instead of failing
// the turn when history (or a single tool result) grows past it:
//
//   - the system prompt is never dropped and never truncated — it
//     carries the identity, the rules, and the fact-aware recall
//     section, so recall + history together stay within budget
//     because the recall-augmented system message is measured here
//     like any other message and the oldest turns pay for it;
//   - the latest user turn is never dropped, and neither is the
//     most recent turn as a whole (usually the tool result the
//     model just produced): if it alone exceeds the budget it is
//     truncated by the rule below, not dropped;
//   - tool-call/tool-result pairing is never broken: whole units
//     are dropped (see historyUnits), and content truncation only
//     ever touches a message's Content, never its ToolCalls, so a
//     result always keeps the call that requested it;
//   - when dropping whole turns is not enough — a single tool
//     result (or a huge user turn) is larger than the budget on its
//     own — the largest remaining messages are truncated head-first
//     with a marker carrying the original size, so the loop
//     continues with partial data instead of erroring out.
//
// The caller's slice is never modified: only the request window is
// trimmed, while the agent's transcript and memory keep the full
// history. A history already within budget is returned unchanged
// (a copy, so later mutation of the result cannot affect the
// caller's messages).
func trimHistoryToBudget(msgs []model.Message, budget int) ([]model.Message, trimStats) {
	out := append([]model.Message(nil), msgs...)
	if len(out) == 0 || budget <= 0 {
		return out, trimStats{}
	}
	costs := make([]int, len(out))
	total := 0
	for i, m := range out {
		costs[i] = messageTokens(m)
		total += costs[i]
	}
	if total <= budget {
		return out, trimStats{}
	}
	// The latest user turn is the last user-role message; the loop
	// may have appended a deferred-call reminder after the original
	// input, and that reminder is the turn that must survive.
	lastUser := -1
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Role == "user" {
			lastUser = i
			break
		}
	}
	var stats trimStats
	drop := make([]bool, len(out))
	// Phase 1: drop the oldest droppable units until the window
	// fits or nothing droppable remains. Units are oldest-first,
	// so the most recent turns survive longest. Three units are
	// never dropped: the system prompt, the unit holding the latest
	// user turn, and the most recent turn itself (usually the
	// tool result the model just produced — dropping it would
	// leave the model with nothing to continue from; if it alone
	// exceeds the budget, phase 2 truncates it instead).
	units := historyUnits(out)
	for ui, u := range units {
		if total <= budget {
			break
		}
		if u[0] == 0 {
			continue // system prompt: never dropped
		}
		if lastUser >= u[0] && lastUser < u[1] {
			continue // latest user turn: never dropped
		}
		if ui == len(units)-1 {
			continue // most recent turn: truncate, never drop
		}
		for i := u[0]; i < u[1]; i++ {
			drop[i] = true
			total -= costs[i]
			stats.dropped++
		}
	}
	// Phase 2: still over budget — one message alone exceeds it.
	// Truncate the largest remaining non-system message contents,
	// head-first, until the window fits. The system prompt is
	// never truncated; the latest user turn is truncated only if
	// it is itself the largest remaining message (dropping it is
	// forbidden, cutting it with a marker beats failing the turn).
	// The loop always makes progress: each cut removes the whole
	// overage plus slack for the marker the truncation itself adds
	// (the marker is fixed text, so a cut smaller than it would
	// grow the message), and the loop stops — accepting a small
	// overage — when a message is at the floor and cutting further
	// cannot shrink the window. It can never spin.
	for total > budget {
		big := -1
		for i := 1; i < len(out); i++ {
			if drop[i] || len(out[i].Content) == 0 {
				continue
			}
			if big < 0 || costs[i] > costs[big] {
				big = i
			}
		}
		if big < 0 {
			break
		}
		over := total - budget
		// ~4 runes per token for the overage, plus slack for the
		// marker appended below (~70 chars) so the cut always
		// exceeds what the marker adds.
		cutRunes := over*4 + 128
		r := []rune(out[big].Content)
		if len(r) <= 64 {
			break // cannot cut further without emptying the message
		}
		keep := len(r) - cutRunes
		if keep < 64 {
			keep = 64
		}
		origBytes := len(out[big].Content)
		kept := string(r[:keep])
		out[big].Content = kept + fmt.Sprintf("\n[truncated for context budget: showing first %d bytes of %d total bytes]", len(kept), origBytes)
		stats.truncated += origBytes - len(out[big].Content)
		newCost := messageTokens(out[big])
		if newCost >= costs[big] {
			// At the floor: the marker now costs more than the
			// cut saves. Stop and accept the small overage
			// rather than looping forever.
			break
		}
		total -= costs[big] - newCost
		costs[big] = newCost
	}
	if stats.dropped == 0 && stats.truncated == 0 {
		return out, stats
	}
	kept := make([]model.Message, 0, len(out))
	for i, m := range out {
		if !drop[i] {
			kept = append(kept, m)
		}
	}
	return kept, stats
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
