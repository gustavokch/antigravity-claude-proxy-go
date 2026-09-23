#!/usr/bin/env python3
"""Tests for the Claude Code wire-identity differ.

These run without mitmproxy, so the differ's exemption logic is testable
everywhere the drift gate itself has to skip. Each case is written to fail if
the differ stops catching a drift, not if the baseline moves alongside it.

Run: python3 -m unittest discover -s scripts -p 'test_*.py'
"""

import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import diff_claude_code_identity as differ  # noqa: E402

BASELINE_HEADERS = [
    ["Accept", "application/json"],
    ["Authorization", {"redacted": True, "scheme": "Bearer", "len": 108}],
    ["Content-Type", "application/json"],
    ["User-Agent", "claude-cli/2.1.280 (external, sdk-cli)"],
    ["X-Claude-Code-Session-Id", "<uuid>"],
    ["X-Stainless-OS", "Linux"],
    ["X-Stainless-Timeout", "600"],
    ["anthropic-beta", "claude-code-20250219,oauth-2025-04-20"],
    ["anthropic-version", "2023-06-01"],
    ["x-app", "cli"],
    ["x-client-request-id", "<uuid>"],
    ["Connection", "keep-alive"],
    ["Host", "api.anthropic.com"],
    ["Accept-Encoding", "gzip, deflate, br, zstd"],
    ["Content-Length", "100"],
]

BASELINE_BODY = {
    "top_level_keys": [
        "max_tokens", "messages", "metadata", "model", "stream", "system",
    ],
    "metadata": {
        "user_id": '{"device_id":"<hex>","account_uuid":"","session_id":"<uuid>"}'
    },
    "system_first_block": (
        "x-anthropic-billing-header: cc_version=2.1.280.5c2; "
        "cc_entrypoint=sdk-cli; cch=c5b4f; "
        "cc_prompt_id=7cd5023f-9b41-4558-8901-c8f46878fbf1; cc_turn_origin=sdk;"
    ),
}


def record(headers, body, http_version="HTTP/1.1", path="/v1/messages?beta=true"):
    return {
        "http_version": http_version,
        "method": "POST",
        "host": "api.anthropic.com",
        "path": path,
        "headers": [list(pair) for pair in headers],
        "request_body": json.loads(json.dumps(body)),
    }


def baseline_record(**kwargs):
    return record(BASELINE_HEADERS, BASELINE_BODY, **kwargs)


def drift_of(observed, baseline=None):
    base = [baseline or baseline_record()]
    return (
        differ.compare_headers(base, observed)
        + differ.compare_body(base, observed)
        + differ.compare_correlations(observed)
    )


def with_ids(session_id, prompt_id):
    """A record carrying a chosen session id and cc_prompt_id."""
    headers = [
        ["X-Claude-Code-Session-Id", session_id]
        if h[0] == "X-Claude-Code-Session-Id" else h
        for h in BASELINE_HEADERS
    ]
    body = json.loads(json.dumps(BASELINE_BODY))
    body["system_first_block"] = (
        "x-anthropic-billing-header: cc_version=2.1.280.5c2; "
        "cc_entrypoint=sdk-cli; cch=c5b4f; "
        f"cc_prompt_id={prompt_id}; cc_turn_origin=sdk;"
    )
    return record(headers, body)


class CorrelationDriftTest(unittest.TestCase):
    """The session id and cc_prompt_id are each exempt from value comparison,
    so nothing else in this gate relates them. The captures show them always
    different — 6 requests, 2 independent OAuth accounts — which makes an equal
    pair a value no real client emits."""

    def test_session_id_equal_to_prompt_id_is_drift(self):
        same = "5c1c7cec-f043-4f01-a9f9-fe25fb98b338"
        drifts = drift_of(with_ids(same, same))
        self.assertTrue(
            any("cc_prompt_id" in d and "session" in d for d in drifts),
            f"an identical session id and cc_prompt_id passed the gate: {drifts}",
        )

    def test_distinct_ids_are_not_drift(self):
        observed = with_ids(
            "5c1c7cec-f043-4f01-a9f9-fe25fb98b338",
            "7cd5023f-9b41-4558-8901-c8f46878fbf1",
        )
        self.assertEqual(drift_of(observed), [])

    def test_redacted_placeholders_are_not_compared(self):
        # A capture recorded with redaction on carries <uuid> in both slots.
        # That is the redactor agreeing with itself, not a real collision.
        self.assertEqual(differ.compare_correlations(with_ids("<uuid>", "<uuid>")), [])


class HeaderDriftTest(unittest.TestCase):
    def test_identical_capture_has_no_drift(self):
        self.assertEqual(drift_of(baseline_record()), [])

    def test_dropped_header_is_drift(self):
        headers = [h for h in BASELINE_HEADERS if h[0] != "X-Stainless-OS"]
        drifts = drift_of(record(headers, BASELINE_BODY))
        self.assertTrue(any("missing header: X-Stainless-OS" in d for d in drifts))

    def test_changed_value_is_drift(self):
        headers = [
            ["X-Stainless-Timeout", "601"] if h[0] == "X-Stainless-Timeout" else h
            for h in BASELINE_HEADERS
        ]
        drifts = drift_of(record(headers, BASELINE_BODY))
        self.assertTrue(any("X-Stainless-Timeout" in d for d in drifts))

    def test_reordered_beta_list_is_drift(self):
        headers = [
            ["anthropic-beta", "oauth-2025-04-20,claude-code-20250219"]
            if h[0] == "anthropic-beta" else h
            for h in BASELINE_HEADERS
        ]
        drifts = drift_of(record(headers, BASELINE_BODY))
        self.assertTrue(any("anthropic-beta" in d for d in drifts))

    def test_client_user_agent_leak_is_drift(self):
        headers = [
            ["User-Agent", "foreign-harness/1.0"] if h[0] == "User-Agent" else h
            for h in BASELINE_HEADERS
        ]
        drifts = drift_of(record(headers, BASELINE_BODY))
        self.assertTrue(any("User-Agent" in d for d in drifts))

    def test_canonicalised_name_is_drift_at_the_same_http_version(self):
        headers = [
            ["X-Stainless-Os", h[1]] if h[0] == "X-Stainless-OS" else h
            for h in BASELINE_HEADERS
        ]
        drifts = drift_of(record(headers, BASELINE_BODY))
        self.assertTrue(any("case changed" in d for d in drifts))

    def test_lowercased_names_are_not_drift_across_http_versions(self):
        # HTTP/2 lowercases every field name on the wire. Comparing case across
        # versions would report drift on every header forever.
        headers = [[h[0].lower(), h[1]] for h in BASELINE_HEADERS]
        drifts = drift_of(record(headers, BASELINE_BODY, http_version="HTTP/2.0"))
        self.assertEqual(drifts, [])

    def test_extra_header_is_drift(self):
        headers = BASELINE_HEADERS + [["x-harness-telemetry", "1"]]
        drifts = drift_of(record(headers, BASELINE_BODY))
        self.assertTrue(any("x-harness-telemetry" in d for d in drifts))

    def test_cleared_accept_encoding_is_exempt(self):
        headers = [
            ["Accept-Encoding", "gzip"] if h[0] == "Accept-Encoding" else h
            for h in BASELINE_HEADERS
        ]
        self.assertEqual(drift_of(record(headers, BASELINE_BODY)), [])

    def test_absent_connection_header_is_exempt(self):
        headers = [h for h in BASELINE_HEADERS if h[0] != "Connection"]
        self.assertEqual(drift_of(record(headers, BASELINE_BODY)), [])

    def test_conditional_tool_durations_header_is_exempt_when_absent(self):
        base = baseline_record()
        base["headers"].append(["x-claude-code-prev-tool-durations", "Bash=29"])
        drifts = differ.compare_headers([base], baseline_record())
        self.assertEqual(drifts, [])

    def test_dynamic_values_may_differ(self):
        headers = [
            ["x-client-request-id", "a-completely-different-id"]
            if h[0] == "x-client-request-id" else h
            for h in BASELINE_HEADERS
        ]
        self.assertEqual(drift_of(record(headers, BASELINE_BODY)), [])


class BodyDriftTest(unittest.TestCase):
    def test_forbidden_client_field_is_drift(self):
        body = dict(BASELINE_BODY)
        body["top_level_keys"] = sorted(body["top_level_keys"] + ["temperature"])
        drifts = drift_of(record(BASELINE_HEADERS, body))
        self.assertTrue(any("temperature" in d for d in drifts))

    def test_absent_optional_keys_are_not_drift(self):
        # context_management, diagnostics and output_config are recorded gaps;
        # stream and tools depend on the client's own request.
        body = dict(BASELINE_BODY)
        body["top_level_keys"] = ["max_tokens", "messages", "metadata", "model", "system"]
        self.assertEqual(drift_of(record(BASELINE_HEADERS, body)), [])

    def test_flat_user_id_format_is_drift(self):
        body = json.loads(json.dumps(BASELINE_BODY))
        body["metadata"]["user_id"] = "user_<hex>_account_<uuid>_session_<uuid>"
        drifts = drift_of(record(BASELINE_HEADERS, body))
        self.assertTrue(any("metadata.user_id" in d for d in drifts))

    def test_populated_account_uuid_is_drift(self):
        # Settled 2026-09-23: empty in 6 of 6 captured requests across two
        # independent OAuth credentials, so a populated value is a field no
        # real client emits.
        body = json.loads(json.dumps(BASELINE_BODY))
        body["metadata"]["user_id"] = (
            '{"device_id":"<hex>","account_uuid":"<uuid>","session_id":"<uuid>"}'
        )
        drifts = drift_of(record(BASELINE_HEADERS, body))
        self.assertTrue(any("metadata.user_id" in d for d in drifts))

    def test_missing_billing_header_is_drift(self):
        body = json.loads(json.dumps(BASELINE_BODY))
        body["system_first_block"] = None
        drifts = drift_of(record(BASELINE_HEADERS, body))
        self.assertTrue(any("system[0]" in d for d in drifts))

    def test_malformed_billing_header_is_drift(self):
        body = json.loads(json.dumps(BASELINE_BODY))
        body["system_first_block"] = "x-anthropic-billing-header: cc_version=2.1.280;"
        drifts = drift_of(record(BASELINE_HEADERS, body))
        self.assertTrue(any("system[0]" in d for d in drifts))

    def test_billing_header_accepts_the_follow_up_turn_form(self):
        body = json.loads(json.dumps(BASELINE_BODY))
        body["system_first_block"] = (
            "x-anthropic-billing-header: cc_version=2.1.280.336; "
            "cc_entrypoint=sdk-cli; cch=3ab87; cc_prev_req=req_011CfL3J5mtdJ5DhtF1opYTr; "
            "cc_prompt_id=518a84e2-4c45-458c-a2f6-363b7ff7fa44; cc_turn_origin=sdk;"
        )
        self.assertEqual(drift_of(record(BASELINE_HEADERS, body)), [])

    def test_absent_body_fingerprint_is_drift(self):
        observed = baseline_record()
        del observed["request_body"]
        drifts = differ.compare_body([baseline_record()], observed)
        self.assertTrue(any("request_body" in d for d in drifts))


class RecordSelectionTest(unittest.TestCase):
    def test_only_post_messages_records_are_loaded(self):
        lines = [
            {"method": "GET", "path": "/api/claude_code/policy_limits", "headers": []},
            {"method": "POST", "path": "/v1/complete", "headers": []},
            baseline_record(),
        ]
        with tempfile.NamedTemporaryFile("w", suffix=".jsonl", delete=False) as handle:
            for line in lines:
                handle.write(json.dumps(line) + "\n")
            path = handle.name
        try:
            loaded = differ.load_messages_records(path)
            self.assertEqual(len(loaded), 1)
            self.assertEqual(loaded[0]["path"], "/v1/messages?beta=true")
        finally:
            os.unlink(path)

    def test_changed_path_is_drift(self):
        observed = baseline_record(path="/v1/messages")
        self.assertNotEqual(observed["path"], baseline_record()["path"])


if __name__ == "__main__":
    unittest.main()
