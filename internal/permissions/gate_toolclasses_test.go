// SetToolClasses tests (tools framework): the gate consults the
// installed per-tool declarations before the hardcoded maps, and
// falls back to Classify for tools the map does not name.
package permissions

import (
	"context"
	"testing"
	"time"
)

func TestGateSetToolClasses(t *testing.T) {
	ctx := WithApprovalContext(context.Background(), 42, 42)
	// A prompter that denies immediately (no approval flow): Check
	// on a gated tool returns false without prompting.
	deny := func(ctx context.Context, req ApprovalRequest) error {
		return context.DeadlineExceeded
	}
	// Without an installed map, calculator is Safe (hardcoded
	// maps) and Check allows it.
	g := NewGate(NewRuleStore(""), deny)
	g.SetTimeout(10 * time.Millisecond)
	if ok, _ := g.Check(ctx, "calculator", `{}`); !ok {
		t.Fatal("calculator without installed classes: Check denied, want allowed (ClassSafe)")
	}
	// Install a map that (wrongly, for the test) declares
	// calculator Destructive: Check must now gate it — the
	// installed declaration wins over the hardcoded maps.
	g.SetToolClasses(map[string]Class{"calculator": ClassDestructive})
	if ok, _ := g.Check(ctx, "calculator", `{}`); ok {
		t.Fatal("calculator declared Destructive: Check allowed, want gated")
	}
	// A tool absent from the map still falls back to Classify:
	// file_read stays Safe, web_fetch stays Network.
	if ok, _ := g.Check(ctx, "file_read", `{}`); !ok {
		t.Fatal("file_read (not in the installed map): Check denied, want allowed (Classify fallback)")
	}
	if ok, _ := g.Check(ctx, "web_fetch", `{}`); ok {
		t.Fatal("web_fetch (not in the installed map): Check allowed, want gated (Classify fallback)")
	}
	// Clearing the map restores the hardcoded behaviour.
	g.SetToolClasses(nil)
	if ok, _ := g.Check(ctx, "calculator", `{}`); !ok {
		t.Fatal("calculator after clearing installed classes: Check denied, want allowed")
	}
	// Nil gate: no-op, never panics.
	var nilGate *Gate
	nilGate.SetToolClasses(map[string]Class{"calculator": ClassDestructive})
	if got := nilGate.classify("calculator"); got != ClassSafe {
		t.Errorf("nil gate classify = %v, want ClassSafe (Classify fallback)", got)
	}
}
