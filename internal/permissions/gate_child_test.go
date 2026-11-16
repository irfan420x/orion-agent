package permissions

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestNewChildGateDeniesGatedTools (F-A23-2): the non-interactive
// child gate denies every destructive and network tool promptly —
// no prompter is ever consulted, so no child turn can stall on an
// approval window — while safe tools still run, so a child keeps
// the read/compute tools it needs. A denylisted shell command is
// denied on the denylist path, before any other check.
func TestNewChildGateDeniesGatedTools(t *testing.T) {
	g := NewChildGate()
	if g == nil {
		t.Fatal("NewChildGate() = nil, want a usable gate")
	}
	ctx := context.Background()

	// Safe tools run: a child that could not read or compute
	// could do no useful work at all.
	for _, tool := range []string{"calculator", "file_read", "grep", "task_create", "file_list"} {
		if ok, reason := g.Check(ctx, tool, `{}`); !ok {
			t.Errorf("child gate Check(%q) denied: %s — safe tools must still run in a child", tool, reason)
		}
	}

	// Destructive and network tools are denied, promptly: the
	// child has no approval surface, so the denial must come
	// back without waiting. 10s is generous — a nil prompter
	// returns immediately — and guards against a future change
	// that makes the child gate wait on an answer that can
	// never arrive.
	denied := []struct {
		tool   string
		args   string
		reason string // substring the denial reason must contain
	}{
		{"shell_exec", `{"command":"echo hi"}`, "no approver"},
		{"file_write", `{"path":"/tmp/x","content":"x"}`, "no approver"},
		{"file_edit", `{"path":"/tmp/x","old":"a","new":"b"}`, "no approver"},
		{"input", `{"op":"move","x":10,"y":10}`, "no approver"},
		{"web_fetch", `{"url":"https://example.com"}`, "no approver"},
		{"web_search", `{"query":"example"}`, "no approver"},
		// file_delete has no registered tool, but the gate
		// still denies it: an unclassified name is denied
		// outright (F-A21-1), before any rule is consulted —
		// so its reason names the classification, not the
		// missing approver.
		{"file_delete", `{"path":"/tmp/x"}`, "not in the permission-gate classification"},
		// An MCP tool is ClassNetwork and would be denied
		// the same way, even though MCP tools never reach a
		// child registry.
		{"mcp__fake__echo", `{"text":"hi"}`, "no approver"},
	}
	for _, c := range denied {
		start := time.Now()
		ok, reason := g.Check(ctx, c.tool, c.args)
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("child gate Check(%q) took %v, want a prompt denial (no approver can ever answer a child turn)", c.tool, d)
		}
		if ok {
			t.Errorf("child gate Check(%q) allowed, want denial", c.tool)
		}
		if !strings.Contains(reason, c.reason) {
			t.Errorf("child gate Check(%q) reason = %q, want it to contain %q", c.tool, reason, c.reason)
		}
	}

	// The shell denylist still fires first, even on the child
	// gate: a denylisted command is denied as denylisted, not
	// merely as unapproved.
	if ok, reason := g.Check(ctx, "shell_exec", `{"command":"rm -rf /"}`); ok || !strings.Contains(reason, "denylist") {
		t.Errorf("child gate Check(shell_exec, denylisted) = (%v, %q), want denial naming the denylist", ok, reason)
	}

	// Case folding (F-A22-6): SHELL_EXEC is shell_exec, never a
	// second, unguarded identity.
	if ok, _ := g.Check(ctx, "SHELL_EXEC", `{"command":"echo hi"}`); ok {
		t.Error("child gate Check(SHELL_EXEC) allowed, want denial (case-folded to shell_exec)")
	}
}
