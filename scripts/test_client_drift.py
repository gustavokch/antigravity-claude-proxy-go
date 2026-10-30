"""Tests for scripts/client_drift.py: the pin registry against this tree, the
guarded writes, the drift classification and when a run is due."""

from __future__ import annotations

import json
import os
import shutil
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

import client_drift as drift
import tls_hello

ROOT = Path(__file__).resolve().parent.parent


class RegistryTest(unittest.TestCase):
    """The registry is only useful while it describes the tree it ships in."""

    def test_every_pin_reads_one_value_from_this_tree(self):
        for pin in drift.PINS:
            with self.subTest(pin=pin.name):
                drift.read_pin(ROOT, pin)

    def test_fingerprint_literals_match_the_replayed_hellos(self):
        for hello in drift.HELLOS:
            with self.subTest(hello=hello.name):
                shipped = tls_hello.fingerprints((ROOT / hello.path).read_bytes())
                self.assertEqual(drift.read_pin(ROOT, drift.PIN[hello.ja4_pin]), shipped["ja4"])
                self.assertEqual(drift.read_pin(ROOT, drift.PIN[hello.ja3_pin]), shipped["ja3"])

    def test_beta_list_survives_a_write_into_the_go_source(self):
        tree = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, tree)
        source = "internal/ccidentity/defaults.go"
        (tree / source).parent.mkdir(parents=True)
        shutil.copy(ROOT / source, tree / source)
        pin = drift.PIN["claude.betas"]
        site_only = drift.Pin(pin.name, pin.client, pin.pattern, pin.sites, decode=pin.decode, encode=pin.encode)
        betas = drift.read_pin(tree, site_only) + ["new-beta-2026-10-29"]
        drift.write_pin(tree, site_only, betas)
        self.assertEqual(drift.read_pin(tree, site_only), betas)


class WritePinTest(unittest.TestCase):
    def setUp(self):
        self.tree = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tree)
        (self.tree / "a.go").write_text('Version = "1.0.0"\nUA = "cli/1.0.0 (x)"\n')
        (self.tree / "b.txt").write_text("runs cli/1.0.0 (x)\n")
        self.pin = drift.Pin("demo", "demo", drift.SEMVER, (
            drift.Site("a.go", 'Version = "<<V>>"'),
            drift.Site("a.go", "cli/<<V>> (x)"),
            drift.Site("b.txt", "cli/<<V>> (x)"),
        ))

    def test_writes_every_site(self):
        changed = drift.write_pin(self.tree, self.pin, "1.2.0")
        self.assertEqual(changed, ["a.go", "b.txt"])
        self.assertEqual((self.tree / "a.go").read_text(), 'Version = "1.2.0"\nUA = "cli/1.2.0 (x)"\n')
        self.assertEqual(drift.read_pin(self.tree, self.pin), "1.2.0")

    def test_refuses_sites_that_disagree(self):
        (self.tree / "b.txt").write_text("runs cli/0.9.0 (x)\n")
        with self.assertRaises(drift.PinError):
            drift.write_pin(self.tree, self.pin, "1.2.0")
        self.assertIn('"1.0.0"', (self.tree / "a.go").read_text())

    def test_refuses_an_ambiguous_site(self):
        (self.tree / "b.txt").write_text("cli/1.0.0 (x) and cli/1.0.0 (x)\n")
        with self.assertRaises(drift.PinError):
            drift.read_pin(self.tree, self.pin)

    def test_refuses_a_value_outside_the_pattern(self):
        with self.assertRaises(drift.PinError):
            drift.write_pin(self.tree, self.pin, "1.2.0 (y)")


def record(path: str, headers: list[list[str]], body: dict | None = None, host: str = "h") -> dict:
    return {"method": "POST", "path": path, "host": host, "http_version": "HTTP/1.1",
            "headers": headers, "request_body": body or {}}


class CompareTest(unittest.TestCase):
    def test_a_pin_change_explains_only_its_own_difference(self):
        pairs = [("1.4.0", "1.4.10"), ("1000", "2000")]
        self.assertTrue(drift.explained(pairs, "agy/1.4.0 (cl=1000)", "agy/1.4.10 (cl=2000)"))
        self.assertFalse(drift.explained(pairs, "agy/1.4.0 (cl=1000)", "agy/1.4.10 (cl=2000; new)"))

    def test_reports_wire_changes_and_skips_explained_and_dynamic_values(self):
        base = record("/v1:load", [["Host", "a"], ["User-Agent", "agy/1.4.0"], ["Content-Type", "application/json"],
                                   ["Content-Length", "10"]],
                      {"top_level_keys": ["metadata", "mode"], "metadata": {"ideType": 9}})
        seen = record("/v1:load", [["Host", "a"], ["User-Agent", "agy/1.4.10"], ["Content-Length", "38"],
                                   ["Content-Type", "application/json"], ["X-New", "1"]],
                      {"top_level_keys": ["metadata"], "metadata": {"ideType": "ANTIGRAVITY"}}, host="other")
        drifts, notes = drift.compare_captures([base], [seen], frozenset({"host", "content-length"}),
                                               [("1.4.0", "1.4.10")])
        text = "\n".join(drifts)
        self.assertIn("host h -> other", text)
        self.assertIn("new headers: x-new", text)
        self.assertIn("header order", text)
        self.assertIn("body top_level_keys", text)
        self.assertIn("body metadata", text)
        self.assertNotIn("user-agent 'agy", text)
        self.assertNotIn("content-length '10'", text)
        self.assertEqual(notes, [])

    def test_requests_seen_on_one_side_are_notes_not_drift(self):
        drifts, notes = drift.compare_captures([record("/a", [])], [record("/b", [])], frozenset(), [])
        self.assertEqual(drifts, [])
        self.assertEqual(len(notes), 2)

    def test_generation_request_keys_are_compared_per_request_type(self):
        path = "/v1internal:streamGenerateContent?alt=sse"

        def stream(request_type: str, keys: list[str]) -> dict:
            return record(path, [], {"requestType": request_type, "shape": {"request": dict.fromkeys(keys, "x")}})

        agy = [stream("checkpoint", ["contents"]), stream("agent", ["contents", "labels", "sessionId", "tools"])]
        proxy = [stream("agent", ["contents", "sessionId", "toolConfig"])]
        drifts, notes = drift.compare_captures(agy, proxy, frozenset(), [])
        self.assertEqual(drifts, ["POST /v1internal:streamGenerateContent (agent): request keys no longer sent: labels"])
        self.assertEqual(notes, ["POST /v1internal:streamGenerateContent (checkpoint): in the baseline, not sent this run"])

    def test_generation_key_order_is_compared_when_both_sides_recorded_it(self):
        path = "/v1internal:streamGenerateContent?alt=sse"

        def stream(order: list[str] | None) -> dict:
            body = {"requestType": "agent"}
            if order is not None:
                body["request_key_order"] = order
            return record(path, [], body)

        agy = [stream(["contents", "systemInstruction", "tools", "labels", "generationConfig", "sessionId"])]
        sorted_proxy = [stream(["contents", "generationConfig", "labels", "sessionId", "systemInstruction", "toolConfig"])]
        drifts, _ = drift.compare_captures(agy, sorted_proxy, frozenset(), [])
        self.assertEqual(len(drifts), 1)
        self.assertIn("request_key_order", drifts[0])
        matching = [stream(["contents", "systemInstruction", "tools", "toolConfig", "labels", "generationConfig", "sessionId"])]
        self.assertEqual(drift.compare_captures(agy, matching, frozenset(), [])[0], [])
        self.assertEqual(drift.compare_captures([stream(None)], sorted_proxy, frozenset(), [])[0], [])

    def test_agy_user_agent_parts(self):
        capture = drift.Capture([record("/v1:load", [[
            "User-Agent",
            "antigravity/cli/1.4.10 (aidev_client; os_type=darwin; arch=arm64; cl=1009308193; auth_method=consumer)",
        ]])], {})
        self.assertEqual(drift.agy_user_agent(capture, "version"), "1.4.10")
        self.assertEqual(drift.agy_user_agent(capture, "cl"), "1009308193")
        self.assertEqual(drift.agy_user_agent(capture, "auth"), "consumer")


class StatusTest(unittest.TestCase):
    def setUp(self):
        self.state = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.state)
        patcher = mock.patch.dict(os.environ, {"CLIENT_DRIFT_STATE_DIR": self.state})
        patcher.start()
        self.addCleanup(patcher.stop)
        self.signature = {"agy": {"path": "/bin/agy", "size": 1, "mtime": 1}, "claude": None, "opencode": None}
        signature = mock.patch.object(drift, "signature", side_effect=lambda: dict(self.signature))
        signature.start()
        self.addCleanup(signature.stop)

    def write(self, state: dict) -> None:
        Path(self.state, "state.json").write_text(json.dumps(state))

    def test_due_until_a_run_records_the_installed_clients(self):
        self.assertTrue(drift.status()["due"])
        self.write({"signature": self.signature, "last": {"status": "pr"}})
        self.assertFalse(drift.status()["due"])
        self.signature["agy"] = {"path": "/bin/agy", "size": 2, "mtime": 2}
        current = drift.status()
        self.assertTrue(current["due"])
        self.assertEqual(current["changed"], ["agy"])

    def test_a_failed_run_is_retried_only_after_the_backoff(self):
        self.write({"signature": self.signature, "last": {"status": "error", "finished_epoch": int(time.time())}})
        self.assertFalse(drift.status()["due"])
        stale = int(time.time()) - drift.RETRY_AFTER_FAILURE - 1
        self.write({"signature": self.signature, "last": {"status": "error", "finished_epoch": stale}})
        self.assertTrue(drift.status()["due"])

    def test_disabled_is_never_due(self):
        with mock.patch.dict(os.environ, {"CLIENT_DRIFT_DISABLED": "1"}):
            self.assertFalse(drift.status()["due"])


if __name__ == "__main__":
    unittest.main()
