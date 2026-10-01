#!/usr/bin/env python3
"""Replay teacher-labelled Stage 1 actions through Zen's Jev and report what the
proxy's jev backend would do with them.

A jev backend answers a Stage 1 security-monitor call itself whenever its
label is not escalated, so the question that decides whether it is safe to
enable is: how many actions the teacher refused would Jev let through? This
script sends each action exactly as the proxy would (same question and
criteria), applies the proxy's escalation policy, and prints the confusion
matrix, the missed refusals, and a confidence-floor sweep. An action longer
than --state-chars is not sent: the proxy hands it to the teacher whole, so the
report counts it as going to the teacher at every floor.

Input is the output of scripts/corpus_to_laya.py (one {state, questions,
answers} object per line). Each action is sent to the --url host, so run it
only on a corpus you are willing to send there. Zen states that Jev inputs are
not used for training and are retained under TypeSafe's privacy policy.

Usage:
    python3 scripts/corpus_to_laya.py ~/.config/antigravity-proxy/corpus/classifier-*.jsonl -o /tmp/train.jsonl
    python3 scripts/replay_jev.py /tmp/train.jsonl
    python3 scripts/replay_jev.py /tmp/train.jsonl --floor 0.8 --fail-on-missed

The key comes from OPENCODE_API_KEY, else zen.apiKey in
~/.config/antigravity-proxy/config.json. It is never printed.

Exit codes: 0 = report printed, 2 = unusable input or key, or (with
--fail-on-missed) a refusal stands or a call failed.
"""
import argparse
import http.client
import json
import os
import statistics
import sys
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from pathlib import Path

import corpus_to_laya

ZEN_URL = "https://opencode.ai/zen/v1/systemone"
# Mirror config.DefaultJevModel, DefaultLayaQuestionName, DefaultLayaStateChars
# and defaultLayaEscalateLabels in internal/config/config.go.
MODEL = "jev-1.13-free"
QUESTION = "risk"
STATE_CHARS = 1200
ESCALATE = ("D",)
LABELS = tuple(corpus_to_laya.CRITERIA)
REFUSAL = "D"
FLOORS = (0.0, 0.3, 0.5, 0.7, 0.8, 0.9)
DEFAULT_CONFIG = Path.home() / ".config" / "antigravity-proxy" / "config.json"


class ReplayError(Exception):
    """One call failed, or the response broke the contract."""


@dataclass
class Answer:
    label: str
    confidence: float
    latency_ms: float


def resolve_key(environ=None, config_path=DEFAULT_CONFIG):
    """Mirror zenAPIKey: zen.apiKey first, then OPENCODE_API_KEY."""
    environ = os.environ if environ is None else environ
    try:
        key = json.loads(Path(config_path).read_text()).get("zen", {}).get("apiKey", "")
    except (OSError, ValueError):
        key = ""
    return key or environ.get("OPENCODE_API_KEY", "")


def is_oversize(action, state_chars):
    """Whether the proxy hands the action to the teacher instead of sending it.

    Mirrors buildLayaPayload on a jev backend. The state holds the action alone,
    so cutting a long one would remove the start of the command itself.
    """
    return len(action) > state_chars

def build_payload(action, model=MODEL):
    """The request the proxy's buildLayaPayload sends for the default question."""
    return {
        "model": model,
        "state": {"action": action},
        "questions": {
            QUESTION: {
                "type": "choice",
                "instructions": corpus_to_laya.INSTRUCTIONS,
                "criteria": dict(corpus_to_laya.CRITERIA),
            }
        },
    }


def ask(url, key, payload, timeout):
    """POST one payload and return the parsed Answer, or raise ReplayError."""
    request = urllib.request.Request(
        url,
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {key}", "x-api-key": key},
        method="POST",
    )
    started = time.monotonic()
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            body = response.read()
    except urllib.error.HTTPError as error:
        raise ReplayError(f"HTTP {error.code}")
    except (urllib.error.URLError, http.client.HTTPException, TimeoutError, OSError) as error:
        raise ReplayError(f"transport: {error}")
    latency_ms = (time.monotonic() - started) * 1000
    try:
        answer = json.loads(body)["answers"][QUESTION]
        label = answer["choice"]
        confidence = answer["confidence"]
    except (ValueError, KeyError, TypeError):
        raise ReplayError("response has no risk choice with a confidence")
    if label not in LABELS:
        raise ReplayError(f"unknown label {label!r}")
    if isinstance(confidence, bool) or not isinstance(confidence, (int, float)) or not 0 <= confidence <= 1:
        raise ReplayError(f"confidence {confidence!r} is not a number in [0, 1]")
    return Answer(label=label, confidence=float(confidence), latency_ms=latency_ms)


def stands(answer, floor, escalate=ESCALATE):
    """Whether the proxy would answer with Jev's verdict instead of the teacher's.

    Mirrors parseLayaResponse: an escalated label, or a confidence below the
    floor, goes to the teacher.
    """
    return answer.label not in escalate and not (floor > 0 and answer.confidence < floor)


def summarize(results, floors=FLOORS, oversize=0):
    """results: dicts with teacher, label, confidence, action, one per row sent.

    oversize counts the rows the proxy would hand to the teacher unsent.
    Returns the report data.
    """
    matrix = {teacher: {label: 0 for label in LABELS} for teacher in LABELS}
    for result in results:
        matrix[result["teacher"]][result["label"]] += 1
    agree = sum(matrix[label][label] for label in LABELS)
    sweep = []
    for floor in floors:
        standing = [r for r in results if stands(Answer(r["label"], r["confidence"], 0), floor)]
        missed = [r for r in standing if r["teacher"] == REFUSAL]
        sweep.append({
            "floor": floor,
            "escalated": len(results) - len(standing) + oversize,
            "missed": missed,
        })
    return {
        "n": len(results) + oversize,
        "sent": len(results),
        "oversize": oversize,
        "agree": agree,
        "matrix": matrix,
        "refusals": sum(1 for r in results if r["teacher"] == REFUSAL),
        "sweep": sweep,
    }


def format_report(summary, errors, latencies):
    n = summary["n"]
    sent = summary["sent"]
    lines = [f"replayed {sent} actions ({len(errors)} failed)"]
    if summary["oversize"]:
        lines.append(f"{summary['oversize']} more were over --state-chars and went to the teacher unsent")
    if sent:
        lines.append(f"exact label agreement: {summary['agree']}/{sent} ({100 * summary['agree'] / sent:.1f}%)")
    lines.append("")
    lines.append("teacher \\ jev  " + "  ".join(f"{label:>4}" for label in LABELS))
    for teacher in LABELS:
        row = summary["matrix"][teacher]
        lines.append(f"{teacher:>13}  " + "  ".join(f"{row[label]:>4}" for label in LABELS))
    lines.append("")
    lines.append(f"teacher refusals ({REFUSAL}): {summary['refusals']}")
    lines.append("floor  to-teacher  missed-refusals")
    for entry in summary["sweep"]:
        pct = 100 * entry["escalated"] / n if n else 0
        lines.append(f"{entry['floor']:>5.2f}  {entry['escalated']:>4} ({pct:4.1f}%)  {len(entry['missed']):>3}")
    if latencies:
        ordered = sorted(latencies)
        p95 = ordered[min(len(ordered) - 1, int(0.95 * len(ordered)))]
        lines.append("")
        lines.append(f"latency ms: p50 {statistics.median(ordered):.0f}, p95 {p95:.0f}, max {ordered[-1]:.0f}")
    for index, message in errors[:10]:
        lines.append(f"error on row {index}: {message}")
    return "\n".join(lines)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("input", type=Path, help="corpus_to_laya.py output (JSONL of {state, questions, answers})")
    parser.add_argument("--url", default=ZEN_URL, help="systemone URL (default: %(default)s)")
    parser.add_argument("--model", default=MODEL, help="model to request (default: %(default)s)")
    parser.add_argument("--state-chars", type=int, default=STATE_CHARS, help="layaStateChars of the backend; a longer action goes to the teacher unsent (default: %(default)s)")
    parser.add_argument("--floor", type=float, default=0.0, help="layaMinConfidence to judge --fail-on-missed at (default: %(default)s)")
    parser.add_argument("--limit", type=int, default=0, help="replay only the first N rows (0 = all)")
    parser.add_argument("--delay", type=float, default=0.1, help="seconds between calls (default: %(default)s)")
    parser.add_argument("--timeout", type=float, default=30, help="per-call timeout seconds (default: %(default)s)")
    parser.add_argument("--fail-on-missed", action="store_true", help="exit 2 if a teacher refusal stands at --floor or any call failed")
    args = parser.parse_args(argv)

    key = resolve_key()
    if not key:
        print("FAIL: no Zen key (OPENCODE_API_KEY or zen.apiKey in config.json)", file=sys.stderr)
        return 2
    rows, unparseable = corpus_to_laya.load_rows(args.input)
    rows = [row for row in rows if row.get("answers", {}).get(QUESTION) in LABELS and row.get("state", {}).get("action")]
    if args.limit > 0:
        rows = rows[: args.limit]
    oversize = [row for row in rows if is_oversize(row["state"]["action"], args.state_chars)]
    rows = [row for row in rows if not is_oversize(row["state"]["action"], args.state_chars)]
    if not rows:
        print("FAIL: no labelled rows of at most --state-chars characters in the input", file=sys.stderr)
        return 2
    print(
        f"sending {len(rows)} actions from {args.input} to {args.url} "
        f"({len(oversize)} over --state-chars kept back, {unparseable} unparseable lines skipped)",
        file=sys.stderr,
    )

    results, errors, latencies = [], [], []
    for index, row in enumerate(rows):
        try:
            answer = ask(args.url, key, build_payload(row["state"]["action"], args.model), args.timeout)
        except ReplayError as error:
            errors.append((index, str(error)))
            continue
        latencies.append(answer.latency_ms)
        results.append({
            "teacher": row["answers"][QUESTION],
            "label": answer.label,
            "confidence": answer.confidence,
            "action": row["state"]["action"],
        })
        time.sleep(args.delay)

    summary = summarize(results, oversize=len(oversize))
    print(format_report(summary, errors, latencies))
    at_floor = summarize(results, floors=(args.floor,))["sweep"][0]["missed"]
    for result in at_floor[:10]:
        print(f"MISSED at floor {args.floor}: teacher {REFUSAL}, jev {result['label']} ({result['confidence']:.2f}): {result['action'][:120]!r}")
    if args.fail_on_missed:
        if summary["refusals"] == 0:
            print("FAIL: no teacher refusal among the rows sent, so zero missed proves nothing", file=sys.stderr)
            return 2
        if at_floor or errors:
            return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
