#!/usr/bin/env python3
"""Convert a classifier corpus into a Laya fine-tune dataset.

Reads the JSONL written by the proxy's classifier capture and emits one
example per labelled row, as a `choice` question over four risk buckets.

Rows with source == "upstream" carry a teacher label and are the only ones
kept by default. Rows produced by the local model itself (source == "laya")
are excluded on purpose: training on them would teach the model its own
answers and entrench its errors. Pass --source once per source to keep other
sources, for example gateway rows that a gateway model graded.

A row whose severity is -1 but whose verdict_raw carries an unclosed
<severity>NN tag (the teacher stopped at its token limit after the digits)
has the severity recovered at export time; a tag quoted inside <thinking>,
closed or cut off by truncation, is rationale, not verdict, and is never
recovered.

Only stage1-severity rows are kept by default. Stage 1 grades harm alone,
while Stage 2 applies user intent the exported state does not carry, so
mixing the two gives one action two different labels. Pass --kind once per
classifier kind to choose other kinds.

Usage:
    python3 scripts/corpus_to_laya.py ~/.config/antigravity-proxy/corpus/*.jsonl -o train.jsonl
"""
import argparse
import json
import re
import sys
from pathlib import Path

# Severity is an integer 0-100 with 50 as the allow/block boundary; see
# docs/classifier-fallback-notes.md. Bucket edges live here, not in Go, so
# they can be revised without recollecting the corpus.
BUCKETS = (
    ("A", 0, 9),
    ("B", 10, 24),
    ("C", 25, 49),
    ("D", 50, 100),
)

CRITERIA = {
    "A": "0-9: routine, allow without a second thought",
    "B": "10-24: low risk, ordinary local effect",
    "C": "25-49: elevated risk, review before allowing",
    "D": "50-100: high risk, the teacher refused this action",
}

INSTRUCTIONS = "Classify the risk of this shell action taken by a coding agent."

TRAINING_SOURCE = "upstream"

# The sources the proxy writes; see corpus.Source in Go.
KNOWN_SOURCES = ("upstream", "stub", "rule", "laya", "gateway")

DEFAULT_SOURCES = (TRAINING_SOURCE,)

# Mirror thinkingPattern, unclosedThinkingPattern and unclosedSeverityPattern
# in internal/classifier/corpus/verdict.go.
THINKING_PATTERN = re.compile(r"<thinking>.*?</thinking>", re.DOTALL)
UNCLOSED_THINKING_PATTERN = re.compile(r"<thinking>.*\Z", re.DOTALL)
UNCLOSED_SEVERITY_PATTERN = re.compile(r"<severity>\s*(-?\d+)")

# The kinds the proxy writes; see classifier.Kind.String() in Go.
KNOWN_KINDS = ("stage1-severity", "stage2-severity", "block-prefilter")

DEFAULT_KINDS = ("stage1-severity",)


def bucket_for(severity):
    """Return the bucket label for a 0-100 severity.

    A severity outside the buckets clamps to the most-risky label. A risk
    classifier must fail toward caution: returning "A" (read-only inspection)
    for an unexpected value would label it the safest thing in the set.
    """
    for label, low, high in BUCKETS:
        if low <= severity <= high:
            return label
    return BUCKETS[-1][0]


def recover_severity(row):
    """Return the severity from an unclosed <severity>NN tag, or -1.

    Only consulted when the row's severity is -1: rows captured before the
    proxy's parser learned the unclosed fallback left the label in
    verdict_raw. Thinking spans, including one a truncated answer never
    closed, are stripped first — a tag quoted in a rationale must not become
    the verdict.
    """
    answer = THINKING_PATTERN.sub("", row.get("verdict_raw") or "")
    answer = UNCLOSED_THINKING_PATTERN.sub("", answer)
    match = UNCLOSED_SEVERITY_PATTERN.search(answer)
    return int(match.group(1)) if match else -1


def to_example(row):
    """Return one Laya training example for a labelled row."""
    return {
        "state": {"action": row["action"]},
        "questions": {
            "risk": {
                "type": "choice",
                "instructions": INSTRUCTIONS,
                "criteria": dict(CRITERIA),
            }
        },
        "answers": {"risk": bucket_for(row["severity"])},
    }


def convert(rows, kinds=DEFAULT_KINDS, sources=DEFAULT_SOURCES):
    """Return (examples, stats). Filtering is explicit and counted.

    kinds is the set of classifier kinds to keep; sources the set of row
    sources to keep. stats["models"] lists the distinct teacher models behind
    the kept rows; stats["recovered"] counts rows whose severity came from an
    unclosed <severity>NN tag in verdict_raw.
    """
    stats = {
        "total": len(rows),
        "skipped_source": 0,
        "skipped_kind": 0,
        "skipped_unlabelled": 0,
        "skipped_no_action": 0,
        "system_hashes": 0,
        "recovered": 0,
        "models": [],
    }
    hashes = set()
    models = set()
    examples = []
    for row in rows:
        if row.get("source") not in sources:
            stats["skipped_source"] += 1
            continue
        if row.get("kind") not in kinds:
            stats["skipped_kind"] += 1
            continue
        severity = row.get("severity", -1)
        if severity < 0:
            severity = recover_severity(row)
            if severity >= 0:
                stats["recovered"] += 1
        if severity < 0:
            stats["skipped_unlabelled"] += 1
            continue
        if not row.get("action"):
            stats["skipped_no_action"] += 1
            continue
        if row.get("system_sha256"):
            hashes.add(row["system_sha256"])
        if row.get("model"):
            models.add(row["model"])
        examples.append(to_example({**row, "severity": severity}))
    stats["system_hashes"] = len(hashes)
    stats["models"] = sorted(models)
    return examples, stats


def load_rows(path):
    """Read one JSONL file, returning (rows, skipped).

    A blank line is not corpus loss and is not counted. An unparseable line is
    a row that went missing — most often a truncated final line, because the
    proxy appends to the corpus while an export runs — so it is counted and
    reported rather than discarded in silence. Neither aborts the file.
    """
    rows = []
    skipped = 0
    with open(path, "r", encoding="utf-8") as handle:
        for line in handle:
            line = line.strip()
            if not line:
                continue
            try:
                rows.append(json.loads(line))
            except ValueError:
                skipped += 1
                continue
    return rows, skipped


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("inputs", nargs="+", type=Path, help="corpus JSONL files")
    parser.add_argument("-o", "--output", type=Path, required=True, help="dataset JSONL to write")
    parser.add_argument(
        "--kind",
        action="append",
        choices=KNOWN_KINDS,
        help="classifier kind to keep; repeat to keep several (default: stage1-severity only)",
    )
    parser.add_argument(
        "--source",
        action="append",
        choices=KNOWN_SOURCES,
        help="row source to keep; repeat to keep several (default: upstream only). "
        "Keeping laya trains the model on its own answers.",
    )
    args = parser.parse_args(argv)
    kinds = tuple(args.kind) if args.kind else DEFAULT_KINDS
    sources = tuple(args.source) if args.source else DEFAULT_SOURCES

    rows = []
    unparseable = 0
    for path in args.inputs:
        file_rows, file_skipped = load_rows(path)
        rows.extend(file_rows)
        unparseable += file_skipped

    examples, stats = convert(rows, kinds, sources)

    with open(args.output, "w", encoding="utf-8") as handle:
        for example in examples:
            handle.write(json.dumps(example, ensure_ascii=False) + "\n")

    print(
        f"read {stats['total']} rows, wrote {len(examples)} examples to {args.output} "
        f"(kinds: {', '.join(kinds)}; sources: {', '.join(sources)})"
    )
    print(
        f"skipped: {stats['skipped_source']} of another source, "
        f"{stats['skipped_kind']} of another kind, "
        f"{stats['skipped_unlabelled']} unlabelled, "
        f"{stats['skipped_no_action']} without an action, "
        f"{unparseable} unparseable line{'' if unparseable == 1 else 's'}"
    )
    if stats["recovered"]:
        print(
            f"recovered {stats['recovered']} severities from unclosed <severity> tags"
        )
    if stats["system_hashes"] > 1:
        print(
            f"WARNING: {stats['system_hashes']} distinct monitor prompts in this corpus. "
            "The teacher changed mid-collection; these rows are not one dataset.",
            file=sys.stderr,
        )
    if len(stats["models"]) > 1:
        print(
            f"WARNING: {len(stats['models'])} distinct models graded these rows "
            f"({', '.join(stats['models'])}). Each is a different teacher; "
            "these rows are not one dataset.",
            file=sys.stderr,
        )
    if not examples:
        if stats["skipped_kind"]:
            hint = (
                f"{stats['skipped_kind']} rows from the selected sources were of "
                f"another kind than {', '.join(kinds)}; pass --kind to include them."
            )
        else:
            hint = (
                "Capture only yields labels while classifier requests reach upstream "
                "— check that rules were set to passthrough."
            )
        print(f"WARNING: no labelled rows. {hint}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
