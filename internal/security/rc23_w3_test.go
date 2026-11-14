package security

import "testing"

// TestRC23W3ProcBypassBlocked (F-A17-5): F-A16-9's sensitive-path
// read rule is bypassed via /proc when the sandbox is OFF —
// /proc is the live process's procfs, so /proc/self/environ leaks
// the daemon's environment and /proc/self/cwd|root re-root a
// sensitive path through the process. The audit's four probe cases
// must ALL be blocked, and ordinary /proc diagnostics must stay
// allowed. Every case calls IsBlockedCommand directly — no live
// command is ever executed.
func TestRC23W3ProcBypassBlocked(t *testing.T) {
	blocked := []string{
		// The audit's four probe cases (Audit 17 F-A17-5).
		`cat /proc/self/environ`,
		`cat /proc/1234/environ`,
		`cat /proc/self/cwd/.orion/config.json`,
		`cat /proc/self/root/home/irfan/.orion/config.json`,
		// Same entries through other binaries / forms.
		`head -c 100 /proc/self/environ`,
		`cat /proc/1/fd/3`,
		`cat /proc/self/fd/0`,
		`cat /proc/self/root/etc/passwd`,
		`grep -r foo /proc/self/cwd`,
		`cat --file=/proc/self/environ`,
		`f=/proc/self/environ; cat $f`,
		`cat //proc/self/environ`,
		`cat /proc/self/root/home/irfan/.ssh/id_ed25519`,
	}
	for _, cmd := range blocked {
		if !IsBlockedCommand(cmd) {
			t.Errorf("IsBlockedCommand(%q) = false, want true (F-A17-5 /proc bypass)", cmd)
		}
	}

	allowed := []string{
		`cat /proc/self/status`,
		`cat /proc/cpuinfo`,
		`cat /proc/1234/status`,
		`ls /proc`,
		`cat /proc/self/stat`,
		`cat /proc/meminfo`,
		`ps aux`,
	}
	for _, cmd := range allowed {
		if IsBlockedCommand(cmd) {
			t.Errorf("IsBlockedCommand(%q) = true, want false (ordinary /proc diagnostics must stay allowed)", cmd)
		}
	}
}
