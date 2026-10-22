import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import classify_cache_rewrites as cl  # noqa: E402

MIN = 100000


def ev(end, lat=10, ttfb=1, tot=0, cr=0, cc=0, cc5=None, cc1=None, trace=None):
    u = {"source": "upstream", "inputTokens": tot, "cacheReadInputTokens": cr, "cacheCreationInputTokens": cc}
    if cc5 is not None:
        u["cacheCreation5mTokens"] = cc5
    if cc1 is not None:
        u["cacheCreation1hTokens"] = cc1
    e = {"time": int(end * 1000), "latencyMs": lat * 1000, "ttfbMs": ttfb * 1000, "status": 200, "targetModel": "m", "usage": u}
    if trace is not None:
        e["cache"] = trace
    return e


def trace(conv, msgs, prefix, **parts):
    return {"conv": conv, "msgs": msgs, "prefix": prefix, **parts}


def verdicts(items):
    rows, _ = cl.build(items)
    return {v.row.i: v for v in cl.run(rows, MIN)}


def labels(items):
    return {i: (v.label, v.evidence, v.hint) for i, v in verdicts(items).items()}


# A conversation of 4 messages, then a request extending it to 6.
P4 = trace("c", 4, ["1:a1", "2:a2", "3:a3", "4:a4"])
R6 = trace("c", 6, ["3:a3", "4:a4", "5:a5", "6:a6"])
R6_BROKEN = trace("c", 6, ["1:a1", "2:a2", "3:x3", "4:x4", "5:x5", "6:x6"])


class ClassifyTest(unittest.TestCase):
    def test_idle_past_every_ttl_is_expired(self):
        got = labels([ev(1000, tot=200000, cr=190000, cc=10000, trace=P4),
                      ev(5100, tot=210000, cr=13800, cc=196200, trace=R6)])
        label, evidence, _ = got[1]
        self.assertEqual(label, "expired")
        self.assertIn("3600", evidence)
        self.assertNotIn("heuristic", evidence)

    def test_five_minute_tier_expires_after_five_minutes(self):
        got = labels([ev(1000, tot=200000, cr=100, cc=199900, cc5=199900, cc1=0, trace=P4),
                      ev(1500, tot=210000, cr=13800, cc=196200, trace=R6)])
        self.assertEqual(got[1][0], "expired")
        self.assertIn("5m tier", got[1][1])

    def test_one_hour_tier_is_not_expired_after_ten_minutes(self):
        got = labels([ev(1000, tot=200000, cr=100, cc=199900, cc5=0, cc1=199900, trace=P4),
                      ev(1600, tot=210000, cr=13800, cc=196200, trace=R6)])
        label, _, hint = got[1]
        self.assertEqual(label, "unknown")
        self.assertIn("scope_changed cannot be tested", hint)

    def test_tier_is_read_through_a_pure_hit(self):
        p4 = trace("c", 4, ["1:a1", "2:a2", "3:a3", "4:a4"])
        p6 = trace("c", 6, ["3:a3", "4:a4", "5:a5", "6:a6"])
        r8 = trace("c", 8, ["5:a5", "6:a6", "7:a7", "8:a8"])
        got = labels([ev(1000, tot=200000, cr=100, cc=199900, cc5=199900, cc1=0, trace=p4),
                      ev(1100, tot=200500, cr=200500, cc=0, cc5=0, cc1=0, trace=p6),
                      ev(1500, tot=202000, cr=13800, cc=188200, trace=r8)])
        self.assertEqual(got[2][0], "expired")

    def test_unknown_tier_between_five_minutes_and_an_hour_stays_unknown(self):
        got = labels([ev(1000, tot=200000, cr=190000, cc=10000, trace=P4),
                      ev(1700, tot=210000, cr=13800, cc=196200, trace=R6)])
        label, _, hint = got[1]
        self.assertEqual(label, "unknown")
        self.assertIn("5m", hint)

    def test_broken_history_inside_ttl_is_prefix_changed(self):
        got = labels([ev(1000, tot=200000, cr=190000, cc=10000, trace=P4),
                      ev(1100, tot=210000, cr=13800, cc=196200, trace=R6_BROKEN)])
        label, evidence, _ = got[1]
        self.assertEqual(label, "prefix_changed")
        self.assertIn("messages 2..3", evidence)

    def test_changed_parts_are_named(self):
        p = trace("c", 4, ["1:a1", "2:a2", "3:a3", "4:a4"], tools="t1", system="s1", settings="g1")
        r = trace("c", 6, ["1:a1", "2:a2", "3:x3", "4:x4", "5:x5", "6:x6"], tools="t2", system="s1", settings="g1")
        got = labels([ev(1000, tot=200000, cr=190000, cc=10000, trace=p), ev(1100, tot=210000, cr=13800, cc=196200, trace=r)])
        self.assertEqual(got[1][0], "prefix_changed")
        self.assertIn("tools changed", got[1][1])

    def test_broken_history_after_five_minutes_with_unknown_tier_is_not_claimed(self):
        got = labels([ev(1000, tot=200000, cr=190000, cc=10000, trace=P4),
                      ev(1700, tot=210000, cr=13800, cc=196200, trace=R6_BROKEN)])
        label, _, hint = got[1]
        self.assertEqual(label, "unknown")
        self.assertEqual(hint, "prefix_changed or expired")

    def test_other_credential_is_scope_changed_only_with_both_known(self):
        a = ev(1000, tot=200000, cr=190000, cc=10000, trace=P4)
        a["accountLabel"] = "acct-1"
        b = ev(1060, tot=210000, cr=13800, cc=196200, trace=R6)
        b["accountLabel"] = "acct-2"
        self.assertEqual(labels([a, b])[1][0], "scope_changed")
        b["accountLabel"] = "acct-1"
        self.assertEqual(labels([a, b])[1][0], "unknown")
        del b["accountLabel"]
        self.assertEqual(labels([a, b])[1][0], "unknown")

    def test_identical_request_started_before_first_byte_is_concurrent(self):
        twin = ev(1030, lat=30, ttfb=20, tot=210000, cr=13800, cc=196200, trace=R6)
        late = ev(1035, lat=20, ttfb=2, tot=210000, cr=13800, cc=196200, trace=R6)
        got = labels([twin, late])
        self.assertEqual(got[1][0], "concurrent_write")
        self.assertIn("identical prompt digest", got[1][1])
        self.assertNotEqual(got[0][0], "concurrent_write")

    def test_overlap_alone_is_not_enough_when_digests_differ(self):
        other = trace("other", 6, ["5:z5", "6:z6"])
        twin = ev(1030, lat=30, ttfb=20, tot=210000, cr=13800, cc=196200, trace=other)
        late = ev(1035, lat=20, ttfb=2, tot=210000, cr=13800, cc=196200, trace=R6)
        self.assertNotEqual(labels([twin, late])[1][0], "concurrent_write")

    def test_request_after_first_byte_is_not_concurrent(self):
        first = ev(1030, lat=30, ttfb=2, tot=210000, cr=13800, cc=196200)
        second = ev(1100, lat=20, ttfb=2, tot=210000, cr=13800, cc=196200)
        self.assertNotEqual(labels([first, second])[1][0], "concurrent_write")

    def test_without_digests_the_previous_request_is_marked_heuristic(self):
        got = labels([ev(1000, tot=200000, cr=190000, cc=10000), ev(9000, tot=205000, cr=0, cc=205000)])
        label, evidence, _ = got[1]
        self.assertEqual(label, "expired")
        self.assertIn("heuristic", evidence)

    def test_no_previous_request_is_unknown(self):
        got = labels([ev(1000, tot=200000, cr=0, cc=200000)])
        self.assertEqual(got[0][0], "unknown")

    def test_only_large_writes_are_rewrites(self):
        rows, _ = cl.build([ev(1000, tot=200000, cr=190000, cc=10000), ev(1100, tot=210000, cr=200000, cc=10000)])
        self.assertEqual(cl.run(rows, MIN), [])

    def test_compaction_followup_is_reported(self):
        got = labels([ev(1000, tot=300000, cr=13800, cc=286200), ev(1020, lat=5, tot=60000, cr=13800, cc=46200)])
        self.assertIn("compaction?", got[0][1])


class ConcurrentBranchTest(unittest.TestCase):
    """Two requests that fork from one history overlap: neither could read the other's write."""

    def later(self, first_marks, branch_marks, first_ttfb, branch_end):
        first = trace("c", 6, ["3:a3", "4:a4", "5:x5", "6:x6"], marks=first_marks)
        branch = trace("c", 6, ["3:a3", "4:a4", "5:y5", "6:y6"], marks=branch_marks)
        got = verdicts([ev(1040, lat=40, ttfb=first_ttfb, tot=210000, cr=0, cc=210000, trace=first),
                        ev(branch_end, lat=20, ttfb=2, tot=210000, cr=0, cc=210000, trace=branch)])
        return next(v for v in got.values() if v.row.end == branch_end)

    def test_branches_sharing_a_breakpoint_entry_are_concurrent(self):
        got = self.later(["m3", "m5"], ["m3"], first_ttfb=20, branch_end=1033)
        self.assertEqual((got.label, got.confidence), ("concurrent_write", "confirmed"))
        self.assertIn("same history up to message 4", got.evidence)

    def test_branches_with_no_shared_entry_are_not_concurrent(self):
        got = self.later(["m5"], ["m3"], first_ttfb=20, branch_end=1033)
        self.assertNotEqual(got.label, "concurrent_write")

    def test_a_start_after_the_first_byte_is_not_concurrent(self):
        got = self.later(["m3", "m5"], ["m3"], first_ttfb=2, branch_end=1043)
        self.assertNotEqual(got.label, "concurrent_write")


class ConfidenceTest(unittest.TestCase):
    """How well the evidence supports a verdict, apart from which verdict it is."""

    def test_expiry_on_a_digest_chain_is_confirmed(self):
        v = verdicts([ev(1000, tot=200000, cr=190000, cc=10000, trace=P4),
                      ev(5100, tot=210000, cr=13800, cc=196200, trace=R6)])[1]
        self.assertEqual((v.label, v.confidence), ("expired", "confirmed"))

    def test_expiry_on_a_size_guessed_predecessor_is_only_likely(self):
        v = verdicts([ev(1000, tot=200000, cr=190000, cc=10000), ev(9000, tot=205000, cr=0, cc=205000)])[1]
        self.assertEqual((v.label, v.confidence), ("expired", "likely"))

    def test_five_minute_expiry_from_the_upstream_split_is_confirmed(self):
        v = verdicts([ev(1000, tot=200000, cr=100, cc=199900, cc5=199900, cc1=0, trace=P4),
                      ev(1500, tot=210000, cr=13800, cc=196200, trace=R6)])[1]
        self.assertEqual((v.label, v.confidence), ("expired", "confirmed"))

    def test_concurrency_needs_a_digest_to_be_confirmed(self):
        twin = ev(1030, lat=30, ttfb=20, tot=210000, cr=13800, cc=196200, trace=R6)
        late = ev(1035, lat=20, ttfb=2, tot=210000, cr=13800, cc=196200, trace=R6)
        self.assertEqual(verdicts([twin, late])[1].confidence, "confirmed")
        twin, late = ev(1030, lat=30, ttfb=20, tot=210000, cr=13800, cc=196200), ev(1035, lat=20, ttfb=2, tot=210000, cr=13800, cc=196200)
        v = verdicts([twin, late])[1]
        self.assertEqual((v.label, v.confidence), ("concurrent_write", "likely"))

    def test_different_settings_are_not_the_same_request(self):
        a = trace("c", 6, ["5:a5", "6:a6"], settings="g1")
        b = trace("c", 6, ["5:a5", "6:a6"], settings="g2")
        twin = ev(1030, lat=30, ttfb=20, tot=210000, cr=13800, cc=196200, trace=a)
        late = ev(1035, lat=20, ttfb=2, tot=210000, cr=13800, cc=196200, trace=b)
        self.assertNotEqual(verdicts([twin, late])[1].label, "concurrent_write")

    def test_changed_history_is_confirmed(self):
        v = verdicts([ev(1000, tot=200000, cr=190000, cc=10000, trace=P4),
                      ev(1100, tot=210000, cr=13800, cc=196200, trace=R6_BROKEN)])[1]
        self.assertEqual((v.label, v.confidence), ("prefix_changed", "confirmed"))

    def test_a_credential_change_is_never_more_than_likely(self):
        a = ev(1000, tot=200000, cr=190000, cc=10000, trace=P4)
        a["accountLabel"] = "acct-1"
        b = ev(1060, tot=210000, cr=13800, cc=196200, trace=R6)
        b["accountLabel"] = "acct-2"
        v = verdicts([a, b])[1]
        self.assertEqual((v.label, v.confidence), ("scope_changed", "likely"))

    def test_unknown_stays_unknown(self):
        v = verdicts([ev(1000, tot=200000, cr=0, cc=200000)])[0]
        self.assertEqual((v.label, v.confidence), ("unknown", "unknown"))

    def test_intact_history_inside_ttl_points_downstream(self):
        v = verdicts([ev(1000, tot=200000, cr=100, cc=199900, cc5=0, cc1=199900, trace=P4),
                      ev(1060, tot=210000, cr=13800, cc=196200, trace=R6)])[1]
        self.assertEqual((v.label, v.confidence), ("unknown", "unknown"))
        self.assertIn("downstream", v.hint)


class CacheKeyTest(unittest.TestCase):
    """Equal prompt digests are not equal cache entries: breakpoints and tier count too."""

    def pair(self, prev_marks, now_marks, prev_cc=(0, 199900), now_cc=(None, None)):
        p = trace("c", 4, ["1:a1", "2:a2", "3:a3", "4:a4"], marks=prev_marks)
        r = trace("c", 6, ["3:a3", "4:a4", "5:a5", "6:a6"], marks=now_marks)
        return verdicts([ev(1000, tot=200000, cr=100, cc=199900, cc5=prev_cc[0], cc1=prev_cc[1], trace=p),
                         ev(1060, tot=210000, cr=13800, cc=196200, cc5=now_cc[0], cc1=now_cc[1], trace=r)])[1]

    def test_a_marker_that_moves_with_the_history_is_the_same_layout(self):
        self.assertNotIn("breakpoints changed", self.pair(["s0:1h@global", "m3:1h"], ["s0:1h@global", "m5:1h"]).evidence)

    def test_a_changed_ttl_or_scope_or_count_is_reported(self):
        for now in (["s0:1h@global", "m5"], ["s0:1h", "m5:1h"], ["m5:1h"]):
            self.assertIn("breakpoints changed", self.pair(["s0:1h@global", "m3:1h"], now).evidence, now)

    def test_a_changed_distance_from_the_newest_message_is_reported(self):
        self.assertIn("breakpoints changed", self.pair(["m3"], ["m3"]).evidence)

    def test_a_changed_tier_is_reported(self):
        v = self.pair(["m3:1h"], ["m5:1h"], prev_cc=(0, 199900), now_cc=(196200, 0))
        self.assertIn("TTL tier changed 1h -> 5m", v.evidence)

    def test_the_breakpoint_entry_links_requests_the_whole_prompt_does_not(self):
        # The previous request cached up to message 2, but ends at 4. The next one
        # lists the digest at 2 and not the one at 4, so only the breakpoint entry
        # connects them.
        p = trace("c", 4, ["1:a1", "2:a2", "3:a3", "4:a4"], marks=["m1"])
        r = trace("c", 6, ["2:a2", "3:y3", "4:y4", "5:y5", "6:y6"])
        got = verdicts([ev(1000, tot=200000, cr=100, cc=199900, cc5=0, cc1=199900, trace=p),
                        ev(1060, tot=210000, cr=13800, cc=196200, trace=r)])[1]
        self.assertEqual(got.label, "unknown")
        self.assertIn("downstream", got.hint)


if __name__ == "__main__":
    unittest.main()
