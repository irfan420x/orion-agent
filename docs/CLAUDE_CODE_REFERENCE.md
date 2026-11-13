# Claude Code — reference for Orion

**Rule: patterns only, never verbatim copying.** Orion's core is Go; this
tree is TypeScript. Study how a mature agent is *organised* (loop, tools,
permissions, skills, subagents, UI) and re-implement the ideas in Go (or in
TypeScript for Orion v0.2's UI). Do not paste its code into Orion.

## Source

- Upstream (user-supplied): https://github.com/Irfan430/claude-codea
- Local copy: `~/workspace/claude-code-reference` (shallow clone, commit
  `c2357be`, branch `main`)
- Size (verified 2026-11-13): **579,561 lines** TS/JS, 1,605 `.ts` files,
  2,498 files total, 52 MB on disk. Package
  `@anthropic-ai/claude-code 0.0.0-leaked`.
- Structure: `src/` (CLI agent), `web/` (web UI), `mcp-server/`,
  `tests/`, `docs/`, `prompts/`, `scripts/`, `docker/`, `helm/`,
  `grafana/`.

## Mapping — Claude Code area → Orion counterpart → what to learn

| Claude Code | Orion today | Idea worth borrowing |
|---|---|---|
| `src/query.ts`, `src/QueryEngine.ts`, `src/Task.ts`, `src/coordinator/` | `internal/core/agent.go` | Agent-loop shape: how turns, tool calls, subagents and retries are sequenced; coordinator/worker split |
| `src/Tool.ts`, `src/tools/`, `src/constants/toolLimits.ts` | `internal/tools/` + `caps.go` | Tool schema/registry shape; per-tool limits (Orion just built caps — compare defaults) |
| `src/hooks/toolPermission/` (`PermissionContext`, `coordinatorHandler`, `interactiveHandler`) | `internal/security` denylist + Telegram allowlist | Permission model: ask/allow/deny flows, per-tool approval gates — the `/approvals` idea for Orion |
| `src/skills/` | `internal/skills/skills.go` | Skill discovery, bundling, invocation |
| `src/coordinator/`, swarm worker handler | `internal/subagent/` | Multi-agent fan-out, task handoff, result merging |
| `src/history.ts`, `src/context/` | `internal/memory`, `internal/core/recall.go`, history budget | Context compaction, what survives a compact, memory writes |
| `src/cost-tracker.ts`, `src/costHook.ts` | `core.TurnStats`, Telegram footer | Token/cost accounting per turn and session |
| `src/commands/`, `src/commands.ts` | `internal/messaging/commands.go` (17 commands) | Command framework shape; which commands exist upstream that Orion lacks |
| `src/ink/`, `src/components/`, `src/screens/` | CLI (`cmd/orion`) | Terminal UI patterns (progress, streaming, menus) |
| `web/` (`WebApp.tsx`, `app/`, `components/`) | **v0.2 TS UI (not built yet)** | The closest direct reference: web app structure, components, hooks — the starting point for Orion's desktop/web UI |
| `src/hooks/`, `src/plugins/` | — (no hook system yet) | Lifecycle hooks and plugin loading — a future Orion feature |
| `src/server/`, `src/remote/`, `mcp-server/` | `oriond` daemon | Daemon/remote control shape; MCP server exposure |
| `src/outputStyles/` | Telegram footer / reply format | Reply-style system (personality/format switching) |

## Where it helps next

- **Audit 3 (running):** reliability patterns — retry behaviour, context
  overflow handling, daemon recovery (compare, don't copy).
- **v0.2 TS UI:** `web/` is the primary reference for structure.
- **Future:** permission/approval UX (`toolPermission` hooks),
  skills, subagent coordination, cost tracking.
