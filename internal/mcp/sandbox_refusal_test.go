package mcp

import (
	"errors"
	"strings"
	"testing"
)

// TestSandboxUnavailableRefusalNamesCauseAndDocs pins the MCP
// sandbox-unavailable refusal (F-A21-4): it names the cause (the
// Linux user namespace / CAP_SYS_ADMIN) and points at the docs, and
// never presents dropping the sandbox ("sandbox": false) or the
// unsandboxed fallback as the remedy.
func TestSandboxUnavailableRefusalNamesCauseAndDocs(t *testing.T) {
	err := sandboxUnavailableRefusal("probe-server", errors.New("sandbox: probe command failed (userns-after-clone): fork/exec /bin/sh: operation not permitted"))
	msg := err.Error()
	for _, want := range []string{"probe-server", "user namespace", "CAP_SYS_ADMIN", "README.md", "refusing to run unsandboxed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("MCP sandbox refusal %q does not contain %q", msg, want)
		}
	}
	for _, banned := range []string{"\"sandbox\": false", "allow_unsandboxed_fallback"} {
		if strings.Contains(msg, banned) {
			t.Errorf("MCP sandbox refusal must not present %s as a remedy, got: %q", banned, msg)
		}
	}
}
