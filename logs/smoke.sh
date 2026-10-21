#!/bin/bash
# Phase 5 production smoke harness. Sends one small non-stream request through
# the OpenCode-only route (single enabled connection) and prints the HTTP status,
# any Kiro-Session response header, and the session telemetry the gateway
# recorded for it. Never prints the session id itself or any credential.
#
# usage: smoke.sh <label> [curl args...]
set -u
BASE=http://localhost:8080
KEY="$ANTHROPIC_AUTH_TOKEN"
LABEL="$1"; shift

HDR=$(mktemp)
BODY=$(mktemp)
code=$(curl -s -o "$BODY" -D "$HDR" -w '%{http_code}' \
  -X POST "$BASE/v1/messages" \
  -H "x-api-key: $KEY" \
  -H "content-type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  "$@" \
  -d '{"model":"deepseek-v4.1-flash","max_tokens":16,"messages":[{"role":"user","content":"Reply with the single word: ok"}]}')

ks=$(grep -i '^kiro-session:' "$HDR" | tr -d '\r' | awk '{print $2}')
echo "== $LABEL"
echo "   http_status = $code"
if [ -n "$ks" ]; then
  echo "   kiro-session response header = PRESENT (len=${#ks}, value withheld)"
  echo "$ks" > "/tmp/kiro_session_$LABEL"
else
  echo "   kiro-session response header = absent"
fi
if [ "$code" != "200" ]; then
  echo "   error body (first 200 chars): $(head -c 200 "$BODY")"
fi
rm -f "$HDR" "$BODY"
