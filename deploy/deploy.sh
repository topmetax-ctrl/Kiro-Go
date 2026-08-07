#!/bin/bash
# ──────────────────────────────────────────────────────────────────
# Kiro-Go Deployment Script
# Supports: Docker (default) and macOS native
# ──────────────────────────────────────────────────────────────────
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
cd "$PROJECT_DIR"

MODE="${1:-docker}"

case "$MODE" in
  docker|d)
    echo "=== Deploying Kiro-Go (Docker) ==="
    docker compose build
    docker compose up -d
    echo ""
    echo "✅ Kiro-Go running on http://localhost:8080"
    echo "   Admin panel: http://localhost:8080/admin"
    echo "   Logs: docker logs kiro-go-kiro-go-1 -f"
    ;;

  native|n|mac|macos)
    echo "=== Deploying Kiro-Go (macOS Native) ==="

    # Build for macOS if binary doesn't exist
    if [ ! -f ./kiro-go-darwin ]; then
      echo "Building macOS binary..."
      docker run --rm -v "$PWD":/app -w /app golang:1.24-alpine sh -c \
        "CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -o kiro-go-darwin ."
    fi

    # Stop Docker if running (port conflict)
    docker compose stop 2>/dev/null || true

    # Kill existing native instance
    pkill -f kiro-go-darwin 2>/dev/null || true
    sleep 1

    # Start
    export CONFIG_PATH="$PROJECT_DIR/data/config.json"
    export KIRO_INJECT_CACHE_DIR="$HOME/.aws/sso/cache"
    export LOOPBACK_HOST="0.0.0.0"

    mkdir -p "$PROJECT_DIR/logs"
    nohup ./kiro-go-darwin \
      >> "$PROJECT_DIR/logs/stdout.log" 2>> "$PROJECT_DIR/logs/stderr.log" &

    sleep 2

    if pgrep -f kiro-go-darwin > /dev/null; then
      echo ""
      echo "✅ Kiro-Go (macOS native) running on http://localhost:8080"
      echo "   PID: $(pgrep -f kiro-go-darwin)"
      echo "   TCP stack: macOS Darwin (native — no Linux VM fingerprint)"
      echo "   TLS: uTLS Safari fingerprint"
      echo "   Logs: tail -f $PROJECT_DIR/logs/stdout.log"
    else
      echo "❌ Failed to start"
      echo "Check logs: $PROJECT_DIR/logs/stderr.log"
      exit 1
    fi
    ;;

  terminator)
    echo "=== Deploying Kiro-Go (Docker) + TLS Terminator (Mac) ==="

    # Start TLS terminator on Mac
    if [ -d "$PROJECT_DIR/.venv" ]; then
      source "$PROJECT_DIR/.venv/bin/activate"
    else
      echo "❌ .venv not found. Run: python3 -m venv .venv && source .venv/bin/activate && pip install curl_cffi"
      exit 1
    fi

    pkill -f kiro_tls_terminator 2>/dev/null || true
    sleep 1

    nohup python3 "$PROJECT_DIR/kiro_tls_terminator.py" --port 8888 \
      >> "$PROJECT_DIR/logs/terminator.log" 2>&1 &
    sleep 2

    if pgrep -f kiro_tls_terminator > /dev/null; then
      echo "✅ TLS Terminator running on localhost:8888"
      echo "   Chrome BoringSSL + macOS TCP stack"
      echo "   Configure account proxyURL: http://host.docker.internal:8888"
    fi

    # Start Docker
    docker compose build
    docker compose up -d
    echo ""
    echo "✅ Kiro-Go (Docker) running on http://localhost:8080"
    echo "   Route accounts through terminator for Chrome TLS + macOS TCP"
    ;;

  stop)
    echo "=== Stopping Kiro-Go ==="
    docker compose stop 2>/dev/null || true
    pkill -f kiro-go-darwin 2>/dev/null || true
    pkill -f kiro_tls_terminator 2>/dev/null || true
    echo "✅ Stopped"
    ;;

  *)
    echo "Usage: $0 {docker|native|terminator|stop}"
    echo ""
    echo "  docker     - Deploy in Docker (Linux VM, uTLS Safari TLS)"
    echo "  native     - Deploy natively on macOS (macOS TCP, uTLS Safari TLS)"
    echo "  terminator - Docker + Mac TLS Terminator (macOS TCP, Chrome BoringSSL)"
    echo "  stop       - Stop all instances"
    echo ""
    echo "Recommended: 'native' for best anti-detection"
    echo "             'terminator' for best anti-detection + Docker isolation"
    exit 1
    ;;
esac
