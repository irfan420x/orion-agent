package security

import (
	"os"
	"path/filepath"
	"testing"
)

// Audit-2 fixes: denylist bypasses found by a fresh-eyes review of the
// current tree. Each case is a bypass (or a legitimate command that
// must stay allowed) that slipped through before the fix.

// TestAudit2WrapperBypasses: shell builtins and wrappers that run an
// arbitrary command (exec, builtin, chroot, watch, script) and the
// namespace/remote-execution wrappers that were not blocked.
func TestAudit2WrapperBypasses(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"exec rm -rf /", true},
		{"exec /bin/sh", true},
		{"builtin exec rm -rf /", true},
		{"exec -a foo rm -rf /", true},
		{"chroot / rm -rf /", true},
		{"chroot / sh", true},
		{"chroot /", true},
		{"nsenter -t 1 -m rm -rf /", true},
		{"nsenter --target 1 --mount sh", true},
		{"unshare -rm rm -rf /", true},
		{"unshare -r sh", true},
		{"systemd-run --scope rm -rf /", true},
		{"watch rm -rf /", true},
		{"watch -n 1 rm -rf /", true},
		{"script -qec 'rm -rf /' /dev/null", true},
		{"ssh localhost rm -rf /", true},
		{"scp /tmp/x localhost:/etc/passwd", true},
		{"setpriv --reuid=0 rm -rf /", true},
		{"capsh --drop=cap_sys_admin -- -c 'rm -rf /'", true},
		{"watch ls -la", false},
		{"exec echo hi", false},
		{"echo hello", false},
	}
	for _, tc := range cases {
		if got := IsBlockedCommand(tc.cmd); got != tc.want {
			t.Errorf("IsBlockedCommand(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

// TestAudit2EnvSplitString: env -S / --split-string turns its value
// into a command line, so a destructive value must be refused.
func TestAudit2EnvSplitString(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"env -S 'rm -rf /' echo hi", true},
		{"env --split-string='rm -rf /' echo hi", true},
		{"env -S'rm -rf /' echo hi", true},
		{"env --split-string 'rm -rf /' echo hi", true},
		// A value of only flags is not a command: allowed here
		// (the following rm is still checked on its own).
		{"env -S '-u FOO' rm -rf /", true},
		{"env -S '-u FOO' echo hi", false},
		{"env -i echo hi", false},
	}
	for _, tc := range cases {
		if got := IsBlockedCommand(tc.cmd); got != tc.want {
			t.Errorf("IsBlockedCommand(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

// TestAudit2DollarQuoting: $'...' and $"..." are quoting syntax, not
// part of the word, so $'rm' must be recognised as rm.
func TestAudit2DollarQuoting(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"$'rm' -rf /", true},
		{"$\"rm\" -rf /", true},
		{"ec$'ho' hi", false},
		{"\\$'rm' -rf /", false}, // escaped $: a literal word, not rm
	}
	for _, tc := range cases {
		if got := IsBlockedCommand(tc.cmd); got != tc.want {
			t.Errorf("IsBlockedCommand(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

// TestAudit2FileOpTargets: cp/mv/ln/chown/chgrp/mkdir writing to or
// touching a system path are refused; the same operations on ordinary
// paths stay allowed.
func TestAudit2FileOpTargets(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"cp /tmp/x /etc/passwd", true},
		{"cp -r /tmp/dir /etc/", true},
		{"cp -t /etc /tmp/x", true},
		{"cp --target-directory=/etc /tmp/x", true},
		{"mv /tmp/x /etc/hostname", true},
		{"mv /etc/passwd /tmp/x", true},
		{"ln -sf /tmp/x /etc/passwd", true},
		{"ln -s /tmp/x /tmp/y", false},
		{"chown -R nobody /etc", true},
		{"chown root /etc/passwd", true},
		{"chgrp root /etc/passwd", true},
		{"mkdir -p /etc/evil", true},
		{"mkdir -- /etc/evil", true},
		{"cp file1 file2 /tmp/", false},
		{"cp /tmp/a /tmp/b", false},
		{"mkdir -p /tmp/newdir", false},
		{"chown user /tmp/file", false},
	}
	for _, tc := range cases {
		if got := IsBlockedCommand(tc.cmd); got != tc.want {
			t.Errorf("IsBlockedCommand(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

// TestAudit2FindExecWrappers: find -exec with a wrapper around a
// blocked binary (sudo rm, env rm, nice rm, timeout 5 rm) must be
// refused: the recursive check alone allows a plain rm of one file,
// but -exec runs it once per file found.
func TestAudit2FindExecWrappers(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"find / -exec sudo rm {} \\;", true},
		{"find / -exec env rm {} \\;", true},
		{"find / -exec nice rm {} \\;", true},
		{"find / -exec timeout 5 rm {} \\;", true},
		{"find / -exec rm {} \\;", true},
		{"find / -exec echo {} \\;", false},
		{"find /tmp -name x", false},
	}
	for _, tc := range cases {
		if got := IsBlockedCommand(tc.cmd); got != tc.want {
			t.Errorf("IsBlockedCommand(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

// TestAudit2VariableIndirection: a command stored in a variable and
// expanded ($x) must be checked against the assignment.
func TestAudit2VariableIndirection(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"x=rm; $x -rf /", true},
		{"x=/bin/rm; $x -rf /", true},
		{"export x=rm; $x -rf /", true},
		{"x=rm $x -rf /", true},
		{"x=echo; $x hi", false},
		{"$x -rf /", false},       // no assignment: cannot resolve statically
		{"x=$x; $x -rf /", false}, // self-referential: no resolution
	}
	for _, tc := range cases {
		if got := IsBlockedCommand(tc.cmd); got != tc.want {
			t.Errorf("IsBlockedCommand(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

// TestAudit2InterpreterKinds: pypy/micropython/jython are Python,
// bun is Node, deno takes `eval <code>`; chmod/chown of a system
// path inside a one-liner is refused.
func TestAudit2InterpreterKinds(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"pypy3 -c \"import os; os.system('rm -rf /')\"", true},
		{"deno eval \"Deno.removeSync('/etc/passwd')\"", true},
		{"deno eval \"Deno.remove('/etc/passwd')\"", true},
		{"deno eval \"console.log('hi')\"", false},
		{"bun -e \"require('fs').rmSync('/tmp/x')\"", true},
		{"bun -e \"console.log('hi')\"", false},
		{"python3 -c \"import os; os.chmod('/etc/passwd', 0o777)\"", true},
		{"python3 -c \"import os; os.chown('/etc/passwd', 0, 0)\"", true},
		{"python3 -c \"import os; os.chmod('/tmp/x', 0o755)\"", false},
		{"node -e \"require('fs').chmodSync('/etc/passwd', 0o777)\"", true},
	}
	for _, tc := range cases {
		if got := IsBlockedCommand(tc.cmd); got != tc.want {
			t.Errorf("IsBlockedCommand(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

// TestAudit2KillMountAccounts: killing PID 1 or a process group,
// mounting, loop/swap/module administration and account
// administration are refused; killing an ordinary PID and listing
// mounts stay allowed.
func TestAudit2KillMountAccounts(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"kill -9 1", true},
		{"kill -9 -1", true},
		{"kill -TERM -1", true},
		{"kill -s 9 -1", true},
		{"kill 1", true},
		{"killall -9 oriond", true},
		{"pkill -9 orion", true},
		{"kill -9 1234", false},
		{"kill -9 %1", false},
		{"kill -l", false},
		{"mount /dev/sda1 /mnt", true},
		{"mount", false},
		{"umount /", true},
		{"umount -a", true},
		{"losetup /dev/loop0 /tmp/x.img", true},
		{"losetup", false},
		{"swapon /dev/sda2", true},
		{"swapoff -a", true},
		{"modprobe evil", true},
		{"insmod /tmp/evil.ko", true},
		{"rmmod snd", true},
		{"sysctl -w kernel.hostname=evil", true},
		{"mkswap /dev/sda2", true},
		{"dmsetup remove_all", true},
		{"passwd", true},
		{"passwd root", true},
		{"chpasswd", true},
		{"useradd evil", true},
		{"userdel root", true},
		{"usermod -aG sudo evil", true},
		{"groupadd evil", true},
		{"visudo", true},
		{"adduser evil", true},
		{"mount | grep sda", false},
		{"ls /tmp", false},
	}
	for _, tc := range cases {
		if got := IsBlockedCommand(tc.cmd); got != tc.want {
			t.Errorf("IsBlockedCommand(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

// TestAuditAppendHeadFailureResyncsTip: an Append whose head update
// fails must not leave the handle's in-memory sequence behind the
// log file, or the next Append reuses the sequence and corrupts
// the chain.
func TestAuditAppendHeadFailureResyncsTip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	log, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	if _, err := log.Append("agent", "a", "first"); err != nil {
		t.Fatalf("Append 1: %v", err)
	}
	// Break head updates: replace the head file with a directory so
	// the atomic rename onto it fails.
	headPath := path + ".head"
	if err := os.Remove(headPath); err != nil {
		t.Fatalf("remove head: %v", err)
	}
	if err := os.Mkdir(headPath, 0o700); err != nil {
		t.Fatalf("mkdir head: %v", err)
	}
	// Append 2: entry lands in the log, head update fails.
	if _, err := log.Append("agent", "a", "second"); err == nil {
		t.Fatalf("Append 2 with broken head: want error, got nil")
	}
	// Restore the head path so later appends can succeed.
	if err := os.Remove(headPath); err != nil {
		t.Fatalf("remove head dir: %v", err)
	}
	// Append 3: must not reuse seq 2 (duplicate) or break the chain.
	e3, err := log.Append("agent", "a", "third")
	if err != nil {
		t.Fatalf("Append 3 after head failure: %v", err)
	}
	if e3.Seq != 3 {
		t.Errorf("Append 3 seq = %d, want 3 (stale in-memory state reused seq 2)", e3.Seq)
	}
	if err := log.Verify(); err != nil {
		t.Errorf("Verify after head-failure recovery: %v", err)
	}
}
