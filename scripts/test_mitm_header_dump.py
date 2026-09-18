"""Unit tests for the mitmproxy header/response dump addon.

Run with: python3 -m unittest discover -s scripts -p 'test_*.py' -v

mitmproxy itself is NOT imported here: the addon guards its own import so
the record builder stays testable on a machine without mitmproxy.
"""

import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from mitm_header_dump import MAX_RESPONSE_BODY, build_record


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


if __name__ == "__main__":
    unittest.main()
