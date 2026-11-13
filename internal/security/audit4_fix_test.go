// Audit 4 P1-1 / P1-2 regression tests (strings only — never executed).
package security

import "testing"

// TestAudit4VariableRedirectBlocked: redirect and file-op targets
// reached through a variable or a command substitution must be
// refused exactly like the literal path.
func TestAudit4VariableRedirectBlocked(t *testing.T) {
	blocked := []string{
		`f=/etc/passwd; echo pwned > $f`,
		`f=/etc/passwd; echo pwned > ${f}`,
		`f=/etc/shadow; echo x | tee $f`,
		`f=/etc/passwd; cp /tmp/x $f`,
		`f=/etc/passwd; mv /tmp/x $f`,
		`f=/etc/passwd; ln -sf /tmp/x $f`,
		`f=/etc/passwd; echo pwned > $f.txt`,
		`echo pwned > $(echo /etc/passwd)`,
		`echo pwned > $(echo /etc/shadow)`,
		`d=/etc; echo pwned > $d/passwd`,
		`d=/etc; echo pwned > ${d}/shadow`,
		`f=/etc/passwd; echo x | tee -a $f`,
		`f=/etc/passwd; chmod 644 $f`,
		`echo pwned > $unknown_var`,
		`f=/tmp/ok; echo pwned > $f; g=/etc/passwd; echo pwned > $g`,
		// Quoted forms: tokenize strips the quotes, the $ survives.
		`f=/etc/passwd; echo pwned > "$f"`,
		`f=/etc/passwd; cp /tmp/x "$f"`,
	}
	for _, cmd := range blocked {
		if !IsBlockedCommand(cmd) {
			t.Errorf("IsBlockedCommand(%q) = false, want true", cmd)
		}
	}
}

// TestAudit4VariableRedirectAllowed: variable indirection to an
// ordinary file, and ordinary redirects, stay allowed.
func TestAudit4VariableRedirectAllowed(t *testing.T) {
	allowed := []string{
		`f=/tmp/note.txt; echo hello > $f`,
		`f=/tmp/note.txt; echo hello > ${f}`,
		`d=/tmp; echo hello > $d/note.txt`,
		`f=/tmp/note.txt; cp /tmp/x $f`,
		`f=/tmp/note.txt; echo x | tee $f`,
		`echo hello > /tmp/note.txt`,
		`echo hello > /dev/null`,
		`f=/tmp/note.txt; chmod 644 $f`,
		`chmod 644 /tmp/note.txt`,
	}
	for _, cmd := range allowed {
		if IsBlockedCommand(cmd) {
			t.Errorf("IsBlockedCommand(%q) = true, want false", cmd)
		}
	}
}

// TestAudit4ChmodSystemPathBlocked: any chmod of a system path is
// refused whatever the mode — 644, 000, setuid/setgid and symbolic
// modes included.
func TestAudit4ChmodSystemPathBlocked(t *testing.T) {
	blocked := []string{
		`chmod 644 /etc/shadow`,
		`chmod 4755 /bin/sh`,
		`chmod 000 /etc/passwd`,
		`chmod u+s /bin/bash`,
		`chmod g+r /etc/shadow`,
		`chmod 755 /etc`,
		`chmod -R 755 /etc`,
		`chmod --recursive 644 /usr/bin/foo`,
		`chmod 777 /tmp/x`,
		`chmod 644 /etc/passwd /tmp/ok`,
		`chmod 644 -- /etc/passwd`,
	}
	for _, cmd := range blocked {
		if !IsBlockedCommand(cmd) {
			t.Errorf("IsBlockedCommand(%q) = false, want true", cmd)
		}
	}
}

// TestAudit4ChmodOrdinaryAllowed: chmod of ordinary files (any
// non-777 mode, recursive or not) stays allowed.
func TestAudit4ChmodOrdinaryAllowed(t *testing.T) {
	allowed := []string{
		`chmod 644 file.txt`,
		`chmod 755 script.sh`,
		`chmod -R 755 ./mydir`,
		`chmod u+x /tmp/tool`,
		`chmod 600 /tmp/secret.txt`,
	}
	for _, cmd := range allowed {
		if IsBlockedCommand(cmd) {
			t.Errorf("IsBlockedCommand(%q) = true, want false", cmd)
		}
	}
}
