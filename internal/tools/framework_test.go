// Framework tests: auto-discovery (factories) and the risk-class
// declaration every built-in tool must make.
package tools

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/irfan/orion/internal/model"
	"github.com/irfan/orion/internal/permissions"
	"github.com/irfan/orion/internal/skills"
	"github.com/irfan/orion/internal/subagent"
)

// TestEveryBuiltInToolDeclaresRiskClass pins the framework's
// fail-closed rule: EVERY built-in tool implements RiskClasser, so
// a destructive tool cannot silently slip in as Safe by forgetting
// to declare. It also pins that each declaration agrees with the
// permission gate's hardcoded maps (Classify): the installed map
// never changes what the maps already decide, it only makes the
// declaration explicit.
func TestEveryBuiltInToolDeclaresRiskClass(t *testing.T) {
	r, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	for _, tool := range r.List() {
		rc, ok := tool.(RiskClasser)
		if !ok {
			t.Errorf("tool %q does not implement RiskClasser: declare its risk class explicitly", tool.Name())
			continue
		}
		if got, want := rc.RiskClass(), permissions.Classify(tool.Name()); got != want {
			t.Errorf("tool %q declares %v, want %v (Classify)", tool.Name(), got, want)
		}
	}
	// The late-registered tools (subagent + skill, registered by
	// app.Build after the agent exists) must declare too: build
	// them the way Build does and check them the same way.
	sub, err := NewSubagentTools(subagent.NewManager(4), UsageSinkFunc(func(int) {}))
	if err != nil {
		t.Fatalf("NewSubagentTools: %v", err)
	}
	skill, err := NewSkillTool(skills.NewRegistry())
	if err != nil {
		t.Fatalf("NewSkillTool: %v", err)
	}
	for _, tool := range append(sub, skill) {
		rc, ok := tool.(RiskClasser)
		if !ok {
			t.Errorf("tool %q does not implement RiskClasser", tool.Name())
			continue
		}
		if got, want := rc.RiskClass(), permissions.Classify(tool.Name()); got != want {
			t.Errorf("tool %q declares %v, want %v (Classify)", tool.Name(), got, want)
		}
	}
}

// TestFactoriesRegisterAllDefaultTools pins that the default
// registry is built from the factories — every registered tool
// comes from exactly one factory, and no factory is missing or
// unregistered.
func TestFactoriesRegisterAllDefaultTools(t *testing.T) {
	r, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	factoryNames := make([]string, 0, len(registeredFactories()))
	for _, e := range registeredFactories() {
		factoryNames = append(factoryNames, e.name)
	}
	toolNames := make([]string, 0, len(r.List()))
	for _, tool := range r.List() {
		toolNames = append(toolNames, tool.Name())
	}
	sort.Strings(factoryNames)
	sort.Strings(toolNames)
	if len(factoryNames) != len(toolNames) {
		t.Fatalf("factories %v != tools %v", factoryNames, toolNames)
	}
	for i := range factoryNames {
		if factoryNames[i] != toolNames[i] {
			t.Fatalf("factories %v != tools %v", factoryNames, toolNames)
		}
	}
}

// --- Register fail-closed enforcement (F-A20-2) ---

// noRiskTool is a minimal Tool that does NOT implement
// RiskClasser, for pinning that Register rejects it loudly.
type noRiskTool struct{ name string }

func (t noRiskTool) Name() string { return t.name }
func (t noRiskTool) Description() string {
	return "test tool without a risk class"
}
func (t noRiskTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: t.name, Description: t.Description(), Parameters: map[string]any{"type": "object"}}
}
func (t noRiskTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return "ok", nil
}

// riskTool is a minimal Tool with a configurable declared risk
// class, for pinning that Register rejects a declaration that
// disagrees with the gate's hardcoded maps.
type riskTool struct {
	name  string
	class permissions.Class
}

func (t riskTool) Name() string { return t.name }
func (t riskTool) Description() string {
	return "test tool with a declared risk class"
}
func (t riskTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: t.name, Description: t.Description(), Parameters: map[string]any{"type": "object"}}
}
func (t riskTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return "ok", nil
}
func (t riskTool) RiskClass() permissions.Class { return t.class }

// TestRegisterRequiresRiskClass pins the fail-closed rule: a
// tool that does not implement RiskClasser is rejected at
// registration, loudly — it must never silently default to Safe
// and run a destructive action without approval.
func TestRegisterRequiresRiskClass(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(noRiskTool{name: "echo"}); err == nil {
		t.Fatal("Register accepted a tool without RiskClasser, want a loud error")
	}
	if _, ok := r.Get("echo"); ok {
		t.Fatal("rejected tool is registered anyway")
	}
}

// TestRegisterRejectsMismatchedRiskClass pins that a tool whose
// declared class disagrees with the gate's hardcoded maps
// (permissions.Classify) is rejected at registration: the maps
// are the authority, and a wrong declaration must fail loudly,
// not silently downgrade the tool to Safe.
func TestRegisterRejectsMismatchedRiskClass(t *testing.T) {
	cases := []struct {
		name  string
		class permissions.Class
	}{
		{"shell_exec", permissions.ClassSafe},        // destructive name, Safe declaration
		{"file_write", permissions.ClassSafe},        // destructive name, Safe declaration
		{"calculator", permissions.ClassDestructive}, // safe name, Destructive declaration
		{"web_fetch", permissions.ClassSafe},         // network name, Safe declaration
		{"mcp__srv__tool", permissions.ClassSafe},    // MCP name, Safe declaration
	}
	for _, tc := range cases {
		r := NewRegistry()
		if err := r.Register(riskTool{name: tc.name, class: tc.class}); err == nil {
			t.Errorf("Register(%q declaring %v): accepted, want a loud error", tc.name, tc.class)
		}
		if _, ok := r.Get(tc.name); ok {
			t.Errorf("Register(%q declaring %v): rejected tool is registered anyway", tc.name, tc.class)
		}
	}
}

// TestRegisterAcceptsMatchingRiskClass pins the other side: a
// tool whose declaration agrees with the gate's maps registers
// fine, including an MCP-named tool declaring Network and an
// unknown name declaring Safe (Classify's default for unknown
// names).
func TestRegisterAcceptsMatchingRiskClass(t *testing.T) {
	cases := []struct {
		name  string
		class permissions.Class
	}{
		{"shell_exec", permissions.ClassDestructive},
		{"file_write", permissions.ClassDestructive},
		{"calculator", permissions.ClassSafe},
		{"web_fetch", permissions.ClassNetwork},
		{"mcp__srv__tool", permissions.ClassNetwork},
		{"echo", permissions.ClassSafe},
	}
	for _, tc := range cases {
		r := NewRegistry()
		if err := r.Register(riskTool{name: tc.name, class: tc.class}); err != nil {
			t.Errorf("Register(%q declaring %v): %v, want accepted", tc.name, tc.class, err)
		}
	}
}

// TestToolClasses pins Registry.ToolClasses: it returns exactly
// the declared class of every RiskClasser tool, and nothing for
// tools that declare nothing.
func TestToolClasses(t *testing.T) {
	r, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	classes := r.ToolClasses()
	if len(classes) != len(r.List()) {
		t.Fatalf("ToolClasses has %d entries for %d tools", len(classes), len(r.List()))
	}
	for _, tool := range r.List() {
		got, ok := classes[tool.Name()]
		if !ok {
			t.Errorf("ToolClasses has no entry for %q", tool.Name())
			continue
		}
		if want := tool.(RiskClasser).RiskClass(); got != want {
			t.Errorf("ToolClasses[%q] = %v, want %v", tool.Name(), got, want)
		}
	}
	// A nil registry yields an empty map, never a panic.
	var nilReg *Registry
	if got := nilReg.ToolClasses(); len(got) != 0 {
		t.Errorf("nil registry ToolClasses = %v, want empty", got)
	}
}
