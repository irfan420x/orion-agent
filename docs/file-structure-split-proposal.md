# File-structure split proposal — third refresh

**Status:** proposal only — no split has been performed. This document
exists so the next builder can use it directly; earlier versions lived
only in audit reports.

**Refreshed:** 2026-11-14, against HEAD `c174f0d` (Audit 21 F1 batch,
after plan_mode). Baseline suite at refresh time: 1,501 tests total
(1,497 pass, 0 fail, 4 skip), 24/24 packages — re-verify with a full
run before starting any split; the numbers below are only valid on a
green baseline.

## Lineage

- **Audit 17 §6** — the original proposal: split the three giants
  (`skills.go`, `security.go`, `telegram.go`) into 6 + 8 + 7 files,
  move-only, with the safety rules in §6 below.
- **Audit 18 §6.3** — first version of the split tables.
- **Audit 19 §6.2 / §6.2.3** — first refresh: telegram.go table
  re-done for rc24 line numbers; skills/security tables verified
  zero-drift.
- **Audit 20 F-A20-14, F-A20-15** — second refresh required: the
  telegram table went stale again (non-uniform drift +82…+211),
  `tryAnswerQuestion` was deleted, 8 new types and a 602-line
  `question.go` were unallocated, and PollStream (880 lines) made
  a move-only split of `poll.go` mathematically impossible.
- **Audit 21 F-A21-6** — this refresh (third): `skills.go` grew
  again (see below), so the "zero drift, verbatim" claim no longer
  holds for any of the three giants.

## Fresh measurements (2026-11-14, `wc -l`)

| File | a633ed3 | 3a6a498 | Now | Δ since 3a6a498 | Cause of the drift |
|---|---|---|---|---|---|
| `internal/skills/skills.go` | 2,712 | 2,730 | **2,731** | +1 | plan_mode added `enter_plan_mode` / `exit_plan_mode` to `knownBuiltInTools` (one line) |
| `internal/security/security.go` | 2,736 | 2,736 | **2,736** | 0 | — |
| `internal/messaging/telegram.go` | 2,836 | 2,836 | **2,945** | +109 | Audit 20 Step-1 fixes only: approval-card notes (F-A20-11), orphaned-card notice (F-A20-9), callback-router loudness (F-A20-5/6/8) |
| **Giants total** | 8,284 | 8,302 | **8,412** | +110 | |
| `internal/messaging/question.go` | — | 602 | **616** | +14 | Step-1 fix hunks in `HandleQuestionCallback` + imports |

Other files over the 700-line watch line (unchanged in shape,
included so the next refresh does not have to rediscover them):
`internal/core/agent.go` 1,538, `internal/app/commands.go` 1,330,
`internal/app/chats.go` 1,279, `cmd/orion/main.go` 1,273,
`internal/tools/planning.go` 1,193, `internal/tools/web.go` 1,175,
`cmd/oriond/main.go` 1,052, `internal/tools/files.go` 875,
`internal/tools/search.go` 826. Per-RC test files: 64 files,
13,213 lines; Go files total 314, test files 230, test lines 65,600,
production lines 48,240.

## What is stale since the last refresh

1. **`skills.go` — "zero drift, verbatim" no longer holds.**
   `KnownBuiltInTools()` (+18 lines at F0, `skills.go:103–120`)
   sits inside the proposed `registry.go` (~445) allocation, and
   plan_mode added one more line to the same map. The
   `registry.go` estimate below is revised ~445 → ~465 to absorb
   it. The rest of the Audit 19 skills table still holds.
2. **`telegram.go` — the +109 does NOT all land in one allocation.**
   98 of the +109 net lines since 3a6a498 are in the approval area
   (`telegram.go:1303–1699`: `ApprovalCards.Note`,
   `ApprovalPrompter`, `HandleApprovalCallback`,
   `ParsePermissionCallback`) and belong in the proposed
   `approval.go`, revised ~280 → ~379. The other **11 lines are
   outside it**: `telegram.go:469` (+5, the `callbackRouterInstalled`
   bool field — gateway/router state, not approval logic) and
   `telegram.go:832–837` (+6 net, `SetCallbackHandler` now returns
   an error — F-A20-6 callback-router hardening), which belong in
   the proposed `gateway.go`, revised ~370 → ~381 (F-A22-11
   corrects the third refresh, which put all 109 in `approval.go`).
   Nothing else in telegram.go moved. Totals (2,945, +109) were
   already correct — only the allocation between the two files
   changes.
3. **`question.go` — now allocated.** It is its own file already
   (`internal/messaging/question.go`, 616 lines, under the 700
   target) holding the question registry, the reply interceptor
   and `HandleQuestionCallback`. No split needed; it just stays
   as-is and is no longer "unallocated". The stale reference to
   the deleted `tryAnswerQuestion` in its test comment was fixed
   in F1 (F-A21-23).
4. **`security.go` — the one table that still holds verbatim.**
   Zero drift since a633ed3 (2,736 lines); the Audit 19 table
   below applies unchanged.

## Refreshed split tables

### `skills.go` (2,731) → 6 files

| Proposed file | Estimate | Note |
|---|---|---|
| `registry.go` | ~465 | Was ~445; +19 for `KnownBuiltInTools()` + plan_mode names, which live here |
| `markers.go` | ~120 | Unchanged |
| `activation.go` | ~550 | Unchanged |
| `clause_words.go` | ~410 | Unchanged |
| `clause.go` | ~830 | Was ~580; +252 for the eight segment/negation helpers the Audit 19 table never allocated (`skills.go:2161–2412`: `segmentNamesSkillAsActivateObject`, `segmentNamesSkillForActivationNoun`, `isNegationWord`, `clauseNegatesVerb`, `clauseHasNegation`, `clauseGovernorIsDirectiveVerb`, `prevWordIs`, `isQuotedSkillReference`) — clause helpers, so they belong here, not in `parse.go` (F-A22-17). `clauseVerbAt` is still AST-exact 345 lines (`skills.go:1565–1909`); phase 2 must break it, phase 1 must not |
| `parse.go` | ~300 | Unchanged |
| *shared file header* | ~45 | NEW row: the file header counted once, not per file — package doc (`skills.go:1–26`) + imports (`skills.go:28–43`), 43 lines. The old table counted it nowhere; each split file gets its own header on top of this (F-A22-17) |

**Correction (Audit 22 F-A22-17):** the old table summed to
2,425 against `skills.go` 2,731 — 306 lines (~11%) short,
unacknowledged. The gap decomposes exactly, measured from the
file: 252 lines of never-allocated helpers (listed in the
`clause.go` row above) + 43 lines of never-counted file header
(the row above) + ~11 lines of rounding slack in the `~`
estimates (measured content 2,436 vs estimates 2,425). Revised
sum: 465+120+550+410+830+300 = 2,675, + ~45 header = 2,720
against 2,731 — the residual ~11 is rounding slack, now stated
instead of hidden. The other two giants' tables are unchanged:
`security.go` sums 2,765 vs 2,736 (+29 over) and `telegram.go`
sums 2,855 vs 2,945 (90 short) — estimate slack of that size is
plausible for `~` numbers, but 306 was not, which is why only
the skills table is corrected here.

### `security.go` (2,736) → 8 files — unchanged, Audit 19 table verbatim

| Proposed file | Estimate |
|---|---|
| `paths.go` | ~170 |
| `wrappers.go` | ~525 |
| `fileops.go` | ~305 |
| `vars.go` | ~430 |
| `segment.go` | ~180 |
| `interpreters.go` | ~325 |
| `payload.go` | ~420 |
| `sensitive.go` | ~410 |

### `telegram.go` (2,945) → 7 files

| Proposed file | Estimate | Drift note |
|---|---|---|
| `gateway.go` | ~381 | Was ~370; +11 since 3a6a498, router state not approval logic (F-A22-11): the `callbackRouterInstalled` gateway-struct field (+5, `telegram.go:469`) and `SetCallbackHandler` returning an error (+6 net, `telegram.go:832–837`, F-A20-6 hardening) |
| `options.go` | ~420 | Unchanged |
| `api.go` | ~350 | Unchanged |
| `approval.go` | **~379** | Was ~280; +98, all Step-1 approval work (see §"What is stale" item 2) — the other 11 lines of the +109 belong to `gateway.go` (F-A22-11, correcting the third refresh's ~390) |
| `send.go` | ~135 | Unchanged |
| `updates.go` | ~150 | Unchanged |
| `poll.go` | **~1,040** | Was ~960 at rc24 (Audit 20 measured the span 998 on a slightly different boundary); content unchanged since 3a6a498 — the +109 approval growth sits above it and only shifts line numbers. Measured span now: `advanceOffset` through end of file, `telegram.go:1905–2945` = 1,041 lines, of which `PollStream` is 880. Still the exception — see below |

Unassigned items: none. Every function, type and field in the
three giants is allocated above — for `skills.go` only after the
F-A22-17 correction (the eight helpers + shared-header row); the
`security.go` (+29 over) and `telegram.go` (90 short) tables still
carry estimate slack, stated in the skills-table correction above
rather than hidden — and `question.go` is allocated
as its own file.

## PollStream — the phase-2 split (logic change, not move-only)

`PollStream` is a **method inside `telegram.go`** (`telegram.go:2045–2924`,
AST-exact **880 lines**). There is **no `poll.go` file** — the
"poll.go" framing in earlier audits means the proposed file the
method would move into, not an existing file.

Move-only splitting cannot bring `poll.go` under the 700-line
target: even after everything else leaves the file, `PollStream`
alone exceeds it by 180 lines. So phase 1 (move-only) ends with
`poll.go` at ~1,040 lines as an accepted exception, and phase 2
breaks `PollStream` itself apart — a **logic change needing its
own plan and its own audit**, never mixed into the move-only
phase. First candidates, unchanged since Audit 19:
`handlePhoto` / `handleMedia` and the dispatch branches, lifted
into separate functions. The named closures inside `PollStream`
(`handlePhoto`, `handleMedia`, `handleCallback`, …) are the seam
to cut along. Do not start phase 2 until phase 1 is fully green
and audited.

Known phase-2-only items carried over: `clauseVerbAt` (345 lines,
phase 2); the interleaved `var` declarations between `isClauseVerb`
and `hasVerbSuffix` (split declaration-by-declaration, never by
cutting a contiguous range).

## Move-only safety rules (Audit 17 §6, still binding)

- Start only on a green baseline (full suite + vet + gofmt).
- One split = one commit, in order skills → security → telegram.
- Each commit: build + affected-package tests + vet green, and
  `git show --stat -M` must show a move.
- Any test failure: **revert, do not fix the test**.
- Final gate: full suite counts equal the baseline, vet clean,
  release-check passes, `go doc` diff empty.
- Root `oriond.service` carries a stale CAP_SETUID comment
  (F-A19-3): fix the comment if a unit file is touched, not as
  part of a split.

## What this proposal does not do

No file was split, moved or renamed for this refresh. The next
step is a builder picking up the tables above — not a further
refresh — unless the tree drifts again first, in which case
re-measure with `wc -l` and update the three tables before
starting.
