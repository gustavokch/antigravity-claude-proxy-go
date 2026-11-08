"""Tests for scripts/client_drift.py: the pin registry against this tree, the
guarded writes, the drift classification and when a run is due."""

from __future__ import annotations

import json
import os
import shutil
import sys
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

    def test_beta_lists_survive_a_write_into_the_go_source(self):
        source = "internal/ccidentity/defaults.go"
        lists = ("claude.betas", "claude.count_tokens_betas")
        for name in lists:
            with self.subTest(pin=name):
                tree = Path(tempfile.mkdtemp())
                self.addCleanup(shutil.rmtree, tree)
                (tree / source).parent.mkdir(parents=True)
                shutil.copy(ROOT / source, tree / source)
                pin = drift.PIN[name]
                site_only = drift.Pin(pin.name, pin.client, pin.pattern, pin.sites, decode=pin.decode, encode=pin.encode)
                betas = drift.read_pin(tree, site_only) + ["new-beta-2026-10-29"]
                drift.write_pin(tree, site_only, betas)
                self.assertEqual(drift.read_pin(tree, site_only), betas)
                # The two lists sit in one file; writing one must not touch the other.
                other = drift.PIN[lists[1] if name == lists[0] else lists[0]]
                self.assertEqual(drift.read_pin(tree, other), drift.read_pin(ROOT, other))


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
        proxy = [stream("agent", ["contents", "sessionId"])]
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
        sorted_proxy = [stream(["contents", "generationConfig", "labels", "sessionId", "systemInstruction"])]
        drifts, _ = drift.compare_captures(agy, sorted_proxy, frozenset(), [])
        self.assertEqual(len(drifts), 1)
        self.assertIn("request_key_order", drifts[0])
        matching = [stream(["contents", "systemInstruction", "labels", "generationConfig", "sessionId"])]
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

    def test_opencode_is_read_from_either_zen_route(self):
        # 2.2.3 left /zen/v1 for /inference/<family>/v1; reading only the old
        # route left its version and ClientHello unchecked.
        for path in ("/zen/v1/chat/completions", "/inference/openai/v1/chat/completions"):
            seen = record(path, [["User-Agent", "opencode/latest/2.2.3/cli"]])
            capture = drift.Capture([record("/other", []), seen], {})
            self.assertEqual(drift.opencode_version(capture), "2.2.3", path)
            zen = next(hello for hello in drift.HELLOS if hello.name == "zen")
            self.assertIs(zen.select(capture), seen, path)

    def test_a_request_only_this_build_sends_needs_a_human(self):
        # agy 1.5.2's print mode stopped sending the title checkpoint the proxy
        # sends; the run dropped that one-sided request, so its shape went
        # unchecked behind a report of zero manual items.
        path = "/v1internal:streamGenerateContent?alt=sse"
        agy = drift.Capture([record("/v1internal:loadCodeAssist", []), record("/v1internal:fetchAdminControls", []),
                             record(path, [], {"requestType": "agent"})], {})
        build = drift.Capture([record("/v1internal:loadCodeAssist", []), record(path, [], {"requestType": "checkpoint"}),
                               record(path, [], {"requestType": "agent"})], {})
        result = drift.Result("agy", "1.5.2", agy)
        # The sequence check reads the committed baseline; this test is about one-sided requests.
        no_baseline = ROOT / "no-baseline.jsonl"
        with mock.patch.object(drift, "analyze_generic"), mock.patch.object(drift, "baseline_path", return_value=no_baseline):
            drift.analyze_agy(ROOT, result, agy, build, None, [])
        self.assertEqual(result.manual, [
            "this build sends POST /v1internal:streamGenerateContent (checkpoint), which agy did not send this run, "
            "so its shape is unchecked",
        ])

    def test_agy_sequence_ignores_order_inside_a_concurrent_group(self):
        # agy sends fetchAdminControls, fetchUserInfo and retrieveUserQuotaSummary
        # together; agy.jsonl shows them in both orders within one run.
        first = [record(f"/v1internal:{name}", []) for name in (
            "loadCodeAssist", "fetchAdminControls", "fetchUserInfo", "retrieveUserQuotaSummary", "fetchAvailableModels")]
        second = [record(f"/v1internal:{name}", []) for name in (
            "loadCodeAssist", "retrieveUserQuotaSummary", "fetchAdminControls", "fetchUserInfo", "fetchAvailableModels")]
        self.assertEqual(drift.agy_call_sequence(first), drift.agy_call_sequence(second))
        self.assertEqual(drift.agy_call_sequence(first), [
            "loadCodeAssist", "fetchAdminControls", "fetchUserInfo", "retrieveUserQuotaSummary", "fetchAvailableModels"])

    def test_a_changed_agy_sequence_needs_a_human(self):
        # The baseline is rewritten only when a pin changes, so a reorder alone
        # would leave the start-burst sequence test on agy's old order.
        def calls(*names):
            out = []
            for name in names:
                if name == "cascadeNuxes":
                    out.append(dict(record("/v1internal/cascadeNuxes?page_size=1000", []), method="GET"))
                else:
                    out.append(record(f"/v1internal:{name}", []))
            return out
        baseline = calls("loadCodeAssist", "fetchUserInfo", "listExperiments", "cascadeNuxes")
        moved = calls("loadCodeAssist", "listExperiments", "fetchUserInfo", "cascadeNuxes")
        path = Path(tempfile.mkdtemp()) / "agy.jsonl"
        self.addCleanup(shutil.rmtree, path.parent)
        path.write_text("".join(json.dumps(item) + "\n" for item in baseline), encoding="utf-8")
        for observed, changed in ((baseline, False), (moved, True)):
            result = drift.Result("agy", "1.5.2", drift.Capture(observed, {}))
            with mock.patch.object(drift, "analyze_generic"), mock.patch.object(drift, "baseline_path", return_value=path):
                drift.analyze_agy(ROOT, result, drift.Capture(observed, {}), None, None, [])
            found = [item for item in result.manual if item.startswith("agy's Cloud Code call sequence changed")]
            self.assertEqual(bool(found), changed, result.manual)
        self.assertIn("baseline loadCodeAssist → fetchUserInfo → listExperiments → cascadeNuxes", found[0])


class AdvisorBetaTest(unittest.TestCase):
    """The drift capture never turns the advisor on, so its beta is read from the binary."""

    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.dir)
        self.pinned = drift.read_pin(ROOT, drift.PIN["claude.advisor_beta"])

    def binary(self, content: bytes) -> str:
        path = self.dir / "claude"
        path.write_bytes(content)
        return str(path)

    def analyze(self, content: bytes) -> drift.Result:
        result = drift.Result("claude", None, drift.Capture([], {}))
        with mock.patch.object(drift, "binary_path", return_value=self.binary(content)):
            drift.analyze_claude(ROOT, result, result.capture, [])
        return result

    def test_reads_the_one_name_the_bundle_carries(self):
        # 2.1.311 carries it twice: in a string table and in its beta registry.
        path = self.binary(b'\x00advisor-tool-2026-03-01\x00n3t=v("advisor_tool","advisor-tool-2026-03-01")')
        self.assertEqual(drift.claude_advisor_beta(path), "advisor-tool-2026-03-01")

    def test_no_name_or_two_names_is_no_reading(self):
        self.assertIsNone(drift.claude_advisor_beta(self.binary(b'"advisor_tool"')))
        self.assertIsNone(drift.claude_advisor_beta(self.binary(b"")))
        self.assertIsNone(drift.claude_advisor_beta(self.binary(b"advisor-tool-2026-03-01 advisor-tool-2026-11-01")))
        self.assertIsNone(drift.claude_advisor_beta(None))

    def test_the_pinned_name_is_no_finding(self):
        result = self.analyze(f'"advisor_tool","{self.pinned}"'.encode())
        self.assertEqual(result.pin_changes, [])
        self.assertFalse([item for item in result.manual if "advisor" in item])

    def test_a_renamed_beta_moves_the_pin_and_needs_a_human(self):
        # A rename can move the beta in the list too, and only a person can
        # read that from the bundle.
        result = self.analyze(b'"advisor_tool","advisor-tool-2099-01-01"')
        self.assertEqual([(pin.name, old, new) for pin, old, new in result.pin_changes],
                         [("claude.advisor_beta", self.pinned, "advisor-tool-2099-01-01")])
        self.assertTrue([item for item in result.manual if "advisorBetaPrecedes" in item])

    def test_a_binary_without_the_beta_needs_a_human(self):
        result = self.analyze(b"no advisor here")
        self.assertEqual(result.pin_changes, [])
        self.assertTrue([item for item in result.manual if "claude.advisor_beta" in item])


# A stand-in for agy's interactive mode: the trust dialog, a turn once the
# dialog is answered, and then it waits for the user like the real client.
FAKE_TERMINAL_CLIENT = """
import os, sys, time
progress, mode = sys.argv[1], sys.argv[2]
open(progress + ".pid", "w").write(str(os.getpid()))
if mode == "exit":
    sys.exit(3)
print("Do you trust the contents of this project?")
print("> Yes, I trust this folder", flush=True)
sys.stdin.readline()
if mode == "turn":
    open(progress, "a").write("agent\\n")
time.sleep(60)
"""


class TerminalRunTest(unittest.TestCase):
    """agy's title checkpoint is sent only in interactive mode, so the capture
    drives it on a terminal; the run must end on its own in every case."""

    def setUp(self):
        self.work = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.work)
        self.client = self.work / "client.py"
        self.client.write_text(FAKE_TERMINAL_CLIENT)
        self.progress = self.work / "progress"

    def run_client(self, mode: str, timeout: int = 20) -> dict:
        def calls() -> int:
            return len(self.progress.read_text().splitlines()) if self.progress.exists() else 0

        return drift.run_on_terminal([sys.executable, str(self.client), str(self.progress), mode],
                                     dict(os.environ), self.work, calls, timeout=timeout, settle=0.3)

    def assert_gone(self) -> None:
        pid = int(Path(str(self.progress) + ".pid").read_text())
        with self.assertRaises(ProcessLookupError):
            os.kill(pid, 0)

    def test_answers_the_trust_dialog_and_ends_the_client_after_its_turn(self):
        run = self.run_client("turn")
        self.assertEqual(run["exit"], 0, run)
        self.assertLess(run["seconds"], 10)
        self.assertEqual(self.progress.read_text(), "agent\n")
        self.assert_gone()

    def test_a_client_that_never_calls_is_ended_at_the_timeout(self):
        run = self.run_client("idle", timeout=2)
        self.assertEqual(run["exit"], "timeout")
        self.assert_gone()

    def test_a_client_that_exits_reports_its_code(self):
        run = self.run_client("exit")
        self.assertEqual(run["exit"], 3)


class HandCommitTest(unittest.TestCase):
    """A run rebuilds the drift branch from main; it must not discard a commit
    a person pushed to that branch (the 2026-11-06 run did, on PR #138)."""

    def setUp(self):
        tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, tmp)
        remote = tmp / "remote.git"
        self.root = tmp / "tree"
        drift.git(tmp, "init", "--bare", "-b", drift.BASE_BRANCH, str(remote))
        drift.git(tmp, "init", "-b", drift.BASE_BRANCH, str(self.root))
        for key, value in (("user.name", "t"), ("user.email", "t@example.com"), ("commit.gpgsign", "false"),
                           ("core.hooksPath", "/dev/null")):
            drift.git(self.root, "config", key, value)
        drift.git(self.root, "remote", "add", drift.REMOTE, str(remote))
        self.commit("base")
        drift.git(self.root, "push", drift.REMOTE, f"HEAD:refs/heads/{drift.BASE_BRANCH}")
        drift.git(self.root, "fetch", drift.REMOTE)

    def commit(self, *message: str) -> None:
        drift.git(self.root, "commit", "--allow-empty", *(arg for line in message for arg in ("-m", line)))

    def push_branch(self) -> str:
        drift.git(self.root, "push", "--force", drift.REMOTE, f"HEAD:refs/heads/{drift.DRIFT_BRANCH}")
        return drift.git(self.root, "rev-parse", "HEAD")

    def test_no_drift_branch_yet(self):
        self.assertEqual(drift.hand_commits(self.root), ("", []))

    def test_a_branch_of_tool_commits_may_be_replaced(self):
        self.commit("chore(identity): follow agy 1.5.2", drift.COMMIT_MARKER + "; the PR body has the report.")
        tip = self.push_branch()
        self.assertEqual(drift.hand_commits(self.root), (tip, []))

    def test_a_hand_commit_is_reported(self):
        self.commit("chore(identity): follow agy 1.5.2", drift.COMMIT_MARKER + "; the PR body has the report.")
        self.commit("fix(identity): pass the gate")
        tip = self.push_branch()
        _, hand = drift.hand_commits(self.root)
        self.assertEqual(len(hand), 1, hand)
        self.assertIn("fix(identity): pass the gate", hand[0])
        self.assertEqual(drift.hand_commits(self.root)[0], tip)


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
