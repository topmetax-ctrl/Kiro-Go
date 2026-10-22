import http.server
import io
import json
import os
import re
import sys
import tempfile
import threading
import unittest
from contextlib import redirect_stderr, redirect_stdout

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import cache_audit_store as store  # noqa: E402
import classify_cache_rewrites as cl  # noqa: E402

DAY = 86400
NOW = 1_800_000_000.0

PERSONAL = ["Alice Nguyen", "key-id-123", "10.1.2.3", "conn-id-9", "conn-name-x", "req-abc", "prov-id-1", "route-id-1",
            "claude-acct@example.com", "upstream said: secret detail"]


def full_event(end, lat=10, ttfb=1, tot=0, cr=0, cc=0, cc5=None, cc1=None, trace=None, account=None):
    u = {"source": "upstream", "protocol": "claude", "inputTokens": tot, "outputTokens": 5,
         "cacheReadInputTokens": cr, "cacheCreationInputTokens": cc}
    if cc5 is not None:
        u["cacheCreation5mTokens"] = cc5
    if cc1 is not None:
        u["cacheCreation1hTokens"] = cc1
    e = {"time": int(end * 1000), "latencyMs": lat * 1000, "ttfbMs": ttfb * 1000, "status": 200, "ok": True, "stream": True,
         "targetModel": "m", "clientModel": "c", "upstreamModel": "m", "endpoint": "claude", "usage": u,
         "sessionSource": "claude-code", "sessionScope": "conversation", "inputTokens": tot, "outputTokens": 5,
         "apiKeyName": "Alice Nguyen", "apiKeyId": "key-id-123", "clientIp": "10.1.2.3", "connectionId": "conn-id-9",
         "connectionName": "conn-name-x", "requestId": "req-abc", "providerId": "prov-id-1", "providerName": "CLIProxyAPI",
         "routeId": "route-id-1", "errorMsg": "upstream said: secret detail", "futureField": "who knows"}
    if account:
        e["accountLabel"] = account
    if trace:
        e["cache"] = trace
    return e


P4 = {"conv": "c", "msgs": 4, "prefix": ["1:a1", "2:a2", "3:a3", "4:a4"], "marks": ["m3"], "sess": "s" * 24, "parent": "p" * 24,
      "signals": {"userId": "json", "parentSession": "present", "subagentMarker": "absent", "agentHeader": "absent",
                  "probe": "absent", "titleHelper": "absent", "extendedTtlBeta": "absent", "surprise": "x"}}
R6 = {"conv": "c", "msgs": 6, "prefix": ["3:a3", "4:a4", "5:a5", "6:a6"], "marks": ["m5"]}


def export(items):
    return {"exported": len(items), "exportedAt": "2026-10-22T10:00:00Z", "total": len(items), "items": items}


class MinimiseTest(unittest.TestCase):
    def test_nothing_personal_survives(self):
        doc = export([full_event(1000, tot=200000, cr=190000, cc=10000, trace=P4, account="claude-acct@example.com")])
        raw = json.dumps(store.minimise(doc, b"salt"))
        for secret in PERSONAL + ["futureField", "who knows", "surprise"]:
            self.assertNotIn(secret, raw)

    def test_analysis_fields_survive(self):
        e = full_event(1000, tot=200000, cr=190000, cc=10000, cc5=0, cc1=10000, trace=P4)
        got = store.minimise(export([e]), b"salt")["items"][0]
        self.assertEqual(got["time"], e["time"])
        self.assertEqual(got["usage"]["cacheCreation1hTokens"], 10000)
        self.assertEqual(got["cache"]["prefix"], P4["prefix"])
        self.assertEqual(got["cache"]["sess"], "s" * 24)
        self.assertEqual(got["cache"]["signals"]["parentSession"], "present")
        self.assertEqual(got["status"], 200)

    def test_credential_label_is_a_token_stable_within_one_snapshot(self):
        items = [full_event(1000, account="acct-1"), full_event(1100, account="acct-1"), full_event(1200, account="acct-2")]
        a = [e["accountLabel"] for e in store.minimise(export(items), b"salt-1")["items"]]
        self.assertEqual(a[0], a[1])
        self.assertNotEqual(a[0], a[2])
        self.assertNotIn("acct-1", a[0])
        b = [e["accountLabel"] for e in store.minimise(export(items), b"salt-2")["items"]]
        self.assertNotEqual(a[0], b[0])

    def test_events_without_optional_parts_are_kept(self):
        got = store.minimise(export([{"time": 5, "status": 500}]), b"s")
        self.assertEqual(got["items"], [{"time": 5, "status": 500}])
        self.assertEqual(got["total"], 1)

    def test_a_bare_list_is_accepted(self):
        self.assertEqual(store.minimise([{"time": 5}], b"s")["total"], 1)

    def test_classifier_gives_the_same_answer_on_the_minimised_data(self):
        items = [
            full_event(1000, tot=200000, cr=190000, cc=10000, trace=P4, account="acct-1"),
            full_event(1060, tot=210000, cr=13800, cc=196200, trace=R6, account="acct-2"),   # credential changed
            full_event(5200, tot=210000, cr=13800, cc=196200, cc5=196200, cc1=0, trace=R6, account="acct-2"),  # idle
            full_event(5230, lat=30, ttfb=20, tot=300000, cr=0, cc=300000, trace=R6, account="acct-2"),
            full_event(5235, lat=20, ttfb=2, tot=300000, cr=0, cc=300000, trace=R6, account="acct-2"),   # concurrent
        ]

        def verdicts(doc):
            # The evidence names credentials; the minimised one names their tokens.
            rows, _ = cl.build(doc["items"])
            return [(v.row.i, v.label, v.confidence, re.sub(r"acct-[0-9a-f]{8}|acct-\d", "ACCT", v.evidence), v.hint)
                    for v in cl.run(rows, 100000)]

        raw, small = export(items), store.minimise(export(items), b"salt")
        self.assertEqual(verdicts(small), verdicts(raw))
        self.assertGreaterEqual(len(verdicts(small)), 3)
        self.assertIn("scope_changed", [v[1] for v in verdicts(small)])


class StoreTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = os.path.join(self.tmp.name, "audit")
        self.old_umask = os.umask(0o022)  # a loose umask: the store must not depend on the caller's
        self.addCleanup(os.umask, self.old_umask)

    def run_main(self, *argv):
        out, err = io.StringIO(), io.StringIO()
        with redirect_stdout(out), redirect_stderr(err):
            code = store.main(list(argv))
        return code, out.getvalue(), err.getvalue()

    def source(self, items=None):
        path = os.path.join(self.tmp.name, "export.json")
        with open(path, "w") as fh:
            json.dump(export(items or [full_event(1000, tot=10, cr=5, cc=5)]), fh)
        return path

    def mode(self, path):
        return os.stat(path).st_mode & 0o777

    def test_save_writes_a_private_file_in_a_private_directory(self):
        code, out, _ = self.run_main("save", "--source", self.source(), "--dir", self.root)
        self.assertEqual(code, 0)
        self.assertEqual(self.mode(self.root), 0o700)
        snaps = os.path.join(self.root, "exports")
        self.assertEqual(self.mode(snaps), 0o700)
        names = os.listdir(snaps)
        self.assertEqual(len(names), 1)
        self.assertTrue(names[0].startswith("audit-") and names[0].endswith(".json"))
        self.assertEqual(self.mode(os.path.join(snaps, names[0])), 0o600)
        self.assertIn("saved 1 events", out)
        with open(os.path.join(snaps, names[0])) as fh:
            saved = json.load(fh)
        self.assertTrue(saved["minimised"])
        self.assertNotIn("Alice", json.dumps(saved))

    def test_a_loose_directory_is_tightened(self):
        os.makedirs(os.path.join(self.root, "exports"), mode=0o755)
        os.chmod(self.root, 0o755)
        self.run_main("save", "--source", self.source(), "--dir", self.root)
        self.assertEqual(self.mode(self.root), 0o700)
        self.assertEqual(self.mode(os.path.join(self.root, "exports")), 0o700)

    def test_two_saves_in_one_second_do_not_overwrite(self):
        snaps = store.ensure_dir(self.root)
        a = store.write_snapshot(snaps, {"items": []}, NOW)
        b = store.write_snapshot(snaps, {"items": []}, NOW)
        self.assertNotEqual(a, b)
        self.assertEqual(len(os.listdir(snaps)), 2)

    def test_refuses_a_git_work_tree(self):
        repo = os.path.join(self.tmp.name, "repo")
        os.makedirs(os.path.join(repo, ".git"))
        with self.assertRaises(SystemExit) as ctx:
            self.run_main("save", "--source", self.source(), "--dir", os.path.join(repo, "audit"))
        self.assertIn("git work tree", str(ctx.exception))
        self.assertFalse(os.path.exists(os.path.join(repo, "audit")))

    def test_refuses_a_directory_that_is_not_a_store(self):
        os.makedirs(self.root, mode=0o755)
        with open(os.path.join(self.root, "notes.txt"), "w") as fh:
            fh.write("mine")
        with self.assertRaises(SystemExit):
            self.run_main("save", "--source", self.source(), "--dir", self.root)
        self.assertEqual(os.listdir(self.root), ["notes.txt"])
        self.assertEqual(self.mode(self.root), 0o755, "a directory that is not ours keeps its mode")

    def test_live_export_needs_the_password_from_the_environment(self):
        os.environ.pop("KIRO_GO_ADMIN_PASSWORD", None)
        with self.assertRaises(SystemExit) as ctx:
            self.run_main("save", "--url", "http://127.0.0.1:1", "--dir", self.root)
        self.assertIn("KIRO_GO_ADMIN_PASSWORD", str(ctx.exception))

    def test_live_export_sends_the_password_as_a_header_and_saves_minimised(self):
        seen = {}

        class H(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                seen["path"], seen["pw"] = self.path, self.headers.get("X-Admin-Password")
                body = json.dumps(export([full_event(1000, tot=10, cr=5, cc=5)])).encode()
                self.send_response(200)
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *a):
                pass

        srv = http.server.HTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.addCleanup(srv.server_close)
        self.addCleanup(srv.shutdown)
        os.environ["KIRO_GO_ADMIN_PASSWORD"] = "pw-123"
        self.addCleanup(os.environ.pop, "KIRO_GO_ADMIN_PASSWORD", None)
        code, out, _ = self.run_main("save", "--url", "http://127.0.0.1:%d" % srv.server_port, "--dir", self.root)
        self.assertEqual(code, 0)
        self.assertEqual(seen["pw"], "pw-123")
        self.assertEqual(seen["path"], "/admin/api/forward-events/export?format=json")
        self.assertNotIn("pw-123", out)

    def test_a_non_http_url_is_refused(self):
        os.environ["KIRO_GO_ADMIN_PASSWORD"] = "x"
        self.addCleanup(os.environ.pop, "KIRO_GO_ADMIN_PASSWORD", None)
        with self.assertRaises(SystemExit):
            self.run_main("save", "--url", "file:///etc/passwd", "--dir", self.root)


class PruneTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.snaps = store.ensure_dir(os.path.join(self.tmp.name, "audit"))

    def make(self, name, age_days, size=10):
        path = os.path.join(self.snaps, name)
        with open(path, "wb") as fh:
            fh.write(b"x" * size)
        t = NOW - age_days * DAY
        os.utime(path, (t, t))
        return path

    def left(self):
        return sorted(os.listdir(self.snaps))

    def test_snapshots_past_the_retention_are_deleted_and_newer_kept(self):
        self.make("audit-old.json", 31)
        self.make("audit-edge.json", 29)
        self.make("audit-new.json", 1)
        deleted, over = store.prune(self.snaps, NOW)
        self.assertEqual(self.left(), ["audit-edge.json", "audit-new.json"])
        self.assertEqual([os.path.basename(d) for d in deleted], ["audit-old.json"])
        self.assertEqual(over, 0)

    def test_the_size_cap_deletes_the_oldest_first(self):
        mb = 1024 * 1024
        self.make("audit-a.json", 10, mb)
        self.make("audit-b.json", 5, mb)
        self.make("audit-c.json", 3, mb)
        store.prune(self.snaps, NOW, max_mb=2)
        self.assertEqual(self.left(), ["audit-b.json", "audit-c.json"])

    def test_a_snapshot_of_a_running_audit_is_never_deleted(self):
        mb = 1024 * 1024
        self.make("audit-a.json", 0.5, mb)   # 12 h old: inside the protected window
        self.make("audit-b.json", 1.0, mb)
        deleted, over = store.prune(self.snaps, NOW, max_mb=1)
        self.assertEqual(self.left(), ["audit-a.json", "audit-b.json"])
        self.assertEqual(deleted, [])
        self.assertGreater(over, 0)  # reported, not forced

    def test_the_protection_also_holds_against_the_age_rule(self):
        self.make("audit-a.json", 0.5)
        store.prune(self.snaps, NOW, max_days=0)
        self.assertEqual(self.left(), ["audit-a.json"])

    def test_only_snapshots_are_touched(self):
        self.make("notes.txt", 400)
        self.make("audit-x.txt", 400)
        self.make("audit-old.json", 400)
        store.prune(self.snaps, NOW)
        self.assertEqual(self.left(), ["audit-x.txt", "notes.txt"])

    def test_a_crashed_save_leaves_no_litter(self):
        stale = self.make(".tmp-123", 1)
        live = self.make(".tmp-456", 0)
        os.utime(live, (NOW - 10, NOW - 10))
        store.prune(self.snaps, NOW)
        self.assertFalse(os.path.exists(stale))
        self.assertTrue(os.path.exists(live))

    def test_dry_run_deletes_nothing(self):
        self.make("audit-old.json", 400)
        deleted, _ = store.prune(self.snaps, NOW, dry_run=True)
        self.assertEqual(len(deleted), 1)
        self.assertEqual(self.left(), ["audit-old.json"])

    def test_prune_command_reports_a_cap_it_cannot_meet(self):
        mb = 1024 * 1024
        self.make("audit-a.json", 0.2, mb)
        out, err = io.StringIO(), io.StringIO()
        with redirect_stdout(out), redirect_stderr(err):
            code = store.main(["prune", "--dir", os.path.dirname(self.snaps), "--max-mb", "0"])
        self.assertEqual(code, 2)
        self.assertIn("over the size cap", err.getvalue())


if __name__ == "__main__":
    unittest.main()
