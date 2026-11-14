# Changelog

All notable changes to Orion are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the
project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.2.0-rc25] — 2026-11-14

### Audit 19 fixes (v0.2.0-rc25) — see `AUDIT19_2026-11-14.md`

Fixes all seven P3s from the fresh-eyes audit of rc24 (0 P0, 0 P1,
0 P2, 7 P3). Audit 18's fixes all still hold; rc8 security holds.
**Verdict: NOT production-ready** — the 24–72 h daemon soak (F-A3-4)
is still open. Verified by the release coordinator on the final
commit: **1,385 tests pass, 0 fail, 4 skip (total 1,389), 24/24
packages, race clean 24/24, no data race** (clean-clone recount;
the coordinator corrects these numbers here if the recount differs).

- **F-A19-1 (P3)** — the remaining media kinds (animation / audio /
  video_note / contact / poll / venue / dice / game) now get an
  honest per-kind reply instead of being silently dropped
  (TGMessage carries the fields; bytes are never downloaded). A
  captioned animation no longer runs a turn on the caption while
  the model never got the GIF — media takes precedence over text,
  as with a captioned video.
- **F-A19-2 (P3)** — `orion backup`'s corrupt-config fallback now
  honours ORION_DATA_DIR (it used config.Default(), which ignores
  the env override), so a backup never silently targets ~/.orion
  while the daemon runs on another dir; the warning names the dir
  actually used. The `oriond --restore` fallback warning now says
  plainly that the config file's data_dir is unknown.
- **F-A19-3 (P3)** — the shipped oriond.service header now names
  the real CapabilityBoundingSet gate (CAP_SYS_ADMIN + CAP_SETFCAP,
  CAP_SETUID is NOT the gate), matching the live unit. A test pins
  the wording in both unit files, so the drift-guard test's blind
  spot (it pins directives, not comments) cannot let this regress.
- **F-A19-4 (P3)** — the sampler's pinned scope ("system"/"user")
  returning "0" (unit known but not running) now falls back to the
  other manager, like the unset-scope path — it no longer returns
  "0" raw and leaves the sampler spinning in its retry loop. The
  pin still controls the PREFERRED order.
- **F-A19-5 (P3)** — the refreshInstanceLock comments are corrected:
  Restore never replaces oriond.lock (it skips it in the verify and
  copy loops), so the premise that a snapshot carries its own lock
  and Restore copies it was false. The refresh stays as a defensive
  guard for any other replacement of the file. The Snapshot-skip
  option was deliberately NOT taken: an existing test relies on
  snapshots containing the lock to prove Restore skips it. No
  behavior change.
- **F-A19-6 (P3)** — a permanent "no owner" budget-alert failure
  is now distinguished from a transient delivery failure: it is
  logged once per chat for the process and never retried, instead
  of being retried (and logged) on every record until UTC midnight.
  It is deliberately NOT marked alerted, so a restart with an owner
  configured can still fire the alert. A transient failure still
  retries (the rc24 retry test still passes).
- **F-A19-7 (P3)** — the Makefile GO fallback now includes the
  audit toolchain path
  ($(HOME)/workspace/orion-audit/toolchain/go/bin/go), and the
  parse-time $(error) no longer breaks Go-free targets: `make clean`
  and `make version` work on a Go-less host (a goal that needs Go
  still fails at parse time with the locations checked).
- **Verification-blocking data race fix (found during the rc25
  clean-clone -race run, rc12/rc21 code, not an audit finding)** —
  PollStream spawned its reply handlers (callback, photo, media)
  as detached goroutines and never waited for them, so a handler
  could log via g.logf (which reads the global os.Stderr at call
  time) AFTER PollStream returned — the race detector caught
  TestW4's callback handler logging "context canceled" after
  return, racing TestDaemonDebugEndpointEnabled's capture() swap
  of os.Stderr. The first clean-clone -race run failed on ENOSPC
  (the /tmp GOCACHE grew to 364M on the 512M tmpfs — no data race,
  no code failure); the re-run with the GOCACHE on the home disk
  exposed this race. The fix tracks the three g.logf handlers in
  a WaitGroup (Add before spawn, Done on return; PollStream waits
  in a defer), so no handler logs after return. The turn handler
  is deliberately NOT tracked: it never calls g.logf, so it cannot
  race on os.Stderr, and waiting for it would delay shutdown for
  the whole turn. Regression test
  `TestPollStreamWaitsForCallbackHandler` (deterministic: fails
  without the wait, passes with it).

Residuals (rc25, carried forward): the live sampler CSV rows are
still only in the live working tree, not the repo (Audit 19 §2.3
correction — the F-A18-3 code fix is verified by tests, not by the
committed CSV); the budget threshold is still config-file-only (no
`/budget threshold N` setter, rc24 residual); the F-A19-5 Snapshot
skip was deliberately not taken (see above).

## [0.2.0-rc24] — 2026-11-14

### Audit 18 fixes (v0.2.0-rc24) — see `AUDIT18_2026-11-14.md`

Fixes all six P2s and all six P3s from the fresh-eyes audit of rc23
(0 P0, 0 P1, 6 P2, 6 P3), plus the F-A17-17 drain-test residual, the
install.sh silent failure, and D4 (AUDIT13 is now committed, so the
definition is verifiable). Audit 17's fixes all still hold; rc8
security holds.
**Verdict: NOT production-ready** — the 24–72 h daemon soak (F-A3-4)
is still open. Verified by the release coordinator on the final
commit: **1,374 tests pass, 0 fail, 4 skip (total 1,378), 24/24
packages, race clean 24/24, no data race** (clean-clone recount;
the coordinator corrects these numbers here if the recount differs).

- **F-A18-1 (P2)** — the install test's `Restart=always` assertion
  was hollow (it matched the comment text in oriond.service, not a
  directive). It now uses a directive parser (comments excluded)
  asserting `Restart=on-failure` + `RestartForceExitStatus=42`.
- **F-A18-2 (P2)** — `oriond --restore` now takes the instance lock
  before restoring and refuses clearly ("stop the daemon first",
  exit 2) when the daemon is running; usage says so. A restored
  snapshot's future allow-all deadline is neutralised (fail-closed)
  so the window cannot resurrect, and Restore no longer replaces
  the live `oriond.lock` (flock follows the inode — replacing it
  would silently drop the lock mid-restore).
- **F-A18-3 (P2)** — the sampler's `resolve_pid` now tries the
  system manager first and falls back to the user manager
  (`ORION_SYSTEMD_SCOPE` pins one); the committed sampler service
  pins `ORION_SYSTEMD_SCOPE=system`. Live-verified: the fixed
  script writes a fresh CSV row for the live daemon (PID 2105,
  rc23). The `${ORION_WORKSPACE}` parameterization was attempted
  and reverted — systemd 255 rejects `${VAR}` in WorkingDirectory /
  ExecStart / StandardOutput (verified with a scratch unit); paths
  stay literal by design, and generating the unit from a template
  is the future path.
- **F-A18-4 (P2)** — the three verified-safe hardening directives
  (NoNewPrivileges, ProtectSystem=true, PrivateTmp) are now in the
  SHIPPED oriond.service as well as the live unit, and the
  drift-guard test pins the match in both (plus the absence of
  ProtectHome / CapabilityBoundingSet).
- **F-A18-5 (P2)** — `orion daemon` / `orion telegram poll` now
  take the same flock on `<dataDir>/oriond.lock`, so a second CLI
  poller exits 2 instead of causing a Telegram 409 flap.
- **F-A18-11 (P2)** — the daily budget guardrail is restored as a
  configurable threshold alert (NOT a reverted limit — the user's
  unlimited setting stays): new
  `daily_budget_alert_threshold_tokens` (0 = disabled); when a
  chat's daily usage crosses it, the owner is notified on Telegram
  once per chat per day (state persists in budget.json, survives
  restarts, retries a failed send). `/budget` shows the threshold.
- **F-A18-6 (P3)** — `/skills reload` is now owner-only.
- **F-A18-7 (P3)** — `oriond --restore` and `orion backup` fall
  back to the default data dir on a corrupt config (with a warning)
  instead of failing; the restore fallback also honours
  ORION_DATA_DIR.
- **F-A18-8 (P3)** — live snapshots are documented as
  non-quiesced: the manifest records `quiesced: false` + a
  consistency note, and the limit is in the Snapshot docs and
  `orion backup --help`. A reader-writer `backup.lock` quiesce was
  surveyed (~14 write sites across 8 packages) and deliberately
  NOT implemented — too invasive for a stability RC, and a partial
  lock would be worse than an honest live snapshot; it belongs in
  a later RC.
- **F-A18-9 (P3)** — non-photo media (sticker / voice / video /
  document / location) now gets an honest per-kind reply instead of
  being silently dropped (TGMessage carries the fields; bytes are
  never downloaded).
- **F-A18-10 (P3)** — `/rules` now uses `isOwner(ctx)` (chat OR
  sender), so the owner in a group chat is recognised; `/rules` is
  wired in the `orion` binary too (single-command exception in the
  daemon turn handler, documented with a do-not-extend guard).
- **F-A18-12 (P3)** — the CapabilityBoundingSet reason is corrected
  (the real gate is CAP_SYS_ADMIN + CAP_SETFCAP, not CAP_SETUID),
  in the live unit's comment and the rc23 CHANGELOG paragraph; the
  9-unit matrix artifact (probe script + real run log + README) is
  now in the repo at `docs/capability-matrix/`. The omission itself
  is unchanged and correct.
- **F-A17-17 residual** — a test now pins drain 30s equality across
  both entrypoints (sensitivity-verified).
- **install.sh** — a present-but-failing systemctl (daemon-reload /
  enable --now) now fails loudly (exit 1) instead of warning and
  exiting 0; "systemctl not found" stays a warning.
- **Makefile** — the GO lookup now verifies each candidate by
  running `go version` and fails with a clear error listing all
  checked locations when none works.
- **D4** — `AUDIT13_2026-11-14.md` is now committed, so the D4
  definition (lines 144–145) is verifiable instead of
  builder-asserted.
- **Verification-blocking race fix (found during the rc24 clean-clone
  verification, rc23 code, not an audit finding)** — `Shutdown`
  returned while the late-outcome waiter (`awaitLateOutcome`) was
  still parked: it was not tracked by `m.wg`, so its persist of the
  settled usage could land after the caller started cleaning up the
  store dir. That was the `TestRC23W3PersistenceRunningTaskLost`
  flake (TempDir RemoveAll "directory not empty", ~1 in 3 full-suite
  runs). The waiter is now tracked in `m.wg` (Add before spawn,
  Done on return; the 10s ShutdownTimeout still bounds a never-
  returning executor), so Shutdown is the durability signal for
  late usage too. Regression test
  `TestRC24ShutdownWaitsForLateOutcomePersist` (deterministic:
  fails 3/3 without the fix, passes 5/5 with it).

Known flaky test (pre-existing, rc23 code, not an rc24 regression):
`TestRC23W3PersistenceRunningTaskLost` (internal/subagent) failed
once in the first -v recount with a TempDir-cleanup race
("directory not empty") and passed standalone 4/5 times and in
the second full recount; the plain suite passed both times. The
test's own assertions pass — the failure is the cleanup racing a
persist write. **FIXED in rc24 by the race fix above** (the waiter
is now tracked in `m.wg`); the flaky test passes 10/10 standalone
and 5/5 in full-package runs after the fix, and the regression test
pins it. Recorded here so the next audit does not mistake the
earlier failure for a regression.

## [0.2.0-rc23] — 2026-11-14

### Audit 17 fixes (v0.2.0-rc23) — see `AUDIT17_2026-11-14.md`

Fixes the P1, all twelve P2s and all five P3s from the fresh-eyes
audit of rc22 (0 P0, 1 P1, 12 P2, 5 P3). Audit 16's fixes are all
genuinely fixed (F-A16-6 is now fully closed by the install.sh test)
and all earlier fixes still hold; rc8 security holds.
**Verdict: NOT production-ready** — the 24–72 h daemon soak (F-A3-4)
is still open. Verified by the release coordinator on the final
commit: **1,323 tests pass, 0 fail, 4 skip (total 1,327), 24/24
packages, race clean 24/24, no data race** (clean-clone recount;
the coordinator corrects these numbers here if the recount differs).

- **F-A17-1 (P1)** — the live unit files are now committed:
  `systemd/oriond.service` and `systemd/oriond-sampler.service`
  (previously only in the working tree, so
  `TestShippedUnitRestartsOnCleanExit` failed in a clean checkout
  and the rc22 "1,250/0" claim was false). The dated log path
  (`oriond-live-2026-11-14.log`) is replaced with the stable
  `oriond-live.log`. The drift guard is preserved — the test is
  unchanged in intent (renamed to
  `TestShippedUnitRestartsOnRestartRequest` for the new policy)
  and passes in a clean checkout.
- **F-A17-2 (P2)** — crash-loop: /restart now exits **42**
  (`exitRestartRequested`), and both units use
  `Restart=on-failure` + `RestartForceExitStatus=42`; a clean exit
  0 no longer restarts. The burst limit (12/60s) is unchanged and
  stays the circuit breaker for genuine faults by design — a
  persistent fault still strands the unit in failed state until
  an operator resets it (no alerting yet).
- **F-A17-3 (P2)** — single-instance guard: an exclusive flock on
  `<dataDir>/oriond.lock` (0600), held for the process lifetime.
  A second oriond exits 2 with a clear log line and never talks
  to Telegram (no more 409 fatal flap). Residual: the CLI
  `orion daemon` / `telegram poll` paths take no instance lock.
  Coordinator fix: the second-instance test's capture helper
  swapped the os.Stdout/os.Stderr globals while the first daemon
  was still writing to them — a real data race the clean-clone
  `-race` run caught. The test now runs the second instance with
  no swap (its line lands in the first daemon's shared, locked
  output buffer), and the harness buffer is a locked type so
  concurrent reads are safe.
- **F-A17-4 (P2)** — the allow-all window deadline is persisted
  atomically in the data dir (`allow-all-until`, RFC3339Nano,
  0600); a restart resumes the recorded window instead of opening
  a fresh one. Corrupt state fails closed. The CLI gateway now
  uses the persisted form too.
- **F-A17-5 (P2)** — the /proc bypass of the F-A16-9 read rule is
  closed: `/proc/<pid|self>/environ`, `/proc/self/cwd[/...]`,
  `/proc/self/root[/...]`, `/proc/*/fd[/...]` operands are blocked
  in `IsBlockedCommand` (literals, `=` values, variable-expanded
  forms). `cat /proc/self/status`, `/proc/cpuinfo`, `ls /proc`
  stay allowed.
- **F-A17-6 (P2)** — legacy global rules: first use of a legacy
  global rule now logs a warning naming the rule and the
  owner-only /rules command. New owner-only `/rules` command:
  bare `/rules` lists rules (globals flagged `GLOBAL (legacy)`),
  `/rules delete <tool>` deletes a legacy global rule,
  `/rules delete <chat>:<tool>` deletes a per-chat rule,
  `/rules scope <tool> <chat>` moves a global rule onto one chat.
- **F-A17-7 (P2)** — sampler: `resolve_pid()` now runs
  `systemctl --user show oriond`, output paths are parameterised
  (`SAMPLER_OUT` / `SAMPLER_CSV` / `ORION_BIN`), and
  `systemd/oriond-sampler.service` is committed. The rc22
  CHANGELOG claim that the sampler service was committed is
  corrected: it was not in rc22; it is in rc23.
- **F-A17-8 (P2)** — `install.sh --systemd` is now pinned by
  `install_systemd_test.go`: the real script runs against a
  scratch HOME/prefix with stub systemctl/loginctl, asserting the
  shipped user unit is installed with ExecStart rewritten to the
  install dir, `enable --now` (not bare enable), linger, the
  existing config left untouched, and the real HOME untouched.
  Boot recovery after a real reboot is still PENDING (laptop only
  — no safe reboot on this VM) and is recorded as such in the
  test, not claimed. No watchdog yet (needs sd_notify).
- **F-A17-9 (P2)** — photos are no longer silently dropped:
  `TGMessage` carries the photo sizes, and a photo gets the
  honest reply "I can't see images yet" (after allowlist/sender
  checks; a caption never runs a turn).
- **F-A17-10 (P2)** — vision 400s from a provider are translated
  into the same cannot-see message instead of a raw error. No
  vision implementation in rc23 (feature phase).
- **F-A17-11 (P2)** — skill API-gating is NOT in rc23
  (pluggable-skills feature phase). The skill format is unchanged.
- **F-A17-12 (P2)** — subagent state is now persisted: task
  records + dead-letter ledger survive a reboot in
  `<dataDir>/subagent-state.json` (0600, atomic temp+rename+dir
  fsync; newest 200 finished tasks, outputs capped 16K runes).
  A task running at stop reloads as failed with an explicit
  "lost on restart" error and contributes nothing to the ledger;
  the startup log line reports recovered/lost counts. Corrupt or
  wrong-version files log and start empty. Coordinator fix: the
  finish is now persisted BEFORE the wait channel closes, so a
  `Wait` return guarantees the result is on disk (persisting
  after the close let a concurrent file replacement be clobbered
  by the still-pending write — a real race caught by the
  coordinator's full-suite run).
- **F-A17-13 (P2)** — `~/.orion` backup: new `internal/backup`
  package — `Snapshot` (timestamped dir, `manifest.json` with
  size + SHA-256 per file; symlinks/non-regular files skipped),
  `Restore` (verifies ALL hashes before writing anything; merge
  restore, never deletes extras), `List`, `Prune` (newest 7).
  Entry points: `orion backup` (snapshot + rotate) and
  `oriond --restore <snapshot>` (restore before the app starts,
  then exit). Verified by test: snapshot → wipe → restore →
  byte-identical; a tampered snapshot is refused with zero writes.
- **F-A17-14 (P3)** — unknown/misspelled tool names in a skill's
  `## Tools` now warn at load with a Levenshtein "did you mean
  …?" suggestion. Load never fails on a warning.
- **F-A17-15 (P3)** — `/skills reload` command: re-reads the
  skills directory and swaps the registry under one lock; a
  total failure leaves the old skills live. (Not owner-only —
  not requested; flagged, not silently restricted.)
- **F-A17-16 (P3)** — the startup banner now carries one line
  with version, PID, data dir and config path.
- **F-A17-17 (P3)** — drain is unified at 30s in both entrypoints
  (/restart and SIGTERM/SIGINT) with honest log language.
- **F-A17-18 (P3)** — systemd hardening on `systemd/oriond.service`:
  `NoNewPrivileges`, `ProtectSystem=true`, `PrivateTmp`.
  `ProtectSystem=true`, not strict: the data dir and working
  directory live under /home. `CapabilityBoundingSet` is
  DELIBERATELY omitted: any restriction of the bounding set breaks
  the sandbox's `unshare --user --map-root-user` uid_map write.
  The gate is CAP_SYS_ADMIN + CAP_SETFCAP (both required) —
  CAP_SETUID is NOT the gate: the 9-unit matrix showed
  {SETUID,SETGID,SYS_ADMIN} still fails with CAP_SETUID present,
  while {SYS_ADMIN,SETFCAP} succeeds without it. (rc24, F-A18-12:
  the rc23 text wrongly blamed CAP_SETUID; the omission itself is
  unchanged and correct — the probe in `docs/capability-matrix/`
  reproduces the bisection.) Restricting the set would
  silently break every sandboxed tool call on this root-run unit.
  Verified: `systemd-analyze verify` clean; the daemon starts,
  polls, writes its data dir, and stops cleanly (SIGTERM, exit 0)
  under exactly this set.

### Queued user items (rc23)

- **/budget command (owner-only)** — `/budget` shows the current
  limit + today's usage for the chat; `/budget <number>` sets the
  daily per-chat limit (1000–10000000, matching config
  validation); `/budget infinity` (also `inf`, `unlimited`) sets
  unlimited (0). The new limit is persisted to the config file
  (atomic 0600 save, before the live limit changes; a save
  failure rolls back) so it survives restarts. The `/বাজেট`
  alias works. Follows the /restart owner-only pattern.
- **web_fetch cost fix** — web_fetch now returns EXTRACTED text,
  not raw dumps: HTML → clean text (scripts/styles/tags stripped,
  entities unescaped), JSON → compact one-line-per-item summaries.
  Default `max_bytes` lowered 256KB → 32KB; extracted output is
  capped at `maxBytes/4` chars (~2k tokens at default). Measured
  shrink: a GitHub HTML page 176,746 → 4,910 bytes (2.8% of raw);
  a 13-repo repos.json 82,617 → 6,007 bytes (7.3%). The agent can
  still raise `max_bytes` explicitly for a full page. web_search
  is unchanged (already compact).
- **resolveHost errors name the trusted egress CIDRs** — a refused
  host now reports e.g. `host "example.com" resolves to non-public
  address 10.1.2.3 (trusted egress CIDRs: 198.18.0.0/15)` (or
  `(trusted egress CIDRs: none configured)` when empty), so the
  model explains the real cause instead of blaming the user's
  network. Tests assert the CIDRs appear.
- **DuckDuckGo captcha detection** — an HTTP 202 from the DDG
  API/lite endpoint, or a zero-result lite page naming the
  challenge, now replies "Search backend is blocking automated
  requests right now (DuckDuckGo bot challenge). Try again later."
  instead of "No results".

## [0.2.0-rc22] — 2026-11-14

### Audit 16 fixes (v0.2.0-rc22) — see `AUDIT16_2026-11-14.md`

Fixes the P1 and all eight P2s from the fresh-eyes audit of rc21
(0 P0, 1 P1, 8 P2). Audit 15's fixes are all genuinely fixed and
all earlier fixes still hold; rc8 security holds; pprof 7/7 PASS.
**Verdict: NOT production-ready** — the 24–72 h daemon soak (F-A3-4)
is still open. Verified by the release coordinator on the final
commit: **1,250 tests pass, 0 fail, 4 skip (total 1,254), 22/22
packages, race clean 22/22, no data race** — CORRECTION (Audit 17,
F-A17-1): that claim was false in a clean checkout; the true rc22
numbers were **1,249 pass, 1 fail, 4 skip (total 1,254), 21/22
packages** (`TestShippedUnitRestartsOnCleanExit` failed — `systemd/`
was not in git). Fixed in rc23 by committing the unit files.
(rc21: 1,227 total:
1,223 pass, 0 fail, 4 skip). New
regression tests: 27 top-level functions — `cmd/oriond/
rc22_teama_test.go` (4) and `cmd/orion/rc22_teama_test.go` (3),
`internal/permissions/rc22_teamb_test.go` (7) and
`internal/messaging/rc22_teamb_test.go` (5),
`internal/skills/rc22_skills_test.go` (3 functions, 45 subtests),
`internal/debug/debug_test.go` (3) and
`internal/security/audit16_fix_test.go` (2).

- **F-A16-1 (P1)** — `/restart` killed the daemon permanently with
  the SHIPPED unit: `oriond.service` had `Restart=on-failure` while
  `/restart` exits 0, so systemd never restarted it; the code
  comment in `cmd/oriond/main.go` claimed a `Restart=on-success`
  that exists nowhere. The shipped unit is now `Restart=always`
  (NOT on-success, and `/restart` still exits 0 — one fix, per the
  auditor's direction), the comment is fixed, and the live unit
  `./systemd/oriond.service` (already `Restart=always`) is verified
  in sync. Verified with a scratch dummy unit: exit 0 with
  `Restart=always` restarts (NRestarts=4 in 10 s); with the old
  `on-failure` it stays dead (NRestarts=0) — the original bug
  reproduced. This does NOT explain the 2026-11-14 ~06:19 silent
  death ("restart requested" appears 0 times in that day's daemon
  log, and the owner was asleep); it fixes a real bug that WOULD
  kill the daemon on the laptop, where `install.sh` installs the
  shipped unit.
- **F-A16-2** — `allowed_updates` appeared nowhere in rc21, so a
  stale server-side filter could silently kill approval callbacks
  again (the rc8 incident). The first successful
  `fetchUpdatesLocked` call now sends
  `allowed_updates=["message","callback_query"]`, once per gateway
  lifetime, which also resets a stale server-side filter. A failed
  filter can never fail startup or stop polling: the flag is set
  only after a fetch succeeds, so a failed first fetch retries the
  filter on the next fetch; on success the gateway logs the filter
  being set. No extra round-trip, no update consumed.
- **F-A16-3** — "Always/Never" rules were global (keyed by tool
  name only), so one chat's decision silenced approval for every
  chat. Rules are now per-chat (`chatID + tool`); the gate looks
  up via `LookupForChat` and persists via `SetForChat`. Legacy v1
  rules on disk load as global (chat 0) and keep applying — the
  live `shell_exec: allow` rule is NOT silently revoked; every
  rule written from rc22 on is per-chat (v2 format, same file
  shape). The approval card now says Always/Never remembers "for
  this tool in this chat only". The denylist stays absolute and
  first in `Check`.
- **F-A16-4** — `neverVerbWords` over-blocked genuine verbs
  `free`/`slow`: "free the plan", "free users", "slow the
  rollout", "slow users" all rejected. They are removed from
  `neverVerbWords` and form a new `transitiveOnlyVerbs` class:
  no verb reading without an object, so the general shape
  predicates stay closed to them and only the explicit
  object rules in `clauseVerbAt` admit them (article, pronoun,
  and bare-plural branches, gated on a complete subject). Bare
  clause-final `free`/`slow` still reject, and before a
  participle they stay modifiers. Two simpler designs were
  probed and rejected: removing them alone re-opens the F-A15-1
  fail-open family, and adding them to `baseFormVerbs` breaks
  rc20's F-A15-3 guard. `busy`/`fast` stay in `neverVerbWords`
  ("busy the plan", "fast the plan" still reject). D4 from Audit
  13 §7 ("Activate the tool, the skill and the venv running
  tests") was verified still loading and is now pinned under the
  `D4` label (`TestRC22Audit13D4StillLoads`).
- **F-A16-5** — SIGTERM/SIGINT shut the daemon down with no log
  line, and SIGHUP was not caught at all. Both binaries now use
  an explicit signal channel so the shutdown path knows which
  signal fired and logs `shutting down on SIGTERM...` plus
  `shutdown complete`; SIGHUP is ignored with a log line (a
  daemon must outlive its terminal — stopping is via SIGINT/
  SIGTERM, `/restart`, or `systemctl stop`; in `cmd/orion` the
  ignore is scoped to long-running `pollTelegram` only). A
  `signalName()` helper spells SIGTERM/SIGINT/SIGHUP in logs
  instead of Go's "terminated"/"interrupt".
- **F-A16-6** — the systemd path: `install.sh --systemd` now
  does `systemctl --user enable --now` (enable-only left the
  operator believing the bot runs when it doesn't) and
  `loginctl enable-linger $USER` (best-effort — warns, never
  fails the install). `StartLimitBurst` raised 5 → 12 in the
  60 s window (matches the live unit). A watchdog is NOT
  implemented — it needs `sd_notify` support in the daemon, a
  bigger change; documented as future work in the unit header
  and `ARCHITECTURE.md`. Boot recovery of the shipped user unit
  on a laptop is still untested (no safe reboot on this VM) and
  documented as such. NOTE: this VM rebooted 2026-11-14 ~08:19
  SGT and both live system units came back on their own
  (oriond PID 2030, sampler PID 2031, ~2 min after boot) —
  boot auto-start is now live-proven for the LIVE units.
- **F-A16-7** — `orion telegram poll` never started the pprof
  endpoint even with `pprof_enabled=true`. The poll path now
  calls the endpoint via a shared `startDebugEndpoint()` helper
  used by both `cmdTelegram` poll and `cmdDaemon` (safe when
  disabled — `debug.Start` returns `(nil, nil)`); the "same …
  endpoint as oriond" doc comment is now true for all three
  Telegram polling entrypoints.
- **F-A16-8** — pprof hardening (not a vuln — loopback-only +
  opt-in): the server now sets `ReadTimeout` 10 s,
  `WriteTimeout` 60 s, `IdleTimeout` 60 s, and profile/trace
  requests are capped at 30 s (`MaxProfileSeconds`, the stdlib
  default — ordinary pprof use unaffected). WriteTimeout 60 s
  does not cut off a 30 s profile: the stdlib extends the write
  deadline by `WriteTimeout + seconds`. No auth added — the
  existing security model stands.
- **F-A16-9** — the denylist blocked destructive/write/network
  ops but not plain reads like `cat ~/.orion/config.json`, so an
  explicit sandbox opt-out would re-open P0-O1. `IsBlockedCommand`
  now refuses reads of sensitive paths (`~/.orion`, `~/.ssh`,
  `~/.gnupg`, plus any other user's `/home/<user>/` and `/root/`
  equivalents — mirroring the file tool), covering `~`, `$HOME`,
  absolute/relative paths, pipes, input redirection, `--flag=`
  forms, globs, and variable indirection. Documented
  non-coverage (unresolvable variables, symlinks, paths built by
  string concatenation inside interpreters) stays with the
  sandbox/file tool as backstop. Tests call `IsBlockedCommand`
  directly — never a live destructive command.

Also folded into rc22 (not audit findings):

- The uncommitted `audit3-daemon-sampler.sh` follow mode (for the
  systemd sampler service) is committed as part of rc22 — but NOTE
  (Audit 17, F-A17-7): only the *script* was committed in rc22; the
  systemd sampler service unit itself
  (`systemd/oriond-sampler.service`) was NOT in the repo in rc22
  and is committed in rc23.
- CHANGELOG fix (auditor §8 #1): the rc19 subtest count said
  "204" — the file has 198 cases (40+21+12+31+51+43), verified by
  counting. Fixed to 198.
- Auditor §8 #2 is an auditor error, not a repo gap:
  `AUDIT11_2026-11-14.md` … `AUDIT15_2026-11-14.md` DO exist at
  `~/workspace/orion-go/` — the auditor looked in
  `~/workspace/orion-audit/` (wrong directory). No change needed.

Deliberately NOT done (and why):

- NO WhatsApp gateway. The auditor's §9 "WhatsApp first"
  recommendation CONTRADICTS Irfan's explicit rule: Telegram
  ONLY, WhatsApp dropped, do NOT build WhatsApp support or the
  Baileys bridge.
- NO cron/scheduler, generic provider adapter, prebuilt-binary
  pipeline, UI, or browser/calendar/email tools. Those are
  feature-phase items (only AFTER production-ready) or
  competitive notes, not rc22.
- NO audit spawned. Audits are Muse #1's job (via the shared
  repo, user is the bridge).

## [0.2.0-rc21] — 2026-11-14

### F-A3-2 fix (v0.2.0-rc21) — debug/pprof endpoint

Closes the open observability gap from Audit 3 (F-A3-2, P2):
the daemon had no debug endpoint, so goroutine count, heap
allocation and GC behaviour were not observable on a running
daemon. **Verdict: NOT production-ready** — the 24–72 h daemon
soak (F-A3-4) is still open. Verified by the release
coordinator on the final commit: **1,223 tests pass, 0 fail,
4 skip (total 1,227), 22/22 packages, race clean 22/22, no data
race** (rc20: 1,213 total: 1,209 pass, 0 fail, 4 skip).
New regression tests: `internal/debug/debug_test.go` (7
functions), `internal/config/debug_test.go` (3 functions),
`cmd/oriond/main_rc21_test.go` and `cmd/orion/rc21_debug_test.go`
(2 functions each) — 14 new top-level test functions. Negative
control: both end-to-end "enabled" tests fail on the unwired
code (endpoint never starts) and pass with the fix.

- **Debug endpoint (F-A3-2)** — new `internal/debug` package
  serving only the standard `net/http/pprof` handlers under
  `/debug/pprof/` (index, goroutine, heap, threadcreate, block,
  mutex, cmdline, profile, symbol, trace). Opt-in via config
  `debug.pprof_enabled` (default off — when off, the daemon
  listens on nothing extra and behaves exactly as before);
  `debug.pprof_port` (default 6060, 0 means the default, out of
  range rejected by `Config.Validate` at load). Binds
  **127.0.0.1 only** (a constant, not a config value — no
  configuration can make it listen on 0.0.0.0); serves no config,
  no secret, and no application data (anything outside
  `/debug/pprof/` gets 404); does not weaken the sandbox. A bind
  failure logs a loud warning and the daemon keeps polling. Wired
  into both daemon entrypoints (`oriond` and `orion daemon`);
  the daemon logs the listen address on startup. See README
  "Debug endpoint (pprof)".

## [0.2.0-rc20] — 2026-11-14

### Audit 15 fixes (v0.2.0-rc20) — see `AUDIT15_2026-11-14.md`

Fixes all four P2s from the fresh-eyes audit of rc19 (0 P0, 0 P1, 4 P2).
**Verdict: NOT production-ready** — the 24–72 h daemon soak (F-A3-4)
is still open, and F-A3-2 (no pprof/debug endpoint) is still open.
Verified by the release coordinator on the final commit: **1,209
tests pass, 0 fail, 4 skip (total 1,213), 21/21 packages, race clean
21/21, no data race**; live DeepSeek proof `17*23+5=396` in 3.1 s
(rc19: 1,207 total: 1,203 pass, 0 fail, 4 skip). New regression
tests: `internal/skills/rc20_skills_test.go`, 6 functions, 227
subtests (52 + 21 + 29 + 10 finding shapes, 60 must-load guards,
55 must-reject guards incl. 23 bonus cases). Negative control:
all 112 finding subtests and all 23 bonus subtests fail on rc19
logic and pass with the fix (stash/restore byte-identical,
sha256 `9a09ad41…`); all 92 guard subtests pass on both.

- **F-A15-1** — rc19's `isVerbShapedAt` re-opened noun phrases after
  plural/compound subjects: the plural widening returned true for
  ANY word not in `neverVerbWords`, so a bare noun or adjective
  after a plural subject read as a verb by shape: "…the skills
  business/success/loss/class/glass" (pure -ss nouns), "…the skills
  notebook/guidebook/keyboard/pencil" (bare nouns), "…the skills
  hot/tall/young/red/cold/dark/wide/deep/soft/hard" and "…the skills
  angry/sorry/afraid/alive/asleep/aware/able" (bare adjectives),
  "…the skills free/busy" (transitive-only verbs, no intransitive
  reading) — 52 shapes, loading on rc19, rejecting on rc18. The
  fix extends `neverVerbWords` with the never-verb classes the
  probe surface shows (36 words): bare never-verb adjectives,
  adjective/adverb-dominant words (fast/slow/free/busy/very),
  pure -ss nouns, pure -s nouns (series, species), a pure -ing
  noun (ceiling), pure -ed adjectives (rugged, wicked), and bare
  nouns (notebook/guidebook/keyboard/pencil). Genuine -ss verbs
  (boss/progress/process/address/access) and genuine -ing/-ed verb
  forms (feeling/meaning/flooring, building/training/learning,
  closed/trained/opened/finished) stay OUT, so they keep loading.
  A noun + subordinator is still a noun phrase, not a clause, so
  "…the skill series/ceiling/business that helps" now rejects
  consistently (only a genuine verb form such as "reports" loads
  before "that").
- **F-A15-2** — `verbTakesGuardedObject` let suffix-bearing pure
  nouns/adjectives take guarded plurals: it excluded only
  `adjectiveVerbs` ("open") and -ing in subject position, then
  returned `isVerbShaped`, which is true for any -s/-ed/-ing
  suffix: "…the skill series/species tools/users/settings/items/
  meetings", "…the skill ceiling tools/items/users/meetings",
  "…the skill rugged/wicked tools/users" (21 shapes). The fix is
  the same `neverVerbWords` extension, now consulted by
  `verbTakesGuardedObject` before the suffix test. The audit also
  suggested a uniform -ed exclusion in subject position; that is
  DELIBERATELY NOT applied — a genuine -ed verb takes a guarded
  object for a compound subject ("…the skill and the venv
  closed/trained/opened/finished/tired/excited tools" load on both
  and are defensible, Audit 15 §6 O4), so the discriminator is the
  word's class (never-verb adjective vs genuine verb form), not
  its suffix.
- **F-A15-3** — the participle-with-object branch in `clauseVerbAt`
  never consulted the candidate's class, so a never-verb adjective
  or adverb before a participle + object read as a clause head:
  "…the skill and the big/small/happy/sad/glad/good/bad/great/
  very/fast/free/busy/tired/hot/tall/young/red running tests",
  "…the skill and the big/very waiting tests", "…the skill big/
  very running tests", "…the skill and the slow/soft/dark running
  tests", "…the skill and the big running guides", "…the skill and
  the angry/sorry running tests" (29 shapes, loading on both rc18
  and rc19). The fix gates the branch: the candidate must be able
  to head the clause — a noun head (the subject of the participle
  phrase, "files running tests") or a verb (taking the participle
  phrase as a gerund object, "build waiting tests"). A never-verb
  word that is not a noun head ("big running tests") and a
  participle that is not itself a noun head ("tired running
  tests") can be neither. A bare word after a plural subject is a
  verb by shape (F-A14-1), so the plural path needs no noun/verb
  evidence beyond not being a never-verb word. `isNounHead` now
  also recognises a pure -ss noun ("class", "glass") as a singular
  noun head, so the O9 noun-head + predicate-participle shapes
  keep loading. The -s verb sub-branch in the same block now also
  consults `neverVerbWords` ("…the skills series/analysis
  waiting" rejects). Fixing the branch exposed a second hole:
  `compoundSubjectStartsClause` skipped only two words for a
  modifier, so a modifier before a participle phrase left its
  object ("tests") exposed as a clause-final -s verb — the scan
  now skips the whole participle phrase (modifier + participle +
  object) and does not mark the subject complete.
- **F-A15-4** — `adjectiveVerbs` listed only "open", so the other
  adjective-verbs took a guarded plural as their object: "…the
  skill and the close/set tools/users/meetings", "…the skill
  close/set tools/users", "…the skills close tools" (10 shapes).
  The fix extends the class to {open, close, set} — the
  object-closer verbs that are also common adjectives in
  attributive position ("close" = nearby tools, "set" = tools of
  the set) — and documents the class property. Suffixed forms
  ("opened", "closed", "sets") still take the object, because the
  list holds base forms only. Noun-verbs such as "test"/"list"
  are NOT in the class: "test tools" is a noun+noun compound, not
  an adjective + noun phrase, so their verb reading wins.

Bonus (pre-existing fail-open, closed as a consequence of the
`neverVerbWords` extension — 23 shapes, all loading on rc19):
pure -s nouns clause-finally and before an article/pronoun/adverb/
preposition/subordinator ("…the skills series/species",
"…the skills series the plan/guides/you/quickly/for beginners/
when ready", "…the skill series the plan/guides"), the pure -ing
noun "ceiling" ("…the skill ceiling", "…the skill ceiling the
plan/for beginners/that helps"), the never-verb -ed adjective
"rugged" ("…the skill rugged", "…the skill rugged the plan"),
and -s nouns before a participle with no object ("…the skills
analysis/series waiting"). Known limitation (documented in the
code): a bare singular noun the validator cannot recognise as a
noun head ("notebook running tests") rejects in the participle
branch, while a listed noun head ("class running tests") loads.

## [0.2.0-rc19] — 2026-11-14

### Audit 14 fixes (v0.2.0-rc19) — see `AUDIT14_2026-11-14.md`

Fixes all four P2s from the fresh-eyes audit of rc18 (0 P0, 0 P1, 4 P2).
**Verdict: NOT production-ready** — the 24–72 h daemon soak (F-A3-4)
is still open, and F-A3-2 (no pprof/debug endpoint) is still open.
Verified by the release coordinator on the final commit: **1,203
tests pass, 0 fail, 4 skip (total 1,207), 21/21 packages, race clean
21/21, no data race**; live DeepSeek proof `17*23+5=396` in 3.0 s
(rc18: 1,201 total: 1,197 pass, 0 fail, 4 skip).

- **F-A14-1** — rc18 closed the verb lists in both directions, so an
  unlisted base-form verb after a plural or compound subject lost
  every position: "…the skills build/design/develop/study/watch/code/
  sing/jump/paint/draw/cook/drive/fly/swim/sleep/walk/eat/laugh",
  "…the skill and the venv build", "…the skills build the plan /
  guides / you / quickly / for beginners / when ready" all rejected
  on rc18 although they loaded on rc17 (40 shapes). The fix is
  shape-based, not more list entries: new helper `isVerbShapedAt`
  (`internal/skills/skills.go`) — after a plural or compound subject
  a bare word in verb position is a verb by shape, in every
  following-word position (article, pronoun, adverb, preposition,
  subordinator, bare plural) and at a clause end; after a singular
  subject a bare word is still not a verb ("the skill build the
  plan" still rejects). The clause-final plural rule no longer
  requires `baseFormVerbs` membership — the list is now only a
  fallback for the singular path and the participle rule.
- **F-A14-2** — The bare-plural rule fired before any verb-shape
  check and exempted only `baseFormVerbs`, so genuine verbs before a
  guarded plural object rejected: "…the skill makes/creates/runs/
  tests/checks/helps/explains/provides users/tools/meetings/settings/
  examples", "…the skill trained users / files tools / notes
  settings / guides items", "…the skills make/create/use/run/help
  users/tools/settings/meetings", "…the skill running tools" (21
  shapes). The guarded-plural rejection now exempts verb-shaped
  candidates (new helper `verbTakesGuardedObject`): an -s/-ed verb,
  an object-region verb or a listed base-form verb takes the guarded
  noun as its object. A participle in subject position ("running
  tools") still does not — -ing is never a finite verb form — and
  neither does a verb that is also a common adjective ("open":
  "the open tools" is an adjective + noun, Audit 13 §7 D2).
- **F-A14-3** — `compoundSubjectStartsClause` set `sawNoun=true` for
  any word that is not an article, a subject modifier or a clause
  adverb — including words `clauseVerbAt` had just early-returned as
  non-verbs (the adjectives in `baseFormNonVerbs`: new/ready/
  available/online/old/simple/easy/active; -ly adverbs: quickly) —
  so the participle+bare-plural branch read the participle as the
  predicate instead of a modifier: "…the skill and the new/ready/
  quickly/available/online/old/simple/easy/active running/waiting
  tests" loaded on rc18 although they rejected on rc17 (12 shapes).
  Only a genuine noun head now sets `sawNoun` (new helper
  `isNounHead`: a pure or ambiguous noun head, a named non-skill
  object noun — "venv", "tool", "mode" — or a bare plural), so "the
  new running tests" stays a noun phrase while "the venv running
  tests" still loads (the noun head completes the phrase).
- **F-A14-4** — Pure -s/-ing nouns were read as verbs by suffix:
  `hasVerbSuffix` made never-verbs verb-shaped, so `isVerbShaped`
  passed every rule: "…the skill analysis / status / news / physics
  / morning / evening / mathematics / economics / chaos / virus /
  campus", in clause-final, article, preposition, subordinator,
  bare-plural and pronoun positions (31 shapes, loading on both rc17
  and rc18 — pre-existing). New list `neverVerbWords` (pure nouns
  plus common adjectives that are never verbs: "happy", "sad",
  "glad", "good", "bad", "great", "big", "small"): a word here is
  never verb-shaped, so no following-word rule, no clause-final rule
  and no bare-singular rule can read it as a verb. It can still be
  the subject of a clause ("…the skill analysis is ready" still
  loads — the aux is the verb, F-A13-3). Words that ARE also verbs
  are deliberately absent: "reference", "report", "setting" (R3).
- Regression tests: `internal/skills/rc19_skills_test.go` — 6 test
  functions, 198 subtests (F-A14-1 must-load, F-A14-2 must-load,
  F-A14-3 must-reject, F-A14-4 must-reject, and two guard functions
  covering the F-A13-1/2/3 behaviours, the F-A12-1 flip cases, the
  F-A12-2 base-form cases, the Audit 12 pin-down and the Audit 13 §7
  residuals). Negative control verified: on unmodified rc18 the
  four finding functions fail (104 subtests) while the two guard
  functions pass, exactly as designed; the fix was stashed and
  restored byte-identical (diff hash 050c3f7a… verified before and after).

## [0.2.0-rc18] — 2026-11-14

### Audit 13 fixes (v0.2.0-rc18) — see `AUDIT13_2026-11-14.md`

Fixes all three P2s from the fresh-eyes audit of rc17 (0 P0, 0 P1, 3 P2).
**Verdict: NOT production-ready** — the 24–72 h daemon soak (F-A3-4)
is still open, and F-A3-2 (no pprof/debug endpoint) is still open.
Verified by the release coordinator on the final commit: **1,197
tests pass, 0 fail, 4 skip (total 1,201), 21/21 packages, race clean
21/21, no data race**; live DeepSeek proof `17*23+5=396` in ~3.3 s
(rc17: 1,195 total: 1,191 pass, 0 fail, 4 skip).

- **F-A13-1** — The rc17 `clauseVerbAt` predicate never required the
  candidate itself to be verb-shaped: its following-word rules looked
  only at what follows the candidate, so a bare noun at a clause end
  after a plural/compound subject ("…the skill and the venv
  documentation / handbook / manual / library / overview /
  framework", "…the skills documentation / handbook / summary"), a
  noun followed by a preposition/subordinator/adverb ("…the skill
  documentation for beginners / with examples / when ready /
  quickly", "…documentation that helps", "…handbook which explains"),
  or a noun followed by a bare plural ("…the skill reference /
  documentation tools", "…the venv documentation tools/settings",
  "…the skill happy tools", "…the venv reference items") was read as
  a verb phrase and loaded, although it rejected on rc16 (33 shapes).
  The fix is shape-based on both sides. New helper `isVerbShaped`
  (`internal/skills/skills.go:1033`) — a verb-form suffix, an
  object-region verb, or a listed base-form verb — and every
  following-word rule (article, pronoun, adverb, preposition,
  subordinator) now requires the candidate to be verb-shaped before
  it fires; a subordinator after an ambiguous noun ("files that
  helps") attaches a relative clause, not a subordinate clause. The
  clause-final plural rule now accepts only listed base-form verbs
  (`baseFormVerbs`, `skills.go:970`, which gains "dance" and "serve"
  for F-A12-2), never a bare word with no verb morphology. The
  bare-plural rule does not fire when the following word is a guarded
  noun ("tools", "items", "settings") unless the candidate is a
  listed verb ("dance users", "serve users" — the F-A12-2 pin-down
  still loads for the right reason); a bare plural that is itself an
  object-region verb ("documentation runs", "running tests") is the
  verb of the clause, with the candidate as its subject.
- **F-A13-2** — The rc17 participle-after rule rejected verb +
  gerund/participle clauses ("…the venv continue learning/running",
  "…the skills stop waiting/learning", "…the venv remain
  waiting/seated", "…the skill files running tests", "…the venv
  waiting tests" rejected on rc17, loaded on rc16 — 13 shapes).
  Listed base-form verbs are now exempt before a participle
  ("continue"/"stop"/"remain" take a gerund/participle complement),
  and a participle with an object — a bare plural ("files running
  tests") or an article ("files running the tests") — is a verb
  phrase whose subject is the candidate. In the compound scan
  (`compoundSubjectStartsClause`, `skills.go:1437`) a participle is
  a predicate only once a noun head has been seen (`subjectComplete`,
  the new 5th parameter of `clauseVerbAt`, `skills.go:1110`): "the
  venv waiting tests" loads, while "the running tests" still rejects
  because the subject noun phrase is still open.
- **F-A13-3** — The rc17 aux-after rule rejected noun-head-subject +
  copula clauses ("…the skill files/notes/guides/reports are ready",
  "…the skill training is ready", "…the skill files will help you /
  have finished" rejected on rc17, loaded on rc16 — 7 shapes). When
  the word after the candidate is an aux, the clause now exists with
  the aux as its verb and the candidate as its subject; the same
  shape covers any other following verb ("the skill files run",
  "…documentation runs"). A guarded noun ("the skill tools are
  ready") still rejects, since it returns earlier. Correction to the
  rc17 notes: "the skill files are ready" was documented as an
  accepted known limitation, but it LOADED on rc16, so this was a
  regression, not a pre-existing limitation — it is fixed here, for
  the right reason (the aux is the verb, not "files").
- Regression tests: `internal/skills/rc18_skills_test.go` — 6 test
  functions, 82 subtests (F-A13-1 must-reject, F-A13-2 must-load,
  F-A13-3 must-load, following-verb must-load, and two guard
  functions covering the F-A11-1 must-load shapes, the F-A12-1 flip
  cases, the F-A12-2 base-form cases and the Audit 12 pin-down).
  Negative control verified: on unmodified rc17 the four finding
  functions fail (57 subtests) while the two guard functions pass,
  exactly as designed; the fix was stashed and restored byte-identical
  (diff hash 6646fbbb… verified before and after).

## [0.2.0-rc17] — 2026-11-14

### Audit 12 fixes (v0.2.0-rc17) — see `AUDIT12_2026-11-14.md`

Fixes both P2s from the fresh-eyes audit of rc16 (0 P0, 0 P1, 2 P2).
**Verdict: NOT production-ready** — the 24–72 h daemon soak (F-A3-4)
is still open, and F-A3-2 (no pprof/debug endpoint) is still open.
Verified by the release coordinator on the final commit: **1,191
tests pass, 0 fail, 4 skip (total 1,195), 21/21 packages, race clean
21/21, no data race**; live DeepSeek proof `17*23+5=396` in ~4.0 s
(rc16: 1,190 total: 1,186 pass, 0 fail, 4 skip).

- **F-A12-1** — The rc16 clause-verb recogniser read `-s`/`-ed`/`-ing`
  nouns and adjectives as clause verbs ("Activate the tool, the skill
  files / training / notes / meetings" and "…the skill and the
  running/open/tired venv" loaded on rc16, rejected on rc15). The
  fix is shape-based on both sides. `isVerbForm` is replaced by
  `clauseVerbAt` (`internal/skills/skills.go:1059`), a context-aware
  predicate that looks at the word after the candidate: a pure noun
  head is never a verb (`verbFormNonVerbs`, extended with the common
  skill-doc noun heads — meetings, recordings, tutorials, manuals,
  resources, materials, templates, summaries, sessions, conversations,
  responses, problems, decisions, discussions, jobs, tickets, users,
  members, teams, goals, customers, clients, editors, developers,
  managers, students, teachers, owners, admins, events, policies,
  procedures, workflows, warnings — `skills.go:826`); a word that is
  both a noun and a verb form (`verbFormAmbiguousNouns`: files,
  notes, guides, training, learning, building — `skills.go:869`) is
  a noun at a clause end, before a singular noun, or before a
  preposition, and a verb when followed by an article, a pronoun, a
  bare plural object, an adverb, or a prepositional phrase ("the
  skill files the report", "the skill training the users" still
  load). In the compound scan (`compoundSubjectStartsClause`,
  `skills.go:1314`) a participle, object-region verb or base-form
  word immediately before a noun is a modifier, not the verb, and
  the scan skips its head noun too, so the head noun is never
  misread as the verb ("the skill and the running tests" rejects).
  A bare plural object is checked before the object-region verb
  rule, because a word like "tests" is both a plural noun and an
  object-region verb, and as the word after a candidate it is the
  object ("the skill running tests" loads). An unlisted adverb
  ("quickly") still rejects, as on rc15/rc16, and the pre-existing
  "the skill setting" load (via `activateObjectClosers`, already
  loading on rc15) is unchanged — it is not part of F-A12-1.
- **F-A12-2** — Base-form verbs outside the finite `baseFormVerbs`
  list ("dance", "serve") were missed, so "the skills dance" and
  "…the skill and the venv serve the team" rejected. A base-form word
  is now a verb by shape too: after a plural or compound subject it
  is a verb when followed by an object/complement ("serve the team",
  "serve users", "dance users") or by nothing at a clause end ("the
  skills dance", "the skill and the venv dance"), while a base-form
  word followed by a bare singular noun is a modifier ("the skill
  and the venv work plan", "…dance plan" reject). The Audit 12
  pin-down is covered: "…and the venv dance users" loads because
  "dance" is the verb, not because the trailing "users" is misread
  as one. After a singular subject a base form is still a noun
  ("the skill work" is work about skills). Known limitation,
  accepted: "the skill files are ready" still rejects (the singular
  path does not forward-scan for a verb after a noun head) — fail
  closed, and the audit did not require it.
- Regression tests: `internal/skills/rc17_skills_test.go` — 5 test
  functions, 53 subtests (F-A12-1 must-reject on the comma and
  compound paths, F-A12-2 must-load, verb-reading preservation,
  adverb guards). Negative control verified: on unmodified rc16 the
  three F-A12-1/F-A12-2 functions fail (27 subtests) while the two
  guard functions pass, exactly as designed. The Audit 12 W2
  asserting probe (188 cases) passes in full and the W3 probe shows
  only the three documented pre-existing mismatches (L6/L11 adverb
  rejects, R3 "setting" loads).

## [0.2.0-rc16] — 2026-11-14

### Audit 11 fix (v0.2.0-rc16) — see `AUDIT11_2026-11-14.md`

Fixes the single P2 from the fresh-eyes audit of rc15 (0 P0, 0 P1, 1 P2).
**Verdict: NOT production-ready** — the 24–72 h daemon soak (F-A3-4)
is still open, and F-A3-2 (no pprof/debug endpoint) is still open.
Verified by the release coordinator on the final commit: **1,186
tests pass, 0 fail, 4 skip (total 1,190), 21/21 packages, race clean
21/21, no data race**; live DeepSeek proof `17*23+5=396` (rc15: 1,185
total: 1,181 pass, 0 fail, 4 skip).

- **F-A11-1** — The comma-list new-clause carve-out now recognises a
  new clause by shape, not by a finite verb list: the word after the
  skill starts a new clause when it is any verb form — a
  copula/auxiliary, an object-region verb, or a third-person singular
  present (`-s`), past (`-ed`), or progressive (`-ing`) form
  recognised morphologically ("Activate the tool, the skill waits /
  works / explains the plan", "the skill finished", "the skill
  waiting"), possibly after an adverb ("the skill then waits") or a
  parenthetical comma ("the skill, is ready") — or when the skill is
  the first part of a compound subject followed by a verb ("the skill
  and the venv are ready", "the skill and the venv wait"). The
  carve-out now also covers the "and"/"then" path, which had none
  ("Activate the tool, and/then the skill is ready" rejected on both
  rc14 and rc15): the main loop in
  `segmentNamesSkillAsActivateObject` consults the following word
  instead of returning true on a bare "skill". A skill in the
  direct-object position — immediately after "activate", before any
  separator ("Activate the skill is ready") — is still the object
  whatever follows it: the verb after it is a relative clause, and
  if the skill were not the object then "activate" would be bare,
  which is itself a directive. A small set of words that look like
  verb forms but are nouns or prepositions in this position ("the
  skill tools", "the skill during the meeting", "the skill
  including the venv") is excluded, so a noun-noun compound is
  still read as a list object. Regression tests
  (`internal/skills/rc16_skills_test.go`, 5 test functions, 54
  subtests: new-clause verbs, compound subjects, the and/then path,
  all 12 rc15 reject variants, and 14 guard shapes) — verified
  sensitive: with the fix reverted (rc15 code) all 27 must-load
  subtests fail while all 26 must-reject guards pass.

## [0.2.0-rc15] — 2026-11-14

### Audit 10 fixes (v0.2.0-rc15) — see `AUDIT10_2026-11-14.md`

Fixes all 5 P2s from the fresh-eyes audit of rc14 (0 P0, 0 P1, 5 P2).
**Verdict: NOT production-ready** — the 24–72 h daemon soak (F-A3-4)
is still open, and F-A3-2 (no pprof/debug endpoint) is still open.
Verified by the release coordinator on the final commit: **1,181
tests pass, 0 fail, 4 skip (total 1,185), 21/21 packages, race clean
21/21, no data race**; live DeepSeek proof `17*23+5=396` (rc14: 1,181
total: 1,177 pass, 0 fail, 4 skip).

- **F-A10-1** — A re-delivered turn no longer drops the skill
  restriction: the `skill` tool is now excluded from the tool journal
  (like the subagent lifecycle tools), so every delivery re-executes
  the invoke and re-activates the skill instead of replaying a
  recorded body that never fired `setActiveSkill`. A tool call the
  restriction refused in run 1 is refused again in run 2, and is
  never journalled. Regression test
  `TestJournalSkillRestrictionSurvivesRedelivery`
  (`internal/core/agent_journal_test.go`) — verified sensitive: with
  the exclusion reverted it fails exactly as the Audit 10 probe did
  (disallowed tool runs once on re-delivery, journal grows to 3
  records). Journal files written by rc14 may still hold a recorded
  skill invoke; it is ignored (the exclusion skips the lookup) and
  pruned with the rest of the file after 24 h.
- **F-A10-2** — The dead-letter ledger now has a production consumer:
  `/usage` surfaces it as its own daemon-wide line — "Evicted
  subagent tasks (never charged, daemon-wide): N task(s), M tokens" —
  the charged/uncharged split, rendered only when the ledger holds
  something. The read is non-draining (via
  `messaging.CommandEnv.DeadLetter`, wired in `cmd/oriond` to
  `DeadLetterTokens`/`DeadLetterTasks`), so the counts stay stable
  across `/usage` calls and `DrainDeadLetter` remains available for a
  future charge path; the ledger tokens are never folded into any
  chat's totals. Tests in `internal/messaging/usage_cost_test.go`.
- **F-A10-3** — A comma-separated object list naming the skill now
  rejects: "Activate the tool, the skill" and "Activate the tool, the
  skill, and the venv" are directives, not prose. The object-region
  logic (`internal/skills/skills.go`, `commaListNamesSkill`) treats a
  comma-separated list as objects of the same `activate`, unless the
  word after the skill starts a new clause with the skill as its
  subject ("Activate the tool, the skill is ready" still loads — as
  do "the skill runs the tests" and "the skill helps you").
- **F-A10-4** — The sweep test no longer flakes under plain
  full-suite load: `TestSweepChargesTaskFinishingDuringSweep` now
  retries up to 30 attempts (was 10) with 200 filler tasks (was 80)
  widening the mid-sweep window. 10/10 plain and 3/3 `-race` runs.
- **F-A10-5** — The `orion daemon` / `orion telegram poll` commands
  no longer return while a turn is still writing: the turn handler
  now registers the run with the App (mirroring oriond's `runTurn`),
  and both commands drain registered turns after `Poll` returns
  (`pollTelegram`, 30 s bound) before exiting — so the data dir is
  never torn down under a live turn. Note: waiting for the turn's
  apology message does NOT work as the completion signal (it is sent
  on the cancelled poll context, so it never reaches the bot); the
  drain is the guarantee.
- **`orion run --config <path>`** (Audit 10 W4 observation i): the
  CLI can now load a named config file instead of
  `~/.orion/config.json`, so scratch-config runs work as written
  (both `--config <path>` and `--config=<path>`). A named file that
  does not exist is an error, not a silent fall back to the defaults
  (which would resolve to the live config).

## [0.2.0-rc14] — 2026-11-13

### Audit 9 fixes (v0.2.0-rc14) — see `AUDIT9_2026-11-13.md`

Fixes the 1 P1 and the P2 batch from the fresh-eyes audit of rc13
(0 P0, 1 P1, 9 P2). **Verdict: NOT production-ready** — the 24–72 h
daemon soak (F-A3-4) is still open, and F-A3-2 (no pprof/debug
endpoint) is still open. Verified by the release coordinator on the
final commit: **1,177 tests pass, 0 fail, 4 skip (total 1,181),
21/21 packages, race clean 21/21, no data race**; live DeepSeek proof
`17*23+5=396` (rc13: 1,151 total: 1,147 pass,
0 fail, 4 skip).

- **F-A9-1** — Re-delivered turns no longer re-run their tools: a
  durable per-update tool journal (`internal/core/tooljournal.go`,
  one 0600 JSONL file per update id under `<dataDir>/tool-journal/`,
  append + Sync before the result returns, 24 h retention) records
  completed tool calls keyed by (update id, tool, canonical
  arguments, occurrence); on re-delivery the recorded result is
  replayed instead of executing again. The update id is plumbed
  through PollStream into the agent context
  (`internal/messaging/telegram.go`, `core.WithUpdateID`), wired in
  app Build / AgentForChat / provider-switch paths
  (`internal/app/app.go`, `internal/app/chats.go`), and cleared for
  subagent child turns (`internal/app/subagent.go`). Subagent
  lifecycle tools are excluded (their results reference in-process
  Manager state that dies with the restart). Regression tests in
  `internal/core/tooljournal_test.go`,
  `internal/core/agent_journal_test.go`,
  `internal/messaging/rc14_journal_test.go`, and
  `internal/app/rc14_journal_test.go` — including a kill-mid-turn
  replay test with a without-journal negative control that runs the
  tool twice.
- **F-A9-P2-6** — Ollama no longer drops MaxTokens: `buildRequest`
  maps it to `options.num_predict`, merged with temperature into one
  Options map (`internal/model/ollama.go:132-142`), with wire-level
  regression tests for Chat and Stream
  (`internal/model/ollama_rc14_test.go`).
- **F-A9-P2-1** — Skills-validator negation carve-out now checks
  that the negated verb is an activate form, not merely that a
  negation word is present (`clauseNegatesVerb`,
  `internal/skills/skills.go:856`).
- **F-A9-P2-8** — Dead-letter ledger is now a real consumer:
  finished tasks carry a `charged` flag (`MarkCharged`,
  `internal/subagent/subagent.go:367`); eviction folds only
  uncharged tokens and logs every eviction; `DrainDeadLetter`
  (subagent.go:388) surfaces the ledger. The sweep
  (`internal/app/chats.go:717`) and the fetch path
  (`internal/tools/subagent.go:393`) both mark charged — the fetch
  site was a coordinator-found gap the worker missed (the CLI path
  has no sweep, so a fetched-only task would still have
  double-counted on eviction).
- **F-A9-P2-2** — Bare-"Activate" trim set extended to `.!?,;:`
  (`internal/skills/skills.go:667`).
- **F-A9-P2-3** — Object-order shield now walks the direct-object
  region of the activate verb instead of only its first object
  (`segmentNamesSkillAsActivateObject`,
  `internal/skills/skills.go:764`): "Activate the tool and the
  skill" / "Activate the tool, then the skill" are rejected,
  "Activate the tool using the skill" still loads.
- **F-A9-P2-4** — `activateWord` now matches reactivate forms
  (`reactivat(e|es|ed|ing|ion)`, `internal/skills/skills.go:439`);
  `-ion` nouns are directives only when governed by a directive verb
  (`start/begin/trigger/...`), so prose discussion ("Skill
  activation is manual.") does not false-positive.
- **F-A9-P2-5** — Marker regex widened: delimiters `=`, `-`, `*`;
  `-`/`_` joins inside marker words (`END-SKILL`, `BEGIN_SKILL`);
  list-item/bullet prefix allowed (`internal/skills/skills.go:351`).
- **F-A9-P2-7** — Panicked turns no longer leak the typing
  goroutine: `stopTyping` is a `sync.OnceFunc` registered as a
  defer (`internal/messaging/telegram.go:1708,1717`), so it runs on
  the panic path too and does the final card edit in the same defer;
  `emit`'s `card.Add` is wrapped so a panic inside it still releases
  cardMu.
- **F-A9-P2-9** — Both timing-flaky tests under -race are
  deterministic now (test-only changes): the sweep test drives the
  sweep in lock-step with a TryLock spin and verifies the
  interleaving (a marked filler proves the sweep passed the blocked
  task while it was still running; `internal/app/rc12_w2_test.go`),
  and the MCP kill test's deadline is load-adaptive (20× observed
  per-line cost, floored at 60 s, capped at 4 min;
  `internal/mcp/mcp_test.go`).

## [0.2.0-rc13] — 2026-11-13

### Audit 8 fixes (v0.2.0-rc13) — see `AUDIT8_2026-11-13.md`

Fixes the 2 P1s and the P2 batch from the fresh-eyes audit of rc12
(0 P0, 2 P1, 8 P2). **Verdict: NOT production-ready** — the 24–72 h
daemon soak (F-A3-4) is still open, and F-A3-2 (no pprof/debug
endpoint) is still open. Verified by the release coordinator on the
final commit: **1,147 tests pass, 0 fail, 4 skip (total 1,151),
21/21 packages, race clean 21/21, no data race**; live DeepSeek proof
`17*23+5=396` in 3.9s, first attempt (rc12: 1,130 total: 1,126 pass,
0 fail, 4 skip).

- **F-A8-1** — CLI `/compress` budget gate now records CLI spend to
  the checked bucket (the gate checked a bucket nothing ever filled,
  so CLI compresses were never refused or recorded).
- **F-A8-2** — PollStream offset is confirmed on handler completion,
  not at handler spawn, so a crash mid-turn can no longer lose the
  update permanently.
- **F-A8-P2-1** — registry eviction no longer voids token accounting:
  finished, unswept subagent tasks are no longer lost when the
  registry evicts past its cap (9.1% of tokens lost in Audit 8).
- **F-A8-P2-2** — the fetch path now charges timed-out late usage
  instead of dropping it.
- **F-A8-P2-3** — `/compress` completion overshoot capped (5.8x
  overshoot in Audit 8).
- **F-A8-P2-4** — version fallback bumped to `0.2.0-rc13`, and `make
  release-check` now also builds without ldflags and asserts the
  plain build reports the release in preparation, so a stale
  fallback can never escape release-check again (this entry).
- **F-A8-P2-5/6/7** — skills validator gaps closed.
- **F-A8-P2-8** — duplicate-tap approval test made deterministic (was
  flaky under `-race`, ~14%).
- **Integration (coordinator)** — two pre-existing data races found
  by `-race` during rc13 verification, both reproduced on rc12 and
  fixed with regression tests: transcript `scrubRecord` mutated the
  caller's shared ToolCalls array, racing with a concurrent FileStore
  Save (rc12: 3/10 runs); approval log lines read the `os.Stderr`
  global from background goroutines, racing with the test `capture()`
  helper's swap (rc12: 1/10 runs). The sweep mid-sweep test's
  missed-interleaving direction now retries instead of hard-failing,
  and the turn-slots-full approval test now drains its queued turns
  before returning (its TempDir cleanup failed under load when a
  still-running turn created files after the test returned).

## [0.2.0-rc12] — 2026-11-13

### Audit 7 fixes (v0.2.0-rc12) — see `AUDIT7_2026-11-13.md`

Fixes the 2 P1s and the P2 batch from the fresh-eyes audit of rc11
(0 P0, 2 P1, 8 P2). **Verdict: NOT production-ready** — the 24–72 h
daemon soak (F-A3-4) is still open, and F-A3-2 (no pprof/debug
endpoint) is still open. Verified by the release coordinator on the
final commit: **1,126 tests pass, 0 fail, 4 skip (total 1,130),
21/21 packages, race clean 21/21, no data race**; live DeepSeek proof
`17*23+5=396` (rc11: 1,105 total: 1,101 pass, 0 fail, 4 skip).

- **F-A7-1** — `/compress` pre-flight budget refusal (metered in
  rc11, never refused at the daily limit).
- **F-A7-2** — subagent token accounting: late usage from a
  ctx-ignoring executor, and the sweep-window race that can
  permanently lose a recorded task's tokens.
- **F-A7-P2-1..P2-7** — fuzzy skill-marker validation; oriond onto
  the shared gate constructor; callback batch-split (dispatch
  callbacks before messages); manual `/compress`
  summary-preservation check; `#### Tools` heading warning; docs
  pass + `make release-check` (this entry).
- **F-A7-P2-8** — slash commands documented as oriond-only (not
  wired): `orion daemon` / `orion telegram poll` are turn-only —
  slash commands go to the agent as plain text there, and
  `/restart` owner-only holds vacuously (there is no `/restart`
  command to refuse, only a text turn about restarting). `oriond`
  is the only entrypoint with the full command framework (the
  19-command `/` menu). Documented in README (quick start + CLI
  table) and in code (`cmd/orion/main.go`: the `cmdTelegram`
  comment and the `daemonTurnHandler` doc); wiring the registry
  into these entrypoints would duplicate ~300 lines of per-turn
  env, pause, footer, retry and restart plumbing with no shared
  home for it.

- **Known limitation (found during the F-A7-2 fix, follow-up for
  rc13/Audit 8):** the subagent Manager evicts finished tasks past
  `maxEntries=100`, so a chat spawning more than 100 subagent tasks
  before a sweep can lose the oldest finished, unswept entries —
  their tokens can never be swept. Fix candidates: raise the cap,
  or sweep-before-evict (needs care — the Manager does not know
  about budgets).

## [0.2.0-rc11] — 2026-11-13

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

- [ ] `make release-check` passes. It builds `orion`, asserts
  `orion version` matches `git describe --tags --exact-match`,
  asserts this file has a heading for the tag, and asserts the
  README test counts match the suite (it runs the full suite
  itself). **The version string must match the tag before any
  push.** If the version check fails, do not push: check that
  `Version` is a `var` in `internal/version/version.go`, that the
  Makefile / `install.sh` ldflags pass `-X
  …version.Version=<tag without the leading v>`, and that the tag
  exists on the exact commit being built (`git describe --tags
  --exact-match`).
- [ ] Full suite green (`go test ./...`, `go test -race ./...`,
  `gofmt -l`, `go vet ./...`) with the counts recorded in this
  section; live DeepSeek proof (`17*23+5=396`) run. (`make
  release-check` already runs the plain suite; `-race`, `gofmt`,
  `go vet`, and the live proof are still manual.)
- [ ] Docs claims re-verified against the code (package count via
  `go list ./...`, tool count, command count). `make release-check`
  covers the CHANGELOG tag heading and the README test counts;
  the remaining numbers are checked by hand.

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
