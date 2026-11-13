# Orion — Audit 2, Part: App / Messaging / CLI (fresh-eyes) — 2026-11-13

Auditor: fresh-eyes auditor (subagent). Scope (only these files):

- `internal/messaging/` — `telegram.go`, `cli.go`
- `internal/app/` — `app.go`
- `internal/subagent/` — `subagent.go`
- `internal/skills/` — `skills.go`
- `internal/version/` — `version.go`
- `cmd/orion/main.go`, `cmd/oriond/main.go`
- `install.sh`, `Makefile`, `oriond.service`, `README.md`, `CHANGELOG.md`
  (read-only review; no edits made there)

Every in-scope file was read line by line, including all in-scope
test files. No secret (API key, Telegram bot token) appears anywhere
in this report, in any test, or in any fix. The test Telegram token
lives only in `~/.orion/config.json` and was never printed.

## Tree state while auditing — read this first

Sibling auditors were actively editing the tree **during** this
audit (same as the first audit). Files outside my scope changed
under me; my scope files did not. Two failures in *other* scopes
were observed at different times and are **not** regressions from
this audit's changes (verified: the failing packages pass on the
committed tree, and my diff touches only `telegram.go`,
`skills.go`, `cmd/orion/main.go` plus three new `audit2_test.go`
files):

- `internal/model`: `TestAnthropicUnknownRoleMapsToUser` /
  `TestAnthropicEmptyRoleMapsToUser` failed while a sibling's
  uncommitted `anthropic.go` edit (same-role message merging via
  new `appendAnthropicMessage`) was in flight; the old tests expect
  2 messages, the merge produces 1. The model-scope owner must
  reconcile the new behaviour with those tests. Later in the audit
  the model package passed again.
- `internal/security`: `TestAudit2Probe` (sibling probe test file
  `audit2_probe_test.go`, not yet committed) fails on denylist
  probes such as `exec rm -rf /`, `chroot / rm -rf /`,
  `nsenter -t 1 -m rm -rf /`. Security-scope work, not mine.

My scope packages were green at every check during the audit.

## Verification performed

- `gofmt -l ./cmd ./internal` — clean.
- `go build ./...` — pass. `go vet ./...` — clean (at the time of
  the scope check; the full-tree vet is hostage to siblings'
  in-flight edits — see above).
- `go test ./internal/messaging/ ./internal/app/ ./internal/subagent/ ./internal/skills/ ./internal/version/ ./cmd/orion/ ./cmd/oriond/ -count=1 -race` — **7/7 packages pass**, including 15 new tests added by this audit.
- Full suite at audit time: 341 tests passing before my changes
  (matches README), 13/14 packages ok after my changes; the one
  failing package (`internal/security`, `TestAudit2Probe`) is a
  sibling's uncommitted probe, outside my scope.
- Smoke-tested the built `orion` binary: unknown command prints
  error + usage to stderr only, exit 2; `config set-model "   "`
  errors with "model name must not be empty", exit 1.

## Severity counts (this part)

- P0: 0
- P1: 3 — all fixed, each with a test
- P2: 9 fixed, each with a test (where testable) + 8 recorded
  (read-only files / open follow-ups, no fix in this audit)

---

## P1 — fixed

### P1-A — Telegram `call`: unbounded error description (messaging)
- `internal/messaging/telegram.go` (was ~`call`, non-OK branch).
- Evidence: on a non-OK response the description fell back to the
  raw response body (`desc = strings.TrimSpace(string(data))`)
  with no cap. The body is read with a 10 MiB `LimitReader`, so a
  single gateway error could be a ~10 MB string. That string is
  printed to stderr by `orion telegram test` / `oriond`, and Poll
  handler errors are sent back to the Telegram chat. A hostile or
  buggy upstream returning a large HTML error page turns one
  failure into multi-megabyte log spam (and a multi-megabyte
  message to the user).
- Fix: new `apiError` type + `truncateErrorDescription`
  (512 runes, rune-based so the result stays valid UTF-8).
  `call` now returns `&apiError{Method, StatusCode, Code,
  Description}` (wrapped by the existing token redaction). The
  message shape is byte-identical to the old `fmt.Errorf` calls
  (`HTTP 401, error 401: Unauthorized` / `HTTP 502: ...`), so no
  existing test or caller changes behaviour. The decode-error
  path never embedded the body, so it needed no change.
- Tests: `TestCallCapsErrorDescription` (raw-body fallback and
  JSON-description subtests; a 100 KB error body yields an error
  under 2 KB with a `...` marker).

### P1-B — Poll retries forever on fatal Bot API errors (messaging)
- `internal/messaging/telegram.go:Poll` (was: any `GetUpdates`
  error → backoff ≤5s → retry forever).
- Evidence: a revoked token (401), a blocked bot (403), or a
  second poller holding the connection (409 Conflict) can never
  heal by retrying, yet Poll looped on them forever. The daemon
  looked alive while polling a dead token every few seconds, and
  the operator got no signal. (The daemon's `GetMe` at startup
  catches a bad token *at start*; a token revoked mid-run, or a
  second `oriond` started by accident, hit this path.)
- Fix: `call` returns the typed `*apiError` (see P1-A);
  `apiError.isFatalForPoll()` is true for 401/403/409 (Telegram
  `error_code` first, HTTP status as fallback when the response
  carried no code). Poll returns a wrapped
  `unrecoverable Bot API error` immediately for those, and keeps
  the backoff loop for everything transient. `errors.As` reaches
  the `*apiError` through the `redactedError` wrapper (its `As`
  forwards), verified by the test.
- Tests: `TestPollExitsOnFatalAPIError` (401 and 409 subtests;
  Poll returns promptly with "unrecoverable", handler never
  called).

### P1-C — `orion telegram test` built the full app (CLI)
- `cmd/orion/main.go:cmdTelegram` (was: `buildApp()` before the
  `test`/`poll` switch).
- Evidence: token verification needs config + token only, but the
  command constructed the provider, tools, memory store, agent,
  and subagent manager first. An unrelated failure — a corrupt
  `memory.json`, an agent that cannot be constructed — made a
  token check fail with the wrong error. Reproduced in the test:
  with corrupt `memory.json` and no token, the old path failed
  with `app: memory: corrupt file ...` instead of reporting the
  missing token.
- Fix: `test` now loads config directly (`config.Load("")`) and
  calls `GetMe`; `poll` still builds the full app (it genuinely
  needs the agent). Residual coupling, noted not fixed: config
  load still validates the default provider (including its API
  key), so a config whose default provider is key-less still
  blocks the token check — fully decoupling that needs a
  config-layer change (out of this audit's scope; see "Still
  open").
- Tests: `TestCmdTelegramTestSkipsProvider` (corrupt memory +
  no token → "no token configured", no mention of memory).

---

## P2 — fixed

### P2-A — Ctrl-C exits 1 with "orion: context canceled" (CLI)
- `cmd/orion/main.go:main` (was: any non-nil error → print +
  exit 1).
- Evidence: `orion chat` / `run` / `subagent run` return
  `ctx.Err()` on SIGINT/SIGTERM, so an ordinary Ctrl-C printed
  `orion: context canceled` and exited 1 — while the daemon path
  already exits 0 cleanly on cancel (`Poll` returns nil). A
  user-initiated stop looked like a failure.
- Fix: new `exitCodeFor(err)` — nil → 0, `context.Canceled`
  (including wrapped) → 0 and silent, anything else → 1 with the
  message. `context.DeadlineExceeded` stays an error (a timeout
  is a real failure, not a user stop).
- Tests: `TestExitCodeFor` (nil, bare/wrapped Canceled,
  generic error, DeadlineExceeded).

### P2-B — Unknown command prints usage to stdout (CLI)
- `cmd/orion/main.go:main` default case (was: `usage()` →
  `fmt.Printf` to stdout after an stderr error line).
- Evidence: `orion bogus` split its output across streams — the
  error on stderr, the usage on stdout — so `orion bogus 2>err`
  still printed usage to the terminal and scripts capturing
  stdout got usage text on an error path.
- Fix: `usage()` now delegates to `usageTo(w io.Writer)`;
  `--help` writes stdout, the unknown-command branch writes
  stderr. Verified by smoke test: stdout 0 bytes, stderr holds
  error + usage, exit 2.
- Tests: `TestUsageToWritesToWriter` (content check; the
  stream routing itself was verified by the smoke test above).

### P2-C — A handler panic in Poll kills the daemon (messaging)
- `internal/messaging/telegram.go:Poll`, `handle` closure (was:
  no `recover` anywhere on the handler path).
- Evidence: each update handler runs in its own goroutine with
  no recover. A panic in the handler (the agent loop) crashed
  the whole `oriond` process — systemd restarts it, but the
  update is lost, polling stops until restart, and one bad
  message can kill the daemon repeatedly. (The subagent manager
  already recovers executor panics; the Telegram handler path
  did not.)
- Fix: `recover` in `handle` (registered after the semaphore
  release, so LIFO order runs it first and the slot is still
  released). The panic is reported to the chat exactly like a
  handler error (`Sorry, I hit an error: <value>`) and polling
  continues.
- Tests: `TestPollRecoversFromHandlerPanic` (chat 1 panics,
  chat 2 still gets its reply; Poll stays alive until cancel).

### P2-D — Concurrent GetUpdates calls could deliver duplicates (messaging)
- `internal/messaging/telegram.go:GetUpdates` (was: offset
  guarded by a mutex, but the read → request → advance sequence
  was not atomic).
- Evidence: the type doc says the gateway is "safe for concurrent
  use: the getUpdates offset is guarded by a mutex". That was
  only half true: two concurrent callers could both read the same
  offset before either advanced it, and Telegram would return the
  same updates to both (double agent runs for one message).
  `Poll` itself calls serially, so this needed a second caller
  (an embedder, a test, a future second poller) to bite.
- Fix: new `fetchMu sync.Mutex` held across the whole
  GetUpdates (offset read, request, offset advance). A second
  caller waits for the in-flight long poll instead of
  duplicating it. `Offset()` (separate mutex) stays responsive.
- Tests: `TestGetUpdatesConcurrentCallsDoNotDuplicate` (fake
  answers only at offset 0; two racing callers deliver exactly
  1 update total, offset ends at 6).

### P2-E — `config set-model` accepted a whitespace-only model (CLI)
- `cmd/orion/main.go:cmdConfig`, `set-model` (was: only
  `len(args)` checked).
- Evidence: `orion config set-model "   "` stored junk — the
  sibling `set-api-key` already rejects blank keys with
  `TrimSpace`, `set-model` did not. `Validate` only checks the
  *effective* model (per-provider wins), so the junk sat in the
  config until the per-provider model was ever cleared, when it
  would surface as a confusing validation failure.
- Fix: `TrimSpace` emptiness check returning `model name must
  not be empty`, mirroring `set-api-key`.
- Tests: `TestCmdConfigSetModelRejectsBlank` (spaces and tab).

### P2-F — A `### ` sub-heading did not end the `## Tools` section (skills)
- `internal/skills/skills.go:parseSkill` (was: only `## ` and
  `# ` headings ended the section).
- Evidence: `### Notes` inside `## Tools` matched neither
  prefix (`"### "` starts with `"##"` but not `"## "`, nor
  `"# "`), so bullets under a `### Notes` subsection were parsed
  as tools. A skill with a notes subsection silently gained
  phantom tools.
- Fix: new `headingLevel` helper (ATX 1–6, requires a space
  after the hashes); any heading ends the Tools section unless
  it is a new `## ` section header. A `#hashtag` line is not a
  heading and no longer ends anything either.
- Tests: `TestParseToolsSubheadingEndsSection`,
  `TestHeadingLevel` (table incl. `#######`, `#hashtag`,
  `##nospace`).

### P2-G — Skill description could be a code-fence line (skills)
- `internal/skills/skills.go:parseSkill` description scan (was:
  first non-empty, non-heading, non-bullet line).
- Evidence: a skill file opening with a fenced code block took
  the fence marker itself (` ```go `) as its description, and a
  numbered list (`1. First step`) the same way. Both are not
  prose.
- Fix: the scan now tracks fenced blocks (`isFenceLine`,
  ```` ``` ```` / `~~~`, toggled) and skips their contents, skips
  numbered-list lines (`numberedItem`), and breaks only on real
  ATX headings (`headingLevel`).
- Tests: `TestParseDescriptionSkipsCodeFence` (fence-then-prose
  and fence-only cases), `TestParseDescriptionSkipsNumberedList`.

### P2-H — Numbered list items were not recognised as tools (skills)
- `internal/skills/skills.go:parseSkill` tools scan (was:
  `- ` / `* ` only).
- Evidence: `1. exec` / `2) read` under `## Tools` were ignored,
  though numbered lists are the natural way to write a tool list.
- Fix: `numberedItem` parses `N. ` / `N) ` items (digits,
  marker, required space); they go through the same
  suffix/backtick/dedupe handling as bullets.
- Tests: `TestParseToolsNumberedItems`, `TestNumberedItem`
  (incl. `1.exec` rejected, `1. ` empty item).

### P2-I — Nil `*Registry` panicked (skills)
- `internal/skills/skills.go:Register/Get/List/Search` (was:
  `r.mu.Lock()` / `RLock()` with no nil check).
- Evidence: any of these on a nil `*Registry` dereferenced the
  mutex and panicked. Callers must use `NewRegistry`, but a nil
  receiver is a one-line mistake away and the failure mode
  (panic, not error) was disproportionate.
- Fix: `Register` returns an error on nil; `Get` reports
  not-found; `List`/`Search` return nil. `LoadDir` on nil now
  reports errors via `Register` instead of panicking; `LoadFile`
  never touched the receiver and already worked.
- Tests: `TestNilRegistryDoesNotPanic` (all four methods plus
  `LoadDir`).

---

## P2 — recorded, not fixed in this audit

### P2-J — No retry on 429 for sendMessage (messaging)
- `internal/messaging/telegram.go:sendMessageOnce`. Telegram
  rate-limits sends (429 + `retry_after`); a 429 fails the send
  outright, so a reply can be lost under flood. The apology path
  is best-effort anyway, and a retry needs `retry_after` parsing
  plus a retry budget — small follow-up, not done here.
  **Open.**

### P2-K — Handler error text sent to the Telegram user is not redacted (messaging)
- `internal/messaging/telegram.go:Poll` (`Sorry, I hit an
  error: %v`). Tool output/errors are redacted at the registry
  layer, but a raw provider/config error could in principle
  reach the chat verbatim. The recipient is the operator (the
  allowlisted chat), so the exposure is low; still, routing this
  through `security.RedactSecrets` would be cheap defence.
  **Open.**

### P2-L — Non-allowlisted chats are dropped silently (messaging)
- `internal/messaging/telegram.go:Poll`. Dropped stranger
  messages produce no log line or counter — the gateway has no
  logger. An operator cannot tell whether strangers are probing
  the bot. Needs a logger or a drop counter plumbed into the
  gateway. **Open.**

### P2-M — `StatusPending` is never assigned (subagent)
- `internal/subagent/subagent.go:44`. Tasks go straight to
  `StatusRunning` in `Spawn`; the `pending` value is dead. Either
  remove it or use it (e.g. for a future queued-spawn mode).
  Cosmetic; no behaviour impact. **Open.**

### P2-N — `RunCLI` prompt appears only after a line arrives (messaging)
- `internal/messaging/cli.go:RunCLI`. Deliberate trade-off from
  the first audit (P2-34): the prompt is printed once a line is
  known to be available, so EOF ends the session with no
  trailing prompt — but in an interactive terminal the user
  types with no visible prompt. Accepted behaviour, not changed.
  **Open (accepted).**

### P2-O — install.sh sanity check fails the install on a broken provider (read-only)
- `install.sh`: `"$BIN_DIR/orion" config show >/dev/null || die
  "installed orion cannot read $CONFIG_PATH"`. `config show`
  fails when the *existing* config's default provider is broken
  (e.g. key-less openai), so a re-install aborts with a
  misleading "cannot read" message even though the binaries
  installed fine. `orion version` (which needs no config) would
  be a truer smoke test; or the message should say the config is
  invalid. **Open — file is read-only for this audit.**

### P2-P — install.sh systemd rewrite breaks on special `BIN_DIR` chars (read-only)
- `install.sh`: `sed "s|^ExecStart=.*|ExecStart=$BIN_DIR/oriond|"`.
  A `--prefix` containing `|`, `&`, or `\` breaks the
  substitution (`&` expands to the matched text). Use a
  different delimiter handling or `awk`/printf. Edge case
  (paths rarely contain these). **Open — read-only.**

### P2-Q — Makefile has no Go fallback (read-only)
- `Makefile`: `GO ?= go` — unlike `install.sh`, it does not fall
  back to `~/go1.27.4/bin/go` or `/usr/local/go/bin/go`, so
  `make build` fails on a host where Go is installed under home
  but not on PATH. **Open — read-only.**

### P2-R — oriond.service has no start-limit (read-only)
- `oriond.service`: `Restart=on-failure` + `RestartSec=5s`
  with no `StartLimitIntervalSec`/`StartLimitBurst`. A persistent
  failure (e.g. bad token — now that Poll exits loudly on 401,
  see P1-B) restarts every 5s forever instead of giving up and
  surfacing. Add e.g. `StartLimitIntervalSec=60s` +
  `StartLimitBurst=5` to `[Unit]`. **Open — read-only.**

### P2-S — README / CHANGELOG drift (read-only)
- `README.md`: CLI table omits `config set-api-key`; the
  Telegram section's `orion config set-api-key ...` line is a
  confusing placeholder (there is no CLI setter for the token —
  the comment below it says so, but the example line suggests
  otherwise). Test counts verified current (341 pass, 3 skip at
  audit start).
- `CHANGELOG.md`: stale — still says "Fact-aware recall
  injection is in progress" though recall shipped, and the HMAC
  audit chain, interpreter one-liner denylist, P2-22 fix,
  streaming redesign, and config-merge fixes are unrecorded.
  **Open — read-only.**

---

## Old-fix spot checks (from AUDIT_2026-11-13) — all still hold

Verified by reading the current code and by the passing tests;
nothing below regressed:

- **P0-2 Telegram allowlist**: `WithAllowedChats` +
  `IsChatAllowed`, enforced in `Poll` before the handler;
  config validates non-zero chat IDs; `TestPollEnforcesAllowedChats`
  and `TestIsChatAllowedNoAllowlist` pass. (Note: Irfan's chat
  `7429947930` is now allowlisted in the live config per MEMORY —
  the empty-allowlist warning no longer fires for the daemon.)
- **P2-30 token URL-escaping**: `url.PathEscape(g.token)` in
  `call`; `TestCallEscapesTokenInURLPath` passes.
- **P2-31 redacted errors**: `Unwrap` returns a recursively
  redacted view, `Is`/`As` forward; `TestRedactedErrorDirect`,
  `TestRedactedErrorUnwrapNeverLeaksToken`,
  `TestRedactedErrorKeepsErrorsAs` pass. (P1-A's new `apiError`
  is reachable via `errors.As` through this wrapper — tested.)
- **P2-32 concurrent Poll**: semaphore (8) + striped per-chat
  locks; `TestPollHandlesChatsConcurrently` passes.
- **P2-33 atomic default base URL**: `atomic.Value` +
  getter/setter; concurrent test passes.
- **P2-34 no trailing prompt on EOF**: `TestRunCLINoTrailingPromptOnEOF`
  passes (see P2-N for the accepted trade-off).
- **P1-9 subagent context release**: `runTask` takes `cancel`
  and defers it; `TestSpawnedTaskContextReleased` passes.
- **P1-10 bounded subagent registry**: eviction at 100 oldest
  finished first + `Forget`; `TestEvictsOldestFinished`,
  `TestForget` pass.
- **P1-13 cmdTools ctx**: signal context threaded through
  `cmdTools` into `Execute`; verified in `main.go`.
- **P1-14 CLI cancellable stdin**: reader goroutine + `select`
  on `ctx.Done()`; `TestRunCLICancelledWhileWaiting` passes.
- **P2-38 shared `app.Local`**: `cmd/orion` delegates to
  `app.BuildLocal`; no second wiring copy.
- **P2-39 status/config surface errors**: `ActiveProvider` and
  memory errors returned, not printed as empty values;
  `TestCmdStatusCorruptMemorySurfacesError` passes.
- **P2-40 `DefaultPath`**: mutating config commands fail
  clearly with HOME unset; `TestCmdConfigHomeUnset` passes.
- **P2-41 `maskSecret`**: returns only `****` / `(not set)`;
  `TestMaskSecret` passes.
- **P2-42 case-insensitive provider names**: `providerKey`;
  `TestCmdConfigSetProviderCaseInsensitive` passes.
- **P2-43 shared version package**: both binaries use
  `internal/version`; `TestVersionIsSemver`,
  `TestStringWithLdflags` pass.
- **Skills fixes (first audit)**: backtick-with-description
  parsing, Tools-slice aliasing, tool dedupe, `.md` empty-name
  rejection — all tests pass.
- **App fixes (first audit)**: rune-based audit truncation,
  redact-before-truncate, nil-`App` guards —
  `TestHandleAuditTruncatesByRunesAndRedactsFirst`,
  `TestHandleNilAppDoesNotPanic` pass.

## Checked and found clean (no finding)

- `subagent` lifecycle: semaphore acquired non-blocking under
  the mutex (no deadlock); `Wait`/`WaitAll` hold entry pointers
  so eviction cannot strand a waiter; `Shutdown` is idempotent
  and bounded (10s); executor panics recovered in `runTask`;
  late results discarded via the buffered channel. `go test
  -race` passes.
- `app.Build`/`BuildLocal`: error wrapping consistent; partial
  build on `NewAgent` failure closes the memory store; audit
  failure degrades to unaudited-with-a-log-line by design.
- `app.Handle`: redacts before truncating; audits even on
  agent error; nil-`App` safe.
- `version.String`: ldflags defaults excluded correctly.
- `cmd/oriond`: `run(args) int` so `defer a.Close()` runs;
  token → allowlist warning → `GetMe` → provider order is
  correct; cancel exits 0.
- `telegram.go` remaining surface: offset advances on
  max(update_id)+1 only after a successful call; nil-`Message`
  updates skipped after the offset advanced (no redelivery);
  `GetUpdates` honours the earliest deadline; backoff resets
  after success; `splitMessage` splits on rune boundaries;
  `SendMessage` aborts on the first failing chunk with its
  index; `WithAllowedChats` ignores zero IDs; empty token
  rejected at construction.
- `cli.go`: 64 KB line cap; scanner errors returned; EOF with
  no trailing prompt; `/quit`/`/exit` after trim.
- No `TODO`/`FIXME`/stub/placeholder markers in scope; no test
  runs a live destructive command; no secret in any test
  (the fake token `test-token-123` is used only against
  `httptest` servers).

## Fixes applied by this audit (summary)

| # | File | Fix | Tests |
|---|------|-----|-------|
| P1-A | `internal/messaging/telegram.go` | Cap API error descriptions (512 runes); typed `apiError` | `TestCallCapsErrorDescription` |
| P1-B | `internal/messaging/telegram.go` | Poll exits on fatal 401/403/409 | `TestPollExitsOnFatalAPIError` |
| P1-C | `cmd/orion/main.go` | `telegram test` loads config only, no full app | `TestCmdTelegramTestSkipsProvider` |
| P2-A | `cmd/orion/main.go` | Ctrl-C → clean exit 0 | `TestExitCodeFor` |
| P2-B | `cmd/orion/main.go` | Usage to stderr on unknown command | `TestUsageToWritesToWriter` + smoke test |
| P2-C | `internal/messaging/telegram.go` | Recover handler panics in Poll | `TestPollRecoversFromHandlerPanic` |
| P2-D | `internal/messaging/telegram.go` | Serialise GetUpdates (`fetchMu`) | `TestGetUpdatesConcurrentCallsDoNotDuplicate` |
| P2-E | `cmd/orion/main.go` | Reject blank `set-model` | `TestCmdConfigSetModelRejectsBlank` |
| P2-F | `internal/skills/skills.go` | `### ` ends Tools section; real ATX heading check | `TestParseToolsSubheadingEndsSection`, `TestHeadingLevel` |
| P2-G | `internal/skills/skills.go` | Skip fenced blocks / numbered lists in descriptions | `TestParseDescriptionSkipsCodeFence`, `TestParseDescriptionSkipsNumberedList` |
| P2-H | `internal/skills/skills.go` | Numbered tool items | `TestParseToolsNumberedItems`, `TestNumberedItem` |
| P2-I | `internal/skills/skills.go` | Nil-Registry guards | `TestNilRegistryDoesNotPanic` |

New test files: `internal/messaging/audit2_test.go`,
`internal/skills/audit2_test.go`, `cmd/orion/audit2_test.go`
(15 new tests, all passing with `-race`).

## Still open in this scope

- P2-J (sendMessage 429 retry), P2-K (redact handler-error
  replies), P2-L (log dropped stranger chats), P2-M (dead
  `StatusPending`), P2-N (CLI prompt trade-off, accepted),
  P2-O/P/Q/R/S (read-only files: install.sh sanity check +
  sed escaping, Makefile Go fallback, oriond.service
  start-limit, README/CHANGELOG drift).
- `telegram test` is decoupled from the app but not from
  provider validation in `config.Load` (needs a config-layer
  decision; out of scope).
- Live Telegram round-trip still untested: needs Irfan to
  message @Magi420x_bot once (`/start`), then
  `ORION_LIVE_TELEGRAM_TEST=1 go test ./internal/messaging/
  -tags live -run TestLive -count=1 -v`. (Pre-existing open
  item, not in this audit's power to close.)
- Sibling-scope failures seen during this audit (model
  same-role merge vs old tests; security `TestAudit2Probe`
  denylist probes) belong to their owners — see "Tree state"
  above.
