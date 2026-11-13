# Audit 2 — Part: Security / Sandbox / Tools

Fresh-eyes audit of Project Orion, 2026-11-13. Scope only:

- `internal/security/` — security.go, scanner.go, audit.go, audit_key.go + tests
- `internal/sandbox/` — sandbox.go + tests (`seccomp.go` is a sibling-added file in the same package; reviewed only for deployment status, not edited)
- `internal/tools/` — tools.go, shell.go, files.go, web.go, misc.go, registry.go

Method: read every file in scope line by line; every finding below was confirmed empirically with temporary probe tests (44 denylist cases, kill/mount/account cases, a live double-chroot escape, a fifo hang, a head-failure corruption, env-leak and SSRF probes) before fixing. The old audit (`AUDIT_2026-11-13.md`) was spot-checked for the items touching these files: P0-1 denylist, P1-12 audit hash ambiguity, P2-16 scanner word boundaries, P2-17 audit modes all still hold; nothing regressed.

Baseline at start: tree green, 66 Go files, 14/14 packages pass. Sibling agents were editing the same tree concurrently (another audit-2 part, a seccomp-investigation phase); where a finding was fixed by the sibling's concurrent work it is marked as such.

No secrets appear in this report, in any test, or in any commit. No live destructive command was run in any test; denylist tests call `IsBlockedCommand` with strings only, sandbox escape tests target files inside a temp dir, never real system paths.

## P0 — fixed

### P0-1 Sandbox chroot escape (confirmed, fixed — pivot by sibling, verified by this audit)

`internal/sandbox/sandbox.go` (wrapper script, ~line 221–252)

A plain chroot is escapable by a process with CAP_SYS_CHROOT (uid 0 in its user namespace): chroot to a subdirectory while cwd is outside it, walk up past the old root, chroot again onto the host filesystem. Proven pre-fix with a probe: inside the sandbox, `python3 -c "import os; os.mkdir('/work/sub'); os.chroot('/work/sub'); [os.chdir('..') for _ in range(12)]; os.chroot('.'); os.chdir('/'); print(open('<host canary>').read())"` printed `CANARY:OUTSIDE-SECRET` — the process reached host `/` and read a host file (the agent runs as root, so full host fs access).

Fix (implemented by the sibling while this audit ran; verified end-to-end by this audit): the wrapper no longer chroots at all. It `cd`s into the new root, `pivot_root . .old-root`, `cd /`, then lazily unmounts the old root with `umount2` (flag 2 = MNT_DETACH) called directly via python3 ctypes — `umount(8)` cannot do it because libmount needs `/proc/mounts` and `/proc` is deliberately not mounted (proven by strace; `umount2` with MNT_FORCE returns EPERM, MNT_DETACH returns 0). If python3 or the arch (x86_64: 166, aarch64: 39) is unavailable the wrapper exits 126 and refuses to run, rather than leaving the host root mounted at `/.old-root`. An earlier setpriv capability-dropping plan was abandoned once the pivot was proven to work. The escape probe now fails: the walk-up stops at the new root and the canary is unreadable and untouched.

Regression tests: `TestSandboxChrootEscapeBlocked` and `TestSandboxNoDevTTY` in `internal/sandbox/sandbox_test.go:268,294`. The package doc (`sandbox.go:35–51`) still described the abandoned setpriv plan; corrected to describe the pivot in this audit (P2-10).

### P0-2 Denylist bypasses — 44 cases (confirmed, fixed)

`internal/security/security.go` — `IsBlockedCommand` (`:2160`), `blockedSegment` (`:1276`), `tokenize` (`:1195`)

All 44 probe cases returned `false` (allowed) before the fix. Classes, with the fix:

- **Wrappers / builtins** — `exec rm -rf /`, `exec /bin/sh`, `builtin exec rm -rf /`, `exec -a foo rm -rf /`, `chroot / rm -rf /`, `chroot / sh`, `chroot /` (no command: runs a shell), `watch rm -rf /`, `watch -n 1 rm -rf /`, `script -qec 'rm -rf /' /dev/null`. Fixed with new cases in `blockedSegment`: `builtin` strips one level and re-checks; `exec` skips `-a` and checks the rest recursively (`blockedExec`, `:521`); `chroot` skips `--userspec`/`--groups`, checks the command after NEWROOT, refuses with no command (`blockedChroot`, `:533`); `watch` skips `-n`/`--interval` and checks the rest (`blockedWatch`, `:547`); `script` requires `-c` and checks its argument, refuses without it (`blockedScript`, `:560`).
- **Namespace / privilege / remote wrappers not blocked** — `nsenter`, `unshare`, `systemd-run`, `setpriv`, `capsh`, `expect`, `socat`, `ssh`, `scp`, `sftp` added to `blockedBinaries`. (Binaries present on this VM, so the wrappers are live, not theoretical: python3, chroot, nsenter, unshare, systemd-run, watch, script, ssh, setpriv, capsh, socat, expect, bun. Absent: `at`, `pypy3`, `deno`.)
- **`env -S` / `--split-string`** — `env -S 'rm -rf /'`, `env --split-string='rm -rf /'`, `env -S'rm -rf /'` all slipped through because `resolveEnv` skipped the value without checking it. Fixed in `resolveEnv` (`:370`): the `-S`/`--split-string` value (attached, `=`-form, or separate token) is checked with `isBlockedCommand`. A value of only flags (e.g. `-S '-u FOO'`) is not a command and does not false-positive.
- **ANSI-C / locale quoting** — `$'rm' -rf /`, `$"rm" -rf /` tokenized as `"$rm"` and evaded the binary check. Fixed in `tokenize` (`:1195`): index-based loop, `$` before `'`/`"` is dropped as quoting syntax and the quote opens; `\$'` still produces a literal `$` (escaped). ANSI-C single quotes track backslash-as-escape (`$'a\'b'`).
- **Variable indirection** — `x=rm; $x -rf /`, `x=/bin/rm; $x -rf /`, `export x=rm; $x -rf /` all slipped through. Fixed: `IsBlockedCommand` now delegates to `isBlockedCommand(cmd, vars)`; assignments are collected per segment in order (`collectSegmentAssignments`, `:921`, including after `export`/`declare`/`local`/`readonly`/`typeset`/`let`) and threaded through every recursive check (`eval`, `su -c`, `xargs`, `find -exec`, `busybox`, `runuser`, `chroot`, `watch`, `script -c`, `exec`); a leading `$x`/`${x}` is spliced with its recorded value and re-checked (`varRef`, `:952`). Guarded against cyclic indirection (`x=$x`, max 8 resolutions). An expansion with no recorded assignment cannot be resolved statically and is left to the sandbox.
- **`find -exec` wrappers** — `find / -exec sudo rm {} \;` (also `env`/`nice`/`timeout 5` variants) slipped through: the recursive check allows a plain `rm` of one file, but `-exec` runs it once per file found. Fixed in `blockedFind` (`:794`) with `resolveExecBinary` (`:834`), which strips assignments/keywords/wrappers to the real binary and blocks it if it is in `blockedBinaries` or starts with `mkfs`.
- **File operations on system paths** — `cp /tmp/x /etc/passwd`, `cp -r /tmp/dir /etc/`, `mv /tmp/x /etc/hostname`, `ln -sf /tmp/x /etc/passwd`, `chown -R nobody /etc`, `chown root /etc/passwd`, `chgrp root /etc/passwd`, `mkdir -p /etc/evil` all slipped through. Fixed with `blockedFileOp` (`:684`): cp checks the destination (last operand / `-t`/`--target-directory`), mv checks every operand (moving a system file away destroys it too), ln checks the link path, chown/chgrp/mkdir/rmdir check every operand; all via `isDestructiveTarget`. `cp /tmp/a /tmp/b`, `mkdir -p /tmp/newdir`, `chown user /tmp/file` stay allowed.
- **Interpreter payloads** — `pypy3 -c "os.system('rm -rf /')"`, `deno eval "Deno.removeSync('/etc/passwd')"`, `bun -e "require('fs').rmSync('/tmp/x')"`, `python3 -c "os.chmod('/etc/passwd', 0o777)"`, `python3 -c "os.chown('/etc/passwd', 0, 0)"`. Fixed: `interpreterKind` (`:1525`) maps `pypy`/`micropython`/`jython` → python, `bun` → node, `deno` → deno; `blockedInterpreter` (`:1715`) handles `deno eval <code>` (skipping eval's flags, analysed with the node rules); `removeSync` added to the destructive APIs and a precise `\bDeno\s*\.\s*remove\b` pattern added (a bare `remove` is not added — Python `list.remove` would false-positive); `blockedInterpreterPayload` (`:2105`) gains `chmodsSystemPath` — a system-path string literal plus a chmod/chown call (incl. `chmodSync`/`chownSync`/`fchmod`/`fchown`, Perl `chmod 0777`) is refused. `python3 -c "os.chmod('/tmp/x', 0o755)"` stays allowed.

Tests: `TestAudit2WrapperBypasses`, `TestAudit2EnvSplitString`, `TestAudit2DollarQuoting`, `TestAudit2FileOpTargets`, `TestAudit2FindExecWrappers`, `TestAudit2VariableIndirection`, `TestAudit2InterpreterKinds` in `internal/security/audit2_fix_test.go`.

## P1 — fixed

### P1-1 Audit `Append` head-failure leaves stale in-memory state (confirmed, fixed)

`internal/security/audit.go` — `Append` (`:522`)

If `writeHead` fails (probe: replaced `<log>.head` with a directory), the entry lands in the log but `Append` returned before updating `lastSeq`/`lastHash`; the next `Append` reused seq 2 and `Verify` then failed ("expected seq 3, got 2") — the chain was permanently corrupted.

Fix: `readAndVerify` was split into `readLogEntries` (`:395`, chain verification only, no head check) plus the head check in `readAndVerify` (`:450`). On `writeHead` failure, `Append` now resyncs the in-memory tip from the log via `readLogEntries(false)` before returning the error, so the next `Append` uses the correct next sequence. If the resync itself fails, the tip is left stale and the next `Append` fails loudly on verification rather than silently reusing a sequence number.

Test: `TestAuditAppendHeadFailureResyncsTip` (`audit2_fix_test.go:255`).

### P1-2 Kill / mount / account administration unblocked (confirmed, fixed)

`internal/security/security.go` — `blockedBinaries`, `blockedKill` (`:624`), `blockedSegment` (`:1276`)

`kill -9 1`, `kill -9 -1`, `kill 1`, `killall -9 oriond`, `pkill -9 orion`, `mount /dev/sda1 /mnt`, `umount /`, `umount -a`, `losetup /dev/loop0 /tmp/x.img`, `swapon`, `swapoff -a`, `modprobe`, `insmod`, `rmmod`, `sysctl -w kernel.hostname=evil`, `mkswap`, `dmsetup remove_all`, `passwd`, `passwd root`, `chpasswd`, `useradd`, `userdel`, `usermod -aG sudo`, `groupadd`, `visudo`, `adduser` were all allowed.

Fix: `killall`/`pkill` and the mount/swap/module/sysctl binaries (`umount`, `swapon`, `swapoff`, `mkswap`, `dmsetup`, `modprobe`, `insmod`, `rmmod`, `sysctl`) and account binaries (`passwd`, `chpasswd`, `useradd`, `userdel`, `usermod`, `groupadd`, `groupdel`, `groupmod`, `visudo`, `adduser`, `deluser`) added to `blockedBinaries`. `mount`/`losetup` are handled specially in `blockedSegment` (bare, with no arguments, they only list mounts/devices — `mount | grep sda` stays allowed). `kill` gets `blockedKill`: only PID 1 or a negative PID (a whole process group; `-1` is every process) is refused; only ONE signal spec is consumed at the front, so `kill -9 -1` is refused (the second `-` token is a PID, not a signal); signal flags (`-9`, `-TERM`, `-s SIG`, `-n NUM`, `-q SIG VAL`, `-l [SIG]`) are skipped so a signal number is never mistaken for a PID; `%N` job specs are allowed. `kill -9 1234` stays allowed.

Test: `TestAudit2KillMountAccounts` (`audit2_fix_test.go:203`).

### P1-3 `file_read` on a fifo blocks forever (confirmed, fixed — approach changed during fixing)

`internal/tools/files.go` — `fileReadTool.Execute` (`:249`), `readFileBounded` (`:52`)

A fifo with no writer blocked `file_read` indefinitely (did not return in 5s): `os.ReadFile` does not watch the context, and there was no deadline on the read. `mkfifo` is not blocked by the denylist, so the model can create one and then read it.

Fix (approach changed from the first plan): the first plan — refuse non-regular files in `file_read` — was abandoned because the existing truncation tests (`TestFileReadMarksTruncationWhenFileGrows`, `TestFileReadTruncationDoesNotSplitUTF8` in `p2_fix_test.go`) deliberately read a fifo to simulate a file growing between Stat and ReadFile (a fifo's Stat size is 0 but it delivers 1MB+). Instead, `file_read` (and `file_edit`) now read via `readFileBounded`: the read runs in a goroutine, capped at `maxFileRead+1` bytes (one byte past the cap is enough to know the file is over it; this also bounds memory against a fifo that streams forever), and returns promptly on context cancellation or after `maxFileReadTimeout` (30s). A read abandoned this way leaves one goroutine blocked on the file — bounded by the number of such reads, far better than hanging the tool call. `file_edit` keeps the non-regular refusal (`file_edit` on a fifo returns `invalid_args`, "not a regular file") since editing a fifo makes no sense and it has no fifo tests.

Tests: `TestAudit2FileReadFifoWithoutWriterReturns` (returns in ~3s on a 3s context, with a timeout error), `TestAudit2FileEditRefusesNonRegular` (`internal/tools/audit2_fix_test.go:25,51`).

### P1-4 `scrubEnv` leaks credential-looking variables (confirmed, fixed)

`internal/tools/shell.go` — `scrubEnv` (`:38`)

Only `ORION_*`, `API_KEY`, `TOKEN`, `SECRET`, `PASSWORD`, `PASSWD` substrings were stripped. A probe with fake values showed `PRIVATE_KEY`, `SSH_KEY`, `MY_AUTH_HEADER`, `GITHUB_KEY` all reaching a shell child.

Fix: added `KEY`, `CREDENTIAL`, `AUTH`, `BEARER` substring matches.

Test: `TestAudit2ScrubEnvStripsCredentialVars` (`audit2_fix_test.go:72`).

### P1-5 Sandbox: `/work` bind lacked nodev/nosuid; `/dev/tty` bind-mounted; root not symlink-resolved (confirmed, fixed)

`internal/sandbox/sandbox.go`

- `/work` bind had no `nodev`/`nosuid`: a device node created in `/work` could be opened. Fixed: the wrapper remounts `/work` `rw,nodev,nosuid` after the bind (`sandbox.go:215`).
- `/dev/tty` was bind-mounted: TIOCSTI risk when the agent runs attached to a tty. Fixed: removed from `devAllowlist` (`sandbox.go:189`) and from the wrapper's device loop. Regression test `TestSandboxNoDevTTY` (`sandbox_test.go:294`) asserts `/dev/tty` absent while `/dev/null` and `/dev/urandom` still work.
- Sandbox root was not symlink-resolved: a root like `/tmp/link` → `/etc` passed lexical `Validate`. Fixed: `Prepare` resolves the root via `filepath.EvalSymlinks` and re-validates, and hands the resolved root to the wrapper (`sandbox.go:349`).
- Clone flags: added `CLONE_NEWIPC` (own SysV IPC namespace) and `CLONE_NEWUTS` (own hostname) (`sandbox.go:400`).

### P1-6 SSRF `isPublicIP` gaps (confirmed, fixed — with one deliberate exception)

`internal/tools/web.go` — `isPublicIP` (`:60`), `nonPublicIPNets` (`:83`)

`net.IP`'s own checks miss: 0.0.0.0/8, 100.64.0.0/10, 192.0.0.0/24, 198.18.0.0/15, 240.0.0.0/4, IPv6 2001:db8::/32, NAT64 64:ff9b::/96. (`ip route get 0.1.2.3` goes via gateway `198.19.0.1 dev host0`; `0/8` is not in the local table on this host.)

Fix: those ranges are now refused, with IPv4-mapped IPv6 normalized via `To4()` first. **Exception — 198.18.0.0/15 is deliberately NOT blocked:** this deployment's egress DNS maps every public host into 198.18.245.0/24 (verified: `example.com` → 198.18.245.209, `www.google.com` → 198.18.245.210, `api.deepseek.com` → 198.18.245.211), so blocking it refused every public fetch — the first version of this fix broke `TestFetchCheckRedirectBlocksPrivate` (example.com). The range holds only the egress gateway (a router, not a service endpoint) in this environment, so the residual risk is low. Documented in the code (`web.go:83` comment) and in the test.

Test: `TestAudit2IsPublicIPRanges` (`audit2_fix_test.go:127`), which also documents the 198.18/15 exception.

### P1-7 `process_list` ps fallback leaks full command lines (confirmed, fixed)

`internal/tools/misc.go` — `listProcsViaPS` (`:519`)

The fallback used `ps -eo pid,comm,args`, leaking full command lines (possible secrets in args) to the model; the `/proc` path only reads comm/state.

Fix: changed to `ps -eo pid,comm,stat`; state is set from fields[2].

Test: `TestAudit2ListProcsViaPSHasNoArgs` (`audit2_fix_test.go:162`) — asserts no returned command name contains a space.

### P1-8 `file_list` reads all entries into memory (confirmed, fixed)

`internal/tools/files.go` — `fileListTool.Execute` (`:467`), `maxListRead` (`:35`)

`os.ReadDir` read every entry (memory DoS on huge dirs); only the display was capped at 200.

Fix: chunked `os.Open` + `ReadDir(512)` loop, capped at 10000 entries read (`maxListRead`); the total is counted exactly up to the cap and reported as "more than 10000 entries" beyond it. The existing `"(250 total entries, showing first 200)"` output for ≤10000-entry dirs is preserved (existing test `p2_fix_test.go` still passes).

Test: `TestAudit2FileListCapsRead` (`audit2_fix_test.go:102`) — 10001 entries, asserts the "more than 10000 entries" marker and exactly 1 header + 200 entries + 1 marker line.

## P2 / residuals — open or documented

- **P2-1 DNS-rebinding TOCTOU in web_fetch** (decided not to fix): check-then-fetch. A proxy IS configured in this environment (`HTTP_PROXY`/`HTTPS_PROXY`/`ALL_PROXY` → egress proxy; `NO_PROXY` covers localhost etc.), so the agent never dials the target directly — the proxy does. Setting `Proxy: nil` would break web_fetch here, and a pinned DialContext would receive the proxy address and refuse it. Documented in `web.go` (`checkFetchHost` comment). The SSRF guard still blocks every static target.
- **P2-2 Secret warning:** the proxy URL in the environment contains a password credential. It appeared in tool output during this audit; it must NEVER be reproduced in any report, file, or test.
- **P2-3 Scanner coverage gaps** (`internal/security/scanner.go`): no patterns for npm tokens, `xoxo-` Slack tokens, GCP service-account keys, Docker Hub/PyPI tokens; `telegramRe` can false-positive on timestamp-like `digits:long-hex` strings. Not fixed in this pass (scanner was not the focus of the P0/P1 work; patterns need care to avoid false positives).
- **P2-4 Audit key creation race** (`internal/security/audit_key.go`): the loser of the `O_EXCL` race can read a partially-written key (length ≠ 32 → error). Fail-closed, rare. Not fixed.
- **P2-5 Two `AuditLog` handles on one path** would interleave seqs and break the chain (fail-closed: Verify fails loudly). No flock. Not fixed.
- **P2-6 Variable indirection beyond simple assignments**, aliased/dynamic interpreter calls, `chr()`-obfuscated payloads, code from stdin/script files: documented residuals; the sandbox is the mitigation. (`$x` inside a command substitution, e.g. `$( $x -rf /)`, is also not resolved — `blockedSubstitution` checks the substitution without the vars map.)
- **P2-7 `ip`/`ifconfig`/`iptables` network mutation** (`ip link set ... down`) is not covered by the denylist. Noted as a residual.
- **P2-8 No PID namespace in the sandbox** (host PIDs not visible anyway: no /proc mounted, mount of proc fails EPERM). Adding `CLONE_NEWPID` is possible future hardening; not implemented (PID 1 signal semantics risk).
- **P2-9 Seccomp filter INACTIVE in this deployment**: the filter design exists (`internal/sandbox/seccomp.go`, blocks chroot 161, pivot_root 155, mount, unshare, setns, etc.) and the launcher source exists (`internal/sandbox/cmd/launcher/main.go`), but NO built `orion-seccomp` binary is installed next to any binary or in PATH, and `Makefile`/`install.sh` do not reference it yet. Probe logs confirm: "seccomp filter unavailable (no orion-seccomp launcher binary found...)". The sandbox currently relies on namespaces + pivot_root. Installing the launcher is a sibling phase's job, not this audit's scope. Note: `gofmt -l` lists `internal/sandbox/seccomp.go` and `seccomp_test.go` as unformatted — those are the sibling's in-progress files; not reformatted here.
- **P2-10 Sandbox package doc was stale** (fixed in this audit): it described the abandoned setpriv plan as the mechanism and claimed pivot_root unmounting was impossible. Corrected to describe pivot_root + umount2 (`sandbox.go:35–51`). No test needed (comment-only change); sandbox tests re-run green.
- **P2-11 `cappedWriter` race: NOT a bug.** Verified in Go 1.27.4 `os/exec` source: when Stdout==Stderr and comparable, only one pipe/goroutine is used.
- **P2-12 `mknod` / `mount -t proc` inside the sandbox: NOT holes.** Both fail with EPERM (kernel superblock ownership / no PID-ns ownership).

## Verification

After all fixes (Go at `/home/hatch/go1.27.4/bin/go`):

- `gofmt -l internal/security/ internal/sandbox/ internal/tools/` — clean (sibling seccomp files excluded; see P2-9).
- `go build ./...` — pass.
- `go vet ./...` — clean.
- `go test ./internal/security/ ./internal/sandbox/ ./internal/tools/ -count=1` — all pass.
- `go test -race ./internal/security/ ./internal/sandbox/ ./internal/tools/ -count=1` — all pass, no races.
- `go test ./... -count=1` — 14/14 packages pass.

New permanent tests (17 test functions; probe files deleted):

- `internal/security/audit2_fix_test.go` — `TestAudit2WrapperBypasses`, `TestAudit2EnvSplitString`, `TestAudit2DollarQuoting`, `TestAudit2FileOpTargets`, `TestAudit2FindExecWrappers`, `TestAudit2VariableIndirection`, `TestAudit2InterpreterKinds`, `TestAudit2KillMountAccounts`, `TestAuditAppendHeadFailureResyncsTip`.
- `internal/tools/audit2_fix_test.go` — `TestAudit2FileReadFifoWithoutWriterReturns`, `TestAudit2FileEditRefusesNonRegular`, `TestAudit2ScrubEnvStripsCredentialVars`, `TestAudit2FileListCapsRead`, `TestAudit2IsPublicIPRanges`, `TestAudit2ListProcsViaPSHasNoArgs`.
- `internal/sandbox/sandbox_test.go` (from the earlier part of this audit) — `TestSandboxChrootEscapeBlocked`, `TestSandboxNoDevTTY`.

Files changed by this audit (scope only): `internal/security/security.go`, `internal/security/audit.go`, `internal/security/audit2_fix_test.go` (new), `internal/sandbox/sandbox.go` (doc correction + earlier /work, /dev/tty, EvalSymlinks, clone-flag fixes), `internal/sandbox/sandbox_test.go` (regression tests), `internal/tools/files.go`, `internal/tools/shell.go`, `internal/tools/web.go`, `internal/tools/misc.go`, `internal/tools/audit2_fix_test.go` (new). The pivot_root sandbox fix itself was the sibling's concurrent work, verified by this audit.
