#!/bin/bash
# Audit 3 Workstream 4 — daemon resource sampler.
# Samples /proc/<pid>/status every 5 min and appends to the measurements file.
# Usage: audit3-daemon-sampler.sh <pid|follow> <label|auto> <max_samples>
#   pid:    a fixed daemon PID to sample (original behaviour).
#   follow: resolve the daemon PID dynamically each iteration via
#           `systemctl show oriond -p MainPID --value`, so sampling
#           survives daemon restarts (systemd service mode).
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
# F-A17-7: the shipped deployment is a USER unit
# (~/.config/systemd/user/oriond.service, installed by
# install.sh), so this MUST ask the user manager (--user): the
# bare `systemctl show oriond` queries the system manager, which
# never sees a user unit, and the sampler then spins in its 60s
# sleep-loop forever with an empty PID.
resolve_pid() {
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
      sleep 60
      continue
    fi
  else
    PID="$PID_ARG"
  fi
  LABEL=$(resolve_label)
  if [ ! -d "/proc/$PID" ]; then
    if [ "$FOLLOW" -eq 1 ]; then
      sleep 60
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
  sleep 300
done
echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') [$LABEL] sampler finished $n samples" >> "$OUT"
