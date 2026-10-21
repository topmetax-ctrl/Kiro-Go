#!/bin/bash
# Detached recreate onto the image carrying 824e0e3 (session-mapping hardening +
# cache-stats v2). Runs detached because the live Claude Code session routes
# through port 8080 and will blink while the container is replaced.
#
# The stop -> snapshot -> start ordering is deliberate: graceful shutdown flushes
# a final v1 stats file, so copying it while the port is down captures the exact
# bytes the new binary will Load() and migrate. That backup is the baseline for
# verifying "reset only the ratio triple, keep everything else".
set -x
cd /Users/macdev/Demo/Kiro-Go || exit 1
TS=$(date +%s)
LOG=/Users/macdev/Demo/Kiro-Go/logs/deploy-v2-$TS.log
mkdir -p logs
exec >>"$LOG" 2>&1
echo "================ DEPLOY v2 $(date) ================"

# Let the caller's turn finish before the port goes away.
sleep 4

# 1. Backup config (accounts/API keys) before touching anything.
cp -f data/config.json "data/config.json.pre-v2.$TS.bak"

# 2. Graceful stop: SIGTERM -> handler.Shutdown() flushes stats synchronously,
#    then in-flight requests drain. -t 45 exceeds the 30s drain deadline.
docker compose stop -t 45 kiro-go

# 3. Snapshot the flushed v1 stats file (pre-migration baseline).
cp -f data/forward_stats.json "data/forward_stats.json.pre-v2-load.$TS.bak"
echo "--- pre-load baseline saved: data/forward_stats.json.pre-v2-load.$TS.bak ---"
python3 - <<'PY'
import json
d = json.load(open('data/forward_stats.json'))
o = d['overall']
print('PRELOAD cacheStatsVersion:', d.get('cacheStatsVersion', '<ABSENT>'))
print('PRELOAD overall:', json.dumps({k: o.get(k, 0) for k in (
    'requests','success','failed','canceled','streamed','totalLatencyMs','totalTtfbMs',
    'ttfbCount','inputTokens','outputTokens','costUsd','modelRounds',
    'cacheReadInputTokens','cacheCreationInputTokens','cacheObservedInputTokens',
    'cacheObservedRequests')}))
print('PRELOAD dims: providers=%d routes=%d ips=%d tools=%d' % (
    len(d.get('byProvider', {})), len(d.get('byRoute', {})),
    len(d.get('byIp', {})), len(d.get('toolStats', {}))))
PY

# 4. Start on the new image (--no-deps keeps blast radius to this service).
docker compose up -d --no-deps kiro-go

# 5. Verify container identity + HTTP reachability.
sleep 6
echo "--- container image after recreate ---"
docker inspect -f '{{.Config.Image}} => {{.Image}}' kiro-go-kiro-go-1
docker compose ps
echo "--- HTTP 8080 probe ---"
for i in $(seq 1 30); do
  code=$(curl -s -o /dev/null -w "%{http_code}" http://localhost:8080/ 2>/dev/null)
  echo "attempt $i -> HTTP $code"
  [ "$code" != "000" ] && break
  sleep 2
done
echo "TS=$TS"
echo "================ DEPLOY v2 DONE $(date) ================"
