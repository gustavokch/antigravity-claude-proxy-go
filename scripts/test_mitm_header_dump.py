"""Unit tests for the mitmproxy header/response dump addon.

Run with: python3 -m unittest discover -s scripts -p 'test_*.py' -v

mitmproxy itself is NOT imported here: the addon guards its own import so
the record builder stays testable on a machine without mitmproxy.
"""

import json
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from mitm_header_dump import (
    MAX_IDENTITY_TEXT,
    MAX_RESPONSE_BODY,
    body_fingerprint,
    build_record,
    hosts_from_env,
    redact_identifier,
    request_body_enabled,
)


class FakeHeaders:
    def __init__(self, pairs):
        self._pairs = list(pairs)

    def items(self):
        return list(self._pairs)


class FakeRequest:
    def __init__(self, headers, content=b""):
        self.headers = FakeHeaders(headers)
        self.content = content
        self.pretty_host = "cloudcode-pa.googleapis.com"
        self.path = "/v1internal:streamGenerateContent?alt=sse"
        self.http_version = "HTTP/2.0"
        self.method = "POST"


class FakeResponse:
    def __init__(self, status_code, headers, content=b""):
        self.status_code = status_code
        self.headers = FakeHeaders(headers)
        self.content = content


class FakeFlow:
    def __init__(self, request, response):
        self.request = request
        self.response = response


def flow(status, response_headers=(), response_body=b"", request_headers=()):
    return FakeFlow(
        FakeRequest(list(request_headers) or [("content-type", "application/json")]),
        FakeResponse(status, list(response_headers), response_body),
    )


class BuildRecordTest(unittest.TestCase):
    def test_error_body_and_status_are_captured(self):
        body = b'{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}'
        record = build_record(flow(429, [("retry-after", "17")], body))
        self.assertEqual(record["status"], 429)
        self.assertEqual(record["response_body"], body.decode())
        self.assertIn(["retry-after", "17"], record["response_headers"])

    def test_success_body_is_never_captured(self):
        record = build_record(flow(200, [], b"data: user generated text\n\n"))
        self.assertEqual(record["status"], 200)
        self.assertNotIn("response_body", record)

    def test_response_body_is_truncated(self):
        record = build_record(flow(429, [], b"x" * (MAX_RESPONSE_BODY + 500)))
        self.assertEqual(len(record["response_body"]), MAX_RESPONSE_BODY)

    def test_request_authorization_is_redacted(self):
        record = build_record(
            flow(429, request_headers=[("authorization", "Bearer ya29.SECRET")])
        )
        name, value = record["headers"][0]
        self.assertEqual(name, "authorization")
        self.assertTrue(value["redacted"])
        self.assertEqual(value["scheme"], "Bearer")
        self.assertNotIn("SECRET", repr(record))

    def test_response_cookie_is_redacted(self):
        record = build_record(flow(429, [("set-cookie", "sid=abc123")]))
        self.assertIn(["set-cookie", "[redacted]"], record["response_headers"])
        self.assertNotIn("abc123", repr(record))

    def test_request_bytes_recorded(self):
        f = flow(429)
        f.request.content = b"12345"
        self.assertEqual(build_record(f)["request_bytes"], 5)


REAL_USER_ID = (
    "user_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
    "_account_11111111-2222-3333-4444-555555555555"
    "_session_66666666-7777-8888-9999-aaaaaaaaaaaa"
)


class HostsFromEnvTest(unittest.TestCase):
    def test_default_is_the_cloud_code_pair(self):
        self.assertEqual(
            hosts_from_env({}),
            {"cloudcode-pa.googleapis.com", "daily-cloudcode-pa.googleapis.com"},
        )

    def test_override_replaces_the_default(self):
        self.assertEqual(hosts_from_env({"MITM_DUMP_HOSTS": "api.anthropic.com"}), {"api.anthropic.com"})

    def test_comma_list_is_split_and_normalized(self):
        got = hosts_from_env({"MITM_DUMP_HOSTS": " API.Anthropic.com , api.packyapi.com "})
        self.assertEqual(got, {"api.anthropic.com", "api.packyapi.com"})

    def test_blank_override_falls_back_to_the_default(self):
        self.assertEqual(hosts_from_env({"MITM_DUMP_HOSTS": "   "}), hosts_from_env({}))
        self.assertEqual(hosts_from_env({"MITM_DUMP_HOSTS": " , "}), hosts_from_env({}))


class RequestBodyEnabledTest(unittest.TestCase):
    def test_off_by_default(self):
        self.assertFalse(request_body_enabled({}))

    def test_only_literal_one_enables(self):
        self.assertTrue(request_body_enabled({"MITM_DUMP_REQUEST_BODY": "1"}))
        for value in ("0", "true", "yes", ""):
            self.assertFalse(request_body_enabled({"MITM_DUMP_REQUEST_BODY": value}), value)


class RedactIdentifierTest(unittest.TestCase):
    def test_shape_survives_and_identifiers_do_not(self):
        got = redact_identifier(REAL_USER_ID)
        self.assertEqual(got, "user_<hex>_account_<uuid>_session_<uuid>")
        self.assertNotIn("11111111", got)
        self.assertNotIn("0123456789abcdef", got)

    def test_words_are_not_mistaken_for_identifiers(self):
        self.assertEqual(redact_identifier("user_account_session"), "user_account_session")

    def test_non_string_passes_through(self):
        self.assertEqual(redact_identifier(7), 7)


class BodyFingerprintTest(unittest.TestCase):
    def _body(self, **overrides):
        body = {
            "model": "claude-sonnet-5",
            "max_tokens": 32000,
            "stream": True,
            "metadata": {"user_id": REAL_USER_ID},
            "system": [
                {"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.280; cc_entrypoint=cli;"},
                {"type": "text", "text": "You are Claude Code."},
            ],
            "messages": [{"role": "user", "content": [{"type": "text", "text": "SECRET_PROMPT_TEXT"}]}],
            "tools": [{"name": "Read", "input_schema": {"type": "object"}}],
        }
        body.update(overrides)
        return json.dumps(body).encode()

    def test_caller_text_never_lands_in_the_fingerprint(self):
        fingerprint = body_fingerprint(self._body())
        self.assertNotIn("SECRET_PROMPT_TEXT", repr(fingerprint))

    def test_identity_marker_is_kept_verbatim(self):
        fingerprint = body_fingerprint(self._body())
        self.assertTrue(fingerprint["system_first_block"].startswith("x-anthropic-billing-header:"))

    def test_second_system_block_is_not_recorded(self):
        fingerprint = body_fingerprint(self._body())
        self.assertNotIn("You are Claude Code.", repr(fingerprint))

    def test_identity_marker_is_truncated(self):
        long_marker = "x-anthropic-billing-header: " + "a" * (MAX_IDENTITY_TEXT * 2)
        fingerprint = body_fingerprint(self._body(system=[{"type": "text", "text": long_marker}]))
        self.assertEqual(len(fingerprint["system_first_block"]), MAX_IDENTITY_TEXT)

    def test_metadata_ids_are_shape_only(self):
        fingerprint = body_fingerprint(self._body())
        self.assertEqual(fingerprint["metadata"]["user_id"], "user_<hex>_account_<uuid>_session_<uuid>")

    def test_top_level_keys_and_identity_scalars(self):
        fingerprint = body_fingerprint(self._body())
        self.assertEqual(
            fingerprint["top_level_keys"],
            ["max_tokens", "messages", "metadata", "model", "stream", "system", "tools"],
        )
        self.assertEqual(fingerprint["model"], "claude-sonnet-5")
        self.assertEqual(fingerprint["max_tokens"], 32000)
        self.assertTrue(fingerprint["stream"])

    def test_shape_carries_types_not_values(self):
        fingerprint = body_fingerprint(self._body())
        self.assertEqual(fingerprint["shape"]["max_tokens"], "number")
        self.assertEqual(fingerprint["shape"]["model"], "string")
        self.assertEqual(fingerprint["shape"]["tools"], [{"input_schema": {"type": "string"}, "name": "string"}, "<x1>"])

    def test_string_system_is_accepted(self):
        fingerprint = body_fingerprint(self._body(system="x-anthropic-billing-header: cc_version=2.1.280;"))
        self.assertTrue(fingerprint["system_first_block"].startswith("x-anthropic-billing-header:"))

    def test_unparsable_body_reports_size_only(self):
        self.assertEqual(body_fingerprint(b"not json"), {"unparsable": True, "bytes": 8})
        self.assertIsNone(body_fingerprint(b""))

    def test_non_object_body_is_unparsable(self):
        self.assertEqual(body_fingerprint(b"[1,2,3]"), {"unparsable": True, "bytes": 7})


class BuildRecordRequestBodyTest(unittest.TestCase):
    def test_body_fingerprint_is_absent_by_default(self):
        f = flow(200)
        f.request.content = b'{"model":"claude-sonnet-5"}'
        self.assertNotIn("request_body", build_record(f))

    def test_body_fingerprint_is_present_when_asked(self):
        f = flow(200)
        f.request.content = b'{"model":"claude-sonnet-5"}'
        record = build_record(f, capture_request_body=True)
        self.assertEqual(record["request_body"]["model"], "claude-sonnet-5")

    def test_empty_body_adds_no_fingerprint(self):
        record = build_record(flow(200), capture_request_body=True)
        self.assertNotIn("request_body", record)

    def test_headers_are_unaffected_by_body_capture(self):
        f = flow(429, request_headers=[("authorization", "Bearer ya29.SECRET")])
        f.request.content = b"{}"
        record = build_record(f, capture_request_body=True)
        self.assertEqual(record["headers"][0][0], "authorization")
        self.assertNotIn("SECRET", repr(record))


if __name__ == "__main__":
    unittest.main()
