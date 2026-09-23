#!/usr/bin/env python3
"""Diff an observed Claude Code wire identity against the committed capture.

Reads two mitm_header_dump.py JSONL files — the committed baseline and one
recorded from the proxy — and reports every drift between them. Exits 1 when
any drift is found.

Called by scripts/verify-claude-code-identity.sh; usable by hand against any
two captures.

Every exemption below names the reason it exists. An exemption is a recorded
decision, so widening this list is a visible diff rather than a silent pass.
"""

from __future__ import annotations

import argparse
import json
import re
import sys

# Values that legitimately differ between any two requests or processes. The
# gate asserts these are PRESENT, never that they are equal.
DYNAMIC_VALUE = {
    "authorization",           # redacted to a hash by the addon, per-account
    "content-length",          # the gate's request body is not the capture's
    "host",                    # mitmdump rewrites this
    "x-claude-code-session-id",  # stable per process, not across processes
    "x-client-request-id",     # fresh per request
}

# Headers HTTP/2 does not carry as ordinary fields. Exempt from the presence
# check only when the two captures disagree on HTTP version: over h2 the
# authority travels as the :authority pseudo-header, which mitmproxy does not
# list among the request headers.
HTTP2_PSEUDO = {"host"}

# Differences the implementation records as deliberate. See the "Deliberate
# gaps" section of docs/superpowers/plans/2026-09-23-claude-code-identity-spoof.md.
ALLOWED_DIFFERENCES = {
    # Spoofing "gzip, deflate, br, zstd" would stop http.Transport decompressing
    # transparently, and br/zstd are not in the Go stdlib. Cleared, not spoofed.
    "accept-encoding",
    # net/http owns connection reuse and does not emit this on HTTP/1.1
    # keep-alive; it is illegal on HTTP/2.
    "connection",
}

# Present in the baseline only under conditions a single synthetic request
# cannot reproduce.
CONDITIONAL = {
    # Emitted only on the turn after a tool has run.
    "x-claude-code-prev-tool-durations",
}

# Body fields Claude Code does not send, which clients routinely do. Their
# presence upstream is the fingerprint normalization exists to remove.
FORBIDDEN_BODY_KEYS = [
    "temperature",
    "top_p",
    "top_k",
    "stop_sequences",
    "tool_choice",
    "service_tier",
]

BILLING_HEADER_RE = re.compile(
    r"^x-anthropic-billing-header: "
    r"cc_version=\d+\.\d+\.\d+\.[0-9a-f]{3}; "
    r"cc_entrypoint=[\w-]+; "
    r"cch=[0-9a-f]{5}; "
    r"(?:cc_prev_req=\S+; )?"
    r"cc_prompt_id=(?:<uuid>|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}); "
    r"cc_turn_origin=[\w-]+;$"
)


def load_messages_records(path: str) -> list[dict]:
    """Every POST /v1/messages record in a capture file, in order."""
    records = []
    with open(path, encoding="utf-8") as handle:
        for line in handle:
            line = line.strip()
            if not line:
                continue
            record = json.loads(line)
            if record.get("method") != "POST":
                continue
            if not str(record.get("path", "")).startswith("/v1/messages"):
                continue
            records.append(record)
    return records


def header_map(record: dict) -> dict[str, tuple[str, object]]:
    """Lowercased name -> (wire name, value).

    Lowercasing is the lookup key only. The wire name is kept so a
    canonicalisation bug — X-Stainless-OS becoming X-Stainless-Os — is caught
    rather than folded away.
    """
    out = {}
    for name, value in record.get("headers", []):
        out[name.lower()] = (name, value)
    return out


def compare_headers(baseline: list[dict], observed: dict) -> list[str]:
    drifts: list[str] = []

    # A header seen in ANY baseline messages request is expected; the first
    # request alone would miss the conditional ones.
    expected: dict[str, tuple[str, object]] = {}
    for record in baseline:
        for key, pair in header_map(record).items():
            expected.setdefault(key, pair)

    seen = header_map(observed)

    # HTTP/2 lowercases every field name on the wire, so exact case is only
    # meaningful when both captures used the same version. The capture is
    # HTTP/1.1 and a live run to api.anthropic.com negotiates h2, which is the
    # normal case — comparing case across them would report drift on every
    # header forever. Case is still asserted when the versions agree, because
    # that is where a net/http canonicalisation bug would show.
    baseline_version = baseline[0].get("http_version")
    observed_version = observed.get("http_version")
    same_version = baseline_version == observed_version

    for key, (wire_name, value) in sorted(expected.items()):
        if key in ALLOWED_DIFFERENCES or key in CONDITIONAL:
            continue
        if key not in seen:
            if not same_version and key in HTTP2_PSEUDO:
                continue
            drifts.append(f"missing header: {wire_name}")
            continue
        observed_name, observed_value = seen[key]
        if same_version and observed_name != wire_name:
            drifts.append(
                f"header name case changed: baseline {wire_name!r}, "
                f"observed {observed_name!r} "
                "(net/http canonicalises names it parses; assign the map directly)"
            )
        if key in DYNAMIC_VALUE:
            continue
        if observed_value != value:
            drifts.append(
                f"header value changed: {wire_name}\n"
                f"  baseline: {value!r}\n"
                f"  observed: {observed_value!r}"
            )

    for key, (observed_name, observed_value) in sorted(seen.items()):
        if key in expected or key in ALLOWED_DIFFERENCES:
            continue
        drifts.append(
            f"unexpected header not in the baseline: {observed_name}: {observed_value!r}"
        )

    return drifts


def compare_body(baseline: list[dict], observed: dict) -> list[str]:
    drifts: list[str] = []

    observed_body = observed.get("request_body")
    if not isinstance(observed_body, dict):
        return ["observed record carries no request_body fingerprint "
                "(set MITM_DUMP_REQUEST_BODY=1 on the capture)"]

    baseline_bodies = [
        record["request_body"]
        for record in baseline
        if isinstance(record.get("request_body"), dict)
    ]
    if not baseline_bodies:
        return ["baseline carries no request_body fingerprint to compare against"]
    reference = baseline_bodies[0]

    observed_keys = set(observed_body.get("top_level_keys") or [])
    baseline_keys = set(reference.get("top_level_keys") or [])
    extra = sorted(observed_keys - baseline_keys)
    if extra:
        drifts.append(
            "body carries top-level keys Claude Code does not send: "
            + ", ".join(extra)
        )
    # Missing keys are NOT drift: context_management, diagnostics and
    # output_config are recorded gaps, and stream/thinking/tools depend on the
    # client's own request.
    present_forbidden = [key for key in FORBIDDEN_BODY_KEYS if key in observed_keys]
    if present_forbidden:
        drifts.append(
            "body leaks client fields Claude Code strips: "
            + ", ".join(present_forbidden)
        )

    observed_user_id = (observed_body.get("metadata") or {}).get("user_id")
    baseline_user_id = (reference.get("metadata") or {}).get("user_id")
    if observed_user_id is None:
        drifts.append("body has no metadata.user_id")
    else:
        # account_uuid is exempt from equality: the capture recorded it empty in
        # every sample, the pooled path sends the real account's UUID, and
        # whether Claude Code ever populates it is an OPEN QUESTION pending a
        # second capture on a different account. Do not resolve it here.
        normalize = lambda text: re.sub(  # noqa: E731
            r'"account_uuid":"[^"]*"', '"account_uuid":"<exempt>"', text or ""
        )
        if normalize(observed_user_id) != normalize(baseline_user_id):
            drifts.append(
                "metadata.user_id shape changed\n"
                f"  baseline: {baseline_user_id!r}\n"
                f"  observed: {observed_user_id!r}"
            )

    observed_system = observed_body.get("system_first_block")
    if not observed_system:
        drifts.append("body has no system[0] billing header")
    elif not BILLING_HEADER_RE.match(observed_system):
        drifts.append(
            "system[0] billing header does not match the captured format\n"
            f"  observed: {observed_system!r}"
        )

    return drifts


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline", required=True)
    parser.add_argument("--observed", required=True)
    args = parser.parse_args()

    baseline = load_messages_records(args.baseline)
    if not baseline:
        print(f"no POST /v1/messages record in {args.baseline}", file=sys.stderr)
        return 1
    observed_records = load_messages_records(args.observed)
    if not observed_records:
        print(f"no POST /v1/messages record in {args.observed}", file=sys.stderr)
        return 1
    observed = observed_records[0]

    drifts: list[str] = []
    if observed.get("path") != baseline[0].get("path"):
        drifts.append(
            f"request path changed: baseline {baseline[0].get('path')!r}, "
            f"observed {observed.get('path')!r}"
        )
    drifts.extend(compare_headers(baseline, observed))
    drifts.extend(compare_body(baseline, observed))

    if drifts:
        print(f"{len(drifts)} drift(s) against {args.baseline}:", file=sys.stderr)
        for drift in drifts:
            print(f"  - {drift}", file=sys.stderr)
        return 1

    header_count = len(header_map(observed))
    print(
        f"no drift: {header_count} headers over {observed.get('http_version')}, "
        f"path {observed.get('path')}, "
        "metadata.user_id and system[0] both match the captured format"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
