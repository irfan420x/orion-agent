package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/permissions"
)

// --- Schedule-time allow-rule warning (F-C3) ----------------------------
//
// cron_schedule must warn in its confirmation when the requesting
// chat holds persisted "always allow" rules for gated tools —
// rules a scheduled turn now ignores — so the user learns the job
// will be denied instead of auto-running. It warns only: the job
// is still created, and the rules themselves are untouched (they
// still apply to interactive turns). Every test here constructs
// a scheduler over its store, so the F-C11 dormant-job warning
// never appears in the note — except the two shadowing tests,
// which deliberately do NOT construct one (the dormant warning
// does appear in their notes, so their assertions pin the
// allow-rule warning with its narrowed text instead of plain
// "Warning"). These tests pin the allow-rule warning only.

// cronScheduleNote schedules a job with tool and returns the
// confirmation's note field (the human-readable behaviour +
// warning text).
func cronScheduleNote(t *testing.T, tool *cronScheduleTool, ctx context.Context, expr, prompt string) string {
	t.Helper()
	out, err := tool.Execute(ctx, json.RawMessage(fmt.Sprintf(`{"expression":%q,"prompt":%q}`, expr, prompt)))
	if err != nil {
		t.Fatalf("cron_schedule(%q): %v", expr, err)
	}
	var parsed struct {
		Job struct {
			ID string `json:"id"`
		} `json:"job"`
		Note string `json:"note"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("cron_schedule result is not JSON (%q): %v", out, err)
	}
	if parsed.Job.ID == "" {
		t.Fatalf("cron_schedule created no job: %s", out)
	}
	return parsed.Note
}

func TestCronScheduleWarnsAboutAllowRules(t *testing.T) {
	store := NewPlanningStore("")
	_ = NewCronScheduler(store) // a scheduler runs: no F-C11 dormant warning in the note
	rules := permissions.NewRuleStore("")
	if err := rules.SetForChat(111, "shell_exec", permissions.ActionAllow); err != nil {
		t.Fatal(err)
	}
	if err := rules.SetForChat(111, "web_fetch", permissions.ActionAllow); err != nil {
		t.Fatal(err)
	}
	// A deny rule must NOT warn: it already denies, so there is
	// no behaviour change to warn about.
	if err := rules.SetForChat(111, "file_write", permissions.ActionDeny); err != nil {
		t.Fatal(err)
	}
	tool := &cronScheduleTool{store: store, rules: rules}
	note := cronScheduleNote(t, tool, cronChatCtx(111, 111), "0 9 * * 1-5", "morning brief")
	if !strings.Contains(note, "Warning") {
		t.Fatalf("schedule note = %q, want a Warning about the allow rules", note)
	}
	for _, want := range []string{"shell_exec", "web_fetch"} {
		if !strings.Contains(note, want) {
			t.Errorf("schedule note = %q, want it to name the allow-ruled tool %q", note, want)
		}
	}
	if strings.Contains(note, "file_write") {
		t.Errorf("schedule note = %q, want no mention of file_write (its rule is a deny, not an allow)", note)
	}
	if !strings.Contains(note, "ignored in scheduled turns") {
		t.Errorf("schedule note = %q, want it to say the rules are ignored in scheduled turns", note)
	}
	// Warn only: the job was still created.
	if got := mustCronList(t, &cronListTool{store: store}, cronChatCtx(111, 111)); got.Count != 1 {
		t.Fatalf("list after warned schedule = %d jobs, want 1 (warn, do not refuse)", got.Count)
	}
	// The rules themselves are untouched: they still apply to
	// interactive turns.
	if action, ok := rules.LookupForChat(111, "shell_exec"); !ok || action != permissions.ActionAllow {
		t.Errorf("LookupForChat(shell_exec) = %q/%v, want allow/true (warning must not revoke the rule)", action, ok)
	}
}

func TestCronScheduleNoWarningWithoutAllowRules(t *testing.T) {
	store := NewPlanningStore("")
	_ = NewCronScheduler(store) // a scheduler runs: no F-C11 dormant warning in the note
	rules := permissions.NewRuleStore("")
	// A deny rule alone changes nothing about scheduled turns.
	if err := rules.SetForChat(111, "shell_exec", permissions.ActionDeny); err != nil {
		t.Fatal(err)
	}
	tool := &cronScheduleTool{store: store, rules: rules}
	note := cronScheduleNote(t, tool, cronChatCtx(111, 111), "0 9 * * 1-5", "morning brief")
	if strings.Contains(note, "Warning") {
		t.Errorf("schedule note = %q, want no Warning when no allow rule exists", note)
	}
	if !strings.Contains(note, "unattended") {
		t.Errorf("schedule note = %q, want it to describe the unattended behaviour", note)
	}
}

func TestCronScheduleNoWarningWithoutRuleStore(t *testing.T) {
	// No rule store wired (a hand-built registry without a gate):
	// the job is still created and the note carries no warning.
	store := NewPlanningStore("")
	_ = NewCronScheduler(store) // a scheduler runs: no F-C11 dormant warning in the note
	tool := &cronScheduleTool{store: store}
	note := cronScheduleNote(t, tool, cronChatCtx(111, 111), "0 9 * * 1-5", "morning brief")
	if strings.Contains(note, "Warning") {
		t.Errorf("schedule note = %q, want no Warning with no rule store", note)
	}
}

func TestCronScheduleWarnsOnlyForRequestingChat(t *testing.T) {
	store := NewPlanningStore("")
	_ = NewCronScheduler(store) // a scheduler runs: no F-C11 dormant warning in the note
	rules := permissions.NewRuleStore("")
	// The allow rule lives in chat 222, not in the requesting
	// chat 111: it must not warn here (per-chat isolation).
	if err := rules.SetForChat(222, "shell_exec", permissions.ActionAllow); err != nil {
		t.Fatal(err)
	}
	tool := &cronScheduleTool{store: store, rules: rules}
	note := cronScheduleNote(t, tool, cronChatCtx(111, 111), "0 9 * * 1-5", "morning brief")
	if strings.Contains(note, "Warning") {
		t.Errorf("schedule note = %q, want no Warning for another chat's allow rule", note)
	}
}

func TestCronScheduleNoWarningWhenPerChatDenyShadowsGlobalAllow(t *testing.T) {
	// The re-verifier's warning-only flaw: a per-chat DENY for a
	// tool shadows a legacy global (chat 0) ALLOW for the same
	// tool — LookupForChat returns the deny, so the job is denied
	// via the deny rule, not via the ignored allow. The warning
	// must therefore NOT name that tool: its mechanism claim
	// ("those [allow] rules are ignored") would be wrong for it.
	// A global allow with NO per-chat deny must still warn.
	store := NewPlanningStore("")
	rules := permissions.NewRuleStore("")
	if err := rules.SetForChat(0, "shell_exec", permissions.ActionAllow); err != nil {
		t.Fatal(err)
	}
	if err := rules.SetForChat(111, "shell_exec", permissions.ActionDeny); err != nil {
		t.Fatal(err)
	}
	// Control: a global allow no per-chat rule shadows.
	if err := rules.SetForChat(0, "web_fetch", permissions.ActionAllow); err != nil {
		t.Fatal(err)
	}
	tool := &cronScheduleTool{store: store, rules: rules}
	note := cronScheduleNote(t, tool, cronChatCtx(111, 111), "0 9 * * 1-5", "morning brief")
	if !strings.Contains(note, "Warning: this chat has persisted") || !strings.Contains(note, "web_fetch") {
		t.Errorf("schedule note = %q, want an always-allow Warning naming web_fetch (unshadowed global allow)", note)
	}
	if strings.Contains(note, "shell_exec") {
		t.Errorf("schedule note = %q, want no mention of shell_exec (per-chat deny shadows the global allow)", note)
	}
	// Warn only: the job was still created, the rules untouched.
	if got := mustCronList(t, &cronListTool{store: store}, cronChatCtx(111, 111)); got.Count != 1 {
		t.Fatalf("list after warned schedule = %d jobs, want 1 (warn, do not refuse)", got.Count)
	}
	if action, ok := rules.LookupForChat(111, "shell_exec"); !ok || action != permissions.ActionDeny {
		t.Errorf("LookupForChat(shell_exec) = %q/%v, want deny/true (warning must not change the effective rule)", action, ok)
	}
	if action, ok := rules.LookupForChat(111, "web_fetch"); !ok || action != permissions.ActionAllow {
		t.Errorf("LookupForChat(web_fetch) = %q/%v, want allow/true (warning must not revoke the rule)", action, ok)
	}

	// Pure case: with ONLY the shadowed pair standing, there is
	// no always-allow warning — no effective allow rule exists.
	// (The dormant-scheduler warning still appears: no scheduler
	// runs in this test process, and it is unrelated to the
	// allow-rule warning this test is about.)
	shadowOnly := NewPlanningStore("")
	shadowRules := permissions.NewRuleStore("")
	if err := shadowRules.SetForChat(0, "shell_exec", permissions.ActionAllow); err != nil {
		t.Fatal(err)
	}
	if err := shadowRules.SetForChat(111, "shell_exec", permissions.ActionDeny); err != nil {
		t.Fatal(err)
	}
	shadowTool := &cronScheduleTool{store: shadowOnly, rules: shadowRules}
	if note := cronScheduleNote(t, shadowTool, cronChatCtx(111, 111), "0 9 * * 1-5", "morning brief"); strings.Contains(note, "Warning: this chat has persisted") {
		t.Errorf("schedule note = %q, want no always-allow Warning when the only allow is shadowed by a per-chat deny", note)
	}
}

func TestCronScheduleWarnsWhenPerChatAllowShadowsGlobalDeny(t *testing.T) {
	// The reverse precedence: a per-chat ALLOW shadowing a
	// global DENY is the effective rule, so the scheduled turn
	// ignores it and the tool must warn — the deny underneath
	// must not suppress the warning.
	store := NewPlanningStore("")
	rules := permissions.NewRuleStore("")
	if err := rules.SetForChat(0, "shell_exec", permissions.ActionDeny); err != nil {
		t.Fatal(err)
	}
	if err := rules.SetForChat(111, "shell_exec", permissions.ActionAllow); err != nil {
		t.Fatal(err)
	}
	tool := &cronScheduleTool{store: store, rules: rules}
	note := cronScheduleNote(t, tool, cronChatCtx(111, 111), "0 9 * * 1-5", "morning brief")
	if !strings.Contains(note, "Warning: this chat has persisted") || !strings.Contains(note, "shell_exec") {
		t.Errorf("schedule note = %q, want a Warning naming shell_exec (per-chat allow is effective over the global deny)", note)
	}
	if got := mustCronList(t, &cronListTool{store: store}, cronChatCtx(111, 111)); got.Count != 1 {
		t.Fatalf("list after warned schedule = %d jobs, want 1 (warn, do not refuse)", got.Count)
	}
	if action, ok := rules.LookupForChat(111, "shell_exec"); !ok || action != permissions.ActionAllow {
		t.Errorf("LookupForChat(shell_exec) = %q/%v, want allow/true (warning must not change the effective rule)", action, ok)
	}
}

func TestCronScheduleWarnsAboutMcpAndUnclassifiedAllowRules(t *testing.T) {
	// The warning must cover allow rules for ALL gated tools held
	// for the chat, not only the built-ins KnownTools() lists:
	// SetForChat accepts any non-empty name, and the gate denies
	// MCP tools (mcp__ prefix, ClassNetwork) and unclassified
	// names (fail-closed ClassDestructive) promptly when
	// unattended — so a chat holding such a rule got no warning
	// yet was denied every run.
	store := NewPlanningStore("")
	_ = NewCronScheduler(store) // a scheduler runs: no F-C11 dormant warning in the note
	rules := permissions.NewRuleStore("")
	if err := rules.SetForChat(111, "mcp__example", permissions.ActionAllow); err != nil {
		t.Fatal(err)
	}
	if err := rules.SetForChat(111, "some_future_tool", permissions.ActionAllow); err != nil {
		t.Fatal(err)
	}
	// Controls: a deny rule for an MCP tool must not warn, and
	// another chat's MCP allow rule must not warn here.
	if err := rules.SetForChat(111, "mcp__other", permissions.ActionDeny); err != nil {
		t.Fatal(err)
	}
	if err := rules.SetForChat(222, "mcp__elsewhere", permissions.ActionAllow); err != nil {
		t.Fatal(err)
	}
	tool := &cronScheduleTool{store: store, rules: rules}
	note := cronScheduleNote(t, tool, cronChatCtx(111, 111), "0 9 * * 1-5", "morning brief")
	if !strings.Contains(note, "Warning") {
		t.Fatalf("schedule note = %q, want a Warning about the allow rules", note)
	}
	for _, want := range []string{"mcp__example", "some_future_tool"} {
		if !strings.Contains(note, want) {
			t.Errorf("schedule note = %q, want it to name the allow-ruled tool %q", note, want)
		}
	}
	for _, unwanted := range []string{"mcp__other", "mcp__elsewhere"} {
		if strings.Contains(note, unwanted) {
			t.Errorf("schedule note = %q, want no mention of %q (deny rule / another chat's rule)", note, unwanted)
		}
	}
	// Warn only: the job was still created, the rules untouched.
	if got := mustCronList(t, &cronListTool{store: store}, cronChatCtx(111, 111)); got.Count != 1 {
		t.Fatalf("list after warned schedule = %d jobs, want 1 (warn, do not refuse)", got.Count)
	}
	if action, ok := rules.LookupForChat(111, "mcp__example"); !ok || action != permissions.ActionAllow {
		t.Errorf("LookupForChat(mcp__example) = %q/%v, want allow/true (warning must not revoke the rule)", action, ok)
	}
}

// TestSetCronScheduleRuleStoreWiring covers the daemon wiring path
// (App.NewPermissionGate calls SetCronScheduleRuleStore on the
// registry): a registry-built cron_schedule tool warns once the
// store is installed, and stays silent before it.
func TestSetCronScheduleRuleStoreWiring(t *testing.T) {
	store := NewPlanningStore("")
	_ = NewCronScheduler(store) // a scheduler runs: no F-C11 dormant warning in the note
	rules := permissions.NewRuleStore("")
	if err := rules.SetForChat(111, "shell_exec", permissions.ActionAllow); err != nil {
		t.Fatal(err)
	}
	reg, err := DefaultRegistryWithDeps(ToolDeps{PlanningStore: store})
	if err != nil {
		t.Fatal(err)
	}
	ctx := cronChatCtx(111, 111)
	scheduleViaRegistry := func() string {
		t.Helper()
		out, err := reg.Execute(ctx, "cron_schedule", json.RawMessage(`{"expression":"0 9 * * *","prompt":"via registry"}`))
		if err != nil {
			t.Fatalf("registry cron_schedule: %v", err)
		}
		var parsed struct {
			Note string `json:"note"`
		}
		if err := json.Unmarshal([]byte(out), &parsed); err != nil {
			t.Fatalf("registry cron_schedule result is not JSON (%q): %v", out, err)
		}
		return parsed.Note
	}
	if note := scheduleViaRegistry(); strings.Contains(note, "Warning") {
		t.Errorf("registry schedule note before wiring = %q, want no Warning", note)
	}
	SetCronScheduleRuleStore(reg, rules)
	if note := scheduleViaRegistry(); !strings.Contains(note, "Warning") || !strings.Contains(note, "shell_exec") {
		t.Errorf("registry schedule note after wiring = %q, want a Warning naming shell_exec", note)
	}
	// Nil-safe: a nil registry or store is a no-op, never a panic.
	SetCronScheduleRuleStore(nil, rules)
	SetCronScheduleRuleStore(reg, nil)
}
