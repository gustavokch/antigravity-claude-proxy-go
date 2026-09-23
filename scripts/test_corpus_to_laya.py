"""Tests for corpus_to_laya. Run: python3 -m pytest scripts/test_corpus_to_laya.py"""
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))

from corpus_to_laya import bucket_for, convert, load_rows, to_example


def test_bucket_boundaries():
    assert bucket_for(0) == "A"
    assert bucket_for(9) == "A"
    assert bucket_for(10) == "B"
    assert bucket_for(24) == "B"
    assert bucket_for(25) == "C"
    assert bucket_for(49) == "C"
    assert bucket_for(50) == "D"
    assert bucket_for(100) == "D"


def test_bucket_for_clamps_unmatched_to_d():
    # An unmatched severity must fail toward the most-risky label, never "A".
    assert bucket_for(-5) == "D"
    assert bucket_for(101) == "D"


def test_to_example_shape():
    row = {
        "v": 1,
        "action": '{"Bash":"rm -rf build/"}',
        "severity": 8,
        "source": "upstream",
    }
    example = to_example(row)
    assert example["state"] == {"action": '{"Bash":"rm -rf build/"}'}
    assert example["answers"]["risk"] == "A"
    assert example["questions"]["risk"]["type"] == "choice"
    assert set(example["questions"]["risk"]["criteria"]) == {"A", "B", "C", "D"}


def test_convert_keeps_only_upstream_rows():
    rows = [
        {"action": "a", "severity": 1, "source": "upstream"},
        {"action": "b", "severity": 2, "source": "laya"},
        {"action": "c", "severity": 3, "source": "stub"},
        {"action": "d", "severity": 4, "source": "rule"},
    ]
    examples, stats = convert(rows)
    assert len(examples) == 1
    assert examples[0]["state"]["action"] == "a"
    assert stats["skipped_source"] == 3


def test_convert_skips_unlabelled_rows():
    rows = [
        {"action": "a", "severity": -1, "source": "upstream"},
        {"action": "b", "severity": 5, "source": "upstream"},
    ]
    examples, stats = convert(rows)
    assert len(examples) == 1
    assert stats["skipped_unlabelled"] == 1


def test_convert_skips_rows_without_an_action():
    rows = [{"action": "", "severity": 5, "source": "upstream"}]
    examples, stats = convert(rows)
    assert examples == []
    assert stats["skipped_no_action"] == 1


def test_convert_counts_distinct_system_hashes():
    rows = [
        {"action": "a", "severity": 1, "source": "upstream", "system_sha256": "aa"},
        {"action": "b", "severity": 2, "source": "upstream", "system_sha256": "bb"},
    ]
    _, stats = convert(rows)
    assert stats["system_hashes"] == 2


def test_load_rows_skips_blank_and_broken_lines(tmp_path):
    path = tmp_path / "classifier-2026-09-23.jsonl"
    path.write_text(
        json.dumps({"action": "a", "severity": 1, "source": "upstream"})
        + "\n\nnot json\n"
        + json.dumps({"action": "b", "severity": 2, "source": "upstream"})
        + "\n"
    )
    rows, skipped = load_rows(path)
    assert len(rows) == 2
    # The blank line is not corpus loss; the unparseable line is, so only it counts.
    assert skipped == 1


def test_load_rows_does_not_count_blank_lines_as_loss(tmp_path):
    path = tmp_path / "classifier-2026-09-23.jsonl"
    path.write_text(
        "\n\n" + json.dumps({"action": "a", "severity": 1, "source": "upstream"}) + "\n\n\n"
    )
    rows, skipped = load_rows(path)
    assert len(rows) == 1
    assert skipped == 0
