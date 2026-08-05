#!/usr/bin/env python3
"""
Kiro TLS Terminator v2 — HTTP Reverse Proxy using curl-impersonate.

Receives plain HTTP requests from Go proxy, forwards to Kiro using
curl-impersonate with REAL Chrome BoringSSL TLS (bit-identical JA3/JA4).

Architecture:
  Go (plain HTTP) → Terminator → curl_cffi (Chrome BoringSSL) → Kiro HTTPS

This gives:
  - macOS TCP/IP stack (native Darwin kernel — no Linux VM fingerprint)
  - Chrome BoringSSL TLS fingerprint (bit-identical to Chrome 131)
  - Real Chrome HTTP/2 SETTINGS frames

The Go proxy sends requests with X-Target-URL header specifying the
actual Kiro endpoint.

Usage:
  source .venv/bin/activate
  python3 kiro_tls_terminator.py --port 8888

Go proxy config:
  Set account proxyURL to: http://host.docker.internal:8888
"""

import argparse
import http.server
import sys
import time
import threading
import urllib.parse
import io

from curl_cffi import requests as curl_requests

# ── constants ──────────────────────────────────────────────────
LISTEN_HOST = "0.0.0.0"
LISTEN_PORT = 8888
IMPERSONATE = "chrome131"  # Chrome 131 on Windows (BoringSSL bit-identical)
TIMEOUT = 300              # per-request timeout (streaming can be long)

# Headers we strip from incoming requests (Go sets them, we rebuild)
STRIP_REQ = {"host", "connection", "transfer-encoding",
             "accept-encoding", "content-length", "content-type"}

# Headers we strip from Kiro responses before forwarding
STRIP_RES = {"transfer-encoding", "connection", "keep-alive"}


# ── handler ─────────────────────────────────────────────────────

class TerminatorHandler(http.server.BaseHTTPRequestHandler):
    """Forward HTTP requests to Kiro via curl_cffi with Chrome BoringSSL TLS."""

    # Per-host session cache: host → curl_cffi.Session
    _sessions = {}
    _lock = threading.Lock()

    def do_POST(self):
        self._forward("POST")

    def do_GET(self):
        self._forward("GET")

    def do_PUT(self):
        self._forward("PUT")

    # --------------------------------------------------------------
    def _forward(self, method):
        start = time.monotonic()

        # Read body
        cl = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(cl) if cl > 0 else b""

        # Determine target URL
        target = self.headers.get("X-Target-URL", "")
        if not target:
            # Go HTTP proxy sends absolute URL in request line:
            # "POST http://q.us-east-1.amazonaws.com/path HTTP/1.1"
            # Python's http.server parses this as self.path
            path = self.path
            if path.startswith("http://") or path.startswith("https://"):
                target = path.replace("http://", "https://", 1)
            elif path.startswith("/https://") or path.startswith("/http://"):
                target = path[1:].replace("http://", "https://", 1)
            else:
                # Relative path — assume Kiro IDE
                target = f"https://q.us-east-1.amazonaws.com{path}"

        if not target.startswith("https://"):
            target = target.replace("http://", "https://", 1)

        parsed = urllib.parse.urlparse(target)
        host = parsed.hostname or "q.us-east-1.amazonaws.com"

        # Build headers
        fwd_headers = {}
        content_type = ""
        for k, v in self.headers.items():
            lk = k.lower()
            if lk in STRIP_REQ:
                continue
            if lk == "content-type":
                content_type = v
            fwd_headers[k] = v

        # Get cached session (Chrome BoringSSL TLS)
        session = self._get_session(host)

        try:
            if method == "GET":
                resp = session.get(target, headers=fwd_headers, timeout=TIMEOUT)
            elif method == "PUT":
                resp = session.put(target, headers=fwd_headers, data=body, timeout=TIMEOUT)
            else:
                resp = session.post(target, headers=fwd_headers, data=body,
                                    timeout=TIMEOUT)

            # Send status
            self.send_response(resp.status_code)

            # Copy response headers
            for k, v in resp.headers.items():
                if k.lower() in STRIP_RES:
                    continue
                try:
                    self.send_header(k, v)
                except Exception:
                    pass

            self.end_headers()

            # Write response body
            self.wfile.write(resp.content)

        except Exception as exc:
            elapsed = time.monotonic() - start
            print(f"[terminator] ERROR {method} {host}: {exc} ({elapsed:.1f}s)",
                  file=sys.stderr)
            try:
                self.send_error(502, str(exc))
            except Exception:
                pass
        else:
            elapsed = time.monotonic() - start
            print(f"[terminator] {resp.status_code} {method} {target[:80]} "
                  f"({len(resp.content)}B, {elapsed:.1f}s)", file=sys.stderr)

    # --------------------------------------------------------------
    def _get_session(self, host: str) -> curl_requests.Session:
        with self._lock:
            sess = self._sessions.get(host)
            if sess is not None:
                return sess

        sess = curl_requests.Session()
        sess.impersonate = IMPERSONATE
        sess.timeout = TIMEOUT
        sess.max_connections = 10
        sess.max_keepalive_connections = 5
        sess.keepalive_expiry = 30

        # Set Chrome-like default headers
        sess.headers.update({
            "Accept": "*/*",
            "Accept-Language": "en-US,en;q=0.9",
        })

        with self._lock:
            self._sessions[host] = sess
        return sess

    def log_message(self, format, *args):
        pass  # we handle logging


# ── main ─────────────────────────────────────────────────────────

def main():
    parser = argparse.ArgumentParser(
        description="Kiro TLS Terminator v2 — HTTP reverse proxy (Chrome BoringSSL)"
    )
    parser.add_argument("--port", type=int, default=LISTEN_PORT)
    parser.add_argument("--host", default=LISTEN_HOST)
    parser.add_argument("--impersonate", default=IMPERSONATE,
                        choices=["chrome131", "chrome130", "chrome124",
                                 "firefox133", "safari18_0", "edge101"])
    args = parser.parse_args()

    TerminatorHandler.IMPERSONATE = args.impersonate

    server = http.server.ThreadingHTTPServer(
        (args.host, args.port), TerminatorHandler
    )

    print(f"[terminator] {args.impersonate} TLS — Chrome BoringSSL (bit-identical)")
    print(f"[terminator] TCP: macOS Darwin (native, not Linux VM)")
    print(f"[terminator] listening on http://{args.host}:{args.port}")
    print(f"[terminator] Ready. Set proxyURL=http://host.docker.internal:{args.port}")

    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\n[terminator] done")


if __name__ == "__main__":
    main()
