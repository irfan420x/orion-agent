package sandbox

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// probeSkipped are blocked syscalls the python probe
// (testdata/seccomp_probe.py) deliberately does not exercise live:
//
//   - reboot would reboot the host if the filter failed to apply.
//   - sethostname/setdomainname would rename the host: the sandbox
//     shares the host UTS namespace (no CLONE_NEWUTS).
//   - iopl/ioperm, fanotify_init and acct fail with EPERM even
//     unfiltered: they need a capability in the initial user
//     namespace, which the sandboxed process (root only in its own
//     namespace) does not have, so the probe cannot tell blocked
//     from unblocked for them.
//   - quotactl and vhangup have no safe, unambiguous probe.
//
// They are covered by TestSeccompFilterProgram, which verifies every
// table entry has a JEQ landing on DENY.
var probeSkipped = map[string]bool{
	"reboot": true, "sethostname": true, "setdomainname": true,
	"iopl": true, "ioperm": true, "fanotify_init": true,
	"acct": true, "quotactl": true, "vhangup": true,
}

// expectedProbeNames is the set of syscall names the python probe
// must report: every table entry except the skipped ones.
func expectedProbeNames(t *testing.T) map[string]bool {
	t.Helper()
	table, ok := seccompTableFor(runtime.GOARCH)
	if !ok {
		t.Skipf("no seccomp table for %s", runtime.GOARCH)
	}
	names := map[string]bool{}
	for _, b := range table.blocked {
		names[b.name] = true
	}
	for _, b := range table.blockedENOSYS {
		names[b.name] = true
	}
	for name := range probeSkipped {
		delete(names, name)
	}
	return names
}

// testdataPath returns a file under this package's testdata dir.
func testdataPath(t *testing.T, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "testdata", name)
}

// copyFileMode copies src to dst with the given permissions.
func copyFileMode(t *testing.T, src, dst string, mode os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, mode); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

var (
	buildLauncherOnce sync.Once
	builtLauncherPath string
	builtLauncherErr  error
)

// testLauncher builds the orion-seccomp helper once per test run
// (CGO_ENABLED=0, so the binary is static and runs inside the
// chroot) and returns its path. The binary is left in a temp dir for
// the OS to reap: it must outlive any single test, so it is not tied
// to t.TempDir.
func testLauncher(t *testing.T) string {
	t.Helper()
	buildLauncherOnce.Do(func() {
		goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
		if _, err := os.Stat(goBin); err != nil {
			builtLauncherErr = fmt.Errorf("go toolchain not found at %s: %v", goBin, err)
			return
		}
		_, file, _, _ := runtime.Caller(0)
		root := filepath.Dir(file)
		for {
			if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
				break
			}
			parent := filepath.Dir(root)
			if parent == root {
				builtLauncherErr = fmt.Errorf("could not find repo root (go.mod) above %s", file)
				return
			}
			root = parent
		}
		dir, err := os.MkdirTemp("", "orion-seccomp-test-*")
		if err != nil {
			builtLauncherErr = err
			return
		}
		out := filepath.Join(dir, "orion-seccomp")
		cmd := exec.Command(goBin, "build", "-trimpath", "-o", out, "./internal/sandbox/cmd/launcher")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		if err := cmd.Run(); err != nil {
			builtLauncherErr = fmt.Errorf("build orion-seccomp: %v\n%s", err, buf.String())
			return
		}
		builtLauncherPath = out
	})
	if builtLauncherErr != nil {
		t.Skipf("orion-seccomp launcher unavailable: %v", builtLauncherErr)
	}
	return builtLauncherPath
}

// buildCloneProbe compiles testdata/cloneprobe.c with gcc. It skips
// the test when gcc is unavailable.
func buildCloneProbe(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("gcc"); err != nil {
		t.Skip("gcc not available; cannot build clone probe")
	}
	out := filepath.Join(t.TempDir(), "cloneprobe")
	cmd := exec.Command("gcc", "-O2", "-Wall", "-o", out, testdataPath(t, "cloneprobe.c"))
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		t.Fatalf("build cloneprobe: %v\n%s", err, buf.String())
	}
	return out
}

var probeLineRe = regexp.MustCompile(`^([a-z0-9_]+) (BLOCKED|UNBLOCKED.*)$`)

// parseProbeOutput parses the python probe's output into
// name -> blocked. It asserts the output covers exactly the expected
// probe names (no missing, no extra, no duplicates) and fails the
// test on any python traceback.
func parseProbeOutput(t *testing.T, out string) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.Contains(line, "Traceback") || strings.Contains(line, "Error") {
			t.Fatalf("probe script failed, output:\n%s", out)
		}
		m := probeLineRe.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("probe output has unexpected line %q (full output:\n%s)", line, out)
		}
		if _, dup := got[m[1]]; dup {
			t.Fatalf("probe reported %q twice", m[1])
		}
		got[m[1]] = m[2] == "BLOCKED"
	}
	want := expectedProbeNames(t)
	if len(got) != len(want) {
		for name := range want {
			if _, ok := got[name]; !ok {
				t.Errorf("probe did not report %q", name)
			}
		}
		for name := range got {
			if !want[name] {
				t.Errorf("probe reported unexpected %q", name)
			}
		}
		t.Fatalf("probe coverage mismatch: got %d names, want %d", len(got), len(want))
	}
	return got
}

// runProbeOnHost runs the probe script directly on the host (no
// sandbox, no filter) and returns name -> blocked. This calibrates
// the control: the host itself runs under a seccomp filter (the cell
// filter) that already blocks some syscalls (swapon, kexec, bpf,
// userfaultfd, setns, settimeofday, syslog on this VM), so those
// read BLOCKED here and cannot discriminate the sandbox filter;
// the rest must read UNBLOCKED here and BLOCKED under the filter.
func runProbeOnHost(t *testing.T) map[string]bool {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	out, err := exec.Command(python, testdataPath(t, "seccomp_probe.py")).CombinedOutput()
	if err != nil {
		t.Fatalf("host probe run: %v\n%s", err, out)
	}
	return parseProbeOutput(t, string(out))
}

// runProbeSandboxed runs the probe script inside the sandbox with
// cfg and returns name -> blocked. The script is staged into
// <root>/work (visible as /work inside).
func runProbeSandboxed(t *testing.T, cfg Config) map[string]bool {
	t.Helper()
	cmd := prepareSandboxed(t, cfg, "python3 /work/seccomp_probe.py")
	copyFileMode(t, testdataPath(t, "seccomp_probe.py"), filepath.Join(cfg.Root, "work", "seccomp_probe.py"), 0o644)
	out, code := runPrepared(t, cmd)
	if code != 0 {
		t.Fatalf("sandboxed probe exit %d, output:\n%s", code, out)
	}
	return parseProbeOutput(t, out)
}

// TestSeccompTableMatchesStdlib cross-checks the amd64 table against
// Go's syscall package, where the constants exist. A typo in the
// table would silently un-block a syscall (or block an innocent
// one); the stdlib numbers are an independent source.
func TestSeccompTableMatchesStdlib(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skipf("stdlib cross-check is amd64-only (test binary is %s)", runtime.GOARCH)
	}
	stdlib := map[string]uint32{
		"mount": syscall.SYS_MOUNT, "umount2": syscall.SYS_UMOUNT2,
		"pivot_root": syscall.SYS_PIVOT_ROOT, "chroot": syscall.SYS_CHROOT,
		"reboot": syscall.SYS_REBOOT, "swapon": syscall.SYS_SWAPON, "swapoff": syscall.SYS_SWAPOFF,
		"keyctl": syscall.SYS_KEYCTL, "add_key": syscall.SYS_ADD_KEY, "request_key": syscall.SYS_REQUEST_KEY,
		"ptrace": syscall.SYS_PTRACE, "kexec_load": syscall.SYS_KEXEC_LOAD,
		"init_module": syscall.SYS_INIT_MODULE, "delete_module": syscall.SYS_DELETE_MODULE,
		"perf_event_open": syscall.SYS_PERF_EVENT_OPEN,
		"unshare":         syscall.SYS_UNSHARE, "clone": syscall.SYS_CLONE,
		"sethostname": syscall.SYS_SETHOSTNAME, "setdomainname": syscall.SYS_SETDOMAINNAME,
		"clock_settime": syscall.SYS_CLOCK_SETTIME, "settimeofday": syscall.SYS_SETTIMEOFDAY,
		"adjtimex": syscall.SYS_ADJTIMEX, "syslog": syscall.SYS_SYSLOG,
		"iopl": syscall.SYS_IOPL, "ioperm": syscall.SYS_IOPERM,
		"fanotify_init": syscall.SYS_FANOTIFY_INIT, "acct": syscall.SYS_ACCT,
		"quotactl": syscall.SYS_QUOTACTL, "vhangup": syscall.SYS_VHANGUP,
	}
	table, ok := seccompTableFor("amd64")
	if !ok {
		t.Fatal("no amd64 seccomp table")
	}
	for _, b := range append(append([]blockedSyscall{}, table.blocked...), table.blockedENOSYS...) {
		want, ok := stdlib[b.name]
		if !ok {
			continue // not in Go's syscall package; covered by the header test
		}
		if b.nr != want {
			t.Errorf("table %q = %d, stdlib SYS_%s = %d", b.name, b.nr, strings.ToUpper(b.name), want)
		}
	}
}

// TestSeccompTableMatchesKernelHeaders cross-checks every table
// entry against the kernel headers on this machine
// (asm/unistd_64.h for amd64, asm-generic/unistd.h for arm64),
// which are the authoritative source for the numbers Go's syscall
// package does not define (fsopen family, clone3, bpf, seccomp,
// ...). Skipped when the headers are absent.
func TestSeccompTableMatchesKernelHeaders(t *testing.T) {
	headers := map[string]string{
		"amd64": "/usr/include/x86_64-linux-gnu/asm/unistd_64.h",
		"arm64": "/usr/include/asm-generic/unistd.h",
	}
	checked := 0
	for _, table := range seccompTables {
		path, ok := headers[table.goarch]
		if !ok {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Logf("kernel header %s absent (%v); skipping %s check", path, err, table.goarch)
			continue
		}
		nums := map[string]uint32{}
		for _, line := range strings.Split(string(data), "\n") {
			var name string
			var num uint32
			if n, _ := fmt.Sscanf(line, "#define __NR_%s %d", &name, &num); n == 2 {
				nums[name] = num
			}
		}
		for _, b := range append(append([]blockedSyscall{}, table.blocked...), table.blockedENOSYS...) {
			want, ok := nums[b.name]
			if !ok {
				t.Errorf("table %s/%q has no __NR_%s in %s", table.goarch, b.name, b.name, path)
				continue
			}
			checked++
			if b.nr != want {
				t.Errorf("table %s/%q = %d, header %s = %d", table.goarch, b.name, b.nr, path, want)
			}
		}
		// The table's own syscall numbers (seccomp, clone, clone3).
		for name, nr := range map[string]uint32{
			"seccomp": table.seccompNr, "clone": table.cloneNr, "clone3": table.clone3Nr,
		} {
			want, ok := nums[name]
			if !ok {
				t.Errorf("table %s: no __NR_%s in %s", table.goarch, name, path)
				continue
			}
			checked++
			if nr != want {
				t.Errorf("table %s %s = %d, header = %d", table.goarch, name, nr, want)
			}
		}
	}
	if checked == 0 {
		t.Skip("no kernel headers available to check against")
	}
	t.Logf("checked %d syscall numbers against kernel headers", checked)
}

// TestSeccompTableForUnknownArch proves an unverified architecture
// fails closed: no table, so InstallSeccompFilter errors and the
// launcher refuses to run the command unfiltered.
func TestSeccompTableForUnknownArch(t *testing.T) {
	for _, arch := range []string{"", "riscv64", "wasm", "386"} {
		if _, ok := seccompTableFor(arch); ok {
			t.Errorf("seccompTableFor(%q): got a table, want none", arch)
		}
	}
}

// TestSeccompFilterProgram decodes the built BPF program and
// verifies its structure: arch check first, one JEQ per blocked
// syscall landing on the right RET, the clone flag check, and no
// jump out of bounds. This is the structural proof for the syscalls
// the live probe deliberately skips (reboot, sethostname, ...).
func TestSeccompFilterProgram(t *testing.T) {
	table, ok := seccompTableFor(runtime.GOARCH)
	if !ok {
		t.Skipf("no seccomp table for %s", runtime.GOARCH)
	}
	prog := buildSeccompFilter(table)
	if len(prog) == 0 || len(prog) > 4096 {
		t.Fatalf("filter program length %d, want 1..4096", len(prog))
	}
	target := func(idx int, off uint8) int { return idx + 1 + int(off) }
	retAt := func(idx int) (uint32, bool) {
		if idx < 0 || idx >= len(prog) || prog[idx].Code != bpfRetK {
			return 0, false
		}
		return prog[idx].K, true
	}
	// prog[0] loads arch, prog[1] checks it; a mismatch jumps to
	// RET ERRNO(ENOSYS).
	if prog[0].Code != bpfLdAbs || prog[0].K != 4 {
		t.Errorf("prog[0] = %+v, want LD ABS 4 (arch)", prog[0])
	}
	if prog[1].Code != bpfJmpEq || prog[1].K != table.auditArch {
		t.Errorf("prog[1] = %+v, want JEQ auditArch %#x", prog[1], table.auditArch)
	}
	if ret, ok := retAt(target(1, prog[1].Jf)); !ok || ret != seccompRetErrno|uint32(syscall.ENOSYS) {
		t.Errorf("arch-mismatch jump lands on RET %#x (ok=%v), want RET ERRNO(ENOSYS)", ret, ok)
	}
	if prog[1].Jt != 0 {
		t.Errorf("arch check jt = %d, want 0 (fall through to the nr load)", prog[1].Jt)
	}
	// prog[2] loads the syscall number.
	if prog[2].Code != bpfLdAbs || prog[2].K != 0 {
		t.Errorf("prog[2] = %+v, want LD ABS 0 (nr)", prog[2])
	}
	blocked := map[uint32]string{}
	for _, b := range table.blocked {
		if _, dup := blocked[b.nr]; dup {
			t.Errorf("duplicate blocked nr %d (%s)", b.nr, b.name)
		}
		blocked[b.nr] = b.name
	}
	enosys := map[uint32]string{}
	for _, b := range table.blockedENOSYS {
		enosys[b.nr] = b.name
	}
	seenBlocked := map[uint32]bool{}
	seenENOSYS := map[uint32]bool{}
	allowIdx := -1
	jsetIdx := -1
	for i, ins := range prog {
		switch ins.Code {
		case bpfJmpEq:
			// The arch check (i == 1) is handled above.
			if i == 1 {
				continue
			}
			if name, ok := blocked[ins.K]; ok {
				seenBlocked[ins.K] = true
				ret, ok := retAt(target(i, ins.Jt))
				if !ok || ret != seccompRetErrno|uint32(syscall.EPERM) {
					t.Errorf("JEQ %s (%d) jumps to RET %#x (ok=%v), want RET ERRNO(EPERM)", name, ins.K, ret, ok)
				}
				if ins.Jf != 0 {
					t.Errorf("JEQ %s (%d) jf = %d, want 0 (fall through)", name, ins.K, ins.Jf)
				}
			} else if name, ok := enosys[ins.K]; ok {
				seenENOSYS[ins.K] = true
				ret, ok := retAt(target(i, ins.Jt))
				if !ok || ret != seccompRetErrno|uint32(syscall.ENOSYS) {
					t.Errorf("JEQ %s (%d) jumps to RET %#x (ok=%v), want RET ERRNO(ENOSYS)", name, ins.K, ret, ok)
				}
			} else if ins.K == table.cloneNr {
				// clone: fall through to the flag check when equal,
				// jump to ALLOW when not.
				if ins.Jt != 0 {
					t.Errorf("JEQ clone jt = %d, want 0 (fall through to flag check)", ins.Jt)
				}
				ret, ok := retAt(target(i, ins.Jf))
				if !ok || ret != seccompRetAllow {
					t.Errorf("JEQ clone jf lands on RET %#x (ok=%v), want RET ALLOW", ret, ok)
				}
				allowIdx = target(i, ins.Jf)
			} else {
				t.Errorf("unexpected JEQ nr %d at index %d", ins.K, i)
			}
		case bpfJmpSet:
			jsetIdx = i
			if ins.K != cloneNewNSMask {
				t.Errorf("JSET mask = %#x, want %#x", ins.K, cloneNewNSMask)
			}
			ret, ok := retAt(target(i, ins.Jt))
			if !ok || ret != seccompRetErrno|uint32(syscall.EPERM) {
				t.Errorf("JSET jumps to RET %#x (ok=%v), want RET ERRNO(EPERM)", ret, ok)
			}
			if ins.Jf != 0 {
				t.Errorf("JSET jf = %d, want 0 (fall through to ALLOW)", ins.Jf)
			}
		case bpfLdAbs:
			// Only three loads: arch (4), nr (0), clone flags (16).
			if ins.K != 4 && ins.K != 0 && ins.K != 16 {
				t.Errorf("unexpected LD ABS %d at index %d", ins.K, i)
			}
		case bpfRetK:
			// Terminals; checked via the jumps above.
		default:
			t.Errorf("unexpected BPF opcode %#x at index %d", ins.Code, i)
		}
		// Every jump must land inside the program.
		if ins.Code == bpfJmpEq || ins.Code == bpfJmpSet {
			for _, off := range []uint8{ins.Jt, ins.Jf} {
				if off != 0 && target(i, off) >= len(prog) {
					t.Errorf("jump at index %d with offset %d lands outside the program", i, off)
				}
			}
		}
	}
	for nr, name := range blocked {
		if !seenBlocked[nr] {
			t.Errorf("blocked syscall %s (%d) has no JEQ in the program", name, nr)
		}
	}
	for nr, name := range enosys {
		if !seenENOSYS[nr] {
			t.Errorf("ENOSYS syscall %s (%d) has no JEQ in the program", name, nr)
		}
	}
	if jsetIdx < 0 {
		t.Fatal("no JSET (clone flag check) in the program")
	}
	// The instruction right after the flag check must be ALLOW:
	// a clone with no namespace bits falls through to it.
	if ret, ok := retAt(jsetIdx + 1); !ok || ret != seccompRetAllow {
		t.Errorf("instruction after JSET is RET %#x (ok=%v), want RET ALLOW", ret, ok)
	}
	if allowIdx >= 0 && allowIdx != jsetIdx+1 {
		t.Errorf("clone JEQ jumps to ALLOW at %d, but ALLOW is at %d", allowIdx, jsetIdx+1)
	}
	// Exactly one ALLOW in the whole program.
	allows := 0
	for _, ins := range prog {
		if ins.Code == bpfRetK && ins.K == seccompRetAllow {
			allows++
		}
	}
	if allows != 1 {
		t.Errorf("program has %d RET ALLOW, want exactly 1", allows)
	}
	t.Logf("filter program: %d instructions, %d EPERM + %d ENOSYS blocked, clone flag-checked",
		len(prog), len(table.blocked), len(table.blockedENOSYS))
}

// TestSeccompLauncherBlocksSyscalls is the live proof: the probe
// script runs inside the sandbox through the launcher, and every
// probed syscall must be denied; ordinary commands (including
// process spawning through both clone and posix_spawn/clone3) must
// still work.
func TestSeccompLauncherBlocksSyscalls(t *testing.T) {
	requireSandbox(t)
	launcher := testLauncher(t)
	root := filepath.Join(t.TempDir(), "root")
	cfg := Config{Enabled: true, Root: root, SeccompLauncher: launcher}

	// The launcher must be staged into the chroot by Prepare.
	cmd := prepareSandboxed(t, cfg, "true")
	staged := filepath.Join(root, ".orion-root", "orion-seccomp")
	if fi, err := os.Stat(staged); err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("launcher not staged at %s: %v (mode %v)", staged, err, fi.Mode())
	}
	if out, code := runPrepared(t, cmd); code != 0 {
		t.Fatalf("staging check run exit %d: %s", code, out)
	}

	// Host calibration first: which probes the ambient (cell) filter
	// already blocks. The sandbox filter must block all of them;
	// the discriminating proof is in the control test below.
	host := runProbeOnHost(t)
	blocked := runProbeSandboxed(t, cfg)
	for name, isBlocked := range blocked {
		if !isBlocked {
			t.Errorf("syscall %s: UNBLOCKED inside the sandbox (host calibration: blocked=%v)", name, host[name])
		}
	}

	// Legitimate commands still work under the filter: shell, files,
	// python, subprocess (clone) and posix_spawn (clone3 -> ENOSYS
	// fallback in glibc).
	root2 := filepath.Join(t.TempDir(), "root")
	cfg2 := Config{Enabled: true, Root: root2, SeccompLauncher: launcher}
	out, code := runSandboxedCfg(t, cfg2, "pwd; echo hello > hello.txt; cat hello.txt; python3 -c 'print(40+2)'; python3 -c \"import subprocess; print(subprocess.check_output(['echo','sub-ok']).decode().strip())\"; python3 -c \"import os; os.posix_spawn('/bin/echo', ['echo','spawn-ok'], os.environ)\"; echo a b c | wc -w; ls /work")
	if code != 0 {
		t.Fatalf("legit commands under the filter exit %d, output:\n%s", code, out)
	}
	for _, want := range []string{"/work", "hello", "42", "sub-ok", "spawn-ok", "3", "hello.txt"} {
		if !strings.Contains(out, want) {
			t.Errorf("legit output missing %q:\n%s", want, out)
		}
	}

	// The mount and unshare CLIs must fail with EPERM under the filter.
	out, _ = runSandboxedCfg(t, cfg2, "mount -t tmpfs none /tmp; echo mount-exit=$?; unshare -m true; echo unshare-exit=$?")
	if !strings.Contains(out, "mount-exit=32") {
		t.Errorf("mount under the filter: want exit 32, output:\n%s", out)
	}
	if !strings.Contains(out, "Operation not permitted") {
		t.Errorf("mount under the filter: want 'Operation not permitted', output:\n%s", out)
	}
	if !strings.Contains(out, "unshare-exit=1") {
		t.Errorf("unshare under the filter: want exit 1, output:\n%s", out)
	}
}

// TestSeccompControlWithoutFilter is the negative control for the
// live proof: the same probe script inside the sandbox with the
// filter disabled must report exactly what the host calibration
// reports. A probe that reads BLOCKED here would mean the probe
// cannot tell filtered from unfiltered (or the cell filter blocks
// it, which the host run already showed); a probe that reads
// UNBLOCKED here but BLOCKED under the filter is the proof the
// filter does the blocking.
func TestSeccompControlWithoutFilter(t *testing.T) {
	requireSandbox(t)
	launcher := testLauncher(t)
	host := runProbeOnHost(t)
	root := filepath.Join(t.TempDir(), "root")
	cfg := Config{Enabled: true, Root: root, SeccompLauncher: launcher, DisableSeccomp: true}
	control := runProbeSandboxed(t, cfg)
	for name, hostBlocked := range host {
		if control[name] != hostBlocked {
			t.Errorf("syscall %s: control (no filter) blocked=%v, host blocked=%v; want the control to match the host calibration", name, control[name], hostBlocked)
		}
	}
	// And the mount/unshare CLIs must SUCCEED without the filter
	// (mounting a tmpfs over /tmp inside the sandbox's own mount
	// namespace is harmless and vanishes with it).
	out, code := runSandboxedCfg(t, cfg, "mount -t tmpfs none /tmp && echo mount-ok; unshare -m true && echo unshare-ok")
	if code != 0 || !strings.Contains(out, "mount-ok") || !strings.Contains(out, "unshare-ok") {
		t.Errorf("mount/unshare without the filter: want both to succeed, code %d, output:\n%s", code, out)
	}
}

// TestSeccompFallbackWithoutLauncher proves the graceful fallback:
// with no launcher discoverable anywhere, Prepare still succeeds
// and the command runs (namespaces + chroot only). The environment
// is made hermetic for the lookup (empty ORION_SECCOMP_LAUNCHER,
// a PATH with no orion-seccomp in it).
func TestSeccompFallbackWithoutLauncher(t *testing.T) {
	requireSandbox(t)
	t.Setenv(SeccompLauncherEnv, "")
	t.Setenv("PATH", t.TempDir())
	cfg := Config{Enabled: true, Root: filepath.Join(t.TempDir(), "root")}
	if got := cfg.findSeccompLauncher(); got != "" {
		t.Fatalf("findSeccompLauncher() = %q, want empty (no launcher discoverable)", got)
	}
	out, code := runSandboxedCfg(t, cfg, "echo fallback-ok; mount -t tmpfs none /tmp && echo mount-ok")
	if code != 0 || !strings.Contains(out, "fallback-ok") || !strings.Contains(out, "mount-ok") {
		t.Fatalf("fallback run: want the command to run unfiltered, code %d, output:\n%s", code, out)
	}
}

// TestSeccompCloneFlagProbe proves the clone(2) flag check live: a
// plain clone must succeed under the filter, a namespace clone must
// fail with EPERM, and both must succeed without the filter.
func TestSeccompCloneFlagProbe(t *testing.T) {
	requireSandbox(t)
	launcher := testLauncher(t)
	probe := buildCloneProbe(t)

	runWith := func(cfg Config, command string) string {
		t.Helper()
		cmd := prepareSandboxed(t, cfg, command)
		copyFileMode(t, probe, filepath.Join(cfg.Root, ".orion-root", "cloneprobe"), 0o755)
		out, _ := runPrepared(t, cmd)
		return out
	}

	// SIGCHLD only (17): plain clone. CLONE_NEWNS|SIGCHLD (131089)
	// and CLONE_NEWUSER|SIGCHLD (268435473): namespace clones.
	const plain = "17"
	const newNS = "131089"
	const newUser = "268435473"

	root := filepath.Join(t.TempDir(), "root")
	cfg := Config{Enabled: true, Root: root, SeccompLauncher: launcher}
	out := runWith(cfg, "/cloneprobe "+plain+"; echo plain-exit=$?; /cloneprobe "+newNS+"; echo ns-exit=$?; /cloneprobe "+newUser+"; echo user-exit=$?")
	if !strings.Contains(out, "clone ok status=42") || !strings.Contains(out, "plain-exit=0") {
		t.Errorf("plain clone under the filter: want success, output:\n%s", out)
	}
	if !strings.Contains(out, "ns-exit=1") || !strings.Contains(out, "user-exit=1") {
		t.Errorf("namespace clones under the filter: want exit 1, output:\n%s", out)
	}
	if !strings.Contains(out, "Operation not permitted") {
		t.Errorf("namespace clones under the filter: want EPERM, output:\n%s", out)
	}

	root2 := filepath.Join(t.TempDir(), "root")
	cfg2 := Config{Enabled: true, Root: root2, SeccompLauncher: launcher, DisableSeccomp: true}
	out = runWith(cfg2, "/cloneprobe "+plain+"; echo plain-exit=$?; /cloneprobe "+newNS+"; echo ns-exit=$?; /cloneprobe "+newUser+"; echo user-exit=$?")
	if strings.Count(out, "clone ok status=42") != 3 {
		t.Errorf("clones without the filter: want all three to succeed, output:\n%s", out)
	}
	if !strings.Contains(out, "plain-exit=0") || !strings.Contains(out, "ns-exit=0") || !strings.Contains(out, "user-exit=0") {
		t.Errorf("clones without the filter: want exit 0 for all, output:\n%s", out)
	}
}
