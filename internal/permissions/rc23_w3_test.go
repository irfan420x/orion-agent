package permissions

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// TestRC23W3LegacyGlobalRuleWarnsOnce (F-A17-6): the first use of a
// legacy global rule for a chat logs a migration warning — once per
// tool — so the owner learns the pre-rc22 rule is still live
// everywhere. A chat's own rule never warns, and a direct global
// lookup (chatID 0) never warns.
func TestRC23W3LegacyGlobalRuleWarnsOnce(t *testing.T) {
	store := NewRuleStore("")
	if err := store.Set("shell_exec", ActionAllow); err != nil {
		t.Fatalf("Set global rule: %v", err)
	}

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })

	if a, ok := store.LookupForChat(123, "shell_exec"); !ok || a != ActionAllow {
		t.Fatalf("LookupForChat(123, shell_exec) = %q, %v, want allow, true", a, ok)
	}
	if got := buf.String(); !strings.Contains(got, "legacy global rule") || !strings.Contains(got, "shell_exec") || !strings.Contains(got, "123") {
		t.Errorf("first fallback use: log = %q, want a legacy-global warning naming the tool and chat", got)
	}
	// Second use of the same tool: no second warning.
	buf.Reset()
	if _, ok := store.LookupForChat(456, "shell_exec"); !ok {
		t.Fatal("LookupForChat(456, shell_exec): expected the global rule to apply")
	}
	if got := buf.String(); got != "" {
		t.Errorf("second fallback use: log = %q, want no repeat warning", got)
	}
	// A different tool warns once for itself.
	if err := store.Set("web_fetch", ActionDeny); err != nil {
		t.Fatalf("Set global web_fetch rule: %v", err)
	}
	buf.Reset()
	if _, ok := store.LookupForChat(123, "web_fetch"); !ok {
		t.Fatal("LookupForChat(123, web_fetch): expected the global rule to apply")
	}
	if got := buf.String(); !strings.Contains(got, "web_fetch") {
		t.Errorf("first web_fetch fallback use: log = %q, want a warning for web_fetch", got)
	}
	// A chat's own rule never warns, and neither does a direct
	// global lookup.
	buf.Reset()
	if err := store.SetForChat(123, "shell_exec", ActionDeny); err != nil {
		t.Fatalf("SetForChat: %v", err)
	}
	if a, ok := store.LookupForChat(123, "shell_exec"); !ok || a != ActionDeny {
		t.Fatalf("LookupForChat(123, shell_exec) after per-chat set = %q, %v, want deny, true", a, ok)
	}
	if _, ok := store.LookupForChat(0, "shell_exec"); !ok {
		t.Fatal("LookupForChat(0, shell_exec): expected the global rule")
	}
	if got := buf.String(); got != "" {
		t.Errorf("per-chat / direct-global lookups: log = %q, want no warning", got)
	}
}
