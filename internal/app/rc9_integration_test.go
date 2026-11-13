// rc9 integration regression tests: coordinator-owned fixes that span
// workers' areas (SwitchProvider skill/hook loss, planning-store
// persistence wiring).
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/irfan/orion/internal/config"
	"github.com/irfan/orion/internal/hooks"
	"github.com/irfan/orion/internal/permissions"
)

// TestRC9SwitchProviderKeepsSkillsAndHooks: a /model switch must not
// silently drop skill discovery or hook enforcement on the
// replacement agent (same class as Audit 4 B P1-1 for Memory v2).
// A fake OpenAI-compatible server stands in for the "openai"
// provider so the post-switch turn really runs: the first request
// must carry the skill section ("Adder"), and the calculator call
// must be blocked by the PreToolUse hook ("blocked by hook"), never
// executed ("396" must not appear).
func TestRC9SwitchProviderKeepsSkillsAndHooks(t *testing.T) {
	a := testApp(t)
	defer a.Close()
	dir := a.Config.DataDir

	// A skill the replacement agent must still advertise.
	skillsDir := filepath.Join(dir, "skills")
	if err := os.MkdirAll(skillsDir, 0o700); err != nil {
		t.Fatalf("mkdir skills: %v", err)
	}
	const adder = "# Adder\nAdds numbers with the calculator.\n\n## Tools\n- calculator\n"
	if err := os.WriteFile(filepath.Join(skillsDir, "adder.md"), []byte(adder), 0o600); err != nil {
		t.Fatalf("write skill: %v", err)
	}
	skillReg := loadSkills(dir, log.New(io.Discard, "", 0))
	if _, ok := skillReg.Get("Adder"); !ok {
		t.Fatalf("skill Adder did not load (have %v)", skillReg.List())
	}
	a.Skills = skillReg
	a.Agent.SetSkills(skillReg)

	// A PreToolUse hook that blocks the calculator.
	hookDir := filepath.Join(dir, "hooks", string(hooks.EventPreToolUse))
	if err := os.MkdirAll(hookDir, 0o700); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	const blockHook = "#!/bin/sh\necho '{\"decision\":\"block\",\"reason\":\"no calc for you\"}'; exit 2\n"
	if err := os.WriteFile(filepath.Join(hookDir, "block.sh"), []byte(blockHook), 0o700); err != nil {
		t.Fatalf("write hook: %v", err)
	}
	bus := hooks.NewBus(dir)
	a.Hooks = bus
	a.Agent.SetHooks(bus)

	// Fake OpenAI-compatible server as the "openai" provider.
	var mu sync.Mutex
	var bodies []string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		calls++
		n := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"calculator","arguments":"{\"expression\":\"17*23+5\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":50,"completion_tokens":10,"total_tokens":60}}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":60,"completion_tokens":5,"total_tokens":65}}`)
	}))
	defer srv.Close()
	p := a.Config.Providers["openai"]
	p.BaseURL = srv.URL
	p.APIKey = "test-fake-key"
	a.Config.Providers["openai"] = p

	if err := a.SwitchProvider("openai"); err != nil {
		t.Fatalf("SwitchProvider(openai): %v", err)
	}
	out, _, err := a.HandleStream(context.Background(), "compute 17*23+5", nil)
	if err != nil {
		t.Fatalf("HandleStream after switch: %v", err)
	}
	if out != "done" {
		t.Fatalf("HandleStream = %q, want %q", out, "done")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) < 2 {
		t.Fatalf("provider got %d request(s), want >= 2 (tool call + follow-up)", len(bodies))
	}
	if !strings.Contains(bodies[0], "Adder") {
		t.Errorf("first provider request does not mention skill %q: skill section was lost by SwitchProvider", "Adder")
	}
	if !strings.Contains(bodies[1], "blocked by hook") {
		t.Errorf("second provider request does not mention the hook block: hooks were lost by SwitchProvider (or the hook did not run)")
	}
	if strings.Contains(bodies[1], "396") {
		t.Errorf("second provider request contains calculator output 396: the hook did not block the call")
	}
}

// TestRC9PlanningStorePersistsAcrossBuild: Build must point the
// planning store at the data dir (rc9 B2 gap), so a task created by
// one App instance is still there after a rebuild on the same dir.
func TestRC9PlanningStorePersistsAcrossBuild(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	cfgPath := filepath.Join(dir, "config.json")
	cfg := config.Default()
	cfg.DataDir = dataDir
	cfg.MaxSteps = 5
	cfg.TimeoutSeconds = 30
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	a1, err := Build(cfgPath)
	if err != nil {
		t.Fatalf("Build #1: %v", err)
	}
	ctx := permissions.WithApprovalContext(context.Background(), 4242, 0)
	created, err := a1.Tools.Execute(ctx, "task_create", json.RawMessage(`{"title":"persist me","notes":"rc9"}`))
	if err != nil {
		t.Fatalf("task_create: %v", err)
	}
	var task struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(created), &task); err != nil {
		t.Fatalf("task_create output is not a task (%q): %v", created, err)
	}
	if task.ID == "" || task.Title != "persist me" {
		t.Fatalf("task_create = %+v, want a task titled %q", task, "persist me")
	}
	if err := a1.Close(); err != nil {
		t.Fatalf("Close #1: %v", err)
	}
	a2, err := Build(cfgPath)
	if err != nil {
		t.Fatalf("Build #2: %v", err)
	}
	defer a2.Close()
	got, err := a2.Tools.Execute(ctx, "task_get", json.RawMessage(`{"id":"`+task.ID+`"}`))
	if err != nil {
		t.Fatalf("task_get after rebuild: %v (planning state did not persist — is SetDataDir wired in Build?)", err)
	}
	if !strings.Contains(got, "persist me") {
		t.Errorf("task_get after rebuild = %q, want it to contain %q", got, "persist me")
	}
}
