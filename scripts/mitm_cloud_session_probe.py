"""mitmproxy addon: record the SHAPE of Claude Code cloud-session API traffic.

Purpose: feasibility report open question 1. Learn which routes, methods,
status codes and JSON key structures the CLI uses for `claude --cloud`, without
recording anything a caller typed or any credential.

Recorded per HTTP exchange (JSONL, appended to $PROBE_OUT):
- method, host, status, content-type;
- the path with identifiers masked ({uuid}, <prefix>_{id}, {hex}, {n}), and the
  segments after `git_proxy` masked entirely (they carry owner/repo names);
- query parameter NAMES only;
- request and response header NAMES; values only for a fixed benign allowlist;
- request and response JSON bodies as a key/type skeleton. A string value is
  kept only when its key is in ENUM_KEYS, it is short, it looks like a plain
  token, and it does not look like an identifier. Everything else is "string";
- for text/event-stream: event names and the skeleton of the first 5 data
  payloads;
- other bodies (uploads, binary): content-type and byte length only.

Also recorded: every proxy CONNECT (host:port) so hosts outside the intercept
set are discovered without decrypting them, and WebSocket upgrades (path only).

Never written: Authorization/cookie values, prompts, file contents, repo names,
branch names, raw ids, response text.
"""

from __future__ import annotations

import json
import os
import re
from datetime import datetime, timezone

try:  # mitmproxy is absent when this module is imported for a dry run
    import mitmproxy.http  # noqa: F401
except ImportError:  # pragma: no cover
    mitmproxy = None

_UUID = re.compile(r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")
_PREFIXED = re.compile(r"^([a-z]{2,12})_[0-9A-Za-z]{10,}$")
_HEX = re.compile(r"^[0-9a-fA-F]{16,}$")
_DIGITS = re.compile(r"^\d{6,}$")
_TOKEN = re.compile(r"^[A-Za-z0-9_.:/-]{1,40}$")

# String values are kept only under these keys (enum-like fields).
ENUM_KEYS = {
    "type", "kind", "status", "state", "role", "event_type", "subtype", "source",
    "permission_mode", "model", "outcome", "mode", "reason", "code", "error_type",
}
# Header values kept verbatim (no identity or credential content).
HEADER_VALUE_ALLOW = {
    "content-type", "accept", "anthropic-version", "anthropic-beta", "user-agent",
    "content-encoding", "transfer-encoding", "cache-control",
}
MAX_DEPTH = 8
MAX_JSON_BYTES = 2_000_000
MAX_SSE_EVENTS = 5


def mask_segment(seg: str) -> str:
    if _UUID.match(seg):
        return "{uuid}"
    m = _PREFIXED.match(seg)
    if m:
        return m.group(1) + "_{id}"
    if _HEX.match(seg):
        return "{hex}"
    if _DIGITS.match(seg):
        return "{n}"
    return seg


def mask_path(path: str) -> tuple[str, list[str]]:
    raw, _, query = path.partition("?")
    segs = raw.split("/")
    out: list[str] = []
    masked_rest = False
    for i, seg in enumerate(segs):
        if masked_rest:
            out.append("{x}")
            continue
        out.append(mask_segment(seg))
        if seg == "git_proxy":
            masked_rest = True
    names = sorted({p.split("=", 1)[0] for p in query.split("&") if p}) if query else []
    return "/".join(out), names


def looks_like_id(value: str) -> bool:
    return bool(_UUID.match(value) or _PREFIXED.match(value) or _HEX.match(value) or _DIGITS.match(value))


def shape(value, key: str = "", depth: int = 0):
    if depth >= MAX_DEPTH:
        return "<depth-limit>"
    if isinstance(value, dict):
        return {k: shape(v, k, depth + 1) for k, v in sorted(value.items())}
    if isinstance(value, list):
        if not value:
            return []
        return [shape(value[0], key, depth + 1), "<x%d>" % len(value)]
    if isinstance(value, bool):
        return "bool"
    if isinstance(value, (int, float)):
        return "number"
    if value is None:
        return "null"
    if key in ENUM_KEYS and isinstance(value, str) and _TOKEN.match(value) and not looks_like_id(value):
        return "string:" + value
    return "string"


def header_summary(headers) -> dict:
    names = []
    values = {}
    for name, value in headers.items():
        lname = name.lower()
        names.append(name)
        if lname in HEADER_VALUE_ALLOW:
            values[lname] = value
    return {"names": names, "values": values}


def body_summary(content: bytes, content_type: str):
    ctype = (content_type or "").lower()
    if not content:
        return None
    if "text/event-stream" in ctype:
        events, names = [], []
        text = content.decode("utf-8", "replace")
        for block in text.split("\n\n"):
            ev = None
            data_lines = []
            for line in block.splitlines():
                if line.startswith("event:"):
                    ev = line[6:].strip()
                elif line.startswith("data:"):
                    data_lines.append(line[5:].strip())
            if ev and ev not in names:
                names.append(ev)
            if data_lines and len(events) < MAX_SSE_EVENTS:
                try:
                    events.append(shape(json.loads("\n".join(data_lines))))
                except ValueError:
                    events.append("<non-json>")
        return {"sse_event_names": names, "sse_first_payload_shapes": events}
    if "json" in ctype and len(content) <= MAX_JSON_BYTES:
        try:
            return {"json_shape": shape(json.loads(content.decode("utf-8")))}
        except (UnicodeDecodeError, ValueError):
            return {"unparsable_json_bytes": len(content)}
    return {"content_type": ctype, "bytes": len(content)}


def build_record(flow, label: str) -> dict:
    req = flow.request
    path, query_names = mask_path(req.path)
    record = {
        "kind": "http",
        "ts": datetime.now(timezone.utc).isoformat(),
        "label": label,
        "method": req.method,
        "host": req.pretty_host,
        "path": path,
        "query_names": query_names,
        "http_version": req.http_version,
        "request_headers": header_summary(req.headers),
        "request_body": body_summary(req.content or b"", req.headers.get("content-type", "")),
    }
    res = getattr(flow, "response", None)
    if res is not None:
        ctype = res.headers.get("content-type", "")
        record["status"] = res.status_code
        record["response_headers"] = header_summary(res.headers)
        record["response_body"] = body_summary(res.content or b"", ctype)
    return record


class CloudSessionProbe:
    def __init__(self) -> None:
        self.out_path = os.environ.get("PROBE_OUT", "/tmp/cloud-session-probe.jsonl")
        self.label_file = os.environ.get("PROBE_LABEL_FILE", "")

    def _label(self) -> str:
        try:
            with open(self.label_file, encoding="utf-8") as f:
                return f.read().strip()
        except OSError:
            return "unlabelled"

    def _write(self, record: dict) -> None:
        with open(self.out_path, "a", encoding="utf-8") as f:
            f.write(json.dumps(record) + "\n")
            f.flush()

    def http_connect(self, flow) -> None:
        self._write({
            "kind": "connect", "ts": datetime.now(timezone.utc).isoformat(), "label": self._label(),
            "host": flow.request.host, "port": flow.request.port,
        })

    def response(self, flow) -> None:
        self._write(build_record(flow, self._label()))

    def websocket_start(self, flow) -> None:
        path, names = mask_path(flow.request.path)
        self._write({
            "kind": "websocket", "ts": datetime.now(timezone.utc).isoformat(), "label": self._label(),
            "host": flow.request.pretty_host, "path": path, "query_names": names,
        })

    def error(self, flow) -> None:
        req = flow.request
        path, _ = mask_path(req.path)
        self._write({
            "kind": "error", "ts": datetime.now(timezone.utc).isoformat(), "label": self._label(),
            "method": req.method, "host": req.pretty_host, "path": path,
            "error": str(getattr(flow.error, "msg", ""))[:120],
        })


addons = [CloudSessionProbe()]
