#!/bin/bash
# Detached cutover: native kiro-go-darwin (port 8080) -> Docker compose.
# Runs independent of the Claude Code session, which itself routes through 8080
# and will briefly lose connectivity while 8080 changes owner.
set -x
cd /Users/macdev/Demo/Kiro-Go || exit 1
LOG=/Users/macdev/Demo/Kiro-Go/logs/cutover.log
mkdir -p logs
exec >>"$LOG" 2>&1
echo "================ CUTOVER $(date) ================"

# 1. Backup config (accounts) before touching anything.
cp -f data/config.json "data/config.json.pre-cutover.$(date +%s).bak"

# 2. Graceful stop of native binary so it flushes config.json cleanly.
pkill -TERM -f kiro-go-darwin
for i in $(seq 1 15); do
  pgrep -f kiro-go-darwin >/dev/null 2>&1 || break
  sleep 1
done
# Force-kill any survivor (it is not launchd-managed, so it will not respawn).
pkill -KILL -f kiro-go-darwin 2>/dev/null

# 3. Wait for port 8080 to be released.
for i in $(seq 1 20); do
  lsof -iTCP:8080 -sTCP:LISTEN >/dev/null 2>&1 || break
  sleep 1
done

# 4. Bring up Docker stack (image already built).
docker compose up -d

# 5. Verify: container state + HTTP reachability of the admin/proxy port.
sleep 6
docker compose ps
echo "--- HTTP 8080 probe ---"
for i in $(seq 1 20); do
  code=$(curl -s -o /dev/null -w "%{http_code}" http://localhost:8080/ 2>/dev/null)
  echo "attempt $i -> HTTP $code"
  [ "$code" != "000" ] && break
  sleep 2
done
echo "================ CUTOVER DONE $(date) ================"
