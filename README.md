# Orion

Orion is a model-agnostic OS agent that can do everything, with many
subagents — built in Go, local-first, Linux/macOS first.

> **Status: v0.1.** The Go core, agent loop, tools, memory, security,
> Telegram gateway, CLI channel, subagents, skills, and CLI are fully
> built and tested. The TypeScript desktop/web UI is v0.2 only (see
> [LANGUAGE_DECISION.md](LANGUAGE_DECISION.md)).

## Quick start

> **Go toolchain:** Go 1.27.4 lives at `~/go1.27.4/bin/go` (installed
> under home because `/usr/local` does not survive VM/container
> restores here). Use `export PATH=$PATH:/home/hatch/go1.27.4/bin`
> in every shell, or `~/go1.27.4/bin/go` directly. `install.sh`
> finds the toolchain itself (PATH, then `~/go1.27.4/bin/go`).

```sh
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
orion config set-provider ollama        # local, no key (default)
orion config set-provider openai
orion config set-api-key openai <YOUR_OPENAI_API_KEY>
orion config set-provider anthropic
orion config set-api-key anthropic <YOUR_ANTHROPIC_API_KEY>
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
orion telegram test                     # verify the token (getMe)
orion telegram poll                     # start the gateway (foreground)
orion daemon                            # same, via the orion CLI
oriond                                  # standalone daemon binary

While a turn runs, the bot streams a live "💻 terminal" card into
the chat (one message, edited in place): each tool call appears as
a block with its command, file writes as ✍️ lines, repeats collapse
to (×N). Displayed commands are redacted, so secrets never leak
into the chat. The final reply is a structured message in your
language, with a runtime footer (model, tokens, steps, duration)
you can toggle with `/footer`. The `/` menu offers 17 commands
(`/help`, `/new`, `/stop`, `/status`, `/model`, `/usage`, `/undo`,
`/retry`, `/pause`, `/agents`, `/title`, `/compress`, `/save`,
`/restart`, `/whoami`, `/footer`, `/memory`); sessions persist in
`~/.orion/sessions.json`.

# Run oriond as a service (systemd user unit, oriond.service):
./install.sh --systemd                  # installs + enables the unit
systemctl --user start oriond
systemctl --user status oriond
journalctl --user -u oriond -f
```

No install step needed for development — `go build ./...` and
`go test ./...` work from the repo root, and `make build` puts
fresh binaries in `bin/`.

## Tests

14/14 packages, 441 tests passing, 3 skipped (the live Ollama and
live Telegram tests, which need a running Ollama server and a bot
chat respectively), race detector clean — `go test ./... -race
-count=1` passes, verified 2026-11-13 (includes 15 fact-recall
tests). Run the suite with `go test ./...` or `make test`.

## Configuration

`~/.orion/config.json` (0600). Provider selection lives here only:

```json
{
  "default_provider": "ollama",
  "providers": {
    "openai":    { "name": "openai",    "api_key": "", "model": "gpt-4o-mini" },
    "anthropic": { "name": "anthropic", "api_key": "", "model": "claude-sonnet-4-5" },
    "ollama":    { "name": "ollama",    "base_url": "http://localhost:11434", "model": "llama3.1" }
  },
  "telegram_token": "",
  "data_dir": "~/.orion",
  "max_steps": 20,
  "timeout_seconds": 120,
  "sandbox": { "enabled": false, "root": "~/.orion/sandbox", "allow_network": false }
}
```

Environment overrides: `ORION_PROVIDER`, `ORION_API_KEY`, `ORION_MODEL`,
`ORION_TELEGRAM_TOKEN`, `ORION_DATA_DIR`, `ORION_SANDBOX` (`on`/`off`),
`ORION_SANDBOX_ROOT`. Secrets are never printed:
`orion config show` masks them, and the secret scanner redacts them
from logs and audit entries.

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
| `orion telegram test/poll` | Verify token / start gateway |
| `orion daemon` / `oriond` | Telegram polling daemon |
| `orion version` | Print version |

## Tools (all wired into the agent)

`shell_exec` (denylist + timeout + 64KB cap; opt-in namespace/chroot
sandbox via `sandbox.enabled` — see [SECURITY.md](SECURITY.md)), `file_read`,
`file_write`, `file_edit`, `file_list`, `web_fetch`, `web_search`
(real DuckDuckGo search), `calculator` (full expression parser),
`datetime`, `process_list`.

## Safety

See [SECURITY.md](SECURITY.md). The short version: one canonical
token-based denylist (`internal/security.IsBlockedCommand`), secret
scanning/redaction, a SHA-256 hash-chained audit log, and an opt-in
sandbox for `shell_exec` (Linux namespaces + chroot) that confines
commands to a sandbox root even when the denylist cannot see what
interpreter code or a script file will do. Tests never execute
destructive commands — denylist tests call
`IsBlockedCommand` with strings only; sandbox escape tests target
files inside `t.TempDir()` only.

## Layout

See [ARCHITECTURE.md](ARCHITECTURE.md).
