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
import platform
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
    "x-claude-code-prompt-id",  # a fresh UUID per user prompt; equals cc_prompt_id
    # The OS and architecture of the machine the proxy runs on, not the
    # capture's container. compare_host checks them against this host instead.
    "x-stainless-os",
    "x-stainless-arch",
}

# The Stainless SDK's spellings of the machine, as internal/ccidentity/defaults.go
# HostOS and HostArch produce them. The SDK reads Node's process.platform and
# process.arch and runs them through normalizePlatform and normalizeArch
# (anthropic-sdk-typescript, src/internal/detect-platform.ts); what Python
# reports is first renamed to Node's name for the same system, then normalized.
#
# platform.system() lowercases to Node's name except for Windows, which Node
# calls win32.
NODE_PLATFORM = {"windows": "win32"}
# platform.machine() is lowercased and then looked up here; the 32-bit ARM
# spellings (armv6l, armv7l, armv8l) are handled in node_arch.
NODE_ARCH = {
    "x86_64": "x64", "amd64": "x64",
    "aarch64": "arm64", "arm64": "arm64",
    "i386": "ia32", "i486": "ia32", "i586": "ia32", "i686": "ia32", "x86": "ia32",
    "ppc64le": "ppc64",
    "loongarch64": "loong64",
}


def node_platform(system: str) -> str:
    name = system.lower()
    return NODE_PLATFORM.get(name, name)


def node_arch(machine: str) -> str:
    name = machine.lower()
    if name == "arm" or re.fullmatch(r"armv\d+\w*", name):
        return "arm"
    return NODE_ARCH.get(name, name)


def normalize_platform(platform_name: str) -> str:
    """The SDK's normalizePlatform, rule for rule."""
    platform_name = platform_name.lower()
    if "ios" in platform_name:
        return "iOS"
    for node_name, spelling in (
        ("android", "Android"), ("darwin", "MacOS"), ("win32", "Windows"),
        ("freebsd", "FreeBSD"), ("openbsd", "OpenBSD"), ("linux", "Linux"),
    ):
        if platform_name == node_name:
            return spelling
    return "Other:" + platform_name if platform_name else "Unknown"


def normalize_arch(arch: str) -> str:
    """The SDK's normalizeArch, rule for rule."""
    if arch == "x32":
        return "x32"
    if arch in ("x86_64", "x64"):
        return "x64"
    if arch == "arm":
        return "arm"
    if arch in ("aarch64", "arm64"):
        return "arm64"
    return "other:" + arch if arch else "unknown"


# Headers HTTP/2 does not carry as ordinary fields. Exempt from the presence
# check only when the two captures disagree on HTTP version, which is itself
# reported as drift: over h2 the authority travels as the :authority
# pseudo-header, which mitmproxy does not list among the request headers.
HTTP2_PSEUDO = {"host"}


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
    r"cc_turn_origin=[\w-]+; "
    # Appended by Claude Code 2.1.304 to every request; the 2.1.280 capture has
    # neither field, and a header without them is the previous client.
    r"cc_prompt_index=\d+; "
    r"cc_turn_index=\d+;$"
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

    # The real client speaks HTTP/1.1 on every request: all four committed
    # captures say so, through an interceptor that negotiates h2 with any client
    # that offers it. A request that arrives as h2 is therefore a difference the
    # server sees without reading a header, and it is reported as one.
    #
    # HTTP/2 also lowercases every field name and carries the authority as a
    # pseudo-header, so when the versions disagree the case and Host comparisons
    # below would each report the same root cause again. They are skipped then,
    # and compared when the versions agree, which is where a net/http
    # canonicalisation bug would show.
    baseline_version = baseline[0].get("http_version")
    observed_version = observed.get("http_version")
    same_version = baseline_version == observed_version
    if not same_version:
        drifts.append(
            f"HTTP version changed: baseline {baseline_version}, observed {observed_version} "
            "(the real client speaks HTTP/1.1; a transport that offers h2 gets it)"
        )

    for key, (wire_name, value) in sorted(expected.items()):
        if key in CONDITIONAL:
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
        if key in expected:
            continue
        drifts.append(
            f"unexpected header not in the baseline: {observed_name}: {observed_value!r}"
        )

    return drifts


def host_identity() -> tuple[str, str]:
    """The Stainless OS and architecture of the machine this script runs on.

    The gate drives the proxy on this machine, so these are the values its
    TCP/IP stack backs up.
    """
    return (
        normalize_platform(node_platform(platform.system())),
        normalize_arch(node_arch(platform.machine())),
    )


def compare_host(observed: dict, host: tuple[str, str] | None = None) -> list[str]:
    """X-Stainless-OS and X-Stainless-Arch must be the machine's own.

    The capture ran in a Linux container, so its values describe that container.
    A proxy that sent them from another machine would claim an OS its network
    stack contradicts: the SYN's TTL, window size and options say which OS sent
    it, whatever the headers claim. The real client reports the machine it runs
    on, so the proxy defaults to the same. A header the request does not carry is
    reported by compare_headers, once, and is not repeated here.
    """
    want_os, want_arch = host or host_identity()
    seen = header_map(observed)
    drifts = []
    for key, want in (("x-stainless-os", want_os), ("x-stainless-arch", want_arch)):
        if key in seen and seen[key][1] != want:
            drifts.append(
                f"{seen[key][0]} is {seen[key][1]!r}, but this host is {want!r}: "
                "the headers must describe the machine the proxy runs on"
            )
    return drifts


def compare_order(baseline: dict, observed: dict) -> list[str]:
    """The headers the two requests share must be written in the same order.

    The order is part of the wire: the real client's fetch stack writes its own
    headers bytewise-sorted and then Connection, Host, Accept-Encoding and
    Content-Length, where net/http writes Host and User-Agent first and sorts
    the rest. The comparison is over the headers both requests carry, so a
    header only one of them has is reported by compare_headers, once, and does
    not also shift the order. Over HTTP/2 field order is not on the wire the
    same way, and the version difference is reported elsewhere.
    """
    if baseline.get("http_version") != observed.get("http_version"):
        return []
    base = [name.lower() for name, _ in baseline.get("headers", [])]
    seen = [name.lower() for name, _ in observed.get("headers", [])]
    shared = set(base) & set(seen)
    base = [name for name in base if name in shared]
    seen = [name for name in seen if name in shared]
    if base == seen:
        return []
    at = next((i for i, (a, b) in enumerate(zip(base, seen)) if a != b), min(len(base), len(seen)))
    want = base[at] if at < len(base) else None
    got = seen[at] if at < len(seen) else None
    return [
        "header order differs from the baseline at position "
        f"{at}: baseline writes {want!r} there, observed writes {got!r}"
    ]


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
    elif observed_user_id != baseline_user_id:
        # account_uuid is compared for equality, not exempted. It was empty in
        # 6 of 6 captured requests across two independent OAuth credentials
        # (2026-09-23), so a populated value is a field no real client emits.
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


def compare_correlations(observed: dict) -> list[str]:
    """Drifts in the RELATION between two values, not in either value alone.

    x-claude-code-session-id, x-claude-code-prompt-id and cc_prompt_id are all
    exempt from value comparison — none of them survives across captures. That
    leaves the relations between them unchecked, and a proxy can satisfy every
    single-field rule here while deriving two of them wrongly.

    Two relations hold in every captured request:

    - The session id and cc_prompt_id are different UUIDs. They differ in all
      six 2.1.280 requests across two independent OAuth accounts, and in all
      seven 2.1.304 requests:

          5c1c7cec-… / 7cd5023f-…    ca6f5fe3-… / 9f414346-…
          ccbbdab8-… / b0f9a831-…    2ca893cf-… / fc5d5bcf-…

    - x-claude-code-prompt-id, new in 2.1.304, equals cc_prompt_id. It does in
      all seven 2.1.304 requests.

    Redacted placeholders are skipped: a capture recorded with redaction on
    carries <uuid> in both slots, which is the redactor agreeing with itself.
    """
    drifts: list[str] = []

    headers = header_map(observed)
    session_pair = headers.get("x-claude-code-session-id")
    session_id = session_pair[1] if session_pair else None
    prompt_header_pair = headers.get("x-claude-code-prompt-id")
    prompt_header = prompt_header_pair[1] if prompt_header_pair else None

    billing = observed.get("request_body", {})
    billing = billing.get("system_first_block") if isinstance(billing, dict) else None
    match = re.search(r"cc_prompt_id=(\S+?);", billing or "")
    prompt_id = match.group(1) if match else None

    def concrete(value) -> bool:
        return isinstance(value, str) and bool(value) and not value.startswith("<")

    if concrete(session_id) and concrete(prompt_id) and session_id == prompt_id:
        drifts.append(
            "x-claude-code-session-id and cc_prompt_id are the same value "
            f"({session_id}); the capture shows them different in every request "
            "across two accounts, so one seed for both is a tell"
        )

    if concrete(prompt_header) and concrete(prompt_id) and prompt_header != prompt_id:
        drifts.append(
            f"x-claude-code-prompt-id ({prompt_header}) and cc_prompt_id "
            f"({prompt_id}) differ; the capture shows one value in both slots "
            "in every 2.1.304 request"
        )

    return drifts


def compare_billing(observed: dict) -> list[str]:
    """Drifts between the billing header's hashed fields and the body.

    Both values are derived by the real client from the request itself:

    - cch is the hash of the body bytes as sent.
    - The cc_version suffix is the hash of the first prompt's text.

    The addon records each value next to the one scripts/cc_billing.py derives
    from the same bytes, and the derivation reproduces every body captured from
    Claude Code 2.1.304. A record without that verdict is a drift, not a pass:
    it means the capture was taken with an older addon and nothing was checked.
    """
    verdict = observed.get("request_body", {})
    verdict = verdict.get("billing") if isinstance(verdict, dict) else None
    if not isinstance(verdict, dict):
        return [
            "the record carries no billing verdict, so cch and the cc_version "
            "suffix are unchecked (record with the current scripts/mitm_header_dump.py)"
        ]
    drifts: list[str] = []
    for field, what in (("cch", "the hash of the body as sent"),
                        ("version_suffix", "the hash of the first prompt")):
        sent, expected = verdict.get(field), verdict.get(field + "_expected")
        if sent is None or expected is None:
            drifts.append(f"billing {field} could not be located in the body")
        elif sent != expected:
            drifts.append(
                f"billing {field} is {sent}, the real client sends {expected}: "
                f"it must be {what}"
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
    drifts.extend(compare_order(baseline[0], observed))
    drifts.extend(compare_host(observed))
    drifts.extend(compare_body(baseline, observed))
    drifts.extend(compare_correlations(observed))
    drifts.extend(compare_billing(observed))

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
