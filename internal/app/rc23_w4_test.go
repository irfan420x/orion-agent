// rc23 W4: App.SetDailyBudget persistence (/budget), App.BudgetInfoForChat,
// App.ReloadSkills (/skills reload), and loadSkills known-tool wiring.
package app

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/budget"
	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/skills"
)

// TestSetDailyBudgetPersistsToConfig proves the /budget set path:
// the new limit is applied to the live Budget AND written to the
// config file, so a daemon restart (which rebuilds the Budget from
// the config) keeps it. An in-memory-only SetLimit would silently
// lose the change on restart — that is the bug this test pins.
func TestSetDailyBudgetPersistsToConfig(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(100000)

	if err := a.SetDailyBudget(5000); err != nil {
		t.Fatalf("SetDailyBudget(5000): %v", err)
	}
	if got := a.Budget.Limit(); got != 5000 {
		t.Errorf("live Budget limit = %d, want 5000", got)
	}
	cfgPath := filepath.Join(a.Config.DataDir, "config.json")
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load(%q): %v", cfgPath, err)
	}
	if reloaded.PerChatDailyTokenBudget != 5000 {
		t.Errorf("config on disk PerChatDailyTokenBudget = %d, want 5000", reloaded.PerChatDailyTokenBudget)
	}

	// Unlimited persists as 0.
	if err := a.SetDailyBudget(0); err != nil {
		t.Fatalf("SetDailyBudget(0): %v", err)
	}
	if got := a.Budget.Limit(); got != 0 {
		t.Errorf("live Budget limit = %d, want 0 (unlimited)", got)
	}
	reloaded, err = config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load after unlimited: %v", err)
	}
	if reloaded.PerChatDailyTokenBudget != 0 {
		t.Errorf("config on disk PerChatDailyTokenBudget = %d, want 0", reloaded.PerChatDailyTokenBudget)
	}

	// Invalid limits are rejected and change nothing, on disk or live.
	for _, bad := range []int{999, 10000001, -5} {
		if err := a.SetDailyBudget(bad); err == nil {
			t.Errorf("SetDailyBudget(%d): expected error, got nil", bad)
		}
	}
	if got := a.Budget.Limit(); got != 0 {
		t.Errorf("live Budget limit after rejected sets = %d, want 0", got)
	}
	reloaded, err = config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load after rejected sets: %v", err)
	}
	if reloaded.PerChatDailyTokenBudget != 0 {
		t.Errorf("config on disk after rejected sets = %d, want 0", reloaded.PerChatDailyTokenBudget)
	}
}

func TestSetDailyBudgetWithoutLiveBudgetStillPersists(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = nil // hand-built App with no Budget
	if err := a.SetDailyBudget(7000); err != nil {
		t.Fatalf("SetDailyBudget with nil Budget: %v", err)
	}
	reloaded, err := config.Load(filepath.Join(a.Config.DataDir, "config.json"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if reloaded.PerChatDailyTokenBudget != 7000 {
		t.Errorf("config on disk = %d, want 7000", reloaded.PerChatDailyTokenBudget)
	}
}

func TestSetDailyBudgetNilApp(t *testing.T) {
	var a *App
	if err := a.SetDailyBudget(5000); err == nil {
		t.Error("nil App SetDailyBudget: expected error, got nil")
	}
}

func TestBudgetInfoForChat(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Budget = budget.New(100000)
	if err := a.Budget.RecordUsage(111, model.Usage{PromptTokens: 3000, CompletionTokens: 337, TotalTokens: 3337}, 0); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	limit, used := a.BudgetInfoForChat(111)
	if limit != 100000 || used != 3337 {
		t.Errorf("BudgetInfoForChat(111) = (%d, %d), want (100000, 3337)", limit, used)
	}
	if limit, used := a.BudgetInfoForChat(222); limit != 100000 || used != 0 {
		t.Errorf("BudgetInfoForChat(222) = (%d, %d), want (100000, 0)", limit, used)
	}
	var nilApp *App
	if limit, used := nilApp.BudgetInfoForChat(111); limit != 0 || used != 0 {
		t.Errorf("nil App BudgetInfoForChat = (%d, %d), want (0, 0)", limit, used)
	}
	a.Budget = nil
	if limit, used := a.BudgetInfoForChat(111); limit != 0 || used != 0 {
		t.Errorf("nil Budget BudgetInfoForChat = (%d, %d), want (0, 0)", limit, used)
	}
}

func TestReloadSkills(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	a.Skills = skills.NewRegistry()
	skillDir := filepath.Join(a.Config.DataDir, "skills")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatalf("mkdir skills: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "alpha.md"), []byte("# Alpha\n\nFirst.\n"), 0o644); err != nil {
		t.Fatalf("write alpha.md: %v", err)
	}
	loaded, err := a.ReloadSkills()
	if err != nil {
		t.Fatalf("ReloadSkills: %v", err)
	}
	if loaded != 1 {
		t.Fatalf("ReloadSkills loaded = %d, want 1", loaded)
	}
	if _, ok := a.Skills.Get("Alpha"); !ok {
		t.Error("Alpha missing after ReloadSkills")
	}
	// A second reload picks up a new file, same registry pointer.
	if err := os.WriteFile(filepath.Join(skillDir, "beta.md"), []byte("# Beta\n\nSecond.\n"), 0o644); err != nil {
		t.Fatalf("write beta.md: %v", err)
	}
	before := a.Skills
	if loaded, err := a.ReloadSkills(); err != nil || loaded != 2 {
		t.Fatalf("second ReloadSkills = (%d, %v), want (2, nil)", loaded, err)
	}
	if a.Skills != before {
		t.Error("ReloadSkills must swap contents in place, not replace the registry pointer")
	}
}

func TestReloadSkillsErrors(t *testing.T) {
	var nilApp *App
	if _, err := nilApp.ReloadSkills(); err == nil {
		t.Error("nil App ReloadSkills: expected error, got nil")
	}
	a := testApp(t)
	defer a.Close()
	if _, err := a.ReloadSkills(); err == nil {
		t.Error("ReloadSkills with nil Skills registry: expected error, got nil")
	}
	a.Skills = skills.NewRegistry()
	// No skills dir at all: the reload fails and nothing loads.
	if loaded, err := a.ReloadSkills(); err == nil || loaded != 0 {
		t.Errorf("ReloadSkills with no skills dir = (%d, %v), want (0, error)", loaded, err)
	}
}

func TestLoadSkillsKnownToolsWiring(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "skills")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatalf("mkdir skills: %v", err)
	}
	content := "# Mcp\n\nUses MCP.\n\n## Tools\n\n- mcp__srv__do_thing\n"
	if err := os.WriteFile(filepath.Join(skillDir, "mcp.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write mcp.md: %v", err)
	}
	// The passed known names suppress the warning; without them
	// the same file warns (warnings go through the standard logger).
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	reg := loadSkills(dir, log.New(&buf, "", 0), []string{"mcp__srv__do_thing"})
	if _, ok := reg.Get("Mcp"); !ok {
		t.Fatal("Mcp skill must load")
	}
	if strings.Contains(buf.String(), "unknown tool") {
		t.Errorf("log must not warn for the wired MCP name: %q", buf.String())
	}
	buf.Reset()
	reg2 := loadSkills(dir, log.New(&buf, "", 0), nil)
	if _, ok := reg2.Get("Mcp"); !ok {
		t.Fatal("Mcp skill must load without known tools too")
	}
	if !strings.Contains(buf.String(), `unknown tool "mcp__srv__do_thing"`) {
		t.Errorf("log = %q, want the unknown-tool warning without the wiring", buf.String())
	}
}
