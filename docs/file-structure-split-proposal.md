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
2. **`telegram.go` — the +109 all lands in one allocation.**
   Every hunk since 3a6a498 is in the approval area
   (`telegram.go:1303–1699`: `ApprovalCards.Note`,
   `ApprovalPrompter`, `HandleApprovalCallback`,
   `ParsePermissionCallback`, one gateway-struct field). All of it
   belongs in the proposed `approval.go`, revised ~280 → ~390.
   Nothing else in telegram.go moved.
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
| `clause.go` | ~580 | Unchanged — `clauseVerbAt` is still AST-exact 345 lines (`skills.go:1565–1909`); phase 2 must break it, phase 1 must not |
| `parse.go` | ~300 | Unchanged |

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
| `gateway.go` | ~370 | Unchanged since Audit 19 |
| `options.go` | ~420 | Unchanged |
| `api.go` | ~350 | Unchanged |
| `approval.go` | **~390** | Was ~280; +109, all Step-1 approval work (see §"What is stale" item 2) |
| `send.go` | ~135 | Unchanged |
| `updates.go` | ~150 | Unchanged |
| `poll.go` | **~1,040** | Was ~960 at rc24 (Audit 20 measured the span 998 on a slightly different boundary); content unchanged since 3a6a498 — the +109 approval growth sits above it and only shifts line numbers. Measured span now: `advanceOffset` through end of file, `telegram.go:1905–2945` = 1,041 lines, of which `PollStream` is 880. Still the exception — see below |

Unassigned items: none. Every function, type and field in the
three giants is allocated above, and `question.go` is allocated
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
