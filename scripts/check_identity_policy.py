#!/usr/bin/env python3
"""Assert WHICH requests get a Claude Code wire identity, not whether one matches.

diff_claude_code_identity.py answers "does this request match the capture?".
It cannot answer "should this request have matched the capture at all?" —
handed a deliberately unnormalized request it reports twenty drifts, every one
of them correct and every one of them missing the point.

Three policies, one per subcommand:

  normalized   the request carries the captured identity (pooled gateway, and
               a custom endpoint with no configured apiKey)
  passthrough  the request carries the CALLER's identity and its own API key,
               because internal/api/server.go refuses to normalize an endpoint
               whose credential normalization would delete
  discovery    every GET /v1/models carries the captured discovery User-Agent,
               not Go's default and not a stale version

No fingerprint value is written here. Each expected value is read out of the
committed baseline capture, so the capture stays the single source of truth.
"""

import argparse
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from diff_claude_code_identity import header_map  # noqa: E402


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


def models_records(records: list[dict]) -> list[dict]:
    return [
        r
        for r in records
        if r.get("method") == "GET" and str(r.get("path", "")).startswith("/v1/models")
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


def check_normalized(baseline: dict, observed: dict) -> list[str]:
    """The observed request must present the captured identity."""
    problems: list[str] = []
    headers = header_map(observed)

    expected_ua = user_agent(baseline)
    observed_ua = user_agent(observed)
    if observed_ua != expected_ua:
        problems.append(f"User-Agent is {observed_ua!r}, want the captured {expected_ua!r}")

    expected_class = header_map(baseline).get("x-claude-code-request-class")
    if expected_class is not None:
        entry = headers.get("x-claude-code-request-class")
        if entry is None:
            problems.append("x-claude-code-request-class is absent; the request was not normalized")
        elif entry[1] != expected_class[1]:
            problems.append(
                f"x-claude-code-request-class is {entry[1]!r}, want {expected_class[1]!r}"
            )

    if "x-api-key" in headers:
        problems.append("x-api-key reached the upstream; the captured OAuth request carries none")

    if "beta=true" not in str(observed.get("path", "")):
        problems.append(
            f"path is {observed.get('path')!r}; the captured request carries beta=true"
        )

    return problems


def check_passthrough(observed: dict, caller_user_agent: str, api_key_sha256: str) -> list[str]:
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

    for name in ("x-claude-code-request-class", "x-stainless-os"):
        if name in headers:
            problems.append(f"{name} was added; this endpoint must not be normalized")

    if "beta=true" in str(observed.get("path", "")):
        problems.append(
            f"path is {observed.get('path')!r}; the captured query must not be added here"
        )

    return problems


def check_discovery(
    baseline_gets: list[dict], observed_gets: list[dict], min_records: int
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

    return problems


def evaluate(args) -> tuple[list[str], str]:
    """The policy's problems, and the subject they are reported against."""
    if args.policy == "discovery":
        baseline_gets = get_records(load_records(args.baseline))
        observed_gets = discovery_records(load_records(args.observed))
        return (
            check_discovery(baseline_gets, observed_gets, args.min_records),
            f"{len(observed_gets)} discovery request(s)",
        )

    observed_posts = messages_records(load_records(args.observed))
    count_problems = check_record_count(observed_posts)
    if count_problems:
        return count_problems, args.observed

    observed = observed_posts[0]
    subject = f"POST {observed.get('path')}"
    if args.policy == "normalized":
        baseline_posts = messages_records(load_records(args.baseline))
        if not baseline_posts:
            raise ValueError(f"no POST /v1/messages record in {args.baseline}")
        return check_normalized(baseline_posts[0], observed), subject
    return (
        check_passthrough(observed, args.caller_user_agent, args.api_key_sha256),
        subject,
    )


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="policy", required=True)

    p_norm = sub.add_parser("normalized")
    p_norm.add_argument("--baseline", required=True)
    p_norm.add_argument("--observed", required=True)

    p_pass = sub.add_parser("passthrough")
    p_pass.add_argument("--observed", required=True)
    p_pass.add_argument("--caller-user-agent", required=True)
    p_pass.add_argument("--api-key-sha256", required=True)

    p_disc = sub.add_parser("discovery")
    p_disc.add_argument("--baseline", required=True)
    p_disc.add_argument("--observed", required=True)
    p_disc.add_argument("--min-records", type=int, default=1)

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
