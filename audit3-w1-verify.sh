#!/bin/bash
export PATH=$PATH:/home/hatch/go1.27.4/bin
cd ~/workspace/orion-go
LOG=/home/hatch/workspace/orion-go/audit3-w1-verify.log
echo "=== Audit3 W1 verification started $(date -u) ===" > "$LOG"
# 1. Wait for a working window, then run the security suite (validates RedactSecrets change).
for i in $(seq 1 30); do
  if go test -count=1 ./internal/security/ >> "$LOG" 2>&1; then echo "SECURITY SUITE: PASS (attempt $i)" >> "$LOG"; break; else echo "security attempt $i failed at $(date -u)" >> "$LOG"; sleep 20; fi
done
# 2. Race run of the Audit3 core tests (budget test + harness + empty-response regression).
for i in $(seq 1 30); do
  if go test -count=1 -race -run 'TestAudit3' -v ./internal/core/ >> "$LOG" 2>&1; then echo "CORE AUDIT3 RACE: PASS (attempt $i)" >> "$LOG"; break; else echo "core-audit3-race attempt $i failed at $(date -u)" >> "$LOG"; sleep 20; fi
done
# 3. Full core suite.
for i in $(seq 1 30); do
  if go test -count=1 ./internal/core/ >> "$LOG" 2>&1; then echo "CORE SUITE: PASS (attempt $i)" >> "$LOG"; break; else echo "core attempt $i failed at $(date -u)" >> "$LOG"; sleep 20; fi
done
# 4. Full repo suite (other workers' in-flight files may fail; record only).
go test -count=1 ./... >> "$LOG" 2>&1; echo "FULL SUITE exit: $?" >> "$LOG"
# 5. Live DeepSeek comparison (a handful of calls).
go test -count=1 -tags live -run 'TestAudit3LiveDeepSeek' -v ./internal/core/ >> "$LOG" 2>&1; echo "LIVE exit: $?" >> "$LOG"
echo "=== done $(date -u) ===" >> "$LOG"
