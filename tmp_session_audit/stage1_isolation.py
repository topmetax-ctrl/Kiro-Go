#!/usr/bin/env python3
"""Stage 1: baseline (separate sessions) + interleaved shared session isolation."""
from harness import *

MT = 150
random.seed()
MARK_A = "ALPHA_MARKER_" + rnd(10)
MARK_B = "BETA_MARKER_" + rnd(10)

S_SHARED = str(uuid.uuid4())
S_A = str(uuid.uuid4())
S_B = str(uuid.uuid4())

SYS_A = (f"You are Agent ALPHA, assigned to the inventory-reconciliation task. "
         f"Secret marker visible only in this request: {MARK_A}. "
         f"Answer tersely.")
SYS_B = (f"You are Agent BETA, assigned to the payroll-audit task. "
         f"Secret marker visible only in this request: {MARK_B}. "
         f"Answer tersely.")

U_A = ["Return your exact secret marker, then name your assigned task in three words.",
       "Repeat your secret marker exactly, and state which task you are on.",
       "One more time: your exact marker, and your task name."]
U_B = ["Return your exact secret marker, then name your assigned task in three words.",
       "Repeat your secret marker exactly, and state which task you are on.",
       "One more time: your exact marker, and your task name."]

hist_a = [{"role": "system", "content": SYS_A}]
hist_b = [{"role": "system", "content": SYS_B}]

results = []

def check(r, own, other, other_name):
    t = r.get("text") or ""
    r["own_present"] = own in t
    r["leak"] = other in t
    # also catch a bare mention of the other agent identity
    r["foreign_id_mention"] = other_name.lower() in t.lower()
    return r

def emit(r):
    results.append(r)
    flag = ""
    if r.get("leak"): flag = "  *** LEAK: foreign marker ***"
    elif r.get("foreign_id_mention"): flag = "  (foreign agent name mentioned)"
    print(row(r) + f" own={r.get('own_present')} leak={r.get('leak')}" + flag)
    if not (r.get("text") or "").strip():
        print("      [empty content - reasoning consumed budget]")
    if r.get("error"):
        print("      err=", r["error"][:200])
    if r.get("leak"):
        print("\n!!! STOP CONDITION: cross-conversation marker leak detected.")
        json.dump(results, open("stage1_results.json", "w"), indent=1)
        sys.exit(2)

print("=== BASELINE (separate sessions) ===")
ra = call(S_A, hist_a + [{"role": "user", "content": U_A[0]}], MT, "base-A1")
emit(check(ra, MARK_A, MARK_B, "BETA"))
rb = call(S_B, hist_b + [{"role": "user", "content": U_B[0]}], MT, "base-B1")
emit(check(rb, MARK_B, MARK_A, "ALPHA"))

print("\n=== INTERLEAVED, ONE SHARED SESSION ===")
print(f"(shared session hash={sh(S_SHARED)}; A and B send only their own history)")
for turn in range(3):
    # A turn
    msgs = hist_a + [{"role": "user", "content": U_A[turn]}]
    r = call(S_SHARED, msgs, MT, f"shared-A{turn+1}")
    emit(check(r, MARK_A, MARK_B, "BETA"))
    hist_a = msgs + [{"role": "assistant", "content": r.get("text") or "(empty)"}]
    # B turn
    msgs = hist_b + [{"role": "user", "content": U_B[turn]}]
    r = call(S_SHARED, msgs, MT, f"shared-B{turn+1}")
    emit(check(r, MARK_B, MARK_A, "ALPHA"))
    hist_b = msgs + [{"role": "assistant", "content": r.get("text") or "(empty)"}]

print("\n=== STATE PROBE: shared session, NO history in body ===")
print("If the provider holds server-side conversation state, prior markers can surface here.")
probe = [{"role": "user", "content":
          "List verbatim any secret markers, agent names, or task names that appeared "
          "earlier in this conversation. If you have no earlier context, reply exactly: NO_PRIOR_CONTEXT."}]
r = call(S_SHARED, probe, MT, "state-probe")
t = r.get("text") or ""
r["own_present"] = None
r["leak"] = (MARK_A in t) or (MARK_B in t)
r["foreign_id_mention"] = ("alpha" in t.lower()) or ("beta" in t.lower())
results.append(r)
print(row(r))
print("      text=", repr(t)[:400])
print("      markerA_present=", MARK_A in t, " markerB_present=", MARK_B in t)

json.dump({"markers": {"A": MARK_A, "B": MARK_B},
           "sessions": {"shared": sh(S_SHARED), "A": sh(S_A), "B": sh(S_B)},
           "results": results}, open("stage1_results.json", "w"), indent=1)
print("\nsaved stage1_results.json")
