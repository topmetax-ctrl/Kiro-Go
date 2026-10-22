#!/usr/bin/env python3
"""Classify large prompt-cache writes ("rewrites") from Kiro-Go forward events.

A rewrite is a request whose cache_creation is large: the upstream had to store
most of the prompt again instead of reading it. For each one this reports the
most likely cause, using only evidence the events carry:

  expired           the conversation was idle longer than the cache lives
  prefix_changed    the prompt no longer extends the previous request's prompt
  scope_changed     the previous request was served by a different credential
  concurrent_write  a request sharing this one's history started first and had not
                    yet begun its response, so its cache entry was not readable yet
  unknown           the evidence does not decide

Each verdict also carries how well the evidence supports it:

  confirmed   the events themselves show it (prompt digests, the upstream's own
              TTL split, request timing)
  likely      it fits the evidence but rests on a guess: a predecessor inferred
              from sizes, a size match instead of a prompt digest, or a credential
              change that does not by itself prove the cache is credential-scoped
  unknown     the evidence does not decide

Anything the data cannot prove stays "unknown", with the open candidates listed.
Every verdict describes the body this gateway sent. The body that reaches the
model after a gateway further upstream rewrote it is not visible here: when the
history is intact and inside the TTL yet the cache missed, the cause is
downstream, and the output says so.

Events come from GET /admin/api/forward-events?limit=N (or its JSON export).
Events recorded before cache tracing existed carry no prompt digests and no TTL
split; for those the previous request is a heuristic guess, and every label that
rests on it is marked "heuristic". See docs/cache-observability.md.

usage: classify_cache_rewrites.py EVENTS.json [--min-write N] [--model M] [--json]
"""

import argparse
import collections
import json
import re
import sys

# No entry outlives this without being read again, whatever tier wrote it.
MAX_TTL = 3600
TTL_S = {"5m": 300, "1h": 3600}
# A heuristic predecessor must be at most this much smaller than the rewrite.
HEURISTIC_GROWTH = 60000
# Twins: the same request sent twice, so their sizes agree this closely.
TWIN_TOLERANCE = 0.01

CONFIRMED, LIKELY, UNKNOWN = "confirmed", "likely", "unknown"

Verdict = collections.namedtuple("Verdict", "row label confidence evidence hint")
MARK = re.compile(r"^([tsm])(\d+)(.*)$")


class Row:
    def __init__(self, e, i):
        u = e.get("usage") or {}
        self.i = i
        self.end = e["time"] / 1000.0
        self.start = self.end - e.get("latencyMs", 0) / 1000.0
        self.first_byte = self.start + e.get("ttfbMs", 0) / 1000.0
        self.tot = u.get("inputTokens", e.get("inputTokens", 0)) or 0
        self.cr = u.get("cacheReadInputTokens") or 0
        self.cc = u.get("cacheCreationInputTokens") or 0
        self.cc5 = u.get("cacheCreation5mTokens")
        self.cc1 = u.get("cacheCreation1hTokens")
        self.model = e.get("targetModel") or e.get("clientModel")
        self.trace = e.get("cache")
        self.account = e.get("accountLabel") or None
        self.pred = None
        self.link = None

    @property
    def entries(self):
        """Prefix digests this request asked the upstream to cache.

        The whole prompt, plus the boundary after every message that carries a
        breakpoint: a later request can only read an entry whose digest it lists.
        """
        if not self.trace:
            return set()
        out = {self.full_prefix} if self.full_prefix else set()
        bounds = set()
        for m in self.trace.get("marks") or []:
            parsed = MARK.match(m)
            if parsed and parsed.group(1) == "m":
                bounds.add("%d:" % (int(parsed.group(2)) + 1))
        out.update(p for p in self.trace.get("prefix") or [] if p.split(":", 1)[0] + ":" in bounds)
        return out

    @property
    def full_prefix(self):
        """Digest of the whole prompt, when the event carries one."""
        if not self.trace:
            return None
        want = "%d:" % self.trace.get("msgs", -1)
        for p in self.trace.get("prefix") or []:
            if p.startswith(want):
                return p
        return None


def load(path, model=None):
    with open(path) as fh:
        data = json.load(fh)
    return build(data["items"] if isinstance(data, dict) else data, model)


def build(items, model=None):
    """Rows for the successful upstream-reported requests of one model, oldest first."""
    items = [e for e in items if e.get("status") == 200 and (e.get("usage") or {}).get("source") == "upstream"]
    if model is None:
        model = collections.Counter(e.get("targetModel") or e.get("clientModel") for e in items).most_common(1)[0][0]
    items = [e for e in items if (e.get("targetModel") or e.get("clientModel")) == model]
    items.sort(key=lambda e: e["time"])
    return [Row(e, i) for i, e in enumerate(items)], model


def link_by_arithmetic(rows):
    """Request B continues A when B read exactly what A held (A.cr + A.cc)."""
    held = {}
    has_successor = set()
    for r in rows:
        if r.cr > 0 and r.cr in held:
            has_successor.add(held[r.cr])
        held[r.cr + r.cc] = r.i
    return has_successor


def find_predecessor(r, rows, has_successor):
    """Return (row, link) with link in chain | same-conv | heuristic, or (None, None)."""
    earlier = [e for e in rows[: r.i] if e.start < r.start]
    if r.trace and r.trace.get("conv"):
        mine = set(r.trace.get("prefix") or [])
        same = [e for e in earlier if e.trace and e.trace.get("conv") == r.trace["conv"]]
        chained = [e for e in same if e.entries & mine]
        if chained:
            return max(chained, key=lambda e: e.end), "chain"
        if same:
            return max(same, key=lambda e: e.end), "same-conv"
        return None, None
    best = None
    for e in earlier:
        if e.end > r.start + 1 or e.i in has_successor:
            continue
        if 0 <= r.tot - e.tot <= HEURISTIC_GROWTH and (best is None or e.end > best.end):
            best = e
    return (best, "heuristic") if best else (None, None)


def tier_of(e, rows):
    """TTL tier of the newest cache write at or before e, when the split is known.

    Walks back along digest-linked requests: a pure cache hit writes nothing, so
    the tier in force is the one its last writer used. Without digests there is
    no trustworthy chain to walk, so only e itself is read.
    """
    for _ in range(25):
        if e is None:
            return None
        if e.cc > 0 and (e.cc5 is not None or e.cc1 is not None):
            if (e.cc5 or 0) > 0 and not e.cc1:
                return "5m"
            if (e.cc1 or 0) > 0 and not e.cc5:
                return "1h"
            return "mixed"
        if not e.trace:
            return None
        e, link = find_predecessor(e, rows, ())
        if link != "chain":
            return None
    return None


def compaction_followup(r, rows):
    """A smaller context that restarts shortly after r, as a compaction leaves behind."""
    for q in rows[r.i + 1 : r.i + 80]:
        dt = q.start - r.end
        if dt > 240:
            break
        if dt >= -5 and q.tot < 0.5 * r.tot and 10000 <= q.tot <= 130000 and q.cr <= 30000:
            return q, dt
    return None


def shared_boundary(r, e):
    """Largest message count at which r lists a prefix digest that e asked to cache, or None.

    That is the entry r would have read had e's write been readable already, so it
    is what makes two requests one prompt even when their last messages differ.
    """
    mine = set(r.trace.get("prefix") or [])
    ks = [int(p.split(":", 1)[0]) for p in e.entries & mine]
    return max(ks) if ks else None


def find_twin(r, rows, min_write):
    """An earlier-started request that wrote what r wrote and had no response yet."""
    for e in rows[max(0, r.i - 300) : r.i + 300]:
        if e.i == r.i or e.cc < min_write * 0.5 or not (e.start < r.start < e.first_byte):
            continue
        if r.trace and e.trace:
            same = (r.trace.get("conv") == e.trace.get("conv") and r.trace.get("settings") == e.trace.get("settings")
                    and shared_boundary(r, e) is not None)
        else:
            same = abs(e.tot - r.tot) <= TWIN_TOLERANCE * r.tot
        if same:
            return e
    return None


def diverged_between(r, p):
    """Message window where r's history stops matching p's, from shared checkpoints."""
    a = {int(k): d for k, d in (x.split(":") for x in p.trace.get("prefix") or [])}
    b = {int(k): d for k, d in (x.split(":") for x in r.trace.get("prefix") or [])}
    shared = sorted(set(a) & set(b))
    good = max((k for k in shared if a[k] == b[k]), default=0)
    bad = min((k for k in shared if k > good and a[k] != b[k]), default=None)
    return good, bad


def mark_shape(trace):
    """The breakpoint layout without the conversation length in it.

    A message breakpoint is named by its distance from the newest message, so
    two turns of one conversation compare equal when the client moved its marker
    along with the history, and unequal when a marker or its ttl/scope changed.
    """
    n = trace.get("msgs", 0)
    out = []
    for m in trace.get("marks") or []:
        parsed = MARK.match(m)
        if parsed and parsed.group(1) == "m":
            out.append("m-%d%s" % (n - 1 - int(parsed.group(2)), parsed.group(3)))
        else:
            out.append(m)
    return out


def own_tier(r):
    """The TTL tier this request itself wrote, when its split says so."""
    if r.cc > 0 and (r.cc5 is not None or r.cc1 is not None):
        if (r.cc5 or 0) > 0 and not r.cc1:
            return "5m"
        if (r.cc1 or 0) > 0 and not r.cc5:
            return "1h"
        return "mixed"
    return None


def cache_key_notes(r, p, rows):
    """What differs between two linked requests besides the prompt content.

    Equal prompt digests do not make equal cache entries: the upstream also keys
    on where the breakpoint sits and how long it lives. Report any difference as
    evidence; it is not a cause by itself.
    """
    notes = []
    if r.trace and p.trace:
        a, b = mark_shape(p.trace), mark_shape(r.trace)
        if a != b:
            notes.append("breakpoints changed %s -> %s" % (",".join(a) or "none", ",".join(b) or "none"))
    before, now = tier_of(p, rows), own_tier(r)
    if before and now and before != now:
        notes.append("TTL tier changed %s -> %s" % (before, now))
    return notes


def classify(r, rows, has_successor, min_write):
    """Return a Verdict for one rewrite."""
    label, confidence, evidence, hint = decide(r, rows, has_successor, min_write)
    if r.pred is not None and label in ("unknown", "prefix_changed", "scope_changed"):
        notes = cache_key_notes(r, r.pred, rows)
        if notes:
            evidence += "; " + "; ".join(notes)
    follow = compaction_followup(r, rows)
    if follow:
        q, dt = follow
        evidence += "; a %d-token context restarts %.0fs later (compaction?)" % (q.tot, dt)
    return Verdict(r, label, confidence, evidence, hint)


def decide(r, rows, has_successor, min_write):
    """Return (label, confidence, evidence, hint)."""
    twin = find_twin(r, rows, min_write)
    if twin:
        digests = bool(r.trace and twin.trace)
        if not digests:
            how = "size within %.1f%%" % (TWIN_TOLERANCE * 100)
        elif r.full_prefix and r.full_prefix in twin.entries:
            how = "identical prompt digest"
        else:
            how = "same history up to message %d, then the prompts differ" % shared_boundary(r, twin)
        return ("concurrent_write", CONFIRMED if digests else LIKELY,
                "request #%d started %.1fs earlier and sent no response for another %.1fs (%s)"
                % (twin.i, r.start - twin.start, twin.first_byte - r.start, how), "")
    p, link = find_predecessor(r, rows, has_successor)
    r.pred, r.link = p, link
    if p is None:
        return "unknown", UNKNOWN, "no earlier request of this conversation is identifiable", ""
    gap = r.start - p.end
    rel = "heuristic " if link == "heuristic" else ""
    sure = LIKELY if link == "heuristic" else CONFIRMED
    if gap > MAX_TTL:
        return "expired", sure, "%sprevious request #%d ended %.0fs earlier; no cache tier lives past %ds" % (rel, p.i, gap, MAX_TTL), ""
    tier = tier_of(p, rows)
    if tier == "5m" and gap > TTL_S["5m"]:
        return "expired", sure, "%sprevious request #%d wrote the 5m tier and ended %.0fs earlier" % (rel, p.i, gap), ""
    expiry_open = tier in (None, "mixed") and gap > TTL_S["5m"]
    if link == "same-conv":
        good, bad = diverged_between(r, p)
        where = "messages %d..%s" % (good, bad if bad else "end")
        parts = [n for n in ("tools", "system", "settings") if (r.trace.get(n) or "") != (p.trace.get(n) or "")]
        what = (", ".join(parts) + " changed; ") if parts else ""
        if expiry_open:
            return "unknown", UNKNOWN, "%shistory diverges at %s, but gap %.0fs is past 5m with tier %s" % (what, where, gap, tier), "prefix_changed or expired"
        return ("prefix_changed", CONFIRMED,
                "%shistory diverges at %s from request #%d (gap %.0fs, expiry excluded)" % (what, where, p.i, gap), "")
    if link == "chain" and r.account and p.account and r.account != p.account and not expiry_open:
        # A different credential is a fact; that it caused the miss assumes the
        # upstream scopes its cache per credential, which nothing here shows.
        return "scope_changed", LIKELY, "request #%d was served by %s, this one by %s" % (p.i, p.account, r.account), ""
    hint = ""
    if expiry_open:
        hint = "expired if the previous write was the 5m tier"
    elif link == "chain":
        hint = "prefix intact and inside the TTL as sent by this gateway: the cause is downstream of it (gateway body rewrite, credential or upstream eviction)"
        if not (r.account and p.account):
            hint += "; scope_changed cannot be tested: no per-request credential"
    elif link == "heuristic":
        hint = "no prompt digest: prefix_changed and scope_changed cannot be tested"
    return "unknown", UNKNOWN, "%sprevious request #%d ended %.0fs earlier; tier %s" % (rel, p.i, gap, tier or "unknown"), hint


def run(rows, min_write):
    has_successor = link_by_arithmetic(rows)
    return [classify(r, rows, has_successor, min_write) for r in rows if r.cc >= min_write]


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("events")
    ap.add_argument("--min-write", type=int, default=100000, help="cache_creation tokens that make a rewrite")
    ap.add_argument("--model", help="target model (default: the most common one)")
    ap.add_argument("--json", action="store_true")
    args = ap.parse_args(argv)
    rows, model = load(args.events, args.model)
    result = run(rows, args.min_write)
    if args.json:
        json.dump([{"index": v.row.i, "end": v.row.end, "tot": v.row.tot, "cr": v.row.cr, "cc": v.row.cc, "label": v.label,
                    "confidence": v.confidence, "evidence": v.evidence, "hint": v.hint} for v in result], sys.stdout, indent=1)
        return 0
    print("model %s: %d requests, %d rewrites (cache_creation >= %d)" % (model, len(rows), len(result), args.min_write))
    for v in result:
        r = v.row
        print("#%-5d tot %7d read %7d write %7d  %-16s %-9s %s%s"
              % (r.i, r.tot, r.cr, r.cc, v.label, v.confidence, v.evidence, "  [open: %s]" % v.hint if v.hint else ""))
    tally = collections.defaultdict(lambda: [0, 0])
    for v in result:
        tally[(v.label, v.confidence)][0] += 1
        tally[(v.label, v.confidence)][1] += v.row.cc
    print("\nlabel              confidence  rewrites  tokens written")
    for (label, confidence), (n, cc) in sorted(tally.items(), key=lambda kv: -kv[1][1]):
        print("%-18s %-10s %9d  %14d" % (label, confidence, n, cc))
    print("\nVerdicts describe the body this gateway sent, not the one a gateway further upstream finally forwarded.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
