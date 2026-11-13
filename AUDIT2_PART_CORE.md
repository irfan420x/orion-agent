# Audit 2 — Part: core / model / memory / config (fresh-eyes review)

- **Auditor scope:** `internal/core/` (agent.go, recall.go + tests), `internal/model/`
  (provider.go, openai.go, anthropic.go, ollama.go, types.go, stream_test.go),
  `internal/memory/` (file_store.go, memory.go), `internal/config/` (config.go).
- **Date:** 2026-11-13. **Tree at audit start:** 66 Go files, 14/14 packages green.
- **Method:** every in-scope file read line by line; old-audit fixes touching
  these files spot-checked against the current tree (see §5).
- **Severity key:** P0 = data loss / security hole / crash; P1 = wrong behaviour
  reachable in normal use, or a latent failure that breaks later runs;
  P2 = edge case, inconsistency, or hardening note.
- **Status column:** FIXED = fixed in this pass with a test; OPEN = reported,
  not changed (rationale given).

## 1. P0 — none found

No P0 in scope. The token-based denylist, the atomic memory persist, and the
provider retry/cancellation paths all held up on re-read.

## 2. P1 — found and fixed (5)

### P1-1 — Deferred tool calls silently lost in the agent loop — FIXED
- **File:** `internal/core/agent.go:353,362` (old code), helpers added at `:394-421`
- **Evidence:** the loop kept `deferred` for multi-call turns, but two paths
  dropped pending calls without any reminder:
  1. A later multi-call turn **overwrote** the list
     (`deferred = append([]model.ToolCall(nil), toolCalls[1:]...)`), so calls
     deferred on turn 1 vanished when turn 2 also requested several calls.
  2. A single-call turn ran `deferred = nil`, so a pending call the model
     simply had not re-planned yet was discarded.
  In both cases the model never re-planned the call, no reminder fired, and
  the only trace was a log line. The user-visible effect: the agent quietly
  does only part of a multi-step request.
- **Fix:** `appendDeferred` appends (deduped by name+arguments) instead of
  overwriting; `removeDeferred` removes a pending call only when the model
  actually executes it (matched by name+arguments); a single-call turn no
  longer clears the list. The reminder still fires once when the model
  answers directly, and clears the list then.
- **Tests:** `TestDeferredToolCallsSurviveLaterMultiCallTurn`,
  `TestDeferredToolCallsSurviveSingleCallTurn` (both assert the reminder
  names the pending calls and the deferred call is never executed early).
  Existing `TestRunExecutesOnlyFirstToolCallPerTurn` /
  `TestRunRemindsModelOfDeferredToolCalls` still pass unchanged.

### P1-2 — Dangling tool-call history poisons later runs (OpenAI/Anthropic 400) — FIXED
- **File:** `internal/core/agent.go:230` (call site), `:443` (`repairHistory`)
- **Evidence:** a run that ends after `AddMessage(assistant tool-call)` but
  before `AddMessage(tool result)` — cancelled in that window, killed between
  the two persists, or a failed tool-result persist — leaves an assistant
  message with `tool_calls` and no following tool message in memory. The next
  run sends it verbatim. OpenAI rejects an assistant message with
  `tool_calls` that no tool message follows (400), and Anthropic rejects a
  `tool_use` block with no `tool_result` (400). The failure repeats on **every
  run until the entry ages out of the 10-message window**. The symmetric
  fault exists too: a tool result whose assistant message aged out (or was
  never stored) is an orphan no provider accepts on its own.
- **Fix:** `repairHistory` sanitises the recent window before it is sent:
  an assistant message's calls are stripped when no tool result immediately
  follows them (message dropped if stripping leaves it empty; kept as plain
  text otherwise); tool results whose ID no in-window assistant message
  requested — or with no assistant message before them — are dropped.
  Satisfaction is **positional**, not a global ID set, because IDs are not
  globally unique (Ollama synthesises `call-1, call-2, ...` per turn). The
  raw window is still what recall uses, so the `older`-window arithmetic in
  `recallFor` is unaffected. Well-formed history passes through unchanged.
- **Tests:** `TestRunRepairsDanglingToolCallHistory`,
  `TestRunRepairsOrphanToolResult`, `TestRunKeepsWellFormedToolHistory`,
  `TestRepairHistoryIsPositional`, `TestRepairHistoryKeepsTextOfStrippedCall`.

### P1-3 — Anthropic: deferred-reminder user message creates consecutive user turns (400) — FIXED
- **File:** `internal/model/anthropic.go:120-152`, helpers `:158-196`
- **Evidence:** `toAnthropicMessages` merged consecutive *tool* messages but
  nothing else. The agent loop appends the deferred-call reminder as a plain
  user message immediately after the tool-result user turn, so the converted
  conversation is `[..., assistant(tool_use), user(tool_result),
  user(reminder)]` — consecutive user turns, which Anthropic rejects with a
  400. Reachable on any Anthropic run where the model issues a multi-call
  turn and then answers directly. (This is a new gap adjacent to old-audit
  P0-4, which only covered the tool-message case.)
- **Fix:** `appendAnthropicMessage` merges any two consecutive same-role
  turns (`mergeAnthropicContent` handles string+string, string+blocks,
  blocks+string, blocks+blocks); a tool_result turn followed by the reminder
  becomes one user turn with a `tool_result` block and a text block. Same
  treatment for consecutive assistant turns.
- **Tests:** `TestToAnthropicMessagesMergesReminderAfterToolResult`,
  `TestToAnthropicMessagesMergesConsecutiveUserText`. Two pre-existing tests
  (`TestAnthropicUnknownRoleMapsToUser`,
  `TestAnthropicEmptyRoleMapsToUser`) asserted the old broken expectation
  (two user turns sent); updated to expect the merged single user turn —
  the unknown/empty-role → user mapping is unchanged.

### P1-4 — Recall: instruction-phrase substring matching drops real fact sentences — FIXED
- **File:** `internal/core/recall.go:81` (pattern), `:257` (`declarativeSentences`)
- **Evidence:** phrases were matched with `strings.Contains`, so `"write"`
  matched `"writer"` and `"compute"` matched `"computer"`:
  "My favourite writer is Asimov." and "My computer runs Linux." were both
  dropped from message recall, so those facts could never surface — a false
  negative in the recall scoring the audit was asked to hunt for. `"use the"`
  had the same class of bug (`"abuse the"` contains it).
- **Fix:** single compiled regexp with `\b` word boundaries for all phrases:
  `\b(?:use the|call the|you must|write|read the file|compute|tell me|do not)\b`.
  "Write a poem …" / "Compute my favourite number …" are still dropped.
- **Tests:** `TestDeclarativeSentencesWordBoundaries`,
  `TestRecallItemsRecallsWriterSentence`.

### P1-5 — `Config.Save` is not atomic and never fixes lax permissions — FIXED
- **File:** `internal/config/config.go:450` (`Save`), `:520` (`syncConfigDir`)
- **Evidence:** `Save` used a plain `os.WriteFile`. Two gaps vs the memory
  store, which does temp+fsync+rename+dir-fsync:
  1. A crash mid-write leaves a torn `config.json`; `Load` then fails to
     parse and Orion will not start.
  2. `WriteFile` never changes the mode of a pre-existing file, so a
     `config.json` created with `0644` stays world-readable — leaking the
     API keys and Telegram token it holds.
- **Fix:** atomic write mirroring `memory.FileStore.saveLocked`: temp file in
  the same directory, `Chmod 0600`, write, fsync, close, rename, directory
  fsync. The rename replaces any lax-permission file with a `0600` one.
- **Tests:** `TestSaveEnforces0600OverLaxFile` (pre-existing `0644` file →
  `0600`, content still loads), `TestSaveCreates0600File`.

## 3. P2 — found and fixed (4)

### P2-1 — Ollama Stream repeats tool calls across chunks — FIXED
- **File:** `internal/model/ollama.go:208` (`Stream`, `:274`)
- **Evidence:** tool calls were appended from every NDJSON line. Ollama may
  repeat the full tool-call list in the final done chunk, so the agent saw
  the same call twice: it executes the first, defers the phantom duplicate,
  and later reminds the model to perform a call it already executed.
- **Fix:** identical `(name, arguments)` calls already recorded are skipped.
- **Test:** `TestOllamaStreamDedupesRepeatedToolCalls`.

### P2-2 — Ollama Stream: one malformed NDJSON line fails the whole stream — FIXED
- **File:** `internal/model/ollama.go:249`
- **Evidence:** a JSON-decode error on a single line returned an error
  immediately, discarding already-streamed content — unlike OpenAI, which
  skips bad SSE chunks.
- **Fix:** skip malformed lines; a stream of nothing but garbage still falls
  back to `Chat` via the existing empty-stream path.
- **Test:** `TestOllamaStreamSkipsMalformedLine`.

### P2-3 — OpenAI Stream: index-gap padding leaks nameless tool calls — FIXED
- **File:** `internal/model/openai.go:258` (`Stream`), `:327` (filter),
  `:368` (`mergeStreamToolCalls`)
- **Evidence:** `mergeStreamToolCalls` pads the list when a delta index
  jumps (first delta at index 1 → `[empty, real]`). The padded entry has no
  name and was returned to the agent, which would try to execute a nameless
  tool. The filter runs before the empty-stream fallback check, so a stream
  of only padding still falls back to `Chat`.
- **Test:** `TestOpenAIStreamDropsPaddedEmptyToolCalls`.

### P2-4 — Anthropic Stream: stale SSE event + system-only request — FIXED
- **File:** `internal/model/anthropic.go:261` (empty-messages guard),
  `:400` (blank-line event reset)
- **Evidence:** (a) a request containing only a system prompt produced an
  empty `messages` array, which Anthropic rejects with a 400 — now a
  client-side error before any HTTP call. (b) the SSE loop never reset
  `event` on blank lines, so a data-only line after a blank line was
  misrouted to the previous event type.
- **Tests:** `TestAnthropicChatRejectsSystemOnlyRequest`,
  `TestAnthropicStreamResetsEventOnBlankLine`.

## 4. P2 — reported, not fixed

- **P2-5 — Fact recall, single generic token (recall.go:123).** Facts inject
  on one shared token by design, so a generic token can drag in an unrelated
  fact: "What is my password?" matches fact `wifi password: hunter2` on
  `"password"`; "What is new?" matches `home city: New York` on `"new"`.
  Deliberate threshold, documented in the file header; changing it to 2
  would break the "What is 17?" case. Left as a design tradeoff — a
  generic-token stoplist would be arbitrary. OPEN.
- **P2-6 — `alreadyVisible` is per-message (recall.go).** A fact whose tokens
  are split across two recent messages is not considered visible and can be
  re-injected even though the model can see all the tokens. Minor
  duplication only. OPEN.
- **P2-7 — Sentence splitter splits abbreviations (recall.go).** The regex
  `[^.!?\n]+[.!?]?` splits "Dr. Smith is here." into "Dr." + "Smith is here."
  The fragment is usually too short to score, so impact is minimal. OPEN.
- **P2-8 — `truncateRecallItem` cuts mid-word, no ellipsis (recall.go).**
  Cosmetic. OPEN.
- **P2-9 — OpenAI Stream 1 MB line cap (openai.go).** `scanner.Buffer(...,
  1024*1024)` fails the stream on a single SSE line over 1 MB (possible with
  very large tool-call arguments). Same cap exists in the Anthropic and
  Ollama streamers. OPEN.
- **P2-10 — OpenAI sends a negative `MaxTokens` (openai.go).** `buildRequest`
  sends `MaxTokens` whenever it is non-zero; a negative value reaches the
  API and 400s. Anthropic clamps `<= 0` to 1024; Ollama ignores `MaxTokens`
  entirely (no `num_predict` option). OPEN.
- **P2-11 — Anthropic temperature range (config.go / anthropic.go).**
  Config allows temperature 0–2, but Anthropic rejects > 1 with a 400.
  Per-provider validation or clamping would be needed. OPEN.
- **P2-12 — `Load` rejects an empty config file (config.go).** An empty file
  is a JSON parse error; `memory` treats an empty file as a fresh store.
  Inconsistent, low impact. OPEN.
- **P2-13 — `ORION_SANDBOX` unknown values silently ignored (config.go).**
  `applyEnv` drops unrecognised values without a word. OPEN.
- **P2-14 — `recallFor` older-window trim when the user message failed to
  store (core/agent.go, recall.go:187).** If `AddMessage(user)` fails, the
  "current input is last" check can trim a *previous* identical user message
  from `older`. Narrow, and recall only mines user messages, so the effect
  is at most one extra recall candidate. OPEN.
- **P2-15 — Empty final answer from `Chat` (model).** A provider returning
  choices with empty content and no tool calls makes the agent return `""`
  with a nil error (the streaming path has the empty-reply fallback; `Chat`
  does not). Rare; changing it risks breaking legitimately empty answers.
  OPEN.
- **P2-16 — No size cap on tool results / history (core, memory).**
  `MaxMessages` caps count, not bytes; a huge tool result is persisted and
  fed back to the model whole. Durability-first design; a cap belongs with
  the tools package. OPEN.
- **P2-17 — Anthropic Stream fallback check (anthropic.go).** On a scanner
  error it falls back only when `len(blocks) == 0`, while OpenAI falls back
  when no usable content/tool call arrived — a started-but-empty tool_use
  block returns an error instead of falling back. Minor inconsistency.
  OPEN.

## 5. Old-audit spot-checks (fixes touching this scope)

All still hold, except where a new gap was found next to one:

| Old item | Verdict |
|---|---|
| OpenAI object-form tool-call arguments (`openai.go` UnmarshalJSON) | HOLDS — string, object, null all normalise. |
| P0-4 Anthropic multi-tool turns (merge consecutive tool messages) | HOLDS for tool messages; **new gap P1-3** above for consecutive *user* turns (reminder). |
| P0-5 Ollama tool IDs (`call-1…`, plus agent central synthesis) | HOLDS in `Chat` and `Stream`. |
| P1-1/P1-2 RunStream double call + streaming drops tool calls | HOLDS — one `Stream` call per step, tool calls parsed in all three streamers. |
| P1-3 Streaming empty reply fallback | HOLDS in all three providers. |
| P1-8 Config partial-provider merge | HOLDS (`mergeProviderDefaults`). |
| P1-15 `NewFileStore` chmods only dirs it created | HOLDS. |
| Memory durability (temp+fsync+rename+dir fsync, 0600/0700, corrupt file is an error, `Close` stays open on failed save) | HOLDS — no findings in `internal/memory/` this pass. |

## 6. Verification

- `gofmt -l` (scope): clean. `go build ./...`: pass. `go vet` (scope): clean.
- `go test ./internal/core/ ./internal/model/ ./internal/memory/ ./internal/config/ -count=1`:
  **all pass, 155 tests total** (was 137 before this pass; +18 new tests, 2
  pre-existing tests updated — see P1-3).
- `go test -race` (scope): all pass, no races.
- Full `go test ./...`: every package passes **except `internal/security`**, which
  fails on `audit2_probe_test.go` — a probe file belonging to the parallel
  security-scope auditor (denylist-bypass probes), outside this scope; not
  touched here. One filtered `go test -run` invocation during this pass
  transiently reported a build failure while other audit-2 workers were
  editing the tree concurrently; the immediate re-run passed.
- No live destructive command was run in any test. No secret appears in this
  report, in any test, or in any fix.

## 7. New tests added (18)

- `internal/core/agent_audit2_test.go` (7): `TestDeferredToolCallsSurviveLaterMultiCallTurn`,
  `TestDeferredToolCallsSurviveSingleCallTurn`, `TestRunRepairsDanglingToolCallHistory`,
  `TestRunRepairsOrphanToolResult`, `TestRunKeepsWellFormedToolHistory`,
  `TestRepairHistoryIsPositional`, `TestRepairHistoryKeepsTextOfStrippedCall`.
- `internal/core/recall_audit2_test.go` (2): `TestDeclarativeSentencesWordBoundaries`,
  `TestRecallItemsRecallsWriterSentence`.
- `internal/model/anthropic_audit2_test.go` (4): `TestToAnthropicMessagesMergesReminderAfterToolResult`,
  `TestToAnthropicMessagesMergesConsecutiveUserText`,
  `TestAnthropicChatRejectsSystemOnlyRequest`,
  `TestAnthropicStreamResetsEventOnBlankLine`.
- `internal/model/openai_audit2_test.go` (1): `TestOpenAIStreamDropsPaddedEmptyToolCalls`.
- `internal/model/ollama_audit2_test.go` (2): `TestOllamaStreamDedupesRepeatedToolCalls`,
  `TestOllamaStreamSkipsMalformedLine`.
- `internal/config/config_audit2_test.go` (2): `TestSaveEnforces0600OverLaxFile`,
  `TestSaveCreates0600File`.

## 8. Still open in scope

- The 13 OPEN P2 items in §4 (all minor/edge-case; none blocks use).
- Telegram round-trip test still needs Irfan to message `@Magi420x_bot` once
  (`/start`) — noted in MEMORY.md 07:26 SGT, outside this audit's scope.
- `internal/security` failure from the parallel auditor's probe file is not
  mine to fix; the parent coordinator should confirm with that worker
  whether its denylist probes are expected-fail probes or real findings.
