# Subagent children run ungated — design note

**Status:** design only (F-A21-16). No code, no behaviour change.
Written 2026-11-14, against HEAD `0ca7a5c`.

## Current state

A subagent child is built by `newSubagentExecutor`
(`internal/app/subagent.go:156`), which creates a fresh
`core.NewAgent` and never calls `SetPermissionGate` (`core/agent.go:382`;
nil gate means no gate, `agent.go:149`). The code documents this
(`subagent.go:145`): "children run without the interactive
permission gate". This predates the F0 framework fixes; `be92b90`
changed nothing here.

What a child **can** do: its registry (`newChildRegistry`,
`subagent.go:126`) is the full default registry, so `shell_exec`,
`file_write`, `file_edit`, `web_fetch` and `web_search` are all
available, with the parent's provider, the read-only typed-memory
view, and the parent's planning store.

What a child **cannot** do: MCP tools and the skill tool are
deliberately not in the child registry, and there are no subagent
tools (no recursion). The plan-mode tools are likewise excluded
(F-A22-16): `enter_plan_mode` needs a Telegram gateway a child
never gets (it would guaranteed-fail), and `exit_plan_mode`
touches the shared planning store, so a child calling it would
clear the PARENT chat's pending plan — the approval boundary
plan mode exists to enforce. Neither tool can work in a child,
so neither is listed there. The plan-mode BLOCK is not removed
with the tools: the registry keeps the plan-mode checker, so a
pending parent plan still refuses the child's destructive
tools. The shell denylist, result caps and the
parent's PreToolUse hook bus (non-interactive, fail-closed) still
apply — a hook can block a child's tool call exactly as it blocks
the parent's.

## The risk

One parent `subagent_spawn` approval buys a whole child turn of
ungated destructive/network calls. The approval the user gives
covers the spawn (the brief), not each tool call inside it: a
child told to "clean up" can run `shell_exec` / `file_write`
freely, and a compromised or confused child (bad brief, prompt
injection via fetched content) has no per-call checkpoint. The
exposure is bounded by the denylist, the hook bus and the missing
MCP/skill tools — but there is no approval boundary inside a
child turn at all.

## Plan for gating children, if wanted

**A. Non-interactive gate on the child (recommended default).**
Give the child a `permissions.Gate` whose prompter never prompts:
deny (or allow-list) by class, e.g. deny Destructive, allow Safe
and Network. Smallest change — one `SetPermissionGate` call plus
a deny-by-default prompter. Trade-off: a child that legitimately
needs to write files (a coding child) is blocked; the policy
needs a per-spawn opt-in to be useful.

**B. Per-brief tool allowlist.** The spawn carries the tools (or
risk classes) the child may use; the child registry is built
filtered to that set instead of the full default registry.
Trade-off: more plumbing (registry construction takes a filter),
but the brief's author decides the child's powers explicitly,
and "no tools" becomes a real option. Combines well with A.

**C. Hook-based child policy.** Extend the existing PreToolUse
hook bus with a child-policy hook (deny Destructive unless the
brief opts in). Trade-off: reuses machinery that already works
on children, but policy then lives in operator-configured hooks,
not in the spawn call — harder to reason about per brief.

**D. Approval escalation to the parent.** The child pauses on a
gated call and the parent approves through the normal Telegram
flow. Trade-off: most faithful to the parent gate, but the most
complex — a child turn must suspend and resume, with deadlock
and stall risks if the parent never answers. Only if interactive
child approvals are ever wanted; do not build this first.

Suggested order if gating is ever wanted: A (deny Destructive
by default, opt-in per spawn) + B (the opt-in is an explicit
tool/class list in the spawn), with C as the operator escape
hatch. D is a separate design, not part of this plan.
