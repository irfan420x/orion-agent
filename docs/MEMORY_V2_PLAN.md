# Memory v2 plan — Claude Code-inspired (patterns only)

Status: **implemented 2026-11-13 (v0.2.0-rc4)** — all 7 steps landed
and verified (809 tests passing, race clean). One design correction
during implementation: feedback memories are standing guidance, so
they bypass the topical selector and the surfaced exclusion and are
injected every turn (max 2, own section header) — the topical
selector alone did not surface them live. See CHANGELOG.md. Source
of the ideas: `~/workspace/claude-code-reference` (`src/memdir/`,
`src/services/SessionMemory/`,
`src/services/compact/sessionMemoryCompact.ts`) — see
`CLAUDE_CODE_REFERENCE.md`. Nothing is copied verbatim; the design is
re-implemented in Go for Orion.

## What is good about Claude Code's memory (verified by reading it)

1. **Typed memories, not a flat fact list.** Four types, each with a
   scope and a body structure:
   - `user` — who the user is, role/goals/knowledge (always private).
   - `feedback` — guidance from the user on how to work: corrections
     AND confirmations ("yes exactly" counts — only saving corrections
     makes the agent drift overcautious). Rule first, then **Why:**
     and **How to apply:** lines.
   - `project` — ongoing work/goals/bugs/incidents; relative dates
     converted to absolute dates on save ("Thursday" → 2026-03-05).
   - `reference` — pointers to external systems (Linear project,
     Grafana board, Slack channel).
   Memories are markdown files with frontmatter (`name`,
   `description`, `type`); the description is what recall matches on.
2. **What NOT to save.** Code patterns, architecture, git history,
   file structure are derivable — never saved as memories. Even an
   explicit user request to save something goes through the gate:
   ask what was *surprising or non-obvious* about it; that part is
   what is kept.
3. **Recall = manifest + small model selection.** Scan headers
   (filename + description, max 200 files, newest first), then a
   small side-query picks up to 5 clearly-relevant ones. Excludes
   already-surfaced memories; aware of recently-used tools (don't
   re-surface docs for tools already in use — but DO surface
   warnings/gotchas about them). Recall failure returns nothing and
   never breaks the turn.
4. **Drift / verification before acting.** "Before recommending from
   memory": a memory naming a file/function/flag is a claim about
   when it was written — verify the file exists / grep for it before
   recommending. If memory conflicts with current state, trust the
   current state and update or remove the stale memory. (Their evals:
   0/2 → 3/3 after adding this.)
5. **Session memory.** A background subagent periodically extracts
   key info from the conversation into a markdown session-memory
   file, without interrupting the main flow (thresholds on tool
   calls / turns). Compaction reads it, so the important bits
   survive context compression.
6. **Team vs private scopes** (team memories sync; private stay
   local) and **memory age** (staleness visible; old memories
   decay). Team scope matters less for Orion (single user,
   Telegram-only) but the distinction is kept for later.

## Orion today (gap)

- `internal/memory`: one JSON file store (`facts` + `messages`).
- `internal/core/recall.go`: token-overlap scoring, top-3 facts
  injected. No types, no Why/How structure, no drift check, no
  background session extraction, no age.
- `/compress` saves a summary as a flat fact; `/memory` reviews
  facts and forgets by number (Hermes-style, fine as a shell).

## Implementation plan (next phase after Audit 3)

1. **Typed memory files.** `~/.orion/memories/*.md` with frontmatter
   (`name`, `description`, `type: user|feedback|project|reference`).
   Migrate existing JSON facts into typed files (default `project`
   with a note). Keep the JSON store as the message log only.
2. **Save rules in the system prompt.** The four types, when-to-save,
   the what-not-to-save list, and the explicit-save gate — adapted
   to Orion (single user; team scope reserved, not built).
3. **Recall v2.** Header manifest + small provider side-query
   (tiny `max_tokens`, JSON list, up to 5) selecting memories;
   fall back to the current token-overlap scorer when the side
   query fails or is disabled. Exclude already-surfaced; skip
   usage-docs for tools in active use but keep warnings.
4. **Drift verification.** Before the agent acts on a recalled fact
   naming a file/function, verify it (file exists / grep). On
   conflict, prefer current state and mark the fact stale.
5. **Session memory.** Background extraction after N tool calls /
   turns into `session-memory.md`; `/compress` feeds from it so
   compaction preserves decisions, not just a summary.
6. **Memory age + UX.** Ages on `/memory` listing, `forget` by
   name/number, memory usage in the Telegram footer.
7. **Tests + sweep.** Unit tests per piece (frontmatter parse,
   manifest scan, selector fallback, drift check, session
   extraction thresholds), regression sweep (gofmt/build/vet/
   test/race), docs + CHANGELOG. Live check: a multi-turn
   Telegram conversation where a saved feedback memory changes
   the agent's behaviour on the next turn.

## Deliberately NOT borrowed

- Team-memory sync (no team; single user).
- The model-based selector as the *only* recall path — Orion keeps
  the cheap keyword scorer as fallback (local/Ollama must work
  offline; a side-query costs a provider call).
