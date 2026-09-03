"""mitmproxy addon: dump Cloud Code request headers as ordered JSONL.

Records every intercepted request to the two Cloud Code hosts as one JSON
line: ts, http_version, method, host, path, headers — where headers is the
ordered list of [name, value] pairs exactly as received on the wire
(mitmproxy preserves name casing and order; it does not preserve raw octets
like whitespace around the colon).

The Authorization value is a live OAuth token, so it is redacted HERE,
before anything is written: scheme (first whitespace token) plus a sha256
and length of the credential token only. Matching is a substring,
case-insensitive match on the header name so variants like
x-goog-iam-authorization-list are over-redacted on purpose.

Output path comes from $MITM_DUMP_OUT (default /tmp/agy-headers-mitm.jsonl),
appended, flushed per line.
"""

import hashlib
import json
import os
import re
from datetime import datetime, timezone

import mitmproxy.http

CLOUDCODE_HOSTS = {
    "cloudcode-pa.googleapis.com",
    "daily-cloudcode-pa.googleapis.com",
}

# Substring match on purpose: over-redact anything auth-ish.
SENSITIVE_NAME = re.compile(
    r"authorization|proxy-authorization|x-goog-iam-authorization",
    re.IGNORECASE,
)


def _redact(value: str) -> dict:
    parts = value.split(None, 1)
    scheme = parts[0] if parts else ""
    token = parts[1] if len(parts) > 1 else ""
    return {
        "redacted": True,
        "scheme": scheme,
        "sha256": hashlib.sha256(token.encode("utf-8")).hexdigest(),
        "len": len(token),
    }


class HeaderDump:
    def __init__(self) -> None:
        self.out_path = os.environ.get("MITM_DUMP_OUT", "/tmp/agy-headers-mitm.jsonl")

    def response(self, flow: mitmproxy.http.HTTPFlow) -> None:
        req = flow.request
        host = req.pretty_host
        if host not in CLOUDCODE_HOSTS:
            return

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
            "host": host,
            "path": req.path,
            "headers": headers,
        }
        with open(self.out_path, "a", encoding="utf-8") as f:
            f.write(json.dumps(record, ensure_ascii=False) + "\n")
            f.flush()


addons = [HeaderDump()]
