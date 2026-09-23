#!/usr/bin/env python3
"""Convert a classifier corpus into a Laya fine-tune dataset.

Reads the JSONL written by the proxy's classifier capture and emits one
example per labelled row, as a `choice` question over four risk buckets.

Only rows with source == "upstream" carry a teacher label. Rows produced by
the local model itself (source == "laya") are excluded on purpose: training on
them would teach the model its own answers and entrench its errors.

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
    "A": "read-only inspection: listing, reading, searching, status, diff",
    "B": "local write: edits files, installs dependencies, runs build or tests",
    "C": "outbound or shared effect: network, push, publish, deploy, send",
    "D": "destructive or credential-touching: deletes data, rewrites history, reads secrets",
}

INSTRUCTIONS = "Classify the risk of this shell action taken by a coding agent."

TRAINING_SOURCE = "upstream"


def bucket_for(severity):
    """Return the bucket label for a 0-100 severity."""
    for label, low, high in BUCKETS:
        if low <= severity <= high:
            return label
    return BUCKETS[-1][0] if severity > 100 else BUCKETS[0][0]


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


def convert(rows):
    """Return (examples, stats). Filtering is explicit and counted."""
    stats = {
        "total": len(rows),
        "skipped_source": 0,
        "skipped_unlabelled": 0,
        "skipped_no_action": 0,
        "system_hashes": 0,
    }
    hashes = set()
    examples = []
    for row in rows:
        if row.get("source") != TRAINING_SOURCE:
            stats["skipped_source"] += 1
            continue
        if row.get("severity", -1) < 0:
            stats["skipped_unlabelled"] += 1
            continue
        if not row.get("action"):
            stats["skipped_no_action"] += 1
            continue
        if row.get("system_sha256"):
            hashes.add(row["system_sha256"])
        examples.append(to_example(row))
    stats["system_hashes"] = len(hashes)
    return examples, stats


def load_rows(path):
    """Read one JSONL file, skipping blank and unparseable lines."""
    rows = []
    with open(path, "r", encoding="utf-8") as handle:
        for line in handle:
            line = line.strip()
            if not line:
                continue
            try:
                rows.append(json.loads(line))
            except ValueError:
                continue
    return rows


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("inputs", nargs="+", type=Path, help="corpus JSONL files")
    parser.add_argument("-o", "--output", type=Path, required=True, help="dataset JSONL to write")
    args = parser.parse_args(argv)

    rows = []
    for path in args.inputs:
        rows.extend(load_rows(path))

    examples, stats = convert(rows)

    with open(args.output, "w", encoding="utf-8") as handle:
        for example in examples:
            handle.write(json.dumps(example, ensure_ascii=False) + "\n")

    print(f"read {stats['total']} rows, wrote {len(examples)} examples to {args.output}")
    print(
        f"skipped: {stats['skipped_source']} non-upstream, "
        f"{stats['skipped_unlabelled']} unlabelled, "
        f"{stats['skipped_no_action']} without an action"
    )
    if stats["system_hashes"] > 1:
        print(
            f"WARNING: {stats['system_hashes']} distinct monitor prompts in this corpus. "
            "The teacher changed mid-collection; these rows are not one dataset.",
            file=sys.stderr,
        )
    if not examples:
        print(
            "WARNING: no labelled rows. Capture only yields labels while classifier "
            "requests reach upstream — check that rules were set to passthrough.",
            file=sys.stderr,
        )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
