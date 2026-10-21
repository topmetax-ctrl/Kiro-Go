#!/usr/bin/env python3
"""Phase 8 controlled cache experiment.

Question: does a stable conversation identity materially improve upstream cache
reuse, versus a different session id on every request?

Design (single model, single provider, single enabled connection, non-stream,
fixed output budget):

  GROUP A  prefix P1, ONE stable session id, 5 turns
  GROUP B  prefix P2, a NEW random session id every turn, 5 turns
  GROUP C  prefix P1 again (already warmed by A), random session per turn

P1 and P2 are distinct and freshly generated, so each group's only cache source
is repetition WITHIN that group -- turn 1 of each group is its own cold control.
Group C then asks the separate question of whether A's warm prefix is reachable
without A's session identity, which separates session-keyed caching from
content/prefix-keyed caching.

Session ids are never printed; only source/scope/mapped labels are.
"""
import json
import os
import random
import sys
import time
import urllib.error
import urllib.request
import uuid

BASE = "http://localhost:8080"
KEY = os.environ["ANTHROPIC_AUTH_TOKEN"]
ADMIN = {"X-Admin-Password": "changeme"}
MODEL = "deepseek-v4.1-flash"
MAX_TOKENS = 32
PREFIX_WORDS = 6000  # ~8k tokens, comfortably over any provider cache minimum

WORDS = ("alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi "
         "omicron pi rho sigma tau upsilon phi chi psi omega vector matrix tensor "
         "gradient kernel buffer socket pointer mutex thread channel latency payload "
         "cursor schema shard replica quorum ledger digest cipher entropy").split()


def make_prefix(tag, nwords=PREFIX_WORDS):
    """Deterministic-but-unique filler. Unique tag => guaranteed cold prefix."""
    rnd = random.Random(tag)
    lines = []
    for i in range(nwords // 12):
        lines.append("%s-%04d %s" % (tag, i, " ".join(rnd.choice(WORDS) for _ in range(12))))
    return ("Reference corpus %s. Treat the following as immutable context.\n" % tag) + "\n".join(lines)


def ask(prefix, question, session_id):
    body = {
        "model": MODEL,
        "max_tokens": MAX_TOKENS,
        "messages": [{"role": "user", "content": [
            {"type": "text", "text": prefix, "cache_control": {"type": "ephemeral"}},
            {"type": "text", "text": question},
        ]}],
    }
    req = urllib.request.Request(
        BASE + "/v1/messages", data=json.dumps(body).encode(), method="POST",
        headers={"content-type": "application/json", "x-api-key": KEY,
                 "anthropic-version": "2023-06-01", "X-Session-Affinity": session_id})
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=180) as r:
            return r.status, json.loads(r.read().decode()), int((time.time() - t0) * 1000)
    except urllib.error.HTTPError as e:
        return e.code, {"error": e.read().decode()[:200]}, int((time.time() - t0) * 1000)


def telemetry(n=12):
    req = urllib.request.Request(
        BASE + "/admin/api/forward-events?offset=0&limit=%d" % n, headers=ADMIN)
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.load(r)["items"]


def run_group(name, prefix, turns, stable):
    sid = str(uuid.uuid4())
    rows = []
    for t in range(1, turns + 1):
        if not stable:
            sid = str(uuid.uuid4())
        status, resp, ms = ask(prefix, "Turn %d. Reply with exactly one word: ok" % t, sid)
        u = (resp.get("usage") or {}) if isinstance(resp, dict) else {}
        rows.append({
            "group": name, "turn": t, "status": status, "ms": ms,
            "input": u.get("input_tokens"), "output": u.get("output_tokens"),
            "cache_read": u.get("cache_read_input_tokens"),
            "cache_creation": u.get("cache_creation_input_tokens"),
            "err": resp.get("error") if status != 200 else None,
        })
        print("  %s turn %d -> %s  in=%s read=%s create=%s out=%s (%dms)" % (
            name, t, status, u.get("input_tokens"), u.get("cache_read_input_tokens"),
            u.get("cache_creation_input_tokens"), u.get("output_tokens"), ms))
        time.sleep(2)
    return rows


def main():
    nonce = uuid.uuid4().hex[:8]
    p1 = make_prefix("P1-" + nonce)
    p2 = make_prefix("P2-" + nonce)
    print("prefix sizes: P1=%d chars  P2=%d chars (distinct, both cold)" % (len(p1), len(p2)))
    print("model=%s max_tokens=%d stream=false\n" % (MODEL, MAX_TOKENS))

    all_rows = []
    print("GROUP A - stable session, prefix P1")
    all_rows += run_group("A_stable", p1, 5, stable=True)
    print("\nGROUP B - random session per request, prefix P2 (own cold prefix)")
    all_rows += run_group("B_random", p2, 5, stable=False)
    print("\nGROUP C - random session, prefix P1 (already warmed by group A)")
    all_rows += run_group("C_random_warm_prefix", p1, 3, stable=False)

    out = "/Users/macdev/Demo/Kiro-Go/logs/cache_experiment_%s.json" % nonce
    with open(out, "w") as f:
        json.dump({"nonce": nonce, "rows": all_rows,
                   "telemetry": telemetry(20)}, f, indent=2)
    print("\nraw results -> %s" % out)


if __name__ == "__main__":
    main()
