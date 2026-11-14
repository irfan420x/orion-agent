// Framework tests: auto-discovery (factories) and the risk-class
// declaration every built-in tool must make.
package tools

import (
	"sort"
	"testing"

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
