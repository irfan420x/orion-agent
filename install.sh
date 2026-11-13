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
# Build info (version from the exact-match git tag, VCS commit +
# build date) is baked into the binaries with -ldflags -X, so
# `orion version` reports the real build. When HEAD is not exactly
# on a tag, no -X Version is passed and the binary reports the
# source default in internal/version/version.go (the release this
# tree is working towards) — never "dev", never a stale tag
# (F-A6-P2-16: the rc10 binary reported rc9 because Version was a
# const ldflags could not override).
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
	die "go toolchain not found (need Go 1.27+). Install Go from https://go.dev/dl and put it on PATH, e.g. ~/go1.27.4/bin/go"
fi

# Build info for `orion version`. Outside a git checkout the commit
# is "unknown" and version.String() leaves it out.
if command -v git >/dev/null 2>&1 && COMMIT="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null)"; then
	:
else
	COMMIT="unknown"
fi
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# Version from the exact-match tag (v0.2.0-rc11 -> 0.2.0-rc11). On an
# untagged commit TAG_VERSION is empty and Version is left at its
# source default (see internal/version/version.go), so the installed
# binary still reports a sane version.
TAG_VERSION=""
if command -v git >/dev/null 2>&1; then
	TAG_VERSION="$(git -C "$REPO_ROOT" describe --tags --exact-match 2>/dev/null | sed -E 's/^v//')" || TAG_VERSION=""
fi
LDFLAGS="-X github.com/irfan/orion/internal/version.Commit=${COMMIT} -X github.com/irfan/orion/internal/version.BuildDate=${BUILD_DATE}"
if [ -n "$TAG_VERSION" ]; then
	LDFLAGS="$LDFLAGS -X github.com/irfan/orion/internal/version.Version=${TAG_VERSION}"
fi

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
# Stale staging temps from a SIGKILLed earlier run (SIGKILL cannot run
# the EXIT trap, so a killed build can leave a 0-byte .install-* file
# behind). They are inert, but clean them so interrupted installs do
# not accumulate junk in the bin dir. Installs are not run
# concurrently; a concurrent install's live temp file would match
# this pattern, so never run two installs at once.
rm -f "$BIN_DIR"/.install-*

# Atomic install: every file this script writes to its final location
# goes through a temp file in the same directory + rename, never a
# direct write. A direct `go build -o $BIN_DIR/orion` (or `cat >
# config.json`) leaves a half-written binary / torn config behind if
# the install is killed mid-write — and a torn config.json is never
# repaired, because the "already exists, leave it untouched" check
# below sees the file and skips it. With temp+rename, a kill at any
# point leaves either the old complete file or no file at all, and a
# re-run always completes. (The three binaries are still installed
# one after another, so a kill between them can leave a mixed-version
# set — each binary is whole, but orion/oriond may briefly be from
# different builds. Fully atomic multi-binary swaps would need a
# staged directory + symlink flip, which is out of scope here.)
#
# install_file SRC DEST moves a finished file into place atomically
# (same filesystem: temp files are created in the destination dir)
# and makes it executable. Dies on failure.
install_file() {
	# $1 = temp file, $2 = destination
	mv -f "$1" "$2" || die "cannot install $2"
	chmod 0755 "$2" || die "cannot chmod $2"
}

# Staging temp files, removed on any exit (a SIGKILL cannot run the
# trap, but a leftover .install-*.tmp in the bin dir is inert and the
# next run overwrites it via mktemp anyway).
STAGE_TMP=""
cleanup_stage() {
	[ -n "$STAGE_TMP" ] && rm -f $STAGE_TMP
	# Always succeed: this runs as the EXIT trap, and a failing trap
	# would override the script's real exit code with 1.
	return 0
}
trap cleanup_stage EXIT

build_one() {
	# $1 = go package, $2 = binary name: build to a temp file in
	# BIN_DIR, then rename into place.
	local pkg="$1" name="$2" tmp
	tmp="$(mktemp "$BIN_DIR/.install-${name}.XXXXXX")" || die "cannot create temp file in $BIN_DIR"
	STAGE_TMP="$STAGE_TMP $tmp"
	"$GO" build -trimpath -ldflags "$LDFLAGS" -o "$tmp" "$pkg" || die "build failed: $pkg"
	install_file "$tmp" "$BIN_DIR/$name"
	STAGE_TMP="${STAGE_TMP% $tmp}"
}

echo "building orion, oriond and orion-seccomp (version ${TAG_VERSION:-$(grep -E '^var Version' internal/version/version.go | sed -E 's/.*"([^"]+)".*/\1/')}, commit ${COMMIT}, built ${BUILD_DATE})..."
build_one ./cmd/orion orion
if [ -d cmd/oriond ]; then
	build_one ./cmd/oriond oriond
	HAS_ORIOND=1
else
	echo "note: cmd/oriond not present — installing orion only"
	HAS_ORIOND=0
fi

# Seccomp launcher: the sandbox's orion-seccomp helper is discovered
# next to the orion binary, so install it alongside when present in
# the tree. The sandbox is ON by default, and a missing launcher
# makes every shell_exec FAIL CLOSED (refused, F-A3-1) unless the
# operator explicitly opted into unfiltered runs with
# sandbox.allow_without_seccomp=true — so a fresh install without
# the launcher ships a broken agent, not a degraded one. The
# launcher is therefore a REQUIRED install, not an optional extra:
# die if its source is absent instead of silently skipping it.
# CGO_ENABLED=0 keeps it static so it runs inside the chroot.
if [ -d internal/sandbox/cmd/launcher ]; then
	CGO_ENABLED=0 build_one ./internal/sandbox/cmd/launcher orion-seccomp
	echo "installed seccomp launcher: $BIN_DIR/orion-seccomp"
else
	die "internal/sandbox/cmd/launcher not present — the sandbox is on by default and needs its launcher; refusing a launcher-less install"
fi

# Default config, first install only. Never overwrite an existing
# config: it may hold API keys and a Telegram token. This matches
# config.Default() (default provider ollama, local-first, sandbox
# ON by default) and is written 0600. Secrets are added afterwards
# by the user, never here.
CONFIG_DIR="$HOME/.orion"
CONFIG_PATH="$CONFIG_DIR/config.json"

# write_default_config writes the first-install default config to
# $CONFIG_PATH via temp file + rename (see the atomic-install note
# above). It never overwrites an existing file — callers check first.
# This matches config.Default() (default provider ollama,
# local-first, sandbox ON by default) and is written 0600. Secrets
# are added afterwards by the user, never here.
write_default_config() {
	mkdir -p "$CONFIG_DIR"
	chmod 0700 "$CONFIG_DIR"
	rm -f "$CONFIG_DIR"/.install-config.*
	# Write via a temp file + rename, like the binaries above: a
	# kill during a direct `cat > config.json` would leave a torn
	# config that this script then refuses to overwrite (the
	# "already exists" branch), so the install would look done
	# while Orion cannot start. mktemp is 0600 already; chmod
	# explicitly so the mode never depends on that.
	# The sandbox block is explicit even though config.Default()
	# enables it anyway: a fresh install must confine shell_exec,
	# and the file should say so instead of relying on the
	# reader knowing the code default. Only an explicit
	# "enabled": false (or ORION_SANDBOX=off) opts out.
	CONFIG_TMP="$(mktemp "$CONFIG_DIR/.install-config.XXXXXX")" || die "cannot create temp config in $CONFIG_DIR"
	STAGE_TMP="$STAGE_TMP $CONFIG_TMP"
	cat >"$CONFIG_TMP" <<EOF
{
  "default_provider": "ollama",
  "providers": {
    "openai": {
      "name": "openai",
      "api_key": "",
      "base_url": "",
      "model": "gpt-4o-mini"
    },
    "deepseek": {
      "name": "deepseek",
      "api_key": "",
      "base_url": "https://api.deepseek.com/v1",
      "model": "deepseek-chat"
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
  "timeout_seconds": 120,
  "sandbox": {
    "enabled": true,
    "root": "$HOME/.orion/sandbox"
  }
}
EOF
	chmod 0600 "$CONFIG_TMP" || die "cannot chmod temp config"
	mv -f "$CONFIG_TMP" "$CONFIG_PATH" || die "cannot install $CONFIG_PATH"
	STAGE_TMP="${STAGE_TMP% $CONFIG_TMP}"
	echo "wrote default config to $CONFIG_PATH (edit it or use 'orion config' to add keys)"
}

# Default config, first install only. Never overwrite an existing
# config: it may hold API keys and a Telegram token.
if [ -f "$CONFIG_PATH" ]; then
	echo "config already exists at $CONFIG_PATH — leaving it untouched"
else
	write_default_config
fi

# Sanity check: the installed binary must at least report its version
# (needs no config, so a broken config cannot fail the install).
"$BIN_DIR/orion" version
if ! "$BIN_DIR/orion" config show >/dev/null 2>&1; then
	# A config that is not valid JSON at all cannot hold usable keys
	# — it is almost certainly a torn write from an interrupted
	# install (pre-atomic installer) or a bad hand-edit. Back it up
	# and replace it with a fresh default, so a re-install recovers
	# instead of leaving Orion unstartable. Any other failure (valid
	# JSON but a key-less default provider, an unknown provider, ...)
	# is left alone: the file may hold secrets and the user must fix
	# it. python3 is only the JSON check; without it, warn as before.
	if command -v python3 >/dev/null 2>&1 && ! python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$CONFIG_PATH" 2>/dev/null; then
		BACKUP="$CONFIG_PATH.corrupt.$(date +%Y%m%d%H%M%S)"
		if mv -f "$CONFIG_PATH" "$BACKUP" 2>/dev/null; then
			echo "warning: $CONFIG_PATH is not valid JSON — moved it to $BACKUP and writing a fresh default config" >&2
			write_default_config
			"$BIN_DIR/orion" config show >/dev/null 2>&1 || echo "warning: fresh default config still unreadable — check $CONFIG_PATH" >&2
		else
			echo "warning: installed orion cannot read $CONFIG_PATH — check the config (e.g. a key-less default provider) before running" >&2
		fi
	else
		echo "warning: installed orion cannot read $CONFIG_PATH — check the config (e.g. a key-less default provider) before running" >&2
	fi
fi

# Optional systemd user unit for the oriond daemon.
if [ "$WITH_SYSTEMD" -eq 1 ]; then
	[ "$HAS_ORIOND" -eq 1 ] || die "--systemd needs oriond, but cmd/oriond is not present in this repo"
	[ -f "$REPO_ROOT/oriond.service" ] || die "oriond.service not found in $REPO_ROOT"
	UNIT_DIR="$HOME/.config/systemd/user"
	mkdir -p "$UNIT_DIR"
	rm -f "$UNIT_DIR"/.install-oriond.*
	# Point ExecStart at the binary we actually installed (the unit
	# file defaults to ~/.local/bin/oriond). Escape sed-special
	# characters in the path first (|, & and backslash). Written via
	# temp+rename like everything else in this script.
	ESCAPED_BIN_DIR="$(printf '%s' "$BIN_DIR" | sed -e 's/[|&\\]/\\&/g')"
	UNIT_TMP="$(mktemp "$UNIT_DIR/.install-oriond.XXXXXX")" || die "cannot create temp unit in $UNIT_DIR"
	STAGE_TMP="$STAGE_TMP $UNIT_TMP"
	sed "s|^ExecStart=.*|ExecStart=$ESCAPED_BIN_DIR/oriond|" "$REPO_ROOT/oriond.service" >"$UNIT_TMP" || die "cannot render oriond.service"
	chmod 0644 "$UNIT_TMP" || die "cannot chmod temp unit"
	mv -f "$UNIT_TMP" "$UNIT_DIR/oriond.service" || die "cannot install $UNIT_DIR/oriond.service"
	STAGE_TMP="${STAGE_TMP% $UNIT_TMP}"
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
[ -x "$BIN_DIR/orion-seccomp" ] && echo "  $BIN_DIR/orion-seccomp"
echo "config: $CONFIG_PATH"
case ":$PATH:" in
*":$BIN_DIR:"*) ;;
*)
	echo "note: $BIN_DIR is not on your PATH — add it, e.g.: export PATH=\"\$PATH:$BIN_DIR\""
	;;
esac
echo "next: orion config set-api-key <openai|anthropic|deepseek> <key>   (or run Ollama locally)"
echo "      orion run \"what time is it?\""
echo "      orion telegram test   (after setting a Telegram bot token)"
echo "      set telegram_allowed_chats in $CONFIG_PATH to your chat ID — oriond refuses to start with an empty allowlist (send /whoami to the bot to learn it)"
