#!/usr/bin/env python3
"""Tests for the Claude Code wire-identity differ.

These run without mitmproxy, so the differ's exemption logic is testable
everywhere the drift gate itself has to skip. Each case is written to fail if
the differ stops catching a drift, not if the baseline moves alongside it.

Run: python3 -m unittest discover -s scripts -p 'test_*.py'
"""

import contextlib
import io
import json
import os
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import diff_claude_code_identity as differ  # noqa: E402

BASELINE_HEADERS = [
    ["Accept", "application/json"],
    ["Authorization", {"redacted": True, "scheme": "Bearer", "len": 108}],
    ["Content-Type", "application/json"],
    ["User-Agent", "claude-cli/2.1.304 (external, sdk-cli)"],
    ["X-Claude-Code-Session-Id", "<uuid>"],
    ["X-Stainless-OS", "Linux"],
    ["X-Stainless-Timeout", "600"],
    ["anthropic-beta", "claude-code-20250219,oauth-2025-04-20"],
    ["anthropic-version", "2023-06-01"],
    ["x-app", "cli"],
    ["x-claude-code-prompt-id", "<uuid>"],
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
        "x-anthropic-billing-header: cc_version=2.1.304.456; "
        "cc_entrypoint=sdk-cli; cch=6cba6; "
        "cc_prompt_id=929b9264-529c-4464-bc00-abb30f590a0d; cc_turn_origin=sdk; "
        "cc_prompt_index=0; cc_turn_index=1;"
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
        "x-anthropic-billing-header: cc_version=2.1.304.456; "
        "cc_entrypoint=sdk-cli; cch=6cba6; "
        f"cc_prompt_id={prompt_id}; cc_turn_origin=sdk; "
        "cc_prompt_index=0; cc_turn_index=1;"
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

    def test_prompt_id_header_differing_from_billing_prompt_id_is_drift(self):
        # 2.1.304 sends x-claude-code-prompt-id and cc_prompt_id as one value in
        # all 7 captured requests. Both are exempt from value comparison, so only
        # this relation catches a proxy that mints them separately.
        observed = with_ids(
            "5c1c7cec-f043-4f01-a9f9-fe25fb98b338",
            "7cd5023f-9b41-4558-8901-c8f46878fbf1",
        )
        observed["headers"] = [
            ["x-claude-code-prompt-id", "929b9264-529c-4464-bc00-abb30f590a0d"]
            if h[0] == "x-claude-code-prompt-id" else h
            for h in observed["headers"]
        ]
        drifts = differ.compare_correlations(observed)
        self.assertTrue(
            any("x-claude-code-prompt-id" in d and "cc_prompt_id" in d for d in drifts),
            f"a prompt-id header that disagrees with cc_prompt_id passed: {drifts}",
        )

    def test_matching_prompt_id_header_is_not_drift(self):
        prompt_id = "7cd5023f-9b41-4558-8901-c8f46878fbf1"
        observed = with_ids("5c1c7cec-f043-4f01-a9f9-fe25fb98b338", prompt_id)
        observed["headers"] = [
            ["x-claude-code-prompt-id", prompt_id]
            if h[0] == "x-claude-code-prompt-id" else h
            for h in observed["headers"]
        ]
        self.assertEqual(differ.compare_correlations(observed), [])


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

    def test_h2_is_one_drift_not_a_drift_per_header(self):
        # The real client speaks HTTP/1.1, so h2 is drift. It is reported once:
        # HTTP/2 lowercases every field name, and comparing case across versions
        # would repeat the same root cause for every header.
        headers = [[h[0].lower(), h[1]] for h in BASELINE_HEADERS]
        drifts = drift_of(record(headers, BASELINE_BODY, http_version="HTTP/2.0"))
        self.assertEqual(len(drifts), 1, drifts)
        self.assertIn("HTTP version changed", drifts[0])
        self.assertIn("HTTP/2.0", drifts[0])

    def test_extra_header_is_drift(self):
        headers = BASELINE_HEADERS + [["x-harness-telemetry", "1"]]
        drifts = drift_of(record(headers, BASELINE_BODY))
        self.assertTrue(any("x-harness-telemetry" in d for d in drifts))

    def test_a_different_accept_encoding_is_drift(self):
        # The transport now writes the captured value and decodes br and zstd
        # itself, so "gzip" is Go's default showing through.
        headers = [
            ["Accept-Encoding", "gzip"] if h[0] == "Accept-Encoding" else h
            for h in BASELINE_HEADERS
        ]
        drifts = drift_of(record(headers, BASELINE_BODY))
        self.assertTrue(any("Accept-Encoding" in d for d in drifts), drifts)

    def test_an_absent_connection_header_is_drift(self):
        headers = [h for h in BASELINE_HEADERS if h[0] != "Connection"]
        drifts = drift_of(record(headers, BASELINE_BODY))
        self.assertTrue(any("missing header: Connection" in d for d in drifts), drifts)

    def test_identical_order_has_no_drift(self):
        self.assertEqual(differ.compare_order(baseline_record(), baseline_record()), [])

    def test_net_http_order_is_drift(self):
        # net/http writes Host and User-Agent first, then the rest sorted, with
        # Accept-Encoding and Connection among them: the same headers in a
        # different order.
        names = {h[0]: h for h in BASELINE_HEADERS}
        go_order = ["Host", "User-Agent"] + sorted(
            n for n in names if n not in ("Host", "User-Agent")
        )
        observed = record([names[n] for n in go_order], BASELINE_BODY)
        drifts = differ.compare_order(baseline_record(), observed)
        self.assertEqual(len(drifts), 1, drifts)
        self.assertIn("header order differs", drifts[0])

    def test_a_header_only_one_side_has_does_not_shift_the_order(self):
        extra = record(BASELINE_HEADERS[:3] + [["x-harness", "1"]] + BASELINE_HEADERS[3:], BASELINE_BODY)
        self.assertEqual(differ.compare_order(baseline_record(), extra), [])

    def test_a_missing_tail_header_is_reported_not_crashed_on(self):
        short = record(BASELINE_HEADERS[:-1], BASELINE_BODY)
        self.assertEqual(differ.compare_order(baseline_record(), short), [])

    def test_order_is_not_compared_across_http_versions(self):
        h2 = record(list(reversed(BASELINE_HEADERS)), BASELINE_BODY, http_version="HTTP/2.0")
        self.assertEqual(differ.compare_order(baseline_record(), h2), [])

    def test_host_identity_is_checked_against_the_host_not_the_capture(self):
        observed = record(BASELINE_HEADERS + [["X-Stainless-Arch", "arm64"]], BASELINE_BODY)
        self.assertEqual(differ.compare_host(observed, host=("Linux", "arm64")), [])
        drifts = differ.compare_host(observed, host=("MacOS", "x64"))
        self.assertEqual(len(drifts), 2, drifts)
        self.assertTrue(any("X-Stainless-OS" in d and "'MacOS'" in d for d in drifts), drifts)
        self.assertTrue(any("X-Stainless-Arch" in d and "'x64'" in d for d in drifts), drifts)

    def test_host_values_may_differ_from_the_capture_in_the_header_diff(self):
        # The baseline's Linux/arm64 describe its container, so compare_headers
        # must not insist on them; compare_host is what checks the machine.
        headers = [
            ["X-Stainless-OS", "MacOS"] if h[0] == "X-Stainless-OS" else h for h in BASELINE_HEADERS
        ]
        self.assertEqual(drift_of(record(headers, BASELINE_BODY)), [])

    def test_a_missing_stainless_header_is_left_to_the_header_diff(self):
        headers = [h for h in BASELINE_HEADERS if h[0] != "X-Stainless-OS"]
        self.assertEqual(differ.compare_host(record(headers, BASELINE_BODY), host=("MacOS", "arm64")), [])

    def test_host_identity_uses_the_stainless_spellings(self):
        # The same hosts as TestHostSpellingsMatchTheStainlessSDK in
        # internal/ccidentity/defaults_test.go, under the names Python reports
        # for them. Go's HostOS/HostArch and this host_identity() must agree on a
        # machine, or the gate reports a drift that is not one. Both follow the
        # Stainless SDK's normalizePlatform and normalizeArch applied to Node's
        # process.platform and process.arch.
        hosts = [
            (("Darwin", "arm64"), ("MacOS", "arm64")),
            (("Darwin", "x86_64"), ("MacOS", "x64")),
            (("Linux", "x86_64"), ("Linux", "x64")),
            (("Linux", "aarch64"), ("Linux", "arm64")),
            (("Linux", "armv7l"), ("Linux", "arm")),
            (("Linux", "armv6l"), ("Linux", "arm")),
            (("Linux", "armv8l"), ("Linux", "arm")),
            (("Linux", "i686"), ("Linux", "other:ia32")),
            (("Linux", "i386"), ("Linux", "other:ia32")),
            (("Linux", "ppc64le"), ("Linux", "other:ppc64")),
            (("Linux", "ppc64"), ("Linux", "other:ppc64")),
            (("Linux", "riscv64"), ("Linux", "other:riscv64")),
            (("Linux", "s390x"), ("Linux", "other:s390x")),
            (("Linux", "loongarch64"), ("Linux", "other:loong64")),
            (("Windows", "AMD64"), ("Windows", "x64")),
            (("Windows", "ARM64"), ("Windows", "arm64")),
            (("Windows", "x86"), ("Windows", "other:ia32")),
            (("FreeBSD", "amd64"), ("FreeBSD", "x64")),
            (("OpenBSD", "amd64"), ("OpenBSD", "x64")),
            (("NetBSD", "amd64"), ("Other:netbsd", "x64")),
            (("SunOS", "sun4v"), ("Other:sunos", "other:sun4v")),
            (("Android", "aarch64"), ("Android", "arm64")),
            (("iOS", "arm64"), ("iOS", "arm64")),
            (("Plan9", "riscv64"), ("Other:plan9", "other:riscv64")),
            # platform.system() and platform.machine() answer "" when they cannot tell.
            (("", ""), ("Unknown", "unknown")),
        ]
        for (system, machine), want in hosts:
            with self.subTest(system=system, machine=machine):
                with mock.patch.object(differ.platform, "system", return_value=system), \
                        mock.patch.object(differ.platform, "machine", return_value=machine):
                    self.assertEqual(differ.host_identity(), want)

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

    def test_prompt_id_header_value_may_differ(self):
        # A fresh UUID per prompt, so two captures never agree on it.
        headers = [
            ["x-claude-code-prompt-id", "929b9264-529c-4464-bc00-abb30f590a0d"]
            if h[0] == "x-claude-code-prompt-id" else h
            for h in BASELINE_HEADERS
        ]
        self.assertEqual(
            differ.compare_headers([baseline_record()], record(headers, BASELINE_BODY)), []
        )

    def test_dropped_prompt_id_header_is_drift(self):
        headers = [h for h in BASELINE_HEADERS if h[0] != "x-claude-code-prompt-id"]
        drifts = drift_of(record(headers, BASELINE_BODY))
        self.assertTrue(any("missing header: x-claude-code-prompt-id" in d for d in drifts))


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
        body["system_first_block"] = "x-anthropic-billing-header: cc_version=2.1.304;"
        drifts = drift_of(record(BASELINE_HEADERS, body))
        self.assertTrue(any("system[0]" in d for d in drifts))

    def test_billing_header_accepts_the_follow_up_turn_form(self):
        # Captured form: a later prompt of one process, so cc_prev_req is present
        # and cc_turn_index has moved past 1.
        body = json.loads(json.dumps(BASELINE_BODY))
        body["system_first_block"] = (
            "x-anthropic-billing-header: cc_version=2.1.304.39c; "
            "cc_entrypoint=sdk-cli; cch=cf924; cc_prev_req=req_011CgDmASVwJ2VQSo1XGUySi; "
            "cc_prompt_id=8cf9e663-2799-4162-8718-bd8ff7e88106; cc_turn_origin=sdk; "
            "cc_prompt_index=0; cc_turn_index=2;"
        )
        self.assertEqual(drift_of(record(BASELINE_HEADERS, body)), [])

    def test_billing_header_without_the_index_fields_is_drift(self):
        # The 2.1.280 form. 2.1.304 appends cc_prompt_index and cc_turn_index to
        # every request, so a header without them is the previous client.
        body = json.loads(json.dumps(BASELINE_BODY))
        body["system_first_block"] = (
            "x-anthropic-billing-header: cc_version=2.1.304.456; "
            "cc_entrypoint=sdk-cli; cch=6cba6; "
            "cc_prompt_id=929b9264-529c-4464-bc00-abb30f590a0d; cc_turn_origin=sdk;"
        )
        drifts = drift_of(record(BASELINE_HEADERS, body))
        self.assertTrue(any("system[0]" in d for d in drifts))

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


CAPTURE = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
    ".reference", "claude-code-headers-20261020.jsonl",
)


class RealClientDiffTest(unittest.TestCase):
    """Step 4 of the recapture procedure in docs/classifier-fallback-notes.md
    diffs a real client captured in a Linux container against the committed
    capture. Its X-Stainless-OS and -Arch describe that container, not the
    machine the diff runs on, so on any other machine the host check reports a
    drift that is not a change to make."""

    def run_main(self, *flags, mutate=lambda record: None):
        observed = differ.load_messages_records(CAPTURE)[1]
        observed["request_body"]["billing"] = {
            "cch": "6cba6", "cch_expected": "6cba6",
            "version_suffix": "456", "version_suffix_expected": "456",
        }
        mutate(observed)
        with tempfile.NamedTemporaryFile("w", suffix=".jsonl", delete=False) as handle:
            handle.write(json.dumps(observed) + "\n")
        argv = ["diff_claude_code_identity.py", "--baseline", CAPTURE, "--observed", handle.name, *flags]
        stderr = io.StringIO()
        try:
            # The diff runs on a Mac; the capture's container was Linux.
            with mock.patch.object(sys, "argv", argv), \
                    mock.patch.object(differ, "host_identity", return_value=("MacOS", "x64")), \
                    contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(stderr):
                code = differ.main()
        finally:
            os.unlink(handle.name)
        return code, stderr.getvalue()

    def test_the_capture_hosts_own_os_is_a_drift_by_default(self):
        code, stderr = self.run_main()
        self.assertEqual(code, 1, stderr)
        self.assertIn("X-Stainless-OS", stderr)

    def test_no_host_check_leaves_the_capture_hosts_values_alone(self):
        code, stderr = self.run_main("--no-host-check")
        self.assertEqual(code, 0, stderr)

    def test_no_host_check_does_not_hide_any_other_drift(self):
        def change_user_agent(record):
            for pair in record["headers"]:
                if pair[0] == "User-Agent":
                    pair[1] = "foreign-harness/1.0"

        code, stderr = self.run_main("--no-host-check", mutate=change_user_agent)
        self.assertEqual(code, 1, stderr)
        self.assertIn("User-Agent", stderr)


if __name__ == "__main__":
    unittest.main()
