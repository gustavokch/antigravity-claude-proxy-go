#!/usr/bin/env python3
"""Convert a classifier corpus into a Laya fine-tune dataset.

Reads the JSONL written by the proxy's classifier capture and emits one
example per labelled row, as a `choice` question over four risk buckets.

Only rows with source == "upstream" carry a teacher label. Rows produced by
the local model itself (source == "laya") are excluded on purpose: training on
them would teach the model its own answers and entrench its errors.

Only stage1-severity rows are kept by default. Stage 1 grades harm alone,
while Stage 2 applies user intent the exported state does not carry, so
mixing the two gives one action two different labels. Pass --kind once per
classifier kind to choose other kinds.

Usage:
    python3 scripts/corpus_to_laya.py ~/.config/antigravity-proxy/corpus/*.jsonl -o train.jsonl
"""
import argparse
import json
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


def convert(rows, kinds=DEFAULT_KINDS):
    """Return (examples, stats). Filtering is explicit and counted.

    kinds is the set of classifier kinds to keep. stats["models"] lists the
    distinct teacher models behind the kept rows.
    """
    stats = {
        "total": len(rows),
        "skipped_source": 0,
        "skipped_kind": 0,
        "skipped_unlabelled": 0,
        "skipped_no_action": 0,
        "system_hashes": 0,
        "models": [],
    }
    hashes = set()
    models = set()
    examples = []
    for row in rows:
        if row.get("source") != TRAINING_SOURCE:
            stats["skipped_source"] += 1
            continue
        if row.get("kind") not in kinds:
            stats["skipped_kind"] += 1
            continue
        if row.get("severity", -1) < 0:
            stats["skipped_unlabelled"] += 1
            continue
        if not row.get("action"):
            stats["skipped_no_action"] += 1
            continue
        if row.get("system_sha256"):
            hashes.add(row["system_sha256"])
        if row.get("model"):
            models.add(row["model"])
        examples.append(to_example(row))
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
    args = parser.parse_args(argv)
    kinds = tuple(args.kind) if args.kind else DEFAULT_KINDS

    rows = []
    unparseable = 0
    for path in args.inputs:
        file_rows, file_skipped = load_rows(path)
        rows.extend(file_rows)
        unparseable += file_skipped

    examples, stats = convert(rows, kinds)

    with open(args.output, "w", encoding="utf-8") as handle:
        for example in examples:
            handle.write(json.dumps(example, ensure_ascii=False) + "\n")

    print(
        f"read {stats['total']} rows, wrote {len(examples)} examples to {args.output} "
        f"(kinds: {', '.join(kinds)})"
    )
    print(
        f"skipped: {stats['skipped_source']} non-upstream, "
        f"{stats['skipped_kind']} of another kind, "
        f"{stats['skipped_unlabelled']} unlabelled, "
        f"{stats['skipped_no_action']} without an action, "
        f"{unparseable} unparseable line{'' if unparseable == 1 else 's'}"
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
                f"{stats['skipped_kind']} upstream rows were of another kind than "
                f"{', '.join(kinds)}; pass --kind to include them."
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
