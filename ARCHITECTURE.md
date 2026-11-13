# Orion Architecture (v0.1)

```
cmd/orion      CLI: run, chat, config, status, tools, memory, skills,
               subagent, telegram, daemon, version
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
internal/tools   Registry + 10 tools (shell, files, web, calculator,
               datetime, process list). Shell delegates denylist
               checks to internal/security.
internal/memory  FileStore: JSON file, atomic writes, facts + 200-msg
               conversation history, case-insensitive fact search.
internal/sandbox  Namespace + pivot_root sandbox for shell_exec
               (opt-in via config; user/mount/net namespaces,
               read-only /usr, minimal /etc copy, writable /work),
               with a seccomp syscall filter (orion-seccomp helper)
               when the helper is installed. Falls back to the
               denylist alone, with a warning, when the host cannot
               create namespaces.
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
  denylist still runs first, and when the sandbox is unavailable
  shell_exec falls back to the denylist alone with a warning.
- Seccomp is a second layer on the sandbox, never a replacement
  for it: sandboxed commands run under a default-allow syscall
  blocklist (orion-seccomp helper, `internal/sandbox/seccomp.go`)
  when a usable launcher is installed, and with namespace /
  pivot_root containment only — plus a logged warning — when it
  is not.
- Every network call has a timeout; every tool output is capped;
  every agent run has a max-step budget.
- Secrets never appear in logs, config dumps, audit entries, or
  tests (scanner redaction + explicit masking in the CLI).
- Tests use `t.TempDir()` and `httptest` only. No test touches the
  real home directory, the live network, or a live destructive command.
