# Capability matrix — why `CapabilityBoundingSet` is omitted (F-A18-12)

## What this proves

The root-run oriond unit deliberately omits `CapabilityBoundingSet`.
Any restriction of the bounding set breaks the sandbox's
`unshare --user --map-root-user` uid_map write, which would silently
break every sandboxed tool call. The gate is **CAP_SYS_ADMIN +
CAP_SETFCAP (both required)** — **CAP_SETUID is NOT the gate**:

- `{SETUID, SETGID, SYS_ADMIN}` FAILS, with CAP_SETUID present.
- `{SYS_ADMIN, SETFCAP}` PASSES, with CAP_SETUID (and SETGID) absent.
- `{SYS_ADMIN}` alone FAILS and `{SETFCAP}` alone FAILS, so both
  halves of the gate are required.

The rc23 comment blamed CAP_SETUID; that reason was wrong (the
omission itself was correct). F-A18-12 corrected the reason in rc24.

## Artifacts

- `probe_capability_matrix.py` — the probe. For each of 9 variants it
  forks, drops every capability except the variant's keep-set from the
  child's bounding set with prctl `PR_CAPBSET_DROP` (the same operation
  systemd's `CapabilityBoundingSet` performs), then execs bare
  `unshare --user --map-root-user true` and records pass/fail. It exits
  non-zero if any variant disagrees with its expected outcome.
  Run as root: `python3 docs/capability-matrix/probe_capability_matrix.py`
- `matrix-2026-11-14.log` — the real output of a run of the probe on
  this VM (2026-11-14), reproduced from the rc23 9-unit systemd matrix.

## Honest limits

- **Bare `unshare` only.** The probe tests the uid_map write in
  isolation, not the full sandbox wrapper (seccomp, mount namespace,
  orion-seccomp). The wrapper can only be *more* restrictive, never
  less — so a variant that fails here also fails there, but a variant
  that passes here is not a guarantee for the wrapper.
- **Ambient capabilities not tested.** The rc23 matrix tried
  `AmbientCapabilities=CAP_SETUID CAP_SETGID` and it still failed;
  this probe does not cover ambient sets at all.
- **Run as root in this VM's nested user namespace**, not under the
  laptop's non-root user unit. The result is about the bounding-set
  gate, not about the unit file as a whole. The systemd matrix (9
  scratch oneshot units, rc23) remains the authority for the unit
  itself; this probe is the bisection reproduction.
- The probe drops capabilities in forked children only; the probe
  process itself keeps its bounding set.
