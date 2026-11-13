// Adversarial sandbox probes (Audit 3, Workstream 2).
//
// These tests try to BREAK the sandbox from the inside. Each one
// stages a real attack — a host canary process or file it must not
// reach, a mount or namespace escape, a privilege-escalation
// primitive, a resource hog — and asserts containment. An attack
// that succeeds here is a sandbox escape: fix it, do not delete
// the test.
//
// Safety rules for this file (the task's hard constraints):
//   - No live destructive command against the host, ever. Every
//     attack runs inside the sandbox, or against a canary file /
//     process created by the test itself inside t.TempDir() and
//     cleaned up by the test.
//   - Every resource hog is bounded (a counted number of
//     processes, a fixed-size allocation, a fixed-size file, a
//     few seconds of CPU) so the probe measures the absence of a
//     limit without endangering the host. Where no limit exists
//     the test proves that with a bounded probe and says so in
//     its name/comment — it does not try to actually exhaust the
//     host (no real fork bomb, no host OOM, no disk fill).
package sandbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// advConfig builds a sandbox Config for adversarial tests with the
// freshly built orion-seccomp launcher wired in explicitly. The
// test binary's own directory has no launcher next to it, so
// auto-discovery would silently run these probes UNFILTERED and
// the filter assertions would be meaningless.
func advConfig(t *testing.T, root string, allowNetwork bool) Config {
	t.Helper()
	return Config{Enabled: true, Root: root, AllowNetwork: allowNetwork, SeccompLauncher: testLauncher(t)}
}

// advConfigNoFilter is advConfig with the seccomp filter disabled:
// the control that proves a probe is really the filter doing the
// blocking (and that the namespaces/pivot alone still contain the
// attack when the filter is absent).
func advConfigNoFilter(t *testing.T, root string, allowNetwork bool) Config {
	t.Helper()
	cfg := advConfig(t, root, allowNetwork)
	cfg.DisableSeccomp = true
	return cfg
}

// hostSeccompFilters reads /proc/self/status Seccomp_filters: for
// the test process — the ambient (cell) filter count. A sandboxed
// run without the Orion launcher must show the same count (the
// filter is inherited across fork/exec); with the launcher it must
// show exactly one more. The bare "Seccomp: 2" field cannot
// discriminate, because the cell filter is inherited either way.
func hostSeccompFilters(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatalf("read /proc/self/status: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Seccomp_filters:") {
			n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Seccomp_filters:")))
			if err != nil {
				t.Fatalf("parse Seccomp_filters %q: %v", line, err)
			}
			return n
		}
	}
	t.Fatal("no Seccomp_filters line in /proc/self/status")
	return 0
}

// markerValue returns the value printed on the "NAME=value" line
// in out, or "" when absent. Exact-line matching matters: a
// substring search for "HOSTNAME=0" would also match
// "CURRENT-HOSTNAME=0", and for "DD=0" would match "DD=0" inside
// longer tokens. All probe assertions in this file use it.
func markerValue(out, name string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, name+"=") {
			return strings.TrimPrefix(line, name+"=")
		}
	}
	return ""
}

// hostCanary starts a sleep process on the host and returns its PID.
// The caller must kill it (t.Cleanup does that here).
func hostCanary(t *testing.T, seconds string) int {
	t.Helper()
	canary := exec.Command("sleep", seconds)
	if err := canary.Start(); err != nil {
		t.Fatalf("start canary sleep: %v", err)
	}
	t.Cleanup(func() { canary.Process.Kill(); canary.Wait() })
	return canary.Process.Pid
}

// TestAdversarialPIDIsolation tries every way to reach a host
// process from inside the sandbox: /proc scan, kill -0 / kill -9
// on the host canary PID, reading its /proc/<pid>/cmdline, and ps.
// The control (kill -0 1) proves the signal path itself works, so
// the failures are isolation, not a broken kill. The canary must
// still be alive afterwards.
func TestAdversarialPIDIsolation(t *testing.T) {
	requireSandbox(t)
	pid := hostCanary(t, "60")
	if pid < 100 {
		t.Skipf("canary pid %d too small to distinguish from namespace PIDs", pid)
	}
	root := filepath.Join(t.TempDir(), "root")
	probe := fmt.Sprintf(`
echo SELF=$$;
kill -0 1 2>/dev/null; echo KILL1=$?;
kill -0 %d 2>/dev/null; echo K0=$?;
kill -9 %d 2>/dev/null; echo K9=$?;
ls /proc/%d 2>&1; echo LSPID=$?;
cat /proc/%d/cmdline 2>&1; echo CMDLINE=$?;
ls /proc/%d/fd 2>&1; echo LFD=$?;
cat /proc/%d/maps >/dev/null 2>&1; echo MAPS=$?;
ls /proc | grep -c '^[0-9]';
`, pid, pid, pid, pid, pid, pid)
	out, code := runSandboxedCfg(t, advConfig(t, root, false), probe)
	if code != 0 {
		t.Fatalf("probe exit %d, output:\n%s", code, out)
	}
	if markerValue(out, "SELF") != "1" {
		t.Errorf("sandboxed shell is not PID 1, output:\n%s", out)
	}
	if markerValue(out, "KILL1") != "0" {
		t.Errorf("kill -0 1 (namespace init) failed: the signal path itself is broken, output:\n%s", out)
	}
	for _, bad := range []string{"K0", "K9"} {
		if markerValue(out, bad) == "0" {
			t.Errorf("signal to host canary pid %d succeeded (%s=0); host processes must be unreachable, output:\n%s", pid, bad, out)
		}
	}
	if markerValue(out, "LSPID") == "0" {
		t.Errorf("/proc/%d is listable inside the sandbox, output:\n%s", pid, out)
	}
	if markerValue(out, "CMDLINE") == "0" {
		t.Errorf("/proc/%d/cmdline is readable inside the sandbox, output:\n%s", pid, out)
	}
	if markerValue(out, "LFD") == "0" {
		t.Errorf("/proc/%d/fd is listable inside the sandbox, output:\n%s", pid, out)
	}
	if markerValue(out, "MAPS") == "0" {
		t.Errorf("/proc/%d/maps is readable inside the sandbox, output:\n%s", pid, out)
	}
	pids, _ := procEntries(t, out)
	if pids[strconv.Itoa(pid)] {
		t.Errorf("host canary pid %d is visible in the sandbox's /proc, output:\n%s", pid, out)
	}
	// ps, when installed, must also show only namespace PIDs.
	if _, err := exec.LookPath("ps"); err == nil {
		out, _ := runSandboxedCfg(t, advConfig(t, root, false), fmt.Sprintf("ps -eo pid 2>&1"))
		for _, line := range strings.Split(out, "\n") {
			if strings.TrimSpace(line) == strconv.Itoa(pid) {
				t.Errorf("ps inside the sandbox lists host canary pid %d, output:\n%s", pid, out)
			}
		}
	}
	// The canary must have survived every signal attempt.
	if err := syscall.Kill(pid, 0); err != nil {
		t.Errorf("host canary pid %d died during the probe (kill -0: %v); the sandbox must not be able to kill host processes", pid, err)
	}
}

// TestAdversarialProcEscapes attacks the /proc surface: the
// detached old root (/.old-root and /proc/1/root/.old-root), the
// host filesystem through /proc/1/root, /proc/sys (subset=pid must
// hide it; the plain-mount fallback binds it read-only), and the
// kernel-information files /proc/kcore, /proc/kallsyms and
// /proc/sysrq-trigger. It also checks that /proc/1/fd exposes no
// host file descriptor (only pipes and /dev/null may appear) and
// that /proc/self/mountinfo no longer mentions the old root.
func TestAdversarialProcEscapes(t *testing.T) {
	requireSandbox(t)
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside.txt")
	if err := os.WriteFile(outside, []byte("OUTSIDE-SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	probe := fmt.Sprintf(`
ls /.old-root 2>&1; echo OLDROOT=$?;
ls /.old-root/etc 2>&1; echo OLDROOT-ETC=$?;
ls /proc/1/root/.old-root 2>&1; echo PROC-OLDROOT=$?;
cat /proc/1/root%s 2>&1; echo PROC-ROOT-CANARY=$?;
cat /proc/self/root%s 2>&1; echo SELF-ROOT-CANARY=$?;
echo "CWD-LINK=$(readlink /proc/1/cwd)";
ls /proc/sys 2>&1; echo LS-SYS=$?;
echo x > /proc/sys/kernel/hostname 2>&1; echo SYS-WRITE=$?;
ls /proc/kcore /proc/kallsyms /proc/sysrq-trigger 2>&1; echo KFILES=$?;
ls /sys 2>&1; echo LS-SYSFS=$?;
cat /sys/kernel/uevent_seq 2>&1; echo SYSFS-READ=$?;
grep -c 'old-root' /proc/self/mountinfo 2>&1; echo MOUNTINFO-OLDROOT=$?;
ls -l /proc/1/fd 2>&1; echo FD-LIST=$?;
`, outside, outside)
	out, code := runSandboxedCfg(t, advConfig(t, root, false), probe)
	if code != 0 {
		t.Fatalf("probe exit %d, output:\n%s", code, out)
	}
	if strings.Contains(out, "OUTSIDE-SECRET") {
		t.Errorf("host canary leaked through /proc/1/root or /proc/self/root:\n%s", out)
	}
	// ls of the detached /.old-root succeeds (it is an empty
	// directory in the new root); what must NOT happen is
	// reaching host content through it. The canary reads above
	// are the real proof; the OLDROOT-* exits are informational.
	if mv := markerValue(out, "PROC-ROOT-CANARY"); mv != "1" && mv != "2" {
		t.Errorf("cat through /proc/1/root did not fail (exit %q), output:\n%s", mv, out)
	}
	if mv := markerValue(out, "SELF-ROOT-CANARY"); mv != "1" && mv != "2" {
		t.Errorf("cat through /proc/self/root did not fail (exit %q), output:\n%s", mv, out)
	}
	// /proc/1/cwd must be /work inside the sandbox, never a host
	// path. (The canary error messages above echo the host path
	// the probe itself passed in — that is the probe's own input,
	// not a leak, so this is checked on the CWD-LINK line only.)
	if got := markerValue(out, "CWD-LINK"); got != "/work" {
		t.Errorf("/proc/1/cwd is %q, want /work, output:\n%s", got, out)
	}
	// /proc/sys: subset=pid hides it (ls fails); either way the
	// write must fail.
	if markerValue(out, "SYS-WRITE") == "0" {
		t.Errorf("writing /proc/sys/kernel/hostname succeeded inside the sandbox, output:\n%s", out)
	}
	if markerValue(out, "KFILES") == "0" {
		t.Errorf("/proc/kcore, /proc/kallsyms or /proc/sysrq-trigger is present inside the sandbox, output:\n%s", out)
	}
	// /sys must be empty and unreadable for kernel information.
	if markerValue(out, "SYSFS-READ") == "0" {
		t.Errorf("/sys/kernel/uevent_seq is readable inside the sandbox, output:\n%s", out)
	}
	// grep -c prints the count, then exits 0 iff the count is
	// > 0. So MOUNTINFO-OLDROOT=0 means old-root IS still in
	// mountinfo (the detach failed); =1 means zero matches.
	if markerValue(out, "MOUNTINFO-OLDROOT") == "0" {
		t.Errorf("mountinfo still mentions old-root, output:\n%s", out)
	}
	// No fd of PID 1 may point at a host file: only pipes and
	// /dev/null are legitimate. A host path here would be a leak
	// of an agent-held descriptor into the sandbox.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, " -> ") && (strings.Contains(line, base) || strings.Contains(line, "/home/hatch/.orion")) {
			t.Errorf("/proc/1/fd exposes a host path: %q\nfull output:\n%s", strings.TrimSpace(line), out)
		}
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "OUTSIDE-SECRET\n" {
		t.Errorf("outside canary: got %q, err %v; want untouched", got, err)
	}
}

// TestAdversarialFilesystemEscape attacks the filesystem boundary:
// symlinks out of /work (absolute host paths, /etc/shadow,
// /proc/1/root), device-node creation, the /dev surface, and
// direct reads of host paths (including the agent's own config
// directory). Mount attempts (bind, overlayfs, tmpfs) are NOT
// probed here: see the NOTE in the probe — they were removed by
// stop order after propagating a tmpfs onto the host's /tmp.
// Mount(2) blocking is proven by the seccomp probe instead.
func TestAdversarialFilesystemEscape(t *testing.T) {
	requireSandbox(t)
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside.txt")
	if err := os.WriteFile(outside, []byte("OUTSIDE-SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	probe := fmt.Sprintf(`
ln -s %s /work/esc; cat /work/esc 2>&1; echo SYMLINK-ABS=$?;
ln -s /etc/shadow /work/shadow; cat /work/shadow 2>&1; echo SYMLINK-SHADOW=$?;
ln -s /proc/1/root /work/r; ls /work/r/home 2>&1; echo SYMLINK-PROCRoot=$?;
ls /work/r/home/hatch/.orion 2>&1; echo SYMLINK-ORION=$?;
cat /home/hatch/.orion/config.json 2>&1; echo DIRECT-CONFIG=$?;
cat /root/.orion/config.json 2>&1; echo DIRECT-ROOT-CONFIG=$?;
cat /etc/shadow 2>&1; echo DIRECT-SHADOW=$?;
# NOTE (Audit 3, 2026-11-13): mount attempts (bind, overlayfs,
# tmpfs) were probed here and REMOVED by stop order — mounting
# inside the sandbox propagated a tmpfs onto the host's /tmp and
# broke the live bot's writes (EPERM on a shadowed /tmp owned by
# a dead user namespace). Mount(2) blocking is still proven at
# the syscall level by TestSeccompLauncherBlocksSyscalls (the
# probe script calls mount with nonexistent paths — no mount is
# ever created). Any future mount probing must run ONLY inside a
# fully private mount namespace, with a before/after "mount |
# grep /tmp" check from an OUTSIDE shell proving the host mount
# table is untouched. See the workstream report.
mknod /work/sda b 8 0 2>&1; echo MKNOD-BLK=$?;
mknod /tmp/vnull c 1 3 2>&1; echo MKNOD-CHR=$?;
if [ -e /tmp/vnull ]; then head -c 1 /tmp/vnull 2>&1; echo OPEN-CHR=$?; fi
mknod /work/fifo p 2>&1; echo MKFIFO=$?;
ls /dev 2>&1; echo LS-DEV=$?;
test -e /dev/tty && echo HAS-TTY || echo NO-TTY;
test -e /dev/pts && echo HAS-PTS || echo NO-PTS;
test -e /dev/sda && echo HAS-SDA || echo NO-SDA;
test -e /dev/shm && echo HAS-SHM || echo NO-SHM;
df -k /tmp | tail -1; echo DF-TMP=$?;
`, outside)
	out, code := runSandboxedCfg(t, advConfig(t, root, false), probe)
	if code != 0 {
		t.Fatalf("probe exit %d, output:\n%s", code, out)
	}
	if strings.Contains(out, "OUTSIDE-SECRET") {
		t.Errorf("outside canary leaked through a symlink or direct read:\n%s", out)
	}
	for _, wantFail := range []string{
		"SYMLINK-ABS", "SYMLINK-SHADOW", "SYMLINK-PROCRoot",
		"SYMLINK-ORION", "DIRECT-CONFIG", "DIRECT-ROOT-CONFIG",
		"DIRECT-SHADOW",
	} {
		if markerValue(out, wantFail) == "0" {
			t.Errorf("escape succeeded (%s=0), output:\n%s", wantFail, out)
		}
	}
	// Device nodes: creation must fail; if one is somehow
	// created, opening it must still fail (nodev on /work and
	// the /tmp tmpfs). A fifo, by contrast, is legitimate and
	// must keep working — mknod is deliberately not blocked.
	if markerValue(out, "MKNOD-BLK") == "0" {
		t.Errorf("block device node created in /work, output:\n%s", out)
	}
	if markerValue(out, "MKNOD-CHR") == "0" && markerValue(out, "OPEN-CHR") == "0" {
		t.Errorf("character device node created in /tmp AND readable, output:\n%s", out)
	}
	if markerValue(out, "MKFIFO") != "0" {
		t.Errorf("mkfifo broke inside the sandbox (it must keep working), output:\n%s", out)
	}
	for _, want := range []string{"NO-TTY", "NO-PTS", "NO-SDA", "NO-SHM"} {
		if !strings.Contains(out, want) {
			t.Errorf("/dev exposes more than the allowlist (%s missing), output:\n%s", want, out)
		}
	}
	for _, node := range []string{"null", "zero", "full", "random", "urandom"} {
		if !strings.Contains(out, node) {
			t.Errorf("/dev listing is missing harmless node %q, output:\n%s", node, out)
		}
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "OUTSIDE-SECRET\n" {
		t.Errorf("outside canary: got %q, err %v; want untouched", got, err)
	}
}

// TestAdversarialNestedUserNSCannotEscape was REMOVED by stop
// order (Audit 3, 2026-11-13): it created nested user namespaces
// via the unshare CLI, and namespace-creation probing is stopped
// alongside mount probing after the host /tmp interference (see
// the NOTE in TestAdversarialFilesystemEscape). Its assertion —
// pivot_root containment without the seccomp filter — is already
// proven by TestSandboxChrootEscapeBlocked, which runs without a
// launcher (auto-discovery finds none next to the test binary),
// i.e. with namespaces + pivot only.

// startLocalListeners starts two host services the sandbox must not
// reach when network is disallowed: a TCP listener on 127.0.0.1
// (ephemeral port) and an abstract unix socket. Both serve the same
// marker. The abstract name is unique per test process so parallel
// runs cannot collide.
func startLocalListeners(t *testing.T) (tcpPort int, abstractName string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen 127.0.0.1: %v", err)
	}
	tcpPort = ln.Addr().(*net.TCPAddr).Port
	abstractName = fmt.Sprintf("orion-adv-%d", os.Getpid())
	uln, err := net.ListenUnix("unix", &net.UnixAddr{Name: "@" + abstractName, Net: "unix"})
	if err != nil {
		ln.Close()
		t.Fatalf("listen abstract unix socket: %v", err)
	}
	serve := func(l net.Listener) {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// Minimal HTTP, not a bare marker: curl rejects a
				// bare "HOST-LOCAL-OK" reply as HTTP/0.9.
				fmt.Fprint(c, "HTTP/1.0 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 14\r\n\r\nHOST-LOCAL-OK\n")
			}(c)
		}
	}
	go serve(ln)
	go serve(uln)
	t.Cleanup(func() { ln.Close(); uln.Close() })
	return tcpPort, abstractName
}

// TestAdversarialNetwork proves the network namespace both ways:
// with network disallowed, egress (curl to a public host), DNS, a
// TCP connection to a host localhost service, and an abstract unix
// socket connection must ALL fail; with AllowNetwork, the localhost
// service, the abstract socket, DNS and egress must ALL work. The
// allowed case is the control that the probes themselves are valid
// (a probe that fails in both modes proves nothing).
func TestAdversarialNetwork(t *testing.T) {
	requireSandbox(t)
	port, abstractName := startLocalListeners(t)
	root := filepath.Join(t.TempDir(), "root")

	// --- Network disallowed: everything must fail. ---
	// curl -k: the cell's egress CA bundle
	// (/run/hatch/egress-tls/ca-bundle.pem, named by
	// CURL_CA_BUNDLE/SSL_CERT_FILE) does not exist inside the
	// chroot, so TLS verification cannot run there at all; -k
	// tests reachability only. That gap (AllowNetwork https is
	// broken out of the box on this host until the bundle is
	// provisioned into the sandbox) is reported, not hidden.
	probe := fmt.Sprintf(`
curl -k -m 5 -sS -o /dev/null -w 'EGRESS-HTTP=%%{http_code}\n' https://example.com 2>&1; echo EGRESS-EXIT=$?;
python3 -c "import socket; print(socket.getaddrinfo('example.com', 80))" 2>&1; echo DNS=$?;
python3 -c "import socket; s=socket.socket(); s.settimeout(3); s.connect(('127.0.0.1', %d)); print(s.recv(64))" 2>&1; echo TCP-LOCAL=$?;
python3 -c "import socket; s=socket.socket(socket.AF_UNIX); s.settimeout(3); s.connect(chr(0)+'%s'); print(s.recv(64).decode())" 2>&1; echo ABSTRACT=$?;
python3 -c "import socket; s=socket.socket(); s.settimeout(3); s.connect(('8.8.8.8', 53)); print('RAW-OK')" 2>&1; echo RAW=$?;
`, port, abstractName)
	out, code := runSandboxedCfg(t, advConfig(t, root, false), probe)
	if code != 0 {
		t.Fatalf("no-network probe exit %d, output:\n%s", code, out)
	}
	if strings.Contains(out, "HOST-LOCAL-OK") {
		t.Errorf("sandbox with network disallowed reached a host localhost service:\n%s", out)
	}
	for _, wantFail := range []string{"EGRESS-EXIT", "DNS", "TCP-LOCAL", "ABSTRACT", "RAW"} {
		if markerValue(out, wantFail) == "0" {
			t.Errorf("network probe succeeded with network disallowed (%s=0), output:\n%s", wantFail, out)
		}
	}
	if markerValue(out, "EGRESS-HTTP") == "200" {
		t.Errorf("egress curl returned 200 with network disallowed, output:\n%s", out)
	}

	// --- Network allowed: the same probes must succeed (control). ---
	root2 := filepath.Join(t.TempDir(), "root")
	probe2 := fmt.Sprintf(`
curl -k -m 10 -sS -o /dev/null -w 'EGRESS-HTTP=%%{http_code}\n' https://example.com 2>&1; echo EGRESS-EXIT=$?;
python3 -c "import socket; print(socket.getaddrinfo('example.com', 80)[0][4])" 2>&1; echo DNS=$?;
curl -m 5 -sS http://127.0.0.1:%d/ 2>&1; echo TCP-LOCAL=$?;
python3 -c "import socket; s=socket.socket(socket.AF_UNIX); s.settimeout(3); s.connect(chr(0)+'%s'); print(s.recv(64).decode())" 2>&1; echo ABSTRACT=$?;
`, port, abstractName)
	out, code = runSandboxedCfg(t, advConfig(t, root2, true), probe2)
	if code != 0 {
		t.Fatalf("network-allowed probe exit %d, output:\n%s", code, out)
	}
	if markerValue(out, "EGRESS-HTTP") != "200" || markerValue(out, "EGRESS-EXIT") != "0" {
		t.Errorf("egress curl failed with network allowed (control broken — the probe is invalid, not the sandbox), output:\n%s", out)
	}
	if markerValue(out, "DNS") != "0" {
		t.Errorf("DNS failed with network allowed (control broken), output:\n%s", out)
	}
	if !strings.Contains(out, "HOST-LOCAL-OK") || markerValue(out, "TCP-LOCAL") != "0" {
		t.Errorf("host localhost service unreachable with network allowed (control broken), output:\n%s", out)
	}
	if markerValue(out, "ABSTRACT") != "0" {
		t.Errorf("abstract unix socket unreachable with network allowed (control broken), output:\n%s", out)
	}
}

// TestAdversarialPrivilegeEscalation tries the escalation
// primitives available inside the sandbox: sudo/su, newuidmap, a
// direct init_module syscall, and sethostname. All must fail. The
// capabilities in /proc/self/status are expected to be FULL —
// they are scoped to the sandbox's own user namespace, so the
// proof is that the privileged operations still fail, not that
// the caps are empty. NOTE: sethostname fails here whether or
// not the seccomp filter is installed (verified by the control
// run below): the UTS namespace is created by the clone in the
// outer user namespace, where the process holds no privilege —
// so the failure proves namespace/capability containment, not
// filter action. The filter itself is proven live by
// TestAdversarialSeccompFilterReallyOn (filter count +1) and
// TestAdversarialSeccompGaps (modify_ldt discriminating).
// NOTE: unshare attempts were probed here and REMOVED by stop
// order (Audit 3, 2026-11-13) alongside the mount probes —
// namespace creation is proven blocked at the syscall level by
// TestSeccompLauncherBlocksSyscalls instead.
func TestAdversarialPrivilegeEscalation(t *testing.T) {
	requireSandbox(t)
	root := filepath.Join(t.TempDir(), "root")
	out, code := runSandboxedCfg(t, advConfig(t, root, false), `
id -u; echo ID=$?;
grep -E '^(CapEff|Seccomp):' /proc/self/status;
hostname evil-name 2>&1; echo HOSTNAME=$?;
hostname; echo CURRENT-HOSTNAME=$?;
sudo -n id 2>&1; echo SUDO=$?;
su root -c id 2>&1; echo SU=$?;
newuidmap $$ 0 1 2>&1; echo NEWUIDMAP=$?;
newgidmap $$ 0 1 2>&1; echo NEWGIDMAP=$?;
python3 -c "import ctypes, os; libc=ctypes.CDLL('libc.so.6', use_errno=True); ctypes.set_errno(0); r=libc.syscall(175, None, 1); e=ctypes.get_errno(); print('INIT-MODULE ret=%d errno=%d (%s)' % (r, e, os.strerror(e)))" 2>&1;
capsh --print 2>&1 | head -3; echo CAPSH=$?;
`)
	if code != 0 {
		t.Fatalf("probe exit %d, output:\n%s", code, out)
	}
	if markerValue(out, "ID") != "0" {
		t.Errorf("sandboxed uid is not 0 (inside-root), output:\n%s", out)
	}
	// CapEff is expected to be all-ones: the process IS root in
	// its own user namespace. Log it; the failures below are the
	// proof those caps do not extend to the host.
	if !strings.Contains(out, "CapEff:") {
		t.Errorf("no CapEff line in /proc/self/status, output:\n%s", out)
	}
	for _, wantFail := range []string{
		"HOSTNAME", "SUDO", "SU", "NEWUIDMAP", "NEWGIDMAP",
	} {
		if markerValue(out, wantFail) == "0" {
			t.Errorf("privilege escalation succeeded (%s=0), output:\n%s", wantFail, out)
		}
	}
	if strings.Contains(out, "INIT-MODULE ret=-1 errno=1") {
		// EPERM: blocked by the seccomp filter (or the cell
		// filter). Either way it did not load a module. The
		// probe prints the strerror too, so the assertion
		// below matches real EPERM evidence, not a string no
		// command emits.
	} else if strings.Contains(out, "INIT-MODULE") {
		t.Errorf("init_module did not fail with EPERM, output:\n%s", out)
	} else {
		t.Errorf("init_module probe produced no output, output:\n%s", out)
	}
	if !strings.Contains(out, "Operation not permitted") {
		t.Errorf("expected EPERM failures from the seccomp filter, output:\n%s", out)
	}
	// Control: sethostname must fail the same way WITHOUT the
	// filter. That proves the failure is namespace/capability
	// containment (the UTS namespace is owned by the outer
	// user namespace), not the filter — and stops this test
	// from ever again being misread as a filter proof.
	root2 := filepath.Join(t.TempDir(), "root")
	ctl, code := runSandboxedCfg(t, advConfigNoFilter(t, root2, false), "hostname evil-name 2>&1; echo HOSTNAME=$?")
	if code != 0 {
		t.Fatalf("control probe exit %d, output:\n%s", code, ctl)
	}
	if markerValue(ctl, "HOSTNAME") == "0" {
		t.Errorf("control: sethostname succeeded without the filter — the containment proof is broken, output:\n%s", ctl)
	}
}

// TestAdversarialSeccompFilterReallyOn proves the filter is
// actually installed for sandboxed commands: the seccomp filter
// count inside (Seccomp_filters) must be exactly one more than
// the ambient count of the test process when the launcher is
// used, and exactly equal when it is disabled. "Seccomp: 2"
// alone proves nothing — the cell filter is inherited either way.
func TestAdversarialSeccompFilterReallyOn(t *testing.T) {
	requireSandbox(t)
	host := hostSeccompFilters(t)
	t.Logf("ambient Seccomp_filters count: %d", host)

	root := filepath.Join(t.TempDir(), "root")
	out, code := runSandboxedCfg(t, advConfig(t, root, false), "grep Seccomp_filters /proc/self/status")
	if code != 0 {
		t.Fatalf("filtered probe exit %d, output:\n%s", code, out)
	}
	want := fmt.Sprintf("Seccomp_filters:\t%d", host+1)
	if !strings.Contains(out, want) {
		t.Errorf("with launcher: want %q, output:\n%s", want, out)
	}

	root2 := filepath.Join(t.TempDir(), "root")
	out, code = runSandboxedCfg(t, advConfigNoFilter(t, root2, false), "grep Seccomp_filters /proc/self/status")
	if code != 0 {
		t.Fatalf("unfiltered probe exit %d, output:\n%s", code, out)
	}
	want = fmt.Sprintf("Seccomp_filters:\t%d", host)
	if !strings.Contains(out, want) {
		t.Errorf("without launcher: want %q (ambient count, no Orion filter), output:\n%s", want, out)
	}
}

// TestAdversarialSeccompGaps probes the syscalls the main probe
// does not cover: io_uring (kernel attack surface, nothing in the
// sandbox needs it) and modify_ldt (LDT manipulation). Both must
// be BLOCKED under the filter. The control expectations differ
// per syscall because the ambient cell filter already blocks
// io_uring on this host (host calibration: EPERM) but NOT
// modify_ldt (host calibration: success) — so modify_ldt is the
// live discriminating proof, while io_uring is proven live here
// and structurally by TestSeccompFilterProgram (which checks
// every table entry has a JEQ landing on DENY).
//
// pidfd_getfd is deliberately NOT blocklisted: it can only target
// processes in the sandbox's own PID namespace (host PIDs do not
// exist there), and the probe asserts the only stealable fd of
// PID 1 is its stdio pipe — never a host file.
func TestAdversarialSeccompGaps(t *testing.T) {
	requireSandbox(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}

	// Host calibration (no sandbox, no Orion filter).
	hostOut, err := exec.Command(python, testdataPath(t, "adversarial_syscall_probe.py")).CombinedOutput()
	if err != nil {
		t.Fatalf("host probe run: %v\n%s", err, hostOut)
	}
	host := string(hostOut)
	t.Logf("host calibration:\n%s", host)
	if !strings.Contains(host, "modify_ldt UNBLOCKED") {
		t.Errorf("host calibration: modify_ldt is not UNBLOCKED on the host — the control is broken, output:\n%s", host)
	}
	// io_uring is already EPERM on this host (the cell filter
	// blocks it); the probe must agree, not contradict it.
	if !strings.Contains(host, "io_uring_setup BLOCKED") {
		t.Errorf("host calibration: io_uring_setup is not BLOCKED on the host — expected the cell filter to block it, output:\n%s", host)
	}

	runProbe := func(cfg Config) string {
		t.Helper()
		cmd := prepareSandboxed(t, cfg, "python3 /work/adversarial_syscall_probe.py")
		copyFileMode(t, testdataPath(t, "adversarial_syscall_probe.py"), filepath.Join(cfg.Root, "work", "adversarial_syscall_probe.py"), 0o644)
		out, code := runPrepared(t, cmd)
		if code != 0 {
			t.Fatalf("sandboxed probe exit %d, output:\n%s", code, out)
		}
		return out
	}

	// With the filter: everything dangerous must be BLOCKED.
	root := filepath.Join(t.TempDir(), "root")
	filtered := runProbe(advConfig(t, root, false))
	t.Logf("filtered:\n%s", filtered)
	for _, name := range []string{"io_uring_setup", "io_uring_enter", "io_uring_register", "modify_ldt"} {
		if !strings.Contains(filtered, name+" BLOCKED") {
			t.Errorf("syscall %s: not BLOCKED under the filter, output:\n%s", name, filtered)
		}
	}

	// Without the filter (control): modify_ldt must be UNBLOCKED
	// (proving the filter is what blocks it above); io_uring
	// stays BLOCKED (the cell filter), matching the host.
	root2 := filepath.Join(t.TempDir(), "root")
	control := runProbe(advConfigNoFilter(t, root2, false))
	t.Logf("control (no filter):\n%s", control)
	if !strings.Contains(control, "modify_ldt UNBLOCKED") {
		t.Errorf("control: modify_ldt is not UNBLOCKED without the filter — the probe cannot discriminate, output:\n%s", control)
	}
	if !strings.Contains(control, "io_uring_setup BLOCKED") {
		t.Errorf("control: io_uring_setup is not BLOCKED without the filter — expected the cell filter to block it, output:\n%s", control)
	}

	// pidfd_getfd: must not yield a host file in either mode.
	for mode, out := range map[string]string{"filtered": filtered, "control": control} {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "pidfd_getfd OK") {
				target := strings.TrimPrefix(line, "pidfd_getfd OK target=")
				if !strings.HasPrefix(target, "pipe:[") && target != "/dev/null" {
					t.Errorf("%s: pidfd_getfd stole fd 1 of PID 1 pointing at %q — expected a pipe or /dev/null, output:\n%s", mode, target, out)
				}
			}
			if strings.HasPrefix(line, "pidfd_getfd PIDFD-FAIL") {
				t.Errorf("%s: pidfd_open(1) failed — the probe is broken, output:\n%s", mode, out)
			}
		}
	}
	// And pidfd_open of a HOST pid must fail inside the sandbox:
	// host PIDs do not exist in the namespace.
	pid := hostCanary(t, "60")
	if pid >= 100 {
		out, _ := runSandboxedCfg(t, advConfig(t, root, false), fmt.Sprintf(
			`python3 -c "import ctypes; libc=ctypes.CDLL('libc.so.6', use_errno=True); ctypes.set_errno(0); r=libc.syscall(434, %d, 0); print('PIDFD-HOST ret=%%d errno=%%d' %% (r, ctypes.get_errno()))" 2>&1`, pid))
		if strings.Contains(out, "PIDFD-HOST ret=") && !strings.Contains(out, "errno=3") {
			t.Errorf("pidfd_open of host pid %d did not fail with ESRCH, output:\n%s", pid, out)
		}
	}
}

// TestAdversarialForkBound spawns 200 processes inside the sandbox
// and counts them — a bounded fork probe, NOT a fork bomb. There
// is no pids cgroup on this host (no pids.max anywhere under
// /sys/fs/cgroup), so no pids limit can exist here; the probe
// proves 200 processes run fine, and that they are all gone when
// the sandbox exits (namespace death kills them — see
// TestAdversarialNamespaceDeathKillsAll). 200 is far below
// anything that could trouble the host (4 CPUs, ~8 GB RAM).
func TestAdversarialForkBound(t *testing.T) {
	requireSandbox(t)
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not available; cannot verify host-side cleanup")
	}
	root := filepath.Join(t.TempDir(), "root")
	out, code := runSandboxedCfg(t, advConfig(t, root, false), `
for i in $(seq 1 200); do sleep 30 >/dev/null 2>&1 & done
echo PROCS=$(ls /proc | grep -c '^[0-9]');
`)
	if code != 0 {
		t.Fatalf("probe exit %d, output:\n%s", code, out)
	}
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "PROCS=") {
			n, _ = strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(line), "PROCS="))
		}
	}
	// 200 sleeps + the shell + a few helpers: the count must show
	// the fork load actually happened. A limit below 200 would
	// show up as a much smaller count (or fork failures).
	if n < 150 {
		t.Errorf("only %d processes visible after spawning 200 sleeps; a pids limit (or fork failures) may be in play, output:\n%s", n, out)
	} else {
		t.Logf("spawned 200 sleeps; %d processes visible inside (no pids limit at this scale)", n)
	}
	// Host-side: no "sleep 30" from the sandbox may survive.
	// The pattern is anchored: an unanchored "sleep 30" would
	// also match an unrelated host "sleep 300".
	time.Sleep(500 * time.Millisecond)
	if stray, err := exec.Command("pgrep", "-f", "^sleep 30$").CombinedOutput(); err == nil && len(strings.TrimSpace(string(stray))) > 0 {
		t.Errorf("stray sandboxed sleeps survived on the host: %q", strings.TrimSpace(string(stray)))
	}
}

// TestAdversarialMemoryHog allocates a bounded 400 MB inside the
// sandbox and proves it SUCCEEDS — i.e. there is NO memory limit.
// There is no memory.max anywhere under /sys/fs/cgroup on this
// host, so no memory cgroup limit can be applied here at all. The
// probe deliberately does NOT try to actually OOM the host: a
// larger hog would push the host toward the OOM killer, which
// could kill host processes. The honest statement is: the sandbox
// has no memory bound on this host; the only bound is the
// shell_exec timeout (the command is killed when its context
// expires) and the host OOM killer. 400 MB is safely below the
// ~734 MB available.
func TestAdversarialMemoryHog(t *testing.T) {
	requireSandbox(t)
	root := filepath.Join(t.TempDir(), "root")
	out, code := runSandboxedCfg(t, advConfig(t, root, false), `
python3 -c "
b = bytearray(400*1024*1024)
for i in range(0, len(b), 4096):
    b[i] = 1
rss = open('/proc/self/status').read().split('VmRSS:')[1].split()[0]
print('ALLOC-OK RSS-KB=' + rss)
" 2>&1; echo ALLOC=$?;
`)
	if code != 0 {
		t.Fatalf("probe exit %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "ALLOC-OK") {
		t.Errorf("bounded 400 MB allocation failed inside the sandbox (a limit would be surprising — the host has none), output:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "RSS-KB=") {
			kb, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(line), "ALLOC-OK RSS-KB="))
			if err != nil || kb < 350000 {
				t.Errorf("RSS after 400 MB allocation is %q (want >= 350000 KB), output:\n%s", line, out)
			} else {
				t.Logf("400 MB allocated inside the sandbox, RSS %d KB — no memory limit (documented limitation, not a fixable bug on this host)", kb)
			}
		}
	}
	// The host has no cgroup memory controller file to set a
	// limit with: record that as the reason.
	if _, err := os.Stat("/sys/fs/cgroup/memory.max"); os.IsNotExist(err) {
		t.Logf("no /sys/fs/cgroup/memory.max on this host: a memory limit cannot be enforced here")
	}
}

// TestAdversarialDiskFill writes a bounded 50 MB file into /work
// and proves it lands on the host (i.e. there is NO disk quota on
// /work — the host disk has ~146 GB free). The file is removed by
// the test. /tmp, by contrast, IS bounded (see
// TestAdversarialTmpSizeBound): it is a tmpfs with size=256M.
func TestAdversarialDiskFill(t *testing.T) {
	requireSandbox(t)
	root := filepath.Join(t.TempDir(), "root")
	out, code := runSandboxedCfg(t, advConfig(t, root, false), `
dd if=/dev/zero of=/work/fill.bin bs=1M count=50 2>&1; echo DD=$?;
ls -l /work/fill.bin 2>&1;
`)
	if code != 0 {
		t.Fatalf("probe exit %d, output:\n%s", code, out)
	}
	if markerValue(out, "DD") != "0" {
		t.Errorf("bounded 50 MB write to /work failed, output:\n%s", out)
	}
	fi, err := os.Stat(filepath.Join(root, "work", "fill.bin"))
	if err != nil {
		t.Fatalf("fill file did not land on the host: %v", err)
	}
	if fi.Size() != 50*1024*1024 {
		t.Errorf("fill file on host is %d bytes, want %d", fi.Size(), 50*1024*1024)
	}
	t.Logf("50 MB written to /work with no quota — /work is unbounded on this host (host disk ~146 GB free); documented limitation")
	if err := os.Remove(filepath.Join(root, "work", "fill.bin")); err != nil {
		t.Fatalf("cleanup fill file: %v", err)
	}
}

// TestAdversarialTmpSizeBound is the regression test for the
// unbounded-/tmp hole: the sandbox's /tmp tmpfs used to be
// mounted with no size=, so filling it consumed host RAM without
// bound (default tmpfs size is 50% of RAM). It is now mounted
// with size=256M. The probe asserts the size and that a 300 MB
// fill fails with ENOSPC. 256 MB is safely below the ~734 MB
// available, and the tmpfs vanishes with the namespace.
func TestAdversarialTmpSizeBound(t *testing.T) {
	requireSandbox(t)
	root := filepath.Join(t.TempDir(), "root")
	out, code := runSandboxedCfg(t, advConfig(t, root, false), `
df -k /tmp | tail -1;
dd if=/dev/zero of=/tmp/big bs=1M count=300 2>&1; echo DD=$?;
df -k /tmp | tail -1;
`)
	if code != 0 {
		t.Fatalf("probe exit %d, output:\n%s", code, out)
	}
	// df total for /tmp must be 256M (262144 KiB), not ~50% of RAM.
	found := false
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "tmpfs" {
			total, err := strconv.Atoi(fields[1])
			if err == nil && total == 256*1024 {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("/tmp tmpfs is not 256M (want total 262144 KiB), output:\n%s", out)
	}
	if markerValue(out, "DD") == "0" {
		t.Errorf("300 MB write to /tmp succeeded: the /tmp tmpfs is not size-bounded, output:\n%s", out)
	}
	if !strings.Contains(out, "No space left on device") {
		t.Errorf("expected ENOSPC from the /tmp fill, output:\n%s", out)
	}
}

// TestAdversarialCPUSpin runs a 4-second CPU spin inside the
// sandbox while a second sandboxed command runs concurrently on
// the host. The echo must complete promptly: the spin consumes
// one core (there is no cpu.max cgroup on this host, so no CPU
// limit can exist), but it must not starve the agent. The
// shell_exec timeout is the only bound on a spin.
func TestAdversarialCPUSpin(t *testing.T) {
	requireSandbox(t)
	spinRoot := filepath.Join(t.TempDir(), "spin")
	spinCmd := prepareSandboxed(t, advConfig(t, spinRoot, false),
		`python3 -c "import time; end=time.time()+4; x=0
while time.time()<end: x+=1
print('SPUN', x)"`)
	var spinBuf strings.Builder
	spinCmd.Stdout = &spinBuf
	spinCmd.Stderr = &spinBuf
	if err := spinCmd.Start(); err != nil {
		t.Fatalf("start spin: %v", err)
	}
	// Do NOT read spinBuf until spinCmd.Wait() returns below:
	// os/exec's stdout-copy goroutine writes to the buffer until
	// the process exits, and strings.Builder is not safe for
	// concurrent use. Reading it while the spin runs is a data
	// race (found by -race in the Audit 3 close-out). If the
	// test fails early, the 30s context from prepareSandboxed
	// (cancelled by t.Cleanup) kills the spin.
	start := time.Now()
	echoRoot := filepath.Join(t.TempDir(), "echo")
	out, code := runSandboxedCfg(t, advConfig(t, echoRoot, false), "echo QUICK-OK")
	elapsed := time.Since(start)
	if code != 0 || !strings.Contains(out, "QUICK-OK") {
		t.Fatalf("echo during spin: code %d, output:\n%s", code, out)
	}
	if elapsed > 10*time.Second {
		t.Errorf("second sandboxed command took %v during a CPU spin — the agent was starved", elapsed)
	}
	// Wait for the spin (and its copy goroutines) BEFORE reading
	// the buffer: Cmd.Wait only returns once copying from stdout
	// and stderr has completed, so the read below is race-free.
	if err := spinCmd.Wait(); err != nil {
		t.Fatalf("spin wait: %v", err)
	}
	t.Logf("second sandboxed command completed in %v while a CPU spin ran (spin output: %q)", elapsed, strings.TrimSpace(spinBuf.String()))
}

// TestAdversarialNamespaceDeathKillsAll proves the kernel
// property the sandbox relies on for cleanup: when PID 1 of a
// PID namespace dies, every process in the namespace is
// SIGKILLed. A sandboxed command that backgrounds sleeps and is
// then killed (context timeout, as the agent does on shell_exec
// timeout) must leave no stray sleep on the host.
func TestAdversarialNamespaceDeathKillsAll(t *testing.T) {
	requireSandbox(t)
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not available; cannot verify host-side cleanup")
	}
	root := filepath.Join(t.TempDir(), "root")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "true")
	cmd.Env = os.Environ()
	cfg := advConfig(t, root, false)
	// Distinctive marker sleeps (not "sleep 60" — the canary).
	const marker = "sleep 177"
	command := marker + " & echo $! >> /work/pids.txt; " + marker + " & echo $! >> /work/pids.txt; sleep 30"
	if err := cfg.Prepare(cmd, command); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	// Start, let the sleeps spawn, then kill the wrapper (PID 1
	// of the namespace) by cancelling the context — the same
	// mechanism as a shell_exec timeout.
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	cancel()
	_ = cmd.Wait()
	// The two sleeps must have existed (small namespace PIDs in
	// the marker file) and must now be gone from the host.
	data, err := os.ReadFile(filepath.Join(root, "work", "pids.txt"))
	if err != nil {
		t.Fatalf("marker pids file missing (the sleeps never spawned): %v\nwrapper output:\n%s", err, buf.String())
	}
	pids := []int{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || n < 1 || n > 1000 {
			t.Fatalf("marker pid %q is not a small namespace PID: %q", line, data)
		}
		pids = append(pids, n)
	}
	if len(pids) != 2 {
		t.Fatalf("want 2 marker sleeps, got %d (%q)", len(pids), data)
	}
	time.Sleep(500 * time.Millisecond)
	// Anchored, for the same reason as the fork test: an
	// unanchored pattern would match an unrelated "sleep 1770".
	if stray, err := exec.Command("pgrep", "-f", "^sleep 177$").CombinedOutput(); err == nil && len(strings.TrimSpace(string(stray))) > 0 {
		t.Errorf("stray marker sleeps survived namespace death on the host: %q", strings.TrimSpace(string(stray)))
	}
}
