# Audit 3 — Workstream 4: Long-Running Daemon Measurements

Daemon under test: `bin/oriond` (Telegram gateway polling as @Magi420x_bot,
provider deepseek / deepseek-chat, allowlisted chat only).

> NOTE: the daemon exposes NO debug/pprof endpoint (verified by grep over
> cmd/oriond and internal/messaging: no `net/http/pprof`, no
> `http.ListenAndServe`). Go goroutine count is therefore NOT directly
> observable from outside; OS thread count (`Threads` in /proc) is used as
> the proxy, as the task allows.

## Daemon under measurement

- PID 15237, started Fri 2026-11-13 00:36:47 UTC (08:36:47 SGT), from the
  **rc1** binaries (commit 69fc437). `/proc/15237/exe` resolves to
  `bin/oriond (deleted)` — the binary was rebuilt for rc2 (commit 7e1cb34)
  after the daemon started, so this process is running rc1 code.
- Log: `/tmp/oriond-2026-11-13-0835.log` (only 1 line: startup banner —
  the daemon logs NOTHING per poll, see Findings).
- Persistence: `~/.orion/memory.json`, `~/.orion/audit.jsonl` (+ signed head),
  `~/.orion/sessions.json`.

## Samples (UTC time; SGT = UTC+8)

Columns: RSS kB | HWM (peak RSS) kB | VmSize kB | Threads | open FDs | CPU utime/stime (ticks) | /proc I/O read_bytes / write_bytes.

| UTC | SGT | phase | PID | elapsed | VmRSS | VmHWM | VmSize | Threads | FDs | CPU | io_read | io_write |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 2026-11-13T01:09:50Z | 09:09:50 | rc1 | 15237 | 33:03 | 10992 | — | 1860704 | 9 | 5 | utime=11 stime=24 | 78680064 | 397312 |
| 2026-11-13T01:10:30Z | 09:10:30 | rc1 | 15237 | 33:43 | 10992 | 16092 | 1860704 | 9 | 6 | utime=11 stime=24 | 78680064 | 397312
0 |

## Findings so far (2026-11-13 ~01:25 UTC)

1. **No goroutine observability**: no pprof/debug endpoint (grep over
   cmd/oriond + internal/messaging). Threads (9, flat) is the proxy.
2. **Polling is invisible in the log**: `/tmp/oriond-2026-11-13-0835.log`
   has exactly 1 line (startup banner) after 40+ min. PollStream
   logged NOTHING on transient errors — a network outage would look
   identical to healthy polling. FIXED (see Fixes).
3. **Polling transport**: single long-poll connection via the host
   egress proxy (198.19.0.1:3128), 30s server long-poll, 35s client
   timeout, backoff 1s doubling to 5s, fatal 401/403/409 exits.
   A second ESTAB connection (fd 6) appeared ~01:14 — under watch.
4. **SIGTERM did not drain**: main's ctx.Done path returned 0 at once
   while /restart drains 10s. FIXED (see Fixes).
5. **Memory persistence is incremental**: agent.Run adds the user
   message at turn start and each assistant/tool message as it is
   produced (FileStore.AddMessage persists immediately), and
   a.Close() → Agent.Shutdown saves again. A SIGTERM mid-turn loses
   at most the in-flight step, not the conversation.

## Fixes (this workstream)

- F1 — `internal/messaging/telegram.go`: `WithLogger` option +
  `TelegramGateway.logf`; PollStream now logs each transient
  getUpdates failure (with backoff) and one recovery line. Errors
  passed are already token-redacted by call().
- F2 — `cmd/oriond/main.go`: gateway wired with a stderr logger;
  SIGTERM/SIGINT path now drains in-flight turns up to 10s (same as
  /restart) before returning 0.
- Tests: `internal/messaging/audit3_daemon_test.go` (NEW, 4 tests):
  recovery from HTTP 500s, from connection resets, survival of a
  sustained outage until ctx cancel, logger lines + no token leak.
  All pass; full messaging + oriond suites pass, vet clean.
- F3 — `internal/messaging/session.go` + `cmd/oriond/main.go`:
  `SessionStore.Save()` (new, exported) + a deferred save on every
  oriond exit path. Found via Workstream 5's
  TestDaemonStartStopRestart, which deterministically failed on
  "sessions.json missing" after a no-turn SIGTERM stop: the store
  only writes on mutation, so a zero-turn daemon left no file.
  Regression test: TestSessionStoreSaveCreatesFile (5th test in
  audit3_daemon_test.go). After F3 the daemon suite is fully green.

## Network-drop simulation

- LIVE simulation against the real daemon was NOT done by blocking
  api.telegram.org: the daemon reaches Telegram through the host's
  shared egress proxy (198.19.0.1:3128) and runs as root (same UID as
  this shell and other agents' work) — there is no per-process
  firewall/namespace handle that would not also break the host's
  egress for everyone else. Faking it would have been dishonest.
- INSTEAD: in-process simulation against the real PollStream code
  (httptest server returning HTTP 500 / RST for the first calls, then
  recovering) — see tests above. Plus a live SIGSTOP/SIGCONT stall
  probe on the real daemon (process-local, safe) — see below.
| 2026-11-13T01:15:31Z | 09:15:31 | rc1 | 15237 | 38:44 | 12164 | 16092 | 1860704 | 9 | 6 | utime=12 stime=29 | 95244288 | 794624
0 |
| 2026-11-13T01:20:31Z | 09:20:31 | rc1 | 15237 | 43:44 | 10744 | 16092 | 1860704 | 9 | 6 | utime=13 stime=33 | 115736576 | 794624
0 |
| 2026-11-13T01:25:32Z | 09:25:32 | rc1 | 15237 | 48:44 | 11452 | 16092 | 1860704 | 9 | 6 | utime=15 stime=38 | 134864896 | 794624
0 |
| 2026-11-13T01:30:32Z | 09:30:32 | rc1 | 15237 | 53:44 | 13592 | 16664 | 1934436 | 10 | 6 | utime=21 stime=44 | 139300864 | 991232
0 |
| 2026-11-13T01:35:32Z | 09:35:32 | rc1 | 15237 | 58:44 | 11104 | 16664 | 1934436 | 10 | 6 | utime=22 stime=47 | 151810048 | 991232
0 |
| 2026-11-13T01:40:32Z | 09:40:32 | rc1 | 15237 | 01:03:45 | 11276 | 16664 | 1934436 | 10 | 6 | utime=24 stime=52 | 183353344 | 991232
0 |
| 2026-11-13T01:45:32Z | 09:45:32 | rc1 | 15237 | 01:08:45 | 11292 | 16664 | 1934436 | 10 | 6 | utime=25 stime=57 | 209666048 | 991232
0 |

## Environment incident — filesystem writes failing EPERM (2026-11-13 ~01:43–01:47 UTC, ongoing)

- `go build`/`go mod init` fail: files are created 0-byte, then the
  write fails "operation not permitted" (exit 1, often with NO output
  on stdout/stderr). Affects the shared ~/.cache/go-build AND fresh
  private GOCACHE dirs AND plain shell writes (`printf > file`,
  `echo > file`) in ~/workspace. Same 0-byte-then-EPERM shape as the
  /tmp flapping Irfan saw 09:25–09:29 SGT. It comes and goes on a
  ~minute scale (sampler wrote fine 01:45:32, my write failed
  01:47:27). Another Audit 3 worker (PID 98097) is already retrying
  `go build ./internal/tools/` in a loop against the same failure.
- IMPACT ON THIS WORKSTREAM: the rc2 rebuild + daemon restart is
  BLOCKED until writes recover — killing the healthy rc1 daemon now
  would leave no daemon at all, so the restart is DEFERRED. The live
  daemon (PID 15237) is unaffected in memory; its ~/.orion writes
  are at risk only if a turn runs during a flap window (FileStore
  Save would error and be logged).
- `go vet` on cmd/oriond + internal/messaging passed BEFORE the flap
  (~01:31 race run clean). Full-suite re-verification will be
  re-run after the flap clears.
| 2026-11-13T01:50:32Z | 09:50:32 | rc1 | 15237 | 01:13:45 | 11376 | 16664 | 1934436 | 10 | 6 | utime=26 stime=62 | 245235712 | 991232
0 |
| 2026-11-13T01:55:32Z | 09:55:32 | rc1 | 15237 | 01:18:45 | 11680 | 16664 | 1934436 | 10 | 6 | utime=27 stime=65 | 259346432 | 991232
0 |
| 2026-11-13T02:00:32Z | 10:00:32 | rc1 | 15237 | 01:23:45 | 9640 | 16664 | 1934436 | 10 | 6 | utime=28 stime=67 | 264896512 | 991232
0 |
| 2026-11-13T02:05:32Z | 10:05:32 | rc1 | 15237 | 01:28:45 | 10676 | 16664 | 1934436 | 10 | 6 | utime=29 stime=69 | 277536768 | 991232
0 |
| 2026-11-13T02:10:33Z | 10:10:33 | rc1 | 15237 | 01:33:45 | 10912 | 16664 | 1934436 | 10 | 6 | utime=36 stime=74 | 298827776 | 991232
0 |
| 2026-11-13T02:15:33Z | 10:15:33 | rc1 | 15237 | 01:38:46 | 10248 | 16664 | 1934436 | 10 | 6 | utime=37 stime=78 | 320200704 | 991232
0 |
| 2026-11-13T02:17:52Z | 10:17:52 | rc2 | 130355 | 01:07 | 16228 | 16228 | 1860520 | 9 | 6 | utime=3 stime=11 | 6512640 | 139264
0 |

## Restart to rc2+fixes (2026-11-13 02:15–02:17 UTC)

- rc1 daemon PID 15237: total uptime 1h39m (00:36:47–02:15:46 UTC).
  RSS over 1h39m: 9.4–12.2 MB band, NO upward trend (first 10992,
  last 10912; HWM 16664 from the 01:13 turn). Threads 9 → 10 after
  the first turn, flat thereafter. No leak.
- SIGTERM 02:15:46 UTC: process exited within ~2s, no error in log.
  Exit code not directly observable (daemon was started by another
  agent via nohup); in-process TestDaemonStartStopRestart proves
  run() returns 0 on SIGTERM for this code path.
- STATE LOSS OBSERVED on that SIGTERM (rc1, no-drain path): Irfan's
  last turn (Python IP-check script request, 26 messages incl. failed
  file_write attempts) is fully in memory.json (19 → 45 messages,
  10KB → 36KB) BUT its audit entry (would be seq 9) is MISSING and
  its usage was never recorded (sessions.json still turns: 2,
  tokens 6501, mtime 01:13). The turn's final Bengali reply is in
  memory but was most likely never SENT (send happens after the
  handler returns; rc1 exited first). Cause: rc1 returns from the
  SIGTERM path immediately without draining, racing the in-flight
  turn's unwind (audit append + usage write + reply send). The Drain
  fix (F2) addresses exactly this. NOTE: this also means the turn was
  still in flight (or just finishing) at 02:15:46 — Irfan may not
  have received the reply; he may retry, and the new daemon will
  answer from the full 45-message history.
- New daemon PID 130355, started 02:16:xx UTC from bin/oriond built
  02:15 UTC in a private mount namespace (fake /dev/null — see env
  incident below) from the current tree: rc2 + my F1/F2/F3 fixes +
  in-flight P1 changes (allowlist default-deny, ORION_TELEGRAM_BASE_URL).
  Verified 61s after start: banner "running as @Magi420x_bot
  (deepseek/deepseek-chat)", single ESTAB poll connection (fd 3) to
  the egress proxy, no 409, rchar advancing, memory/sessions md5
  unchanged across the restart. Log:
  ~/workspace/orion-go/oriond-live-2026-11-13.log (gitignored).
- rc2 sampler started 02:17:52 UTC (label rc2, 5-min cadence).
- Full suite re-run (in unshare, 02:16 UTC): all packages pass
  EXCEPT internal/tools — 5 failures in web_ssrf_test.go
  (TestFetchPinsResolvedIPAgainstRebinding, RedirectToPrivate,
  PinnedDialPreservesSNIAndHost, ViaProxyKeepsPreCheck,
  TrustedEgressCIDRAllowsFetch), all EOF/fetch errors in the P1
  phase's in-flight DNS-rebinding work. NOT my area, NOT my code.
  cmd/oriond + internal/messaging: PASS (incl. the 5 new tests).

## Environment incident, continued (02:09–02:12 UTC)

- `/dev/null` is MISSING from /dev (tmpfs): `ls /dev/null` → No
  such file; PID 1 and the daemon hold it as "(deleted)". mknod
  fails EPERM (no CAP_MKNOD in this user namespace). `go build`
  fails because the compiler opens /dev/null for build IDs.
- WORKAROUND (safe, private only): `unshare -rm` + bind-mount an
  empty regular file over /dev/null inside the private mount
  namespace, then build there. No host-visible mounts are touched.
  Binaries built this way are fine (verified: oriond --version,
  full messaging/oriond suites pass, live daemon runs).
- The EPERM-on-write flap and the missing /dev/null are VM-level
  incidents, not Orion bugs. The coordinator/parent should know:
  /dev/null needs restoring at the host level (a VM reset restores
  it), and `go build`/`go test` currently REQUIRE the unshare
  workaround in this VM.
2026-11-13T02:20:33Z [rc1] pid 15237 gone after 14 samples
| 2026-11-13T02:22:52Z | 10:22:52 | rc2 | 130355 | 06:07 | 11464 | 15800 | 1860528 | 9 | 6 | utime=4 stime=15 | 25522176 | 139264
0 |
| 2026-11-13T02:27:52Z | 10:27:52 | rc2 | 130355 | 11:07 | 9604 | 15800 | 1860528 | 9 | 6 | utime=5 stime=17 | 31297536 | 139264
0 |
| 2026-11-13T02:32:52Z | 10:32:52 | rc2 | 130355 | 16:07 | 9248 | 15800 | 1860528 | 9 | 6 | utime=6 stime=18 | 34304000 | 139264
0 |
| 2026-11-13T02:37:52Z | 10:37:52 | rc2 | 130355 | 21:08 | 10116 | 15800 | 1860528 | 9 | 6 | utime=6 stime=21 | 51920896 | 139264
0 |
| 2026-11-13T02:42:52Z | 10:42:52 | rc2 | 130355 | 26:08 | 10196 | 15800 | 1860528 | 9 | 6 | utime=7 stime=24 | 68038656 | 139264
0 |
| 2026-11-13T02:46:45Z | 10:46:45 | rc2b | 2971 | 00:43 | 15444 | 15444 | 1860184 | 9 | 6 | utime=2 stime=4 | 6426624 | 122880
0 |

## Runtime restart incident + F1 live proof (2026-11-13 ~02:41–02:47 UTC)

- The runtime restarted mid-phase (my wait-5 sleep was interrupted
  after ~4m with no confirmed result; recovery notice received).
  Effects: the rc2 daemon PID 130355 is GONE (no /proc entry) and
  my rc2 sampler is gone. The daemon did NOT exit by itself: its
  log's last line is a transient poll error, not a fatal one — it
  was killed externally by the runtime restart.
- F1 PROVED LIVE: the log's last line from PID 130355 is
  `oriond: telegram poll: transient getUpdates error (attempt 1,
  retry in 1s): messaging: telegram getUpdates: request: Get
  "https://api.telegram.org/bot[redacted]/getUpdates?offset=0&timeout=30":
  unexpected EOF`
  A real connection drop (unexpected EOF, almost certainly the
  restart disrupting the proxy link) was logged with the token
  redacted and the backoff named. No recovery line followed because
  the process was killed during the 1s backoff. (offset=0 is
  normal: no updates had arrived since the 02:16 start, so the
  offset never advanced.)
- `/dev/null` is BACK after the restart: `crw-rw-rw- 1 root root
  1, 3` dated 02:45. The unshare workaround is no longer needed
  for builds (verify on next build).
- RESTARTED 02:46 UTC: PID 2971, same bin/oriond (02:15 build,
  rc2+F1/F2/F3). Verified 37s after start: banner in log, single
  ESTAB poll connection (fd 3), rchar 617361, RSS 15320, Threads 9,
  no new errors. State files unchanged across the incident
  (memory.json md5 0f18a989…, sessions.json 694f4d63…, audit seq 8).
- rc2b sampler started 02:46:45 UTC (label rc2b, 5-min cadence).
- NOTE on the earlier stall probes (60s + 5-min SIGSTOP): both
  survived with NO poll error at all — the long-poll response
  buffers server-side and is read normally on resume; the 5-min
  freeze only forced a reconnect (new local port). Stall survival
  is proven; the error path itself is proven by the unit tests AND
  now by this live transient-error log line.

## Stall probes (2026-11-13 02:33–02:41 UTC, PID 130355)

- 60s SIGSTOP: survived, same TCP connection (port 57592), no log
  lines, rchar +290 in 90s. No error, no reconnect needed.
- 5-min SIGSTOP: survived, connection re-established (port 57592
  → 47004, fd 3), rchar +7210, RSS 9368 → 13340 transiently
  (reconnect spike; sampler showed it settling back to ~10.2MB
  by 02:37–02:42). No error logged — nothing failed, so nothing
  to log. Polling resumes cleanly after multi-minute stalls.
| 2026-11-13T03:03:07Z | 11:03:07 | rc2b | 2971 | 17:05 | 9156 | 15668 | 1934260 | 10 | 6 | utime=6 stime=9 | 27361280 | 122880
0 |
| 2026-11-13T03:03:28Z | 11:03:28 | rc2b | 2971 | 17:26 | 9856 | 15668 | 1934260 | 10 | 6 | utime=6 stime=10 | 30814208 | 122880 |
| 2026-11-13T03:08:28Z | 11:08:28 | rc2b | 2971 | 22:26 | 11184 | 15668 | 1934260 | 10 | 6 | utime=7 stime=15 | 53129216 | 122880 |
| 2026-11-13T03:13:28Z | 11:13:28 | rc2b | 2971 | 27:26 | 5552 | 15668 | 1934260 | 10 | 6 | utime=8 stime=19 | 85102592 | 122880 |
| 2026-11-13T03:18:28Z | 11:18:28 | rc2b | 2971 | 32:26 | 11252 | 15668 | 1934260 | 10 | 6 | utime=9 stime=24 | 112324608 | 122880 |
| 2026-11-13T03:19:09Z | 11:19:09 | rc2c | 37429 | 00:17 | 14428 | 14532 | 1859672 | 9 | 6 | utime=1 stime=3 | 1523712 | 57344 |

## Final live SIGTERM verification + handover (2026-11-13 03:18–03:19 UTC)

- F2+F3 PROVED LIVE on PID 2971 (rc2b, 32 min uptime): SIGTERM
  03:18:47 → exited in ~2s, no error in log. sessions.json mtime
  02:45:25 → 03:18:48 — the deferred shutdown Save (F3) ran;
  content md5 unchanged (694f4d63…, correct: no turns in that
  run). memory.json intact (0f18a989…). Drain had no active turns
  to wait for, so the exit was prompt; the Drain path itself is
  covered by TestDaemonStartStopRestart (in-process, exit 0).
- RESTARTED 03:18:5x UTC: PID 37429, same bin/oriond (02:15 build).
  Verified: banner in log, single ESTAB poll connection (fd 3,
  port 60642), rchar advancing, RSS 14428 at start. rc2c sampler
  running (5-min cadence). THIS DAEMON IS LEFT RUNNING for the
  parent/next phase. Its log:
  ~/workspace/orion-go/oriond-live-2026-11-13.log (appended, all
  four banners + the one live transient-error line are in it).
- FINAL VERIFICATION (plain toolchain, no unshare — /dev/null is
  back): gofmt clean, go build ./cmd/oriond ./cmd/orion exit 0,
  go vet clean, go test -count=1 ./cmd/oriond ./internal/messaging
  PASS, go test -race same PASS.
- Sampler note: the rc2b sampler (PID 3884) wedged 02:46–03:03
  (no rows; cause unknown, most likely the FS flap stalling its
  reads) and was restarted 03:03 with a fixed awk (anchored
  $1=="write_bytes:" — the old pattern also matched
  cancelled_write_bytes and split table rows). Gap honestly
  recorded: no 5-min samples 02:51–02:58; direct readings in the
  window (RSS 9668 @ 03:02) were in-band. Earlier "0" lines in
  the CSV/MD table are the cancelled_write_bytes fragments from
  that bug — values are all present, just ragged.

## Verdict (measured, not impressions)

- NO MEMORY LEAK: 25+ samples over 2h10m of wall-clock sampling
  (01:09–03:19 UTC), three daemon incarnations. RSS bands:
  rc1 9.4–12.2 MB (HWM 16.6), rc2 9.2–11.5 MB (HWM 15.8),
  rc2b 5.5–11.3 MB (HWM 15.7). Spikes come from turns/reconnects
  and settle back; first-vs-last in every incarnation is flat.
  Threads 9 → 10 once (after the first turn; also 9 → 10 in a
  turn-less rc2b run — one lazy runtime/poller thread) and flat
  thereafter. No goroutine count available (no pprof endpoint).
- POLLING IS HEALTHY AND RECOVERS: sole-poller verified (single
  ESTAB, no 409 across two restarts); 60s and 5-min SIGSTOP
  stalls survived with no error and clean reconnect; one real
  transient error (unexpected EOF) was logged live with token
  redacted; in-process tests prove recovery from HTTP 500s,
  connection resets, and sustained outages.
- RESTARTS ARE CLEAN: two live SIGTERMs exited in ~2s with state
  intact (memory/audit/sessions md5s unchanged across restarts).
  One real wart found and fixed: rc1's no-drain SIGTERM raced an
  in-flight turn and lost its audit entry + usage record + reply
  send (the turn's messages survived in memory). F2/F3 close it.
- NOT DONE / OPEN: (1) live Telegram round-trip through the NEW
  daemon still needs Irfan to message @Magi420x_bot (last real
  turn was on rc1; the script-request turn's reply may never have
  reached him — see the 02:15 note); (2) 24–72h soak is beyond
  one phase — the rc2c daemon + sampler are left running for it;
  (3) the VM's filesystem incidents (EPERM flap, missing
  /dev/null) are host problems, not Orion's, but they blocked
  builds for ~30 min and killed the rc2 daemon via the runtime
  restart; (4) full-suite green is blocked by the P1 phase's
  in-flight web_ssrf_test.go failures (not this workstream).
2026-11-13T03:23:28Z [rc2b] pid 2971 gone after 4 samples
| 2026-11-13T03:24:09Z | 11:24:09 | rc2c | 37429 | 05:17 | 8964 | 14532 | 1860520 | 9 | 6 | utime=3 stime=5 | 8024064 | 57344 |
| 2026-11-13T03:27:07Z | 11:27:07 | rc3 | 45268 | 02:23 | 14180 | 14180 | 1860484 | 9 | 6 | utime=2 stime=6 | 5484544 | 73728 |
2026-11-13T03:29:09Z [rc2c] pid 37429 gone after 2 samples
| 2026-11-13T03:32:07Z | 11:32:07 | rc3 | 45268 | 07:23 | 9384 | 13988 | 1860500 | 9 | 6 | utime=3 stime=7 | 13291520 | 73728 |
| 2026-11-13T03:37:07Z | 11:37:07 | rc3 | 45268 | 12:24 | 9152 | 13988 | 1860500 | 9 | 6 | utime=4 stime=10 | 29634560 | 73728 |
| 2026-11-13T03:42:07Z | 11:42:07 | rc3 | 45268 | 17:24 | 10644 | 13988 | 1860500 | 9 | 6 | utime=5 stime=16 | 48123904 | 73728 |
| 2026-11-13T03:47:07Z | 11:47:07 | rc3 | 45268 | 22:24 | 9696 | 13988 | 1860508 | 9 | 6 | utime=5 stime=21 | 74223616 | 73728 |
| 2026-11-13T03:52:07Z | 11:52:07 | rc3 | 45268 | 27:24 | 10560 | 13988 | 1934240 | 10 | 6 | utime=6 stime=26 | 92262400 | 73728 |
