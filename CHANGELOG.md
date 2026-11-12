# Changelog

All notable changes to Orion are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the
project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
  memory's. See `docs/E2E_LIVE_OLLAMA.md`. Fact-aware recall
  injection is in progress.
- **A real Hermes model needs a bigger machine.** `hermes3:3b` needs
  ~8 GB RAM and `hermes3:8b` needs ~16 GB; this VM has ~700 MiB free
  (~7.7 GiB total), so no Hermes model can run here. The loop was
  proven with the smallest viable instruct model instead, and the
  provider interface is unchanged — a Hermes model works the same
  way on a host with enough RAM (`ollama pull hermes3:8b`).
- **Telegram round-trip untested.** The gateway verifies the token
  (`getMe`) but no chat has messaged the bot yet, so the full
  message → agent → reply round-trip has not been exercised live.
- **TypeScript UI is v0.2.** v0.1 is Go only (core, daemon, CLI);
  the desktop/web UI is out of scope for this release.
- **Sandbox is host-dependent.** It needs unprivileged user
  namespaces or root; on hosts without them it falls back to the
  denylist alone with a warning. No seccomp filter yet.

[0.1.0]: https://github.com/irfan/orion/releases/tag/v0.1.0
