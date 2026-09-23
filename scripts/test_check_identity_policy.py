#!/usr/bin/env python3
"""Tests for the wire-identity policy checker.

These run without mitmproxy, so the policy logic is testable everywhere the
drift gate itself has to skip. Each case is written to fail if the checker
stops catching a policy violation, not if the baseline moves alongside it.

Run: python3 -m pytest scripts/test_check_identity_policy.py -q
"""

import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import check_identity_policy as policy  # noqa: E402

CAPTURED_UA = "claude-cli/2.1.280 (external, sdk-cli)"
DISCOVERY_UA = "claude-code/2.1.280"
CALLER_UA = "foreign-harness/1.0"
# sha256 of "gate-key", as the mitm addon records an x-api-key value.
GATE_KEY = "gate-key"
GATE_KEY_SHA = "2ecdda0fa468a95225e6309a105b7cc045e85013482364c03609a18eb7180c21"


def baseline_post(ua=CAPTURED_UA):
    return {
        "method": "POST",
        "path": "/v1/messages?beta=true",
        "headers": [
            ["User-Agent", ua],
            ["x-app", "cli"],
            ["x-claude-code-request-class", "main"],
            ["X-Stainless-OS", "Linux"],
        ],
    }


def baseline_get(ua=DISCOVERY_UA, path="/api/claude_code/settings"):
    return {"method": "GET", "path": path, "headers": [["User-Agent", ua]]}


def observed_normalized(**overrides):
    record = {
        "method": "POST",
        "path": "/v1/messages?beta=true",
        "headers": [
            ["user-agent", CAPTURED_UA],
            ["x-claude-code-request-class", "main"],
            ["x-stainless-os", "Linux"],
        ],
    }
    record.update(overrides)
    return record


def observed_passthrough(**overrides):
    record = {
        "method": "POST",
        "path": "/v1/messages",
        "headers": [
            ["user-agent", CALLER_UA],
            ["x-api-key", {"redacted": True, "scheme": "", "sha256": GATE_KEY_SHA, "len": 8}],
        ],
    }
    record.update(overrides)
    return record


def write_jsonl(records):
    handle = tempfile.NamedTemporaryFile("w", suffix=".jsonl", delete=False)
    for record in records:
        handle.write(json.dumps(record) + "\n")
    handle.close()
    return handle.name


class NormalizedTest(unittest.TestCase):
    def test_accepts_a_normalized_request(self):
        problems = policy.check_normalized(baseline_post(), observed_normalized())
        self.assertEqual(problems, [])

    def test_rejects_a_foreign_user_agent(self):
        observed = observed_normalized(
            headers=[["user-agent", CALLER_UA], ["x-claude-code-request-class", "main"]]
        )
        problems = policy.check_normalized(baseline_post(), observed)
        self.assertTrue(any("User-Agent" in p for p in problems), problems)

    def test_rejects_a_surviving_api_key(self):
        observed = observed_normalized(
            headers=[
                ["user-agent", CAPTURED_UA],
                ["x-claude-code-request-class", "main"],
                ["x-api-key", {"redacted": True, "sha256": GATE_KEY_SHA, "len": 8}],
            ]
        )
        problems = policy.check_normalized(baseline_post(), observed)
        self.assertTrue(any("x-api-key" in p for p in problems), problems)

    def test_rejects_a_missing_captured_query(self):
        observed = observed_normalized(path="/v1/messages")
        problems = policy.check_normalized(baseline_post(), observed)
        self.assertTrue(any("beta=true" in p for p in problems), problems)


class PassthroughTest(unittest.TestCase):
    def test_accepts_an_unnormalized_keyed_request(self):
        problems = policy.check_passthrough(observed_passthrough(), CALLER_UA, GATE_KEY_SHA)
        self.assertEqual(problems, [])

    def test_rejects_a_deleted_api_key(self):
        observed = observed_passthrough(headers=[["user-agent", CALLER_UA]])
        problems = policy.check_passthrough(observed, CALLER_UA, GATE_KEY_SHA)
        self.assertTrue(any("x-api-key" in p for p in problems), problems)

    def test_rejects_a_different_api_key(self):
        observed = observed_passthrough(
            headers=[
                ["user-agent", CALLER_UA],
                ["x-api-key", {"redacted": True, "sha256": "0" * 64, "len": 8}],
            ]
        )
        problems = policy.check_passthrough(observed, CALLER_UA, GATE_KEY_SHA)
        self.assertTrue(any("sha256" in p for p in problems), problems)

    def test_rejects_a_normalized_request(self):
        observed = observed_passthrough(
            headers=[
                ["user-agent", CAPTURED_UA],
                ["x-claude-code-request-class", "main"],
                ["x-api-key", {"redacted": True, "sha256": GATE_KEY_SHA, "len": 8}],
            ]
        )
        problems = policy.check_passthrough(observed, CALLER_UA, GATE_KEY_SHA)
        self.assertTrue(any("User-Agent" in p for p in problems), problems)
        self.assertTrue(any("x-claude-code-request-class" in p for p in problems), problems)

    def test_rejects_the_captured_query_being_added(self):
        observed = observed_passthrough(path="/v1/messages?beta=true")
        problems = policy.check_passthrough(observed, CALLER_UA, GATE_KEY_SHA)
        self.assertTrue(any("beta=true" in p for p in problems), problems)


class DiscoveryTest(unittest.TestCase):
    def test_accepts_the_captured_discovery_user_agent(self):
        observed = [
            {"method": "GET", "path": "/v1/models", "headers": [["user-agent", DISCOVERY_UA]]}
        ] * 3
        problems = policy.check_discovery([baseline_get()], observed, min_records=3)
        self.assertEqual(problems, [])

    def test_rejects_the_go_default(self):
        observed = [
            {
                "method": "GET",
                "path": "/v1/models",
                "headers": [["user-agent", "Go-http-client/1.1"]],
            }
        ]
        problems = policy.check_discovery([baseline_get()], observed, min_records=1)
        self.assertTrue(any("Go-http-client" in p for p in problems), problems)

    def test_checks_a_non_get_discovery_request_too(self):
        observed = [
            {
                "method": "POST",
                "path": "/v1/oauth/token",
                "headers": [["user-agent", "Go-http-client/1.1"]],
            }
        ]
        problems = policy.check_discovery([baseline_get()], observed, min_records=1)
        self.assertTrue(any("/v1/oauth/token" in p for p in problems), problems)

    def test_rejects_a_stale_version(self):
        observed = [
            {
                "method": "GET",
                "path": "/v1/models",
                "headers": [["user-agent", "Claude-Code/2.1.246"]],
            }
        ]
        problems = policy.check_discovery([baseline_get()], observed, min_records=1)
        self.assertTrue(any("2.1.246" in p for p in problems), problems)

    def test_rejects_too_few_records(self):
        observed = [
            {"method": "GET", "path": "/v1/models", "headers": [["user-agent", DISCOVERY_UA]]}
        ]
        problems = policy.check_discovery([baseline_get()], observed, min_records=3)
        self.assertTrue(any("3" in p for p in problems), problems)

    def test_rejects_a_baseline_without_a_get(self):
        problems = policy.check_discovery([], [], min_records=1)
        self.assertTrue(any("baseline" in p for p in problems), problems)


class RecordSelectionTest(unittest.TestCase):
    """A phase drives exactly one request, so it must assert on exactly one.

    The capture carries no phase marker — mitm_header_dump.build_record stores
    no model — so a record left over from an earlier phase is indistinguishable
    from this phase's own. Counting is the only defence.
    """

    def test_accepts_exactly_one(self):
        self.assertEqual(policy.check_record_count([observed_normalized()]), [])

    def test_rejects_more_than_one_messages_record(self):
        problems = policy.check_record_count([observed_normalized(), observed_normalized()])
        self.assertTrue(any("2" in p for p in problems), problems)

    def test_rejects_no_messages_record(self):
        problems = policy.check_record_count([])
        self.assertTrue(any("no POST /v1/messages" in p for p in problems), problems)


class LoaderTest(unittest.TestCase):
    def test_splits_records_by_method_and_path(self):
        path = write_jsonl(
            [
                observed_normalized(),
                {"method": "GET", "path": "/v1/models", "headers": []},
                {"method": "POST", "path": "/v1/oauth/token", "headers": []},
            ]
        )
        try:
            records = policy.load_records(path)
            self.assertEqual(len(policy.messages_records(records)), 1)
            self.assertEqual(len(policy.models_records(records)), 1)
            # Wider than models_records on purpose: the oauth POST is a
            # discovery-family request too.
            self.assertEqual(len(policy.discovery_records(records)), 2)
        finally:
            os.unlink(path)


if __name__ == "__main__":
    unittest.main()
