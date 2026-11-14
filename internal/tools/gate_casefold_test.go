// Case-folding + typed-nil tests for the tool registry
// (F-A22-7, F-A22-9's helper): the registry's identity is the
// FOLDED tool name — a mixed-case duplicate must be rejected, a
// mixed-case lookup must find the one entry, and the shared
// nil helper must catch typed nils without panicking on
// non-nilable kinds.
package tools

import (
	"context"
	"testing"

	"github.com/irfan/orion/internal/permissions"
)

// TestRegisterDuplicateCaseVariant reproduces the Audit 22 F-A22-7
// live probe: registering "calculator" then "Calculator" (both
// Safe) must reject the second — two entries sharing one folded
// classification must never exist. Before the fix both were
// accepted.
func TestRegisterDuplicateCaseVariant(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(riskTool{name: "calculator", class: permissions.ClassSafe}); err != nil {
		t.Fatalf("Register(calculator): %v", err)
	}
	for _, dup := range []string{"Calculator", "CALCULATOR", " calculator "} {
		if err := r.Register(riskTool{name: dup, class: permissions.ClassSafe}); err == nil {
			t.Errorf("Register(%q) after calculator: accepted, want duplicate error", dup)
		}
	}
	if got := len(r.List()); got != 1 {
		t.Errorf("registry holds %d tools after duplicate attempts, want 1", got)
	}
	// The one entry stays reachable under every spelling of its
	// name: folding the store without folding the lookup would
	// turn a mixed-case Execute into "unknown tool".
	for _, name := range []string{"calculator", "Calculator", "CALCULATOR"} {
		tool, ok := r.Get(name)
		if !ok {
			t.Errorf("Get(%q): not found, want the registered calculator", name)
			continue
		}
		if tool.Name() != "calculator" {
			t.Errorf("Get(%q).Name() = %q, want the originally registered %q", name, tool.Name(), "calculator")
		}
	}
	out, err := r.Execute(context.Background(), "Calculator", []byte(`{}`))
	if err != nil || out != "ok" {
		t.Errorf("Execute(Calculator) = %q, %v; want %q, nil", out, err, "ok")
	}
}

// TestRegisterDuplicateExactStillRejected is the negative control:
// the exact-spelling duplicate the old code already caught must
// still be caught — the fold must not have weakened it.
func TestRegisterDuplicateExactStillRejected(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(riskTool{name: "calculator", class: permissions.ClassSafe}); err != nil {
		t.Fatalf("Register(calculator): %v", err)
	}
	if err := r.Register(riskTool{name: "calculator", class: permissions.ClassSafe}); err == nil {
		t.Error("Register(calculator) twice: accepted, want duplicate error")
	}
}

// TestRegisterFactoryDuplicateCaseVariant pins that
// RegisterFactory folds the name at the boundary: a factory
// registered as "CALCULATOR" duplicates the init-registered
// "calculator" factory and must panic loudly at registration
// time, not sail through to a second tool at construction.
// The panic happens BEFORE the append, so this test leaves the
// global factory list untouched (DefaultRegistry tests elsewhere
// in this package must keep seeing exactly one calculator).
func TestRegisterFactoryDuplicateCaseVariant(t *testing.T) {
	before := len(registeredFactories())
	defer func() {
		if rec := recover(); rec == nil {
			t.Error("RegisterFactory(CALCULATOR): no panic, want a loud duplicate panic")
		}
		if got := len(registeredFactories()); got != before {
			t.Errorf("factory list grew %d -> %d after the rejected registration; want unchanged", before, got)
		}
	}()
	RegisterFactory("CALCULATOR", func(deps ToolDeps) (Tool, error) {
		return riskTool{name: "calculator", class: permissions.ClassSafe}, nil
	})
}

// TestRemoveFoldsName pins that Registry.Remove folds the name
// like Register and Get do (F-A22-7's folded identity):
// Remove("Calculator") must remove the "calculator" entry.
// Before the fix it returned false and the tool stayed
// registered — a mixed-case spelling silently failed to remove.
func TestRemoveFoldsName(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(riskTool{name: "calculator", class: permissions.ClassSafe}); err != nil {
		t.Fatalf("Register(calculator): %v", err)
	}
	if !r.Remove("Calculator") {
		t.Fatal(`Remove("Calculator") = false, want true (folded identity)`)
	}
	if _, ok := r.Get("calculator"); ok {
		t.Error(`registry still has "calculator" after Remove("Calculator")`)
	}
	if r.Remove("CALCULATOR") {
		t.Error(`second Remove("CALCULATOR") = true, want false`)
	}
	if got := len(r.List()); got != 0 {
		t.Errorf("registry holds %d tools after removal, want 0", got)
	}
}
