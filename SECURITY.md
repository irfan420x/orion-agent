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
   - `chmod 777` / `chmod 7777` (any target).
   - Empty commands.
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
  started with `CLONE_NEWUSER` (agent uid/gid mapped to root
  inside), `CLONE_NEWNS`, and — unless `allow_network` is set —
  `CLONE_NEWNET` (a network namespace whose only interface is a
  down loopback: no network at all).
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
  - `/proc` and `/sys` are empty directories: no host process or
    kernel information leaks in.
- Environment: the usual scrubbed env, plus `HOME=/work`,
  `TMPDIR=/tmp`, `PATH` set to the in-sandbox directories.
- Result: even `rm -rf /` inside can only destroy the sandbox's
  own contents. Proven by `TestSandboxContainsDestructiveCommand`
  and `TestShellExecSandboxContains` (temp dirs only, canary file
  outside the root survives, canary inside `/work` is destroyed).

### Config

```json
{ "sandbox": { "enabled": true, "root": "~/.orion/sandbox", "allow_network": false } }
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
  user-namespaced container; writing `uid_map` as `nobody` fails
  with EPERM), so the sandbox only works when the agent runs as
  root, or on a host that allows unprivileged user namespaces.
  `sandbox.Available()` probes the real mechanism (a trivial
  command through the full wrapper) and caches the result. When
  sandbox mode is on but unavailable — or setup fails — `shell_exec`
  falls back to the denylist alone, logs a warning, and marks the
  result `[warning: sandbox ...]` instead of failing outright.
- The sandbox confines the **filesystem and network**, not the
  process's own capabilities: the sandboxed process is root inside
  its own user namespace, so it can unmount/remount things *inside*
  its namespaces, but it holds no fd or path to anything outside
  the chroot, so there is nothing to escape to.
- A sandboxed command cannot see the agent's real working
  directory or workspace — only `<root>/work`. Point `root` at a
  project directory's parent (or copy files in) when a command
  needs project files.
- No seccomp filter is applied (no `golang.org/x/sys` seccomp
  helper is vendored); the namespace + chroot + read-only mounts
  are the containment. Adding a syscall filter is future work.

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
  before the handler (and the agent, and its tools) ever sees them.
  When the list is empty, all chats are accepted and the daemon
  prints a warning — set the allowlist before exposing the bot.
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
