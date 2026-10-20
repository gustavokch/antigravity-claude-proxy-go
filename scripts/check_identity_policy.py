#!/usr/bin/env python3
"""Assert WHICH requests get a Claude Code wire identity, not whether one matches.

diff_claude_code_identity.py answers "does this request match the capture?".
It cannot answer "should this request have matched the capture at all?" —
handed a deliberately unnormalized request it reports twenty drifts, every one
of them correct and every one of them missing the point.

Three policies, one per subcommand:

  normalized   the request carries the captured identity (pooled gateway, and
               a custom endpoint with no configured apiKey): every captured
               header with its value and spelling, in the captured order, over
               the captured HTTP version, with its prompt fields agreeing
  passthrough  the request carries the CALLER's identity and its own API key,
               because internal/api/server.go refuses to normalize an endpoint
               whose credential normalization would delete
  discovery    every GET /v1/models carries the captured discovery User-Agent,
               not Go's default and not a stale version, over the captured
               HTTP version

No fingerprint value is written here. Every expected value — the User-Agent,
the normalization marker headers, the captured query string — is read out of
the committed baseline capture, so the capture stays the single source of
truth. The only literals are header-name prefixes, which name a family rather
than a value.
"""

import argparse
import json
import os
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from diff_claude_code_identity import (  # noqa: E402
    compare_billing,
    compare_correlations,
    compare_headers,
    compare_host,
    compare_order,
    header_map,
)


def load_records(path: str) -> list[dict]:
    """Every record in a capture file, in order.

    A malformed line is reported with its file and line number rather than
    left to raise a bare JSONDecodeError. The gate truncates the live capture
    between phases while mitmdump may be mid-append, and a record above
    PIPE_BUF is not an atomic write, so a half-written line is a real outcome —
    and one an operator must not read as the proxy's headers having drifted.
    """
    records = []
    with open(path, encoding="utf-8") as handle:
        for number, line in enumerate(handle, start=1):
            line = line.strip()
            if not line:
                continue
            try:
                records.append(json.loads(line))
            except json.JSONDecodeError as error:
                raise ValueError(
                    f"{path} line {number} is not a JSON record ({error.msg}); "
                    "the capture is truncated or was written to mid-read, "
                    "which says nothing about the proxy's wire identity"
                ) from error
    return records


def messages_records(records: list[dict]) -> list[dict]:
    return [
        r
        for r in records
        if r.get("method") == "POST" and str(r.get("path", "")).startswith("/v1/messages")
    ]


def discovery_records(records: list[dict]) -> list[dict]:
    """Every request that is NOT the messages path.

    Deliberately wider than GET /v1/models. DiscoveryUserAgent's own comment
    calls it a different family from the messages path, so the rule is about
    the path, not about one caller: /v1/oauth/token and /api/oauth/profile in
    internal/auth belong to it too. Widening the selector costs nothing and
    means a future request that starts reaching api.anthropic.com is checked
    rather than ignored.
    """
    return [
        r
        for r in records
        if not (
            r.get("method") == "POST"
            and str(r.get("path", "")).startswith("/v1/messages")
        )
    ]


def get_records(records: list[dict]) -> list[dict]:
    return [r for r in records if r.get("method") == "GET"]


# Header families the captured identity is made of. These are prefixes, not
# values: the names under them come out of the baseline, so a capture that
# grows or drops one moves the policy with it.
MARKER_PREFIXES = ("x-claude-code-", "x-stainless-")


def captured_query(baseline: dict) -> str:
    """The query string the captured request carries, if any."""
    path = str(baseline.get("path", ""))
    _, separator, query = path.partition("?")
    return query if separator else ""


def normalization_markers(baseline: dict) -> list[str]:
    """Header names that only a normalized request carries.

    Read off the baseline rather than written here, so the passthrough policy
    rejects every marker the capture actually contains instead of the two that
    happened to be worth naming when it was written.
    """
    return sorted(
        name
        for name in header_map(baseline)
        if name.startswith(MARKER_PREFIXES)
    )


def check_record_count(observed_posts: list[dict]) -> list[str]:
    """Each phase drives exactly one request, so it must find exactly one.

    Without this the policies read observed_posts[0] and a leftover record
    decides what gets asserted. The gate snapshots a phase as soon as the
    capture file is non-empty, and build_record stores no model, so a retried
    or late-flushed earlier flow appended after that snapshot is
    indistinguishable from this phase's own and sorts ahead of it. A normalized
    request re-checked as itself passes while proving nothing.
    """
    if not observed_posts:
        return ["no POST /v1/messages record reached the wire for this phase"]
    if len(observed_posts) > 1:
        return [
            f"{len(observed_posts)} POST /v1/messages records reached the wire, want 1; "
            "a record from another phase cannot be told apart from this one"
        ]
    return []


def user_agent(record: dict) -> object:
    entry = header_map(record).get("user-agent")
    return entry[1] if entry else None


def without_header(record: dict, name: str) -> dict:
    """A copy of the record with every header of that name removed."""
    return {
        **record,
        "headers": [pair for pair in record.get("headers", []) if pair[0].lower() != name],
    }


def check_normalized(
    baseline: dict,
    observed: dict,
    expect_turn_index: int | None = None,
    tls_reference: dict | None = None,
    keyless: bool = False,
) -> list[str]:
    """The observed request must present the captured identity.

    Every header, its value, its spelling and the HTTP version go through the
    differ's compare_headers, so a request is held to the capture by this
    policy as it is by the differ. The keyless custom-endpoint phase is checked
    by this policy alone, and without that comparison it passed a request with
    no x-claude-code-prompt-id, no X-Stainless-OS and Accept-Encoding: gzip.
    The values that differ between any two requests, or between the capture's
    container and this host, are the differ's own exemptions and are not
    repeated here.

    keyless is a request to a custom endpoint that configures no apiKey. It is
    sent no credential: forwardToCustomEndpoint deletes the caller's
    Authorization. The capture's request carries one, so such a request is held
    to the capture without it, which also makes a credential that reaches the
    endpoint a drift.

    expect_turn_index, when given, is the number of user prompts the request
    carried: cc_turn_index counts them, so a request sent with two must say 2.
    """
    problems: list[str] = []
    headers = header_map(observed)
    if baseline.get("http_version") is None:
        # compare_headers reads a missing version as agreement with a request
        # that records none, which would let an h2 request pass in silence.
        problems.append("the baseline records no http_version, so this policy cannot tell HTTP/1.1 from h2")
    reference = without_header(baseline, "authorization") if keyless else baseline
    problems.extend(compare_headers([reference], observed))
    problems.extend(compare_correlations(observed))
    problems.extend(compare_billing(observed))
    problems.extend(compare_order(reference, observed))
    problems.extend(compare_host(observed))
    if tls_reference is not None:
        problems.extend(check_tls(tls_reference, observed, "fetch"))

    if expect_turn_index is not None:
        body = observed.get("request_body")
        block = body.get("system_first_block") if isinstance(body, dict) else None
        match = re.search(r"cc_turn_index=(\d+);", block or "")
        if match is None:
            problems.append("system[0] carries no cc_turn_index to check")
        elif int(match.group(1)) != expect_turn_index:
            problems.append(
                f"cc_turn_index is {match.group(1)}, want {expect_turn_index}: "
                f"the request carried {expect_turn_index} user prompt(s)"
            )

    expected_ua = user_agent(baseline)
    observed_ua = user_agent(observed)
    if observed_ua != expected_ua:
        problems.append(f"User-Agent is {observed_ua!r}, want the captured {expected_ua!r}")

    expected_class = header_map(baseline).get("x-claude-code-request-class")
    if expected_class is None:
        # Skipping the assertion here would degrade this policy to a
        # User-Agent comparison without saying so, which is the failure mode
        # this whole checker exists to rule out. check_discovery treats a
        # baseline with no GET the same way.
        problems.append(
            "the baseline POST carries no x-claude-code-request-class, so this "
            "policy cannot tell a normalized request from an unnormalized one"
        )
    else:
        entry = headers.get("x-claude-code-request-class")
        if entry is None:
            problems.append("x-claude-code-request-class is absent; the request was not normalized")
        elif entry[1] != expected_class[1]:
            problems.append(
                f"x-claude-code-request-class is {entry[1]!r}, want {expected_class[1]!r}"
            )

    if "x-api-key" in headers:
        problems.append("x-api-key reached the upstream; the captured OAuth request carries none")

    expected_query = captured_query(baseline)
    if expected_query and expected_query not in str(observed.get("path", "")):
        problems.append(
            f"path is {observed.get('path')!r}; the captured request carries {expected_query}"
        )

    return problems


def check_passthrough(
    observed: dict,
    caller_user_agent: str,
    api_key_sha256: str,
    baseline: dict,
) -> list[str]:
    """The observed request must present the CALLER's identity and its own key."""
    problems: list[str] = []
    headers = header_map(observed)

    entry = headers.get("x-api-key")
    if entry is None:
        problems.append(
            "x-api-key is absent; an endpoint configured with one cannot authenticate without it"
        )
    else:
        value = entry[1]
        digest = value.get("sha256") if isinstance(value, dict) else None
        if digest is None:
            problems.append(f"x-api-key was recorded as {value!r}, want a redacted sha256")
        elif digest != api_key_sha256:
            problems.append(
                f"x-api-key sha256 is {digest}, want {api_key_sha256}; a different key was sent"
            )

    observed_ua = user_agent(observed)
    if observed_ua != caller_user_agent:
        problems.append(
            f"User-Agent is {observed_ua!r}, want the caller's {caller_user_agent!r}; "
            "this endpoint must not be normalized"
        )

    for name in normalization_markers(baseline):
        if name in headers:
            problems.append(f"{name} was added; this endpoint must not be normalized")

    expected_query = captured_query(baseline)
    if expected_query and expected_query in str(observed.get("path", "")):
        problems.append(
            f"path is {observed.get('path')!r}; the captured query must not be added here"
        )

    return problems


def check_discovery(
    baseline_gets: list[dict],
    observed_gets: list[dict],
    min_records: int,
    tls_reference: dict | None = None,
) -> list[str]:
    """Every observed non-messages request must carry the captured User-Agent."""
    problems: list[str] = []

    expected = {user_agent(r) for r in baseline_gets}
    expected.discard(None)
    if not expected:
        problems.append("the baseline holds no GET record to take a discovery User-Agent from")
        return problems
    if len(expected) > 1:
        problems.append(f"the baseline GETs disagree on User-Agent: {sorted(expected)}")
        return problems
    expected_ua = expected.pop()

    versions = {r.get("http_version") for r in baseline_gets}
    versions.discard(None)
    if len(versions) != 1:
        problems.append(
            f"the baseline GETs record {sorted(versions) or 'no http_version'}, want exactly one "
            "to compare the observed requests against"
        )
        return problems
    expected_version = versions.pop()

    if len(observed_gets) < min_records:
        problems.append(
            f"only {len(observed_gets)} discovery request(s) reached the wire, want {min_records}"
        )

    for record in observed_gets:
        observed_ua = user_agent(record)
        if observed_ua != expected_ua:
            problems.append(
                f"{record.get('method')} {record.get('path')} sent User-Agent "
                f"{observed_ua!r}, want the captured {expected_ua!r}"
            )
        # The real client's discovery requests are HTTP/1.1 like its messages
        # requests, so a request that arrives as h2 is a tell.
        if record.get("http_version") != expected_version:
            problems.append(
                f"{record.get('method')} {record.get('path')} arrived as "
                f"{record.get('http_version')!r}, want the captured {expected_version!r}"
            )
        # The node stack's tail: the Accept-Encoding it advertises, closing the
        # connection after each request, and writing its headers in the order
        # the first captured GET shows (settings; policy_limits orders its own
        # headers differently, which no single rule reproduces).
        reference_headers = header_map(baseline_gets[0])
        observed_headers = header_map(record)
        for key in ("accept-encoding", "connection"):
            want = reference_headers.get(key)
            got = observed_headers.get(key)
            if want is not None and (got is None or got[1] != want[1]):
                problems.append(
                    f"{record.get('method')} {record.get('path')} sent {key} "
                    f"{got[1] if got else None!r}, want the captured {want[1]!r}"
                )
        problems.extend(compare_order(baseline_gets[0], record))
        if tls_reference is not None:
            problems.extend(check_tls(tls_reference, record, "node"))

    return problems


def check_tls(reference: dict, observed: dict, stack: str) -> list[str]:
    """The request must have been sent by the TLS stack the real client uses for it.

    Claude Code has two: fetch for messages and node:https for the discovery
    GETs. The reference (.reference/claude-code-tls-*.json) records the JA4 and
    JA3 each one presents, as the first connection to a host (ja4, ja3) and as
    every later, resumed one (ja4_resumed, ja3_resumed). Either form is the real
    client; a JA4 from one form with the JA3 of the other is not. A record
    without a fingerprint is a problem, not a pass: it means the capture was
    taken without the current addon.
    """
    expected = (reference.get("stacks") or {}).get(stack)
    if not isinstance(expected, dict):
        return [f"the TLS reference has no {stack!r} stack to compare against"]
    got = observed.get("tls")
    what = f"{observed.get('method')} {observed.get('path')}"
    if not isinstance(got, dict):
        return [f"{what} carries no TLS fingerprint (record with the current scripts/mitm_header_dump.py)"]
    forms = [(expected.get("ja4"), expected.get("ja3"))]
    if expected.get("ja4_resumed") and expected.get("ja3_resumed"):
        forms.append((expected["ja4_resumed"], expected["ja3_resumed"]))
    if (got.get("ja4"), got.get("ja3")) in forms:
        return []
    problems = []
    for field in ("ja4", "ja3"):
        if got.get(field) not in {form[0 if field == "ja4" else 1] for form in forms}:
            problems.append(
                f"{what} {field} is {got.get(field)}, the real client's {stack} stack "
                f"presents {expected.get(field)}"
            )
    return problems or [
        f"{what} presents a JA4 and a JA3 from different forms of the {stack} stack's hello"
    ]


def load_tls_reference(path: str) -> dict:
    with open(path, encoding="utf-8") as handle:
        return json.load(handle)


def evaluate(args) -> tuple[list[str], str]:
    """The policy's problems, and the subject they are reported against."""
    if args.policy == "discovery":
        baseline_gets = get_records(load_records(args.baseline))
        observed_gets = discovery_records(load_records(args.observed))
        return (
            check_discovery(
                baseline_gets, observed_gets, args.min_records, load_tls_reference(args.tls_reference)
            ),
            f"{len(observed_gets)} discovery request(s)",
        )

    observed_posts = messages_records(load_records(args.observed))
    count_problems = check_record_count(observed_posts)
    if count_problems:
        return count_problems, args.observed

    observed = observed_posts[0]
    subject = f"POST {observed.get('path')}"
    baseline_posts = messages_records(load_records(args.baseline))
    if not baseline_posts:
        raise ValueError(f"no POST /v1/messages record in {args.baseline}")
    if args.policy == "normalized":
        return (
            check_normalized(
                baseline_posts[0],
                observed,
                args.expect_turn_index,
                load_tls_reference(args.tls_reference),
                keyless=args.keyless,
            ),
            subject,
        )
    return (
        check_passthrough(
            observed, args.caller_user_agent, args.api_key_sha256, baseline_posts[0]
        ),
        subject,
    )


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="policy", required=True)

    p_norm = sub.add_parser("normalized")
    p_norm.add_argument("--baseline", required=True)
    p_norm.add_argument("--observed", required=True)
    p_norm.add_argument(
        "--expect-turn-index",
        type=int,
        help="the number of user prompts the observed request carried",
    )
    p_norm.add_argument("--tls-reference", required=True)
    p_norm.add_argument(
        "--keyless",
        action="store_true",
        help="the request went to an endpoint with no credential of its own, so it carries no Authorization",
    )

    p_pass = sub.add_parser("passthrough")
    p_pass.add_argument("--baseline", required=True)
    p_pass.add_argument("--observed", required=True)
    p_pass.add_argument("--caller-user-agent", required=True)
    p_pass.add_argument("--api-key-sha256", required=True)

    p_disc = sub.add_parser("discovery")
    p_disc.add_argument("--baseline", required=True)
    p_disc.add_argument("--observed", required=True)
    p_disc.add_argument("--min-records", type=int, default=1)
    p_disc.add_argument("--tls-reference", required=True)

    args = parser.parse_args()

    # An unreadable or half-written capture is a problem with the capture, not
    # a verdict on the proxy. Report it as itself rather than as a traceback
    # under the caller's "the wire identity has drifted" message.
    try:
        problems, subject = evaluate(args)
    except (OSError, ValueError) as error:
        print(f"cannot evaluate policy {args.policy}: {error}", file=sys.stderr)
        return 1

    if problems:
        print(f"{len(problems)} policy violation(s) in {subject}:", file=sys.stderr)
        for problem in problems:
            print(f"  - {problem}", file=sys.stderr)
        return 1

    print(f"policy {args.policy} holds for {subject}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
