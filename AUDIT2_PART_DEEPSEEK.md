# AUDIT2 — Part: DeepSeek provider

Scope: ONLY the new DeepSeek provider, which landed after the main
fresh-eyes audit started. Auditor: DeepSeek-scope subagent, 2026-11-13.

Files in scope:
- `internal/model/provider.go` — new `"deepseek"` case in `NewProvider`
- `internal/model/openai.go` — provider `name` field, `Name()` / error labels
- `internal/config/config.go` — `knownProviders`, `providerNeedsAPIKey`, default entry
- `internal/model/provider_test.go`, `internal/config/config_test.go`
- `internal/model/deepseek_live_test.go` (live-tagged)

Out of scope: everything else in the tree (covered by the parallel audit).

## Verdict

No P0s. Two P1s found, both fixed with tests. Two P2s found, one fixed,
one flagged without a code change (see P2-2 — a naive fix would be
harmful). Repo contains no DeepSeek API key. Defaults, config validation,
and default-provider wiring all check out.

## P1

### P1-1 — DeepSeek errors were labelled "openai:" (9 sites) — FIXED

`internal/model/openai.go` — the shared OpenAI-compatible client gained a
`name` field and `Name()` returns `"deepseek"` for DeepSeek clients, and
`apiError()` already used `p.Name()`. But the direct `fmt.Errorf` calls in
`Chat` / `Stream` / `streamFallback` still hardcoded the `"openai:"`
prefix, so every DeepSeek failure surfaced as an OpenAI failure:

- `internal/model/openai.go:220` — `Chat` encoding request
- `internal/model/openai.go:232` — `Chat` decoding response
- `internal/model/openai.go:235` — `Chat` API error (`resp.Error.Message`)
- `internal/model/openai.go:238` — `Chat` no choices
- `internal/model/openai.go:272` — `Stream` nil chunk callback
- `internal/model/openai.go:279` — `Stream` encoding stream request
- `internal/model/openai.go:286` — `Stream` stream request failed
- `internal/model/openai.go:354` — `Stream` reading stream
- `internal/model/openai.go:405` — `streamFallback` failed

Evidence (pre-fix): a DeepSeek 401 was correctly labelled by `apiError`
(which already used `p.Name()`), but a malformed DeepSeek response
produced `openai: decoding response: ...`, and a DeepSeek stream failure
`openai: stream request failed: ...`. This matters operationally: the
daemon log / Telegram error would blame the wrong provider while
DeepSeek is the default.

Fix: all 9 now use `fmt.Errorf("%s: ...", p.Name(), ...)`. The other
single-provider clients (`anthropic.go`, `ollama.go`) hardcode their own
name, which is correct for them — only the shared client needed the
dynamic label.

Tests (new file `internal/model/deepseek_test.go`):
- `TestDeepSeekChatSuccess` — DeepSeek client hits `/chat/completions`
  with `Bearer` auth and returns content (396).
- `TestDeepSeekErrorsLabelledDeepSeek` — 401, undecodable body, and
  Stream 401 all contain `deepseek:` and never mention `openai`.
- `TestOpenAIErrorLabelUnchanged` — control: an OpenAI client still says
  `openai:` (the fix did not flatten the label).
- `TestDeepSeekNameCaseInsensitive` — `NewProvider("DeepSeek")` → `Name()`
  is `"deepseek"`.

### P1-2 — Config API-key test never covered DeepSeek — FIXED

`internal/config/config_test.go:204` `TestValidateAPIKeyRequired` looped
over `[]string{"openai", "anthropic"}` only. The config-layer guarantee
"deepseek requires a key" (enforced by `providerNeedsAPIKey` +
`Validate`) had no test, so a future refactor dropping `"deepseek"` from
that switch would go green while `NewProvider` still demanded a key —
config and model would disagree silently.

Fix: `"deepseek"` added to the loop (covers missing key, whitespace-only
key, and fake-key-validates). No production-code change was needed: the
enforcement itself was already correct.

## P2

### P2-1 — Live test failed instead of skipping when not DeepSeek-configured — FIXED

`internal/model/deepseek_live_test.go` used `t.Fatalf` when
`default_provider != "deepseek"` and `t.Fatal` when no DeepSeek key was
configured. So `go test -tags live ./internal/model/` on any machine whose
default provider is Ollama/OpenAI (or with no key) FAILED instead of
skipping. The sibling live tests skip in that situation
(`internal/messaging/telegram_live_test.go` skips unless opted in;
`internal/core/ollama_live_multiturn_test.go` skips when the model isn't
pulled / daemon unreachable).

Fix: both "not configured for DeepSeek" branches are now `t.Skipf` /
`t.Skip`. Hard failures remain only for real failures (provider build,
live API call) — that is the point of a live test.

Verified: without `-tags live` the test does not run at all (`no tests to
run`); with `-tags live` and `HOME` pointed at an empty dir (no config)
it SKIPs; with a config whose default is `ollama` it SKIPs.

Not changed: the test still reads `~/.orion/config.json` directly with
its own struct instead of `config.Load`, so it ignores env overrides
(`ORION_PROVIDER` / `ORION_API_KEY`). That is a deliberate-looking
consequence of the design rule "model stays dependency-free"
(`config.ProviderConfig` mirrors `model.ProviderConfig` to avoid the
import); changing it would couple `model` to `config`. Noted as a
limitation, not a defect: if Irfan ever runs DeepSeek purely via env
vars, this live test will skip while the app itself works.

### P2-2 — 60s first-live-attempt stall: no code defect, but a real timeout-design note — FLAGGED, NOT CHANGED

Parent context: the first live DeepSeek attempt stalled ~60s; curl and
plain Go reached the API fine; the immediate re-run passed in 1.57s.
Checked the OpenAI-compatible path for a timeout/retry gap:

- `sharedHTTPClient` has a 120s timeout (`provider.go`); each agent step
  gets its own 120s `stepCtx` (`internal/core/agent.go:266`).
- Retries happen only on 429/5xx and retryable transport errors
  (`doWithRetry`, `doStreamRequest`); a slow-but-eventually-successful
  first byte is not retried, it is just slow.
- When `client.Do` fails, `doWithRetry` returns `ctx.Err()` before
  considering a retry — so a timeout-class failure can only retry while
  the caller's context still has budget.

What the stall was: the live test's own context is 60s, so a ~60s
server-side stall hit the test's deadline. In production the 120s step
context would have tolerated that exact stall (slow success, not
failure). It was transient DeepSeek slowness, not a code or network
fault — consistent with the immediate clean re-run. No DeepSeek-specific
timeout bug exists.

The genuine (shared, all-provider) design note: one stalled attempt can
consume the entire step budget, because the per-attempt client timeout
(120s) equals the step context (120s) — the retry budget is unreachable
for timeout-class failures. A naive fix (e.g. a hard 30–60s per-attempt
cap in `doWithRetry`/`doStreamRequest`) would be WRONG here: legitimately
slow responses exist (reasoning models can take minutes to first token),
and DeepSeek's reasoning models are exactly that case. The correct fix
needs design (e.g. a first-byte/progress deadline distinct from the
total deadline, or retry-on-timeout only when context remains) and
touches shared helpers all providers use, while the parallel audit is
also editing this area. Left as a follow-up; no change made in this audit.

## Verified clean (no finding)

- **No DeepSeek API key in the repo.** Grepped for `sk-[A-Za-z0-9_-]{20,}`
  and for `deepseek` across the tree. The only `sk-` matches are
  synthetic test fakes, labelled as such in the source
  (`internal/security/security_test.go`, `scanner_audit_test.go`,
  `internal/app/app_test.go` — e.g. `sk-` + repeated characters /
  `testfake` markers). No key value was printed or read at any point;
  the live key was never opened (per task instructions, `~/.orion/config.json`
  was not read for its key value). `.gitignore` excludes `.orion/`,
  `config.json`, `*.env`, `audit.key`.
- **Key handling in error paths.** The key is only ever placed in the
  `Authorization: Bearer` header (`openai.go: headers()`). No error path
  includes it: `doWithRetry`/`doStreamRequest` errors carry only the URL;
  `apiError`/`bodyExcerpt` echo the *server's* response body (capped at
  500 chars), never the request. `cmd/orion config show` prints keys via
  `maskSecret`, which returns only `(not set)` / `****`. `config.Save`
  writes 0600 atomically and restores file values for env-supplied keys
  so an env key is never persisted. The secret scanner's generic
  `sk-` pattern covers DeepSeek keys (same `sk-` shape as OpenAI);
  there is no DeepSeek-specific pattern, and none is needed.
- **Defaults agree everywhere.** `provider.go`: `https://api.deepseek.com/v1`
  + `deepseek-chat`; `config.go` default entry: same base URL and model.
  `NewProvider("deepseek")` with no BaseURL/Model fills both (tested in
  `TestNewProviderDefaults`). Case-insensitive (`"DeepSeek"` works) and
  whitespace-tolerant, matching the other providers.
- **Config validation gaps checked.** Unknown provider rejected
  (`Validate`, `KnownProvider`, `NewProvider` default case — all agree on
  the same four names). Keyless DeepSeek rejected at *both* layers:
  `config.Validate` (via `providerNeedsAPIKey`, default-provider check)
  and `model.NewProvider` (explicit `deepseek: API key is required`).
  `Validate` only checks the *default* provider's key — documented
  design (other entries are spares); `NewProvider` enforces the key when
  a spare is actually built, so no keyless DeepSeek can ever run.
- **Default-provider wiring (DeepSeek is now the default).** Verified by
  reading: `config.Load` → `Validate` (fails fast on keyless default) →
  `app.Build` → `ActiveProvider` → `model.NewProvider` (re-checks key,
  fills defaults) → agent. A `default_provider` naming a provider with
  no key fails at startup with `config: provider "deepseek" requires an
  API key (set providers.deepseek.api_key or ORION_API_KEY)` — it cannot
  silently run keyless, and `orion run` / `oriond` both go through this
  path. `ORION_API_KEY` env override applies to the default provider
  entry, so it works for DeepSeek-as-default too. `ActiveProvider`
  itself does not check the key, but it is never reached without
  `Validate` + `NewProvider` on either side of it.
- **Consistency with the provider pattern.** DeepSeek reuses the
  OpenAI-compatible client with its own name/endpoint/model, exactly how
  the codebase already treats other OpenAI-compatible endpoints (vLLM,
  Ollama-compat, Hermes-class). No new wire format, no new retry/usage
  logic — the only DeepSeek-specific code is the factory case, the
  constants, and the config entry.
- **Live test gating.** `//go:build live` excludes it from normal runs
  (verified: `go test ./internal/model/` reports `no tests to run` for
  `-run TestLiveDeepSeek`). The key is never printed; only the reply and
  provider name are logged. (One live call was already made by the
  parent: PASS, 396 in 1.57s; this audit made no live calls.)

## Observations (out of scope, not fixed)

- `internal/config/config.go:133` default Anthropic model is
  `claude-sonnet-4-5`, but `internal/model/provider.go`
  `defaultAnthropicModel` is `claude-sonnet-4-20250514`. The config
  default wins on the config path, so the provider constant is dead
  there. Pre-existing, Anthropic-only — flagging for the main audit,
  not changed here. (DeepSeek has no such mismatch.)
- `defaultConfig` sets an explicit `BaseURL` for DeepSeek and Ollama
  but leaves OpenAI/Anthropic empty (filled by `NewProvider`). Harmless
  inconsistency; `mergeProviderDefaults` + `NewProvider` cover both
  shapes. Not changed.

## Verification (commands run)

```
gofmt -l internal/model/ internal/config/   # clean
go build ./internal/model/ ./internal/config/  # pass
go vet   ./internal/model/ ./internal/config/  # pass
go test ./internal/model/ ./internal/config/ -count=1  # ok model 0.987s, ok config 0.013s
go test -tags live ./internal/model/ -run TestLiveDeepSeek  # no tests to run (no tags);
   # SKIP with HOME lacking config; SKIP with default_provider=ollama
```

Repo-wide `go build ./...` / `go vet ./...` currently FAIL in
`internal/security` (`isBlockedCommand` undefined) — that is the
parallel fresh-eyes audit team's in-flight edit, not this scope and not
caused by these changes. My packages build, vet, and test green
independently. A repo-wide green re-run should be done after that team
lands.

## Files changed by this audit

- `internal/model/openai.go` — P1-1 label fix (9 sites, no behaviour change
  other than the error prefix).
- `internal/config/config_test.go` — P1-2: DeepSeek added to
  `TestValidateAPIKeyRequired`.
- `internal/model/deepseek_live_test.go` — P2-1: fail → skip when not
  DeepSeek-configured.
- `internal/model/deepseek_test.go` — new: DeepSeek Chat success, error
  labelling (Chat/Stream/decode), OpenAI label control, case-insensitive name.
- `AUDIT2_PART_DEEPSEEK.md` — this report.
