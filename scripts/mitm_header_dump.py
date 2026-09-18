"""mitmproxy addon: dump Cloud Code requests AND responses as ordered JSONL.

Records every intercepted exchange with the two Cloud Code hosts as one JSON
line: ts, http_version, method, host, path, request_bytes, headers, status,
response_headers and — for error statuses only — response_body.

The Authorization value is a live OAuth token, so it is redacted HERE,
before anything is written: scheme (first whitespace token) plus a sha256
and length of the credential token only. Matching is a substring,
case-insensitive match on the header name so variants like
x-goog-iam-authorization-list are over-redacted on purpose.

Response bodies are kept for error statuses ONLY. A 200 on
streamGenerateContent is the user's own generated text and must never land
in a capture file; the 4xx body is what carries the throttle dimension this
capture exists to find.

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


def build_record(flow) -> dict:
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

    def response(self, flow: mitmproxy.http.HTTPFlow) -> None:
        if flow.request.pretty_host not in CLOUDCODE_HOSTS:
            return
        record = build_record(flow)
        with open(self.out_path, "a", encoding="utf-8") as f:
            f.write(json.dumps(record, ensure_ascii=False) + "\n")
            f.flush()


addons = [HeaderDump()]
