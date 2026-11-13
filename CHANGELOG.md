# Changelog

All notable changes to Orion are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the
project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Memory v2 — Claude Code-inspired typed memories**
  (`internal/memory/typed.go`, `gate.go`, `dates.go`, `feedback.go`,
  `migrate.go`, `session.go`, `internal/core/recall_v2.go`,
  `drift.go`, `extraction.go`, `internal/app/memory_v2.go`,
  `internal/messaging/commands.go`, `render.go`): memories are now
  markdown files under `~/.orion/memories/*.md` with frontmatter
  (`name`, `description`, `type: user|feedback|project|reference`).
  Feedback memories carry a rule + `Why:` + `How to apply:`;
  corrections AND confirmations are saved. A what-not-to-save gate
  refuses derivable things (code patterns, architecture, git
  history, file structure); even explicit save requests keep only
  what was surprising or non-obvious. Relative dates are converted
  to absolute dates on save for project memories. Existing JSON
  facts are migrated into typed files on open (copied, never
  deleted; idempotent) and the JSON store stays the message log.
  Recall v2 scans a header manifest (max 200, newest-first) and a
  small provider side-query selects at most 5, with the existing
  keyword token-overlap scorer as fallback/offline; already-surfaced
  memories are excluded and warnings/gotchas for tools in active
  use are kept. Drift verification checks a recalled memory's
  file/function/flag references against the current tree before
  injecting; on conflict the current state wins and the memory is
  marked stale (`stale: true` in frontmatter, persistent) and never
  re-surfaced. Background session-memory extraction runs after 10
  tool calls / 5 turns into `session-memory.md` (single-flight,
  best-effort); `/compress` feeds from it and cites it in the saved
  fact. `/memory` lists typed memories grouped by type with ages
  and forget-by-name; the Telegram footer shows `· N memories`
  when typed memories were injected. Live proof (DeepSeek): a typed
  memory "Favourite number is 17" (in no message) was recalled in
  a later turn, and a feedback "keep replies to one or two
  sentences" rule changed a later reply to a single sentence.
  Feedback memories are standing guidance: they bypass the topical
  selector and the surfaced exclusion and are injected every turn
  (max 2, under their own "Standing guidance" section header) —
  the topical selector alone did not surface them live.
  Team-memory sync deliberately not built (single user).

- **Sandbox PID namespace** (`internal/sandbox/sandbox.go`):
  `CLONE_NEWPID` joins the sandbox's user/mount/IPC/UTS/network
  namespaces, so sandboxed processes can no longer see or signal
  host processes (the sandboxed shell runs as PID 1 and reaps
  orphans; `/proc` is a real procfs for the namespace). Restructure
  forced by this host's kernel, which refuses procfs mounts from
  inside a nested user namespace: the wrapper mounts `/proc` first
  (as the agent's UID in the host user namespace), pivots, detaches
  the old root, then drops into a fresh user namespace via
  `exec unshare --user --map-root-user` plus a fresh mount
  namespace. New dependencies: `unshare(1)` alongside `python3`.
  Fail-closed: clone failure means the child never starts, and the
  wrapper exits 126 before any user code if setup fails. Live
  proof: a host canary process is invisible in the sandbox's
  `/proc` and `kill -0` on it fails. See `SECURITY.md`.
- **Tool-result and history size caps** (`internal/tools/caps.go`,
  `internal/core/agent.go`, `internal/config`): every tool result
  and error string is truncated centrally in `Registry.Execute`
  (default 256 KiB, configurable via `MaxToolResultBytes`,
  1 KiB–10 MiB); the agent loop trims conversation history to a
  token budget (default 32K, `MaxHistoryTokens`, 1K–1M) oldest-first
  while never dropping the system prompt or the latest user turn
  and preserving tool-call/result pairing. Multi-MB tool output is
  truncated safely and the loop continues. See `README.md`.
- **Provider timeout architecture** (`internal/model/provider.go`):
  three distinct, nested timeouts replace the old single 120s
  client timeout — connection 10s, progress 60s (first byte and
  mid-stream silence; any byte resets the window, so slow-but-
  steady reasoning streams are never killed), total 100s inside the
  120s step budget. Progress timeouts are retryable; a stalled
  stream retries only when nothing usable was delivered, so live
  output is never duplicated. See `ARCHITECTURE.md`.

### Security

- **DNS-rebinding SSRF TOCTOU closed** (`internal/tools/web.go`,
  `internal/config`, `internal/app`, `internal/tools/tools.go`):
  `web_fetch` used to `LookupIP` → check public → fetch (resolving
  a second time), so a rebinding host could answer public once and
  private on the fetch. Now each hostname is resolved exactly once
  per fetch, **all** resolved IPs are validated public, and the
  dial pins the validated IPs directly (custom `DialContext`;
  TLS SNI and HTTP Host keep the original hostname). Redirect
  targets go through the same resolve-validate-pin step. Proxy-
  aware: when an egress proxy is configured the proxy does the
  dialing, so pinning does not apply and the pre-check is the only
  guard (documented in code). Regression tests cover a rebinding
  host, redirect-to-private, SNI/Host preservation, the proxy path,
  and the trusted-egress path.
- **198.18/15 benchmarking range is no longer a global exception**
  (`internal/tools/web.go`, `internal/config`): the range is now in
  `nonPublicIPNets` (strict by default). Deployments whose egress
  DNS maps public hosts into it list the range explicitly in the
  new `trusted_egress_cidrs` config field (validated CIDRs);
  this deployment uses `198.18.31.0/24`. Without the field,
  `web_fetch` to public hosts is refused in such deployments.
- **Sandbox fail-open closed** (`internal/tools/shell.go`,
  `internal/sandbox/sandbox.go`): when the sandbox is enabled but
  unavailable or `Prepare` fails, `shell_exec` used to log a
  warning and run the command unsandboxed. It now refuses the
  command (`denied`, naming the opt-in) and runs nothing. The old
  behaviour is available only via the explicit
  `sandbox.allow_unsandboxed_fallback=true` opt-in (default false).
- **Telegram empty allowlist no longer means "everyone"**
  (`internal/messaging/telegram.go`, `cmd/oriond/main.go`,
  `cmd/orion/main.go`, `internal/config`): the daemon and the
  `orion telegram poll` / `orion daemon` commands refuse to start
  polling when `telegram_allowed_chats` is empty, unless
  `allow_all_telegram_chats=true` is explicitly set. The refusal
  tells the operator to set their chat ID (via `/whoami` or the
  daemon log, where dropped-chat IDs are now logged without
  message text). `Config.Validate()` still accepts an empty
  allowlist so `config show`/`status`/`test` keep working.
- **Sandbox root symlink validation order**
  (`internal/sandbox/sandbox.go`): `Prepare` used to `MkdirAll`
  the root first and only then `EvalSymlinks` + re-validate. It
  now canonicalizes (longest existing ancestor + re-join) and
  validates the canonical root **before** creating any directory;
  the post-creation re-validate stays as a second check.
- **Session persistence dir fsync** (`internal/messaging/
  session.go`): `saveLocked` now fsyncs the parent directory
  after the temp-file fsync + rename, matching the memory/config
  stores, so a renamed `sessions.json` survives a crash.
- **Version + docs sync** (`internal/version`): `Version` is now
  `0.2.0-rc2` (was `0.1.0` while the tag was `v0.2.0-rc2`);
  README status, SECURITY.md (seccomp launcher is built/installed
  by `install.sh`/`Makefile`; Telegram authorisation is refusal,
  not a warning; sandbox fallback is fail-closed) and
  ARCHITECTURE.md corrected.

### Fixed

- **Egress proxy regression caught at integration** (timeout
  rework): the first custom `http.Transport` omitted `Proxy`,
  which silently disabled the environment egress proxy and broke
  live DeepSeek calls (`tls: first record does not look like a TLS
  handshake`). `ProxyFromEnvironment` restored, pinned by
  `TestSharedClientHonoursProxyEnv`. Live-verified again:
  `TestLiveDeepSeek` PASS (396 in 1.52s).
- **Stale SECURITY.md line**: the seccomp section claimed the
  sandbox "shares the host UTS namespace" — UTS has been
  namespaced since before this phase; corrected (own UTS
  namespace, shared host clock).
  (`internal/core/events.go`, `internal/messaging/render.go`,
  `redact.go`, `commands.go`, `session.go`): the gateway now streams
  a Hermes-style "💻 terminal" card per turn — one message, edited
  in place as tool calls accumulate (blockquote Shell blocks,
  ✍️ file writes, `(×N)` dedupe, 4000-rune budget, HTML escaping),
  typing indicator while the turn runs, and a structured final reply
  in the user's language with an optional runtime footer (model,
  tokens, steps, duration; `/footer` toggles it). A slash-command
  registry + `setMyCommands` menu ships 17 real commands
  (`/help`, `/new`, `/stop`, `/status`, `/model`, `/usage`,
  `/undo`, `/retry`, `/pause`, `/agents`, `/title`, `/compress`,
  `/save`, `/restart`, `/whoami`, `/footer`, `/memory`) backed by a
  persistent per-chat session store. Better than the Hermes
  reference in one place that matters: displayed commands are
  passed through `RedactDisplayText`, so credential paths and
  secret-looking values (e.g. `$(cat ~/.hermes/tokens/…)`) render
  as `[redacted]` and never leak into the chat. Live-verified:
  441 tests pass, race clean.
- **DeepSeek provider** (`internal/model`, `internal/config`): a
  first-class provider reusing the OpenAI-compatible client
  (defaults `https://api.deepseek.com/v1` + `deepseek-chat`), now the
  default provider. Live-verified: `TestLiveDeepSeek` PASS
  (17*23+5=396 in 1.57s) and `orion run` answers 396.
- **Seccomp filter for the sandbox** (`internal/sandbox`): a
  pure-stdlib classic-BPF filter (no cgo/libseccomp) applied by the
  `orion-seccomp` helper — 46 syscalls EPERM, flag-checked `clone`,
  `clone3` denied with ENOSYS so glibc falls back to `clone(2)`.
  `install.sh` and `Makefile` now build and install
  `orion-seccomp` next to `orion`/`oriond`, so the filter engages
  on a normal install. See `SECURITY.md`.
- **Fact-aware memory recall** (`internal/core/recall.go`): relevant
  stored facts and older-turn statements are injected into the
  system prompt (top 3 facts, word-boundary matching), so the agent
  recalls facts even when they have fallen out of the 10-message
  window. Multi-turn live E2E run 4 was the first full pass
  (recall turns T4/T7 went 0/2 → 2/2).

### Fixed

- **Audit 2 (fresh-eyes, 2026-11-13)** — full report in
  `AUDIT2_2026-11-13.md`: 2 P0s, 18 P1s, 25 P2s fixed
  (404 tests pass, race clean). Highlights: sandbox chroot escape
  closed with `pivot_root` + `umount2` detach; 44 denylist bypass
  cases (wrappers, `env -S`, `$'...'` quoting, variable
  indirection, `find -exec` wrappers, system-path file ops,
  interpreter payloads); dangling tool-call history no longer
  poisons later runs (400s); deferred tool calls no longer silently
  lost; Anthropic consecutive user turns merged; `Config.Save`
  atomic with 0600; audit head-failure resync; `file_read` fifo
  hang fixed; SSRF range gaps; Telegram Poll exits on fatal
  401/403/409; handler panics recovered in Poll; handler-error
  replies redacted before reaching the chat; DeepSeek errors
  correctly labelled (were "openai:"); `orion-seccomp` install
  wiring; Anthropic default model aligned (`claude-sonnet-4-5`).

- **Audit 3 (production readiness, 2026-11-13)** — full report in
  `AUDIT3_2026-11-13.md`: 100+ scripted Telegram-style conversations
  through the real agent loop (deterministic fake provider), 37
  provider-stress tests (timeouts, mid-stream drops, malformed
  streams, retries), 13 sandbox adversarial probes (PID/FS/network
  isolation, privilege escalation, seccomp filter proof with a
  discriminating modify_ldt check, bounded resource hogs), daemon
  soak (rc1 1h39m, RSS flat ~10 MB, no leak; 60s and 5-min SIGSTOP
  stalls survived), poll-transient logging (F1), SIGTERM drain up to
  10s (F2), session save on every exit path (F3), config `Load`
  tightens a lax 0644 config to 0600, daemon start/stop/restart
  against a fake Bot API. Verdict: NOT production-ready — the
  24–72h soak was not achieved, live-model volume is small (fake
  provider is the volume proof), and one new fail-open gap remains
  (see Known limitations). Close-out also fixed two test defects
  (a privilege-escalation assertion that matched no emitted string,
  a data race in the CPU-spin test) and corrected the record: the
  seccomp filter IS installed for the adversarial probes, and
  sethostname fails from namespace/capability scoping, not from
  the filter. 568 tests pass, race clean.

### Known limitations

- **Telegram round-trip: tested live 2026-11-13.** Irfan messaged
  @Magi420x_bot — DeepSeek replied in proper Bengali, and a weather
  question ran `shell_exec curl wttr.in` with a Bengali summary.
  Full agentic loop with tool use works. (Replies are only as good
  as the model: the 0.5B local model's replies were generic/confused;
  DeepSeek's were proper Bengali.)
- **Small local models recall unreliably.** The multi-turn live
  E2E test against `qwen2.5-0.5b-local` (0.5 B parameters) scored
  **0/2 full passes** before fact-aware recall; after recall
  injection, run 4 passed fully (T4/T7 recall 2/2). Tool turns are
  reliable (calculator, file_write, file_read: 2/2 each). See
  `docs/E2E_LIVE_OLLAMA.md`.
- **A real Hermes model needs a bigger machine.** `hermes3:3b` needs
  ~8 GB RAM and `hermes3:8b` needs ~16 GB; this VM has ~700 MiB free
  (~7.7 GiB total), so no Hermes model can run here. The loop was
  proven with the smallest viable instruct model instead, and the
  provider interface is unchanged — a Hermes model works the same
  way on a host with enough RAM (`ollama pull hermes3:8b`).
- **TypeScript UI is v0.2.** v0.1 is Go only (core, daemon, CLI);
  the desktop/web UI is out of scope for this release.
- **Sandbox is host-dependent.** It needs unprivileged user
  namespaces or root; on hosts without them, `shell_exec` with the
  sandbox enabled **refuses** commands (fail-closed) unless
  `sandbox.allow_unsandboxed_fallback=true` is explicitly set. The seccomp filter needs the
  `orion-seccomp` binary next to `orion` (installed by
  `install.sh`/`make build`); the arm64 filter table is
  header-checked but unrun on arm64 hardware.
- **Seccomp filter is fail-open when the launcher is missing.**
  When the sandbox is enabled and the filter is not disabled, but
  no `orion-seccomp` binary can be found (or the probe/install
  fails), the command runs with namespace/chroot containment but
  NO syscall filtering, with only a once-per-process warning. The
  next hardening phase should refuse in that case unless an
  explicit opt-out is set (same pattern as the sandbox fail-closed
  fix). See `AUDIT3_2026-11-13.md` §10 (F-A3-1).

## [0.1.0] — 2026-11-13

First release: a model-agnostic, local-first OS agent in Go, with a
Telegram gateway, a CLI channel, and a daemon.

### Added

- **Core agent loop** (`internal/core`): system prompt + recent
  memory → provider → tool calls → tool results → reply, with a
  max-step budget, per-iteration timeouts, context cancellation,
  streaming on the final turn, memory recording, and graceful
  shutdown.
- **Multi-provider LLM layer** (`internal/model`): one `Provider`
  interface (`Chat`, `Stream`) with three providers — OpenAI-compatible
  (OpenAI, vLLM, OpenRouter, Hermes-class open models), Anthropic,
  and Ollama (local) — plus a `NewProvider` factory and retries with
  backoff. Provider selection is config-only; no provider is
  hardcoded in the core.
- **10 tools** (`internal/tools`), all wired into the agent:
  `shell_exec` (denylist + timeout + 64 KB cap, opt-in sandbox),
  `file_read`, `file_write`, `file_edit`, `file_list`, `web_fetch`,
  `web_search` (real DuckDuckGo search), `calculator` (full expression
  parser), `datetime`, `process_list`.
- **Durable memory** (`internal/memory`): JSON file store with atomic
  writes, facts plus a 200-message conversation history, and
  case-insensitive fact search. Survives crashes and restarts.
- **Security** (`internal/security`):
  - One canonical token-based denylist (`IsBlockedCommand`) — binary
    basename resolution (`/bin/rm` == `rm`), `sudo`/`env`/`nice`/`time`
    wrapper stripping, per-segment checks on `;`/`|`/`&&`.
  - Secret scanner/redactor for OpenAI, Anthropic, GitHub, AWS,
    Telegram bot tokens, Bearer tokens, private keys, and generic
    `api_key`/`token`/`password`/`secret` assignments; CLI masks
    secrets in `orion config show`.
  - SHA-256 hash-chained, HMAC-signed audit log
    (`~/.orion/audit.jsonl`, 0600): tamper-evident, verifiable with
    `Verify()`.
  - File-tool confinement (symlinks resolved, `~/.ssh`, `~/.gnupg`,
    `~/.orion` unreachable), SSRF guard on `web_fetch`, scrubbed
    environment for `shell_exec`.
- **Sandbox** (`internal/sandbox`): opt-in Linux namespace + chroot
  confinement for `shell_exec` (stdlib only), with a graceful fallback
  to the denylist alone when namespaces are unavailable. See
  [SECURITY.md](SECURITY.md).
- **Telegram gateway** (`internal/messaging`): Bot API over HTTP with
  long polling, a stranger-chat allowlist (`telegram_allowed_chats`),
  and token redaction in gateway errors. Telegram ONLY — no WhatsApp.
  The CLI channel drives the same agent and memory.
- **Subagents** (`internal/subagent`): managed child agents with
  timeouts, a concurrency limit, cancellation, and panic recovery.
- **Skills** (`internal/skills`): Markdown skill registry loading
  `~/.orion/skills/*.md`.
- **CLI** (`cmd/orion`, 11 commands): `run`, `chat`, `config`
  (`show` / `set-provider` / `set-model` / `set-api-key`), `status`,
  `tools`, `memory`, `skills`, `subagent`, `telegram` (`test`/`poll`),
  `daemon`, `version`.
- **Daemon** (`cmd/oriond`): standalone `oriond` binary that polls
  Telegram until interrupted.
- **Packaging**: `install.sh` (build + install + default config,
  never overwrites an existing config), `Makefile` (`build`,
  `install`, `test`, `vet`, `fmt`, `version`, `clean`), and a systemd
  user unit (`oriond.service`). Build info (commit + date) is baked
  in with `-ldflags` and reported by `orion version`.
- **Docs**: `README.md`, `ARCHITECTURE.md`, `SECURITY.md`,
  `LANGUAGE_DECISION.md` (Go core, TypeScript UI deferred to v0.2),
  and `docs/E2E_LIVE_OLLAMA.md` (live-model evidence and hardware
  findings).

### Fixed

Loop bugs found by the live Ollama end-to-end runs and fixed in
`internal/core/agent.go` (each with a regression test):

- **First tool call per turn only.** The model issued dependent tool
  calls (calculator + file_write + file_read) in one turn; later
  calls were written before earlier results existed. The loop now
  executes only the first tool call of a turn and re-plans the rest
  next turn; the system prompt states the rule explicitly.
  (`TestRunExecutesOnlyFirstToolCallPerTurn`)
- **Deferred-call reminder.** After the first tool result the model
  sometimes answered directly instead of re-planning deferred calls.
  The loop now remembers deferred calls and injects a one-time
  reminder (tool names only) if the model answers while any are
  pending, discarding the premature answer.
  (`TestRunRemindsModelOfDeferredToolCalls`)
- **Temperature passthrough.** `config.Temperature` (nil = provider
  default, validated 0–2) is now passed through to every provider
  call. (`TestRunPassesTemperatureThrough`)
- Provider-side fixes from the hardening pass: OpenAI tool-call
  arguments sent as a JSON object (vLLM/Ollama/Hermes), Anthropic
  multi-tool turns, Ollama tool IDs, Telegram token leak via
  `*url.Error` (now redacted), file confinement symlink escapes,
  shell environment scrubbing, and the audit hash chain v2 format
  with length-prefixed fields and an HMAC-signed head.

### Known limitations

- **Small local models recall unreliably.** The multi-turn live
  E2E test against `qwen2.5-0.5b-local` (0.5 B parameters) scored
  **0/2 full passes**: tool turns are reliable (calculator,
  file_write, file_read: 2/2 each) but recall turns are not
  (post-restart recall of a stated fact: 0/2; the model confabulates
  numbers, e.g. answering "51" — the most recent computed value —
  instead of the stored favourite number "17"). Persistence, crash
  survival, restart reload, and every tool execution passed in both
  runs — the failures are the model's, not the loop's or the
  memory's. See `docs/E2E_LIVE_OLLAMA.md`. (Superseded by the
  fact-aware recall in [Unreleased], which passed a full run.)
- **A real Hermes model needs a bigger machine.** `hermes3:3b` needs
  ~8 GB RAM and `hermes3:8b` needs ~16 GB; this VM has ~700 MiB free
  (~7.7 GiB total), so no Hermes model can run here. The loop was
  proven with the smallest viable instruct model instead, and the
  provider interface is unchanged — a Hermes model works the same
  way on a host with enough RAM (`ollama pull hermes3:8b`).
- **TypeScript UI is v0.2.** v0.1 is Go only (core, daemon, CLI);
  the desktop/web UI is out of scope for this release.
- **Sandbox is host-dependent.** It needs unprivileged user
  namespaces or root; on hosts without them it falls back to the
  denylist alone with a warning. No seccomp filter yet.

[0.1.0]: https://github.com/irfan/orion/releases/tag/v0.1.0
