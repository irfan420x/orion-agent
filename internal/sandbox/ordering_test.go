package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// F-A21-3 (probed dual-ordering userns-at-clone): the probe tries
// the current ordering first and falls back to the userns-at-clone
// ordering when it fails, caching whichever works. These tests
// drive that decision with stub probes (no live host needed) plus
// unit-level checks that Prepare builds each ordering correctly.
// The live behaviour on each host is covered by
// TestProbeOrderingIsKnown below and by the laptop live-verify.

// TestSelectOrderingPrefersAfterClone proves the VM path is
// preserved: when the current ordering works, the fallback is never
// even probed.
func TestSelectOrderingPrefersAfterClone(t *testing.T) {
	probed := []sandboxOrdering{}
	o, err := selectOrdering(func(ord sandboxOrdering) error {
		probed = append(probed, ord)
		return nil
	})
	if err != nil {
		t.Fatalf("selectOrdering with a working first ordering: %v", err)
	}
	if o != orderingAfterClone {
		t.Errorf("selected ordering = %v, want userns-after-clone", o)
	}
	if len(probed) != 1 || probed[0] != orderingAfterClone {
		t.Errorf("probed orderings = %v, want only [userns-after-clone]", probed)
	}
}

// TestSelectOrderingFallsBackToAtCloneOnEPERM is the laptop case:
// the current ordering fails with EPERM (an unprivileged user has
// no CAP_SYS_ADMIN for the clone) and the userns-at-clone ordering
// works — it must be selected.
func TestSelectOrderingFallsBackToAtCloneOnEPERM(t *testing.T) {
	probed := []sandboxOrdering{}
	o, err := selectOrdering(func(ord sandboxOrdering) error {
		probed = append(probed, ord)
		if ord == orderingAfterClone {
			return fmt.Errorf("fork/exec /bin/sh: %w", syscall.EPERM)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("selectOrdering with EPERM first / working fallback: %v", err)
	}
	if o != orderingAtClone {
		t.Errorf("selected ordering = %v, want userns-at-clone", o)
	}
	if len(probed) != 2 || probed[0] != orderingAfterClone || probed[1] != orderingAtClone {
		t.Errorf("probed orderings = %v, want [userns-after-clone userns-at-clone]", probed)
	}
}

// TestSelectOrderingFallsBackOnExit126 pins the second
// ordering-dependent failure (F-A22-13): a wrapper-stage failure
// (e.g. the /proc mount, exit 126) is worth a retry on the other
// ordering, so the fallback is tried. This used to pin the
// deliberate ANY-failure fallback
// (TestSelectOrderingFallsBackOnAnyFailure); the fallback is now
// narrowed to EPERM + exit 126, and non-ordering failures must
// NOT retry (see
// TestSelectOrderingSkipsFallbackOnOrderingIndependentFailure).
// The fail-closed outcome is unchanged — both failing still
// refuses. The probe stub runs a real command exiting 126, so
// the error is a genuine *exec.ExitError, not a string that
// merely mentions the code.
func TestSelectOrderingFallsBackOnExit126(t *testing.T) {
	o, err := selectOrdering(func(ord sandboxOrdering) error {
		if ord == orderingAfterClone {
			return exec.Command("/bin/sh", "-c", "exit 126").Run()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("selectOrdering with an exit-126 first failure: %v", err)
	}
	if o != orderingAtClone {
		t.Errorf("selected ordering = %v, want userns-at-clone", o)
	}
}

// TestSelectOrderingBothFailNamesBoth proves fail-closed with
// diagnostics: when neither ordering works the error names both
// failures, so the operator can see which ordering died where.
// The first failure is EPERM (ordering-dependent), so the
// fallback IS attempted — a non-ordering first failure would
// skip it (see
// TestSelectOrderingSkipsFallbackOnOrderingIndependentFailure)
// and only the first failure would be named.
func TestSelectOrderingBothFailNamesBoth(t *testing.T) {
	probed := []sandboxOrdering{}
	_, err := selectOrdering(func(ord sandboxOrdering) error {
		probed = append(probed, ord)
		return fmt.Errorf("ordering %s failed: %w", ord, syscall.EPERM)
	})
	if err == nil {
		t.Fatal("selectOrdering with both orderings failing: want error, got nil")
	}
	if len(probed) != 2 {
		t.Fatalf("probed orderings = %v, want both (an EPERM first failure must attempt the fallback)", probed)
	}
	for _, want := range []string{"userns-after-clone", "userns-at-clone"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("both-fail error %q does not name %q", err, want)
		}
	}
}

// TestSelectOrderingSkipsFallbackOnOrderingIndependentFailure pins
// the F-A22-13 narrowing: a first failure that cannot differ
// between orderings must NOT trigger a second probe — the other
// ordering would fail the same way, and retrying would only burn
// a second (up to 15s) probe and bury the real cause under
// "neither namespace ordering works". The error must say no
// fallback was attempted, so the operator sees the real cause.
func TestSelectOrderingSkipsFallbackOnOrderingIndependentFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{
			"probe temp dir failure",
			fmt.Errorf("sandbox: probe temp dir: %w", syscall.ENOSPC),
		},
		{
			"missing /bin/sh",
			&os.PathError{Op: "fork/exec", Path: "/bin/sh", Err: syscall.ENOENT},
		},
		{
			"probe timeout",
			fmt.Errorf("sandbox: probe command failed (userns-after-clone): %w", context.DeadlineExceeded),
		},
		{
			"generic failure",
			errors.New("sandbox: probe setup (userns-after-clone): boom"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probed := []sandboxOrdering{}
			_, err := selectOrdering(func(ord sandboxOrdering) error {
				probed = append(probed, ord)
				return tc.err
			})
			if err == nil {
				t.Fatal("selectOrdering with an ordering-independent first failure: want error, got nil")
			}
			if len(probed) != 1 || probed[0] != orderingAfterClone {
				t.Fatalf("probed orderings = %v, want only [userns-after-clone] (no pointless retry)", probed)
			}
			if !strings.Contains(err.Error(), "no fallback attempted") {
				t.Fatalf("error %q does not say no fallback was attempted", err)
			}
			if strings.Contains(err.Error(), "neither namespace ordering works") {
				t.Fatalf("error %q blames both orderings, want it to name the ordering-independent cause", err)
			}
		})
	}
}

// TestPrepareAtCloneRequestsUserNamespace is the unit-level half of
// the fallback ordering: Prepare with userns-at-clone must ask the
// clone for a user namespace (CLONE_NEWUSER) plus uid/gid mappings
// (agent uid/gid mapped to root inside), while still requesting the
// PID namespace. It is the mirror of
// TestPrepareRequestsPIDNamespace, which pins CLONE_NEWUSER NOT set
// for the default ordering — that test is unchanged and must keep
// passing.
func TestPrepareAtCloneRequestsUserNamespace(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "true")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cfg := Config{Enabled: true, Root: filepath.Join(t.TempDir(), "root"), DisableSeccomp: true}
	if err := cfg.prepareWithOrdering(cmd, "true", orderingAtClone); err != nil {
		t.Fatalf("prepareWithOrdering(userns-at-clone): %v", err)
	}
	attr := cmd.SysProcAttr
	if attr == nil {
		t.Fatal("prepareWithOrdering did not set SysProcAttr")
	}
	if attr.Cloneflags&syscall.CLONE_NEWPID == 0 {
		t.Error("Cloneflags lack CLONE_NEWPID: the sandbox would run without a PID namespace")
	}
	if attr.Cloneflags&syscall.CLONE_NEWUSER == 0 {
		t.Error("Cloneflags lack CLONE_NEWUSER: the userns-at-clone ordering must create the user namespace in the clone")
	}
	if len(attr.UidMappings) != 1 || attr.UidMappings[0] != (syscall.SysProcIDMap{ContainerID: 0, HostID: os.Getuid(), Size: 1}) {
		t.Errorf("UidMappings = %v, want [{ContainerID:0 HostID:%d Size:1}] (agent uid mapped to root)", attr.UidMappings, os.Getuid())
	}
	if len(attr.GidMappings) != 1 || attr.GidMappings[0] != (syscall.SysProcIDMap{ContainerID: 0, HostID: os.Getgid(), Size: 1}) {
		t.Errorf("GidMappings = %v, want [{ContainerID:0 HostID:%d Size:1}] (agent gid mapped to root)", attr.GidMappings, os.Getgid())
	}
}

// TestWrapperScriptForAtCloneSkipsLateUserNS proves the
// userns-at-clone wrapper never performs the late switch: the child
// is already root inside its own user namespace, so a second nested
// switch would only re-nest the mounts. The fresh mount namespace
// is kept (as a plain unshare --mount).
func TestWrapperScriptForAtCloneSkipsLateUserNS(t *testing.T) {
	script := wrapperScriptFor(orderingAtClone)
	if strings.Contains(script, "unshare --user") {
		t.Error("userns-at-clone wrapper still contains 'unshare --user': the late user-namespace switch must be skipped")
	}
	if !strings.Contains(script, "unshare --mount /bin/sh") {
		t.Error("userns-at-clone wrapper lost the fresh mount namespace (unshare --mount /bin/sh)")
	}
	if !strings.Contains(script, "unshare --mount /orion-seccomp") {
		t.Error("userns-at-clone wrapper lost the seccomp path (unshare --mount /orion-seccomp)")
	}
	if !strings.Contains(script, "already runs as root inside its own") {
		t.Error("userns-at-clone wrapper comment does not describe the at-clone state (stale after-clone comment?)")
	}
	if strings.Contains(script, "Drop into a fresh user namespace before any user code runs") {
		t.Error("userns-at-clone wrapper still carries the after-clone switch comment")
	}
}

// TestWrapperScriptForAfterCloneIsOriginal proves the default
// ordering's wrapper is byte-identical to the long-standing script:
// the dual-ordering change must not alter the VM path at all.
func TestWrapperScriptForAfterCloneIsOriginal(t *testing.T) {
	if got := wrapperScriptFor(orderingAfterClone); got != wrapperScript {
		t.Error("userns-after-clone wrapper differs from wrapperScript: the default ordering must be unchanged")
	}
}

// TestProbeOrderingIsKnown is the live half of the probe: on a host
// where the sandbox works, Ordering() names one of the two
// orderings (which one is host-dependent — this VM selects
// userns-after-clone, the laptop should select userns-at-clone),
// and Available() agrees with it. On a host without a sandbox it
// skips instead of asserting a mechanism that cannot exist there.
func TestProbeOrderingIsKnown(t *testing.T) {
	if !Available() {
		t.Skipf("sandbox unavailable on this host: %v", AvailabilityError())
	}
	o := Ordering()
	t.Logf("sandbox available, probe selected ordering %q (uid=%d)", o, os.Getuid())
	if o != "userns-after-clone" && o != "userns-at-clone" {
		t.Errorf("Ordering() = %q, want userns-after-clone or userns-at-clone", o)
	}
}
