// Byte-stable system message tests (layer-0 cost reduction, P1):
// messages[0] is the framework system prompt (identity + rules +
// personality) and must be byte-identical across turns, whatever
// the recall input and the skill registry do. The volatile
// sections — recall and the skill catalogue — ride as their own
// system messages before the user turn, after the stable prefix,
// so the provider's prefix cache can match the system prompt +
// history across turns. On the base commit the skill catalogue
// was appended to messages[0], so any registry change rewrote
// the first message and the cache never matched after it.
package core

import (
	"context"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/skills"
	"github.com/irfan/orion/internal/tools"
)

// registryWithSkill builds a skill registry holding one skill.
func registryWithSkill(t *testing.T, name, desc string) *skills.Registry {
	t.Helper()
	reg := skills.NewRegistry()
	if err := reg.Register(skills.Skill{
		Name:        name,
		Description: desc,
		Content:     "# " + name + "\nBODY-MARKER-" + name + "\nDo the thing.",
	}); err != nil {
		t.Fatalf("Register %s: %v", name, err)
	}
	return reg
}

// systemMessagesBeforeUser returns every system message before the
// last user message in req, in order.
func systemMessagesBeforeUser(t *testing.T, req model.ChatRequest) []model.Message {
	t.Helper()
	msgs := req.Messages
	if len(msgs) < 2 || msgs[len(msgs)-1].Role != "user" {
		t.Fatalf("request does not end with a user message: %+v", msgs)
	}
	var out []model.Message
	for _, m := range msgs[:len(msgs)-1] {
		if m.Role == "system" {
			out = append(out, m)
		}
	}
	return out
}

// TestSystemMessageByteIdenticalAcrossTurns: two turns with
// different recall (turn 1 recalls a stored fact, turn 2 recalls
// nothing) and different skill registries (alpha, then beta)
// must send a byte-identical messages[0] — the bare system
// prompt, with no skill or recall text in it — while each turn's
// own skill catalogue rides in its own message before the user
// turn. On the base commit the catalogue was appended to
// messages[0], so the two first messages differ.
func TestSystemMessageByteIdenticalAcrossTurns(t *testing.T) {
	prov := &fakeProvider{}
	mem := newFakeStore()
	if err := mem.AddFact("favourite number", "17"); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	a := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), mem)
	var reqs [2]model.ChatRequest
	prov.chatFn = func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		if call >= 1 && call <= 2 {
			reqs[call-1] = req
		}
		return textResponse("ok"), nil
	}

	a.SetSkills(registryWithSkill(t, "alpha", "Does alpha things."))
	if _, err := a.Run(context.Background(), "What is my favourite number?"); err != nil {
		t.Fatalf("turn 1 Run: %v", err)
	}
	a.SetSkills(registryWithSkill(t, "beta", "Does beta things, nothing like alpha."))
	if _, err := a.Run(context.Background(), "Tell me about the weather"); err != nil {
		t.Fatalf("turn 2 Run: %v", err)
	}

	sys1, sys2 := reqs[0].Messages[0].Content, reqs[1].Messages[0].Content
	if sys1 != sys2 {
		t.Fatalf("messages[0] differs across turns:\nturn 1: %q\nturn 2: %q", sys1, sys2)
	}
	if sys1 != a.SystemPrompt() {
		t.Fatalf("messages[0] = %q, want the bare SystemPrompt() %q", sys1, a.SystemPrompt())
	}
	for _, needle := range []string{"alpha", "beta", "Skills", "favourite number"} {
		if strings.Contains(sys1, needle) {
			t.Errorf("messages[0] contains %q (volatile content must ride separately): %q", needle, sys1)
		}
	}

	// Turn 1: recall fired (the fact) and the alpha catalogue rode
	// before the user turn; turn 2: no recall, beta catalogue.
	sysBefore1 := systemMessagesBeforeUser(t, reqs[0])
	foundRecall, foundAlpha := false, false
	for _, m := range sysBefore1 {
		if strings.Contains(m.Content, "favourite number: 17") {
			foundRecall = true
		}
		if strings.Contains(m.Content, "alpha") {
			foundAlpha = true
		}
	}
	if !foundRecall {
		t.Errorf("turn 1 has no recall message with the fact: %+v", reqs[0].Messages)
	}
	if !foundAlpha {
		t.Errorf("turn 1 has no skill message with alpha: %+v", reqs[0].Messages)
	}
	sysBefore2 := systemMessagesBeforeUser(t, reqs[1])
	foundBeta := false
	for _, m := range sysBefore2 {
		if strings.Contains(m.Content, "beta") {
			foundBeta = true
		}
		if strings.Contains(m.Content, "favourite number: 17") {
			t.Errorf("turn 2 carries turn 1's recall (recall must vary with the input): %+v", reqs[1].Messages)
		}
	}
	if !foundBeta {
		t.Errorf("turn 2 has no skill message with beta: %+v", reqs[1].Messages)
	}

	// The skill messages are in-flight only: neither catalogue
	// was recorded into memory as history.
	stored, err := mem.RecentMessages(0)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	for _, m := range stored {
		for _, needle := range []string{"alpha", "beta", "Skills —"} {
			if strings.Contains(m.Content, needle) {
				t.Errorf("stored history contains %q (skill catalogue must never persist): %q", needle, m.Content)
			}
		}
	}
}

// TestSystemMessageStableWithNoSkills: with no registry installed
// the request is exactly [system prompt, user turn] — no phantom
// skill message — and messages[0] is the bare system prompt.
func TestSystemMessageStableWithNoSkills(t *testing.T) {
	prov := &fakeProvider{}
	a := newTestAgent(t, testConfig(5), prov, tools.NewRegistry(), newFakeStore())
	prov.chatFn = func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		return textResponse("ok"), nil
	}
	if _, err := a.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	msgs := prov.lastReq.Messages
	if len(msgs) != 2 {
		t.Fatalf("request has %d messages, want 2 (system prompt + user turn): %+v", len(msgs), msgs)
	}
	if msgs[0].Content != a.SystemPrompt() {
		t.Fatalf("messages[0] = %q, want the bare SystemPrompt() %q", msgs[0].Content, a.SystemPrompt())
	}
	if msgs[1].Role != "user" || msgs[1].Content != "hello" {
		t.Fatalf("messages[1] = %+v, want the user turn", msgs[1])
	}
}

// TestSkillMessageSurvivesTightBudget: the skill message is
// system-role content like recall, so trimHistoryToBudget never
// drops or truncates it — under a tight explicit budget the
// oldest history turns pay for it, not the skill message. (The
// ceiling bounds derived budgets at 32K; an explicit small
// budget is the tight case.)
func TestSkillMessageSurvivesTightBudget(t *testing.T) {
	prov := &fakeProvider{}
	mem := newFakeStore()
	cfg := testConfig(5)
	cfg.MaxHistoryTokens = 2048
	a := newTestAgent(t, cfg, prov, tools.NewRegistry(), mem)
	a.SetSkills(registryWithSkill(t, "alpha", "Does alpha things."))
	runFillerTurns(t, a, 6)
	prov.chatFn = func(call int, req model.ChatRequest) (*model.ChatResponse, error) {
		return textResponse("ok"), nil
	}
	if _, err := a.Run(context.Background(), "hello again"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := false
	for _, m := range prov.lastReq.Messages {
		if m.Role == "system" && strings.Contains(m.Content, "alpha") {
			found = true
		}
	}
	if !found {
		t.Errorf("skill message was trimmed away under a tight budget (system content must survive): %+v", prov.lastReq.Messages)
	}
	if got := prov.lastReq.Messages[0].Content; got != a.SystemPrompt() {
		t.Errorf("messages[0] under a tight budget = %q, want the bare SystemPrompt()", got)
	}
}
