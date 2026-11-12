# Language Decision (LOCKED)

Decision date: 2026-11-13. Locked by the owner. Do not revisit
without the owner's explicit approval.

## Decision

- **Go** for the core/daemon/agent loop/tools/memory/security/CLI
  (`internal/*`, `cmd/orion`, `cmd/oriond`).
- **TypeScript** for the desktop/web UI — **v0.2 ONLY**. Do not build
  it in v0.1.
- **Python is rejected** as the primary language. (A Python prototype
  existed before this decision; it is reference only and is not
  shipped.)
- **Rust is deferred** to v0.2 hot paths, if ever needed.

## Why Go

- Single static binaries for the daemon and CLI — easy to ship and
  run local-first on Linux/macOS.
- Strong standard library: HTTP clients/servers, JSON, crypto
  (SHA-256 audit chain), `os/exec` with timeouts, concurrency
  primitives for the subagent manager.
- Fast compile/test cycle and a strict compiler that catches whole
  classes of errors before runtime — important for a stability-first
  agent that runs shell commands.
- No runtime dependency on the user's machine (unlike Python).

## What this means in practice

- All v0.1 code is Go, module `github.com/irfan/orion`, Go 1.27.
- No Python runtime, no Node runtime in v0.1.
- The provider layer is language-agnostic at the boundary (HTTP
  JSON APIs), so the Go core can talk to any LLM provider.
- The v0.2 TypeScript UI will talk to the Go daemon over HTTP;
  that interface is not designed yet and is out of v0.1 scope.
