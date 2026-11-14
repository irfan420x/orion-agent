// Shared typed-nil helper tests (F-A22-9).
package tools

import (
	"testing"

	"github.com/irfan/orion/internal/permissions"
)

// TestIsNilValue pins the shared typed-nil helper against the
// testify/lo kind list (Chan, Func, Interface, Map, Ptr, Slice,
// UnsafePointer): typed nils are nil, live values are not, and —
// the load-bearing part — values of non-nilable kinds (int,
// string, struct) return false WITHOUT panicking, because IsNil
// is only ever called after the Kind guard.
func TestIsNilValue(t *testing.T) {
	var nilTool *nilReceiverTool
	var nilFn func()
	var nilMap map[string]int
	var nilSlice []string
	var nilChan chan int
	var nilIface any
	var nilPtr *int
	unsafeNil := func() any {
		// A typed nil through the empty interface, the shape
		// Register and replyChain.add actually receive.
		var p *nilReceiverTool
		return p
	}()
	nilCases := map[string]any{
		"untyped nil":      nil,
		"nil *T":           nilTool,
		"nil func":         nilFn,
		"nil map":          nilMap,
		"nil slice":        nilSlice,
		"nil chan":         nilChan,
		"nil interface":    nilIface,
		"nil *int":         nilPtr,
		"typed nil in any": unsafeNil,
	}
	for name, v := range nilCases {
		if !IsNilValue(v) {
			t.Errorf("IsNilValue(%s): false, want true", name)
		}
	}
	// The test-writing trap from the research: asserting with a
	// plain interface comparison, NOT a reflecting assertion —
	// a NotNil-style check would call the typed nil nil itself
	// and pass against unfixed code. Here the plain comparison
	// proves the probe value really is a typed nil (an interface
	// holding a nil pointer compares non-nil).
	if unsafeNil == nil {
		t.Fatal("test setup broken: the typed-nil probe compares nil as an interface, so it would not exercise the guard")
	}
	nonNil := map[string]any{
		"live *T":      &nilReceiverTool{},
		"live func":    func() {},
		"live map":     map[string]int{},
		"live slice":   []string{},
		"zero int":     0,
		"empty string": "",
		"zero struct":  struct{}{},
		"false bool":   false,
		"live Tool":    riskTool{name: "calculator", class: permissions.ClassSafe},
	}
	for name, v := range nonNil {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.Errorf("IsNilValue(%s): panicked %v, want false (Kind guard must skip IsNil)", name, rec)
				}
			}()
			if IsNilValue(v) {
				t.Errorf("IsNilValue(%s): true, want false", name)
			}
		}()
	}
}
