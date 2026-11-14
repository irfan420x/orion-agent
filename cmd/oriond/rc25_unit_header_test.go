// rc25 tests: F-A19-3 — the shipped unit's header must name the
// real CapabilityBoundingSet gate (CAP_SYS_ADMIN + CAP_SETFCAP),
// not CAP_SETUID. The drift-guard test pins directives, not
// comments, so the wrong claim survived in the header unnoticed;
// this test pins the comment itself, in both unit files, so the
// two copies cannot drift again.
package main

import (
	"os"
	"strings"
	"testing"
)

func TestUnitHeaderNamesRealCapabilityGate(t *testing.T) {
	for _, path := range []string{"../../oriond.service", "../../systemd/oriond.service"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(raw)
		// Both files must name the real gate: CAP_SYS_ADMIN +
		// CAP_SETFCAP, with CAP_SETUID explicitly NOT the gate.
		if !strings.Contains(text, "CAP_SYS_ADMIN + CAP_SETFCAP") {
			t.Errorf("%s: header does not name the real gate \"CAP_SYS_ADMIN + CAP_SETFCAP\"", path)
		}
		if !strings.Contains(text, "CAP_SETUID is NOT the gate") {
			t.Errorf("%s: header does not say CAP_SETUID is NOT the gate", path)
		}
		// The old wrong claim — that the uid_map write needs
		// effective CAP_SETUID — must stay gone.
		if strings.Contains(text, "needs effective\n#   CAP_SETUID") || strings.Contains(text, "needs effective CAP_SETUID") {
			t.Errorf("%s: header still claims the uid_map write needs effective CAP_SETUID (F-A19-3)", path)
		}
	}
}
