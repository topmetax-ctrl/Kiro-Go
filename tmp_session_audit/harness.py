#!/usr/bin/env python3
"""Controlled audit of X-Opencode-Session semantics. Read-only w.r.t. Kiro-Go."""
import json, os, sys, time, uuid, hashlib, random, string, threading
import urllib.request, urllib.error

CFG = "/Users/macdev/Demo/Kiro-Go/data/config.json"
_c = json.load(open(CFG))
_u = [x for x in _c["upstreams"] if x["name"] == "OpenCode"][0]
KEY = [k for k in _u["connections"] if k["enabled"]][0]["apiKey"]
BASE = _u["baseUrl"].rstrip("/")
MODEL = "deepseek-v4.1-flash"

def sh(s):
    """short stable hash so sessions can be correlated without logging raw ids"""
    return hashlib.sha256(s.encode()).hexdigest()[:8]

def rnd(n=8):
    return "".join(random.choices(string.ascii_uppercase + string.digits, k=n))

def call(session, messages, max_tokens=64, label="", temperature=0.0):
    body = {"model": MODEL, "max_tokens": max_tokens, "temperature": temperature,
            "messages": messages, "stream": False}
    req = urllib.request.Request(BASE + "/chat/completions",
                                 data=json.dumps(body).encode(), method="POST")
    req.add_header("Authorization", "Bearer " + KEY)
    req.add_header("Content-Type", "application/json")
    req.add_header("User-Agent", "opencode/1.0")
    if session is not None:
        req.add_header("x-opencode-session", session)
    t0 = time.time()
    out = {"label": label, "session": (sh(session) if session else None),
           "t_start": t0}
    try:
        r = urllib.request.urlopen(req, timeout=180)
        raw = r.read().decode()
        out["status"] = r.status
        d = json.loads(raw)
        out["raw"] = d
        ch = (d.get("choices") or [{}])[0]
        out["text"] = ((ch.get("message") or {}).get("content") or "")
        u = d.get("usage") or {}
        ptd = u.get("prompt_tokens_details") or {}
        out["input"] = u.get("prompt_tokens")
        out["output"] = u.get("completion_tokens")
        out["cache_read"] = ptd.get("cached_tokens")
        out["cache_write"] = ptd.get("cache_write_tokens", ptd.get("cache_creation_tokens"))
        out["usage_raw"] = u
        out["resp_id"] = d.get("id")
    except urllib.error.HTTPError as e:
        out["status"] = e.code
        out["text"] = ""
        out["error"] = e.read().decode()[:400]
    except Exception as e:
        out["status"] = "ERR"
        out["text"] = ""
        out["error"] = str(e)[:300]
    out["latency"] = round(time.time() - t0, 3)
    out["t_end"] = time.time()
    return out

def row(r):
    cr = r.get("cache_read"); cw = r.get("cache_write"); inp = r.get("input")
    pct = f"{100.0*cr/inp:.0f}%" if (cr and inp) else ("0%" if inp else "-")
    return (f"{r['label']:<16} st={r['status']:<5} sess={str(r.get('session')):<9} "
            f"in={str(inp):<6} cr={str(cr):<6} cw={str(cw):<5} hit={pct:<5} "
            f"out={str(r.get('output')):<5} lat={r['latency']:<7}")
