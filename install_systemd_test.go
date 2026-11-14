// install_systemd_test.go — F-A17-8 (rc23): pin install.sh --systemd
// behaviour. Audit 17 found install.sh had NO test at all, so the
// --systemd path (install the shipped user unit, enable --now,
// linger) could regress silently. This test runs the real script
// against a scratch HOME / prefix with stub systemctl/loginctl on
// PATH, so it never touches the real /etc, the real ~/.config, or
// the live daemon.
//
// What this test does NOT prove (PENDING, laptop only): boot
// recovery — that the unit actually comes back up after a real
// reboot. There is no safe way to trigger a reboot on this VM, so
// boot-recovery proof stays PENDING for the user's laptop; this
// test pins install + enable --now + linger only. No watchdog
// (WatchdogSec) is asserted either: it needs sd_notify support in
// oriond and is documented future work.
package orion_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// installTestRepoRoot returns the repo root (the directory this
// test file lives in), independent of the test's working directory.
func installTestRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(file)
}

// installTestGoDir returns a directory holding a Go toolchain, or
// "" when none is found (same search order as install.sh itself).
func installTestGoDir(t *testing.T) string {
	t.Helper()
	if dir, err := exec.LookPath("go"); err == nil {
		return filepath.Dir(dir)
	}
	home, err := os.UserHomeDir()
	if err == nil {
		for _, cand := range []string{
			filepath.Join(home, "go1.27.4", "bin", "go"),
			"/usr/local/go/bin/go",
		} {
			if st, err := os.Stat(cand); err == nil && !st.IsDir() {
				return filepath.Dir(cand)
			}
		}
	}
	return ""
}

// installTestStub writes a stub binary that records its invocation
// (one line per call) to $ORION_TEST_STUB_LOG and always exits 0.
func installTestStub(t *testing.T, dir, name string) {
	t.Helper()
	// The stub appends "$0 $*" to the shared log; the log path
	// travels in the environment so the stub needs no hardcoding.
	script := "#!/bin/sh\n" +
		"echo \"" + name + " $*\" >> \"$ORION_TEST_STUB_LOG\"\n" +
		"exit 0\n"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub %s: %v", name, err)
	}
}

// installTestSnapshot records whether path exists and, if so, its
// contents, so the test can prove the real HOME was untouched.
func installTestSnapshot(t *testing.T, path string) (exists bool, content []byte) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		t.Fatalf("snapshot %s: %v", path, err)
	}
	return true, b
}

func TestInstallSystemdInstallsAndEnablesUnit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is a bash script; not runnable on Windows")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	goDir := installTestGoDir(t)
	if goDir == "" {
		t.Skip("no Go toolchain found (install.sh needs Go 1.27+)")
	}
	repoRoot := installTestRepoRoot(t)
	script := filepath.Join(repoRoot, "install.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("install.sh not found at %s: %v", script, err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "oriond.service")); err != nil {
		t.Fatalf("shipped oriond.service not found in %s: %v", repoRoot, err)
	}

	// Scratch HOME + prefix: everything install.sh writes must
	// land here, never in the real HOME or /etc.
	scratch := t.TempDir()
	home := filepath.Join(scratch, "home")
	prefix := filepath.Join(scratch, "prefix")
	stubBin := filepath.Join(scratch, "stubbin")
	for _, dir := range []string{home, prefix, stubBin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stubLog := filepath.Join(scratch, "stub.log")
	installTestStub(t, stubBin, "systemctl")
	installTestStub(t, stubBin, "loginctl")

	// Pre-create a config with a sentinel: install.sh must never
	// overwrite an existing ~/.orion/config.json.
	configDir := filepath.Join(home, ".orion")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := "{\"sentinel\": \"keep-me\", \"default_provider\": \"ollama\"}\n"
	configPath := filepath.Join(configDir, "config.json")
	if err := os.WriteFile(configPath, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}

	// Snapshot the REAL home's unit file (if any): the run must
	// leave it byte-identical (or still absent). The real HOME is
	// read before it is overridden for the child process.
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	realUnit := filepath.Join(realHome, ".config", "systemd", "user", "oriond.service")
	realExists, realContent := installTestSnapshot(t, realUnit)

	// Child environment: scratch HOME, stubs first on PATH, Go
	// toolchain reachable, USER set (install.sh is `set -u` and
	// passes $USER to loginctl enable-linger). GOTMPDIR stays
	// under /tmp (never under /home — it breaks the suite) and
	// TMPDIR is unset.
	gotmp := filepath.Join(scratch, "gotmp")
	if err := os.MkdirAll(gotmp, 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"HOME=" + home,
		"USER=testuser",
		"PATH=" + stubBin + ":" + goDir + ":" + os.Getenv("PATH"),
		"ORION_TEST_STUB_LOG=" + stubLog,
		"GOTMPDIR=" + gotmp,
		"GOCACHE=" + os.Getenv("GOCACHE"),
	}
	// Drop TMPDIR and any inherited GOCACHE-empty entry: an empty
	// GOCACHE= entry is harmless, but keep the env explicit.
	if os.Getenv("GOCACHE") == "" {
		env = env[:len(env)-1]
	}

	cmd := exec.Command(bash, script, "--prefix", prefix, "--systemd")
	cmd.Dir = repoRoot
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh --prefix %s --systemd failed: %v\noutput:\n%s", prefix, err, out)
	}

	// 1. Binaries installed and executable.
	for _, name := range []string{"orion", "oriond", "orion-seccomp"} {
		p := filepath.Join(prefix, "bin", name)
		st, err := os.Stat(p)
		if err != nil {
			t.Errorf("installed binary missing: %s: %v", p, err)
			continue
		}
		if st.Mode().Perm()&0o111 == 0 {
			t.Errorf("installed binary not executable: %s (mode %v)", p, st.Mode())
		}
	}
	// The installed orion binary must at least report its version.
	ver := exec.Command(filepath.Join(prefix, "bin", "orion"), "version")
	ver.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	verOut, err := ver.CombinedOutput()
	if err != nil {
		t.Errorf("installed orion version failed: %v\noutput:\n%s", err, verOut)
	} else if len(strings.TrimSpace(string(verOut))) == 0 {
		t.Errorf("installed orion version printed nothing")
	}

	// 2. The shipped USER unit is installed under the scratch
	// HOME with ExecStart rewritten to the binary we installed.
	unitPath := filepath.Join(home, ".config", "systemd", "user", "oriond.service")
	raw, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("installed unit missing at %s: %v", unitPath, err)
	}
	if st, err := os.Stat(unitPath); err == nil && st.Mode().Perm() != 0o644 {
		t.Errorf("installed unit mode = %v, want 0644", st.Mode().Perm())
	}
	unit := string(raw)
	if !strings.Contains(unit, "ExecStart="+filepath.Join(prefix, "bin", "oriond")) {
		t.Errorf("installed unit ExecStart was not rewritten to the install dir %s/bin/oriond:\n%s", prefix, unit)
	}
	if strings.Contains(unit, "%h/.local/bin/oriond") {
		t.Errorf("installed unit still has the default ExecStart (not rewritten to the install dir):\n%s", unit)
	}
	if !strings.Contains(unit, "Restart=always") {
		t.Errorf("installed unit lost Restart=always (F-A16-1):\n%s", unit)
	}
	if strings.Contains(unit, "hatch-egress-proxy") {
		t.Errorf("installed unit is the live VM unit, not the shipped user unit (contains the VM proxy hostname):\n%s", unit)
	}
	if strings.Contains(unit, "User=root") {
		t.Errorf("installed unit is a system unit (User=root), not the shipped user unit:\n%s", unit)
	}

	// 3. systemctl was asked to reload AND enable --now (not
	// bare enable — F-A16-6), and linger was attempted.
	stubRaw, err := os.ReadFile(stubLog)
	if err != nil {
		t.Fatalf("stub log missing at %s: %v", stubLog, err)
	}
	stub := string(stubRaw)
	if !strings.Contains(stub, "systemctl --user daemon-reload") {
		t.Errorf("systemctl was not asked to daemon-reload; stub log:\n%s", stub)
	}
	if !strings.Contains(stub, "systemctl --user enable --now oriond.service") {
		t.Errorf("systemctl was not asked to enable --now (bare enable leaves the bot not running); stub log:\n%s", stub)
	}
	if !strings.Contains(stub, "loginctl enable-linger testuser") {
		t.Errorf("loginctl was not asked to enable linger; stub log:\n%s", stub)
	}

	// 4. The pre-existing config was left untouched.
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("config vanished: %v", err)
	}
	if string(got) != sentinel {
		t.Errorf("existing config was overwritten (sentinel lost):\n%s", got)
	}

	// 5. The real HOME's unit file is untouched by the run.
	nowExists, nowContent := installTestSnapshot(t, realUnit)
	if nowExists != realExists {
		t.Errorf("real HOME unit %s existence changed (was %v, now %v) — install.sh touched the real HOME", realUnit, realExists, nowExists)
	} else if realExists && string(nowContent) != string(realContent) {
		t.Errorf("real HOME unit %s was modified by the run", realUnit)
	}
}
