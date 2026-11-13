// Audit 3, Workstream 1 — agent reliability harness.
//
// This file drives the REAL agent loop (Agent.Run / Agent.RunStream)
// through 100+ scripted Telegram-style conversations — multi-turn user
// messages with tool calls — against a deterministic fake provider
// (scriptedModel, which implements model.Provider in this file only;
// it never touches internal/model). The fake provider behaves like a
// cooperative model that follows a small command language embedded in
// the user message, so chains are deterministic and every run is
// reproducible, while the loop, the real tool registry, and the real
// memory store all execute for real.
//
// Coverage:
//   - multi-step tool chains (calculator -> file_write -> file_read,
//     shell_exec, multi-call turns and the deferred-call reminder)
//   - model failures: HTTP 5xx (hard fail + provider-internal retry),
//     malformed JSON tool-call arguments, empty responses, nil
//     responses, truncated streams, a hanging provider (step timeout
//     must catch it), unknown tool names, max-steps exhaustion
//   - recovery: the turn after every failure must succeed, with no
//     dangling tool calls left in history (a dangling call would 400
//     every later provider call)
//   - history integrity: every request the loop sends to the provider
//     is validated here (tool-call/tool-result pairing, token budget)
//   - a dedicated 32768-token budget test (TestAudit3HistoryBudget32768)
//
// Safety: the only shell commands any scenario requests are "echo ..."
// and one denied "rm -rf /" (denylist path only — never executed).
// File tools only ever touch t.TempDir().
package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/tools"
)

// ---------------------------------------------------------------------------
// scriptedModel: a deterministic fake provider
// ---------------------------------------------------------------------------

// scriptedModel implements model.Provider by following a small command
// language in the user message. The current command and the phase
// (number of tool results since the command started) are derived from
// the request on every call, so the model is stateless across calls
// except for fault-injection flags and observations.
//
// Commands (first word of the user message):
//
//	CALC <expr>                 calculator, then final answer
//	CALCWRITE <expr> <file>     calculator -> file_write -> file_read
//	SHELL <cmd>                 shell_exec, then final answer
//	MULTI                       two tool calls at once (tests deferral)
//	MULTIANSWER                 two calls, then a premature answer
//	                            (tests the deferred-call reminder)
//	DENIED                      shell_exec "rm -rf /" (must be refused)
//	BADARGS <expr>              malformed JSON args once, then correct
//	BADNAME                     call to an unknown tool
//	LOOP                        tool call on every step (max steps)
//	CHAT                        plain text reply
//	RECALL                      answer from earlier turns in history
//	FAIL5XX                     this call fails (simulated HTTP 500)
//	FLAKY <subcommand>          two internal failed attempts, then the
//	                            subcommand succeeds (provider retries)
//	NILRESP                     return (nil, nil)
//	EMPTY                       return an empty final message
//	HANG                        block until ctx is done (step timeout)
//	BIG <file>                  file_read a large file (budget test)
//	TRUNCSTREAM                 (Stream only) emit a chunk, then error
type scriptedModel struct {
	mu  sync.Mutex
	dir string // temp dir file tools may write to

	chatCalls   int
	streamCalls int

	// Fault injection (set at construction per conversation).
	flakyLeft       int  // internal failed attempts to simulate per call
	hangOnce        bool // next Chat call blocks until ctx is done
	truncStreamOnce bool // next Stream call emits one chunk, then errors

	// Observations (read after the conversation).
	internalAttempts int
	maxReqTokens     int
	invalidRequests  []string // every request that broke pairing/budget
	reminderSeen     int
	multiCallTurns   int
	firstReqMsgs     []model.Message // messages of the first request seen
	pendingArgs      map[string]string
}

func (m *scriptedModel) Name() string { return "scripted" }

func (m *scriptedModel) Chat(ctx context.Context, req model.ChatRequest) (*model.ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chatCalls++
	m.observe(req)
	if m.hangOnce {
		m.hangOnce = false
		m.mu.Unlock()
		<-ctx.Done()
		m.mu.Lock()
		return nil, ctx.Err()
	}
	if m.flakyLeft > 0 {
		// Simulate provider-internal retries: fail twice inside
		// this one call, then succeed — the agent loop sees a
		// single successful Chat.
		for m.flakyLeft > 0 {
			m.flakyLeft--
			m.internalAttempts++
			time.Sleep(5 * time.Millisecond)
		}
	}
	return m.decide(req)
}

func (m *scriptedModel) Stream(ctx context.Context, req model.ChatRequest, fn func(chunk string) error) (*model.ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.streamCalls++
	m.observe(req)
	if m.truncStreamOnce {
		m.truncStreamOnce = false
		if err := fn("partial answer that never completes..."); err != nil {
			return nil, err
		}
		return nil, errors.New("scripted: truncated stream (connection dropped mid-response)")
	}
	resp, err := m.decide(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if resp.Message.Content != "" {
		// Emit the content in two chunks, like a real stream.
		half := len(resp.Message.Content) / 2
		for _, chunk := range []string{resp.Message.Content[:half], resp.Message.Content[half:]} {
			if chunk == "" {
				continue
			}
			if err := fn(chunk); err != nil {
				return nil, err
			}
		}
	}
	return resp, nil
}

// observe validates every request the loop sends: tool-call/tool-result
// pairing must be intact (a dangling call would 400 on OpenAI/Anthropic
// on every later turn) and the estimated token cost is recorded.
func (m *scriptedModel) observe(req model.ChatRequest) {
	if m.firstReqMsgs == nil {
		m.firstReqMsgs = append([]model.Message(nil), req.Messages...)
	}
	total := 0
	for _, msg := range req.Messages {
		total += messageTokens(msg)
	}
	if total > m.maxReqTokens {
		m.maxReqTokens = total
	}
	for i, msg := range req.Messages {
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				found := false
				for j := i + 1; j < len(req.Messages) && req.Messages[j].Role == "tool"; j++ {
					if req.Messages[j].ToolCallID == tc.ID {
						found = true
					}
				}
				if !found {
					m.invalidRequests = append(m.invalidRequests,
						fmt.Sprintf("dangling tool call %q (id %q) at request message %d", tc.Name, tc.ID, i))
				}
			}
		}
		if msg.Role == "tool" {
			// The tool result must be preceded (possibly after
			// sibling results) by an assistant message that
			// requested this exact ID.
			requested := false
			for j := i - 1; j >= 0; j-- {
				if req.Messages[j].Role == "assistant" {
					for _, tc := range req.Messages[j].ToolCalls {
						if tc.ID == msg.ToolCallID {
							requested = true
						}
					}
					break
				}
				if req.Messages[j].Role != "tool" {
					break
				}
			}
			if !requested {
				m.invalidRequests = append(m.invalidRequests,
					fmt.Sprintf("orphan tool result (id %q) at request message %d", msg.ToolCallID, i))
			}
		}
	}
	if len(req.Messages) > 0 {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "user" && strings.HasPrefix(last.Content, "You still have pending tool calls") {
			m.reminderSeen++
		}
	}
}

// lastCommand finds the most recent user message that is a scenario
// command (not the deferred-call reminder) and returns its words plus
// the number of tool results since it.
func lastCommand(req model.ChatRequest) (cmd []string, toolResults int, ok bool) {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		msg := req.Messages[i]
		if msg.Role == "tool" {
			toolResults++
			continue
		}
		if msg.Role == "user" {
			if strings.HasPrefix(msg.Content, "You still have pending tool calls") {
				continue
			}
			return strings.Fields(msg.Content), toolResults, true
		}
	}
	return nil, 0, false
}

// decide returns the model's next reply for this request.
func (m *scriptedModel) decide(req model.ChatRequest) (*model.ChatResponse, error) {
	msgs := req.Messages
	if len(msgs) == 0 {
		return textResponse("nothing to do"), nil
	}
	last := msgs[len(msgs)-1]

	// Deferred-call reminder: the loop is nudging the model to finish
	// pending calls. Re-issue the first pending call by name, using
	// the arguments recorded when the multi-call turn was issued.
	if last.Role == "user" && strings.HasPrefix(last.Content, "You still have pending tool calls") {
		names := parseReminderNames(last.Content)
		for _, name := range names {
			args, ok := m.pendingArgs[name]
			if !ok {
				args = "{}"
			}
			return toolCallResponse("call-reminder-"+name, name, args), nil
		}
		return textResponse("no pending calls left"), nil
	}

	cmd, phase, ok := lastCommand(req)
	if !ok || len(cmd) == 0 {
		return textResponse("hello"), nil
	}
	// A tool result is the last message: advance the chain.
	var lastResult string
	if last.Role == "tool" {
		lastResult = last.Content
	}

	switch cmd[0] {
	case "CALC":
		if phase == 0 {
			return toolCallResponse("call-1", "calculator", jsonArgs(map[string]string{"expression": strings.Join(cmd[1:], " ")})), nil
		}
		return textResponse("The answer is " + calcResult(lastResult) + "."), nil
	case "CALCWRITE":
		// CALCWRITE <expr> <file>
		expr := cmd[1]
		file := cmd[2]
		path := filepath.Join(m.dir, file)
		switch phase {
		case 0:
			return toolCallResponse("call-1", "calculator", jsonArgs(map[string]string{"expression": expr})), nil
		case 1:
			return toolCallResponse("call-2", "file_write", jsonArgs(map[string]string{"path": path, "content": calcResult(lastResult)})), nil
		case 2:
			return toolCallResponse("call-3", "file_read", jsonArgs(map[string]string{"path": path})), nil
		default:
			return textResponse("Verified: the file contains " + strings.TrimSpace(lastResult) + "."), nil
		}
	case "SHELL":
		if phase == 0 {
			return toolCallResponse("call-1", "shell_exec", jsonArgs(map[string]string{"command": strings.Join(cmd[1:], " ")})), nil
		}
		return textResponse("The shell printed: " + strings.TrimSpace(lastResult)), nil
	case "MULTI":
		if phase == 0 {
			m.pendingArgs = map[string]string{
				"calculator": jsonArgs(map[string]string{"expression": "7*6"}),
				"datetime":   "{}",
			}
			m.multiCallTurns++
			return &model.ChatResponse{
				Message: model.Message{
					Role:    "assistant",
					Content: "",
					ToolCalls: []model.ToolCall{
						{ID: "call-1", Name: "calculator", Arguments: m.pendingArgs["calculator"]},
						{ID: "call-2", Name: "datetime", Arguments: "{}"},
					},
				},
				FinishReason: "tool_calls",
			}, nil
		}
		if phase == 1 {
			return toolCallResponse("call-2", "datetime", "{}"), nil
		}
		return textResponse("Both tools ran."), nil
	case "MULTIANSWER":
		if phase == 0 {
			m.pendingArgs = map[string]string{
				"calculator": jsonArgs(map[string]string{"expression": "7*6"}),
				"datetime":   "{}",
			}
			m.multiCallTurns++
			return &model.ChatResponse{
				Message: model.Message{
					Role:    "assistant",
					Content: "",
					ToolCalls: []model.ToolCall{
						{ID: "call-1", Name: "calculator", Arguments: m.pendingArgs["calculator"]},
						{ID: "call-2", Name: "datetime", Arguments: "{}"},
					},
				},
				FinishReason: "tool_calls",
			}, nil
		}
		if phase == 1 {
			// Answer prematurely while datetime is still
			// deferred: the loop must remind, not finish.
			return textResponse("The calculator says 42."), nil
		}
		return textResponse("All done: calculator said 42 and I checked the time."), nil
	case "DENIED":
		if phase == 0 {
			return toolCallResponse("call-1", "shell_exec", jsonArgs(map[string]string{"command": "rm -rf /"})), nil
		}
		return textResponse("That command was refused, as it should be."), nil
	case "BADARGS":
		if phase == 0 {
			return toolCallResponse("call-1", "calculator", "{not json"), nil
		}
		if phase == 1 {
			// The previous result was the invalid-arguments error
			// text; retry with correct arguments.
			return toolCallResponse("call-2", "calculator", jsonArgs(map[string]string{"expression": strings.Join(cmd[1:], " ")})), nil
		}
		return textResponse("The answer is " + calcResult(lastResult) + "."), nil
	case "BADNAME":
		if phase == 0 {
			return toolCallResponse("call-1", "no_such_tool", "{}"), nil
		}
		return textResponse("Unknown tool, noted."), nil
	case "LOOP":
		return toolCallResponse("call-1", "calculator", jsonArgs(map[string]string{"expression": "1+1"})), nil
	case "CHAT":
		return textResponse("Hello! I am Orion, here to help."), nil
	case "RECALL":
		// Answer from the earlier turns visible in this request:
		// find the most recent calculator result in history.
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == "tool" && strings.Contains(msgs[i].Content, " = ") {
				parts := strings.SplitN(msgs[i].Content, " = ", 2)
				return textResponse("Earlier you asked me to calculate " + parts[0] + " and the answer was " + strings.TrimSpace(parts[1]) + "."), nil
			}
		}
		return textResponse("I don't see an earlier calculation in our history."), nil
	case "FAIL5XX":
		return nil, errors.New("scripted: simulated HTTP 500 (provider retries exhausted)")
	case "FLAKY":
		// Two internal attempts already simulated in Chat before
		// decide; now behave as the subcommand.
		sub := append([]string(nil), cmd[1:]...)
		if len(sub) == 0 {
			return textResponse("nothing to do"), nil
		}
		// Re-dispatch on the subcommand with phase 0 semantics:
		// rebuild a synthetic request is overkill — handle the
		// common CALC subcommand directly.
		if sub[0] == "CALC" {
			if phase == 0 {
				return toolCallResponse("call-1", "calculator", jsonArgs(map[string]string{"expression": strings.Join(sub[1:], " ")})), nil
			}
			return textResponse("The answer is " + calcResult(lastResult) + "."), nil
		}
		return textResponse("flaky subcommand done"), nil
	case "NILRESP":
		return nil, nil
	case "EMPTY":
		return &model.ChatResponse{Message: model.Message{Role: "assistant", Content: ""}, FinishReason: "stop"}, nil
	case "HANG":
		// Chat already blocks on ctx in the hang branch; Stream
		// must not hang forever either.
		return nil, errors.New("scripted: hang requested on a non-Chat path")
	case "BIG":
		if phase == 0 {
			return toolCallResponse("call-1", "file_read", jsonArgs(map[string]string{"path": filepath.Join(m.dir, cmd[1])})), nil
		}
		return textResponse("Read the big file."), nil
	default:
		return textResponse("I don't understand that command."), nil
	}
}

// parseReminderNames extracts the tool names from the loop's
// deferred-call reminder ("...pending tool calls from your previous
// message: a, b. Call them, ...").
func parseReminderNames(reminder string) []string {
	rest := strings.TrimPrefix(reminder, "You still have pending tool calls from your previous message: ")
	if idx := strings.Index(rest, "."); idx >= 0 {
		rest = rest[:idx]
	}
	var names []string
	for _, n := range strings.Split(rest, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// calcResult pulls the value out of a calculator result ("expr = 396").
func calcResult(toolResult string) string {
	if idx := strings.LastIndex(toolResult, " = "); idx >= 0 {
		return strings.TrimSpace(toolResult[idx+3:])
	}
	return strings.TrimSpace(toolResult)
}

// jsonArgs marshals a string map to a JSON object string (never
// hand-escaped).
func jsonArgs(m map[string]string) string {
	b, err := json.Marshal(m)
	if err != nil {
		panic(fmt.Sprintf("audit3 harness: jsonArgs: %v", err))
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Harness: conversation definitions, runner, history validation
// ---------------------------------------------------------------------------

type audit3Turn struct {
	input string
	// wantErr: substring the Run error must contain ("" = expect success).
	wantErr string
	// wantContains: substring the reply must contain ("" = no check).
	wantContains string
	// checkFile: file (relative to the conversation temp dir) that must
	// exist and contain wantFileContains after this turn.
	checkFile        string
	wantFileContains string
	// wantDuration: [min, max] bounds for this turn's wall time.
	minDuration time.Duration
	maxDuration time.Duration
}

type audit3Conv struct {
	name string
	// stream: drive the turns through RunStream instead of Run.
	stream bool
	// truncStream: first Stream call emits a chunk, then errors.
	truncStream bool
	// hang: first Chat call blocks until the step timeout fires.
	hang bool
	// flaky: simulate two internal provider failures per call.
	flaky bool
	// wantReminders: minimum deferred-call reminders the loop must emit.
	wantReminders int
	// wantInternalAtt: provider-internal retries that must have happened.
	wantInternalAtt int
	turns           []audit3Turn
}

type audit3Outcome struct {
	conv         string
	turns        int
	ok           bool
	detail       string
	duration     time.Duration
	slowestTurn  time.Duration
	providerCall int
	maxReqTokens int
	reminders    int
	multiCalls   int
	internalAtt  int
	failureMode  string // "" on success; otherwise the mode exercised
}

// audit3HistoryClean validates the stored conversation: every assistant
// tool call must be immediately followed by its tool result, and no
// tool result may be orphaned. Returns a description of the first
// problem, or "".
func audit3HistoryClean(msgs []model.Message) string {
	for i, m := range msgs {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				found := false
				for j := i + 1; j < len(msgs) && msgs[j].Role == "tool"; j++ {
					if msgs[j].ToolCallID == tc.ID {
						found = true
					}
				}
				if !found {
					return fmt.Sprintf("message %d: dangling tool call %q (id %q) with no result", i, tc.Name, tc.ID)
				}
			}
		}
		if m.Role == "tool" {
			requested := false
			for j := i - 1; j >= 0; j-- {
				if msgs[j].Role == "assistant" {
					for _, tc := range msgs[j].ToolCalls {
						if tc.ID == m.ToolCallID {
							requested = true
						}
					}
					break
				}
				if msgs[j].Role != "tool" {
					break
				}
			}
			if !requested {
				return fmt.Sprintf("message %d: orphan tool result (id %q)", i, m.ToolCallID)
			}
		}
	}
	return ""
}

// runAudit3Conv executes one scripted conversation against a fresh
// agent, registry, store and temp dir, and returns its outcome.
func runAudit3Conv(t *testing.T, conv audit3Conv) audit3Outcome {
	t.Helper()
	out := audit3Outcome{conv: conv.name, turns: len(conv.turns), ok: true}
	dir := t.TempDir()
	// The BIG scenario needs a large file present before the turn.
	for _, turn := range conv.turns {
		if strings.HasPrefix(turn.input, "BIG ") {
			name := strings.Fields(turn.input)[1]
			if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Repeat("w", 3*1024*1024)), 0o600); err != nil {
				t.Fatalf("write big file: %v", err)
			}
		}
	}
	reg, err := tools.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	mem := newFakeStore()
	prov := &scriptedModel{dir: dir, truncStreamOnce: conv.truncStream, hangOnce: conv.hang, flakyLeft: boolToInt(conv.flaky) * 2}
	cfg := config.Default()
	cfg.MaxSteps = 8
	cfg.TimeoutSeconds = 5 // a hung provider must be caught in ~5s, not 120s
	agent := newTestAgent(t, cfg, prov, reg, mem)

	start := time.Now()
	for ti, turn := range conv.turns {
		// Watchdog: 10s per turn. The agent's own 5s step timeout
		// must fire first for a hang; if the watchdog fires, the
		// loop itself hung and that is recorded, not hidden.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		t0 := time.Now()
		var reply string
		var runErr error
		if conv.stream {
			var chunks []string
			reply, runErr = agent.RunStream(ctx, turn.input, func(chunk string) error {
				chunks = append(chunks, chunk)
				return nil
			})
			if runErr == nil && strings.Join(chunks, "") != reply {
				out.ok = false
				out.detail = fmt.Sprintf("turn %d: streamed chunks %q do not join to reply %q", ti+1, strings.Join(chunks, ""), reply)
			}
		} else {
			reply, runErr = agent.Run(ctx, turn.input)
		}
		dur := time.Since(t0)
		cancel()
		if dur > out.slowestTurn {
			out.slowestTurn = dur
		}
		if turn.wantErr != "" {
			if runErr == nil {
				out.ok = false
				out.detail = fmt.Sprintf("turn %d (%q): expected error containing %q, got success with reply %q", ti+1, turn.input, turn.wantErr, reply)
				break
			}
			if !strings.Contains(runErr.Error(), turn.wantErr) {
				out.ok = false
				out.detail = fmt.Sprintf("turn %d (%q): error %q does not contain %q", ti+1, turn.input, runErr.Error(), turn.wantErr)
				break
			}
			out.failureMode = turn.wantErr
		} else {
			if runErr != nil {
				out.ok = false
				out.detail = fmt.Sprintf("turn %d (%q): unexpected error: %v", ti+1, turn.input, runErr)
				if errors.Is(runErr, context.DeadlineExceeded) || strings.Contains(runErr.Error(), "cancelled") {
					out.failureMode = "timeout/hang"
				} else {
					out.failureMode = "unexpected error"
				}
				break
			}
			if turn.wantContains != "" && !strings.Contains(reply, turn.wantContains) {
				out.ok = false
				out.detail = fmt.Sprintf("turn %d (%q): reply %q does not contain %q", ti+1, turn.input, reply, turn.wantContains)
				out.failureMode = "wrong answer"
				break
			}
		}
		if turn.minDuration > 0 && dur < turn.minDuration {
			out.ok = false
			out.detail = fmt.Sprintf("turn %d (%q): took %v, expected at least %v (step timeout did not fire?)", ti+1, turn.input, dur, turn.minDuration)
			break
		}
		if turn.maxDuration > 0 && dur > turn.maxDuration {
			out.ok = false
			out.detail = fmt.Sprintf("turn %d (%q): took %v, over max %v", ti+1, turn.input, dur, turn.maxDuration)
			out.failureMode = "slow turn"
			break
		}
		if turn.checkFile != "" {
			b, err := os.ReadFile(filepath.Join(dir, turn.checkFile))
			if err != nil {
				out.ok = false
				out.detail = fmt.Sprintf("turn %d: expected file %s: %v", ti+1, turn.checkFile, err)
				out.failureMode = "missing file"
				break
			}
			if !strings.Contains(string(b), turn.wantFileContains) {
				out.ok = false
				out.detail = fmt.Sprintf("turn %d: file %s contains %q, want %q", ti+1, turn.checkFile, string(b), turn.wantFileContains)
				out.failureMode = "file mismatch"
				break
			}
		}
		// History must stay clean after every turn, including
		// after failed turns: a dangling call would 400 the next
		// provider call.
		if problem := audit3HistoryClean(mem.allMessages()); problem != "" {
			out.ok = false
			out.detail = fmt.Sprintf("turn %d (%q): history corrupted: %s", ti+1, turn.input, problem)
			out.failureMode = "history corruption"
			break
		}
	}
	out.duration = time.Since(start)
	prov.mu.Lock()
	out.providerCall = prov.chatCalls + prov.streamCalls
	out.maxReqTokens = prov.maxReqTokens
	out.reminders = prov.reminderSeen
	out.multiCalls = prov.multiCallTurns
	out.internalAtt = prov.internalAttempts
	if len(prov.invalidRequests) > 0 {
		out.ok = false
		out.detail = fmt.Sprintf("loop sent invalid request(s) to provider: %s", strings.Join(prov.invalidRequests, "; "))
		out.failureMode = "invalid provider request"
	}
	prov.mu.Unlock()
	if conv.wantReminders > 0 && out.reminders < conv.wantReminders {
		out.ok = false
		out.detail = fmt.Sprintf("expected at least %d deferred-call reminder(s), saw %d", conv.wantReminders, out.reminders)
		out.failureMode = "reminder missing"
	}
	if conv.wantInternalAtt > 0 && out.internalAtt < conv.wantInternalAtt {
		out.ok = false
		out.detail = fmt.Sprintf("expected at least %d provider-internal retries, saw %d", conv.wantInternalAtt, out.internalAtt)
		out.failureMode = "retry missing"
	}
	return out
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// audit3Conversations builds the full volume set: 128 scripted
// Telegram-style conversations across every scenario class.
func audit3Conversations() []audit3Conv {
	var convs []audit3Conv

	// 30x calc -> write -> read round-trips, each followed by a
	// history-recall turn and a second calculation.
	for i := 0; i < 30; i++ {
		a, b, c := 10+i, 7+(i%5), 3+(i%4)
		expr := fmt.Sprintf("%d*%d+%d", a, b, c)
		want := fmt.Sprintf("%d", a*b+c)
		file := fmt.Sprintf("result-%02d.txt", i)
		convs = append(convs, audit3Conv{
			name: fmt.Sprintf("calcwrite-%02d", i),
			turns: []audit3Turn{
				{input: "CALCWRITE " + expr + " " + file, wantContains: "Verified: the file contains " + want + ".", checkFile: file, wantFileContains: want},
				{input: "RECALL", wantContains: want},
				{input: "CALC " + fmt.Sprintf("%d+%d", i, i), wantContains: fmt.Sprintf("The answer is %d.", 2*i)},
			},
		})
	}

	// 15x shell_exec turns.
	for i := 0; i < 15; i++ {
		convs = append(convs, audit3Conv{
			name: fmt.Sprintf("shell-%02d", i),
			turns: []audit3Turn{
				{input: fmt.Sprintf("SHELL echo hello-audit3-%d", i), wantContains: "hello-audit3-" + fmt.Sprint(i)},
				{input: fmt.Sprintf("SHELL echo second-%d", i), wantContains: "second-" + fmt.Sprint(i)},
			},
		})
	}

	// 10x multi-call turns (loop executes the first, model re-plans
	// the second) and 10x premature-answer turns (reminder path).
	for i := 0; i < 10; i++ {
		convs = append(convs, audit3Conv{
			name: fmt.Sprintf("multi-%02d", i),
			turns: []audit3Turn{
				{input: "MULTI", wantContains: "Both tools ran."},
				{input: "CALC 3*3", wantContains: "The answer is 9."},
			},
		})
		convs = append(convs, audit3Conv{
			name:          fmt.Sprintf("multianswer-%02d", i),
			wantReminders: 1,
			turns: []audit3Turn{
				{input: "MULTIANSWER", wantContains: "All done:"},
				{input: "CALC 4*4", wantContains: "The answer is 16."},
			},
		})
	}

	// 5x denied destructive command, then a normal turn.
	for i := 0; i < 5; i++ {
		convs = append(convs, audit3Conv{
			name: fmt.Sprintf("denied-%02d", i),
			turns: []audit3Turn{
				{input: "DENIED", wantContains: "refused"},
				{input: "CALC 5*5", wantContains: "The answer is 25."},
			},
		})
	}

	// 10x malformed JSON tool-call arguments, recovered in-turn.
	for i := 0; i < 10; i++ {
		convs = append(convs, audit3Conv{
			name: fmt.Sprintf("badargs-%02d", i),
			turns: []audit3Turn{
				{input: "BADARGS 6*7", wantContains: "The answer is 42."},
				{input: "CALC 2*8", wantContains: "The answer is 16."},
			},
		})
	}

	// 10x flaky provider: two internal failures, then success in the
	// same call (provider-level retry, invisible to the loop).
	for i := 0; i < 10; i++ {
		convs = append(convs, audit3Conv{
			name:            fmt.Sprintf("flaky-%02d", i),
			flaky:           true,
			wantInternalAtt: 2,
			turns: []audit3Turn{
				{input: "FLAKY CALC 8*9", wantContains: "The answer is 72."},
				{input: "CALC 1+1", wantContains: "The answer is 2."},
			},
		})
	}

	// 5x hard 5xx: the turn fails, the next turn must recover.
	for i := 0; i < 5; i++ {
		convs = append(convs, audit3Conv{
			name: fmt.Sprintf("fail5xx-%02d", i),
			turns: []audit3Turn{
				{input: "FAIL5XX", wantErr: "provider call"},
				{input: "CALC 9*9", wantContains: "The answer is 81."},
			},
		})
	}

	// 3x nil provider response: the turn fails, the next recovers.
	for i := 0; i < 3; i++ {
		convs = append(convs, audit3Conv{
			name: fmt.Sprintf("nilresp-%02d", i),
			turns: []audit3Turn{
				{input: "NILRESP", wantErr: "nil response"},
				{input: "CALC 3*7", wantContains: "The answer is 21."},
			},
		})
	}

	// 3x empty final response: the loop must now reject it as an
	// error (Audit 3 fix: the caller never gets an empty reply with
	// a nil error) instead of silently returning "", and the next
	// turn must recover with clean history.
	for i := 0; i < 3; i++ {
		convs = append(convs, audit3Conv{
			name: fmt.Sprintf("empty-%02d", i),
			turns: []audit3Turn{
				{input: "EMPTY", wantErr: "empty response"},
				{input: "CALC 2*2", wantContains: "The answer is 4."},
			},
		})
	}

	// 3x max-steps exhaustion, then recovery.
	for i := 0; i < 3; i++ {
		convs = append(convs, audit3Conv{
			name: fmt.Sprintf("loop-%02d", i),
			turns: []audit3Turn{
				{input: "LOOP", wantErr: "max steps exceeded"},
				{input: "CALC 6*6", wantContains: "The answer is 36."},
			},
		})
	}

	// 3x unknown tool name, fed back and answered.
	for i := 0; i < 3; i++ {
		convs = append(convs, audit3Conv{
			name: fmt.Sprintf("badname-%02d", i),
			turns: []audit3Turn{
				{input: "BADNAME", wantContains: "Unknown tool"},
				{input: "CALC 7*7", wantContains: "The answer is 49."},
			},
		})
	}

	// 6x plain multi-turn chats with a recall turn in the middle.
	for i := 0; i < 6; i++ {
		convs = append(convs, audit3Conv{
			name: fmt.Sprintf("chat-%02d", i),
			turns: []audit3Turn{
				{input: fmt.Sprintf("CALC %d*%d", 11+i, 3), wantContains: fmt.Sprintf("The answer is %d.", (11+i)*3)},
				{input: "CHAT", wantContains: "Hello!"},
				{input: "RECALL", wantContains: fmt.Sprintf("%d", (11+i)*3)},
				{input: "CALC 10+10", wantContains: "The answer is 20."},
			},
		})
	}

	// 3x truncated stream: the turn fails mid-stream, the next
	// (streamed) turn recovers.
	for i := 0; i < 3; i++ {
		convs = append(convs, audit3Conv{
			name:        fmt.Sprintf("truncstream-%02d", i),
			stream:      true,
			truncStream: true,
			turns: []audit3Turn{
				{input: "CHAT", wantErr: "truncated stream"},
				{input: "CALC 5+5", wantContains: "The answer is 10."},
			},
		})
	}

	// 2x hanging provider: the 5s step timeout must catch it (the
	// 10s watchdog must NOT be the thing that fires), then recovery.
	for i := 0; i < 2; i++ {
		convs = append(convs, audit3Conv{
			name: fmt.Sprintf("hang-%02d", i),
			hang: true,
			turns: []audit3Turn{
				{input: "HANG", wantErr: "cancelled", minDuration: 4 * time.Second, maxDuration: 9 * time.Second},
				{input: "CALC 2*3", wantContains: "The answer is 6."},
			},
		})
	}

	// 10x calcwrite chains over the streaming path (Telegram uses
	// RunStream).
	for i := 0; i < 10; i++ {
		a := 20 + i
		file := fmt.Sprintf("stream-%02d.txt", i)
		convs = append(convs, audit3Conv{
			name:   fmt.Sprintf("stream-calcwrite-%02d", i),
			stream: true,
			turns: []audit3Turn{
				{input: fmt.Sprintf("CALCWRITE %d*2 %s", a, file), wantContains: fmt.Sprintf("Verified: the file contains %d.", a*2), checkFile: file, wantFileContains: fmt.Sprintf("%d", a*2)},
				{input: "CALC 1+2", wantContains: "The answer is 3."},
			},
		})
	}

	return convs
}

// TestAudit3ReliabilityHarness runs the full volume set and reports
// real numbers: conversations, success rate, per-failure-mode counts,
// slowest turn, and the largest provider request seen. It fails if any
// conversation deviates from its expectation.
func TestAudit3ReliabilityHarness(t *testing.T) {
	convs := audit3Conversations()
	if len(convs) < 100 {
		t.Fatalf("harness defines %d conversations, want >= 100", len(convs))
	}
	var (
		totalTurns   int
		failed       []audit3Outcome
		byMode       = map[string]int{}
		slowest      time.Duration
		slowestConv  string
		maxReqTokens int
		totalCalls   int
		reminders    int
		multiCalls   int
		internalAtt  int
	)
	suiteStart := time.Now()
	for _, conv := range convs {
		out := runAudit3Conv(t, conv)
		totalTurns += out.turns
		totalCalls += out.providerCall
		reminders += out.reminders
		multiCalls += out.multiCalls
		internalAtt += out.internalAtt
		if out.maxReqTokens > maxReqTokens {
			maxReqTokens = out.maxReqTokens
		}
		if out.slowestTurn > slowest {
			slowest = out.slowestTurn
			slowestConv = out.conv
		}
		if !out.ok {
			failed = append(failed, out)
			if out.failureMode != "" {
				byMode[out.failureMode]++
			}
		} else if out.failureMode != "" {
			// Expected-failure conversations that behaved exactly
			// as designed (5xx, max steps, ...) count per mode too.
			byMode[out.failureMode+" (expected, recovered)"]++
		}
	}
	suiteDur := time.Since(suiteStart)
	t.Logf("AUDIT3 RELIABILITY: %d conversations, %d turns, %d provider calls, %d passed, %d failed, total %v",
		len(convs), totalTurns, totalCalls, len(convs)-len(failed), len(failed), suiteDur)
	t.Logf("AUDIT3 RELIABILITY: success rate %.1f%%", 100*float64(len(convs)-len(failed))/float64(len(convs)))
	t.Logf("AUDIT3 RELIABILITY: slowest turn %v (%s); largest provider request %d tokens; deferred reminders %d; multi-call turns %d; provider-internal retries %d",
		slowest, slowestConv, maxReqTokens, reminders, multiCalls, internalAtt)
	for mode, n := range byMode {
		t.Logf("AUDIT3 RELIABILITY: failure mode %q: %d conversation(s)", mode, n)
	}
	for _, f := range failed {
		t.Errorf("conversation %s FAILED: %s", f.conv, f.detail)
	}
}

// TestAudit3HistoryBudget32768 drives one turn against the real
// 32768-token history budget: seeded history plus a 3 MiB file read
// (capped to 256 KiB by the registry) far exceeds the budget, and the
// loop must keep going — trimming the request to budget without
// breaking tool-call pairing — and the turn after must still work.
// TestAudit3EmptyFinalAnswerIsError is the regression test for the
// Audit 3 loop fix: a provider reply with no content and no tool
// calls must be an error, not a silent empty success, the empty
// message must not be stored in memory, and the next turn must work.
func TestAudit3EmptyFinalAnswerIsError(t *testing.T) {
	prov := &fakeProvider{
		chatFn: func(call int, _ model.ChatRequest) (*model.ChatResponse, error) {
			if call == 1 {
				return &model.ChatResponse{
					Message:      model.Message{Role: "assistant", Content: "   "},
					FinishReason: "stop",
				}, nil
			}
			return textResponse("recovered"), nil
		},
	}
	mem := newFakeStore()
	a := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), mem)

	out, err := a.Run(context.Background(), "say nothing")
	if err == nil {
		t.Fatalf("Run with an empty final answer: expected error, got success %q", out)
	}
	if !strings.Contains(err.Error(), "empty response") {
		t.Fatalf("Run error = %q, want it to mention \"empty response\"", err.Error())
	}
	// Only the user message may be stored: no empty assistant
	// message, no dangling anything.
	msgs := mem.allMessages()
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("memory after empty reply = %d message(s), want exactly the 1 user message", len(msgs))
	}
	if problem := audit3HistoryClean(msgs); problem != "" {
		t.Fatalf("history corrupted after empty reply: %s", problem)
	}

	out, err = a.Run(context.Background(), "say something")
	if err != nil {
		t.Fatalf("recovery turn failed: %v", err)
	}
	if out != "recovered" {
		t.Fatalf("recovery turn = %q, want %q", out, "recovered")
	}
}

func TestAudit3HistoryBudget32768(t *testing.T) {
	dir := t.TempDir()
	bigPath := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(bigPath, []byte(strings.Repeat("w", 3*1024*1024)), 0o600); err != nil {
		t.Fatalf("write big file: %v", err)
	}
	mem := newFakeStore()
	// ~60 filler messages, ~850 tokens each: ~51k tokens of history,
	// on top of a 256 KiB (capped) tool result. Uniquely numbered so
	// the test can tell which ones the trimmer dropped.
	for i := 0; i < 30; i++ {
		_ = mem.AddMessage(model.Message{Role: "user", Content: fmt.Sprintf("old filler turn %03d ", i) + strings.Repeat("filler ", 200)})
		_ = mem.AddMessage(model.Message{Role: "assistant", Content: fmt.Sprintf("old filler answer %03d ", i) + strings.Repeat("answer ", 200)})
	}
	reg, err := tools.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	prov := &scriptedModel{dir: dir}
	cfg := config.Default()
	cfg.MaxSteps = 5
	cfg.TimeoutSeconds = 5
	cfg.MaxHistoryTokens = 32768 // the real production budget
	agent := newTestAgent(t, cfg, prov, reg, mem)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t0 := time.Now()
	out, err := agent.Run(ctx, "BIG big.txt")
	dur := time.Since(t0)
	if err != nil {
		t.Fatalf("Run over the 32768-token budget failed: %v", err)
	}
	if out != "Read the big file." {
		t.Fatalf("Run = %q, want %q", out, "Read the big file.")
	}
	prov.mu.Lock()
	maxTokens := prov.maxReqTokens
	invalid := append([]string(nil), prov.invalidRequests...)
	firstReq := append([]model.Message(nil), prov.firstReqMsgs...)
	prov.mu.Unlock()
	if len(invalid) > 0 {
		t.Fatalf("trimmed requests broke pairing: %s", strings.Join(invalid, "; "))
	}
	if maxTokens > 32768 {
		t.Fatalf("provider request peaked at %d tokens, over the 32768 budget", maxTokens)
	}
	// Trimming must have actually dropped the oldest filler: the
	// first request must not contain turn 000, but must still hold
	// the newest turns and the system prompt.
	var firstReqText strings.Builder
	for _, m := range firstReq {
		firstReqText.WriteString(m.Content)
		firstReqText.WriteString("\n")
	}
	joined := firstReqText.String()
	if strings.Contains(joined, "old filler turn 000") {
		t.Fatalf("first request still contains the oldest filler turn: trimming did not drop it")
	}
	if !strings.Contains(joined, "old filler turn 029") {
		t.Fatalf("first request lost the newest filler turn: trimming dropped too much")
	}
	if len(firstReq) == 0 || firstReq[0].Role != "system" {
		t.Fatalf("first request has no system message first")
	}
	t.Logf("AUDIT3 BUDGET: turn over 32768-token budget completed in %v; largest request %d tokens; oldest filler dropped, newest kept", dur, maxTokens)

	// The loop must stay alive after the trimmed turn.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	out2, err := agent.Run(ctx2, "CALC 11*11")
	if err != nil {
		t.Fatalf("turn after the budget turn failed: %v", err)
	}
	if out2 != "The answer is 121." {
		t.Fatalf("turn after budget = %q, want %q", out2, "The answer is 121.")
	}
	if problem := audit3HistoryClean(mem.allMessages()); problem != "" {
		t.Fatalf("history corrupted after budget turn: %s", problem)
	}
}
