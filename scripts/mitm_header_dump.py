"""mitmproxy addon: dump Cloud Code requests AND responses as ordered JSONL.

Records every intercepted exchange with a filtered host set as one JSON line:
ts, http_version, method, host, path, request_bytes, headers, status,
response_headers and — for error statuses only — response_body.

Host set defaults to the two Cloud Code hosts. $MITM_DUMP_HOSTS overrides it
with a comma-separated list, which is what lets the same addon capture
api.anthropic.com for the Claude Code baseline.

The Authorization value is a live OAuth token, so it is redacted HERE,
before anything is written: scheme (first whitespace token) plus a sha256
and length of the credential token only. Matching is a substring,
case-insensitive match on the header name so variants like
x-goog-iam-authorization-list are over-redacted on purpose.

Response bodies are kept for error statuses ONLY. A 200 on
streamGenerateContent is the user's own generated text and must never land
in a capture file; the 4xx body is what carries the throttle dimension this
capture exists to find.

REQUEST bodies are NOT dumped. Even with $MITM_DUMP_REQUEST_BODY=1 the addon
records a body *fingerprint* instead of the body: the key tree with value
types, the redacted metadata map, and a short prefix of the first system block
(the client identity marker). Everything the caller typed — prompts, file
contents, tool output — stays out of the capture file by construction, not by
review.

Output path comes from $MITM_DUMP_OUT (default /tmp/agy-headers-mitm.jsonl),
appended, flushed per line.
"""

from __future__ import annotations

import hashlib
import json
import os
import re
from datetime import datetime, timezone

try:  # mitmproxy is absent when the unit tests import this module
    import mitmproxy.http
except ImportError:  # pragma: no cover - only outside mitmdump
    mitmproxy = None

CLOUDCODE_HOSTS = {
    "cloudcode-pa.googleapis.com",
    "daily-cloudcode-pa.googleapis.com",
}

DEFAULT_HOSTS = CLOUDCODE_HOSTS

# Substring match on purpose: over-redact anything auth-ish. Cookie and
# api-key names are included so a future agy that grows a session cookie or
# key header never lands a live credential in the JSONL.
SENSITIVE_NAME = re.compile(
    r"authorization|proxy-authorization|x-goog-iam-authorization|cookie|api[_-]?key",
    re.IGNORECASE,
)

# Header values carrying credentials as "<scheme> <token>" get scheme kept in
# the clear and the token hashed. Values in any other shape (Cookie crumbs,
# raw keys) are hashed whole — nothing of the value stays readable.
AUTH_SCHEMES = {"bearer", "basic", "digest", "token", "negotiate", "oauth"}

# A Google error payload is well under 1 KB. The cap only stops an HTML
# error page from bloating the capture.
MAX_RESPONSE_BODY = 8192

# The client identity marker is one short line; 400 characters covers it with
# room to spare without dragging the rest of the system block into the file.
MAX_IDENTITY_TEXT = 400

# How deep the body key tree is walked. A messages payload nests well under
# this; the cap only stops a pathological body from recursing forever.
MAX_SHAPE_DEPTH = 6

_UUID_RE = re.compile(
    r"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-"
    r"[0-9a-fA-F]{4}-[0-9a-fA-F]{12}"
)
# 16+ hex characters is longer than any English word that is also pure hex
# ("decade", "accede"), so the separators in metadata.user_id survive.
_HEX_RUN_RE = re.compile(r"[0-9a-fA-F]{16,}")

# Scalar fields whose exact value IS the fingerprint the capture exists to
# record: the wire contract, not the caller's content.
IDENTITY_SCALARS = ("model", "max_tokens", "stream")


def _redact(value: str) -> dict:
    parts = value.split(None, 1)
    scheme = parts[0] if parts and parts[0].lower() in AUTH_SCHEMES else ""
    token = parts[1] if scheme and len(parts) > 1 else value
    return {
        "redacted": True,
        "scheme": scheme,
        "sha256": hashlib.sha256(token.encode("utf-8")).hexdigest(),
        "len": len(token),
    }


def hosts_from_env(environ=None) -> set[str]:
    """Host set to filter on. $MITM_DUMP_HOSTS wins over the Cloud Code default."""
    env = os.environ if environ is None else environ
    raw = (env.get("MITM_DUMP_HOSTS") or "").strip()
    if not raw:
        return set(DEFAULT_HOSTS)
    hosts = {part.strip().lower() for part in raw.split(",") if part.strip()}
    return hosts or set(DEFAULT_HOSTS)


def request_body_enabled(environ=None) -> bool:
    """Whether to record the body fingerprint. Off unless explicitly asked."""
    env = os.environ if environ is None else environ
    return (env.get("MITM_DUMP_REQUEST_BODY") or "").strip() == "1"


def redact_identifier(value: str) -> str:
    """Reduce an identifier-shaped string to its shape.

    metadata.user_id carries the account and session UUIDs. The capture needs
    the literal prefix and separators to reproduce the format; the identifiers
    themselves are the operator's and are replaced.
    """
    if not isinstance(value, str):
        return value
    return _HEX_RUN_RE.sub("<hex>", _UUID_RE.sub("<uuid>", value))


def _shape(value, depth: int = 0):
    """Recursive key/type skeleton with every scalar replaced by its type name."""
    if depth >= MAX_SHAPE_DEPTH:
        return "<depth-limit>"
    if isinstance(value, dict):
        return {key: _shape(item, depth + 1) for key, item in sorted(value.items())}
    if isinstance(value, list):
        if not value:
            return []
        return [_shape(value[0], depth + 1), "<x%d>" % len(value)]
    if isinstance(value, bool):
        return "bool"
    if isinstance(value, (int, float)):
        return "number"
    if value is None:
        return "null"
    return "string"


def _metadata_redacted(body: dict) -> dict:
    meta = body.get("metadata")
    if not isinstance(meta, dict):
        return {}
    out = {}
    for key, value in sorted(meta.items()):
        out[key] = redact_identifier(value) if isinstance(value, str) else _shape(value)
    return out


def _first_system_block(body: dict) -> str:
    """Prefix of system[0] — the block that carries the client identity marker."""
    system = body.get("system")
    if isinstance(system, str):
        return system[:MAX_IDENTITY_TEXT]
    if isinstance(system, list) and system:
        first = system[0]
        if isinstance(first, str):
            return first[:MAX_IDENTITY_TEXT]
        if isinstance(first, dict) and isinstance(first.get("text"), str):
            return first["text"][:MAX_IDENTITY_TEXT]
    return ""


def body_fingerprint(content: bytes) -> dict | None:
    """Shape-only fingerprint of a request body. Returns None when unparsable.

    No string from the caller's content survives except the identity marker in
    the first system block, which is a wire constant rather than user text.
    """
    if not content:
        return None
    try:
        body = json.loads(content.decode("utf-8"))
    except (UnicodeDecodeError, ValueError):
        return {"unparsable": True, "bytes": len(content)}
    if not isinstance(body, dict):
        return {"unparsable": True, "bytes": len(content)}

    fingerprint = {
        "top_level_keys": sorted(body.keys()),
        "shape": _shape(body),
        "metadata": _metadata_redacted(body),
        "system_first_block": _first_system_block(body),
    }
    for name in IDENTITY_SCALARS:
        if name in body and not isinstance(body[name], (dict, list)):
            fingerprint[name] = body[name]
    return fingerprint


def build_record(flow, capture_request_body: bool = False) -> dict:
    """Return the JSON record for one intercepted flow."""
    req = flow.request

    headers = []
    for name, value in req.headers.items():
        if SENSITIVE_NAME.search(name):
            headers.append([name, _redact(value)])
        else:
            headers.append([name, value])

    record = {
        "ts": datetime.now(timezone.utc).isoformat(),
        "http_version": req.http_version,
        "method": req.method,
        "host": req.pretty_host,
        "path": req.path,
        "request_bytes": len(req.content or b""),
        "headers": headers,
    }

    if capture_request_body:
        fingerprint = body_fingerprint(req.content or b"")
        if fingerprint is not None:
            record["request_body"] = fingerprint

    res = getattr(flow, "response", None)
    if res is None:
        return record

    record["status"] = res.status_code
    # Response headers carry no credential of ours, but a Set-Cookie could
    # start a session; redact by the same rule rather than reason about it.
    record["response_headers"] = [
        [name, "[redacted]" if SENSITIVE_NAME.search(name) else value]
        for name, value in res.headers.items()
    ]
    if res.status_code >= 400:
        body = (res.content or b"").decode("utf-8", "replace")
        record["response_body"] = body[:MAX_RESPONSE_BODY]
    return record


class HeaderDump:
    def __init__(self) -> None:
        self.out_path = os.environ.get("MITM_DUMP_OUT", "/tmp/agy-headers-mitm.jsonl")
        self.hosts = hosts_from_env()
        self.capture_request_body = request_body_enabled()

    def response(self, flow: mitmproxy.http.HTTPFlow) -> None:
        if flow.request.pretty_host.lower() not in self.hosts:
            return
        record = build_record(flow, capture_request_body=self.capture_request_body)
        with open(self.out_path, "a", encoding="utf-8") as f:
            f.write(json.dumps(record, ensure_ascii=False) + "\n")
            f.flush()


addons = [HeaderDump()]