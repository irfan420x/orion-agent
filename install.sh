#!/usr/bin/env bash
# install.sh — build and install Orion (orion CLI + oriond daemon).
#
#   ./install.sh                 install to /usr/local/bin if writable,
#                                otherwise ~/.local/bin
#   ./install.sh --prefix DIR    install binaries to DIR/bin
#   ./install.sh --systemd       also install the oriond systemd user unit
#                                (and enable it, if systemd --user works)
#   ./install.sh --help          show usage
#
# Idempotent: re-running rebuilds and reinstalls the binaries, and
# never overwrites an existing ~/.orion/config.json. The default
# config is written only on first install, with no secrets in it —
# add your provider API key / Telegram token afterwards (see README).
#
# Build info (VCS commit + build date) is baked into the binaries
# with -ldflags -X, so `orion version` reports the real build.
set -euo pipefail

PROG="install.sh"
PREFIX=""
WITH_SYSTEMD=0

usage() {
	cat <<EOF
Usage: $PROG [--prefix DIR] [--systemd] [--help]

Build Orion from source and install the binaries.

Options:
  --prefix DIR   install binaries to DIR/bin (default: /usr/local/bin
                 when writable, otherwise ~/.local/bin)
  --systemd      install the oriond systemd user unit
                 (~/.config/systemd/user/oriond.service) and enable it
  --help, -h     show this help

The default config is written to ~/.orion/config.json only if no
config exists there yet; an existing config is never overwritten.
EOF
}

die() {
	echo "$PROG: error: $*" >&2
	exit 1
}

while [ $# -gt 0 ]; do
	case "$1" in
	--prefix)
		[ $# -ge 2 ] || die "--prefix needs a directory argument"
		PREFIX="$2"
		shift 2
		;;
	--prefix=*)
		PREFIX="${1#--prefix=}"
		shift
		;;
	--systemd)
		WITH_SYSTEMD=1
		shift
		;;
	--help | -h)
		usage
		exit 0
		;;
	*)
		die "unknown argument: $1 (try --help)"
		;;
	esac
done

# The repo root is the directory this script lives in, so the script
# works when run from the repo root (./install.sh) or from anywhere
# else by absolute path.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$REPO_ROOT"

[ -f go.mod ] || die "go.mod not found in $REPO_ROOT — run this script from the Orion repo"
[ -d cmd/orion ] || die "cmd/orion not found in $REPO_ROOT — this does not look like the Orion repo"

# Go toolchain: prefer one on PATH, then the per-user installs this
# VM/container setup is known to use. /usr/local/go does not survive
# restores here, so it is only a last resort.
GO=""
if command -v go >/dev/null 2>&1; then
	GO="$(command -v go)"
elif [ -x "$HOME/go1.27.4/bin/go" ]; then
	GO="$HOME/go1.27.4/bin/go"
elif [ -x /usr/local/go/bin/go ]; then
	GO=/usr/local/go/bin/go
else
	die "go toolchain not found (need Go 1.27+). Install Go and put it on PATH, e.g. ~/go1.27.4/bin/go"
fi

# Build info for `orion version`. Outside a git checkout the commit
# is "unknown" and version.String() leaves it out.
if command -v git >/dev/null 2>&1 && COMMIT="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null)"; then
	:
else
	COMMIT="unknown"
fi
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
LDFLAGS="-X github.com/irfan/orion/internal/version.Commit=${COMMIT} -X github.com/irfan/orion/internal/version.BuildDate=${BUILD_DATE}"

# Install target: an explicit --prefix wins; otherwise /usr/local/bin
# when writable, else ~/.local/bin (no sudo needed, on PATH in most
# setups once ~/.local/bin is added — see the note printed below).
if [ -n "$PREFIX" ]; then
	BIN_DIR="${PREFIX%/}/bin"
elif [ -w /usr/local/bin ]; then
	BIN_DIR="/usr/local/bin"
else
	BIN_DIR="$HOME/.local/bin"
fi
mkdir -p "$BIN_DIR" || die "cannot create $BIN_DIR"

echo "building orion and oriond (commit ${COMMIT}, built ${BUILD_DATE})..."
"$GO" build -trimpath -ldflags "$LDFLAGS" -o "$BIN_DIR/orion" ./cmd/orion
if [ -d cmd/oriond ]; then
	"$GO" build -trimpath -ldflags "$LDFLAGS" -o "$BIN_DIR/oriond" ./cmd/oriond
	HAS_ORIOND=1
else
	echo "note: cmd/oriond not present — installing orion only"
	HAS_ORIOND=0
fi
chmod 0755 "$BIN_DIR/orion"
[ "$HAS_ORIOND" -eq 1 ] && chmod 0755 "$BIN_DIR/oriond"

# Default config, first install only. Never overwrite an existing
# config: it may hold API keys and a Telegram token. This matches
# config.Default() (default provider ollama, local-first) and is
# written 0600. Secrets are added afterwards by the user, never here.
CONFIG_DIR="$HOME/.orion"
CONFIG_PATH="$CONFIG_DIR/config.json"
if [ -f "$CONFIG_PATH" ]; then
	echo "config already exists at $CONFIG_PATH — leaving it untouched"
else
	mkdir -p "$CONFIG_DIR"
	chmod 0700 "$CONFIG_DIR"
	cat >"$CONFIG_PATH" <<EOF
{
  "default_provider": "ollama",
  "providers": {
    "openai": {
      "name": "openai",
      "api_key": "",
      "base_url": "",
      "model": "gpt-4o-mini"
    },
    "anthropic": {
      "name": "anthropic",
      "api_key": "",
      "base_url": "",
      "model": "claude-sonnet-4-5"
    },
    "ollama": {
      "name": "ollama",
      "api_key": "",
      "base_url": "http://localhost:11434",
      "model": "llama3.1"
    }
  },
  "model": "",
  "telegram_token": "",
  "telegram_allowed_chats": [],
  "data_dir": "$HOME/.orion",
  "max_steps": 20,
  "timeout_seconds": 120
}
EOF
	chmod 0600 "$CONFIG_PATH"
	echo "wrote default config to $CONFIG_PATH (edit it or use 'orion config' to add keys)"
fi

# Sanity check: the installed binary must at least report its version
# and read the config we just installed (or left alone).
"$BIN_DIR/orion" version
"$BIN_DIR/orion" config show >/dev/null || die "installed orion cannot read $CONFIG_PATH"

# Optional systemd user unit for the oriond daemon.
if [ "$WITH_SYSTEMD" -eq 1 ]; then
	[ "$HAS_ORIOND" -eq 1 ] || die "--systemd needs oriond, but cmd/oriond is not present in this repo"
	[ -f "$REPO_ROOT/oriond.service" ] || die "oriond.service not found in $REPO_ROOT"
	UNIT_DIR="$HOME/.config/systemd/user"
	mkdir -p "$UNIT_DIR"
	# Point ExecStart at the binary we actually installed (the unit
	# file defaults to ~/.local/bin/oriond).
	sed "s|^ExecStart=.*|ExecStart=$BIN_DIR/oriond|" "$REPO_ROOT/oriond.service" >"$UNIT_DIR/oriond.service"
	chmod 0644 "$UNIT_DIR/oriond.service"
	echo "installed systemd user unit to $UNIT_DIR/oriond.service"
	if command -v systemctl >/dev/null 2>&1; then
		if systemctl --user daemon-reload 2>/dev/null && systemctl --user enable oriond.service 2>/dev/null; then
			echo "oriond.service enabled — start it with: systemctl --user start oriond"
		else
			echo "warning: could not reload/enable the user systemd instance (no user session?)" >&2
			echo "warning: run manually once logged in: systemctl --user daemon-reload && systemctl --user enable --now oriond" >&2
		fi
	else
		echo "warning: systemctl not found — unit installed but not enabled; enable it on a systemd host" >&2
	fi
fi

echo
echo "installed:"
echo "  $BIN_DIR/orion"
[ "$HAS_ORIOND" -eq 1 ] && echo "  $BIN_DIR/oriond"
echo "config: $CONFIG_PATH"
case ":$PATH:" in
*":$BIN_DIR:"*) ;;
*)
	echo "note: $BIN_DIR is not on your PATH — add it, e.g.: export PATH=\"\$PATH:$BIN_DIR\""
	;;
esac
echo "next: orion config set-api-key <openai|anthropic> <key>   (or run Ollama locally)"
echo "      orion run \"what time is it?\""
echo "      orion telegram test   (after setting a Telegram bot token)"
