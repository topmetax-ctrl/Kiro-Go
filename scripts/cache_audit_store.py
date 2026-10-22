#!/usr/bin/env python3
"""Keep minimised exports of Kiro-Go forward events for cache analysis.

The event ring lives in memory and is gone after a restart. This saves what the
cache analysis needs, and only that, to a directory outside the repository, and
deletes it again after a retention period.

  save   take an export (a file, or the live admin API), keep an allowlist of its
         fields and write one snapshot; then prune
  prune  delete snapshots past their age or over the size cap

A snapshot keeps timing, token usage, the cache digests and signals, and the
session digests. It drops everything that names a person, a client, a connection
or a request: API key name and id, client address, connection, request id,
provider and route ids, error text. A credential label is replaced by a stable
token that is only meaningful inside that one snapshot. The classifier
(classify_cache_rewrites.py) reads a snapshot as it reads an export.

Files are 0600 in a 0700 directory, and the directory may not be inside a git
work tree: an export must never be committed.

usage: cache_audit_store.py save [--source EXPORT.json | --url http://127.0.0.1:8080] [--dir DIR]
       cache_audit_store.py prune [--dir DIR] [--dry-run]
The admin password for --url is read from KIRO_GO_ADMIN_PASSWORD, never from argv.
"""

import argparse
import hashlib
import json
import os
import sys
import time
import urllib.parse
import urllib.request

DEFAULT_DIR = os.path.join("~", ".kiro-go-audit")
SNAPSHOTS = "exports"
PREFIX = "audit-"
SUFFIX = ".json"

MAX_DAYS = 30
# A full ring is about 15 MB minimised; one snapshot a day for 30 days fits.
MAX_MB = 400
# Snapshots this young belong to an audit that may still be running: pruning
# never touches them, whatever the size cap says.
PROTECT_HOURS = 48
# A temp file this old was left by a crash.
STALE_TMP_SECONDS = 3600

EVENT_FIELDS = ("time", "clientModel", "targetModel", "upstreamModel", "endpoint", "status", "ok", "stream",
                "streamOutcome", "latencyMs", "ttfbMs", "inputTokens", "outputTokens", "sessionSource", "sessionScope")
USAGE_FIELDS = ("inputTokens", "outputTokens", "cacheReadInputTokens", "cacheCreationInputTokens",
                "cacheCreation5mTokens", "cacheCreation1hTokens", "reasoningOutputTokens", "source", "protocol")
CACHE_FIELDS = ("conv", "tools", "system", "settings", "msgs", "prefix", "marks", "policy", "sess", "parent")
SIGNAL_FIELDS = ("userId", "parentSession", "subagentMarker", "agentHeader", "probe", "titleHelper", "extendedTtlBeta")


def pick(src, fields):
    return {k: src[k] for k in fields if k in src}


def minimise(doc, salt):
    """The export with only the allowlisted fields. salt keys the credential token."""
    items = doc["items"] if isinstance(doc, dict) else doc
    out = []
    for e in items:
        m = pick(e, EVENT_FIELDS)
        if isinstance(e.get("usage"), dict):
            m["usage"] = pick(e["usage"], USAGE_FIELDS)
        if isinstance(e.get("cache"), dict):
            c = pick(e["cache"], CACHE_FIELDS)
            if isinstance(e["cache"].get("signals"), dict):
                c["signals"] = pick(e["cache"]["signals"], SIGNAL_FIELDS)
            m["cache"] = c
        if e.get("accountLabel"):
            digest = hashlib.sha256(salt + str(e["accountLabel"]).encode()).hexdigest()[:8]
            m["accountLabel"] = "acct-" + digest
        out.append(m)
    exported_at = doc.get("exportedAt") if isinstance(doc, dict) else None
    return {"minimised": True, "exportedAt": exported_at, "total": len(out), "items": out}


def in_git_work_tree(path):
    path = os.path.realpath(path)
    while True:
        if os.path.exists(os.path.join(path, ".git")):
            return True
        parent = os.path.dirname(path)
        if parent == path:
            return False
        path = parent


def ensure_dir(root):
    """The snapshot directory, created 0700 and tightened if it was not."""
    root = os.path.expanduser(root)
    if in_git_work_tree(root):
        raise SystemExit("refusing to keep exports inside a git work tree: %s" % root)
    snapshots = os.path.join(root, SNAPSHOTS)
    if os.path.isdir(root) and os.listdir(root) and not os.path.isdir(snapshots):
        raise SystemExit("refusing to use %s: it is not empty and is not an audit store" % root)
    os.makedirs(snapshots, mode=0o700, exist_ok=True)
    for d in (root, snapshots):
        if os.stat(d).st_mode & 0o077:
            os.chmod(d, 0o700)
    return snapshots


def fetch(url):
    password = os.environ.get("KIRO_GO_ADMIN_PASSWORD")
    if not password:
        raise SystemExit("set KIRO_GO_ADMIN_PASSWORD to read the live export")
    if urllib.parse.urlparse(url).scheme not in ("http", "https"):
        raise SystemExit("--url must be http or https")
    req = urllib.request.Request(url.rstrip("/") + "/admin/api/forward-events/export?format=json",
                                 headers={"X-Admin-Password": password})
    with urllib.request.urlopen(req, timeout=60) as resp:
        return json.load(resp)


def write_snapshot(snapshots, doc, now):
    stamp = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime(now))
    final = os.path.join(snapshots, PREFIX + stamp + SUFFIX)
    n = 1
    while os.path.exists(final):
        final = os.path.join(snapshots, "%s%s-%d%s" % (PREFIX, stamp, n, SUFFIX))
        n += 1
    tmp = os.path.join(snapshots, ".tmp-%d" % os.getpid())
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as fh:
        json.dump(doc, fh, separators=(",", ":"))
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, final)
    return final


def snapshot_files(snapshots):
    out = []
    for name in os.listdir(snapshots):
        if name.startswith(PREFIX) and name.endswith(SUFFIX):
            st = os.stat(os.path.join(snapshots, name))
            out.append((st.st_mtime, st.st_size, os.path.join(snapshots, name)))
    return sorted(out)


def prune(snapshots, now, max_days=MAX_DAYS, max_mb=MAX_MB, protect_hours=PROTECT_HOURS, dry_run=False):
    """Delete snapshots past max_days, then the oldest until under max_mb.

    A snapshot younger than protect_hours is never deleted. Returns
    (deleted paths, bytes still over the cap).
    """
    deleted = []

    def drop(path):
        deleted.append(path)
        if not dry_run:
            os.remove(path)

    for name in os.listdir(snapshots):
        path = os.path.join(snapshots, name)
        if name.startswith(".tmp-") and now - os.stat(path).st_mtime > STALE_TMP_SECONDS:
            drop(path)

    keep = []
    for mtime, size, path in snapshot_files(snapshots):
        age = now - mtime
        if age > max_days * 86400 and age > protect_hours * 3600:
            drop(path)
        else:
            keep.append((mtime, size, path))
    total = sum(size for _, size, _ in keep)
    cap = max_mb * 1024 * 1024
    for mtime, size, path in keep:
        if total <= cap:
            break
        if now - mtime > protect_hours * 3600:
            drop(path)
            total -= size
    return deleted, max(0, total - cap)


def add_limits(p):
    p.add_argument("--dir", default=DEFAULT_DIR, help="store root (default %(default)s)")
    p.add_argument("--max-days", type=float, default=MAX_DAYS)
    p.add_argument("--max-mb", type=float, default=MAX_MB)
    p.add_argument("--protect-hours", type=float, default=PROTECT_HOURS, help="never delete a snapshot younger than this")


def report_prune(deleted, over, dry_run):
    verb = "would delete" if dry_run else "deleted"
    for path in deleted:
        print("%s %s" % (verb, path))
    if over:
        print("WARNING: %.1f MB over the size cap and nothing old enough to delete" % (over / 1048576), file=sys.stderr)


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    save = sub.add_parser("save", help="minimise an export and keep it")
    src = save.add_mutually_exclusive_group(required=True)
    src.add_argument("--source", help="an export file (the JSON from /admin/api/forward-events/export)")
    src.add_argument("--url", help="base URL of a running Kiro-Go")
    add_limits(save)
    pr = sub.add_parser("prune", help="apply the retention rules")
    pr.add_argument("--dry-run", action="store_true")
    add_limits(pr)
    args = ap.parse_args(argv)

    os.umask(0o077)
    snapshots = ensure_dir(args.dir)
    now = time.time()
    if args.cmd == "save":
        if args.source:
            with open(args.source) as fh:
                doc = json.load(fh)
        else:
            doc = fetch(args.url)
        snap = minimise(doc, os.urandom(16))
        path = write_snapshot(snapshots, snap, now)
        print("saved %d events to %s (%d bytes)" % (snap["total"], path, os.path.getsize(path)))
    deleted, over = prune(snapshots, now, args.max_days, args.max_mb, args.protect_hours, getattr(args, "dry_run", False))
    report_prune(deleted, over, getattr(args, "dry_run", False))
    return 2 if over and args.cmd == "prune" else 0


if __name__ == "__main__":
    sys.exit(main())
