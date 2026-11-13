#!/usr/bin/env python3
"""Adversarial syscall probe for the Orion sandbox (Audit 3).

Probes the dangerous syscalls the main probe does NOT cover, with
arguments that are safe even when the call is NOT blocked:

  - io_uring_setup / io_uring_enter / io_uring_register: the
    io_uring kernel attack surface. setup gets a zeroed params
    buffer (a real ring if unblocked — the process exits right
    after); enter/register get fd -1 (EBADF when unblocked).
  - modify_ldt: read (func 0) of the LDT into a scratch buffer
    (returns 0 when unblocked).
  - pidfd_getfd: pidfd_open(1) then pidfd_getfd(pidfd, 1) — tries
    to duplicate a file descriptor of the namespace init. This is
    NOT in the seccomp blocklist, so it is expected to SUCCEED;
    the assertion is about WHAT it can steal: the duplicated fd
    must be a pipe or /dev/null (the namespace init's stdio),
    never a host regular file. A host process cannot be targeted
    at all: pidfd_open of a host PID fails, because host PIDs do
    not exist in the namespace.

Prints one line per probe:

    <name> BLOCKED              (errno is EPERM)
    <name> UNBLOCKED ret=.. errno=..
    pidfd_getfd <outcome> target=<readlink of the dup, if any>
"""

import ctypes
import os
import platform
import sys

TABLES = {
    "x86_64": {
        "io_uring_setup": 425, "io_uring_enter": 426,
        "io_uring_register": 427, "modify_ldt": 154,
        "pidfd_open": 434, "pidfd_getfd": 438,
    },
    "aarch64": {
        "io_uring_setup": 425, "io_uring_enter": 426,
        "io_uring_register": 427,
        "pidfd_open": 434, "pidfd_getfd": 438,
    },
}

EPERM = 1


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

    def report(name, ret, errno):
        if ret == -1 and errno == EPERM:
            print("%s BLOCKED" % name)
        else:
            print("%s UNBLOCKED ret=%d errno=%d" % (name, ret, errno))

    params = ctypes.create_string_buffer(256)
    report("io_uring_setup", *call(table["io_uring_setup"], 2, params))
    report("io_uring_enter", *call(table["io_uring_enter"], -1, 0, 0, 0, None, 0))
    report("io_uring_register", *call(table["io_uring_register"], -1, 0, None, 0))
    if "modify_ldt" in table:
        buf = ctypes.create_string_buffer(64)
        report("modify_ldt", *call(table["modify_ldt"], 0, buf, 64))

    # pidfd_getfd: not blocklisted (see the Go test for why that is
    # acceptable). Steal fd 1 of PID 1 and report what it is.
    pidfd, errno = call(table["pidfd_open"], 1, 0)
    if pidfd < 0:
        print("pidfd_getfd PIDFD-FAIL errno=%d" % errno)
        return 0
    dup, errno = call(table["pidfd_getfd"], pidfd, 1, 0)
    if dup < 0:
        print("pidfd_getfd FAIL errno=%d" % errno)
        return 0
    try:
        target = os.readlink("/proc/self/fd/%d" % dup)
    except OSError as e:
        target = "readlink-failed:%s" % e
    print("pidfd_getfd OK target=%s" % target)
    return 0


if __name__ == "__main__":
    sys.exit(main())
