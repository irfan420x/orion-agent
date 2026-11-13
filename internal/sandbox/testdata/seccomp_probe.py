#!/usr/bin/env python3
"""Live seccomp probe for the Orion sandbox.

Calls a set of escape-relevant syscalls directly (via libc.syscall)
with arguments that are safe even when the call is NOT blocked, and
prints one line per syscall:

    <name> BLOCKED              (errno is EPERM — or ENOSYS for
                                 clone3, which the filter denies with
                                 ENOSYS so glibc falls back to clone)
    <name> UNBLOCKED <detail>   (anything else: the call got through)

Safety: every probe is side-effect-free when unblocked.
  - mount/umount2/pivot_root/chroot/swapon/swapoff/open_tree use
    nonexistent paths (ENOENT when unblocked, never a real mount).
  - fsopen/fsconfig/fsmount/fspick/open_tree/move_mount/mount_setattr,
    kexec_load/kexec_file_load/init_module/finit_module,
    bpf/perf_event_open, add_key/request_key, seccomp, userfaultfd,
    clock_settime/settimeofday/adjtimex/clock_adjtime, syslog,
    process_vm_readv/writev, open_by_handle_at/name_to_handle_at
    use NULL pointers or invalid fds (EFAULT/EBADF/EINVAL when
    unblocked).
  - unshare(CLONE_NEWNS) succeeds when unblocked, but only creates a
    namespace for this throwaway process.
  - ptrace(PEEKDATA) on self returns ESRCH when unblocked (a process
    cannot trace itself).
  - keyctl(GET_KEYRING_ID, 0) returns EINVAL when unblocked.
  - clone3(NULL) returns EFAULT when unblocked.
  - io_uring_setup(2 entries, zeroed params) creates a real ring
    when unblocked (the process exits right after); enter/register
    get fd -1 (EBADF when unblocked). io_uring is already blocked
    by the cell filter on this host, so the host calibration reads
    BLOCKED for these, like swapon/bpf — the discriminating proof
    is TestSeccompFilterProgram, not the live probe.
  - modify_ldt(0, buf, 64) reads the LDT (returns 0 when unblocked).

Deliberately NOT probed here (see the Go test's probeSkipped):
reboot, sethostname, setdomainname (host side effects if the filter
failed), iopl/ioperm, fanotify_init, acct (fail with EPERM even
unfiltered: they need a capability in the initial user namespace),
quotactl, vhangup (no safe unambiguous probe).
"""

import ctypes
import os
import platform
import sys

# Syscall numbers from the kernel headers (asm/unistd_64.h for
# x86_64, asm-generic/unistd.h for aarch64). These must match the
# tables in internal/sandbox/seccomp.go; the Go test cross-checks
# the two by asserting the probe output covers exactly the table
# entries (minus the skipped ones).
TABLES = {
    "x86_64": {
        "mount": 165, "umount2": 166, "pivot_root": 155, "chroot": 161,
        "fsopen": 430, "fsconfig": 431, "fsmount": 432, "fspick": 433,
        "open_tree": 428, "move_mount": 429, "mount_setattr": 442,
        "swapon": 167, "swapoff": 168,
        "kexec_load": 246, "kexec_file_load": 320,
        "init_module": 175, "finit_module": 313, "delete_module": 176,
        "keyctl": 250, "add_key": 248, "request_key": 249,
        "ptrace": 101, "bpf": 321, "perf_event_open": 298,
        "userfaultfd": 323, "seccomp": 317,
        "open_by_handle_at": 304, "name_to_handle_at": 303,
        "setns": 308, "unshare": 272, "clone3": 435,
        "clock_settime": 227, "settimeofday": 164, "adjtimex": 159,
        "clock_adjtime": 305, "syslog": 103,
        "process_vm_readv": 310, "process_vm_writev": 311,
        "io_uring_setup": 425, "io_uring_enter": 426,
        "io_uring_register": 427, "modify_ldt": 154,
    },
    "aarch64": {
        "mount": 40, "umount2": 39, "pivot_root": 41, "chroot": 51,
        "fsopen": 430, "fsconfig": 431, "fsmount": 432, "fspick": 433,
        "open_tree": 428, "move_mount": 429, "mount_setattr": 442,
        "swapon": 224, "swapoff": 225,
        "kexec_load": 104, "kexec_file_load": 294,
        "init_module": 105, "finit_module": 273, "delete_module": 106,
        "keyctl": 219, "add_key": 217, "request_key": 218,
        "ptrace": 117, "bpf": 280, "perf_event_open": 241,
        "userfaultfd": 282, "seccomp": 277,
        "open_by_handle_at": 265, "name_to_handle_at": 264,
        "setns": 268, "unshare": 97, "clone3": 435,
        "clock_settime": 112, "settimeofday": 170, "adjtimex": 171,
        "clock_adjtime": 266, "syslog": 116,
        "process_vm_readv": 270, "process_vm_writev": 271,
        "io_uring_setup": 425, "io_uring_enter": 426,
        "io_uring_register": 427,
    },
}

EPERM = 1
ENOSYS = 38
CLONE_NEWNS = 0x00020000
MISSING = b"/nonexistent-orion-probe"

# Syscalls denied with ENOSYS instead of EPERM: clone3 returns
# ENOSYS so glibc's posix_spawn (and clone fallback paths) retry
# with clone(2); EPERM would break them. The filter's ENOSYS is
# the blocked signal for these; anything else is unblocked.
ENOSYS_BLOCKED = {"clone3"}


def main():
    machine = platform.machine()
    if machine == "arm64":
        machine = "aarch64"
    table = TABLES.get(machine)
    if table is None:
        print("UNSUPPORTED-ARCH %s" % machine)
        return 0

    libc = ctypes.CDLL("libc.so.6", use_errno=True)

    def call(nr, *args):
        ctypes.set_errno(0)
        ret = libc.syscall(nr, *args)
        return ret, ctypes.get_errno()

    # Each probe: (name, args). All arguments are chosen so the call
    # is harmless when unblocked (see the module docstring).
    probes = [
        ("mount", (b"none", MISSING, b"tmpfs", 0, b"")),
        ("umount2", (MISSING, 0)),
        ("pivot_root", (b"/nonexistent-a", b"/nonexistent-b")),
        ("chroot", (MISSING,)),
        ("fsopen", (None, 0)),
        ("fsconfig", (-1, 0, None, None, 0)),
        ("fsmount", (-1, 0, 0)),
        ("fspick", (-1, None, 0)),
        ("open_tree", (-100, MISSING, 0)),
        ("move_mount", (-1, b"", -1, b"", 0)),
        ("mount_setattr", (-100, MISSING, 0, None, 0)),
        ("swapon", (MISSING, 0)),
        ("swapoff", (MISSING,)),
        ("kexec_load", (0, 0, None, 0)),
        ("kexec_file_load", (-1, -1, None, 0, 0)),
        ("init_module", (None, 1)),
        ("finit_module", (-1, None, 0)),
        ("delete_module", (b"nonexistent-orion-module", 0)),
        ("keyctl", (0, 0, 0, 0, 0)),
        ("add_key", (b"user", b"probe", None, 1, 0)),
        ("request_key", (b"user", b"probe", None, 0)),
        ("ptrace", (2, os.getpid(), None, None)),
        ("bpf", (5, None, 0)),
        ("perf_event_open", (None, 0, -1, -1, 0)),
        ("userfaultfd", (0,)),
        ("seccomp", (1, 0, None)),
        ("open_by_handle_at", (-100, None, 0)),
        ("name_to_handle_at", (-100, MISSING, None, None, 0)),
        ("setns", (-1, 0)),
        ("unshare", (CLONE_NEWNS,)),
        ("clone3", (None, 0)),
        ("clock_settime", (0, None)),
        ("settimeofday", (None, None)),
        ("adjtimex", (None,)),
        ("clock_adjtime", (0, None)),
        ("syslog", (3, None, 0)),
        ("process_vm_readv", (os.getpid(), None, 1, None, 0, 0)),
        ("process_vm_writev", (os.getpid(), None, 1, None, 0, 0)),
        ("io_uring_setup", (2, ctypes.create_string_buffer(256))),
        ("io_uring_enter", (-1, 0, 0, 0, None, 0)),
        ("io_uring_register", (-1, 0, None, 0)),
    ]
    if "modify_ldt" in table:
        probes.append(("modify_ldt", (0, ctypes.create_string_buffer(64), 64)))

    for name, args in probes:
        nr = table[name]
        ret, errno = call(nr, *args)
        blocked_errno = ENOSYS if name in ENOSYS_BLOCKED else EPERM
        if ret == -1 and errno == blocked_errno:
            print("%s BLOCKED" % name)
        else:
            print("%s UNBLOCKED ret=%d errno=%d" % (name, ret, errno))
    return 0


if __name__ == "__main__":
    sys.exit(main())
