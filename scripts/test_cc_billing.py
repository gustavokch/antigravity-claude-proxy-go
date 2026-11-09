#!/usr/bin/env python3
"""cc_billing.py must reproduce what the real client sent.

The fixture holds three /v1/messages bodies Claude Code 2.1.304 sent, byte for
byte, so a derivation that drifts from the client's own fails here rather than
silently passing every proxy capture it is asked to judge.
"""

import gzip
import json
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import cc_billing  # noqa: E402

FIXTURE = os.path.join(
    os.path.dirname(os.path.abspath(__file__)),
    "..", "internal", "ccidentity", "testdata", "claude-code-2.1.304-bodies.jsonl.gz",
)


def captured_bodies():
    with gzip.open(FIXTURE, "rb") as handle:
        return [line.rstrip(b"\n") for line in handle if line.strip()]


class RealClientTest(unittest.TestCase):
    def test_reproduces_every_captured_body(self):
        bodies = captured_bodies()
        self.assertEqual(len(bodies), 3)
        for body in bodies:
            verdict = cc_billing.billing_verdict(body)
            self.assertIsNotNone(verdict)
            self.assertIsNotNone(verdict["cch"])
            self.assertEqual(verdict["cch"], verdict["cch_expected"])
            self.assertIsNotNone(verdict["version_suffix"])
            self.assertEqual(verdict["version_suffix"], verdict["version_suffix_expected"])

    def test_a_changed_body_no_longer_matches_its_cch(self):
        body = captured_bodies()[0].replace(b'"stream":true', b'"stream":false')
        verdict = cc_billing.billing_verdict(body)
        self.assertNotEqual(verdict["cch"], verdict["cch_expected"])

    def test_xxh64_reference_vectors(self):
        self.assertEqual(cc_billing.xxh64(b""), 0xEF46DB3751D8E999)
        self.assertEqual(cc_billing.xxh64(b"abc"), 0x44BC2CF5AD770999)


FIRST_PROMPT = "Read calc.py and tell me in one sentence what add does"


def billed_body(content, thread):
    """A 2.1.311-shaped /v1/messages body with one user message."""
    body = {
        "model": "claude-sonnet-5-5",
        "messages": [{"role": "user", "content": content}],
        "system": [{
            "type": "text",
            "text": "x-anthropic-billing-header: cc_version=2.1.311.c6a; "
                    "cc_entrypoint=cli; cch=00000;",
        }],
        "max_tokens": 128000,
        "thread": thread,
    }
    return json.dumps(body, separators=(",", ":")).encode()


class ThreadContinuationTest(unittest.TestCase):
    """2.1.311 main follow-ups name the previous reply in
    thread.previous_message_id and send only the messages after it
    (.reference/claude-code-count-tokens-20261107.jsonl lines 16, 18, 55, 56),
    so the first prompt the suffix hashes is not in the body."""

    def test_a_thread_continuation_leaves_the_suffix_unchecked(self):
        tool_result = [{"type": "tool_result", "tool_use_id": "toolu_01",
                        "content": "def add(a, b): return a + b"}]
        verdict = cc_billing.billing_verdict(
            billed_body(tool_result, {"type": "t", "previous_message_id": "msg_01"}))
        self.assertEqual(verdict["version_suffix"], "c6a")
        self.assertIsNone(verdict["version_suffix_expected"])
        self.assertEqual(verdict["version_suffix_unchecked"], cc_billing.THREAD_CONTINUATION)
        self.assertIsNotNone(verdict["cch_expected"])

    def test_a_thread_without_a_previous_message_is_checked(self):
        # A conversation's first request (line 15) carries thread with no
        # previous_message_id and the whole first prompt.
        prompt = [{"type": "text", "text": FIRST_PROMPT}]
        verdict = cc_billing.billing_verdict(billed_body(prompt, {"type": "t"}))
        self.assertEqual(verdict["version_suffix_expected"], "c6a")
        self.assertNotIn("version_suffix_unchecked", verdict)


if __name__ == "__main__":
    unittest.main()
