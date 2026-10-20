#!/usr/bin/env python3
"""tls_hello.py must give the JA4 and JA3 the real client presents.

The two hellos are the ones the proxy replays (internal/transport). The
expected values are what Claude Code 2.1.304 presented when they were recorded
off the wire (.reference/claude-code-tls-20261020.json), so an implementation
that drifts from the JA4 or JA3 specification fails here instead of making the
gate judge against a wrong yardstick.
"""

import json
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import tls_hello  # noqa: E402

ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")


def capture(name):
    with open(os.path.join(ROOT, "internal", "transport", name), "rb") as handle:
        return handle.read()


def reference():
    with open(os.path.join(ROOT, ".reference", "claude-code-tls-20261020.json")) as handle:
        return json.load(handle)["stacks"]


class FingerprintTest(unittest.TestCase):
    def test_fetch_hello_matches_the_real_client(self):
        got = tls_hello.fingerprints(capture("claude-code-fetch-clienthello.bin"))
        want = reference()["fetch"]
        self.assertEqual((got["ja4"], got["ja3"], got["alpn"]), (want["ja4"], want["ja3"], want["alpn"]))

    def test_node_hello_matches_the_real_client(self):
        got = tls_hello.fingerprints(capture("claude-code-node-clienthello.bin"))
        want = reference()["node"]
        self.assertEqual((got["ja4"], got["ja3"], got["alpn"]), (want["ja4"], want["ja3"], want["alpn"]))

    def test_the_record_header_is_optional(self):
        record = capture("claude-code-fetch-clienthello.bin")
        self.assertEqual(tls_hello.fingerprints(record), tls_hello.fingerprints(record[5:]))

    def test_a_truncated_hello_is_a_value_error(self):
        with self.assertRaises(ValueError):
            tls_hello.fingerprints(capture("claude-code-fetch-clienthello.bin")[:60])

    def test_a_non_hello_is_a_value_error(self):
        with self.assertRaises(ValueError):
            tls_hello.fingerprints(b"\x16\x03\x01\x00\x05\x02\x00\x00\x01\x00")


if __name__ == "__main__":
    unittest.main()
