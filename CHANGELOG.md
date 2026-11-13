# Changelog

All notable changes to Orion are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the
project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Telegram response style — live terminal card + command menu**
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
  namespaces or root; on hosts without them it falls back to the
  denylist alone with a warning. The seccomp filter needs the
  `orion-seccomp` binary next to `orion` (installed by
  `install.sh`/`make build`); the arm64 filter table is
  header-checked but unrun on arm64 hardware.

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
