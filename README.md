# Orion

Orion is a model-agnostic OS agent that can do everything, with many
subagents — built in Go, local-first, Linux/macOS first.

> **Status: v0.2.0-rc25.** The Go core, agent loop, tools, memory, security,
> Telegram gateway, CLI channel, subagents, skills, and CLI are fully
> built and tested (2,000 tests total — 1,996 pass, 0 fail, 4 skip;
> 24/24 packages; recount in the working tree after the
> Audit 22 fix batch, the provider-auto context-window change, the cron tool, the behavior fix, the busy-input acknowledgement, the computer-use M3 input tool (2026-11-15) and the Audit 23 fix batch (2026-11-16) and the Audit 23 fix follow-up batch (2026-11-17) and the Audit 23 F-A23W2-9 element re-check (2026-11-17) — see CHANGELOG for the rc25 audit fixes).
> The model can now delegate to subagents (`subagent_spawn` /
> `list` / `result` / `stop`), and the footer token count is honest
> (the 59k-token turn was a real prompt bloat — a stale news feed in
> history — fixed by a calibrated token estimator; same turn now costs
> ~3.3k tokens). The TypeScript desktop/web UI is v0.2 only (see
> [LANGUAGE_DECISION.md](LANGUAGE_DECISION.md)).
>
> **License: MIT** — see [LICENSE](LICENSE). The repository is public
> at https://github.com/irfan420x/orion-agent; anyone may use, copy,
> and modify the code under the MIT terms.

## Quick start

> **Go toolchain:** Orion needs Go 1.27+ — install it from
> https://go.dev/dl and put it on PATH. (On this VM/container Go
> 1.27.4 lives at `~/go1.27.4/bin/go` because `/usr/local` does not
> survive restores here; use `export PATH=$PATH:/home/hatch/go1.27.4/bin`
> in every shell, or `~/go1.27.4/bin/go` directly.) `install.sh`
> finds the toolchain itself (PATH, then `~/go1.27.4/bin/go`).

```sh
# 0. Get the code (the repo is public, MIT-licensed — no access needed):
git clone https://github.com/irfan420x/orion-agent.git
cd orion-agent

# 1. Install — builds orion + oriond, installs to /usr/local/bin
#    when writable, otherwise ~/.local/bin; writes a default
#    ~/.orion/config.json only if none exists (never overwrites).
./install.sh
#    ./install.sh --help            usage
#    ./install.sh --prefix DIR      install to DIR/bin
#    ./install.sh --systemd         also install + enable the oriond
#                                   systemd user unit (see below)
#    make install                   same, via the Makefile
#    make build                     just build ./bin/orion{,d}

# 2. Configure a provider — pick one:
orion config set-provider ollama        # local, no key (default;
#    needs Ollama running with a model pulled, e.g. `ollama pull qwen2.5:0.5b`)
orion config set-provider deepseek      # hosted, needs a key
orion config set-api-key deepseek <YOUR_DEEPSEEK_API_KEY>
orion config set-provider openai
orion config set-api-key openai <YOUR_OPENAI_API_KEY>
orion config set-provider anthropic
orion config set-api-key anthropic <YOUR_ANTHROPIC_API_KEY>
#    Tip: a key passed on the command line lands in shell history and
#    the process list — prefer stdin: orion config set-api-key deepseek - < deepseek-key.txt
orion config show                       # secrets are masked
orion status                            # provider, tools, memory, Telegram

# 3. Run
orion version                           # version + build info
orion run "what time is it in Singapore?"
orion chat                              # interactive CLI chat

# 4. Telegram — set the bot token, verify it, then poll:
orion config set-api-key ...            # (no CLI setter for the token;
#    set telegram_token in ~/.orion/config.json, or export
#    ORION_TELEGRAM_TOKEN=<YOUR_TELEGRAM_BOT_TOKEN>)
#    Also set telegram_allowed_chats in ~/.orion/config.json to your
#    chat ID(s) — oriond, `orion telegram poll` and `orion daemon`
#    refuse to start with an empty allowlist unless
#    allow_all_telegram_chats=true is set explicitly (any chat could
#    otherwise drive the agent). Send /whoami to the bot to learn
#    your chat ID.
orion telegram test                     # verify the token (getMe)
orion telegram poll                     # start the gateway (foreground)
orion daemon                            # same, via the orion CLI
oriond                                  # standalone daemon binary
#    Testing without Telegram: oriond, `orion daemon` and
#    `orion telegram poll` all honour ORION_TELEGRAM_BASE_URL to point
#    at a self-hosted Bot API server (or a local fake for testing —
#    see `oriond --help`).

`orion telegram poll` and `orion daemon` are turn-only: they run the
same gated, per-chat-isolated agent loop as `oriond`, but they have
no slash-command framework — `/restart`, `/new`, `/model`, `/help`
and the rest go to the agent as plain text, not as commands.
`/restart` owner-only therefore holds vacuously there (there is no
`/restart` command to refuse, only a text turn about restarting).
`oriond` is the only entrypoint with the full command framework —
the 22-command `/` menu below. Use `oriond` for a bot that answers
`/commands`.

While a turn runs, the bot streams a live "💻 terminal" card into
the chat (one message, edited in place): each tool call appears as
a block with its command, file writes as ✍️ lines, repeats collapse
to (×N). Displayed commands are redacted, so secrets never leak
into the chat. The final reply is a structured message in your
language, with a runtime footer (model, tokens, steps, duration)
you can toggle with `/footer`. On `oriond`, the `/` menu offers 23 commands
(`/help`, `/new`, `/sessions`, `/resume`, `/stop`, `/status`, `/model`,
`/usage`, `/budget`, `/busy`, `/undo`, `/retry`, `/pause`, `/agents`, `/title`, `/compress`,
`/save`, `/restart`, `/rules`, `/whoami`, `/footer`, `/memory`, `/skills`); per-chat
command state (title, footer preference, pause flag, usage totals, busy-input mode)
persists in `~/.orion/sessions.json`, while conversation transcripts
persist separately as per-session JSONL files (see [Sessions](#sessions)).

While a turn runs, a new message from the same chat is acknowledged
at once instead of waiting silently: the bot sends a short status
message and handles the new message according to the chat's busy-input
mode — **interrupt** (default) cancels the running turn and redirects
it to the new message, **queue** runs the new message after the current
task finishes, **steer** joins the new message into the running turn at
the next step boundary. Change the mode with `/busy queue`, `/busy
steer`, or `/busy interrupt` (`/busy` alone shows the current mode).
Queued messages run in arrival order. Commands (`/stop`, `/busy`, …)
never trigger busy handling. If the running turn is waiting on a plan
decision, interrupt degrades to queue so the plan card is not orphaned;
if it is waiting on a question, the question card is visibly cancelled.

# Run oriond as a service (systemd user unit, oriond.service):
./install.sh --systemd                  # installs + enables + starts the unit
systemctl --user status oriond
journalctl --user -u oriond -f
```

No install step needed for development — `go build ./...` and
`go test ./...` work from the repo root, and `make build` puts
fresh binaries in `bin/`.

## Tests

24/24 packages, 2,000 tests total (1,996 passing, 0 failing, 4 skipped)
(recount in the working tree after the Audit 22 fix batch, the
provider-auto context-window change, the cron tool, the behavior fix, the busy-input acknowledgement, the computer-use M3 input tool, 2026-11-15, and the Audit 23 fix batch, 2026-11-16, and the Audit 23 fix follow-up batch, 2026-11-17, and the Audit 23 F-A23W2-9 element re-check, 2026-11-17; rc25 clean-clone baseline was 1,389 total / 1,385 passing).
Run the suite with
`go test ./...` or `make test`.

## Configuration

`~/.orion/config.json` (0600). Provider selection lives here only:

```json
{
  "default_provider": "ollama",
  "providers": {
    "openai":    { "name": "openai",    "api_key": "", "model": "gpt-4o-mini" },
    "deepseek":  { "name": "deepseek",  "api_key": "", "base_url": "https://api.deepseek.com/v1", "model": "deepseek-flash" },
    "anthropic": { "name": "anthropic", "api_key": "", "model": "claude-sonnet-4-5" },
    "ollama":    { "name": "ollama",    "base_url": "http://localhost:11434", "model": "llama3.1" }
  },
  "telegram_token": "",
  "telegram_allowed_chats": [],
  "data_dir": "~/.orion",
  "max_steps": 20,
  "timeout_seconds": 120,
  "max_tool_result_bytes": 262144,
  "max_history_tokens": 0,
  "sandbox": { "enabled": true, "root": "~/.orion/sandbox", "allow_network": false, "allow_without_seccomp": false }
}
```

Environment overrides: `ORION_PROVIDER`, `ORION_API_KEY`, `ORION_MODEL`,
`ORION_TELEGRAM_TOKEN`, `ORION_TELEGRAM_BASE_URL` (Bot API endpoint —
honoured by `oriond`, `orion daemon` and `orion telegram poll`), `ORION_DATA_DIR`, `ORION_SANDBOX` (`on`/`off`),
`ORION_SANDBOX_ROOT`. Secrets are never printed:
`orion config show` masks them, and the secret scanner redacts them
from logs and audit entries.

### Size caps: tool results and history

Two limits keep a runaway tool or a long conversation from blowing
up the model's context window (and the daemon's memory). Both are
configurable in `config.json`; both degrade gracefully — truncate
with a marker — instead of failing the turn.

- `max_tool_result_bytes` (default `262144` = 256 KiB, allowed
  1 KiB–10 MiB): the maximum bytes of any single tool result that
  reach the model. Enforced centrally in the tool registry, so it
  covers every tool, built-in or added later. A result over the cap
  is cut at a UTF-8 boundary and marked
  `[truncated: showing first N bytes of M total bytes]`, so the
  model knows data was cut. Tool error text is capped the same way.
  Per-tool caps still bound memory while a tool runs (`shell_exec`
  64 KiB with a marker reporting the true total output size,
  `file_read` 1 MiB, `web_fetch` 256 KiB by default): a file over
  1 MiB is returned truncated with its true size, not refused.
  Lower this if your provider has a small context window; raise it
  (up to 10 MiB) if you routinely need large file reads.
- `max_history_tokens` (default `0` = auto, allowed `0` or 1K–1M):
  the token budget for the history fed to the provider on each
  step — system prompt, recent history, fact-aware recall
  injection, and in-turn messages, estimated with
  `model.CountTokens` (~4 runes/token, the same estimate the
  model layer uses for usage). With the default `0` the budget is
  derived from the active provider's declared context window:
  **half the window** (the old 32768-token budget was this same
  rule for the old, incorrect 64K DeepSeek window). Half a window
  leaves headroom for the tool definitions and the completion.
  An explicit value overrides the derivation. When the window is
  over budget the agent loop trims the oldest turns first and
  never drops the system prompt, never drops the latest user turn
  or the most recent turn, and never breaks a tool-call/tool-result
  pair (whole turns are dropped, and content is only ever cut —
  never the tool-call fields). If a single tool result alone
  exceeds the budget it is truncated head-first with a
  `[truncated for context budget …]` marker and the turn
  continues. The full transcript is still kept in memory and in
  the `MaxStepsError` transcript; only the request window is
  trimmed. Recall counts against the same budget, so recall +
  history together stay within it: when the window is tight, the
  oldest turns pay for the recalled facts, never the system
  prompt. The 80% auto-compact / 70% warning thresholds are
  fractions of this budget, so they scale with the provider
  automatically.

  Provider context windows (verified against the official provider
  docs on 2026-11-15; unknown models fall back to the provider's
  smallest verified window, and a provider that declares no window
  at all falls back to the legacy 32768-token budget):

  | Provider | Model | Window | Derived history budget |
  |---|---|---|---|
  | DeepSeek | deepseek-chat, deepseek-reasoner, deepseek-flash, deepseek-v4-pro | 1,000,000 | 500,000 |
  | OpenAI | gpt-4o, gpt-4o-mini | 128,000 | 64,000 |
  | OpenAI | gpt-4.1, gpt-4.1-mini | 1,047,576 | 523,788 |
  | Anthropic | claude-sonnet-4-5, claude-opus-4-5, claude-haiku-4-5 | 200,000 | 100,000 |
  | Ollama | llama3.1, llama3.2 | 131,072 | 65,536 |
  | Ollama | qwen2.5-0.5b, qwen2.5-7b, qwen2.5-14b | 32,768 | 16,384 |

  DeepSeek's aliases currently route to deepseek-v4-flash at a 1M
  context (the old 64K figure was stale) and retire 2026-07-24;
  the derived DeepSeek budget is therefore 500,000 tokens, not
  32768 — the faithful result of deriving half the verified
  window, with no extra ceiling. Ollama's `mistral` has no
  verified window (no official statement found) and falls to the
  32,768 unknown-model fallback. For Ollama, every chat request
  also sends `options.num_ctx` set to the declared window, so the
  server default (4096 per the current official FAQ) can never
  silently truncate a derived budget.

Switch providers without rebuilding:

```sh
orion config set-provider anthropic
orion config set-provider openai
```

**Model size / hardware:** local models run through Ollama. A real
Hermes model needs a bigger machine than a small VM: `hermes3:3b`
needs ~8 GB RAM and `hermes3:8b` needs ~16 GB. On a host with enough
RAM: `ollama pull hermes3:8b`, then set the model in config. See
[docs/E2E_LIVE_OLLAMA.md](docs/E2E_LIVE_OLLAMA.md) for the measured
evidence (this VM has ~700 MiB free, so no Hermes model runs here).

### Debug endpoint (pprof)

The daemon exposes an optional pprof/debug endpoint (F-A3-2) so
goroutine count, heap allocation and GC behaviour are observable on
a running daemon — before rc21 these were only measurable in
scratch benchmarks. It is **off by default** and opt-in only:

```json
"debug": { "pprof_enabled": true, "pprof_port": 6060 }
```

- `pprof_enabled` (default `false`): when off, the daemon listens
  on nothing extra — behaviour is exactly as before.
- `pprof_port` (default `6060`, allowed 0–65535): `0` means the
  6060 default; an out-of-range port fails config validation at
  startup. A port that is already in use logs a loud warning and
  the daemon keeps polling without the endpoint.
- The endpoint binds **127.0.0.1 only** — never `0.0.0.0` — and
  serves only the standard `net/http/pprof` handlers under
  `/debug/pprof/` (index, `goroutine`, `heap`, `threadcreate`,
  `block`, `mutex`, `cmdline`, `profile`, `symbol`, `trace`).
  Anything outside `/debug/pprof/` gets 404. It serves no config,
  no secret, and no application data, and enabling it does not
  weaken the sandbox. The daemon logs the listen address on
  startup (`oriond: debug pprof listening on 127.0.0.1:6060`).
  Both `oriond` and `orion daemon` honour the same setting.

```sh
curl http://127.0.0.1:6060/debug/pprof/              # index
curl http://127.0.0.1:6060/debug/pprof/goroutine?debug=1
curl http://127.0.0.1:6060/debug/pprof/heap
go tool pprof http://127.0.0.1:6060/debug/pprof/heap
```

## Sending files (Telegram)

When the user asks for a file, or the agent created one they should
have, the agent sends it with a `MEDIA:` line on its own line in its
reply, with the absolute path and no spaces — for example
`MEDIA:/home/irfan/Downloads/report.csv`. The gateway uploads the
file to the chat (photo, video, audio, voice, or document by
extension; unknown extensions go as a document), strips the tag from
the visible reply, and appends a per-file delivery report (`📎 Sent
…` / `⛔ Could not send …`) after the footer. Only explicit `MEDIA:`
tags send: a path merely mentioned in prose is never sent, and the
CLI (`orion run`) leaves tags as visible text.

- **No allowlist by default — denylist only.** Files may be sent
  from anywhere on the laptop except the denylisted paths below.
  Set `media_allow_dirs` in `config.json` to restrict sending to
  specific directories (an explicit list restricts, never
  appends). A file outside an explicitly configured list is
  refused with a visible note — never sent, never silent.
- **Denylist (absolute, non-overridable).** `~/.orion`, `~/.ssh`,
  `~/.gnupg`, `/etc`, and credential/key filenames (`*.env`,
  `*.pem`, `*.key`, `id_*`, `*credentials*`) are never sendable —
  not even by the owner, not even from an allowlisted dir, and not
  through a symlink that points at them (paths are resolved
  before the check). The refusal note names the path and says it
  is never sendable; it never hints at what the file contains.
- **Limits.** At most 5 files per reply (`media_max_files`,
  clamped 1–20) and 50 MiB per file (`media_max_bytes`, clamped
  to 50 MiB, the Bot API limit). Over-limit files are refused
  with a visible note. One file's failure never blocks the others
  or the text reply.
- **Kill-switch.** `"media": { "enabled": false }` disables
  extraction entirely: tags stay as visible text and no file is
  sent.

## Sandbox (shell_exec)

`shell_exec` runs confined by default: Linux namespaces + chroot
(see [SECURITY.md](SECURITY.md)), with a seccomp filter when the
`orion-seccomp` launcher is installed. The sandbox is **on by
default** — a fresh install confines `shell_exec`, and a config
file that omits the `sandbox` section still confines it. Turning
it off is an explicit operator opt-out: set
`sandbox.enabled=false` in `~/.orion/config.json`, or export
`ORION_SANDBOX=off`. An opted-out run is logged every time
(`shell_exec: sandbox disabled; running command without sandbox
isolation`), so the loss of confinement is visible per command,
not once per process. When the sandbox is on but cannot be
established on the host, `shell_exec` refuses the command
(fail-closed) instead of running it unsandboxed, unless
`sandbox.allow_unsandboxed_fallback=true` is set explicitly; a
missing seccomp filter is likewise refused unless
`sandbox.allow_without_seccomp=true` is set.

With the sandbox **off**, the shell denylist's documented bypass
classes are open (F-A23W2-16): the denylist is a static, string-level
check — it cannot see a path that reaches a sensitive file through
(1) an operand whose variable cannot be resolved statically
(`cat $X/.orion/config.json` with `$X` unassigned), (2) a symlink
alias (`ln -s ~/.orion /tmp/l; cat /tmp/l/config.json`), (3) a path
built by string concatenation inside an interpreter, or (4) a file
read through an already-open file descriptor (`cat <&3`). The
sandbox was the only layer containing those four classes; with it
off, nothing else stops a shell command from reading
`~/.orion/config.json` (Telegram token, API keys), `~/.ssh`, or
`~/.gnupg`. An opted-out daemon therefore prints a LOUD startup
warning naming all four classes, and every unsandboxed `shell_exec`
is still logged per command. The limits are deliberate and
documented in `internal/security/security.go` — the fix is the
warning, not a wider denylist: re-enable the sandbox
(`sandbox.enabled=true`, remove `ORION_SANDBOX=off`) to close them.

The sandbox needs a host that lets it create Linux user namespaces:
it works when the agent runs as root, or on a host that allows
unprivileged user namespaces. A plain unprivileged user on a host
without that capability cannot run the sandbox at all — `shell_exec`
then refuses every command (fail-closed), and `orion config show`
reports `sandbox_probe: unavailable` with the reason. Orion probes
both namespace orderings at startup (user namespace created after
the `/proc` mount first, then at clone time) and uses whichever
works; if neither works the sandbox stays unavailable until the
host allows user namespaces (or the agent runs as root), and the
probe result is cached for the process lifetime, so a daemon restart
is needed to re-probe after fixing the host. See [SECURITY.md](SECURITY.md)
for the full reasoning and the fail-closed rules.

## MCP servers (confinement)

MCP servers configured in `<data_dir>/mcp.json` (e.g. `~/.orion/mcp.json`,
never read from the repo) run **confined by default**, under the same
sandbox `shell_exec` uses: new mount/PID/IPC/UTS namespaces, a
pivot_root into a chroot holding only `/usr` (read-only) and the
sandbox work dir, and the `orion-seccomp` filter. A sandboxed server
cannot read `~/.orion/config.json` (the provider keys, the Telegram
token), `~/.ssh`, or the home directory — `HOME` is `/work` and
`TMPDIR` is `/tmp` inside the sandbox, and its environment is
scrubbed like `shell_exec`'s. Confinement is fail-closed: when the
sandbox is unavailable, the server binary is not visible inside the
chroot, or the seccomp launcher is missing, the server is refused at
start — it never silently runs unsandboxed.

Consequences the operator must know:

- The server binary must live under `/usr` (or a path that resolves
  there, e.g. `/bin`, `/sbin` — the merged-`/usr` symlinks). A binary
  in `~/.local/bin`, `/opt`, or anywhere else is refused at start.
- MCP servers keep the host network namespace: they are network-class
  by design (the permission gate treats every `mcp__<server>__<tool>`
  as network, so it needs approval on Telegram), and API-backed
  servers cannot function without egress. The network does not help
  exfiltrate what the server cannot read — the config, keys, and home
  directory are outside the chroot.
- Opt-out is per server and explicit: `"sandbox": false` in `mcp.json`
  runs that server fully trusted, like a hook — it can read the
  agent's config and home directory. The daemon logs the opt-out on
  start.
- Per-server work dirs (Audit 6, F-A6-P2-14, fixed in rc11): each
  sandboxed MCP server gets its own work dir
  (`<data_dir>/sandbox-mcp-<name>/work`, i.e. its own `/work`) instead
  of sharing `shell_exec`'s `<data_dir>/sandbox/work`, so a server can
  no longer read files a shell turn left in `/work` or plant files a
  later shell turn might run. Do not leave secrets in a server's
  `/work`; see [SECURITY.md](SECURITY.md).

## Per-chat budget and USD cost

One Telegram chat cannot spend the daemon's whole allowance. Each
chat gets its own daily token budget — default **100,000 tokens per
UTC day** (`per_chat_daily_token_budget` in `config.json`, 0 =
unlimited, 1,000–10,000,000) — enforced in `runChatTurn`
(`internal/app`): a pre-flight check refuses a turn whose projected
input would exceed the remaining budget, a mid-turn check aborts
after a provider response that crosses it (it reads the main
agent's usage; subagent spend is charged at turn end by the sweep
— Audit 6, F-A6-P2-6), and the turn's spend is
recorded even when the turn fails (so an over-budget turn is logged,
and the *next* turn is the one refused). Spend is persisted to
`<data_dir>/budget.json`, so a daemon restart does not reset any
chat's daily total; yesterday's spend never counts against today.
A usage alert threshold (`daily_budget_alert_threshold_tokens` in
`config.json`, 0 = disabled, 1–10,000,000) notifies the owner once
per chat per day via Telegram when that chat's usage reaches it —
an absolute token count, so it works with an unlimited budget too,
where it watches usage that nothing refuses. The "alerted today"
flag is persisted in `budget.json` alongside the usage, so a
restart never re-alerts for the same day. Side-channel provider
calls (selector, extraction, auto-compact) and finished-but-unfetched
subagent tasks are charged to the owning
chat at turn end, so they count against the same budget. Metering of
`/compress` itself landed in rc11 (Audit 6, F-A6-1 — no production
caller passed the metering sink on the `/compress` path in rc10);
Audit 7 verified it live (the 218-token `/compress` summarisation
recorded in `budget.json`). The remaining gap — `/compress` is
metered but never refused at the daily limit (Audit 7, F-A7-1) —
is rc12 scope. See [SECURITY.md](SECURITY.md) for the enforcement
points and the known metering gaps.

`/usage` shows the chat's day: tokens today with the
prompt/completion/subagent split, the daily budget (limit,
remaining, UTC-midnight reset), and an **estimated USD cost**
(`internal/cost`, price table v2026-11-13, effective 2026-11-13 —
standard on-demand list prices, USD per 1M tokens). The figure is
an estimate, labelled as such wherever it appears: cache hits,
peak/off-peak rates, batch and enterprise rates are not reflected.
Each (provider, model) bucket is priced at its own model's rate, so
a morning on `deepseek-chat` is never repriced at an afternoon
switch to another model. A model with no price in the table reports
its cost as "unknown", never $0.

## Sessions

Every conversation is kept as an append-only per-session JSONL
transcript — one file per session, never rewritten:

- Telegram: `<data_dir>/chats/<chatID>/sessions/<session-id>.jsonl`
- CLI: `<data_dir>/sessions/<session-id>.jsonl`

The live session id is recorded in `sessions/current.json` under
the same root. `/new` starts a fresh session (the old transcript
stays on disk); `/sessions` lists sessions and `/resume <id or
unique prefix>` resumes one, reloading its messages into the
working history. The CLI has the same operations:
`orion sessions` (list), `orion sessions resume <id>`,
`orion sessions new`. A chat created before transcripts existed
has its turns from the legacy `memory.json` imported once into
the transcript the first time it is opened — the JSON file itself
is never deleted or modified by the migration.

## CLI

| Command | What it does |
|---|---|
| `orion run "prompt"` | One agent run, print the reply |
| `orion chat` | Interactive CLI chat |
| `orion config show / set-provider / set-model` | Inspect and change config |
| `orion status` | Provider, tools, memory, skills, Telegram status |
| `orion tools` / `orion tools exec <name> '<json>'` | List / run a tool |
| `orion memory list/add/get/search` | Long-term facts |
| `orion skills` / `orion skills search <q>` | Loaded skills (`~/.orion/skills/*.md`) |
| `orion subagent run "brief"` | Run a brief as a managed child agent |
| `orion agents` | List subagent tasks with status, duration, tokens and result summary (same view as Telegram `/agents`) |
| `orion sessions` / `orion sessions list` | List CLI sessions (transcripts, newest first) |
| `orion sessions resume <id\|prefix>` | Resume a CLI session: its transcript becomes the working history |
| `orion sessions new` | Start a fresh CLI session (old transcript stays saved) |
| `orion telegram test/poll` | Verify token / start gateway (turn-only — no slash commands; see above) |
| `orion daemon` | Telegram polling daemon (turn-only — no slash commands; see above) |
| `oriond` | Telegram polling daemon with the full slash-command framework (22-command `/` menu) |
| `orion version` | Print version |

## Tools (all wired into the agent)

`shell_exec` (denylist + timeout + 64KB cap; namespace/chroot
sandbox with seccomp filter by default — see [SECURITY.md](SECURITY.md)), `file_read`,
`file_write`, `file_edit`, `file_list`, `grep` (confined content
search — use instead of shell grep, which bypasses confinement),
`glob` (recursive glob patterns), `web_fetch`, `web_search`
(real DuckDuckGo search), `calculator` (full expression parser),
`datetime`, `process_list`, `skill` (invoke a skill from the user
data dir; lazy body load; allowed-tools enforced),
`ask_user_question` (ask the user a question mid-turn and wait for
the answer), `todo_write`
(session checklist), `task_create` / `task_get` / `task_list` /
`task_update` (durable planning tasks, per chat),
`enter_plan_mode` / `exit_plan_mode` (present a plan and work only
after the user approves it / clear a pending plan), `cron_schedule` /
`cron_list` / `cron_cancel` / `cron_enable` (recurring scheduled turns in this chat,
5-field cron expressions, times UTC; cron_enable re-enables a disabled job), `screenshot` (capture the
desktop and return the image to the model; read-only, no input;
refuses on sensitive windows and in unattended turns), `ground` (ground the screen: numbered-marker
screenshot plus the exact name/role/bounding box of every interactive element; read-only; stale
snapshots are refused), `input` (move the cursor, click, type or press a key on the desktop; gated as
destructive; refuses on sensitive windows and in unattended turns), `subagent_spawn`
(run a brief as a managed child agent), `subagent_list`, `subagent_result`,
`subagent_stop`. MCP servers configured in the data dir add
`mcp__<server>__<tool>` tools dynamically. Every tool result is capped at
`max_tool_result_bytes` (default 256 KiB) before it reaches the model;
see "Size caps" under [Configuration](#configuration).

## Safety

See [SECURITY.md](SECURITY.md). The short version: one canonical
token-based denylist (`internal/security.IsBlockedCommand`), secret
scanning/redaction, a SHA-256 hash-chained audit log, and a sandbox
for `shell_exec`, on by default (Linux namespaces + chroot), that confines
commands to a sandbox root even when the denylist cannot see what
interpreter code or a script file will do. Tests never execute
destructive commands — denylist tests call
`IsBlockedCommand` with strings only; sandbox escape tests target
files inside `t.TempDir()` only.

## Layout

See [ARCHITECTURE.md](ARCHITECTURE.md).

## License

MIT — see [LICENSE](LICENSE). The repository is public
(https://github.com/irfan420x/orion-agent); the MIT license permits
use, copying, modification, and distribution, including commercial
use, under its terms.
