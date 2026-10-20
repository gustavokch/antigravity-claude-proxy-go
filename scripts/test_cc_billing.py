#!/usr/bin/env python3
"""cc_billing.py must reproduce what the real client sent.

The fixture holds three /v1/messages bodies Claude Code 2.1.304 sent, byte for
byte, so a derivation that drifts from the client's own fails here rather than
silently passing every proxy capture it is asked to judge.
"""

import gzip
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


if __name__ == "__main__":
    unittest.main()
