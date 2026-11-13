// Regression test for Audit 5 F-A5-P2-35 (rc10, worker W5):
// subagent children silently lost all planning state because
// newChildRegistry built a fresh DefaultRegistry with a fresh
// in-memory PlanningStore. Passing the parent's store into
// newChildRegistry must make a child's task_create land in the
// chat's files, visible to the parent's task_list.
package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/tools"
)

// TestRC10ChildRegistrySharesParentPlanningStore (F-A5-P2-35): a
// child registry built with the parent's PlanningStore shares the
// chat's planning state — a task created through the child is
// visible through the parent's tools, in the chat's files on disk.
func TestRC10ChildRegistrySharesParentPlanningStore(t *testing.T) {
	dataDir := t.TempDir()
	parentStore := tools.NewPlanningStore(dataDir)
	chat := permissions.WithApprovalContext(context.Background(), 4242, 4242)

	childReg, err := newChildRegistry(testSubagentConfig(5), parentStore)
	if err != nil {
		t.Fatalf("newChildRegistry: %v", err)
	}
	args, _ := json.Marshal(map[string]any{"title": "child-planned task"})
	if _, err := childReg.Execute(chat, "task_create", args); err != nil {
		t.Fatalf("child task_create failed: %v", err)
	}
	// The parent sees it through its own store (same store object).
	parentTools := tools.NewPlanningTools(parentStore)
	var listTool tools.Tool
	for _, tl := range parentTools {
		if tl.Name() == "task_list" {
			listTool = tl
		}
	}
	if listTool == nil {
		t.Fatal("parent planning tools lack task_list")
	}
	out, err := listTool.Execute(chat, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("parent task_list failed: %v", err)
	}
	if !strings.Contains(out, "child-planned task") {
		t.Fatalf("parent must see the child's task, got:\n%s", out)
	}
	// And it landed in the chat's files, not in memory only.
	var list struct {
		Tasks []*tools.Task `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tasks) != 1 {
		t.Fatalf("expected 1 shared task, got %d", len(list.Tasks))
	}
}

// TestRC10ChildRegistryWithoutParentStoreIsIsolated (F-A5-P2-35):
// the nil-store form keeps the old behaviour — a child without a
// parent store plans in its own in-memory store, invisible to the
// parent. This pins the opt-in: only registries built with a store
// share state.
func TestRC10ChildRegistryWithoutParentStoreIsIsolated(t *testing.T) {
	dataDir := t.TempDir()
	parentStore := tools.NewPlanningStore(dataDir)
	chat := permissions.WithApprovalContext(context.Background(), 4242, 4242)

	childReg, err := newChildRegistry(testSubagentConfig(5))
	if err != nil {
		t.Fatalf("newChildRegistry: %v", err)
	}
	args, _ := json.Marshal(map[string]any{"title": "child-only task"})
	if _, err := childReg.Execute(chat, "task_create", args); err != nil {
		t.Fatalf("child task_create failed: %v", err)
	}
	parentTools := tools.NewPlanningTools(parentStore)
	var listTool tools.Tool
	for _, tl := range parentTools {
		if tl.Name() == "task_list" {
			listTool = tl
		}
	}
	out, err := listTool.Execute(chat, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("parent task_list failed: %v", err)
	}
	if strings.Contains(out, "child-only task") {
		t.Fatalf("parent must NOT see a task from an unshared child store, got:\n%s", out)
	}
}
