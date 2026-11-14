// F-A22-16: the plan-mode tools must not be callable from a
// subagent child. A child runs in the parent's context, inherits
// the parent's chat id, and shares the parent's planning store —
// so a child calling exit_plan_mode would clear the PARENT
// chat's pending plan. Both tools are excluded from the child
// registry; the pending-plan BLOCK itself must survive the
// exclusion (the registry keeps the plan-mode checker), so a
// child is still refused destructive tools while a parent plan
// is pending.
package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/tools"
)

func TestChildRegistryExcludesPlanModeTools(t *testing.T) {
	dataDir := t.TempDir()
	parentStore := tools.NewPlanningStore(dataDir)
	chat := permissions.WithApprovalContext(context.Background(), 4242, 4242)

	childReg, err := newChildRegistry(testSubagentConfig(5), parentStore)
	if err != nil {
		t.Fatalf("newChildRegistry: %v", err)
	}
	for _, name := range []string{"enter_plan_mode", "exit_plan_mode"} {
		if _, ok := childReg.Get(name); ok {
			t.Errorf("child registry has %q, want it excluded (F-A22-16)", name)
		}
		if _, err := childReg.Execute(chat, name, json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "unknown tool") {
			t.Errorf("child Execute(%q): err = %v, want an unknown-tool error", name, err)
		}
	}
	// Only the plan-mode tools are gone: the child keeps the
	// rest of the registry (a child's task_create still lands in
	// the shared store).
	if _, ok := childReg.Get("task_create"); !ok {
		t.Error("child registry lost task_create, want only the plan-mode tools removed")
	}

	// The parent stores a pending plan in the shared store (a
	// plans file written the way the plan tools write it).
	now := time.Now().UTC()
	doc := map[string]any{"plan": map[string]any{
		"id":         "plan_parent",
		"goal":       "Parent plan",
		"steps":      []string{"Step"},
		"requester":  4242,
		"created_at": now.Add(-time.Minute).Format(time.RFC3339),
		"expires_at": now.Add(5 * time.Minute).Format(time.RFC3339),
	}}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	chatDir := filepath.Join(dataDir, "chats", "4242")
	if err := os.MkdirAll(chatDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	plansPath := filepath.Join(chatDir, "plans.json")
	if err := os.WriteFile(plansPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// The block survives the exclusion: the child's destructive
	// tools are still refused while the parent's plan is pending.
	if blocked, _ := childReg.PlanModeBlocks(chat, "shell_exec"); !blocked {
		t.Error("child PlanModeBlocks(shell_exec) with a pending parent plan = false, want true (checker retained)")
	}
	// The adversarial attempt: the child tries to clear the
	// parent's plan. It must fail as an unknown tool, and the
	// plan must survive.
	if _, err := childReg.Execute(chat, "exit_plan_mode", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("child Execute(exit_plan_mode) with a pending parent plan: err = %v, want an unknown-tool error", err)
	}
	after, err := os.ReadFile(plansPath)
	if err != nil {
		t.Fatalf("plans file after the child's exit attempt: %v", err)
	}
	if !strings.Contains(string(after), "plan_parent") {
		t.Errorf("plans file after the child's exit attempt = %s, want the parent's plan still stored", after)
	}
	if blocked, _ := childReg.PlanModeBlocks(chat, "shell_exec"); !blocked {
		t.Error("child PlanModeBlocks(shell_exec) after the child's exit attempt = false, want true (plan survived)")
	}

	// The PARENT can still clear its own plan through its own
	// registry — the plan was real, and only the parent can
	// abandon it.
	parentReg, err := tools.DefaultRegistryWithDeps(tools.ToolDeps{PlanningStore: parentStore})
	if err != nil {
		t.Fatalf("DefaultRegistryWithDeps (parent): %v", err)
	}
	res, err := parentReg.Execute(chat, "exit_plan_mode", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("parent exit_plan_mode: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res), &out); err != nil {
		t.Fatalf("parent exit result %q is not JSON: %v", res, err)
	}
	if out["exited"] != true || out["plan_id"] != "plan_parent" {
		t.Errorf("parent exit = %v, want exited=true with plan_id plan_parent", out)
	}
	if blocked, _ := childReg.PlanModeBlocks(chat, "shell_exec"); blocked {
		t.Error("child PlanModeBlocks(shell_exec) after the parent's exit = true, want false")
	}
}
