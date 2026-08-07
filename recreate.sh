#!/bin/bash
# Detached Docker->Docker recreate: swap the running kiro-go container onto the
# freshly built image (which carries the clientId inject fix). The live Claude
# Code session routes through port 8080 and may blink while the container is
# recreated, so this runs detached to complete regardless.
set -x
cd /Users/macdev/Demo/Kiro-Go || exit 1
LOG=/Users/macdev/Demo/Kiro-Go/logs/recreate.log
mkdir -p logs
exec >>"$LOG" 2>&1
echo "================ RECREATE $(date) ================"

# Backup config (accounts) before touching anything.
cp -f data/config.json "data/config.json.pre-recreate.$(date +%s).bak"

# Recreate the service onto the new image. compose detects the image change and
# replaces the container; --no-deps limits blast radius to this one service.
docker compose up -d --no-deps kiro-go

# Verify: container is on the new image + HTTP reachability of port 8080.
sleep 6
echo "--- container image after recreate ---"
docker inspect -f '{{.Config.Image}} => {{.Image}}' kiro-go-kiro-go-1
docker compose ps
echo "--- HTTP 8080 probe ---"
for i in $(seq 1 20); do
  code=$(curl -s -o /dev/null -w "%{http_code}" http://localhost:8080/ 2>/dev/null)
  echo "attempt $i -> HTTP $code"
  [ "$code" != "000" ] && break
  sleep 2
done
echo "================ RECREATE DONE $(date) ================"
