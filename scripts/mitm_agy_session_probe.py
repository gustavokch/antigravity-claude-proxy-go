"""mitmproxy addon: record ONLY session-shaped identifiers of agy Cloud Code requests.

Purpose: answer whether agy's request `sessionId` is per account, per process
or per conversation (feasibility report open question 6). Nothing but the
following leaves the request:

- the endpoint path (query string stripped),
- for every body field whose KEY matches session/conversation/trajectory:
  its JSON path, the length of its value, and the first 8 hex chars of the
  value's sha256,
- the same (name, length, sha8) for request headers whose NAME matches.

No prompt, no file content, no credential and no raw identifier is written.
The run label is read from the file named by $PROBE_LABEL_FILE on every
request, so the wizard can relabel between runs without restarting mitmdump.

Output: $PROBE_OUT (JSONL, appended).
"""

from __future__ import annotations

import hashlib
import json
import os
import re
from datetime import datetime, timezone

try:  # mitmproxy is absent when this module is imported for a dry run
    import mitmproxy.http  # noqa: F401
except ImportError:  # pragma: no cover
    mitmproxy = None

HOSTS = {"cloudcode-pa.googleapis.com", "daily-cloudcode-pa.googleapis.com"}
# `trajectory` is included on purpose: agy sends writeTrajectoryAcls, so a
# trajectory id may be the real per-conversation key rather than sessionId.
KEY_RE = re.compile(r"session|conversation|trajectory", re.IGNORECASE)
MAX_DEPTH = 8


def sha8(value) -> str:
    return hashlib.sha256(str(value).encode("utf-8")).hexdigest()[:8]


def walk(value, path: str, out: list, depth: int = 0) -> None:
    if depth >= MAX_DEPTH:
        return
    if isinstance(value, dict):
        for key, item in value.items():
            child = f"{path}.{key}" if path else key
            if KEY_RE.search(key) and not isinstance(item, (dict, list)):
                out.append({"path": child, "len": len(str(item)), "sha8": sha8(item)})
            walk(item, child, out, depth + 1)
    elif isinstance(value, list):
        for item in value:
            walk(item, f"{path}[]", out, depth + 1)


def build_record(flow, label: str) -> dict:
    req = flow.request
    record = {
        "ts": datetime.now(timezone.utc).isoformat(),
        "label": label,
        "host": req.pretty_host,
        "method": req.method,
        "endpoint": req.path.split("?", 1)[0],
        "request_bytes": len(req.content or b""),
        "body_hits": [],
        "header_hits": [],
    }
    for name, value in req.headers.items():
        if KEY_RE.search(name):
            record["header_hits"].append({"name": name, "len": len(value), "sha8": sha8(value)})
    try:
        body = json.loads((req.content or b"").decode("utf-8"))
    except (UnicodeDecodeError, ValueError):
        record["body_note"] = "empty or non-json"
        return record
    walk(body, "", record["body_hits"])
    return record


class SessionProbe:
    def __init__(self) -> None:
        self.out_path = os.environ.get("PROBE_OUT", "/tmp/agy-session-probe.jsonl")
        self.label_file = os.environ.get("PROBE_LABEL_FILE", "")

    def _label(self) -> str:
        try:
            with open(self.label_file, encoding="utf-8") as f:
                return f.read().strip()
        except OSError:
            return "unlabelled"

    def request(self, flow) -> None:
        if flow.request.pretty_host.lower() not in HOSTS:
            return
        record = build_record(flow, self._label())
        with open(self.out_path, "a", encoding="utf-8") as f:
            f.write(json.dumps(record) + "\n")
            f.flush()


addons = [SessionProbe()]
