# Orion Architecture (v0.2.0-rc10)

```
cmd/orion      CLI: run, chat, config, status, tools, memory, skills,
               subagent, agents, telegram, daemon, version
cmd/oriond     Daemon: Telegram polling until interrupted
internal/app   Wiring: config -> provider -> tools -> memory -> agent
               -> subagents -> skills -> audit. Built exactly once,
               used by both entrypoints.
internal/config  JSON config + env overrides + validation (0600 file)
internal/model   Provider interface {Chat, Stream} + providers:
               openai (OpenAI-compatible: OpenAI, vLLM, OpenRouter,
               Hermes-class open models), anthropic, ollama.
               NewProvider factory; retries with backoff.
internal/core    Agent loop: system prompt + recent memory -> provider
               -> tool calls -> tool results -> reply. Max steps,
               per-iteration timeouts, ctx cancellation, streaming
               final turn, memory recording, graceful shutdown.
internal/tools   Registry + 22 tools (shell, files, web, calculator,
               datetime, process list, skill, grep, glob, todo/task,
               subagent; MCP tools register dynamically). Central
               schema validation in Registry.Execute. Shell delegates denylist
               checks to internal/security.
internal/memory  FileStore: JSON file, atomic writes, facts + 200-msg
               conversation history, case-insensitive fact search.
internal/sandbox  Namespace + pivot_root sandbox for shell_exec
               (on by default; user/mount/net namespaces,
               read-only /usr, minimal /etc copy, writable /work),
               with a seccomp syscall filter (orion-seccomp helper)
               when the helper is installed. Refuses the command
               (fail-closed) when the sandbox is enabled but cannot
               be established, unless the operator explicitly opted
               into an unsandboxed fallback.
internal/security  Canonical token-based IsBlockedCommand (ONE copy),
               secret scanner/redactor, SHA-256 hash-chained audit log.
internal/messaging Telegram gateway (Bot API over HTTP, long polling)
               + CLI channel. Telegram ONLY — no WhatsApp/Baileys.
               Both channels call app.Handle -> same agent, same memory.
internal/subagent  Manager: spawn tasks with timeouts, concurrency
               limit, cancel, Wait/WaitAll, panic recovery. Executor
               is injected (the agent loop) to avoid import cycles.
internal/skills  Registry loading Markdown skill files (*.md) with
               name/description/tools parsing.
```

## Data flow

```
Telegram update / CLI line
        |
        v
  app.Handle(ctx, input)  -->  core.Agent.Run
        |                          |  (system + memory + input)
        |                          v
        |                     model.Provider.Chat
        |                          |  (tool calls?)
        |                          v
        |                     tools.Registry.Execute
        |                          |  (shell -> security.IsBlockedCommand)
        |                          v
        |                     reply recorded to memory.Memory
        v
  reply sent back on the same channel
```

## Invariants

- No provider is hardcoded in the core. Selection is config-only,
  via `model.NewProvider`.
- No package duplicates the denylist. `security.IsBlockedCommand`
  is the single implementation; tools delegate to it.
- Sandbox mode is a second layer, never a replacement: the
  denylist still runs first, and when the sandbox is enabled but
  cannot be established shell_exec refuses the command
  (fail-closed), unless the operator explicitly opted into an
  unsandboxed fallback (`sandbox.allow_unsandboxed_fallback`).
- Seccomp is a second layer on the sandbox, never a replacement
  for it: sandboxed commands run under a default-allow syscall
  blocklist (orion-seccomp helper, `internal/sandbox/seccomp.go`)
  when a usable launcher is installed, and are REFUSED
  (fail-closed, Audit 3 F-A3-1) when it is not — unless the
  operator explicitly opted into unfiltered runs
  (`sandbox.allow_without_seccomp`), in which case every degraded
  run is logged, or disabled the filter outright
  (`sandbox.disable_seccomp`).
- Every network call has a timeout; every tool output is capped;
  every agent run has a max-step budget.
- Secrets never appear in logs, config dumps, audit entries, or
  tests (scanner redaction + explicit masking in the CLI).
- Tests use `t.TempDir()` and `httptest` only. No test touches the
  real home directory, the live network, or a live destructive command.

## Provider timeouts (internal/model)

One stalled attempt used to eat the whole agent step: the
per-attempt HTTP client timeout (120s) was the same as the step
context (120s), so a hung DeepSeek call left the agent with no time
to retry and no time to report a clear error. The timeouts below are
three DISTINCT bounds, one per failure mode, and they nest:

```
connectTimeout (10s)  <  progressTimeout (60s)  <  totalTimeout (100s)  <  step timeout (120s)
```

All three live as package-level vars in
`internal/model/provider.go` (`connectTimeout`, `progressTimeout`,
`totalTimeout`) and are shared by all three providers
(OpenAI-compatible incl. DeepSeek, Anthropic, Ollama) through the
shared helpers `doWithRetry` / `doStreamRequest` and the shared
`sharedHTTPClient` — there is no per-provider timeout logic.

| Timeout | What it bounds | How it is enforced |
|---|---|---|
| `connectTimeout` = 10s | TCP dial + TLS handshake | `http.Transport.DialContext` (dial timeout read from the var on every dial) + `TLSHandshakeTimeout`. Fails fast on an unreachable or black-holed endpoint. |
| `progressTimeout` = 60s | Silence: first byte AND mid-stream gaps | First byte: transport `ResponseHeaderTimeout`. Body: `progressReader`, a wrapper around every response body (see below). |
| `totalTimeout` = 100s | One whole `Chat` or `Stream` invocation, including retries, backoff and any `Chat` fallback | A context derived with `WithTimeout` by `doWithRetry` and by each `Stream` function for the whole streaming call. 100s sits inside the agent step's 120s (`internal/core`: `defaultTimeoutSeconds`) with 20s to spare, so the provider returns a clear timeout error while the step still has time to record it. If the step budget ever grows, `totalTimeout` should grow with it; it must always stay below the step timeout. The caller's own context remains the final outer bound. |

There is deliberately no `http.Client.Timeout`: it is fixed when
the client is built and would be a second, per-attempt wall-clock
bound that a slow-but-steady stream must never hit. The per-call
context is the wall-clock bound, and it is read at call time, so
tests can tune the vars down against fake slow servers (a test that
changes `connectTimeout` or `progressTimeout` must also rebuild the
client with `newSharedHTTPClient()`, since the transport fields are
fixed at build time — see `setTestTimeouts` in
`internal/model/timeout_test.go`).

### Progress, not wall-clock, is the signal for streams

`progressReader` (`internal/model/provider.go`) wraps a response
body and aborts a read that goes silent for `progressTimeout` with
a `*progressTimeoutError` (a `net.Error` with `Timeout() == true`).
Every byte that arrives resets the window, so a reasoning model
that streams slowly but STEADILY is never killed, however long the
whole response takes — only a stream that goes completely silent
for the full 60s is aborted. 60s is deliberately generous:
reasoning models (DeepSeek reasoner, Claude extended thinking) can
pause for tens of seconds between bursts, and time-to-first-token
alone can reach ~30s under load; a 60s silence means the server or
a proxy is stuck, not thinking.

The reader runs one pump goroutine per body. The pump owns its
read buffer and hands completed chunks to `Read` over a channel,
so no buffer is ever shared with a caller that moved on after a
timeout. `Close` is idempotent and unblocks the pump; the pump
also exits on its own at EOF/error, so a fully-read body leaves no
goroutine behind.

### Retry interaction

- A progress timeout is a transient, retryable fault: the error
  implements `net.Error` with `Timeout() == true`, so the existing
  retry policy (`isRetryableTransportError`, 429/5xx + `Retry-After`)
  applies to it unchanged. A stalled `Chat` body read is retried
  inside `doWithRetry` like a failed round trip; a stalled stream
  setup is retried inside `doStreamRequest`.
- A stream that stalls mid-way is retried ONLY when nothing usable
  was delivered yet (no content chunk emitted, no named tool call
  announced). Re-running the stream then cannot duplicate a live
  chunk. Once a chunk is live with the caller, a stall returns a
  clear progress-timeout error instead — the stream is never
  silently restarted after delivery started.
- A stream that yields nothing usable (empty deltas, a bare
  `content_block_start`, a truncated stream) still falls back to a
  single `Chat` call, as before; the fallback is bounded by the
  same `totalTimeout` context as the stream attempts.
- One subtlety the implementation respects: `doStreamRequest` must
  NOT derive a `totalTimeout` context of its own, because the
  response body outlives the call — cancelling a context derived
  there would kill the caller's stream the moment the helper
  returned. The total bound for streaming lives in the `Stream`
  functions, which derive one context for the whole call and pass
  it down.

