#!/usr/bin/env python3
"""Smoke-check a live laya-serve against the wire contract the proxy relies on.

The proxy's Laya adapter (internal/api/classifier_laya.go) was checked
against laya-serve 0.3.20 on 2026-09-24. Any mismatch falls through to the
built-in classifier path silently, so "the proxy works" never proves "Laya
works". This script is the cheap proof: it sends
one typed-decision request built exactly like the proxy builds it — same
question name, same instructions, same criteria text the exporter bakes
into training examples — and verifies the response carries a known A-D
choice and a calibrated answer_confidence for the question asked.

Runbook: see docs/classifier-rules.md, "Checking a live laya-serve".

Usage:
    python3 scripts/check_laya.py --url http://127.0.0.1:8000/v1/systemone
    python3 scripts/check_laya.py --url ... --action "git push origin main"

Exit codes: 0 = the contract holds, 2 = check failed (see message).
"""
import argparse
import json
import sys
import urllib.error
import urllib.request
from dataclasses import dataclass

import corpus_to_laya

DEFAULT_ACTION = "ls -la"
QUESTION = "risk"
# config.DefaultLayaModel: the checkpoint the proxy pins when a backend sets
# no model. test_check_laya.py keeps the two in sync.
MODEL = "english"
VALID_LABELS = tuple(corpus_to_laya.CRITERIA.keys())


class CheckError(Exception):
    """The wire contract did not hold; the message names what broke."""


@dataclass
class Result:
    label: str
    answer_confidence: float


def build_payload(action):
    """Return the request body the proxy's buildLayaPayload sends, with the
    same instructions and criteria text as the exporter's training
    examples."""
    return {
        "model": MODEL,
        "state": {"action": action},
        "questions": {
            QUESTION: {
                "type": "choice",
                "instructions": corpus_to_laya.INSTRUCTIONS,
                "criteria": dict(corpus_to_laya.CRITERIA),
            }
        },
    }


def parse_answer(body):
    """Extract the typed choice for the question from a response body.

    Mirrors the proxy's parseLayaResponse: an unknown label or a missing
    question is a contract violation, not a guess.
    """
    try:
        answers = body["answers"]
    except (KeyError, TypeError):
        raise CheckError("response has no answers object")
    if QUESTION not in answers:
        raise CheckError(f"response carries no answer for question {QUESTION!r}")
    answer = answers[QUESTION]
    label = answer.get("choice")
    if label not in VALID_LABELS:
        raise CheckError(f"answer label {label!r} is not one of {VALID_LABELS}")
    # answer_confidence is what layaMinConfidence compares; "confidence" is
    # laya's entropy score on another scale. Under a floor the proxy
    # escalates every answer without a usable one, so a missing or mangled
    # value breaks the contract even though the label parsed.
    answer_confidence = answer.get("answer_confidence")
    if isinstance(answer_confidence, bool) or not isinstance(answer_confidence, (int, float)):
        raise CheckError(f"answer_confidence {answer_confidence!r} is not a number")
    if not 0 <= answer_confidence <= 1:
        raise CheckError(f"answer_confidence {answer_confidence} is outside [0, 1]")
    return Result(label=label, answer_confidence=float(answer_confidence))


def check(url, action, timeout):
    """Send one graded action and return the parsed Result.

    Raises CheckError on any contract violation or transport failure.
    """
    request = urllib.request.Request(
        url,
        data=json.dumps(build_payload(action)).encode(),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            body = response.read()
    except urllib.error.HTTPError as error:
        raise CheckError(f"server returned HTTP {error.code}")
    except urllib.error.URLError as error:
        raise CheckError(f"cannot connect to {url}: {error.reason}")
    try:
        decoded = json.loads(body)
    except json.JSONDecodeError:
        raise CheckError("response is not JSON")
    return parse_answer(decoded)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--url", required=True, help="laya-serve /v1/systemone URL")
    parser.add_argument("--action", default=DEFAULT_ACTION, help="graded action to send")
    parser.add_argument("--timeout", type=float, default=30, help="request timeout seconds")
    args = parser.parse_args(argv)

    try:
        result = check(args.url, args.action, args.timeout)
    except CheckError as error:
        print(f"FAIL: {error}", file=sys.stderr)
        return 2
    print(f"OK: label={result.label} answer_confidence={result.answer_confidence:.2f}")
    print("The wire contract holds: request shape accepted, typed A-D choice parsed.")
    print("Next: run the proxy-level check in docs/classifier-rules.md so a corpus")
    print('row with source "laya" proves the full reroute path.')
    return 0


if __name__ == "__main__":
    sys.exit(main())
