#!/usr/bin/env python3
"""probe_capability_matrix.py — reproduce the CapabilityBoundingSet bisection (F-A18-12, rc24).

The rc23 systemd matrix (9 scratch oneshot units) showed that restricting
the bounding set of the root-run oriond unit breaks the sandbox's
`unshare --user --map-root-user` uid_map write, and that the gate is
CAP_SYS_ADMIN + CAP_SETFCAP (both required) — NOT CAP_SETUID, as the rc23
comment wrongly claimed. This probe reproduces that bisection without
systemd: for each variant it forks, drops every capability except the
variant's keep-set from the child's bounding set with prctl(2)
PR_CAPBSET_DROP (the same operation systemd's CapabilityBoundingSet
performs), then execs bare `unshare --user --map-root-user true` and
records pass/fail.

Run as root:  python3 docs/capability-matrix/probe_capability_matrix.py
Exit status is 0 only if every variant matches its expected outcome;
any disagreement fails loudly instead of being papered over.

Stdlib only. No secrets, no network, no writes outside stdout.
"""

import ctypes
import os
import platform
import shutil
import subprocess
import sys
from datetime import datetime, timezone

PR_CAPBSET_DROP = 24

# Linux capability numbers (linux/capability.h).
CAPS = {
    "CHOWN": 0, "DAC_OVERRIDE": 1, "DAC_READ_SEARCH": 2, "FOWNER": 3,
    "FSETID": 4, "KILL": 5, "SETGID": 6, "SETUID": 7, "SETPCAP": 8,
    "LINUX_IMMUTABLE": 9, "NET_BIND_SERVICE": 10, "NET_BROADCAST": 11,
    "NET_ADMIN": 12, "NET_RAW": 13, "IPC_LOCK": 14, "IPC_OWNER": 15,
    "SYS_MODULE": 16, "SYS_RAWIO": 17, "SYS_CHROOT": 18, "SYS_PTRACE": 19,
    "SYS_PACCT": 20, "SYS_ADMIN": 21, "SYS_BOOT": 22, "SYS_NICE": 23,
    "SYS_RESOURCE": 24, "SYS_TIME": 25, "SYS_TTY_CONFIG": 26, "MKNOD": 27,
    "LEASE": 28, "AUDIT_WRITE": 29, "AUDIT_CONTROL": 30, "SETFCAP": 31,
    "MAC_OVERRIDE": 32, "MAC_ADMIN": 33, "SYSLOG": 34, "WAKE_ALARM": 35,
    "BLOCK_SUSPEND": 36, "AUDIT_READ": 37, "PERFMON": 38, "BPF": 39,
    "CHECKPOINT_RESTORE": 40,
}
ALL_CAPS = set(CAPS.values())

# (label, keep-set, expected pass?) — order matters for the narrative:
# control, then the failures that exonerate CAP_SETUID, then the gate.
VARIANTS = [
    ("FULL (no drops, control)", None, True),
    ("{} (empty bounding set)", set(), False),
    ("{SETUID, SETGID}", {"SETUID", "SETGID"}, False),
    ("{SYS_ADMIN}", {"SYS_ADMIN"}, False),
    ("{SETUID, SETGID, SYS_ADMIN}", {"SETUID", "SETGID", "SYS_ADMIN"}, False),
    ("{SYS_ADMIN, SETFCAP}", {"SYS_ADMIN", "SETFCAP"}, True),
    ("{SETFCAP}", {"SETFCAP"}, False),
    ("{SETUID, SETGID, SYS_ADMIN, SETFCAP}", {"SETUID", "SETGID", "SYS_ADMIN", "SETFCAP"}, True),
    ("{SETUID, SYS_ADMIN, SETFCAP}", {"SETUID", "SYS_ADMIN", "SETFCAP"}, True),
]


def cap_bnd_mask() -> int:
    """Bounding-set mask of this process, from /proc/self/status."""
    with open("/proc/self/status") as f:
        for line in f:
            if line.startswith("CapBnd:"):
                return int(line.split(":", 1)[1].strip(), 16)
    raise RuntimeError("CapBnd not found in /proc/self/status")


def make_dropper(keep_names):
    """Return a preexec_fn dropping every cap except keep_names.

    Runs in the forked child, before exec. A failed drop is fatal to
    the child (exit 126) so a broken drop can never masquerade as a
    variant result.
    """
    keep = {CAPS[n] for n in keep_names} if keep_names is not None else ALL_CAPS

    def dropper():
        libc = ctypes.CDLL("libc.so.6", use_errno=True)
        for cap in sorted(ALL_CAPS - keep):
            if libc.prctl(PR_CAPBSET_DROP, cap, 0, 0, 0) != 0:
                err = ctypes.get_errno()
                os.write(2, ("probe: PR_CAPBSET_DROP cap %d failed errno=%d\n" % (cap, err)).encode())
                os._exit(126)
    return dropper


def main() -> int:
    print("capability-matrix probe (F-A18-12, rc24)")
    print("run at (UTC): %s" % datetime.now(timezone.utc).isoformat())
    print("kernel: %s" % platform.uname().release)
    print("uid=%d euid=%d" % (os.getuid(), os.geteuid()))
    unshare = shutil.which("unshare")
    print("unshare: %s" % unshare)
    if unshare is None:
        print("FATAL: unshare not on PATH; cannot probe.")
        return 2
    if os.geteuid() != 0:
        print("FATAL: must run as root (dropping bounding-set caps needs CAP_SETPCAP).")
        return 2
    bnd = cap_bnd_mask()
    print("starting CapBnd: 0x%016x" % bnd)
    for needed in ("SYS_ADMIN", "SETFCAP"):
        if not bnd & (1 << CAPS[needed]):
            print("FATAL: starting bounding set lacks CAP_%s; this host cannot run the bisection." % needed)
            return 2
    print("command per variant: unshare --user --map-root-user true")
    print()

    failures = []
    for label, keep, expected in VARIANTS:
        proc = subprocess.run(
            [unshare, "--user", "--map-root-user", "true"],
            capture_output=True, text=True,
            preexec_fn=make_dropper(keep),
        )
        passed = proc.returncode == 0
        verdict = "PASS" if passed else "FAIL"
        match = "as expected" if passed == expected else "UNEXPECTED"
        detail = ""
        if not passed:
            detail = " (%s)" % proc.stderr.strip().splitlines()[-1] if proc.stderr.strip() else " (exit %d)" % proc.returncode
        if proc.returncode == 126:
            detail = " (probe error: capability drop failed, see stderr)"
        print("[%s] %-42s expected=%s got=%s%s" % (match, label, "PASS" if expected else "FAIL", verdict, detail))
        if passed != expected:
            failures.append(label)

    print()
    print("summary: %d/%d variants matched expectation" % (len(VARIANTS) - len(failures), len(VARIANTS)))
    if failures:
        print("MISMATCHES (probe disagrees with the recorded gate):")
        for label in failures:
            print("  - %s" % label)
        print("Do NOT update the unit comment to match a failed probe; investigate first.")
        return 1
    print("verdict: the bisection holds on this host —")
    print("  * {SETUID, SETGID, SYS_ADMIN} FAILS with CAP_SETUID present,")
    print("  * {SYS_ADMIN, SETFCAP} PASSES with CAP_SETUID absent,")
    print("  * {SYS_ADMIN} alone and {SETFCAP} alone both FAIL,")
    print("  so the gate is CAP_SYS_ADMIN + CAP_SETFCAP (both required);")
    print("  CAP_SETUID is NOT the gate. CapabilityBoundingSet stays omitted.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
