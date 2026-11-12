// Live multi-turn end-to-end test: the REAL Orion agent loop against a
// REAL local model through the Ollama provider, over a long
// conversation with a restart in the middle.
//
// Skipped unless ORION_LIVE_OLLAMA_E2E=1. Model from
// ORION_LIVE_OLLAMA_MODEL (default "qwen2.5-0.5b-local"). See
// docs/E2E_LIVE_OLLAMA.md for why the 0.5B model runs here instead of
// a real Hermes model (this VM has ~700MiB free; hermes3:3b needs
// ~8GB RAM).
//
// Conversation plan — every turn is single-purpose with one fact or
// one tool call, the shape the 0.5B model handles reliably in a fresh
// agent (see the single-turn live E2E: it makes one tool call and
// then answers):
//
//	T1  tell the agent the number (17) and ask it back — no tool
//	T2  calculator: favourite number * 3 (= 51) — first tool call,
//	    while the context is still short; model must recall 17
//	    from T1 (no number is given in this prompt)
//	T3  tell the agent the colour (blue) and ask it back — no tool
//	    [deterministic probe: a second FileStore opened on the same
//	    data dir WITHOUT any shutdown must already see T1-T3 —
//	    FileStore persists after every message, so even a kill -9
//	    loses nothing already answered]
//	    RESTART: Shutdown + new FileStore + new Agent on the same dir
//	T4  recall the number after restart — reply "17"
//	T5  file_write the computed result (51) to a temp file
//	T6  file_read the file back — reply "51"
//	T7  LATE RECALL PROBE (not fatal): favourite number again. By
//	    now the conversation is ~20 messages and the agent only
//	    feeds the last 10 into the model (maxRecentMessages), so
//	    the T1 messages have slid out of the context window. This
//	    turn measures that truncation effect, honestly, instead of
//	    pretending long conversations recall everything.
//
// Deterministic checks (independent of model prose) run alongside
// every model check: the on-disk memory.json must contain the fact
// strings, the written file must contain exactly "51", and tool
// results in the transcript must contain the expected values, so a
// hallucinated answer cannot pass.
//
// Prompt design notes, learned from runs 1-2 with this model — the
// 0.5B model is extremely literal and echoes prompt wording, so the
// prompts avoid giving it a reply template or trigger words:
//   - Run 1: T1/T2 said "Reply with only the words: Noted 17". The
//     model learned the "Noted ..." style and applied it to every
//     later turn, answering "Noted 17 * 3 = 51" / "Noted 51 ...
//     written" in prose and never calling a tool.
//   - Run 2: T1/T2 said "Remember this fact: ... Reply briefly to
//     confirm you will remember it." The model called web_fetch
//     with a hallucinated URL (https://www.example.com/fact/17,
//     404) and then parroted "Confirm you will remember the fact."
//     — the words "remember"/"fact" baited a web lookup, and the
//     "confirm" wording was echoed verbatim. It then fabricated
//     tool use in prose ("Calculator tool called. Result: 54" —
//     wrong arithmetic, no tool call at all). The facts still
//     persisted (they are in the user messages), so recall T3
//     passed anyway.
//   - Hence: remember-turns are plain statements followed by a
//     natural ask-back question ("My favourite number is 17. What
//     is my favourite number?"), with no "remember"/"fact"/
//     "confirm" wording and no reply template; tool turns carry an
//     explicit "you must call the tool" mandate. Assertions are
//     unchanged — a tool result must still appear in the
//     transcript and the file must really hold 51.
//
// Retries: each of T1-T6 gets up to 2 attempts in the same
// conversation (a re-ask, not a fresh agent — the failed attempt
// stays in history, exactly as it would for a real user correcting
// the agent). Failures are recorded per turn with timings and the
// test continues to the next turn so one bad turn yields a full
// report instead of aborting the run; T1-T6 failures are reported
// with t.Errorf at the end. T7 never fails the test.
//
// Run with (the default 10m go-test timeout is too short):
//
//	ORION_LIVE_OLLAMA_E2E=1 ORION_LIVE_OLLAMA_MODEL=qwen2.5-0.5b-local \
//	  go test ./internal/core/ -run TestLiveOllamaMultiTurnE2E -count=1 -v -timeout 25m
package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/memory"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/tools"
)

// requireLiveOllamaModel checks the Ollama daemon is up and the named
// model is present, skipping with a clear reason otherwise.
func requireLiveOllamaModel(t *testing.T, modelName string) {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("http://localhost:11434/api/tags")
	if err != nil {
		t.Skipf("ollama daemon not reachable at http://localhost:11434: %v", err)
	}
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		resp.Body.Close()
		t.Fatalf("decode /api/tags: %v", err)
	}
	resp.Body.Close()
	for _, m := range tags.Models {
		if m.Name == modelName || strings.HasPrefix(m.Name, modelName+":") {
			return
		}
	}
	t.Skipf("model %q not pulled; ollama list shows %v", modelName, tags.Models)
}

// liveOllamaModelName returns the model for live Ollama tests.
func liveOllamaModelName() string {
	modelName := os.Getenv("ORION_LIVE_OLLAMA_MODEL")
	if strings.TrimSpace(modelName) == "" {
		modelName = "qwen2.5-0.5b-local"
	}
	return modelName
}

// diskContains reports whether the persisted memory.json in dataDir
// contains substr. It reads the file straight off disk, so it proves
// persistence independently of any in-memory state or model reply.
func diskContains(t *testing.T, dataDir, substr string) bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dataDir, "memory.json"))
	if err != nil {
		t.Logf("diskContains: read memory.json: %v", err)
		return false
	}
	return bytes.Contains(raw, []byte(substr))
}

// historyContains reports whether any of the store's retained messages
// contains substr.
func historyContains(t *testing.T, store memory.Store, substr string) bool {
	t.Helper()
	msgs, err := store.RecentMessages(0)
	if err != nil {
		t.Logf("historyContains: RecentMessages: %v", err)
		return false
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, substr) {
			return true
		}
	}
	return false
}

// toolResultContains reports whether the transcript holds a tool
// result containing want — i.e. the model really called a tool and
// the loop really executed it, rather than the answer being
// hallucinated.
func toolResultContains(t *testing.T, store memory.Store, want string) bool {
	t.Helper()
	msgs, err := store.RecentMessages(0)
	if err != nil {
		t.Logf("toolResultContains: RecentMessages: %v", err)
		return false
	}
	for _, m := range msgs {
		if m.Role == "tool" && strings.Contains(m.Content, want) {
			return true
		}
	}
	return false
}

// turnOutcome is the per-turn record for the final report.
type turnOutcome struct {
	name     string
	attempts int
	elapsed  time.Duration
	passed   bool
	detail   string
}

// TestLiveOllamaMultiTurnE2E runs a 6-turn (plus 1 probe turn)
// conversation against the live local model, with an agent restart
// between turns 3 and 4.
func TestLiveOllamaMultiTurnE2E(t *testing.T) {
	if os.Getenv("ORION_LIVE_OLLAMA_E2E") != "1" {
		t.Skip("live Ollama multi-turn E2E: set ORION_LIVE_OLLAMA_E2E=1 to run (needs a local model)")
	}
	modelName := liveOllamaModelName()
	requireLiveOllamaModel(t, modelName)

	prov, err := model.NewProvider(model.ProviderConfig{
		Name:    "ollama",
		BaseURL: "http://localhost:11434",
		Model:   modelName,
	})
	if err != nil {
		t.Fatalf("model.NewProvider(ollama): %v", err)
	}
	reg, err := tools.DefaultRegistry()
	if err != nil {
		t.Fatalf("tools.DefaultRegistry: %v", err)
	}
	cfg := config.Default()
	cfg.MaxSteps = 8
	cfg.TimeoutSeconds = 300
	// Temperature 0: the most deterministic output this provider
	// offers, so repeated runs measure the loop rather than sampling
	// noise.
	temp := 0.0
	cfg.Temperature = &temp

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	dataDir := t.TempDir()
	workdir := t.TempDir()
	resultPath := filepath.Join(workdir, "multiturn-result.txt")

	// newAgent builds an agent on the shared store, with a fresh log
	// buffer so each agent instance's log is attributable.
	newAgent := func(store memory.Store) (*Agent, *bytes.Buffer) {
		t.Helper()
		agent, err := NewAgent(cfg, prov, reg, store)
		if err != nil {
			t.Fatalf("NewAgent: %v", err)
		}
		var buf bytes.Buffer
		agent.SetLogger(log.New(&buf, "", log.LstdFlags))
		return agent, &buf
	}

	store, err := memory.NewFileStore(dataDir)
	if err != nil {
		t.Fatalf("memory.NewFileStore: %v", err)
	}
	agent, agentLog := newAgent(store)
	// storeFn always returns the CURRENT store (it changes at the
	// restart), so turn checks read the live transcript.
	storeFn := func() memory.Store { return store }

	var outcomes []turnOutcome

	// runTurn drives one conversation turn with up to two attempts in
	// the same conversation. check judges the attempt from the
	// agent's answer plus the store transcript / disk / files; it
	// returns (passed, detail). The second attempt re-asks with an
	// explicit correction, as a user would. Each attempt's answer,
	// timing and failure detail are logged so the report shows
	// exactly what the model did.
	runTurn := func(name, input, retryInput string, check func(answer string) (bool, string)) turnOutcome {
		t.Helper()
		out := turnOutcome{name: name}
		start := time.Now()
		for attempt := 1; attempt <= 2; attempt++ {
			prompt := input
			if attempt == 2 {
				prompt = retryInput
			}
			attemptStart := time.Now()
			answer, runErr := agent.Run(ctx, prompt)
			attemptElapsed := time.Since(attemptStart)
			out.attempts = attempt
			// Dump the transcript so a failure can be diagnosed
			// precisely: did the model call a tool (and with what
			// arguments), call the wrong tool, or answer in prose?
			if msgs, merr := storeFn().RecentMessages(0); merr == nil {
				for i, m := range msgs {
					t.Logf("turn %s attempt %d transcript msg %d role=%s content=%q toolcalls=%v toolcallid=%s", name, attempt, i, m.Role, m.Content, m.ToolCalls, m.ToolCallID)
				}
			}
			if runErr != nil {
				t.Logf("turn %s attempt %d: Run error after %v: %v (answer %q)", name, attempt, attemptElapsed, runErr, answer)
				out.detail = fmt.Sprintf("attempt %d Run error: %v", attempt, runErr)
				continue
			}
			passed, detail := check(answer)
			t.Logf("turn %s attempt %d: took %v, answer %q, passed=%v (%s)", name, attempt, attemptElapsed, answer, passed, detail)
			out.detail = fmt.Sprintf("attempt %d: %s", attempt, detail)
			if passed {
				out.passed = true
				break
			}
		}
		out.elapsed = time.Since(start)
		outcomes = append(outcomes, out)
		t.Logf("turn %s: %s in %v over %d attempt(s)", name, map[bool]string{true: "PASS", false: "FAIL"}[out.passed], out.elapsed, out.attempts)
		return out
	}

	// T1: state the number and ask it back. Plain statement plus a
	// natural question — no "remember"/"fact" wording (run 2: that
	// baited a web_fetch of a hallucinated fact URL) and no reply
	// template (run 1: "Noted ..." was echoed into every later
	// turn, including fabricated tool confirmations).
	runTurn("T1-state-number",
		"My favourite number is 17. What is my favourite number?",
		"That reply was wrong. My favourite number is 17. What is my favourite number?",
		func(answer string) (bool, string) {
			disk := diskContains(t, dataDir, "favourite number is 17")
			if !strings.Contains(answer, "17") {
				return false, fmt.Sprintf("answer lacks 17; persisted-on-disk=%v", disk)
			}
			if !disk {
				return false, "answer has 17 but the fact is NOT in memory.json on disk (persistence bug)"
			}
			return true, fmt.Sprintf("answer has 17; fact persisted on disk=%v", disk)
		})

	// T2: dependent compute, while the context is still short — the
	// model must recall 17 from T1 (no number is given in this
	// prompt) and call the calculator. The mandate is explicit
	// because in runs 1-2 the model answered the arithmetic in
	// prose ("17 * 3 = 51", "Calculator tool called. Result: 54")
	// without ever calling the tool.
	runTurn("T2-compute-favourite-times-3",
		"Use the calculator tool to compute my favourite number times 3, and tell me the result. You must call the calculator tool for this — do not answer from your own arithmetic without calling it.",
		"That did not work — you answered without calling the calculator tool. Use the calculator tool to compute my favourite number times 3, and tell me the result.",
		func(answer string) (bool, string) {
			toolOK := toolResultContains(t, storeFn(), "51")
			if !strings.Contains(answer, "51") {
				return false, fmt.Sprintf("answer lacks 51; calculator tool result 51 seen=%v", toolOK)
			}
			if !toolOK {
				return false, "answer has 51 but no calculator tool result with 51 in the transcript (hallucinated, not computed)"
			}
			return true, "answer 51 backed by a calculator tool result"
		})

	// T3: state the colour and ask it back.
	runTurn("T3-state-colour",
		"My favourite colour is blue. What is my favourite colour?",
		"That reply was wrong. My favourite colour is blue. What is my favourite colour?",
		func(answer string) (bool, string) {
			disk := diskContains(t, dataDir, "favourite colour is blue")
			if !strings.Contains(strings.ToLower(answer), "blue") {
				return false, fmt.Sprintf("answer lacks blue; persisted-on-disk=%v", disk)
			}
			if !disk {
				return false, "answer has blue but the fact is NOT in memory.json on disk (persistence bug)"
			}
			return true, fmt.Sprintf("answer has blue; fact persisted on disk=%v", disk)
		})

	// Deterministic crash-survival probe (no model involved): open a
	// SECOND store on the same data dir without shutting the first
	// one down — the equivalent of the agent process being killed
	// (kill -9 / power loss) with no graceful Shutdown. FileStore
	// persists after every message, so the new store must already
	// see all three facts.
	probeStore, err := memory.NewFileStore(dataDir)
	if err != nil {
		t.Fatalf("crash-survival probe: NewFileStore on live data dir: %v", err)
	}
	probeOK := historyContains(t, probeStore, "favourite number is 17") &&
		historyContains(t, probeStore, "favourite colour is blue")
	probeMsgs, _ := probeStore.RecentMessages(0)
	t.Logf("crash-survival probe (second store, no shutdown): sees all facts=%v (%d messages on disk)", probeOK, len(probeMsgs))
	if err := probeStore.Close(); err != nil {
		t.Fatalf("crash-survival probe: Close: %v", err)
	}
	if !probeOK {
		t.Errorf("crash-survival probe FAILED: a store opened without shutdown does not see T1-T3 — messages are not durable before Shutdown")
	}

	// RESTART: graceful Shutdown, then a fresh store and agent on
	// the SAME data dir. This is the restart the test is about:
	// the new agent must recall the pre-restart facts and finish
	// the dependent chain (write the computed 51, read it back).
	if err := agent.Shutdown(); err != nil {
		t.Fatalf("restart: agent Shutdown: %v", err)
	}
	t.Logf("restart: agent log before shutdown:\n%s", agentLog.String())
	store, err = memory.NewFileStore(dataDir)
	if err != nil {
		t.Fatalf("restart: NewFileStore: %v", err)
	}
	agent, agentLog = newAgent(store)
	restartOK := historyContains(t, store, "favourite number is 17") &&
		historyContains(t, store, "favourite colour is blue")
	reloaded, _ := store.RecentMessages(0)
	t.Logf("restart: reloaded store sees both facts=%v (%d messages reloaded)", restartOK, len(reloaded))
	if !restartOK {
		t.Errorf("restart FAILED: reloaded store does not contain the pre-restart facts")
	}

	// T4: recall the number after the restart.
	runTurn("T4-recall-number-after-restart",
		"What is my favourite number? Reply briefly with just the number.",
		"That answer was wrong. Look back at our earlier conversation, before the restart, and tell me my favourite number. Reply briefly with just the number.",
		func(answer string) (bool, string) {
			if strings.Contains(answer, "17") {
				return true, "answer recalls 17"
			}
			return false, fmt.Sprintf("answer %q does not contain 17 (forgot the fact, or recalled a wrong number)", answer)
		})

	// T5: write the computed result to a file.
	runTurn("T5-write-result-to-file",
		fmt.Sprintf("Use the file_write tool to write the number 51 (only the number, no other text) to the file %s. You must call the file_write tool — do not just say you wrote it.", resultPath),
		fmt.Sprintf("That did not work — you answered without calling the file_write tool. Use the file_write tool to write the number 51 (only the number, no other text) to the file %s.", resultPath),
		func(answer string) (bool, string) {
			raw, rerr := os.ReadFile(resultPath)
			if rerr != nil {
				return false, fmt.Sprintf("result file unreadable: %v", rerr)
			}
			toolOK := toolResultContains(t, storeFn(), "wrote 2 bytes")
			if strings.TrimSpace(string(raw)) != "51" {
				return false, fmt.Sprintf("file holds %q, want 51; file_write tool result seen=%v", strings.TrimSpace(string(raw)), toolOK)
			}
			if !toolOK {
				return false, "file holds 51 but no file_write tool result in the transcript"
			}
			return true, "file holds exactly 51, via file_write"
		})

	// T6: read the file back.
	runTurn("T6-read-result-back",
		fmt.Sprintf("Use the file_read tool to read the file %s and tell me the number in it. You must call the file_read tool — do not just say what you think is in the file.", resultPath),
		fmt.Sprintf("That did not work — you answered without calling the file_read tool. Use the file_read tool to read the file %s and tell me the number in it.", resultPath),
		func(answer string) (bool, string) {
			toolOK := toolResultContains(t, storeFn(), "51")
			if !strings.Contains(answer, "51") {
				return false, fmt.Sprintf("answer lacks 51; file_read tool result 51 seen=%v", toolOK)
			}
			if !toolOK {
				return false, "answer has 51 but no file_read tool result with 51 in the transcript (hallucinated, not read)"
			}
			return true, "answer 51 backed by a file_read tool result"
		})

	// T7: LATE RECALL PROBE — not fatal. By now the transcript is
	// ~20 messages and the agent only feeds the last 10 to the
	// model, so the T1 messages have slid out of the context
	// window. Record whether the model still recalls 17 and whether
	// T1 is still in the window, so the truncation effect is
	// measured rather than assumed.
	window, _ := store.RecentMessages(10)
	t1InWindow := false
	for _, m := range window {
		if strings.Contains(m.Content, "favourite number is 17") {
			t1InWindow = true
		}
	}
	full, _ := store.RecentMessages(0)
	probe := runTurn("T7-late-recall-probe",
		"What is my favourite number? Reply briefly with just the number.",
		"That answer was wrong. Look back at our earlier conversation and tell me my favourite number. Reply briefly with just the number.",
		func(answer string) (bool, string) {
			return strings.Contains(answer, "17"),
				fmt.Sprintf("answer %q; T1 still in 10-message window=%v (transcript %d messages)", answer, t1InWindow, len(full))
		})
	t.Logf("T7 late-recall probe (NOT fatal): passed=%v, T1-in-window=%v, transcript=%d messages", probe.passed, t1InWindow, len(full))

	if err := agent.Shutdown(); err != nil {
		t.Errorf("final Shutdown: %v", err)
	}
	t.Logf("restarted agent log:\n%s", agentLog.String())

	// Final report: per-turn table, then fail on T1-T6 failures.
	t.Logf("=== multi-turn live E2E report (model %s) ===", modelName)
	for _, o := range outcomes {
		t.Logf("  %s: passed=%v attempts=%d total=%v — %s", o.name, o.passed, o.attempts, o.elapsed, o.detail)
	}
	var failed []string
	for _, o := range outcomes {
		if o.name == "T7-late-recall-probe" {
			continue
		}
		if !o.passed {
			failed = append(failed, o.name)
		}
	}
	if len(failed) > 0 {
		t.Errorf("multi-turn live E2E: failed turns: %v (see per-turn log above; T7 probe excluded)", failed)
	}
}
