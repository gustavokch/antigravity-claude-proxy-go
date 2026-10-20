"""Unit tests for the mitmproxy header/response dump addon.

Run with: python3 -m unittest discover -s scripts -p 'test_*.py' -v

mitmproxy itself is NOT imported here: the addon guards its own import so
the record builder stays testable on a machine without mitmproxy.
"""

import gzip
import json
import os
import re
import sys
import tempfile
import types
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from mitm_header_dump import (
    MAX_IDENTITY_TEXT,
    MAX_RESPONSE_BODY,
    HeaderDump,
    body_fingerprint,
    build_record,
    generation_config_fingerprint,
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


class GenerationConfigFingerprintTest(unittest.TestCase):
    def _cloud_code_body(self, generation_config):
        return json.dumps({
            "project": "p",
            "model": "claude-opus-4-6-thinking",
            "requestType": "agent",
            "userAgent": "antigravity",
            "request": {
                "contents": [{"role": "user", "parts": [{"text": "SECRET_PROMPT_TEXT"}]}],
                "generationConfig": generation_config,
            },
        }).encode()

    def test_thinking_and_sampling_scalars_are_kept_verbatim(self):
        fingerprint = body_fingerprint(self._cloud_code_body({
            "temperature": 0.2,
            "maxOutputTokens": 64000,
            "thinkingConfig": {"include_thoughts": True, "thinking_budget": 1024},
        }))
        self.assertEqual(fingerprint["generation_config"], {
            "temperature": 0.2,
            "maxOutputTokens": 64000,
            "thinkingConfig": {"include_thoughts": True, "thinking_budget": 1024},
        })

    def test_key_casing_is_the_casing_on_the_wire(self):
        snake = generation_config_fingerprint(
            {"request": {"generationConfig": {"thinkingConfig": {"thinking_budget": 1}}}})
        camel = generation_config_fingerprint(
            {"request": {"generationConfig": {"thinkingConfig": {"thinkingBudget": 1}}}})
        self.assertEqual(snake, {"thinkingConfig": {"thinking_budget": 1}})
        self.assertEqual(camel, {"thinkingConfig": {"thinkingBudget": 1}})

    def test_enum_level_is_kept(self):
        got = generation_config_fingerprint(
            {"request": {"generationConfig": {"thinkingConfig": {"thinkingLevel": "HIGH", "includeThoughts": True}}}})
        self.assertEqual(got, {"thinkingConfig": {"thinkingLevel": "HIGH", "includeThoughts": True}})

    def test_prompt_bearing_fields_never_land_in_the_capture(self):
        fingerprint = body_fingerprint(self._cloud_code_body({
            "stopSequences": ["SECRET_STOP"],
            "responseSchema": {"description": "SECRET_SCHEMA"},
            "responseMimeType": "application/json",
            "maxOutputTokens": 1,
            "thinkingConfig": {"thinkingLevel": "x" * 500, "extra": "SECRET_EXTRA"},
        }))
        self.assertEqual(fingerprint["generation_config"], {"maxOutputTokens": 1})
        for secret in ("SECRET_PROMPT_TEXT", "SECRET_STOP", "SECRET_SCHEMA", "SECRET_EXTRA"):
            self.assertNotIn(secret, repr(fingerprint))

    def test_snake_case_knobs_are_kept_and_snake_case_prompt_fields_are_dropped(self):
        fingerprint = body_fingerprint(self._cloud_code_body({
            "max_output_tokens": 64000,
            "top_p": 0.9,
            "top_k": 5,
            "stop_sequences": ["SECRET_STOP"],
            "response_schema": {"description": "SECRET_SCHEMA"},
        }))
        self.assertEqual(fingerprint["generation_config"], {
            "max_output_tokens": 64000,
            "top_p": 0.9,
            "top_k": 5,
        })
        for secret in ("SECRET_STOP", "SECRET_SCHEMA"):
            self.assertNotIn(secret, repr(fingerprint))

    def test_wire_constants_of_the_envelope_are_identity_scalars(self):
        fingerprint = body_fingerprint(self._cloud_code_body({"maxOutputTokens": 1}))
        self.assertEqual(fingerprint["requestType"], "agent")
        self.assertEqual(fingerprint["userAgent"], "antigravity")
        self.assertEqual(fingerprint["model"], "claude-opus-4-6-thinking")

    def test_bodies_without_a_generation_config_add_nothing(self):
        self.assertNotIn("generation_config", body_fingerprint(b'{"model":"m"}'))
        self.assertNotIn("generation_config", body_fingerprint(b'{"request":{"contents":[]}}'))
        self.assertNotIn("generation_config", body_fingerprint(b'{"request":"text"}'))


ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
ANTHROPIC_HOST = "api.anthropic.com"


def shipped_hello(stack):
    """A ClientHello the proxy replays, which the real client sent."""
    with open(os.path.join(ROOT, "internal", "transport", f"claude-code-{stack}-clienthello.bin"), "rb") as handle:
        return handle.read()


def real_client_tls(stack):
    """The JA4, JA3 and ALPN the real client presented, recorded off the wire."""
    with open(os.path.join(ROOT, ".reference", "claude-code-tls-20261020.json"), encoding="utf-8") as handle:
        recorded = json.load(handle)["stacks"][stack]
    return {"ja4": recorded["ja4"], "ja3": recorded["ja3"], "alpn": recorded["alpn"]}


def real_client_body():
    """A /v1/messages body Claude Code 2.1.304 sent, byte for byte."""
    path = os.path.join(ROOT, "internal", "ccidentity", "testdata", "claude-code-2.1.304-bodies.jsonl.gz")
    with gzip.open(path, "rb") as handle:
        return next(line.rstrip(b"\n") for line in handle if line.strip())


def hello_event(connection_id, raw):
    """What mitmproxy hands tls_clienthello: a ClientHelloData."""
    return types.SimpleNamespace(
        client_hello=types.SimpleNamespace(raw_bytes=lambda: raw),
        context=types.SimpleNamespace(client=types.SimpleNamespace(id=connection_id)),
    )


def messages_flow(connection_id, content=b""):
    request = FakeRequest([("content-type", "application/json")], content)
    request.pretty_host = ANTHROPIC_HOST
    request.path = "/v1/messages?beta=true"
    request.http_version = "HTTP/1.1"
    f = FakeFlow(request, FakeResponse(200, []))
    f.client_conn = types.SimpleNamespace(id=connection_id)
    return f


class AddonTlsRecordTest(unittest.TestCase):
    """The gate's TLS verdict reads record["tls"], so the addon has to attach to
    each request the fingerprints of the hello that opened ITS connection."""

    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.out = os.path.join(self.dir.name, "out.jsonl")
        self.hello_dir = os.path.join(self.dir.name, "hellos")

    def addon(self, hello_dir=True):
        environ = {"MITM_DUMP_OUT": self.out, "MITM_DUMP_HOSTS": ANTHROPIC_HOST}
        if hello_dir:
            environ["MITM_DUMP_HELLO_DIR"] = self.hello_dir
        with mock.patch.dict(os.environ, environ):
            if not hello_dir:
                os.environ.pop("MITM_DUMP_HELLO_DIR", None)
            return HeaderDump()

    def records(self):
        with open(self.out, encoding="utf-8") as handle:
            return [json.loads(line) for line in handle]

    def test_a_record_carries_the_fingerprints_of_its_own_connections_hello(self):
        addon = self.addon()
        addon.tls_clienthello(hello_event("fetch-conn", shipped_hello("fetch")))
        addon.tls_clienthello(hello_event("node-conn", shipped_hello("node")))
        addon.response(messages_flow("node-conn"))
        addon.response(messages_flow("fetch-conn"))

        node, fetch = self.records()
        self.assertEqual(node["tls"], real_client_tls("node"))
        self.assertEqual(fetch["tls"], real_client_tls("fetch"))

    def test_a_connection_whose_hello_was_not_seen_has_no_fingerprint(self):
        # A record without one is a gate failure, not a pass: nothing is made up.
        addon = self.addon()
        addon.tls_clienthello(hello_event("seen", shipped_hello("fetch")))
        addon.response(messages_flow("unseen"))
        self.assertNotIn("tls", self.records()[0])

    def test_a_malformed_hello_is_ignored(self):
        addon = self.addon()
        addon.tls_clienthello(hello_event("broken", shipped_hello("fetch")[:60]))
        addon.response(messages_flow("broken"))
        self.assertNotIn("tls", self.records()[0])
        self.assertFalse(os.path.exists(self.hello_dir))

    def test_each_distinct_hello_is_kept_under_its_ja4(self):
        addon = self.addon()
        for stack in ("fetch", "node"):
            addon.tls_clienthello(hello_event(stack, shipped_hello(stack)))
        for stack in ("fetch", "node"):
            path = os.path.join(self.hello_dir, real_client_tls(stack)["ja4"] + ".bin")
            with open(path, "rb") as handle:
                self.assertEqual(handle.read(), shipped_hello(stack), stack)
        self.assertEqual(len(os.listdir(self.hello_dir)), 2)

    def test_the_first_hello_of_a_ja4_is_the_one_kept(self):
        addon = self.addon()
        os.makedirs(self.hello_dir)
        kept = os.path.join(self.hello_dir, real_client_tls("fetch")["ja4"] + ".bin")
        with open(kept, "wb") as handle:
            handle.write(b"earlier")
        addon.tls_clienthello(hello_event("again", shipped_hello("fetch")))
        with open(kept, "rb") as handle:
            self.assertEqual(handle.read(), b"earlier")

    def test_no_hello_directory_is_written_unless_asked_for(self):
        addon = self.addon(hello_dir=False)
        addon.tls_clienthello(hello_event("conn", shipped_hello("fetch")))
        addon.response(messages_flow("conn"))
        self.assertFalse(os.path.exists(self.hello_dir))
        self.assertEqual(self.records()[0]["tls"], real_client_tls("fetch"))


class BillingRecordTest(unittest.TestCase):
    """The gate's cch and cc_version verdicts read request_body.billing: the
    value the request SENT next to the one the real client's derivation gives."""

    def billing(self, content):
        record = build_record(messages_flow("c", content), capture_request_body=True)
        return record["request_body"]["billing"]

    def test_a_body_the_real_client_sent_agrees_with_itself(self):
        body = real_client_body()
        text = body.decode("utf-8")
        sent_cch = re.search(r"cch=([0-9a-f]{5});", text).group(1)
        sent_suffix = re.search(r"cc_version=\d+\.\d+\.\d+\.([0-9a-f]{3});", text).group(1)

        billing = self.billing(body)

        self.assertEqual(billing["cch"], sent_cch)
        self.assertEqual(billing["cch_expected"], sent_cch)
        self.assertEqual(billing["version_suffix"], sent_suffix)
        self.assertEqual(billing["version_suffix_expected"], sent_suffix)

    def test_a_body_changed_after_signing_no_longer_agrees(self):
        body = real_client_body()
        sent_cch = re.search(r"cch=([0-9a-f]{5});", body.decode("utf-8")).group(1)

        billing = self.billing(body.replace(b'"stream":true', b'"stream":false'))

        self.assertEqual(billing["cch"], sent_cch)
        self.assertNotEqual(billing["cch_expected"], sent_cch)
        self.assertEqual(billing["version_suffix"], billing["version_suffix_expected"])

    def test_a_first_prompt_that_is_not_the_one_hashed_disagrees_on_the_suffix(self):
        body = real_client_body()
        suffix = re.search(r"cc_version=\d+\.\d+\.\d+\.([0-9a-f]{3});", body.decode("utf-8")).group(1)
        # Break the version suffix itself, leaving the first prompt alone.
        other = "000" if suffix != "000" else "111"
        tampered = body.replace(f".{suffix};".encode(), f".{other};".encode(), 1)

        billing = self.billing(tampered)

        self.assertEqual(billing["version_suffix"], other)
        self.assertEqual(billing["version_suffix_expected"], suffix)

    def test_a_request_without_a_billing_header_has_no_verdict(self):
        record = build_record(
            messages_flow("c", b'{"model":"m","system":[{"type":"text","text":"plain"}]}'),
            capture_request_body=True,
        )
        self.assertNotIn("billing", record["request_body"])


if __name__ == "__main__":
    unittest.main()
