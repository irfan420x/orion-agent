#!/bin/bash
# Audit 3 Workstream 4 — daemon resource sampler.
# Samples /proc/<pid>/status every 5 min and appends to the measurements file.
# Usage: audit3-daemon-sampler.sh <pid> <label> <max_samples>
# Stops when the PID disappears or max_samples is reached.
set -u
PID="$1"
LABEL="$2"
MAX="${3:-24}"
OUT="$HOME/workspace/orion-go/AUDIT3_DAEMON_MEASUREMENTS.md"
CSV="$HOME/workspace/orion-go/AUDIT3_DAEMON_SAMPLES.csv"
n=0
while [ "$n" -lt "$MAX" ]; do
  if [ ! -d "/proc/$PID" ]; then
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
