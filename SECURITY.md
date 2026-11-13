# Orion Security

## Denylist — the rule that matters most

On 2026-11-13 a denylist test ran a live `rm -rf /` and destroyed
the system. That must never happen again. The rules below are
non-negotiable.

1. **Never run a live destructive command in any test or script.**
   Denylist tests call `IsBlockedCommand()` with strings only.
   They never execute the command.
2. **One canonical implementation.** `IsBlockedCommand` lives only
   in `internal/security/security.go`. Every other package delegates
   to it. No duplicate copies that can disagree.
3. **Token-based blocking.** The command is tokenized (quotes and
   escapes honoured); the binary basename is resolved, so `/bin/rm`,
   `/usr/bin/rm`, `./rm` and plain `rm` all match `rm`. `sudo`,
   `env`, `nice`, `time` wrappers are stripped before matching.
   Segments split on `;`, `|`, `&&` are each checked.
4. **Blocked patterns include:**
   - `rm -rf /`, `rm -rf /*`, `rm --no-preserve-root`, any
     recursive `rm` (`-r`/`-R`/`-rf`) — recursive rm is blocked
     outright; plain `rm <explicit file>` is allowed.
   - `mkfs*`, `shred`, `wipe`, `fdisk`, `parted`, `blkdiscard`,
     `reboot`, `shutdown`, `halt`, `poweroff`, `init`,
     `systemctl`, `service`.
   - `dd` writing to a device (`of=/dev/...`), and `dd` without an
     explicit safe target.
   - `chmod 777` / `chmod 7777` (any target), and any `chmod` of a
     system path whatever the mode (`chmod 644 /etc/shadow`,
     `chmod 4755 /bin/sh`, `chmod u+s /bin/bash` — Audit 4).
   - Empty commands.
   - Redirect and file-operation targets reached through a variable
     or command substitution (`f=/etc/passwd; echo x > $f`,
     `cp /tmp/x $f`, `echo x > $(echo /etc/passwd)` — Audit 4):
     targets are expanded through the recorded assignments and an
     unresolvable `$` expansion or a substitution target is refused.
5. **Shell tool enforcement.** `shell_exec` calls
   `IsBlockedCommand` before spawning anything and returns a
   `denied` ToolError. It also runs in its own process group with
   SIGKILL on timeout, so a timed-out command cannot linger.
6. **No test may delete, format, or overwrite anything outside a
   temp dir created by `t.TempDir()`.**

## Sandbox mode (shell_exec)

Opt-in confinement for `shell_exec`, on top of the denylist — it
exists because the denylist cannot see what aliased/dynamic
interpreter code, a bare interpreter, a script file, or stdin will
do. The sandbox can; the denylist still runs first, so a blocked
command is refused before anything is spawned.

### Mechanism (surveyed on this VM, 2026-11-13)

- `bwrap` and `firejail` are **not installed**; `systemd-run
  --user` has no D-Bus (`Failed to connect to bus`). The only
  container mechanism this host offers is **Linux namespaces**,
  and those work (`unshare -rm`, `unshare -rmn` succeed as root).
- The sandbox is therefore built from namespaces directly, with no
  new dependency (`internal/sandbox`, stdlib only): the child is
  started with `CLONE_NEWNS`, `CLONE_NEWIPC` (its own SysV IPC
  namespace), `CLONE_NEWUTS` (its own hostname), `CLONE_NEWPID`
  (its own PID namespace — see "PID namespace" below), and —
  unless `allow_network` is set — `CLONE_NEWNET` (a network
  namespace whose only interface is a down loopback: no network at
  all). The user namespace is **not** created by the clone: the
  wrapper mounts `/proc` first and only then drops into a fresh
  user namespace with `unshare --user --map-root-user` (agent
  uid/gid mapped to root inside), immediately before running user
  code — see "PID namespace" for why the order matters. The end
  state is the same as creating it at clone time.
- Inside the namespaces a wrapper builds a chroot root under
  `<root>/.orion-root` and runs the command there as `/bin/sh -c`
  with cwd `/work`:
  - `/usr` bind-mounted **read-only** (`ro,nosuid,nodev`); `/bin`,
    `/sbin`, `/lib`, `/lib64` are symlinks into it (merged-/usr).
  - `/etc` is a **minimal copy** of `passwd, group, hosts,
    resolv.conf, nsswitch.conf` — never bind-mounted: the host
    `/etc` contains runtime submounts (including a credentials
    tmpfs at `/etc/hatch/credentials`) and the bind is refused
    inside a nested user namespace.
  - `/dev` is the harmless nodes (`null, zero, full, random,
    urandom, tty`) bind-mounted individually; `/dev` itself is
    never bound (tmpfs with submounts, same refusal).
  - `/tmp` is a per-run tmpfs (`nosuid,nodev,noexec`) that vanishes
    with the namespace; `<root>/work` is bind-mounted read-write
    as `/work` — the only persistent writable place.
  - The chroot root itself is remounted read-only (`--rbind`, which
    replicates the submounts — a plain self-bind drops them).
  - `/proc` is a real procfs for the new PID namespace (see "PID
    namespace" below): `ps` and `ls /proc` work, but only ever show
    namespace processes. `/sys` is an empty directory: no host
    kernel information leaks in.
- Environment: the usual scrubbed env, plus `HOME=/work`,
  `TMPDIR=/tmp`, `PATH` set to the in-sandbox directories.
- Result: even `rm -rf /` inside can only destroy the sandbox's
  own contents. Proven by `TestSandboxContainsDestructiveCommand`
  and `TestShellExecSandboxContains` (temp dirs only, canary file
  outside the root survives, canary inside `/work` is destroyed).

### PID namespace

Added 2026-11-13 (v0.2 hardening). The sandboxed child is started
with `CLONE_NEWPID`, so it is PID 1 in its own PID namespace: it
cannot see host processes (`ls /proc` shows only namespace PIDs)
and cannot signal them (`kill -0 <host-pid>` fails with ESRCH).
Proven live by `TestSandboxPIDNamespaceIsolatesHost` (host canary
`sleep`, known PID) and `TestSandboxProcReflectsNamespace`.

**Why the user namespace moved out of the clone.** `/proc` must be
mounted for the PID namespace to be observable (`ps`, `ls /proc`).
On this host the kernel (7.0.0-44-generic, custom) refuses procfs
mounts from inside a nested user namespace: every variant was
tried and fails with EPERM — single clone with
`CLONE_NEWUSER|CLONE_NEWPID`, two-stage `unshare` (user namespace
first, then PID namespace inside it), `subset=pid` and plain
mounts, `/proc` and arbitrary mountpoints, even mounting proc for a
PID namespace owned by the cell's user namespace from inside a
nested one. The same mount succeeds from the cell's user namespace
(no nested user namespace). So the user namespace is **not**
created at clone time. The wrapper instead runs first as the
agent's uid in the host user namespace, mounts `/proc` (and
everything else), `pivot_root`s, detaches the old root, and only
then drops into a fresh user namespace with `unshare --user
--map-root-user` immediately before running user code. The end
state is identical to creating it at clone time — root inside its
own user namespace, capabilities only over its own namespaces.

**The mount namespace follows the user namespace.** The mount
namespace is created by the clone, so it is owned by the host user
namespace; after the switch the process would hold no capabilities
over it and could not mount anything at all — not even a tmpfs on
`/tmp` inside its own namespace. That would also void the seccomp
control test (mounts would be blocked with or without the filter,
so the filter's blocking would be unprovable). So the switch is
immediately followed by a fresh mount namespace (`unshare
--mount`, a copy of the prepared one), owned by the new user
namespace: the process owns its mount namespace again, exactly as
when the user namespace was created at clone time, and seccomp —
not the lack of a capability — is what restricts mounts.

**`/proc` contents.** Mounted with `subset=pid` when the kernel
supports it (5.8+): `/proc` then contains only per-process entries
plus `self`/`thread-self` — no `/proc/sys`, `/proc/kcore`,
`/proc/kallsyms` or `/proc/sysrq-trigger` at all. On older kernels
the fallback is a plain proc mount with `/proc/sys` bound
read-only. Either way a sandboxed process sees only its own
namespace's processes, and `/proc/sys` is not writable.

**PID 1 reaping.** The user shell is PID 1 (the `exec` chain
preserves the PID through the wrapper, the `unshare` switch and the
seccomp launcher). It reaps orphaned children through its normal
wait loop — proven by `TestSandboxPID1ReapsOrphans` (a grandchild
orphaned onto PID 1 leaves no zombie). PID 1 exiting kills the
whole namespace, so nothing can linger after the command ends.

**Fail-closed decision (explicit).** There are three layers, and
none of them degrades silently:
1. The PID namespace is requested atomically with the other
   namespaces in the single `clone` — if its creation fails the
   clone fails, the child never starts, and the command never runs.
   There is no code path where the PID namespace is silently
   missing. (`TestPrepareRequestsPIDNamespace` pins the flags.)
2. The wrapper exits 126 before running any user code if the
   `/proc` mount fails (or any other setup step fails).
   (`TestSandboxWrapperRefusesWithoutProc` proves it by running
   the wrapper where the mount is refused.)
3. Availability is probed up front: `sandbox.Available()` runs a
   trivial command through the full wrapper, including the PID
   namespace and the `/proc` mount. If the host cannot do it, the
   sandbox reports unavailable and `shell_exec` REFUSES the command
   rather than running it unsandboxed — fail-closed by default (see
   "Limits" below). Only an explicit opt-in
   (`sandbox.allow_unsandboxed_fallback=true`) restores the old
   warn-and-run behaviour. The probe is a once-per-process
   capability decision, not a per-command silent downgrade.

This makes `unshare(1)` (util-linux, in `/usr`) a sandbox
availability dependency alongside `python3`: if it is absent the
wrapper fails before user code runs and the probe reports the
sandbox unavailable.

### Config

```json
{ "sandbox": { "enabled": true, "root": "~/.orion/sandbox", "allow_network": false, "allow_without_seccomp": false } }
```

Env overrides: `ORION_SANDBOX=on`, `ORION_SANDBOX_ROOT=<path>`.
Default root is `<data_dir>/sandbox`; default is **off** — when
off, `shell_exec` behaves exactly as before. `root` must be an
absolute path, never `/` or inside a system tree (`/etc`, `/usr`,
`/proc`, …). Commands run with cwd `/work` (the root's `work/`
subdir on the host), so files the agent writes persist there
between runs; `/tmp` does not.

### Limits

- **Availability is host-dependent.** Nested uid/gid mapping is
  blocked for unprivileged users on this VM (the cell itself is a
  user-namespaced container), so the sandbox only works when the
  agent runs as root, or on a host that allows unprivileged user
  namespaces. The wrapper does all of its mounts before the
  user-namespace switch, while running as the agent's uid in the
  host user namespace, so as a non-root agent the very first mount
  fails and the probe reports the sandbox unavailable.
  `sandbox.Available()` probes the real mechanism (a trivial
  command through the full wrapper) and caches the result. When
  sandbox mode is on but unavailable — or setup fails — `shell_exec`
  REFUSES the command (fail-closed) instead of running it
  unsandboxed; only an explicit
  `sandbox.allow_unsandboxed_fallback=true` restores the old
  warn-and-run behaviour (warning logged, result marked
  `[warning: sandbox ...]`). The same fail-closed rule covers a
  missing seccomp filter: when the sandbox is enabled and the
  filter is not disabled but no usable `orion-seccomp` launcher
  can be found (or the launcher probe/staging fails), the command
  is REFUSED unless the operator explicitly opted into unfiltered
  runs with `sandbox.allow_without_seccomp=true` (Audit 3,
  F-A3-1). The two opt-ins govern different degradations and
  neither implies the other: the fallback opt-in covers a sandbox
  that cannot be established at all, the filter opt-in covers
  "sandboxed without syscall filtering" — a missing filter is
  refused even with the fallback opt-in set, because letting it
  run the command fully unsandboxed would be a worse degradation
  than sandboxed-without-filter. An opted-in unfiltered run is
  logged on every command, naming the opt-in, so the loss of
  filtering is visible per command, not once per process.
- The sandbox confines the **filesystem, network, and syscall
  surface**, not the process's own capabilities in the abstract:
  the sandboxed process is root inside its own user namespace, so
  without the seccomp filter it can unmount/remount things *inside*
  its namespaces — but after `pivot_root` it holds no fd or path to
  anything outside the new root, so there is nothing to escape to.
  (The pivot replaces the earlier plain chroot, which was escapable
  via a cwd held outside it.)
- The wrapper `pivot_root`s into the new root and detaches the old
  (host) root. The detach calls `umount2(2)` directly via `python3`
  (present in the new root through the `/usr` bind mount; the
  syscall number is arch-dependent) instead of the `umount` CLI:
  the direct syscall is proven and never depends on `/proc` or
  libmount — the CLI was strace-proven (2026-11-13) to fail opening
  `/proc/mounts` back when `/proc` was not mounted at all, and
  there is no reason to re-prove it now that the namespace's own
  `/proc` exists. If `python3` is unavailable the wrapper exits
  126 and the sandbox is unavailable rather than running with the
  host root visible — which also makes `python3` a sandbox
  availability dependency.
- A sandboxed command cannot see the agent's real working
  directory or workspace — only `<root>/work`. Point `root` at a
  project directory's parent (or copy files in) when a command
  needs project files.
- Seccomp filter: see "Seccomp filter" below. When a usable
  `orion-seccomp` launcher is installed, sandboxed commands run
  under a syscall filter (default-allow + blocklist); without it
  the command is refused (fail-closed, Audit 3 F-A3-1) unless
  the operator opted in with `sandbox.allow_without_seccomp=true`
  (then it runs with namespace/pivot_root containment only and
  every degraded run is logged) or disabled the filter outright
  with `sandbox.disable_seccomp=true`.

### Seccomp filter

Feasibility (surveyed on this VM, 2026-11-13): the kernel
supports seccomp (`Seccomp: 2` in `/proc/self/status`);
`libseccomp2` 2.5.5 is installed but the `-dev` headers are
absent, and the module has no Go dependencies, so neither
`libseccomp-golang` (cgo + `libseccomp-dev`) nor
`golang.org/x/sys/unix` is used. The filter is pure-stdlib Go: a
hand-rolled classic-BPF program installed with raw `seccomp(2)` +
`prctl(PR_SET_NO_NEW_PRIVS)`. No cgo, no new dependency. Proven
live by `TestSeccompLauncherBlocksSyscalls`.

Design (`internal/sandbox/seccomp.go`):

- Default ALLOW with an explicit blocklist of escape-relevant
  syscalls (46 on amd64): mounts (`mount`, `umount2`,
  `pivot_root`, `chroot`, and the whole new mount API `fsopen`/
  `fsconfig`/`fsmount`/`fspick`/`open_tree`/`move_mount`/
  `mount_setattr`), host control (`reboot`, `swapon`/`swapoff`,
  `kexec_load`, `kexec_file_load`, `init_module`/`finit_module`/
  `delete_module`, `clock_settime`/`settimeofday`/`adjtimex`/
  `clock_adjtime`, `sethostname`/`setdomainname` — the sandbox
  has its own UTS namespace (so hostname changes are contained
  there anyway) but shares the host clock, so clock syscalls are
  blocked outright), keyrings (`keyctl`,
  `add_key`, `request_key`), kernel attack surface (`ptrace`,
  `bpf`, `perf_event_open`, `userfaultfd`, `fanotify_init`),
  chroot escape via file handles (`open_by_handle_at`,
  `name_to_handle_at`), namespace escape (`setns`, `unshare`),
  host info/control (`syslog`, `iopl`, `ioperm`, `acct`,
  `quotactl`, `vhangup`), cross-process memory
  (`process_vm_readv`/`process_vm_writev`), and `seccomp` itself.
  A default-deny allowlist was considered and rejected: the
  sandbox must run arbitrary user tools, and enumerating every
  syscall any tool might need is fragile — one missing number
  silently breaks a legitimate command. The dangerous set is small
  and enumerable; the namespaces + pivot_root remain the primary
  containment.
- `clone(2)` is allowed, but only when its flags carry no
  namespace-creation bit (`CLONE_NEWNS`/`NEWUTS`/`NEWIPC`/
  `NEWUSER`/`NEWPID`/`NEWNET`/`NEWCGROUP`/`NEWTIME`, checked by
  BPF on the flags argument). `clone3` is denied with ENOSYS (not
  EPERM): glibc's `posix_spawn` retries with `clone(2)` on ENOSYS,
  so process-spawning tools keep working; EPERM would break them.
  `clone3`'s flags live in a pointed-to struct BPF cannot
  dereference, so it is denied outright. Proven by
  `TestSeccompCloneFlagProbe` and the `posix_spawn` case in
  `TestSeccompLauncherBlocksSyscalls`.
- A syscall in an unexpected architecture (e.g. a 32-bit compat
  binary, whose numbers differ) fails with ENOSYS.
- `mknod`/`mknodat` are deliberately not blocked: the kernel
  already refuses device-node creation inside the sandbox (EPERM
  on both `/work` and the `/tmp` tmpfs, probed 2026-11-13), while
  fifos (`mkfifo`) need `mknod` and are legitimate.
- Tables are per-architecture (amd64, arm64), verified against
  the kernel headers (`asm/unistd_64.h`, `asm-generic/unistd.h`)
  and cross-checked against Go's `syscall` package where the
  constants exist (`TestSeccompTableMatchesKernelHeaders`,
  `TestSeccompTableMatchesStdlib`). Any other `GOARCH` fails
  closed: the launcher exits 126 instead of running the command
  unfiltered.

Mechanism: Go's `os/exec` has no pre-exec hook, so the filter is
installed by a tiny helper binary, `orion-seccomp`
(`internal/sandbox/cmd/launcher`, built `CGO_ENABLED=0` so it is
static and runs inside the new root), which installs the filter on
itself and execs the target command — filters persist across
`execve`. `Prepare` stages the helper into the new root
(`<root>/.orion-root/orion-seccomp`, atomically) and the wrapper
execs through it when present. Discovery: `Config.SeccompLauncher`,
`$ORION_SECCOMP_LAUNCHER`, `orion-seccomp` next to the running
binary, then `PATH`. Config: `seccomp_launcher` (explicit path),
`disable_seccomp` (opt out even when a launcher is available —
a deliberate choice, never a refusal), `allow_without_seccomp`
(opt into running WITHOUT the filter when no usable launcher
exists; default false). When no usable launcher is found (or the
kernel probe fails, or staging fails) and neither the opt-in nor
the disable is set, `Prepare` refuses with an error wrapping
`sandbox.ErrSeccompUnavailable` that names the opt-in, and
`shell_exec` refuses the command (`denied`) — the old
graceful-fallback behaviour (run unfiltered, warn once) is gone
(Audit 3, F-A3-1; `TestSeccompMissingLauncherRefused`,
`TestShellExecSandboxRefusesWithoutSeccompLauncher`). The
availability probe (`sandbox.Available()`) is unaffected: it runs
with `DisableSeccomp: true`, so it never logs a misleading
"seccomp filter unavailable" warning
(`TestPrepareWithDisabledSeccompLogsNoWarning`).

Open: none — `install.sh` and the `Makefile` build and install
`orion-seccomp` alongside `orion`/`oriond`, so in a normal install
the filter is active whenever a usable launcher is present. The
host cell itself already runs under a seccomp filter that blocks a
subset (`swapon`, `kexec`, `bpf`, `userfaultfd`, `setns`,
`settimeofday`, `syslog` on this VM); the sandbox filter is the
portable layer that blocks the rest on any host.

## Secret handling

- `security.RedactSecrets` / `ScanSecrets` detect OpenAI keys,
  Anthropic keys, GitHub tokens, AWS keys, Telegram bot tokens,
  Bearer tokens, private-key blocks, and generic
  `api_key/token/password/secret` assignments, replacing values
  with `[REDACTED]`.
- The CLI masks secrets in `orion config show`.
- Audit entries and logs pass through the redactor.
- Bot tokens and API keys live only in `~/.orion/config.json`
  (0600) or environment variables. They are never committed,
  never printed, never logged, and never appear in tests
  (tests use fake values like `sk-testfakekey123`).
- `.gitignore` excludes `.env`, `config.json/yaml`, `.orion/`.

## Audit log

`~/.orion/audit.jsonl` is append-only JSON lines with a SHA-256
hash chain: each entry's hash covers the previous hash, sequence,
timestamp, actor, action, and detail. `Verify()` recomputes the
whole chain; any tampering (byte flip, deletion, reorder) fails
verification. The log is created 0600 in a 0700 directory.

## Telegram authorisation

- `telegram_allowed_chats` in config lists the chat IDs that may
  drive the agent. The gateway drops messages from any other chat
  before the handler (and the agent, and its tools) ever sees them,
  and logs the dropped chat's ID so an operator who has not
  allowlisted themselves yet can find it in the daemon log. When
  the list is empty, the daemon refuses to start polling at all
  unless `allow_all_telegram_chats=true` is set explicitly in
  config — an empty allowlist must never silently mean "everyone".
  Set the allowlist (send `/whoami` to the bot to learn your chat
  ID) before exposing the bot.
- Gateway errors never leak the bot token: `*url.Error` embeds the
  request URL (which contains the token), so errors are wrapped in
  a redacting error type (`[redacted]` in `Error()`; `Unwrap()` is
  preserved for `errors.Is`/`errors.As`).

## File-tool confinement

- File tools (`file_read`, `file_write`, `file_edit`, `file_list`)
  are confined to the working directory and the OS temp directory.
  Symlinks are resolved before the check, so a link cannot smuggle
  a path out of bounds. `$HOME/.orion`, `$HOME/.ssh`, and
  `$HOME/.gnupg` are never reachable, even under an allowed root.
- Writes are atomic (temp file + rename), 0600; edits preserve the
  file's existing mode. Writes are capped at 1MB; `file_list` caps
  output at 200 entries and reports the total.

## Other hardening

- All HTTP clients have timeouts; all tool outputs are capped
  (shell 64KB, web fetch 256KB default, file read 1MB).
- Config files are written 0600; memory files 0600 in a 0700 dir.
- The agent loop has a max-step budget and per-iteration timeouts
  and honours context cancellation.
- File tools refuse directory paths where a file is expected and
  cap reads.
- Tool output is redacted (`RedactSecrets`) at the registry before
  it reaches the model, so a file or command that surfaces a secret
  does not leak it into the conversation. The secret scanner itself
  never redacts ordinary prose (`Basic training`, `Bearer
  authentication` are left alone — the token must look like a real
  token).
- `shell_exec` runs with a scrubbed environment (no `ORION_*`,
  `*_API_KEY`, `*_TOKEN`, `*_SECRET*`, `*_PASSWORD*` variables),
  and its output buffer is capped while the command runs, so a
  flooding command cannot exhaust memory.
- `web_fetch` refuses non-public targets (loopback, RFC1918,
  link-local, multicast) and re-checks every redirect target
  (SSRF guard). URL credentials are redacted from error messages.
- Config `Save` never persists an environment-supplied API key or
  Telegram token: file values are restored for secrets that still
  hold their env value; a deliberately changed secret persists.
- Audit hashes are length-prefixed per field (old-format logs still
  verify via a v1 fallback on read; new entries always use the new
  format).
