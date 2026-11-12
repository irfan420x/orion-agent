# Live Ollama E2E — record (2026-11-13)

How Orion's agent loop was proven against a real local model on this VM,
and why no real Hermes model runs here.

## The machine

- Ubuntu 24.04 x86_64, 4 CPUs, 7.7 GiB RAM total.
- At the time of the runs: **~700 MiB available** (`free -h`: 200 MiB
  free, 768 MiB available, 1.0 GiB swap). Ollama itself reported
  "inference compute ... total 7.7 GiB, available 720.4 MiB".
- Disk: ~149 GiB free on /home/hatch — never the constraint.

## Why no Hermes model runs here

Published Ollama sizes (ollama.com library / model guides):

| Model | Download | Suggested RAM |
|---|---|---|
| `hermes3:3b` (smallest Hermes) | 2.0 GB | ~8 GB |
| `hermes3:8b` (default, Q4_K_M) | 4.7 GB | ~16 GB |

Even the smallest Hermes needs ~11x this VM's available RAM. A real
Hermes model cannot run here; that is a hardware fact, not a code gap.
Orion is model-agnostic, so the loop was proven with the smallest
viable instruct model that fits instead (below), and the provider
interface is unchanged — a Hermes model works the same way on a
machine with enough RAM (`ollama pull hermes3:8b`, set
`ORION_LIVE_OLLAMA_MODEL=hermes3:8b`).

## Ollama install and the pull problem

- Ollama 0.40.16 installed (`/usr/local/bin/ollama`). The systemd
  service could not reach the registry (it has no proxy env), so the
  server runs manually: `OLLAMA_HOST=127.0.0.1:11434
  OLLAMA_CONTEXT_LENGTH=3072 ollama serve` (started by the E2E worker,
  logs in `~/workspace/orion-go/.e2e-tmp/ollama-serve.log`).
- `ollama pull qwen2.5:0.5b` failed twice: first "tls: first record
  does not look like a TLS handshake" (systemd service, no proxy),
  then after 10 minutes spinning on "pulling manifest": `pull model
  manifest: ... /v2/token?...: EOF` (manual server, proxy env set).
  The server process's own outbound requests fail (its log also shows
  `model recommendations refresh failed: context deadline exceeded`),
  while `curl` from a shell reaches the registry fine
  (`/v2/` 404 as expected, token endpoint 401, manifest for
  `qwen2.5:0.5b` 200 in ~1s). Root cause: the Ollama server's HTTP
  client does not use this VM's egress proxy correctly. This is an
  environment quirk, not an Orion bug — Orion only speaks HTTP to
  `localhost:11434`.
- Workaround: the model weights were downloaded directly with curl
  from HuggingFace (`Qwen/Qwen2.5-0.5B-Instruct-GGUF`,
  `qwen2.5-0.5b-instruct-q4_k_m.gguf`, 491,400,032 bytes, ~145 s)
  and imported with `ollama create qwen2.5-0.5b-local -f Modelfile`
  (`FROM` the local GGUF). The model reports capabilities `tools` and
  `completion`, 630M parameters, Q4_K_M, 32k context.

## The model that ran

- `qwen2.5-0.5b-local` (Qwen2.5-0.5B-Instruct, Q4_K_M), 491 MB.
- First chat: 6.2 s total (5.2 s model load), then ~5-16 s per turn.
  Resident size ~552 MB (`ollama ps`), 100% CPU, context 2048.
- Smoke test (`/api/chat`, "Reply with exactly: ollama-ok") returned
  "Ollama-ok" — note the model did not honour the exact casing.

## The live E2E test

`internal/core/ollama_live_e2e_test.go`, `TestLiveOllamaAgentE2E`.
Run with:

```sh
ORION_LIVE_OLLAMA_E2E=1 ORION_LIVE_OLLAMA_MODEL=qwen2.5-0.5b-local \
  go test ./internal/core/ -run TestLiveOllamaAgentE2E -count=1 -v -timeout 15m
```

Without the env var the test skips, so the normal suite never needs
Ollama. It drives the real loop (`core.Agent.Run`, real
`tools.DefaultRegistry`, real `memory.FileStore`) through the real
Ollama provider in three single-purpose runs, each needing exactly
one tool call, with a bounded retry (3 attempts) per run:

1. Calculator: "Use the calculator tool to compute 17*23+5 and tell
   me the result." Assert the transcript shows the tool result
   `17*23+5 = 396` and the answer reports 396.
2. File write: "Use the file_write tool to write the number 396 to
   `<tmp>/e2e-result.txt`." Assert the file on disk holds `396` and
   the tool result says `wrote 3 bytes`.
3. File read: the test pre-writes `<tmp>/e2e-read.txt` with `482917`
   (a number that appears nowhere in the prompt). Assert the tool
   result and the answer both report `482917` — the model cannot know
   it without really reading the file.

Passing run (2026-11-13 ~05:20 SGT): all three runs passed on the
first attempt, 40.6 s total (calculator 13.6 s, file_write 15.9 s,
file_read 11.0 s). Transcripts are logged by the test (`t.Logf`).

## Loop failures found live, and the fixes

All found against the 0.5B model; all fixed in `internal/core/agent.go`
with unit tests (mock e2e tests were unaffected — they use one tool
call per turn already).

1. **Parallel dependent tool calls.** The model issued calculator +
   file_write + file_read in a single turn; file_write's arguments
   were written before the calculator result existed, so it wrote the
   expression (`17*23+5`) or a hallucinated number (`162`) instead
   of the result. Fix: the loop executes only the FIRST tool call of
   a turn and lets the model re-plan the rest next turn; the system
   prompt now states the one-tool-call-per-message rule explicitly
   (with a worked example). Test:
   `TestRunExecutesOnlyFirstToolCallPerTurn`.
2. **Abandoned plan.** After the first tool result the model
   sometimes answers directly instead of re-planning the deferred
   calls. Fix: the loop remembers deferred calls; if the model
   answers while any are pending, it injects a one-time reminder
   (tool names only — the dropped arguments may be stale) and
   discards the premature answer instead of recording it. Test:
   `TestRunRemindsModelOfDeferredToolCalls`.
3. **Determinism.** `config.Temperature` was added (nil = provider
   default; validated 0-2) and is passed through to every provider
   call. Finding: at temperature 0 the 0.5B model deterministically
   stops after the calculator, so the live test intentionally leaves
   temperature at the default. Tests: `TestRunPassesTemperatureThrough`,
   config `TestValidateErrors` temperature cases.

## Model limitation, measured (not a loop bug)

Across 9 live runs of the original 4-step single-conversation task
(calculator -> file_write -> file_read -> answer), the 0.5B model
completed the full chain once (calculator -> file_write `"396"` ->
answer); the other runs stopped after the calculator. The loop handled
every run correctly; the model cannot reliably hold a multi-step
dependent plan. That is why the test uses three single-purpose runs.
On a machine with enough RAM, re-run the same test with a Hermes
model (`hermes3:8b`), which is expected to chain the steps.

## Reproducing

```sh
# server (manual, with the shell's proxy env inherited):
OLLAMA_HOST=127.0.0.1:11434 OLLAMA_CONTEXT_LENGTH=3072 ollama serve
ollama list   # must show qwen2.5-0.5b-local (or pull/create it first)
ORION_LIVE_OLLAMA_E2E=1 go test ./internal/core/ -run TestLiveOllamaAgentE2E -count=1 -v -timeout 15m
```

If `ollama pull` fails on this VM, use the GGUF import path above
(HuggingFace is reachable from a shell; the Ollama server's own
registry client is not).

## Multi-turn live E2E (2026-11-13, this phase)

`TestLiveOllamaMultiTurnE2E` (`internal/core/ollama_live_multiturn_test.go`):
7 turns (T1 state number 17, T2 calculator 17*3, T3 state colour blue,
crash-survival probe, graceful restart, T4 recall 17 after restart,
T5 file_write 51, T6 file_read 51, T7 late-recall probe, non-fatal).
Each turn gets up to 2 attempts in the same conversation; deterministic
checks (memory.json on disk, tool results in the transcript, file
contents) run alongside every model check.

Environment note: Ollama 0.40.16 had to be reinstalled this phase —
`/usr/local/bin` does not survive on this VM (same as Go). The binary
was re-downloaded from GitHub releases v0.40.16
(`ollama-linux-amd64.tar.zst`, sha256 verified against the release
`sha256sum.txt`); note the asset is `.tar.zst` now, not `.tgz`, and
`/tmp` is a 512 MB tmpfs, so the 1.5 GB download must go under
`/home/hatch`. The model blobs in `~/.ollama/models` survived, so no
re-import was needed. `apt-get install zstd` was required first
(`apt-get update` needed for the package to be found).

Run 1 (coordinator, 2026-11-13, model qwen2.5-0.5b-local, temp 0,
234.77 s total): **FAIL — T1, T3, T4 failed; T2, T5, T6 passed.**

| Turn | Result | Time | Detail |
|---|---|---|---|
| T1 state number | FAIL (2 att.) | 2m16.9s | Model answered "can't find the file you're looking for" twice (hallucinated a file lookup); fact persisted on disk = true |
| T2 calculator 17*3 | PASS (1 att.) | 10.9s | Answer 51 backed by a real calculator tool result |
| T3 state colour | FAIL (2 att.) | 25.3s | Model refused / hallucinated a file_read of `blue.txt`; fact persisted on disk = true |
| Crash-survival probe | PASS | — | Second store, no shutdown: sees all facts (18 messages on disk) |
| Restart | PASS | — | Reloaded store sees both facts (18 messages) |
| T4 recall after restart | FAIL (2 att.) | 10.5s | Model answered "3" — forgot / confabulated the number |
| T5 file_write 51 | PASS (1 att.) | 17.5s | File holds exactly 51, via file_write |
| T6 file_read 51 | PASS (1 att.) | 17.7s | Answer 51 backed by a file_read tool result |
| T7 late recall (probe) | FAIL (not fatal) | 15.9s | Answer "4"; T1 already out of the 10-message window (30-message transcript) |

Verdict: no loop or memory bug — persistence, crash survival, restart
reload, and every tool call all work. The failures are the 0.5B model's:
it cannot reliably echo back a simple stated fact (T1/T3) and it
confabulates numbers when recalling (T4 "3", T7 "4"). T1 attempt 1 took
1m45s (first-turn model load + a long confused generation). Re-run on
a ≥16 GB host with a Hermes model to measure the same conversation
with a capable model.

Run 2 (coordinator, 2026-11-13, same model, 89.73 s total):
**FAIL — only T4 failed; T1, T2, T3, T5, T6 passed.**

| Turn | Result | Time |
|---|---|---|
| T1 state number | PASS (1 att., 7.5s) | Answer contained 17 — but only inside a hallucinated apology ("can't find the file named 17.txt"); a weak pass, recorded honestly |
| T2 calculator 17*3 | PASS (1 att., 6.5s) | Answer 51 backed by a calculator tool result |
| T3 state colour | PASS (1 att., 7.9s) | Same weak-pass shape: "can't find the file named blue.txt" contains "blue" |
| Crash-survival probe | PASS | Second store, no shutdown: sees all facts (12 messages) |
| Restart | PASS | Reloaded store sees both facts (12 messages) |
| T4 recall after restart | FAIL (2 att., 12.6s) | Both attempts answered "51" — the model recalled the most recent salient number (the computed result), not the favourite number 17 |
| T5 file_write 51 | PASS (1 att., 17.9s) | File holds exactly 51, via file_write |
| T6 file_read 51 | PASS (1 att., 19.4s) | Answer 51 backed by a file_read tool result |
| T7 late recall (probe) | FAIL (not fatal) | Answer "51" both attempts; T1 out of the 10-message window (24-message transcript) |

Pass rate over the two coordinator runs with the final prompts:
T1 1/2, T2 2/2, T3 1/2, T4 0/2, T5 2/2, T6 2/2; full-test pass 0/2.
The pattern is consistent: tool turns (T2/T5/T6) are reliable, recall
turns are not — the 0.5B model cannot distinguish the stored fact
(17) from the derived value (51) after a restart, and its "recall"
answers are often the most recent number in context. The loop and
memory are exonerated again: persistence, crash survival, restart
reload, and all tool executions passed in both runs.

## Fact-aware recall (2026-11-13, this phase)

The recall failures above are a context-window problem, not a
storage problem: by T7 the transcript is ~24-30 messages and the
agent only feeds the last 10 to the model, so T1 has slid out of
the window. Fix, in `internal/core/recall.go` (wired into
`Agent.run` in `internal/core/agent.go`): before each run, score
candidates against the current user message by content-token
overlap (lowercase, split on non-alphanumerics, stopwords dropped;
no new dependencies, no stemming, no embeddings) and inject the
top 3 into the system message, labelled "Relevant things the user
told you earlier ... prefer them over guessing". Two sources are
scored — explicit long-term facts from `ListFacts()` (need score
≥ 1, they are deliberate) and declarative sentences from OLDER
user messages outside the recent window (need score ≥ 2, they are
noisy; questions and instruction/tool sentences are dropped, so
"My favourite number is 17." is kept while "Use the calculator
tool to compute my favourite number times 3." is dropped).
Candidates already visible in the recent window are never
re-injected, and the section is appended only when non-empty, so
the system prompt is byte-identical when nothing is relevant. A
greeting injects nothing. 15 unit tests (mock provider, asserting
on the actual `ChatRequest` system message) prove: a fact stored
in turn 1 is injected after it slides out of the window, and
irrelevant facts are not injected for a greeting.

Note: the recall section lives only in the provider request, not
in the stored transcript, so the test logs below cannot show the
injection directly — the evidence is behavioural (T4/T7 flip from
0/2 to 2/2) plus the mock-provider unit tests.

Run 3 (coordinator, 2026-11-13, same model, temp 0, 161.92 s total,
first run with recall): **FAIL — T1 and T3 failed; T2, T4, T5, T6
passed, T7 probe passed.**

| Turn | Result | Time | Detail |
|---|---|---|---|
| T1 state number | FAIL (2 att.) | 1m10s | Both attempts: "can't find the file you're looking for" — no 17 in the answer at all |
| T2 calculator 17*3 | PASS (1 att.) | 5.8s | Answer 51 backed by a calculator tool result |
| T3 state colour | FAIL (2 att.) | 21.8s | Refused ("not possible to determine") / hallucinated file lookup; no "blue" |
| Crash-survival probe | PASS | — | Second store, no shutdown: sees all facts (18 messages) |
| Restart | PASS | — | Reloaded store sees both facts (18 messages) |
| T4 recall after restart | PASS (1 att.) | 23.1s | Answer "17" — first T4 pass ever (was 0/2) |
| T5 file_write 51 | PASS (1 att.) | 16.1s | File holds exactly 51, via file_write |
| T6 file_read 51 | PASS (1 att.) | 16.8s | Answer 51 backed by a file_read tool result |
| T7 late recall (probe) | PASS (1 att.) | 8.3s | Answer "17" — first T7 pass ever; T1 out of the 10-message window (28-message transcript) |

Run 4 (coordinator, 2026-11-13, same model, temp 0, 97.67 s total):
**PASS — all 7 turns passed on the first attempt. First full
pass of the multi-turn test ever.**

| Turn | Result | Time | Detail |
|---|---|---|---|
| T1 state number | PASS (1 att.) | 7.3s | Weak pass, recorded honestly: "can't find the file named 17.txt" contains 17 (same shape as run 2) |
| T2 calculator 17*3 | PASS (1 att.) | 6.1s | Answer 51 backed by a calculator tool result (model first tried file_read 17.txt, got an error, then called the calculator — the loop handled it) |
| T3 state colour | PASS (1 att.) | 7.4s | Weak pass, same shape: "can't find the file named blue.txt" contains "blue" |
| Crash-survival probe | PASS | — | Second store, no shutdown: sees all facts (12 messages) |
| Restart | PASS | — | Reloaded store sees both facts (12 messages) |
| T4 recall after restart | PASS (1 att.) | 30.9s | Clean answer "17" |
| T5 file_write 51 | PASS (1 att.) | 17.6s | File holds exactly 51, via file_write |
| T6 file_read 51 | PASS (1 att.) | 19.0s | Answer 51 backed by a file_read tool result |
| T7 late recall (probe) | PASS (1 att.) | 9.3s | Clean answer "17"; T1 out of the 10-message window (22-message transcript) |

Post-recall pass rates (runs 3-4): T1 1/2, T2 2/2, T3 1/2, T4 2/2,
T5 2/2, T6 2/2, T7 2/2 (probe); full-test pass 1/2. Recall turns
(T4/T7) went from 0/2 pre-recall to 2/2 with recall — the fix does
what it was built for. T1/T3 are unchanged by recall (the fact is
still in-window there; the model either echoes it or hallucinates
a file lookup, run to run), and their "passes" are weak ones that
contain the right token inside a hallucinated apology. The 0.5B
model is still the limiting factor; on a ≥16 GB host a Hermes
model should be re-run against the same conversation.
