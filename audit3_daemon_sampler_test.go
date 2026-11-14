// audit3_daemon_sampler_test.go — F-A18-3 (rc24): pin the sampler's
// PID resolution. Audit 18 found resolve_pid() asked ONLY the user
// manager (--user), but the live daemon is a SYSTEM unit, so the
// sampler spun in its retry loop forever writing nothing (CSV had
// no rows since 03:04:38Z). These tests run the real script in
// follow mode with a stub systemctl on PATH, so they never touch
// the real systemd, the real daemon, or the live CSV.
//
// The stub answers `systemctl show oriond -p MainPID --value`
// from $ORION_TEST_SYSTEM_PID and the --user variant from
// $ORION_TEST_USER_PID (or fails it when $ORION_TEST_USER_FAIL is
// set, mimicking root's user manager having no bus). The sampled
// PIDs are real live processes (the test process itself and a
// `sleep` child), because the script reads /proc/<pid>/status.
//
// What this test does NOT prove: the both-managers-empty case —
// in follow mode the script then retries forever by design, so
// there is no terminating run to assert on; and that the live
// sampler service actually picks up the fix (that needs the
// service restarted, which is a live-deployment step, not a test).
package orion_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// samplerTestStubSystemctl writes a stub systemctl that answers
// MainPID queries from the environment (see the file header) and
// logs every invocation to $ORION_TEST_STUB_LOG.
func samplerTestStubSystemctl(t *testing.T, dir string) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"echo \"systemctl $*\" >> \"$ORION_TEST_STUB_LOG\"\n" +
		"user=\"\"\n" +
		"for a in \"$@\"; do\n" +
		"  if [ \"$a\" = \"--user\" ]; then user=1; fi\n" +
		"done\n" +
		"if [ -n \"$user\" ]; then\n" +
		"  if [ -n \"$ORION_TEST_USER_FAIL\" ]; then\n" +
		"    echo \"Failed to connect to bus: No medium found\" >&2\n" +
		"    exit 1\n" +
		"  fi\n" +
		"  printf '%s\\n' \"${ORION_TEST_USER_PID:-}\"\n" +
		"else\n" +
		"  printf '%s\\n' \"${ORION_TEST_SYSTEM_PID:-}\"\n" +
		"fi\n" +
		"exit 0\n"
	path := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub systemctl: %v", err)
	}
}

// samplerTestRun runs the real sampler script once (follow mode,
// one sample) with the stub systemctl and scratch outputs, and
// returns the CSV row fields and the script's combined output.
func samplerTestRun(t *testing.T, scratch, stubBin string, envExtra []string) (fields []string, combined string) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	repoRoot := installTestRepoRoot(t)
	script := filepath.Join(repoRoot, "audit3-daemon-sampler.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("audit3-daemon-sampler.sh not found at %s: %v", script, err)
	}

	home := filepath.Join(scratch, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(scratch, "measurements.md")
	csv := filepath.Join(scratch, "samples.csv")
	stubLog := filepath.Join(scratch, "stub.log")

	env := []string{
		"HOME=" + home,
		"PATH=" + stubBin + ":" + os.Getenv("PATH"),
		"ORION_TEST_STUB_LOG=" + stubLog,
		"SAMPLER_OUT=" + out,
		"SAMPLER_CSV=" + csv,
		// Zero sleeps: the run must finish in ~1s, not after
		// the production 300s inter-sample sleep.
		"SAMPLER_SLEEP=0",
		"SAMPLER_RETRY_SLEEP=0",
	}
	env = append(env, envExtra...)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bash, script, "follow", "testlabel", "1")
	cmd.Dir = repoRoot
	cmd.Env = env
	combinedBytes, err := cmd.CombinedOutput()
	combined = string(combinedBytes)
	if err != nil {
		t.Fatalf("sampler follow run failed: %v\noutput:\n%s", err, combined)
	}

	raw, err := os.ReadFile(csv)
	if err != nil {
		t.Fatalf("CSV missing after run (no sample written?): %v\noutput:\n%s", err, combined)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("want exactly 1 CSV row, got %d:\n%s\noutput:\n%s", len(lines), raw, combined)
	}
	fields = strings.Split(lines[0], ",")
	if len(fields) != 11 {
		t.Fatalf("want 11 CSV fields, got %d in row %q", len(fields), lines[0])
	}
	if fields[1] != "testlabel" {
		t.Errorf("CSV label = %q, want %q (row %q)", fields[1], "testlabel", lines[0])
	}
	return fields, combined
}

func TestSamplerResolvePidSystemFirst(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("audit3-daemon-sampler.sh is a bash script; not runnable on Windows")
	}
	// A second live PID, distinct from the test process PID, so
	// the system-vs-user choice is observable in the CSV.
	sleepCmd := exec.Command("sleep", "120")
	if err := sleepCmd.Start(); err != nil {
		t.Fatalf("start sleep child: %v", err)
	}
	defer func() {
		_ = sleepCmd.Process.Kill()
		_ = sleepCmd.Wait()
	}()
	userPID := strconv.Itoa(sleepCmd.Process.Pid)
	systemPID := strconv.Itoa(os.Getpid())

	scratch := t.TempDir()
	stubBin := filepath.Join(scratch, "stubbin")
	if err := os.MkdirAll(stubBin, 0o755); err != nil {
		t.Fatal(err)
	}
	samplerTestStubSystemctl(t, stubBin)

	cases := []struct {
		name       string
		systemPID  string
		userPID    string
		userFail   bool
		scope      string // "" = unset (default order)
		wantPID    string
		wantUser   bool // the --user manager must be consulted
		wantSystem bool // the system manager must be consulted
		wantWarn   bool // stderr must warn about the scope value
	}{
		{
			name:       "system-first: system PID wins, user manager never consulted",
			systemPID:  systemPID,
			userPID:    userPID,
			wantPID:    systemPID,
			wantSystem: true,
		},
		{
			name:       "system-first with user bus down (live F-A18-3 case)",
			systemPID:  systemPID,
			userPID:    userPID,
			userFail:   true,
			wantPID:    systemPID,
			wantSystem: true,
		},
		{
			name:       "fallback: empty system MainPID falls through to user",
			systemPID:  "",
			userPID:    userPID,
			wantPID:    userPID,
			wantUser:   true,
			wantSystem: true,
		},
		{
			name:       "fallback: system MainPID 0 (unit not running) falls through to user",
			systemPID:  "0",
			userPID:    userPID,
			wantPID:    userPID,
			wantUser:   true,
			wantSystem: true,
		},
		{
			name:      "scope=user pins the user manager even when system has a PID",
			systemPID: systemPID,
			userPID:   userPID,
			scope:     "user",
			wantPID:   userPID,
			wantUser:  true,
		},
		{
			// F-A19-4 (rc25): a PINNED scope returning "0" (unit
			// known but not running) falls back to the other
			// manager, like the unset-scope path — it must not
			// return "0" raw and leave the sampler spinning.
			name:       "scope=system with system MainPID 0 falls back to user",
			systemPID:  "0",
			userPID:    userPID,
			scope:      "system",
			wantPID:    userPID,
			wantUser:   true,
			wantSystem: true,
		},
		{
			name:       "scope=system with empty system MainPID falls back to user",
			systemPID:  "",
			userPID:    userPID,
			scope:      "system",
			wantPID:    userPID,
			wantUser:   true,
			wantSystem: true,
		},
		{
			name:       "scope=user with user MainPID 0 falls back to system",
			systemPID:  systemPID,
			userPID:    "0",
			scope:      "user",
			wantPID:    systemPID,
			wantUser:   true,
			wantSystem: true,
		},
		{
			name:       "scope=system pins the system manager even when user has a PID",
			systemPID:  systemPID,
			userPID:    userPID,
			scope:      "system",
			wantPID:    systemPID,
			wantSystem: true,
		},
		{
			name:       "invalid scope warns and fails open to system-first",
			systemPID:  systemPID,
			userPID:    userPID,
			scope:      "bogus",
			wantPID:    systemPID,
			wantSystem: true,
			wantWarn:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runDir := t.TempDir()
			extra := []string{
				"ORION_TEST_SYSTEM_PID=" + tc.systemPID,
				"ORION_TEST_USER_PID=" + tc.userPID,
			}
			if tc.userFail {
				extra = append(extra, "ORION_TEST_USER_FAIL=1")
			}
			if tc.scope != "" {
				extra = append(extra, "ORION_SYSTEMD_SCOPE="+tc.scope)
			}
			fields, combined := samplerTestRun(t, runDir, stubBin, extra)
			if fields[2] != tc.wantPID {
				t.Errorf("CSV PID = %s, want %s (system %s, user %s, scope %q)",
					fields[2], tc.wantPID, tc.systemPID, tc.userPID, tc.scope)
			}
			if tc.wantWarn && !strings.Contains(combined, "ignoring invalid ORION_SYSTEMD_SCOPE='bogus'") {
				t.Errorf("invalid scope produced no stderr warning; output:\n%s", combined)
			}

			stubRaw, err := os.ReadFile(filepath.Join(runDir, "stub.log"))
			if err != nil {
				t.Fatalf("stub log missing: %v", err)
			}
			stub := string(stubRaw)
			askedUser := strings.Contains(stub, "systemctl --user show oriond")
			askedSystem := false
			for _, line := range strings.Split(strings.TrimSpace(stub), "\n") {
				if line == "systemctl show oriond -p MainPID --value" {
					askedSystem = true
				}
			}
			if tc.wantUser && !askedUser {
				t.Errorf("user manager was not consulted; stub log:\n%s", stub)
			}
			if !tc.wantUser && askedUser {
				t.Errorf("user manager was consulted but must not be (scope %q); stub log:\n%s", tc.scope, stub)
			}
			if tc.wantSystem && !askedSystem {
				t.Errorf("system manager was not consulted; stub log:\n%s", stub)
			}
			if !tc.wantSystem && askedSystem {
				t.Errorf("system manager was consulted but must not be (scope %q); stub log:\n%s", tc.scope, stub)
			}
		})
	}
}

// TestSamplerServiceParameterised pins the F-A18-3 service-file
// change. The rc24 attempt parameterised the workspace with one
// Environment=ORION_WORKSPACE variable referenced as
// ${ORION_WORKSPACE} — but systemd 255 REJECTS that at load time
// in WorkingDirectory, ExecStart and StandardOutput ("path is not
// absolute" / "Neither a valid executable name nor an absolute
// path"), in both systemd-analyze verify AND the live manager
// (proven with a scratch transient unit, 2026-11-14). So the
// paths here are literal ON PURPOSE and the file must say so;
// the only F-A18-3 change that survives in the unit is
// ORION_SYSTEMD_SCOPE=system, which pins PID resolution to the
// system manager for this system-unit deployment.
func TestSamplerServiceParameterised(t *testing.T) {
	repoRoot := installTestRepoRoot(t)
	raw, err := os.ReadFile(filepath.Join(repoRoot, "systemd", "oriond-sampler.service"))
	if err != nil {
		t.Fatalf("sampler service not found: %v", err)
	}
	unit := string(raw)
	for _, want := range []string{
		"Environment=ORION_SYSTEMD_SCOPE=system",
		"WorkingDirectory=/home/hatch/workspace/orion-go",
		"ExecStart=/home/hatch/workspace/orion-go/audit3-daemon-sampler.sh follow auto 0",
		"StandardOutput=append:/home/hatch/workspace/orion-go/sampler-systemd.log",
		"StandardError=append:/home/hatch/workspace/orion-go/sampler-systemd.log",
		// The header must document WHY the paths are literal.
		"systemd 255 REJECTS that at load time",
		"Do NOT re-introduce ${...} path references here",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("sampler service missing %q:\n%s", want, unit)
		}
	}
	// The header comment may (and does) mention the rejected
	// ${ORION_WORKSPACE} attempt — only directive lines must be
	// free of it, since systemd 255 refuses to load a unit whose
	// directives reference it.
	for _, line := range strings.Split(unit, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if strings.Contains(trim, "${ORION_WORKSPACE}") {
			t.Errorf("sampler service directive %q references ${ORION_WORKSPACE} — systemd 255 refuses to load such a unit", trim)
		}
	}
	// The stale F-A17-7 claim must be gone from the script: the
	// user manager is now a fallback, not the sole mechanism.
	script, err := os.ReadFile(filepath.Join(repoRoot, "audit3-daemon-sampler.sh"))
	if err != nil {
		t.Fatalf("sampler script not found: %v", err)
	}
	if strings.Contains(string(script), "MUST ask the user manager") {
		t.Error("sampler script still carries the stale F-A17-7 claim that --user is required")
	}
	if !strings.Contains(string(script), "ORION_SYSTEMD_SCOPE") {
		t.Error("sampler script does not mention ORION_SYSTEMD_SCOPE")
	}
}
