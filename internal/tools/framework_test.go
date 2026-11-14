// Framework tests: auto-discovery (factories) and the risk-class
// declaration every built-in tool must make.
package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
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

// TestToolDepsHasNoDataDirField pins F-A20-3: ToolDeps must not
// carry a DataDir — the field existed, was read by nothing (the
// planning store's data dir travels with ToolDeps.PlanningStore,
// pointed at it by SetDataDir; TestRegistryPlanningStoreViaDeps
// pins that path end to end), and was removed. If a data dir is
// ever needed again it belongs on the store, not on ToolDeps: a
// field here would silently invite a second, drifting source of
// truth for where planning state lives.
func TestToolDepsHasNoDataDirField(t *testing.T) {
	typ := reflect.TypeOf(ToolDeps{})
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).Name == "DataDir" {
			t.Errorf("ToolDeps must not have a DataDir field (F-A20-3): the planning store's data dir travels with ToolDeps.PlanningStore")
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
		{"SHELL_EXEC", permissions.ClassSafe},        // case folding (F-A21-17): mixed-case gated name, Safe declaration
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
// fine, including an MCP-named tool declaring Network. Every name
// here is explicitly classified (in a map or by the mcp__ prefix):
// an unknown name declaring Safe is NOT accepted — see
// TestRegisterRejectsNovelNames (F-A21-1). The {"echo", Safe} case
// this test used to pin was the hole itself: Classify's old
// default for unknown names let any novel name in as Safe.
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
		// Case folding (F-A21-17): a mixed-case spelling of a
		// mapped name agrees with the folded classification.
		{"SHELL_EXEC", permissions.ClassDestructive},
	}
	for _, tc := range cases {
		r := NewRegistry()
		if err := r.Register(riskTool{name: tc.name, class: tc.class}); err != nil {
			t.Errorf("Register(%q declaring %v): %v, want accepted", tc.name, tc.class, err)
		}
	}
}

// TestRegisterRejectsNovelNames pins the F-A21-1 fail-closed rule:
// a tool whose name is in none of the gate's maps is rejected
// whatever it declares — Safe, Network or Destructive. This is the
// auditor's wipe_notes probe: a novel destructive tool declaring
// Safe must not sail through Register on a vacuous agreement with
// Classify's default. The gate denies such a name too (see
// TestGateDeniesUnknownTool), so the hole is closed at both layers.
func TestRegisterRejectsNovelNames(t *testing.T) {
	cases := []struct {
		name  string
		class permissions.Class
	}{
		{"wipe_notes", permissions.ClassSafe},
		{"wipe_notes", permissions.ClassNetwork},
		{"wipe_notes", permissions.ClassDestructive},
		{"brand_new_tool", permissions.ClassSafe},
		{"unknown_tool", permissions.ClassSafe},
		// Case folding (F-A21-17): a mixed-case spelling of an
		// unmapped name is still unmapped, whatever it declares.
		{"WIPE_NOTES", permissions.ClassSafe},
		{"WIPE_NOTES", permissions.ClassDestructive},
	}
	for _, tc := range cases {
		r := NewRegistry()
		if err := r.Register(riskTool{name: tc.name, class: tc.class}); err == nil {
			t.Errorf("Register(%q declaring %v): accepted, want a loud error (name is not in the gate's classification)", tc.name, tc.class)
		}
		if _, ok := r.Get(tc.name); ok {
			t.Errorf("Register(%q declaring %v): rejected tool is registered anyway", tc.name, tc.class)
		}
	}
}

// TestToolClasses pins Registry.ToolClasses: it returns exactly
// the declared class of every registered tool — one entry per
// tool, equal to its RiskClasser declaration. Every tool in a
// registry declares: Register rejects tools that do not implement
// RiskClasser, so there is no "tool that declares nothing" case
// left to pin here, and ToolClasses can never hold an entry for a
// non-declaring tool.
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
		rc, ok := tool.(RiskClasser)
		if !ok {
			t.Errorf("tool %q does not implement RiskClasser, but Register accepted it", tool.Name())
			continue
		}
		if want := rc.RiskClass(); got != want {
			t.Errorf("ToolClasses[%q] = %v, want %v", tool.Name(), got, want)
		}
	}
	// A nil registry yields an empty map, never a panic.
	var nilReg *Registry
	if got := nilReg.ToolClasses(); len(got) != 0 {
		t.Errorf("nil registry ToolClasses = %v, want empty", got)
	}
}

// TestReadmeToolsListEqualsFactorySet (F-A21-5) pins the README
// Tools section to the factory set exactly. The list is manual,
// and it went stale silently: ask_user_question shipped without
// ever being added, and plan_mode's two tools were missing too,
// while `make check-readme-counts` only ever parsed the test-count
// lines and never looked at the list. A tool added without a
// README entry now fails loudly here instead of shipping with
// stale user-facing docs. (The MCP sentence and the size-cap
// sentence are not a tool list, so the section is cut at "MCP
// servers configured" before the names are extracted.)
func TestReadmeToolsListEqualsFactorySet(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	text := string(raw)
	start := strings.Index(text, "## Tools (all wired into the agent)")
	if start < 0 {
		t.Fatal("README.md has no '## Tools (all wired into the agent)' section (heading changed?)")
	}
	rest := text[start:]
	end := strings.Index(rest, "\n## ")
	if end < 0 {
		t.Fatal("README.md Tools section has no following '## ' heading (section is last?)")
	}
	section := rest[:end]
	cut := strings.Index(section, "MCP servers configured")
	if cut < 0 {
		t.Fatal("README.md Tools section no longer has the 'MCP servers configured' sentence this test cuts the list at; update the test's cut point")
	}
	listPart := section[:cut]
	nameRe := regexp.MustCompile("`([a-z][a-z0-9_]*)`")
	matches := nameRe.FindAllStringSubmatch(listPart, -1)
	seen := map[string]bool{}
	var got []string
	for _, m := range matches {
		if seen[m[1]] {
			t.Errorf("README Tools section names %q twice", m[1])
		}
		seen[m[1]] = true
		got = append(got, m[1])
	}
	sort.Strings(got)
	want := fullBuiltInToolNames(t)
	if len(got) != len(want) {
		t.Fatalf("README Tools section lists %d tools %v, want the %d factory tools %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("README Tools section lists %v, want the factory set %v", got, want)
		}
	}
}

// --- F-A20-16: the manual lists are pinned to the factory set ---

// fullBuiltInToolNames returns every built-in tool name, built
// the way production wires them: the factory-registered tools
// (DefaultRegistry) plus the late-registered subagent tools and
// the skill tool (app.Build registers those after the agent
// exists).
func fullBuiltInToolNames(t *testing.T) []string {
	t.Helper()
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sub, err := NewSubagentTools(subagent.NewManager(4), UsageSinkFunc(func(int) {}))
	if err != nil {
		t.Fatalf("NewSubagentTools: %v", err)
	}
	if err := reg.RegisterAll(sub); err != nil {
		t.Fatalf("RegisterAll subagent tools: %v", err)
	}
	skill, err := NewSkillTool(skills.NewRegistry())
	if err != nil {
		t.Fatalf("NewSkillTool: %v", err)
	}
	if err := reg.Register(skill); err != nil {
		t.Fatalf("Register skill tool: %v", err)
	}
	names := make([]string, 0, len(reg.List()))
	for _, tool := range reg.List() {
		names = append(names, tool.Name())
	}
	sort.Strings(names)
	return names
}

// setKeys returns the sorted keys of a name set, for readable
// test failures.
func setKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TestKnownBuiltInToolsMatchesFactorySet pins the skills
// fallback list (knownBuiltInTools) to the factory set: every
// built-in tool name must appear in it, and it must contain no
// extra name. Adding a tool without updating the fallback — or
// removing one without removing it — fails loudly here instead
// of silently letting a skill call an unknown tool (or warning
// about a tool that no longer exists).
func TestKnownBuiltInToolsMatchesFactorySet(t *testing.T) {
	want := fullBuiltInToolNames(t)
	got := skills.KnownBuiltInTools()
	if len(got) != len(want) {
		t.Fatalf("KnownBuiltInTools %v != factory set %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("KnownBuiltInTools %v != factory set %v", got, want)
		}
	}
}

// TestClassifyMapsMatchFactorySet pins the gate's hardcoded maps
// (permissions.Classify) to the factory set, in both directions:
// over every built-in tool name, the Destructive set must be
// exactly {shell_exec, file_write, file_edit, subagent_spawn} and
// the Network set exactly {web_fetch, web_search}; every built-in
// name must be EXPLICITLY classified (ClassifyKnown), so a Safe
// tool is Safe because it is listed in the safe map, never because
// Classify defaulted it; and the classified-name set
// (KnownTools) must equal the factory set exactly, so a map entry
// for a tool that no longer exists fails loudly here too.
func TestClassifyMapsMatchFactorySet(t *testing.T) {
	wantDestructive := map[string]bool{"shell_exec": true, "file_write": true, "file_edit": true, "subagent_spawn": true}
	wantNetwork := map[string]bool{"web_fetch": true, "web_search": true}
	gotDestructive := map[string]bool{}
	gotNetwork := map[string]bool{}
	for _, name := range fullBuiltInToolNames(t) {
		if _, ok := permissions.ClassifyKnown(name); !ok {
			t.Errorf("built-in tool %q is not explicitly classified (not in any gate map): add it to the maps in internal/permissions/gate.go", name)
		}
		switch permissions.Classify(name) {
		case permissions.ClassDestructive:
			gotDestructive[name] = true
		case permissions.ClassNetwork:
			gotNetwork[name] = true
		}
	}
	if len(gotDestructive) != len(wantDestructive) {
		t.Fatalf("Classify Destructive set = %v, want %v", setKeys(gotDestructive), setKeys(wantDestructive))
	}
	for name := range wantDestructive {
		if !gotDestructive[name] {
			t.Fatalf("Classify Destructive set = %v, want %v (missing %q)", setKeys(gotDestructive), setKeys(wantDestructive), name)
		}
	}
	if len(gotNetwork) != len(wantNetwork) {
		t.Fatalf("Classify Network set = %v, want %v", setKeys(gotNetwork), setKeys(wantNetwork))
	}
	for name := range wantNetwork {
		if !gotNetwork[name] {
			t.Fatalf("Classify Network set = %v, want %v (missing %q)", setKeys(gotNetwork), setKeys(wantNetwork), name)
		}
	}
	// Both directions: the maps must name exactly the built-ins —
	// a stale entry for a removed/renamed tool fails here.
	want := fullBuiltInToolNames(t)
	got := permissions.KnownTools()
	if len(got) != len(want) {
		t.Fatalf("KnownTools %v != factory set %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("KnownTools %v != factory set %v", got, want)
		}
	}
}
