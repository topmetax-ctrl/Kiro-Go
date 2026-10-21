#!/usr/bin/env python3
"""Stage 2: cache matrix. same/different content x same/different session."""
from harness import *

MT = 60  # keep output cheap; cache behavior is read from prompt_tokens side

def prefix(tag, n=520):
    """Deterministic, distinct, ~3-4k token filler. Byte-identical across calls."""
    rr = random.Random(tag)          # seeded by tag => stable content per tag
    words = ["ledger", "variance", "accrual", "tolerance", "batch", "reconcile",
             "threshold", "vendor", "invoice", "adjustment", "posting", "schedule"]
    lines = [f"DOMAIN {tag} SPECIFICATION"]
    for i in range(n):
        lines.append(f"{tag}-rule-{i:04d}: when {rr.choice(words)} of {rr.randint(1000,9999)} "
                     f"exceeds {rr.choice(words)} bound {rr.randint(10,99)}, escalate to "
                     f"{rr.choice(words)} tier {rr.randint(1,9)} and record {rr.choice(words)}.")
    return "\n".join(lines)

P = {t: prefix(t) for t in ("P_A", "P_B", "P_C")}
for t, v in P.items():
    print(f"{t}: {len(v)} chars (~{len(v)//4} tok est)")

S_SHARED = str(uuid.uuid4())
S_OTHER = str(uuid.uuid4())
print(f"S_SHARED hash={sh(S_SHARED)}  S_OTHER hash={sh(S_OTHER)}\n")

def msgs(tag):
    return [{"role": "system", "content": P[tag]},
            {"role": "user", "content": f"Reply with exactly the token {tag}_ACK and nothing else."}]

res = []
def go(tag, sess, label, pause=2.0):
    r = call(sess, msgs(tag), MT, label)
    r["prefix"] = tag
    res.append(r)
    print(row(r), " text=", repr((r.get("text") or ""))[:40])
    if r.get("error"): print("      err=", r["error"][:200])
    time.sleep(pause)
    return r

print("--- cold fills, both prefixes on the SAME session ---")
go("P_A", S_SHARED, "A-cache-1")
go("P_B", S_SHARED, "B-cache-1")

print("\n--- reuse, same session, same content ---")
go("P_A", S_SHARED, "A-cache-2")
go("P_B", S_SHARED, "B-cache-2")

print("\n--- unrelated content, same session (expect ~0 cache read) ---")
go("P_C", S_SHARED, "C-unrelated")

print("\n--- same content P_A, DIFFERENT session (control) ---")
go("P_A", S_OTHER, "A-diff-sess")

print("\n--- second turn on the different session, to confirm it can warm too ---")
go("P_A", S_OTHER, "A-diff-sess-2")

json.dump({"sessions": {"shared": sh(S_SHARED), "other": sh(S_OTHER)},
           "results": res}, open("stage2_results.json", "w"), indent=1)
print("\nsaved stage2_results.json")
