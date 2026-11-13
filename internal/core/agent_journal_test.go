package core

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/skills"
	"github.com/irfan/orion/internal/tools"
)

// journalStep is one scripted Chat reply of the journal test
// provider: a tool call, a final answer, or a block on ctx
// cancellation (the kill stand-in: the provider never answers
// because the turn is killed first).
type journalStep struct {
	tool   *model.ToolCall
	answer string
	block  bool
}

// journalProvider is a scriptable model.Provider for the journal
// replay tests. Each Chat call runs the next scripted step; a
// step past the end of the script is an error, so a test that
// drives more provider calls than scripted fails loudly instead
// of answering from a stale script.
type journalProvider struct {
	mu    sync.Mutex
	steps []journalStep
	calls int
}

func (p *journalProvider) Name() string { return "journal-test" }

func (p *journalProvider) Chat(ctx context.Context, _ model.ChatRequest) (*model.ChatResponse, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call > len(p.steps) {
		return nil, fmt.Errorf("journal-test provider: no script step for call %d", call)
	}
	step := p.steps[call-1]
	if step.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if step.tool != nil {
		return toolCallResponse(step.tool.ID, step.tool.Name, step.tool.Arguments), nil
	}
	return textResponse(step.answer), nil
}

func (p *journalProvider) Stream(_ context.Context, _ model.ChatRequest, _ func(chunk string) error) (*model.ChatResponse, error) {
	return nil, fmt.Errorf("journal-test provider: Stream unused")
}

func journalTestAgent(t *testing.T, prov model.Provider, reg *tools.Registry, journal *ToolJournal) *Agent {
	t.Helper()
	a := newTestAgent(t, testConfig(5), prov, reg, newFakeStore())
	if journal != nil {
		a.SetToolJournal(journal)
	}
	return a
}

// waitJournalRecord waits until the journal holds n records for
// update id (proving the record is durable), or fails the test.
func waitJournalRecord(t *testing.T, j *ToolJournal, updateID int64, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(j.Records(updateID)) < n {
		if time.Now().After(deadline) {
			t.Fatalf("journal for update %d still holds %d records, want %d", updateID, len(j.Records(updateID)), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestJournalReplaysToolAcrossRestart is the F-A9-1 regression
// test: a turn killed mid-execution (the provider never answers
// its second call because the run context is cancelled — the
// SIGTERM/restart stand-in) is re-delivered after the restart,
// and the tool it already ran executes exactly once across both
// runs. The "without journal" subtest is the negative control:
// without the fix the tool runs twice, proving the test is
// sensitive to the bug.
func TestJournalReplaysToolAcrossRestart(t *testing.T) {
	toolCall := &model.ToolCall{ID: "call-1", Name: "echo", Arguments: `{"text":"hello"}`}

	runKilledTurn := func(t *testing.T, agent *Agent, echo *recordingTool, journal *ToolJournal) {
		t.Helper()
		ctx, cancel := context.WithCancel(WithUpdateID(context.Background(), 100))
		errCh := make(chan error, 1)
		go func() {
			_, err := agent.Run(ctx, "do the thing")
			errCh <- err
		}()
		// Wait for the record to be durable, then kill the
		// turn mid-execution (after the tool ran, before the
		// provider answered the next step).
		if journal != nil {
			waitJournalRecord(t, journal, 100, 1)
		} else {
			deadline := time.Now().Add(5 * time.Second)
			for echo.callCount() == 0 {
				if time.Now().After(deadline) {
					t.Fatal("run 1: tool never executed")
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		cancel()
		select {
		case err := <-errCh:
			if err == nil {
				t.Error("run 1: expected a cancellation error, got nil")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("run 1 did not return after cancel")
		}
	}

	t.Run("with journal", func(t *testing.T) {
		dir := t.TempDir()
		j1, err := NewToolJournal(dir)
		if err != nil {
			t.Fatalf("NewToolJournal: %v", err)
		}
		echo := &recordingTool{name: "echo"}
		reg := tools.NewRegistry()
		if err := reg.Register(echo); err != nil {
			t.Fatalf("Register echo: %v", err)
		}
		prov1 := &journalProvider{steps: []journalStep{
			{tool: toolCall},
			{block: true},
		}}
		agent1 := journalTestAgent(t, prov1, reg, j1)
		runKilledTurn(t, agent1, echo, j1)
		if got := echo.callCount(); got != 1 {
			t.Fatalf("run 1: echo ran %d times, want 1", got)
		}
		// Restart: a fresh agent and provider, the journal
		// reloaded from the same directory, the same update id.
		j2, err := NewToolJournal(dir)
		if err != nil {
			t.Fatalf("NewToolJournal (restart): %v", err)
		}
		prov2 := &journalProvider{steps: []journalStep{
			{tool: toolCall},
			{answer: "final answer"},
		}}
		agent2 := journalTestAgent(t, prov2, reg, j2)
		reply, err := agent2.Run(WithUpdateID(context.Background(), 100), "do the thing")
		if err != nil {
			t.Fatalf("run 2: %v", err)
		}
		if reply != "final answer" {
			t.Errorf("run 2 reply = %q, want \"final answer\"", reply)
		}
		if got := echo.callCount(); got != 1 {
			t.Errorf("tool ran %d times across the restart, want exactly 1", got)
		}
		if recs := j2.Records(100); len(recs) != 1 {
			t.Errorf("journal holds %d records for update 100, want 1 (the replay must not record again)", len(recs))
		}
	})

	t.Run("without journal", func(t *testing.T) {
		echo := &recordingTool{name: "echo"}
		reg := tools.NewRegistry()
		if err := reg.Register(echo); err != nil {
			t.Fatalf("Register echo: %v", err)
		}
		prov1 := &journalProvider{steps: []journalStep{
			{tool: toolCall},
			{block: true},
		}}
		agent1 := journalTestAgent(t, prov1, reg, nil)
		runKilledTurn(t, agent1, echo, nil)
		prov2 := &journalProvider{steps: []journalStep{
			{tool: toolCall},
			{answer: "final answer"},
		}}
		agent2 := journalTestAgent(t, prov2, reg, nil)
		if _, err := agent2.Run(WithUpdateID(context.Background(), 100), "do the thing"); err != nil {
			t.Fatalf("run 2: %v", err)
		}
		if got := echo.callCount(); got != 2 {
			t.Errorf("without a journal the tool ran %d times, want 2 (the negative control must show the double execution)", got)
		}
	})
}

// TestJournalSkillRestrictionSurvivesRedelivery is the F-A10-1
// regression test: a re-delivered turn must keep the skill
// restriction. Run 1 invokes the adder skill (Tools: echo), then
// calls the disallowed tool "other" (refused), then the allowed
// "echo", and is killed before the provider answers. Run 2
// re-delivers the same update with the same script: the skill
// invoke must re-execute and re-activate (it is excluded from
// the journal — a replayed invoke would never fire
// setActiveSkill), so "other" is refused again, and "echo"
// replays instead of re-executing.
//
// The Audit 10 probe reproduced the bug on rc14: run 2 executed
// "other" once (the restriction had dropped with the replayed
// invoke) and journalled it, so a third delivery would replay
// the bypassed call too. On the fixed code "other" runs zero
// times across both runs and the journal holds exactly one
// record (echo): the skill invoke is never recorded.
func TestJournalSkillRestrictionSurvivesRedelivery(t *testing.T) {
	dir := t.TempDir()
	j1, err := NewToolJournal(dir)
	if err != nil {
		t.Fatalf("NewToolJournal: %v", err)
	}
	skillReg := skills.NewRegistry()
	if err := skillReg.Register(skills.Skill{
		Name:        "adder",
		Description: "Adds numbers with the echo tool.",
		Content:     "# Adder\nADD-BODY-MARKER-9z\nUse the echo tool to say the answer.",
		Tools:       []string{"echo"},
	}); err != nil {
		t.Fatalf("Register adder: %v", err)
	}
	skillTool, err := tools.NewSkillTool(skillReg)
	if err != nil {
		t.Fatalf("NewSkillTool: %v", err)
	}
	echo := &recordingTool{name: "echo"}
	other := &recordingTool{name: "other"}
	reg := tools.NewRegistry()
	for _, tool := range []tools.Tool{skillTool, echo, other} {
		if err := reg.Register(tool); err != nil {
			t.Fatalf("Register %s: %v", tool.Name(), err)
		}
	}
	script := []journalStep{
		{tool: &model.ToolCall{ID: "c1", Name: "skill", Arguments: `{"name":"adder"}`}},
		{tool: &model.ToolCall{ID: "c2", Name: "other", Arguments: `{"text":"x"}`}},
		{tool: &model.ToolCall{ID: "c3", Name: "echo", Arguments: `{"text":"hi"}`}},
		{block: true},
	}
	agent1 := journalTestAgent(t, &journalProvider{steps: script}, reg, j1)
	agent1.SetSkills(skillReg)
	ctx, cancel := context.WithCancel(WithUpdateID(context.Background(), 100))
	errCh := make(chan error, 1)
	go func() {
		_, err := agent1.Run(ctx, "do the thing")
		errCh <- err
	}()
	// The skill invoke is excluded from the journal, so the only
	// record run 1 leaves is the allowed echo call; wait for it
	// to be durable, then kill the turn mid-execution.
	waitJournalRecord(t, j1, 100, 1)
	cancel()
	select {
	case err := <-errCh:
		if err == nil {
			t.Error("run 1: expected a cancellation error, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run 1 did not return after cancel")
	}
	if got := other.callCount(); got != 0 {
		t.Fatalf("run 1: disallowed tool \"other\" ran %d times, want 0 (the skill restriction must refuse it)", got)
	}
	if got := echo.callCount(); got != 1 {
		t.Fatalf("run 1: echo ran %d times, want 1", got)
	}
	// Restart: a fresh agent and provider, the journal reloaded
	// from the same directory, the same update id and script,
	// ending in a final answer instead of the kill.
	j2, err := NewToolJournal(dir)
	if err != nil {
		t.Fatalf("NewToolJournal (restart): %v", err)
	}
	script2 := append(append([]journalStep{}, script[:3]...), journalStep{answer: "final answer"})
	agent2 := journalTestAgent(t, &journalProvider{steps: script2}, reg, j2)
	agent2.SetSkills(skillReg)
	reply, err := agent2.Run(WithUpdateID(context.Background(), 100), "do the thing")
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if reply != "final answer" {
		t.Errorf("run 2 reply = %q, want \"final answer\"", reply)
	}
	if got := other.callCount(); got != 0 {
		t.Errorf("disallowed tool \"other\" ran %d times across the re-delivery, want 0 (the skill restriction must hold on run 2 as well)", got)
	}
	if got := echo.callCount(); got != 1 {
		t.Errorf("echo ran %d times across the re-delivery, want exactly 1 (run 2 must replay it)", got)
	}
	if recs := j2.Records(100); len(recs) != 1 {
		t.Errorf("journal holds %d records for update 100, want 1 (echo only: the skill invoke is excluded, the refused call was never recorded)", len(recs))
	}
}

// TestJournalRepeatsIdenticalCallInOneTurn proves the occurrence
// index does not over-replay: a turn that legitimately calls the
// same tool with the same arguments twice executes it twice, and
// a re-delivered turn replays both occurrences without executing
// either again.
func TestJournalRepeatsIdenticalCallInOneTurn(t *testing.T) {
	dir := t.TempDir()
	j1, err := NewToolJournal(dir)
	if err != nil {
		t.Fatalf("NewToolJournal: %v", err)
	}
	echo := &recordingTool{name: "echo"}
	reg := tools.NewRegistry()
	if err := reg.Register(echo); err != nil {
		t.Fatalf("Register echo: %v", err)
	}
	call := &model.ToolCall{ID: "call-1", Name: "echo", Arguments: `{"text":"a"}`}
	script := []journalStep{
		{tool: call},
		{tool: call},
		{answer: "done"},
	}
	agent1 := journalTestAgent(t, &journalProvider{steps: script}, reg, j1)
	if reply, err := agent1.Run(WithUpdateID(context.Background(), 100), "twice"); err != nil || reply != "done" {
		t.Fatalf("run 1: reply %q, err %v", reply, err)
	}
	if got := echo.callCount(); got != 2 {
		t.Fatalf("run 1: echo ran %d times, want 2", got)
	}
	if recs := j1.Records(100); len(recs) != 2 {
		t.Fatalf("journal holds %d records, want 2 (occurrences 0 and 1)", len(recs))
	}
	// Re-delivery: the same script, a fresh agent, the journal
	// reloaded. Both calls replay; neither executes.
	j2, err := NewToolJournal(dir)
	if err != nil {
		t.Fatalf("NewToolJournal (restart): %v", err)
	}
	agent2 := journalTestAgent(t, &journalProvider{steps: script}, reg, j2)
	if reply, err := agent2.Run(WithUpdateID(context.Background(), 100), "twice"); err != nil || reply != "done" {
		t.Fatalf("run 2: reply %q, err %v", reply, err)
	}
	if got := echo.callCount(); got != 2 {
		t.Errorf("tool ran %d times across the re-delivery, want exactly 2", got)
	}
}

// TestJournalCanonicalisesArguments proves a re-issued call with
// the same arguments in a different key order still replays:
// the journal keys on canonical arguments, not raw text.
func TestJournalCanonicalisesArguments(t *testing.T) {
	dir := t.TempDir()
	j1, err := NewToolJournal(dir)
	if err != nil {
		t.Fatalf("NewToolJournal: %v", err)
	}
	echo := &recordingTool{name: "echo"}
	reg := tools.NewRegistry()
	if err := reg.Register(echo); err != nil {
		t.Fatalf("Register echo: %v", err)
	}
	agent1 := journalTestAgent(t, &journalProvider{steps: []journalStep{
		{tool: &model.ToolCall{ID: "call-1", Name: "echo", Arguments: `{"b":1,"a":2}`}},
		{answer: "done"},
	}}, reg, j1)
	if _, err := agent1.Run(WithUpdateID(context.Background(), 100), "order"); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	j2, err := NewToolJournal(dir)
	if err != nil {
		t.Fatalf("NewToolJournal (restart): %v", err)
	}
	agent2 := journalTestAgent(t, &journalProvider{steps: []journalStep{
		{tool: &model.ToolCall{ID: "call-1", Name: "echo", Arguments: `{"a":2,"b":1}`}},
		{answer: "done"},
	}}, reg, j2)
	if _, err := agent2.Run(WithUpdateID(context.Background(), 100), "order"); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if got := echo.callCount(); got != 1 {
		t.Errorf("tool ran %d times, want 1 (key order must not defeat the replay)", got)
	}
}

// TestJournalExcludesSubagentTools proves the subagent lifecycle
// tools are never journalled: their results reference in-process
// Manager state that does not survive a restart, so a
// re-delivered turn re-executes them against the live Manager
// instead of replaying a dead task id.
func TestJournalExcludesSubagentTools(t *testing.T) {
	dir := t.TempDir()
	j1, err := NewToolJournal(dir)
	if err != nil {
		t.Fatalf("NewToolJournal: %v", err)
	}
	spawn := &recordingTool{name: "subagent_spawn"}
	reg := tools.NewRegistry()
	if err := reg.Register(spawn); err != nil {
		t.Fatalf("Register subagent_spawn: %v", err)
	}
	script := []journalStep{
		{tool: &model.ToolCall{ID: "call-1", Name: "subagent_spawn", Arguments: `{"text":"work"}`}},
		{answer: "done"},
	}
	agent1 := journalTestAgent(t, &journalProvider{steps: script}, reg, j1)
	if _, err := agent1.Run(WithUpdateID(context.Background(), 100), "spawn"); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if got := spawn.callCount(); got != 1 {
		t.Fatalf("run 1: subagent_spawn ran %d times, want 1", got)
	}
	if recs := j1.Records(100); len(recs) != 0 {
		t.Fatalf("journal holds %d records for an excluded tool, want 0", len(recs))
	}
	j2, err := NewToolJournal(dir)
	if err != nil {
		t.Fatalf("NewToolJournal (restart): %v", err)
	}
	agent2 := journalTestAgent(t, &journalProvider{steps: script}, reg, j2)
	if _, err := agent2.Run(WithUpdateID(context.Background(), 100), "spawn"); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if got := spawn.callCount(); got != 2 {
		t.Errorf("subagent_spawn ran %d times across the re-delivery, want 2 (excluded tools always re-execute)", got)
	}
}
