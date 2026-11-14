package sandbox

import (
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

// TestSelectOrderingFallsBackOnAnyFailure pins the deliberate
// deviation from an EPERM-only fallback: a wrapper-stage failure
// (e.g. the /proc mount, exit 126) is ordering-dependent too, so
// the fallback is tried on ANY first-ordering failure. The
// fail-closed outcome is unchanged — both failing still refuses.
func TestSelectOrderingFallsBackOnAnyFailure(t *testing.T) {
	o, err := selectOrdering(func(ord sandboxOrdering) error {
		if ord == orderingAfterClone {
			return errors.New("sandbox: probe command failed (userns-after-clone): exit status 126")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("selectOrdering with a non-EPERM first failure: %v", err)
	}
	if o != orderingAtClone {
		t.Errorf("selected ordering = %v, want userns-at-clone", o)
	}
}

// TestSelectOrderingBothFailNamesBoth proves fail-closed with
// diagnostics: when neither ordering works the error names both
// failures, so the operator can see which ordering died where.
func TestSelectOrderingBothFailNamesBoth(t *testing.T) {
	_, err := selectOrdering(func(ord sandboxOrdering) error {
		return fmt.Errorf("ordering %s failed", ord)
	})
	if err == nil {
		t.Fatal("selectOrdering with both orderings failing: want error, got nil")
	}
	for _, want := range []string{"userns-after-clone", "userns-at-clone"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("both-fail error %q does not name %q", err, want)
		}
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
