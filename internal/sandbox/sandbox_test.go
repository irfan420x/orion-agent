package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// runSandboxed runs command inside the sandbox rooted at root and
// returns combined output and exit code. It is the test-only driver
// for Prepare: it builds the /bin/sh -c invocation exactly the way
// shell_exec does, then hands it to the sandbox.
func runSandboxed(t *testing.T, root string, command string, allowNetwork bool) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Env = os.Environ()
	cfg := Config{Enabled: true, Root: root, AllowNetwork: allowNetwork}
	if err := cfg.Prepare(cmd, command); err != nil {
		t.Fatalf("Prepare(%q): %v", command, err)
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run %q: %v", command, err)
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
