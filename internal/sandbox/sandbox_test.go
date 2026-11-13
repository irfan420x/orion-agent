package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// runSandboxed runs command inside the sandbox rooted at root and
// returns combined output and exit code. It is the test-only driver
// for Prepare: it builds the /bin/sh -c invocation exactly the way
// shell_exec does, then hands it to the sandbox.
func runSandboxed(t *testing.T, root string, command string, allowNetwork bool) (string, int) {
	t.Helper()
	cfg := Config{Enabled: true, Root: root, AllowNetwork: allowNetwork}
	return runSandboxedCfg(t, cfg, command)
}

// runSandboxedCfg is runSandboxed with an explicit Config.
func runSandboxedCfg(t *testing.T, cfg Config, command string) (string, int) {
	t.Helper()
	return runPrepared(t, prepareSandboxed(t, cfg, command))
}

// prepareSandboxed builds the sandboxed /bin/sh -c invocation and
// returns the prepared cmd, not yet started, so a test can stage
// extra files into the chroot (under <root>/.orion-root, or into
// <root>/work, which is /work inside) before running it.
func prepareSandboxed(t *testing.T, cfg Config, command string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	// The context must outlive the returned cmd's Run; cancel when
	// the test ends rather than when this helper returns.
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Env = os.Environ()
	if err := cfg.Prepare(cmd, command); err != nil {
		t.Fatalf("Prepare(%q): %v", command, err)
	}
	return cmd
}

// runPrepared runs a cmd from prepareSandboxed and returns combined
// output and exit code.
func runPrepared(t *testing.T, cmd *exec.Cmd) (string, int) {
	t.Helper()
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run %v: %v", cmd.Args, err)
		}
	}
	return buf.String(), code
}

func requireSandbox(t *testing.T) {
	t.Helper()
	if !Available() {
		t.Skipf("sandbox unavailable on this host: %v", AvailabilityError())
	}
}

// TestAvailabilityProbe records the host survey result. As root on
// this VM the mechanism MUST work (user namespaces, mount, chroot);
// an unprivileged run may legitimately lack it, so only root asserts.
func TestAvailabilityProbe(t *testing.T) {
	t.Logf("sandbox available=%v err=%v uid=%d", Available(), AvailabilityError(), os.Getuid())
	if os.Getuid() == 0 && !Available() {
		t.Fatalf("sandbox unavailable as root: %v", AvailabilityError())
	}
}

func TestResolveAndValidate(t *testing.T) {
	if got := (Config{}).Resolve("/data").Root; got != filepath.Join("/data", "sandbox") {
		t.Errorf("Resolve(/data).Root = %q, want %q", got, filepath.Join("/data", "sandbox"))
	}
	if got := (Config{Root: "/custom"}).Resolve("/data").Root; got != "/custom" {
		t.Errorf("Resolve keeps explicit root: got %q", got)
	}
	for _, tc := range []struct {
		root    string
		wantErr bool
	}{
		{"", false},
		{"/home/hatch/work", false},
		{"/tmp/sbx", false},
		{"/", true},
		{"relative/path", true},
		{"/etc/evil", true},
		{"/usr/x", true},
		{"/proc/x", true},
		{"/root/x", true},
		{"/home", false}, // /home itself is not a system root in the list
	} {
		err := (Config{Enabled: true, Root: tc.root}).Validate()
		if tc.wantErr && err == nil {
			t.Errorf("Validate(%q): want error, got nil", tc.root)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("Validate(%q): want nil, got %v", tc.root, err)
		}
	}
}

func TestPrepareRejectsBadConfig(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "true")
	if err := (Config{}).Prepare(cmd, "true"); err == nil {
		t.Error("Prepare on disabled config: want error, got nil")
	}
	cmd = exec.Command("/bin/sh", "-c", "true")
	if err := (Config{Enabled: true, Root: "/"}).Prepare(cmd, "true"); err == nil {
		t.Error("Prepare with root /: want error, got nil")
	}
}

// TestPrepareRefusesSymlinkedRoot is the regression test for the
// symlink-validation-order hole: Prepare used to MkdirAll the root
// tree FIRST and only then EvalSymlinks + re-validate, so a root
// that is a symlink into a system tree got directories created
// under the target before validation could stop it. Now the root is
// canonicalized and validated before anything is created: Prepare
// must refuse, and no directories may appear under the target.
// The target is a path under /etc (a real system tree) that must
// not exist before the test; the symlink itself lives in
// t.TempDir() and is harmless.
func TestPrepareRefusesSymlinkedRoot(t *testing.T) {
	target := "/etc/orion-sandbox-test-root"
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Skipf("refusing to test against pre-existing %s (err %v)", target, err)
	}
	t.Cleanup(func() { os.RemoveAll(target) })

	t.Run("root is a symlink into a system tree", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("/bin/sh", "-c", "true")
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		if err := (Config{Enabled: true, Root: link}).Prepare(cmd, "true"); err == nil {
			t.Error("Prepare with a root symlinked into /etc: want error, got nil")
		}
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			t.Errorf("target %s exists after refused Prepare: directories were created under the system tree (err %v)", target, err)
		}
	})

	t.Run("root under a symlinked ancestor into a system tree", func(t *testing.T) {
		sub := "/etc/orion-sandbox-test-sub"
		if _, err := os.Lstat(sub); !os.IsNotExist(err) {
			t.Skipf("refusing to test against pre-existing %s", sub)
		}
		t.Cleanup(func() { os.RemoveAll(sub) })
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink("/etc", link); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("/bin/sh", "-c", "true")
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		if err := (Config{Enabled: true, Root: filepath.Join(link, "orion-sandbox-test-sub")}).Prepare(cmd, "true"); err == nil {
			t.Error("Prepare with a root under a symlink into /etc: want error, got nil")
		}
		if _, err := os.Lstat(sub); !os.IsNotExist(err) {
			t.Errorf("target %s exists after refused Prepare: directories were created under the system tree", sub)
		}
	})
}

// TestPrepareFirstRunRootNotExist proves the normal first-run path
// still works after the canonicalize-first change: a root that does
// not exist yet (only its parent does) must be accepted, and the
// root tree created under it.
func TestPrepareFirstRunRootNotExist(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root") // does not exist yet
	cmd := exec.Command("/bin/sh", "-c", "true")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	if err := (Config{Enabled: true, Root: root}).Prepare(cmd, "true"); err != nil {
		t.Fatalf("Prepare with a not-yet-existing root: %v", err)
	}
	for _, d := range []string{
		filepath.Join(root, "work"),
		filepath.Join(root, ".orion-root", "usr"),
		filepath.Join(root, ".orion-root", "etc"),
	} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Errorf("expected directory %s after Prepare, err %v", d, err)
		}
	}
}

// TestSandboxContainsDestructiveCommand is the core containment
// proof. Everything happens inside t.TempDir(): the sandbox root,
// a canary file OUTSIDE the root, and a canary file inside /work.
// A destructive command runs inside the sandbox (via the sandbox
// layer directly, not through the denylist — that is the point: the
// sandbox must contain what the denylist cannot see). Afterwards
// the outside canary must be untouched, the inside canary must be
// gone, and writes to /etc and to the host path must have failed.
func TestSandboxContainsDestructiveCommand(t *testing.T) {
	requireSandbox(t)
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside.txt")
	if err := os.WriteFile(outside, []byte("OUTSIDE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "keep.txt"), []byte("KEEP\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := fmt.Sprintf(
		"rm -rf /work/*; echo PWNED > /etc/hosts; echo PWNED > %q; mkdir -p /home/hatch/evil; echo hello > /work/hello.txt",
		outside,
	)
	out, _ := runSandboxed(t, root, cmd, false)
	if !strings.Contains(out, "Read-only file system") {
		t.Errorf("expected /etc write to fail read-only, output:\n%s", out)
	}
	if !strings.Contains(out, "Directory nonexistent") {
		t.Errorf("expected write to host path to fail (path absent in chroot), output:\n%s", out)
	}

	if got, err := os.ReadFile(outside); err != nil || string(got) != "OUTSIDE\n" {
		t.Errorf("outside canary: got %q, err %v; want untouched OUTSIDE", got, err)
	}
	if _, err := os.Stat(filepath.Join(work, "keep.txt")); !os.IsNotExist(err) {
		t.Errorf("work canary keep.txt survived the sandboxed rm -rf; want it destroyed (proves the command really ran)")
	}
	if got, err := os.ReadFile(filepath.Join(work, "hello.txt")); err != nil || strings.TrimSpace(string(got)) != "hello" {
		t.Errorf("hello.txt in work: got %q, err %v; want hello (proves legit writes still work)", got, err)
	}
	if _, err := os.Stat("/home/hatch/evil"); !os.IsNotExist(err) {
		t.Errorf("host path /home/hatch/evil exists after sandboxed mkdir; want it never created")
	}
	// The /etc copy inside the sandbox must not have been overwritten.
	if got, err := os.ReadFile(filepath.Join(root, ".orion-root", "etc", "hosts")); err != nil || strings.Contains(string(got), "PWNED") {
		t.Errorf("sandbox /etc/hosts copy corrupted: %q, err %v", got, err)
	}
}

// TestSandboxLegitimateCommands proves ordinary work still works:
// files, cwd, interpreters, and a scrubbed-ish environment (HOME
// and TMPDIR point inside).
func TestSandboxLegitimateCommands(t *testing.T) {
	requireSandbox(t)
	root := filepath.Join(t.TempDir(), "root")
	out, code := runSandboxed(t, root, "pwd; echo hello > hello.txt; cat hello.txt; python3 -c 'print(40+2)'; echo HOME=$HOME; ls", false)
	if code != 0 {
		t.Fatalf("legit command exit %d, output:\n%s", code, out)
	}
	for _, want := range []string{"/work", "hello", "42", "HOME=/work", "hello.txt"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if got, err := os.ReadFile(filepath.Join(root, "work", "hello.txt")); err != nil || strings.TrimSpace(string(got)) != "hello" {
		t.Errorf("hello.txt on host: got %q, err %v", got, err)
	}
}

// TestSandboxNoNetwork proves the default network namespace has no
// interface but loopback, while AllowNetwork sees the host's
// interfaces. if_nameindex needs no /proc, which the sandbox does
// not mount.
func TestSandboxNoNetwork(t *testing.T) {
	requireSandbox(t)
	root := filepath.Join(t.TempDir(), "root")
	out, code := runSandboxed(t, root, "python3 -c \"import socket; print(socket.if_nameindex())\"", false)
	if code != 0 {
		t.Fatalf("if_nameindex exit %d, output:\n%s", code, out)
	}
	if strings.TrimSpace(out) != "[(1, 'lo')]" {
		t.Errorf("no-network interfaces = %q, want exactly [(1, 'lo')]", strings.TrimSpace(out))
	}

	// Only assert the allowed-network case differs when the host
	// actually has more than loopback; on a lo-only host the two
	// views are identical and there is nothing to distinguish.
	if ifs, err := net.Interfaces(); err != nil || len(ifs) <= 1 {
		t.Skip("host has only loopback; allowed-network view is identical by construction")
	}
	out, code = runSandboxed(t, root, "python3 -c \"import socket; print(socket.if_nameindex())\"", true)
	if code != 0 {
		t.Fatalf("if_nameindex (network allowed) exit %d, output:\n%s", code, out)
	}
	if strings.TrimSpace(out) == "[(1, 'lo')]" {
		t.Errorf("network-allowed interfaces = %q, want the host interfaces", strings.TrimSpace(out))
	}
}

// TestSandboxConcurrentRuns shares one root between parallel runs:
// the symlink/mkdir setup in Prepare must be race-safe, and each
// run's mounts must be independent.
func TestSandboxConcurrentRuns(t *testing.T) {
	requireSandbox(t)
	root := filepath.Join(t.TempDir(), "root")
	var wg sync.WaitGroup
	errs := make(chan string, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, code := runSandboxed(t, root, fmt.Sprintf("echo v%d > f%d.txt; cat f%d.txt", i, i, i), false)
			if code != 0 || !strings.Contains(out, fmt.Sprintf("v%d", i)) {
				errs <- fmt.Sprintf("run %d: code %d, output %q", i, code, out)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestSandboxChrootEscapeBlocked is the regression test for the
// chroot-escape hole (audit 2, P0). A plain chroot is escapable: a
// process with CAP_SYS_CHROOT can chroot to a subdirectory while
// its cwd is outside it, walk up past the old root, and chroot
// again onto the host filesystem. The sandbox pivot_roots into the
// new root and lazily unmounts the old root, so the walk-up stops
// at the new root and the host file stays unreachable. The escape
// below is the full classic sequence (it read the canary before
// the pivot fix). Everything happens inside t.TempDir().
func TestSandboxChrootEscapeBlocked(t *testing.T) {
	requireSandbox(t)
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside.txt")
	if err := os.WriteFile(outside, []byte("OUTSIDE-SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Double chroot: chroot to /work/sub while cwd is /work (outside
	// the new root), walk up as far as possible, chroot again to
	// wherever that lands, then read the host canary by absolute
	// path. Before the pivot fix this printed OUTSIDE-SECRET.
	esc := fmt.Sprintf(`python3 -c "import os; os.mkdir('/work/sub'); os.chroot('/work/sub'); [os.chdir('..') for _ in range(12)]; os.chroot('.'); os.chdir('/'); print('CANARY:'+open('%s').read())"`, outside)
	out, _ := runSandboxed(t, root, esc, false)
	if strings.Contains(out, "OUTSIDE-SECRET") {
		t.Errorf("chroot escape read the outside canary:\n%s", out)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "OUTSIDE-SECRET\n" {
		t.Errorf("outside canary: got %q, err %v; want untouched", got, err)
	}
}

// TestSandboxNoDevTTY proves /dev/tty is not present inside: it is
// the process's controlling terminal, and a sandboxed command with
// access to it could inject input into it (TIOCSTI) when the agent
// runs attached to a tty.
func TestSandboxNoDevTTY(t *testing.T) {
	requireSandbox(t)
	root := filepath.Join(t.TempDir(), "root")
	out, _ := runSandboxed(t, root, "ls /dev/tty 2>&1; test -e /dev/null && echo NULL-OK; test -e /dev/urandom && echo URANDOM-OK", false)
	if !strings.Contains(out, "No such file or directory") {
		t.Errorf("/dev/tty exists inside the sandbox, output:\n%s", out)
	}
	for _, want := range []string{"NULL-OK", "URANDOM-OK"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q (harmless device nodes must still work):\n%s", want, out)
		}
	}
}

// procEntries returns the lines of a sandboxed `ls /proc` that are
// pure numbers (i.e. PIDs) plus the non-numeric names, so tests can
// assert about the visible process set without parsing `ps` output.
func procEntries(t *testing.T, out string) (pids map[string]bool, names map[string]bool) {
	t.Helper()
	pids = map[string]bool{}
	names = map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if _, err := strconv.Atoi(line); err == nil {
			pids[line] = true
		} else {
			names[line] = true
		}
	}
	return pids, names
}

// TestPrepareRequestsPIDNamespace is the unit-level half of the
// fail-closed rule: Prepare must ask the clone for a PID namespace,
// and must NOT ask for a user namespace or set uid/gid mappings —
// the user namespace is created by the wrapper after the /proc
// mount (see sandbox.go), so a clone that silently lacked NEW PID
// would be a bug, not a degraded mode.
func TestPrepareRequestsPIDNamespace(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "true")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cfg := Config{Enabled: true, Root: filepath.Join(t.TempDir(), "root")}
	if err := cfg.Prepare(cmd, "true"); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	attr := cmd.SysProcAttr
	if attr == nil {
		t.Fatal("Prepare did not set SysProcAttr")
	}
	if attr.Cloneflags&syscall.CLONE_NEWPID == 0 {
		t.Error("Cloneflags lack CLONE_NEWPID: the sandbox would run without a PID namespace")
	}
	if attr.Cloneflags&syscall.CLONE_NEWUSER != 0 {
		t.Error("Cloneflags still request CLONE_NEWUSER: the user namespace is the wrapper's job (after the /proc mount), not the clone's")
	}
	if len(attr.UidMappings) != 0 || len(attr.GidMappings) != 0 {
		t.Error("Prepare sets uid/gid mappings; the wrapper maps root via unshare --map-root-user")
	}
}

// TestSandboxPIDNamespaceIsolatesHost is the live proof for the PID
// namespace. A host canary process (sleep, known PID) runs outside
// the sandbox; inside, that PID must not exist: it is absent from
// /proc and kill -0 on it fails. The sandboxed shell must also see
// itself as PID 1 — it is the namespace init.
func TestSandboxPIDNamespaceIsolatesHost(t *testing.T) {
	requireSandbox(t)
	canary := exec.Command("sleep", "60")
	if err := canary.Start(); err != nil {
		t.Fatalf("start canary sleep: %v", err)
	}
	t.Cleanup(func() { canary.Process.Kill(); canary.Wait() })
	pid := canary.Process.Pid
	// A namespace only ever holds a handful of small PIDs; a host
	// PID this small could collide with a real namespace PID and
	// make the assertions meaningless. (Host PIDs here are in the
	// thousands; this is belt-and-braces, not expected to trigger.)
	if pid < 100 {
		t.Skipf("canary pid %d too small to distinguish from namespace PIDs", pid)
	}
	root := filepath.Join(t.TempDir(), "root")
	out, code := runSandboxed(t, root, fmt.Sprintf("echo SELF=$$; ls /proc; kill -0 %d 2>&1; echo KILL_EXIT=$?", pid), false)
	if code != 0 {
		t.Fatalf("probe exit %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "SELF=1") {
		t.Errorf("sandboxed shell is not PID 1, output:\n%s", out)
	}
	if strings.Contains(out, "KILL_EXIT=0") {
		t.Errorf("kill -0 on host canary pid %d succeeded inside the sandbox; host processes must be unreachable, output:\n%s", pid, out)
	}
	pids, _ := procEntries(t, out)
	if pids[strconv.Itoa(pid)] {
		t.Errorf("host canary pid %d is visible in the sandbox's /proc, output:\n%s", pid, out)
	}
	if !pids["1"] {
		t.Errorf("sandbox /proc does not list PID 1, output:\n%s", out)
	}
}

// TestSandboxProcReflectsNamespace proves /proc inside is a real
// procfs for the new PID namespace, not the host's and not an empty
// directory: it lists PID 1 and self, /proc/1/comm names the shell,
// /proc/self/status is readable, and /proc/sys cannot be written
// (subset=pid hides it entirely; the plain-mount fallback binds it
// read-only — either way the write must fail).
func TestSandboxProcReflectsNamespace(t *testing.T) {
	requireSandbox(t)
	root := filepath.Join(t.TempDir(), "root")
	out, code := runSandboxed(t, root, "ls /proc; cat /proc/1/comm; test -r /proc/self/status && echo SELF-STATUS-OK; echo x > /proc/sys/kernel/hostname 2>&1; echo SYS-WRITE-EXIT=$?", false)
	if code != 0 {
		t.Fatalf("probe exit %d, output:\n%s", code, out)
	}
	pids, names := procEntries(t, out)
	if !pids["1"] {
		t.Errorf("/proc does not list PID 1, output:\n%s", out)
	}
	if !names["self"] {
		t.Errorf("/proc does not list self, output:\n%s", out)
	}
	lines := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		lines[strings.TrimSpace(line)] = true
	}
	if !lines["sh"] {
		t.Errorf("/proc/1/comm is not the shell, output:\n%s", out)
	}
	if !strings.Contains(out, "SELF-STATUS-OK") {
		t.Errorf("/proc/self/status is not readable, output:\n%s", out)
	}
	if strings.Contains(out, "SYS-WRITE-EXIT=0") {
		t.Errorf("writing /proc/sys/kernel/hostname succeeded inside the sandbox; /proc/sys must not be writable, output:\n%s", out)
	}
}

// TestSandboxPID1ReapsOrphans proves the namespace init reaps its
// children. The user shell is PID 1; a grandchild orphaned onto it
// (its parent, a subshell, exits immediately) must not linger as a
// zombie — after it exits and the shell has waited, its /proc entry
// must be gone. The check retries briefly so a slow reap cannot
// flake the test.
func TestSandboxPID1ReapsOrphans(t *testing.T) {
	requireSandbox(t)
	root := filepath.Join(t.TempDir(), "root")
	cmd := `(sleep 0.2 & echo $! > /work/orphan.pid)
sleep 1
P=$(cat /work/orphan.pid)
echo "ORPHAN=$P"
for i in 1 2 3 4 5; do [ -e /proc/$P/stat ] || break; sleep 0.2; done
if [ -e /proc/$P/stat ]; then echo "ZOMBIE-LEAK: $(cat /proc/$P/stat)"; else echo REAPED-OK; fi`
	out, code := runSandboxed(t, root, cmd, false)
	if code != 0 {
		t.Fatalf("probe exit %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "REAPED-OK") {
		t.Errorf("orphan was not reaped by PID 1, output:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "ORPHAN=") {
			if n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(line), "ORPHAN=")); err != nil || n < 1 || n > 1000 {
				t.Errorf("orphan pid %q is not a small namespace PID, output:\n%s", line, out)
			}
		}
	}
}

var (
	nestedProcProbeOnce sync.Once
	nestedProcBlocked   bool
	nestedProcProbed    bool
)

// procMountBlockedInNestedUserNS probes the host fact the fail-closed
// test depends on: whether a procfs mount is refused from inside a
// nested user namespace. On this host it is (EPERM — the kernel only
// allows the mount from the cell's user namespace). If a future host
// allows it, the wrapper-failure test below is skipped instead of
// asserting a failure that cannot happen there.
func procMountBlockedInNestedUserNS(t *testing.T) bool {
	t.Helper()
	nestedProcProbeOnce.Do(func() {
		dir := t.TempDir()
		cmd := exec.Command("unshare", "-Urm", "/bin/sh", "-c", "mount -t proc proc "+dir+" 2>/dev/null")
		// The mount happens in the child's fresh mount namespace
		// (unshare -m), so a success could not leak to the host.
		if err := cmd.Run(); err != nil {
			nestedProcBlocked = true
		}
		nestedProcProbed = true
	})
	if !nestedProcProbed {
		t.Fatal("nested proc-mount probe did not run")
	}
	return nestedProcBlocked
}

// TestSandboxWrapperRefusesWithoutProc is the fail-closed proof for
// the /proc mount: if the wrapper cannot mount procfs, it must exit
// 126 before running any user code. The wrapper is run directly
// inside a nested user namespace (via unshare -Urm, no PID
// namespace), where this host refuses the mount — the marker command
// must never run, so the marker file must not exist. The root tree
// is built by Prepare first, exactly as for a real run.
func TestSandboxWrapperRefusesWithoutProc(t *testing.T) {
	if !procMountBlockedInNestedUserNS(t) {
		t.Skip("this host allows procfs mounts inside a nested user namespace, so the wrapper-failure path is not exercisable here")
	}
	root := filepath.Join(t.TempDir(), "root")
	cfg := Config{Enabled: true, Root: root}
	dummy := exec.Command("/bin/sh", "-c", "true")
	dummy.Env = os.Environ()
	if err := cfg.Prepare(dummy, "true"); err != nil {
		t.Fatalf("Prepare (build root tree): %v", err)
	}
	marker := filepath.Join(root, "work", "marker.txt")
	wcmd := exec.Command("unshare", "-Urm", "/bin/sh", "-c", wrapperScript, "orion-sandbox", root, "echo MARKER > /work/marker.txt")
	var buf bytes.Buffer
	wcmd.Stdout = &buf
	wcmd.Stderr = &buf
	err := wcmd.Run()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run wrapper directly: %v", err)
		}
	}
	if code != 126 {
		t.Errorf("wrapper without a working /proc mount: exit %d, want 126 (setup failure, before user code), output:\n%s", code, buf.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("marker file %s exists: user code ran despite the failed /proc mount", marker)
	}
}
