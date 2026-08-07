#!/usr/bin/env python3
"""
Kiro TLS proxy — forwards plain HTTP requests from Docker to Kiro API
using macOS SecureTransport (via curl) to avoid AWS anti-abuse detection.

Docker → HTTP localhost:8888 → curl HTTPS Kiro API (SecureTransport)

Usage: python3 kiro_tls_proxy.py [--port 8888]
"""

import http.server
import subprocess
import sys
import os
import argparse
import threading
import urllib.parse

LISTEN_HOST = "127.0.0.1"
LISTEN_PORT = 8888
CURL_BIN = "/usr/bin/curl"
# Timeout per request
CURL_TIMEOUT = 300


class KiroProxyHandler(http.server.BaseHTTPRequestHandler):
    """Forward HTTP requests to Kiro API via curl (SecureTransport TLS)."""

    def do_POST(self):
        self._forward_request("POST")

    def do_GET(self):
        self._forward_request("GET")

    def do_PUT(self):
        self._forward_request("PUT")

    def _forward_request(self, method):
        # Reconstruct the target URL from the request path
        # The proxy receives requests like http://proxy:8888/https://kiro-api.amazonaws.com/path
        # Docker connects to proxy directly for the Kiro API host
        content_length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(content_length) if content_length > 0 else b""

        # Determine target URL
        # The proxy expects the Host header or a custom header to know where to forward
        target_url = self.headers.get("X-Target-URL", "")
        if not target_url:
            # Default: forward to Kiro IDE endpoint
            target_url = "https://q.us-east-1.amazonaws.com" + self.path

        # Build curl command
        cmd = [
            CURL_BIN,
            "-s",
            "--max-time", str(CURL_TIMEOUT),
            "-X", method,
            "-H", f"Host: {urllib.parse.urlparse(target_url).hostname}",
            "-o", "-",  # stdout
            "-D", "-",  # stderr for headers (we'll parse them)
        ]

        # Pass through headers (except hop-by-hop)
        skip_headers = {"host", "connection", "proxy-connection", "x-target-url",
                        "transfer-encoding", "keep-alive"}
        for key, value in self.headers.items():
            if key.lower() in skip_headers:
                continue
            cmd.extend(["-H", f"{key}: {value}"])

        # Add body if present
        if body:
            cmd.extend(["--data-binary", "@-"])

        cmd.append(target_url)

        try:
            if body:
                proc = subprocess.Popen(
                    cmd,
                    stdin=subprocess.PIPE,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                )
                stdout, stderr = proc.communicate(input=body, timeout=CURL_TIMEOUT)
            else:
                proc = subprocess.Popen(
                    cmd,
                    stdin=subprocess.DEVNULL,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                )
                stdout, stderr = proc.communicate(timeout=CURL_TIMEOUT)
        except subprocess.TimeoutExpired:
            proc.kill()
            self.send_error(504, "Gateway Timeout")
            return
        except Exception as e:
            self.send_error(502, f"Bad Gateway: {e}")
            return

        # Parse curl's -D output from stderr
        header_end = stderr.find(b"\r\n\r\n")
        if header_end == -1:
            header_end = stderr.find(b"\n\n")
        if header_end == -1:
            response_headers = stderr
            response_body = stdout
        else:
            response_headers = stderr[:header_end]
            response_body = stdout

        # Send response back
        self.send_response(proc.returncode if proc.returncode != 0 else 200)

        # Add response headers
        for line in response_headers.decode("utf-8", errors="replace").splitlines():
            line = line.strip()
            if ":" in line:
                key, value = line.split(":", 1)
                key = key.strip()
                value = value.strip()
                if key.lower() in {"transfer-encoding", "connection"}:
                    continue
                self.send_header(key, value)

        self.end_headers()
        self.wfile.write(response_body)

    def log_message(self, format, *args):
        print(f"[KiroProxy] {args[0]}", file=sys.stderr)


def main():
    parser = argparse.ArgumentParser(description="Kiro TLS Proxy (macOS SecureTransport)")
    parser.add_argument("--port", type=int, default=LISTEN_PORT)
    parser.add_argument("--host", default=LISTEN_HOST)
    args = parser.parse_args()

    server = http.server.ThreadingHTTPServer(
        (args.host, args.port), KiroProxyHandler
    )
    print(f"[KiroProxy] listening on http://{args.host}:{args.port}", file=sys.stderr)
    print(f"[KiroProxy] forwarding to Kiro API via curl (macOS SecureTransport)", file=sys.stderr)

    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\n[KiroProxy] shutting down", file=sys.stderr)


if __name__ == "__main__":
    main()
