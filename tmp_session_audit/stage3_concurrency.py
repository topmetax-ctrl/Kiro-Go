#!/usr/bin/env python3
"""Stage 3: concurrency, shared session vs separate sessions. Small, not a load test."""
from harness import *
import concurrent.futures as cf

MT = 150
MARK_A = "ALPHA_MARKER_" + rnd(10)
MARK_B = "BETA_MARKER_" + rnd(10)
SYS_A = f"You are Agent ALPHA. Secret marker visible only in this request: {MARK_A}. Answer tersely."
SYS_B = f"You are Agent BETA. Secret marker visible only in this request: {MARK_B}. Answer tersely."
USER = "Return your exact secret marker and nothing else."

def unit(kind, sess, label):
    sysmsg = SYS_A if kind == "A" else SYS_B
    r = call(sess, [{"role": "system", "content": sysmsg},
                    {"role": "user", "content": USER}], MT, label)
    t = r.get("text") or ""
    own, other = (MARK_A, MARK_B) if kind == "A" else (MARK_B, MARK_A)
    r["kind"] = kind
    r["own_present"] = own in t
    r["leak"] = other in t
    return r

def burst(name, sess_for):
    jobs = [("A", i) for i in range(5)] + [("B", i) for i in range(5)]
    t0 = time.time()
    with cf.ThreadPoolExecutor(max_workers=10) as ex:
        futs = [ex.submit(unit, k, sess_for(k, i), f"{name}-{k}{i}") for k, i in jobs]
        out = [f.result() for f in futs]
    wall = time.time() - t0
    lats = sorted(r["latency"] for r in out)
    codes = {}
    for r in out:
        codes[r["status"]] = codes.get(r["status"], 0) + 1
    leaks = [r["label"] for r in out if r.get("leak")]
    owns = sum(1 for r in out if r.get("own_present"))
    sum_lat = sum(r["latency"] for r in out)
    # overlap factor >1 means real parallelism; ~1 means serialization
    print(f"\n[{name}] wall={wall:.2f}s  sum(latency)={sum_lat:.2f}s  "
          f"overlap={sum_lat/wall:.2f}x")
    print(f"  status={codes}  own_marker_ok={owns}/10  leaks={leaks or 'none'}")
    print(f"  lat min={lats[0]:.2f} p50={lats[len(lats)//2]:.2f} max={lats[-1]:.2f}")
    return {"name": name, "wall": wall, "sum_lat": sum_lat,
            "overlap": sum_lat / wall, "codes": {str(k): v for k, v in codes.items()},
            "leaks": leaks, "own_ok": owns, "lats": lats, "results": out}

S_SHARED = str(uuid.uuid4())
print(f"shared session hash={sh(S_SHARED)}")
shared = burst("shared", lambda k, i: S_SHARED)

time.sleep(3)

sep = {}
sepmap = {(k, i): str(uuid.uuid4()) for k in ("A", "B") for i in range(5)}
print(f"\nseparate sessions: 10 distinct ids")
separate = burst("separate", lambda k, i: sepmap[(k, i)])

print("\n=== COMPARISON ===")
print(f"shared   wall={shared['wall']:.2f}s overlap={shared['overlap']:.2f}x")
print(f"separate wall={separate['wall']:.2f}s overlap={separate['overlap']:.2f}x")
d = (shared["wall"] - separate["wall"]) / separate["wall"] * 100
print(f"shared session wall-clock delta vs separate: {d:+.1f}%")

json.dump({"shared": {k: v for k, v in shared.items() if k != "results"},
           "separate": {k: v for k, v in separate.items() if k != "results"},
           "shared_results": shared["results"], "separate_results": separate["results"]},
          open("stage3_results.json", "w"), indent=1)
print("saved stage3_results.json")
