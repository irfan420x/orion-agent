# Changelog

All notable changes to Orion are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the
project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Audit 6 fixes (v0.2.0-rc11) — see `AUDIT6_2026-11-13.md`

Fixes the 3 P1s and the P2 batch from the fresh-eyes audit of rc10
(0 P0, 3 P1, 21 P2), plus the reproduced live approval-button
failure (callback never delivered — see below). **Verdict: NOT
production-ready** — the 24–72 h daemon soak (F-A3-4) is still open,
and F-A3-2 (no pprof/debug endpoint) is still open. Verified by the
release coordinator on the final commit: **1,101 tests pass, 0 fail,
4 skip (total 1,105), 21/21 packages, race clean 21/21, no data
race**; live DeepSeek proof `17*23+5=396` (rc10: 1,059 total: 1,055
pass, 0 fail, 4 skip).

- **F-A6-1 `/compress` unmetered** (`internal/app/chats.go`,
  `commands.go`): the metering sink is threaded into `handleCompress`
  and the `/compress` dispatch, so a chat at its daily limit can no
  longer run `/compress` repeatedly for free; regression test asserts
  `Budget.Usage` increments after a real `/compress`.
- **F-A6-2 skill END-marker injection** (`internal/skills/
  skills.go`): a skill body containing a line that parses as a
  `BEGIN SKILL` / `END SKILL` marker is rejected (or neutralised) at
  load time, so a body can no longer close its own delimiter early
  and inject a fake footer plus fresh instructions; regression test
  with the exact marker line in the body. Belt-and-braces:
  `formatSkillBody` also neutralises marker lines at render time,
  so a programmatically-constructed skill cannot blur the boundary
  either.
- **F-A6-3 `orion daemon` / `orion telegram poll` ungated and
  unisolated** (`cmd/orion/main.go`, `internal/app`): the permission
  gate is now wired into both entrypoints via the shared
  `app.NewPermissionGate` / `wireGate` helper, and `orion telegram
  poll` uses the per-chat handler; `oriond` keeps its own
  pre-existing gate wiring (verified in Audit 6 §1.4 — the security
  property, a gate on all three paths, now holds everywhere).
  Test asserts every command path that starts a Telegram loop has a
  non-nil gate.
- **Approval-button callback never delivered (live, rc10)**:
  callback queries were dispatched only after acquiring a slot on
  the turn-handler semaphore (8 slots), acquired in the poll loop
  before `go handleCallback` — with all 8 slots held by long turns
  (including 90 s approval waits) the poll loop blocked and the tap
  was never dispatched: two consecutive live cards expired with
  zero tap lines. Fixed with a dedicated callback semaphore plus
  per-update poll logging (`telegram poll: received update N
  callback_query …`), so the next incident is diagnosable in one
  line. Regression tests fail on rc10 code and pass with the fix.
- **P2 batch**: enum short-circuit in schema validation (F-A6-P2-11);
  `Close()` during MCP `Start()` leaking the server process
  (F-A6-P2-13); per-server MCP work dir instead of the shared
  `/work` (F-A6-P2-14); transcript corrupt counting for overlong
  lines and resumable migration (F-A6-P2-4/5); summary verification
  hardening (≥3 distinct shared tokens, whole-input cap) plus
  summariser redaction (F-A6-P2-1/2/3); subagent spend attribution
  for the mid-turn check and timed-out / late-finishing tasks
  (F-A6-P2-6/7); permission-gate `Resolve`/timer race closed with a
  single state transition (F-A6-P2-8); `### Tools` heading warning
  (F-A6-P2-9); skill hardlink refusal and open-then-fstat
  (F-A6-P2-10); symlinked `DataDir` resolved before the planning
  refusal (F-A6-P2-12); MCP system-prompt warning that MCP results
  are untrusted (F-A6-P2-19); value-shape redaction in `ScrubEnv`
  (F-A6-P2-20); rate limit on repeated near-cap MCP lines
  (F-A6-P2-21).
- **Flaky tests made deterministic** (F-A6-P2-17/18):
  `TestAdversarialNamespaceDeathKillsAll` polls for readiness (pids
  file + pgrep, bounded timeouts) instead of fixed sleeps;
  `TestSubagentManagerEndToEndUsageAndIsolation` gets a deterministic
  step budget (the flake is a test-design race: the parent's tight
  poll loop burns the 10-step budget before the child goroutine runs
  on a single CPU). The `TestW3BResolveTimerConsistency` stress test
  added in this release was itself scheduling-dependent (timer side
  never won under `-race`); it now alternates raced and
  timer-wins-by-construction iterations.
- **Docs + version (F-A6-P2-15/16)**: README no longer calls the repo
  private (it is public under MIT), 19 Telegram commands, `orion
  sessions` in the CLI table, MIT License mentions, MCP-confinement
  and per-chat budget/USD-cost sections; ARCHITECTURE tree and
  package list match `go list ./...` (21 packages); SECURITY.md
  covers MCP confinement, the permission gate, per-chat isolation,
  the owner tier, and the daily budget; `Version` is a `var` set from
  `git describe --tags --exact-match` in the Makefile and
  `install.sh`, with the source default as the untagged-build
  fallback (never "dev", never a stale tag) — the rc10 binary
  reported rc9 because `Version` was a `const` ldflags could not
  override.

#### Release checklist (before any push)

- [ ] `make build && ./bin/orion version` reports the version matching
  the tag (e.g. `0.2.0-rc11` for tag `v0.2.0-rc11`) — **the version
  string must match the tag before any push**. If it does not, do not
  push: check that `Version` is a `var` in `internal/version/
  version.go`, that the Makefile / `install.sh` ldflags pass `-X
  …version.Version=<tag without the leading v>`, and that the tag
  exists on the exact commit being built (`git describe --tags
  --exact-match`).
- [ ] Full suite green (`go test ./...`, `go test -race ./...`,
  `gofmt -l`, `go vet ./...`) with the counts recorded in this
  section; live DeepSeek proof (`17*23+5=396`) run.
- [ ] Docs claims re-verified against the code (package count via
  `go list ./...`, tool count, command count, version string).

## [0.2.0-rc10] — 2026-11-13

### Audit 5 fixes (v0.2.0-rc10) — see `AUDIT5_2026-11-13.md`

**License: MIT** — `LICENSE` added (commit `d2e6b67`); the repository
is public under the MIT License.

Fixes all 9 P1s and **all 38 P2s** from the fresh-eyes audit of
rc9; nothing deferred. **Verdict: NOT production-ready** — the
24–72 h daemon soak (F-A3-4) is still open, and F-A3-2 (no
pprof/debug endpoint) is still open. 1055 tests pass, 0 fail, 4
skip (3 Ollama live tests + 1 MCP process test), race clean,
21/21 packages. Live proof: glob with a 20×`**` pattern finds a
12-deep file and 17×23+5=396 in a 4.7 s DeepSeek turn (the glob
alone took 42.6 s before the fix).

- **F-A5-1 FIFO cap silent history loss** (`internal/core/compact.go`,
  `internal/core/recall.go`, `internal/session/transcript.go`):
  bounded emergency summarisation (16k token cap) when the
  breaker trips or pressure >2×, spill to KindDropped records,
  surfaced in CompactionStatus + `/status` + the terminal card.
- **F-A5-2 summary content verified** (`internal/core/compact.go`):
  summaries are rejected when they share zero tokens with the
  source (or fall below the length floor); the latest summary is
  injected at 2000 runes in its own recall slot.
- **F-A5-3 MCP newline-less flood** (`internal/mcp/client.go`):
  ReadSlice loop kills a server past the 16 MiB cap — a 96 MiB
  flood dies in ~0.1 s with heap <64 MiB.
- **F-A5-4 MCP servers confined** (`internal/mcp/sandbox.go`):
  MCP servers run sandboxed (fail-closed, default-on; proven
  live with a real probe server: config unreadable, HOME=/work,
  cwd=/work), 64 tools/server + 64 KiB schema cap.
- **F-A5-5 unmetered side-channel calls** (`internal/core/agent.go`,
  `recall_v2.go`, `extraction.go`, `compact.go`, `internal/app/commands.go`):
  selector, extraction, `/compress` and auto-compact provider
  calls all fold into the turn's usage via `AddSideChannelUsage`.
- **F-A5-6 budget pre-flight-only** (`internal/app/chats.go`):
  projection check refuses a turn whose projected input exceeds
  the remaining budget before the provider is called, plus a
  mid-turn abort (`SetTurnUsageLimitCheck`) after every provider
  response.
- **F-A5-7 unfetched subagent tokens** (`internal/app/chats.go`,
  `internal/subagent/subagent.go`, `internal/tools/subagent.go`):
  turn-end sweep charges finished-but-unfetched tasks exactly
  once — and tasks are now attributed to their owning chat at
  spawn, so one chat's sweep can never charge another chat's
  tasks (unattributed/CLI tasks are never swept).
- **F-A5-8 skill bodies escaping `## Tools`** (`internal/core/skills.go`,
  `internal/tools/skill.go`): skill bodies are delimited with
  BEGIN/END SKILL markers and a hierarchy instruction in the
  system prompt; `## Tools` is documented as anti-drift, not a
  security boundary.
- **F-A5-9 Glob `**` exponential blowup** (`internal/tools/search.go`):
  pattern caps (512 runes, 64 segments) + consecutive `**`
  collapsed + linear-time DP matcher — 20×`**` on a 12-deep
  tree went 42.6 s → 0.00 s.
- **P2-1…P2-8 compact/transcripts**: summary-append failure
  aborts the compaction instead of dropping silently; pressure
  recomputed after a drop; transcript reads via a `bufio.Reader`
  loop (overlong line = one corrupt line, not an unloadable
  session); resumable migration; resume loads summaries and
  reports corrupt lines + the 200 cap; transcript flock +
  shared-handle registry; breaker re-arms after a 5-min
  cooldown with a jump warning; `RedactSecrets` on transcript
  append.
- **P2-9…P2-17 hooks/MCP**: 64 tools/server + 64 KiB schema
  cap; decisionless JSON lines skipped, case-insensitive
  decision match; fail-closed on any non-zero PreToolUse exit;
  dead servers reaped and deregistered; concurrent startup with
  a 30 s total budget; hook args redacted; `Alive()` requires a
  started client; MCP results delimited + a system-prompt
  warning; atomic `Start` check-and-set.
- **P2-18…P2-23 cost/budget/approval**: per-(provider,model)
  day buckets + per-model pricing in `/usage`; budget persisted
  to `budget.json` under the data dir (wired in `Build`, so a
  daemon restart no longer resets every chat's spend); Resolve
  records the outcome under the mutex before delivery (timer
  race closed); the resolved card's buttons are cleared; the
  three silent approval log paths now log; the remainder is
  priced at the output rate.
- **P2-24…P2-27 skills/validation**: skill descriptions
  sanitised and quoted in the system prompt; `## Tools` heading
  variants normalised with a warning on unrecognised Tools-like
  headings; `LoadDir`/`LoadFile` refuse symlinks; the validator
  accepts `[]any`/`[]string`/`[]int`/`[]float64` enums, normalises
  numerics, recurses into nested schemas, and enforces
  minimum/maximum/minLength/maxLength/pattern.
- **P2-28…P2-36 search/planning**: grep include/exclude
  applied to symlinked files; symlink targets under `.git`
  skipped; hardlink refusal via `readFileBounded`; grep 32 MB
  total-bytes cap; grep 2000 rendered-lines cap + `task_list`
  pagination; planning flock around load-modify-save; O_NOFOLLOW
  + Lstat refuses symlinked state files and symlinked
  `chats/<id>` dirs; the subagent child's planning store is
  wired to the parent's (spawn → child `task_create` → parent
  `task_list` sees it); dir fsync after rename, `loadTodos`
  validates statuses, state files capped at 32 MB, grep
  single-file FIFO pre-check, dead symlinked-dir branches
  removed.
- **P2-37 flaky test-only data race**: `TestCallbackQueryReachesHandler`
  writes `gotSender` inside the mutex and reads it under the
  mutex; `-race -count=3` passes.
- **P2-38 docs staleness**: README/SECURITY.md/ARCHITECTURE.md
  no longer call the sandbox "opt-in"/"default off"; README
  gains Sandbox and Sessions (JSONL transcripts) sections; the
  stale rc7 `./orion` binary at the repo root is removed.
- **Coordinator integration**: `/resume` reports the cap,
  corrupt lines and summaries merged (daemon path wired);
  session rotation now closes the old transcript instead of
  leaking one fd + flock per rotation (chat + global paths).

### Feature gaps (v0.2.0-rc9) — see `orion-muse-sync/audit.md` §12/§8

Builds the capability features the external audit identified as
missing. **Verdict: NOT production-ready** — the 24–72 h daemon soak
(F-A3-4) is still open. 972 tests pass, 0 fail, 4 skip (3 Ollama live
tests + 1 MCP process test), race clean, 21/21 packages.

- **Skill wiring** (`internal/skills`, `internal/core/skills.go`,
  `internal/tools/skill.go`): the model sees skills in the system
  prompt (names + one-line descriptions, never bodies), invokes one
  with the `skill` tool (body lazy-loaded verbatim), and while a
  skill is active only its allowed tools may be used (hard block in
  `executeTool`). Skills load only from `~/.orion/skills`, never
  from repo cwd. Subagent children are deliberately skill-free.
- **Schema validation** (`internal/tools/validate.go`): central
  validation in `Registry.Execute` — `required` fields enforced,
  per-parameter JSON types + `enum` checked, clean `invalid_args`
  errors, never a panic; runs after the permission gate, before
  tool code and the denylist.
- **Grep + Glob tools** (`internal/tools/search.go`): confined
  content search and recursive glob on the same file roots as
  file_read/file_write (O_NOFOLLOW + fd verification, symlink
  escapes refused) — content search no longer needs `shell_exec`
  `grep`, which bypassed confinement.
- **Todo/Task tools** (`internal/tools/planning.go`): session
  checklist (`todo_write`) + durable per-chat task store
  (`task_create/get/list/update`), persisted under
  `chats/<chatID>/` (the daemon wires `SetDataDir` in `Build`).
- **Auto-compact + summary-before-drop** (`internal/core/compact.go`):
  compaction at ~80% of the history budget, 70% warning band (shown
  on the Telegram terminal card), summary of dropped turns written
  before the 200-message FIFO cap drops them, 3-failure circuit
  breaker (manual `/compress` resets it). Manual `/compress`
  unchanged.
- **Per-session JSONL transcript** (`internal/session/`,
  `internal/app/sessions.go`): append-only per-session transcript
  under `chats/<chatID>/sessions/` (CLI: `sessions/`), per-message
  timestamps, `/sessions` + `/resume` commands, `orion sessions`
  CLI; legacy `memory.json` turns migrate once, never deleted.
  Facts and typed memories stay shared per the P1-O3 boundary.
- **Hook bus** (`internal/hooks/`): PreToolUse/PostToolUse/Stop
  events, exit-code protocol (0 = continue, 2 = block), trust-gated
  to `~/.orion/hooks` only, scrubbed env, fail-closed PreToolUse.
  Subagent children get the parent's bus. Hook blocks surface on
  the Telegram terminal card.
- **MCP client (stdio)** (`internal/mcp/`): launches configured
  servers (config from the data dir only), exposes
  `mcp__<server>__<tool>` tools through the Tool interface with the
  permission gate + redaction + caps; server child env scrubbed.
  MCP server mode is out of scope. Children deliberately get no MCP
  tools (they run without the interactive gate; MCP tools are
  ClassNetwork).
- **USD cost** (`internal/cost/`): versioned price table
  (v2026-11-13, effective 2026-11-13) + per-chat/per-day USD in
  `/usage`, built on the rc8 per-chat daily token budget; unknown
  prices report "unknown", never $0. Subagent tokens are attributed
  to the chat footer (`1,240 tokens (incl. 300 subagent)`), and the
  per-chat budget is now enforced on the `orion daemon` path too
  (single enforcement point in `runChatTurn`).
- **Approval-button hotfix — live rc8 bug** (`cmd/oriond`,
  `internal/permissions`, `internal/messaging`): Irfan tapped
  Approve on a live approval card and the gate timed out with zero
  callback lines in the log. Reproduced end to end with the real
  PollStream + a long-poll-faithful fake Bot API + a real turn
  blocked on approval: **the delivery path is sound** — an
  in-window tap resolves (poll trace: long-poll returns the
  callback update, handler resolves, turn proceeds); the tap did
  not reach Resolve in-window (late tap, or lost before Telegram).
  Two real defects made it look like a broken button and made it
  undiagnosable, both fixed: (1) an approval card NEVER expired
  visually — its buttons stayed live-looking after the 90s window,
  so a tap on the dead card (answered only with a transient
  "expired" toast) looked exactly like a broken button. The gate
  now has an `OnExpire` hook; the daemon edits the card in place
  on timeout/cancel (text → "⏰ Expired…", keyboard REMOVED via
  new `EditMessageTextMarkup`). (2) The callback path logged
  NOTHING on success or expiry, and the prompt logged nothing —
  a tap that never arrived and a tap on a dead card looked
  identical in the log. Every prompt, tap and outcome is now
  logged with the request ID (`oriond: approval prompt/tap/
  expired/resolved`). Regression tests: live-shape approve
  round-trip (long-poll, tap in a later cycle), card-expiry
  E2E (no tap → card edited expired + keyboard removed),
  realistic raw Telegram callback payload through GetUpdates.

### External-audit fixes (v0.2.0-rc8) — see `orion-muse-sync/audit.md`

Fixes the security findings and docs staleness from the external
audit (verified against rc7: 9 of its 10 findings genuinely open;
Audit 4's rc7 fixes closed none of them). **Verdict: NOT
production-ready** — the 24–72 h daemon soak (F-A3-4) is still open.
809 tests pass, 0 fail, 3 skip (Ollama live tests), race clean,
17/17 packages.

- **P0-O1 — sandbox ON by default** (`internal/sandbox`,
  `internal/config`, `install.sh`): a fresh install and any config
  that omits the sandbox section confine `shell_exec` to the
  namespace/pivot-root sandbox with the seccomp filter; only an
  explicit `sandbox.enabled=false` (or `ORION_SANDBOX=off`) opts
  out, and every unsandboxed run is logged. A missing
  `orion-seccomp` launcher still refuses commands fail-closed
  (F-A3-1) via `ErrSeccompUnavailable`. `make build` and
  `install.sh` now build the launcher (static, `CGO_ENABLED=0`)
  and install it next to `orion`/`oriond`; `install.sh` refuses a
  launcher-less install. Live config enabled
  (`sandbox.enabled=true`) and daemon restarted on the rc8 binary.
- **P1-O2 — permission/confirmation gate** (`internal/permissions`,
  `internal/core`): destructive (`shell_exec`, `file_write`,
  `file_edit`, `subagent_spawn`) and network (`web_fetch`,
  `web_search`) tools need approval — Telegram inline
  Approve/Deny/Always/Never with persisted rules
  (`permissions.json`, 0600), 90 s timeout, clean deny. The shell
  denylist stays absolute and can never be approved. CLI runs stay
  ungated (denylist only).
- **P1-O3 — per-chat isolation** (`internal/app`,
  `internal/messaging`): each chat has its own agent, history,
  provider/model override, session memory, usage and pause
  (`chats/<chatID>/`); `/new`, `/model`, `/pause`, `/undo`,
  `/compress`, `/save`, `/status`, `/memory` act on the caller's
  chat only. Long-term facts and typed memories stay shared (the
  user's knowledge base).
- **P1-O4 — sender check + owner tier** (`internal/messaging`,
  `cmd/oriond`): the sender's user ID must be allowlisted, not just
  the chat; `/restart` is owner-only (owner = first
  `telegram_allowed_chats` entry). Approval taps accepted only from
  the requesting sender or the owner.
- **P2-O5..O10** (`internal/config`, `internal/tools`,
  `internal/security`, `internal/budget`): `allow_all_telegram_chats`
  time-boxed to 10 minutes from daemon start + loud startup warning;
  `scrubEnv` strips credential-bearing proxy vars; file tools use
  `O_NOFOLLOW` + fd-path verification (TOCTOU closed); audit-log HMAC
  key moved out of the log dir with cross-process `flock`; per-chat
  daily token budget (default 100,000/UTC day); `ip`, `ifconfig`,
  `iptables`, `ip6tables`, `route`, `nc`, `ncat` added to the denylist.
- **Docs**: ARCHITECTURE header (rc8, 14 tools, `agents` in CLI list),
  CHANGELOG DeepSeek-default line corrected (code default is ollama),
  MEMORY_V2_PLAN test count, README CLI table + Tools section (14
  tools), SECURITY.md HMAC checkpoint documented, AUDIT3 corrections
  (36 provider-stress tests, RSS band from the CSV).

### Audit 4 (v0.2.0-rc7) — see `AUDIT4_2026-11-13.md`

Fresh-eyes production-readiness audit of rc6 plus the fresh-install
run (F-A3-3, now closed). No P0 found in any of the three workstreams
(A security, B stability/correctness, C fresh install). All eight P1s
fixed in rc7 (below) and re-proven with regression tests. **Verdict:
NOT production-ready** — the 24–72 h daemon soak (F-A3-4) is still
open (longest continuous run ~1h39m) and the live config still runs
with the sandbox disabled (A P1-5, operational decision, not a code
fix). Live token accounting verified against real DeepSeek usage: a
trivial turn costs 3,453 tokens, footer == provider total exactly.
Daemon incident during the audit: a VM/runtime restart (~05:10 UTC)
killed the rc6 daemon silently; state files intact, daemon restarted
(rc6, then rc7). P2 backlog recorded in the report.

### Fixed — Audit 4 (v0.2.0-rc7)

All Audit 4 P1 findings (workstreams A security, B stability, C
install polish) are fixed and re-proven with regression tests; no
P0s were found. 717 tests pass, 0 fail, 3 skip (Ollama live tests),
race clean, 14/14 packages.

- **A P1-1 — denylist bypass via variable/command-substitution path
  indirection** (`internal/security/security.go`): redirect targets
  and file-op operands (`cp`/`mv`/`ln`/`chown`/`chgrp`/`mkdir`/
  `rmdir`, `tee`) are now expanded through the per-segment variable
  assignments (`$var`, `${var}`, `$d/passwd`) before the destructive-
  target check, and an unresolvable `$` expansion or a command-
  substitution target (`> $(echo /etc/passwd)`) is refused outright.
  `${...}` is kept as one token by the tokenizer so it can be
  resolved. `f=/etc/passwd; echo pwned > $f`, `> ${f}`,
  `echo x | tee $f`, `cp/mv/ln -sf … $f`, `> $d/passwd` are all
  refused now; variable indirection to ordinary files stays allowed.
- **A P1-2 — `chmod` of system paths** (`internal/security/
  security.go`): any `chmod` with a system-path operand is refused
  whatever the mode (`chmod 644 /etc/shadow`, `chmod 4755 /bin/sh`,
  `chmod 000 /etc/passwd`, `chmod u+s /bin/bash`, `chmod g+r
  /etc/shadow`), mirroring `chown`. Ordinary chmod is unchanged.
- **A P1-3 — Memory v2 prompt injection** (`internal/memory/
  typed.go`, `feedback.go`, `migrate.go`, `internal/core/recall_v2.go`,
  `extraction.go`, `internal/app/commands.go`, `memory_v2.go`,
  `internal/messaging/commands.go`, `cmd/oriond/main.go`): every
  typed memory now carries frontmatter provenance (`user` |
  `migrated` | `model-derived`; legacy files without it are
  grandfathered as user-written). `TypedStore.Save` refuses feedback
  with migrated/model-derived provenance; `NewFeedbackMemory`
  (the only feedback constructor) marks user provenance.
  `MigrateFacts` marks migrated memories `project` + `migrated` and
  applies the what-not-to-save gate (derivable facts skipped).
  Session extraction and `/compress` apply the gate to model-derived
  text, and `/compress` no longer tells the summariser to "treat
  [session memory] as established" — it is untrusted recorded text.
  Recall demotes non-user feedback to topical (selector + surfaced
  set, never always-on) and renders all injected memories as quoted
  data under a header stating they are not instructions; only
  user-provenance feedback is standing guidance. The only way to
  create a feedback memory is the explicit user command
  `/memory feedback <name> :: <rule> :: <why> :: <how>` (wired in
  `buildCommandEnv`; `App.SaveFeedbackMemory`). Trade-off recorded:
  legacy files without a provenance field are still trusted as
  user-written — only explicitly-marked non-user provenance is
  distrusted.
- **A P1-4 — `web_search` SSRF guard** (`internal/tools/web.go`,
  `tools.go`): search now routes through the same `fetchSession`
  pinning/redirect validation as `web_fetch` (per-search session,
  trusted egress CIDRs plumbed from config). A search endpoint that
  302-redirects onto a private address is refused before the dial;
  the private body never surfaces as a result. Existing search
  tests that hit local servers now use the test-only private-host
  override, as the `web_fetch` tests do.
- **B P1-1 — `/model` switch dropped Memory v2**
  (`internal/app/memory_v2.go`, `app.go`, `commands.go`): the wiring
  (typed source, session appender, drift root) is factored into one
  shared step used by both `Build` and `SwitchProvider`, so a switch
  no longer silently loses typed recall/feedback/drift/extraction.
  The drift root is now set explicitly at wiring time (nearest
  ancestor with a `go.mod`, logged) instead of relying on the daemon
  happening to start in the repo.
- **B P1-2 — concurrent chats corrupted each other's cards/footers**
  (`internal/core/agent.go`, `events.go`, `internal/app/commands.go`):
  `Agent.RunWithEventHandler` installs a per-run handler inside
  `run()` while `runMu` is held; `emit` prefers it over the
  agent-wide handler. `HandleStream` uses it, so two concurrent
  chats each receive exactly their own `TurnFinished` (1/1, proven
  with a 300 ms slow-provider race test). Concurrency is kept —
  chats are not serialised.
- **B P1-3 — drift check permanently killed valid memories**
  (`internal/core/drift.go`, `recall_v2.go`): prose `--flag` /
  `Capitalised(` references only count as tree claims when the
  memory shows code context (a backticked span or a bare file path);
  `--prod`, `--verbose`, `Start(`, `--no-preserve-root` with no code
  context are no longer stale. Defence in depth: staleness is
  persisted only on missing-file evidence — flag/symbol-only drift
  failures drop the memory for that turn only and are re-verified
  next turn, so they can resurface. (One pre-existing test case,
  "bare prose flag is stale", was updated to the new rule; the
  backticked-flag case still expects stale.)
- **Install polish** (`cmd/orion/main.go`, `cmd/orion/main_test.go`,
  `oriond.service`, `README.md`): `ORION_TELEGRAM_BASE_URL` is now
  honoured by `orion daemon` and `orion telegram poll` (and `test`),
  not just `oriond` (F7); usage/error strings list `deepseek`
  alongside the other providers (F8); `orion config set-api-key
  <provider> -` reads the key from stdin so it never lands in shell
  history or the process list (F9, documented in README and
  `--help`); `oriond.service` Documentation URL now points at
  `https://github.com/irfan420x/orion-agent` (F4-remainder). The
  workstream-C doc fixes already in the tree (README quick-start,
  install.sh) are included in this release.

### Added

- **Subagent delegation — the model can now delegate** (v0.2.0-rc6):
  the subagent engine (present since v0.1 but unreachable — no tool
  exposed it, so the bot honestly answered "0 sub-agents") is now
  wired into the agent loop. Four model-facing tools
  (`internal/tools/subagent.go`): `subagent_spawn` (brief + optional
  timeout), `subagent_list`, `subagent_result`, `subagent_stop`,
  with schemas describing when to delegate (independent research,
  parallel work, long-running jobs) vs doing the work directly.
  Each spawn runs a REAL agent loop in a fresh child `core.Agent`
  (`internal/app/subagent.go`): same provider/config/tools (minus
  subagent tools, so no recursion), own ephemeral history, read-only
  typed memories, no shared session writes, same denylist/sandbox
  rules; maxConcurrent 4, timeouts, cancellation and panic recovery
  unchanged. `/agents` (Telegram + `orion agents`) renders real
  subagents (id, status, brief, duration, tokens, result summary).
  Consumed results (DONE and FAILED — their tokens were genuinely
  spent) are charged exactly once to the parent turn's usage
  (`internal/core/agent.go` accumulator, `tools.UsageSinkFunc`
  resolving the CURRENT agent, so a provider switch no longer
  orphans the sink). Live proof (DeepSeek, scratch session): the
  model spawned subagent `f3d2f2386f5a4d90`, which ran its own loop
  (file_write attempt, 17*23+5 = 396, reasoned report) and its
  result came back into the parent turn with id, duration and
  tokens (5,949). 693 tests pass, 0 fail, 3 skip (Ollama live
  tests), race clean.

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
- **Seccomp-missing fail-open closed (Audit 3, F-A3-1)**
  (`internal/sandbox/seccomp.go`, `sandbox.go`,
  `internal/tools/shell.go`): when the sandbox is enabled and the
  filter is not disabled, but no usable `orion-seccomp` launcher
  can be found (or the launcher probe/staging fails), the command
  used to run with namespace/chroot containment but NO syscall
  filtering, with only a once-per-process warning. It is now
  REFUSED (`denied`, error wraps the new
  `sandbox.ErrSeccompUnavailable` sentinel and names the opt-in)
  and runs nothing, unless the operator explicitly opts into
  unfiltered runs with the new `sandbox.allow_without_seccomp=true`
  (default false) — in which case every degraded run is logged,
  naming the opt-in. `sandbox.disable_seccomp=true` still runs
  without the filter and is never a refusal (explicit disable is
  a deliberate choice). The two opt-ins govern different
  degradations and neither implies the other: a missing filter is
  refused even with `allow_unsandboxed_fallback=true` (the
  fallback opt-in covers a sandbox that cannot be established at
  all, and letting it run the command fully unsandboxed would be
  a worse degradation). The availability probe is unaffected (it
  runs with the filter disabled, so it never logs a misleading
  missing-filter warning). Tests:
  `TestSeccompMissingLauncherRefused`,
  `TestSeccompMissingLauncherOptInRuns`,
  `TestSeccompDisabledRunsWithoutLauncher`,
  `TestPrepareWithDisabledSeccompLogsNoWarning`,
  `TestShellExecSandboxRefusesWithoutSeccompLauncher`,
  `TestShellExecSandboxSeccompOptInRunsDegraded`,
  `TestShellExecSandboxDisableSeccompRuns`,
  `TestShellExecSandboxSeccompFallbackComposition`.
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
- **Version + docs sync** (`internal/version`): `Version` is
  `0.2.0-rc7` (the rc2-era sync fixed it from `0.1.0`, when the tag
  was already `v0.2.0-rc2`);
  README status, SECURITY.md (seccomp launcher is built/installed
  by `install.sh`/`Makefile`; Telegram authorisation is refusal,
  not a warning; sandbox fallback is fail-closed) and
  ARCHITECTURE.md corrected.

### Fixed

- **Footer token accounting — the 59,635 was real, not a counting
  bug** (v0.2.0-rc6): a trivial one-question turn reported
  `deepseek-chat · 59,635 tokens · 1 steps` because a 96,929-rune
  Google News Bengali RSS feed sat in the recent-10 history window
  and was re-sent every turn, while the old `runes/4` token estimator
  undercounted dense content 2.34× (24,232 estimated vs 56,742 real),
  so the 32,768-token history budget never actually bound real
  tokens. `CountTokens` (`internal/model/types.go`) is now a
  BPE-shaped, live-calibrated estimator (short word runs 1 token,
  long varied runs ~1.4 runes/token, repetitive runs /4, other runes
  1 token): validated vs DeepSeek real counts — prose 1.03×, code
  1.06×, Bengali 1.54×, RSS feed 1.06×, conservative everywhere.
  The footer format is unchanged (`model · tokens · steps ·
  duration`); the number is now honest. Live before/after on the
  same question and a frozen copy of the live session: 59,732 →
  3,337 tokens (1 step), history trim now drops the giant feed
  units. Streaming double-counting was ruled out (the Telegram path
  does not stream, and `Stream` never sums chunk usage).

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
  (defaults `https://api.deepseek.com/v1` + `deepseek-chat`), the
  default provider in the live operator config
  (`~/.orion/config.json`) — not the code default, which remains
  `ollama` (`internal/config/config.go`). Live-verified: `TestLiveDeepSeek` PASS
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
  `install.sh`/`make build`); with the filter enabled and no usable
  launcher, commands are **refused** unless
  `sandbox.allow_without_seccomp=true` is explicitly set (Audit 3,
  F-A3-1, closed in [Unreleased]); the arm64 filter table is
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

[0.1.0]: https://github.com/irfan420x/orion-agent/releases/tag/v0.1.0
