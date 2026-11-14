#!/bin/bash
# Audit 3 Workstream 4 — daemon resource sampler.
# Samples /proc/<pid>/status every 5 min and appends to the measurements file.
# Usage: audit3-daemon-sampler.sh <pid|follow> <label|auto> <max_samples>
#   pid:    a fixed daemon PID to sample (original behaviour).
#   follow: resolve the daemon PID dynamically each iteration via
#           resolve_pid() (system manager first, user manager as
#           fallback; see below), so sampling survives daemon
#           restarts (systemd service mode).
#   label:  the release label written to the CSV/MD (e.g. rc21).
#   auto:   derive the label each iteration from `bin/orion version`
#           (e.g. "0.2.0-rc21" -> "rc21"); falls back to "unknown".
#   max_samples: stop after this many samples; 0 = run forever
#           (default 24 for fixed-PID mode, 0 for follow mode).
# Fixed-PID mode stops when the PID disappears or max_samples is reached.
# Follow mode never exits on a missing daemon: it waits 60s and retries.
# Output paths are overridable for tests / alternate checkouts
# (F-A17-7): SAMPLER_OUT, SAMPLER_CSV, ORION_BIN — each defaults
# to the $HOME/workspace/orion-go paths below when unset.
# ORION_SYSTEMD_SCOPE (F-A18-3): system|user — pin which systemd
# manager resolve_pid() asks; unset means system-first with a
# user-manager fallback. SAMPLER_SLEEP / SAMPLER_RETRY_SLEEP
# (seconds) override the inter-sample / retry sleeps for tests
# and manual checks; defaults are the production 300s / 60s.
set -u
PID_ARG="${1:?usage: audit3-daemon-sampler.sh <pid|follow> <label|auto> <max_samples>}"
LABEL_ARG="${2:?usage: audit3-daemon-sampler.sh <pid|follow> <label|auto> <max_samples>}"
FOLLOW=0
if [ "$PID_ARG" = "follow" ]; then
  FOLLOW=1
fi
if [ "$FOLLOW" -eq 1 ]; then
  MAX="${3:-0}"
else
  MAX="${3:-24}"
fi
OUT="${SAMPLER_OUT:-$HOME/workspace/orion-go/AUDIT3_DAEMON_MEASUREMENTS.md}"
CSV="${SAMPLER_CSV:-$HOME/workspace/orion-go/AUDIT3_DAEMON_SAMPLES.csv}"
ORION_BIN="${ORION_BIN:-$HOME/workspace/orion-go/bin/orion}"
SAMPLER_SLEEP="${SAMPLER_SLEEP:-300}"
SAMPLER_RETRY_SLEEP="${SAMPLER_RETRY_SLEEP:-60}"

# Derive the release label (e.g. rc21) from the built CLI version string.
# Prints the fixed label unchanged when LABEL_ARG is not "auto".
resolve_label() {
  if [ "$LABEL_ARG" != "auto" ]; then
    printf '%s' "$LABEL_ARG"
    return
  fi
  ver=$("$ORION_BIN" version 2>/dev/null | awk '{print $2}')
  if [ -n "$ver" ]; then
    printf '%s' "$ver" | sed -E 's/^[0-9]+\.[0-9]+\.[0-9]+-//'
  else
    printf 'unknown'
  fi
}

# Resolve the live daemon PID from systemd (follow mode only).
# F-A18-3 (supersedes the F-A17-7 comment this replaces): the
# SHIPPED deployment is a user unit (~/.config/systemd/user/
# oriond.service, installed by install.sh), but the LIVE
# deployment on this VM is a SYSTEM unit (systemd/oriond.service,
# User=root) — and root's user manager has no system units, so
# the old --user-only resolve returned empty and the sampler spun
# in its 60s sleep-loop forever writing nothing (CSV had no rows
# since 03:04:38Z, rc22 PID 25867, while the live daemon was
# PID 2105 rc23). So: try the system manager first, fall back to
# the user manager, and honour ORION_SYSTEMD_SCOPE=system|user
# for explicit control. An invalid ORION_SYSTEMD_SCOPE is
# ignored with a stderr warning (fail open to the default
# system-first order, never fail the sample). A "0" MainPID
# (unit known but not running) is treated as empty so the
# fallback still gets a chance. F-A19-4: that is true for a PINNED
# scope too — a pinned scope returning "0" falls back to the other
# manager, it does not return "0" raw (the old pinned branches
# returned it raw, so the sampler spun in its retry loop even when
# the other manager had a running unit). The pin still controls the
# PREFERRED order.
resolve_pid() {
  case "${ORION_SYSTEMD_SCOPE:-}" in
    system)
      pid=$(systemctl show oriond -p MainPID --value 2>/dev/null | tr -d ' ')
      if [ -n "$pid" ] && [ "$pid" != "0" ]; then
        printf '%s' "$pid"
        return
      fi
      systemctl --user show oriond -p MainPID --value 2>/dev/null | tr -d ' '
      return
      ;;
    user)
      pid=$(systemctl --user show oriond -p MainPID --value 2>/dev/null | tr -d ' ')
      if [ -n "$pid" ] && [ "$pid" != "0" ]; then
        printf '%s' "$pid"
        return
      fi
      systemctl show oriond -p MainPID --value 2>/dev/null | tr -d ' '
      return
      ;;
    "")
      ;;
    *)
      echo "sampler: ignoring invalid ORION_SYSTEMD_SCOPE='${ORION_SYSTEMD_SCOPE}' (want system|user)" >&2
      ;;
  esac
  pid=$(systemctl show oriond -p MainPID --value 2>/dev/null | tr -d ' ')
  if [ -n "$pid" ] && [ "$pid" != "0" ]; then
    printf '%s' "$pid"
    return
  fi
  systemctl --user show oriond -p MainPID --value 2>/dev/null | tr -d ' '
}

n=0
while :; do
  if [ "$MAX" -ne 0 ] && [ "$n" -ge "$MAX" ]; then
    break
  fi
  if [ "$FOLLOW" -eq 1 ]; then
    PID=$(resolve_pid)
    if [ -z "$PID" ] || [ "$PID" = "0" ]; then
      sleep "$SAMPLER_RETRY_SLEEP"
      continue
    fi
  else
    PID="$PID_ARG"
  fi
  LABEL=$(resolve_label)
  if [ ! -d "/proc/$PID" ]; then
    if [ "$FOLLOW" -eq 1 ]; then
      sleep "$SAMPLER_RETRY_SLEEP"
      continue
    fi
    echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') [$LABEL] pid $PID gone after $n samples" >> "$OUT"
    exit 0
  fi
  rss=$(awk '/VmRSS/{print $2}' /proc/$PID/status)
  hwm=$(awk '/VmHWM/{print $2}' /proc/$PID/status)
  vms=$(awk '/VmSize/{print $2}' /proc/$PID/status)
  th=$(awk '/Threads/{print $2}' /proc/$PID/status)
  fds=$(ls /proc/$PID/fd 2>/dev/null | wc -l)
  cpu=$(awk '{print "utime="$14" stime="$15}' /proc/$PID/stat)
  io_r=$(awk '$1=="read_bytes:"{print $2}' /proc/$PID/io 2>/dev/null)
  io_w=$(awk '$1=="write_bytes:"{print $2}' /proc/$PID/io 2>/dev/null)
  el=$(ps -o etime= -p "$PID" 2>/dev/null | tr -d ' ')
  ts=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
  sgt=$(TZ=Asia/Singapore date '+%H:%M:%S')
  line="| $ts | $sgt | $LABEL | $PID | $el | $rss | $hwm | $vms | $th | $fds | $cpu | $io_r | $io_w |"
  echo "$line" >> "$OUT"
  echo "$ts,$LABEL,$PID,$el,$rss,$hwm,$vms,$th,$fds,$io_r,$io_w" >> "$CSV"
  n=$((n+1))
  sleep "$SAMPLER_SLEEP"
done
echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') [$LABEL] sampler finished $n samples" >> "$OUT"
