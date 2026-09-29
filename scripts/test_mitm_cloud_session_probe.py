"""Unit tests for the cloud-session probe addon's redaction.

Run with: python3 -m unittest discover -s scripts -p 'test_*.py' -v

mitmproxy itself is NOT imported here: the addon guards its own import so the
shape and path helpers stay testable on a machine without mitmproxy.
"""

import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from mitm_cloud_session_probe import mask_path, shape


class ShapeRedactionTests(unittest.TestCase):
    def test_free_text_keys_never_keep_their_value(self):
        for key in ("source", "reason", "code"):
            self.assertEqual(shape({key: "acme/private-repo"})[key], "string", key)

    def test_repo_slugs_and_host_ports_are_not_kept_under_enum_keys(self):
        for value in ("octo-org/secret.git", "acme/private-repo", "host:8080"):
            self.assertEqual(shape({"type": value})["type"], "string", value)

    def test_enum_values_are_still_kept(self):
        self.assertEqual(
            shape({"type": "user", "model": "claude-opus-5-5"}),
            {"model": "string:claude-opus-5-5", "type": "string:user"},
        )

    def test_ids_are_not_kept_even_under_enum_keys(self):
        self.assertEqual(shape({"type": "session_01ABCDEFGHJK"})["type"], "string")

    def test_ids_in_paths_are_masked(self):
        path, _ = mask_path("/v1/code/sessions/session_01ABCDEFGHJK/events")
        self.assertEqual(path, "/v1/code/sessions/session_{id}/events")


if __name__ == "__main__":
    unittest.main()
