"""Tests for corpus_to_laya. Run: python3 -m pytest scripts/test_corpus_to_laya.py"""
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))

from corpus_to_laya import bucket_for, convert, load_rows, main, to_example

STAGE1 = "stage1-severity"


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
        {"action": "a", "severity": 1, "source": "upstream", "kind": STAGE1},
        {"action": "b", "severity": 2, "source": "laya", "kind": STAGE1},
        {"action": "c", "severity": 3, "source": "stub", "kind": STAGE1},
        {"action": "d", "severity": 4, "source": "rule", "kind": STAGE1},
        {"action": "e", "severity": 5, "source": "gateway", "kind": STAGE1},
    ]
    examples, stats = convert(rows)
    assert len(examples) == 1
    assert examples[0]["state"]["action"] == "a"
    assert stats["skipped_source"] == 4


def test_convert_skips_unlabelled_rows():
    rows = [
        {"action": "a", "severity": -1, "source": "upstream", "kind": STAGE1},
        {"action": "b", "severity": 5, "source": "upstream", "kind": STAGE1},
    ]
    examples, stats = convert(rows)
    assert len(examples) == 1
    assert stats["skipped_unlabelled"] == 1


def test_convert_skips_rows_without_an_action():
    rows = [{"action": "", "severity": 5, "source": "upstream", "kind": STAGE1}]
    examples, stats = convert(rows)
    assert examples == []
    assert stats["skipped_no_action"] == 1


def test_convert_counts_distinct_system_hashes():
    rows = [
        {"action": "a", "severity": 1, "source": "upstream", "kind": STAGE1, "system_sha256": "aa"},
        {"action": "b", "severity": 2, "source": "upstream", "kind": STAGE1, "system_sha256": "bb"},
    ]
    _, stats = convert(rows)
    assert stats["system_hashes"] == 2


def test_convert_keeps_only_stage1_rows_by_default():
    # Stage 2 applies user intent the exported state does not carry, so its
    # label for the same action can differ from Stage 1's.
    rows = [
        {"action": "a", "severity": 1, "source": "upstream", "kind": STAGE1},
        {"action": "b", "severity": 30, "source": "upstream", "kind": "stage2-severity"},
        {"action": "c", "severity": 0, "source": "upstream", "kind": "block-prefilter"},
        {"action": "d", "severity": 2, "source": "upstream"},
    ]
    examples, stats = convert(rows)
    assert [example["state"]["action"] for example in examples] == ["a"]
    assert stats["skipped_kind"] == 3


def test_convert_keeps_the_kinds_asked_for():
    rows = [
        {"action": "a", "severity": 1, "source": "upstream", "kind": STAGE1},
        {"action": "b", "severity": 30, "source": "upstream", "kind": "stage2-severity"},
    ]
    examples, stats = convert(rows, kinds=(STAGE1, "stage2-severity"))
    assert [example["state"]["action"] for example in examples] == ["a", "b"]
    assert stats["skipped_kind"] == 0


def test_convert_lists_the_models_of_kept_rows_only():
    rows = [
        {"action": "a", "severity": 1, "source": "upstream", "kind": STAGE1, "model": "claude-opus-5-5"},
        {"action": "b", "severity": 2, "source": "upstream", "kind": STAGE1, "model": "claude-sonnet-5"},
        # Filtered rows must not count toward the teacher mix.
        {"action": "c", "severity": 3, "source": "laya", "kind": STAGE1, "model": "english"},
    ]
    _, stats = convert(rows)
    assert stats["models"] == ["claude-opus-5-5", "claude-sonnet-5"]


def test_convert_recovers_an_unclosed_severity_tag():
    # A teacher that stops at its token limit leaves `<severity>10` with no
    # closing tag; the proxy records severity -1 and the label sits in
    # verdict_raw.
    rows = [
        {"action": "a", "severity": -1, "verdict_raw": "<severity>10", "source": "upstream", "kind": STAGE1},
    ]
    examples, stats = convert(rows)
    assert len(examples) == 1
    assert examples[0]["answers"]["risk"] == "B"
    assert stats["recovered"] == 1
    assert stats["skipped_unlabelled"] == 0


def test_convert_does_not_recover_a_tag_quoted_in_thinking():
    rows = [
        {
            "action": "a",
            "severity": -1,
            "verdict_raw": "<thinking>A force push would be <severity>90</thinking>",
            "source": "upstream",
            "kind": STAGE1,
        },
    ]
    examples, stats = convert(rows)
    assert examples == []
    assert stats["recovered"] == 0
    assert stats["skipped_unlabelled"] == 1


def test_convert_does_not_recover_a_tag_in_truncated_thinking():
    # The teacher was cut off inside its rationale: no verdict was given.
    rows = [
        {
            "action": "a",
            "severity": -1,
            "verdict_raw": "<thinking>A force push would be <severity>90",
            "source": "upstream",
            "kind": STAGE1,
        },
    ]
    examples, stats = convert(rows)
    assert examples == []
    assert stats["recovered"] == 0


def test_convert_keeps_the_sources_asked_for():
    rows = [
        {"action": "a", "severity": 1, "source": "upstream", "kind": STAGE1},
        {"action": "b", "severity": 2, "source": "gateway", "kind": STAGE1},
    ]
    examples, stats = convert(rows, sources=("upstream", "gateway"))
    assert [example["state"]["action"] for example in examples] == ["a", "b"]
    assert stats["skipped_source"] == 0


def _write_corpus(path, rows):
    path.write_text("".join(json.dumps(row) + "\n" for row in rows))


def test_main_warns_when_kept_rows_mix_models(tmp_path, capsys):
    corpus = tmp_path / "classifier-2026-09-23.jsonl"
    _write_corpus(corpus, [
        {"action": "a", "severity": 1, "source": "upstream", "kind": STAGE1, "model": "claude-opus-5-5"},
        {"action": "b", "severity": 2, "source": "upstream", "kind": STAGE1, "model": "claude-sonnet-5"},
    ])
    assert main([str(corpus), "-o", str(tmp_path / "train.jsonl")]) == 0
    err = capsys.readouterr().err
    assert "2 distinct models" in err
    assert "claude-opus-5-5" in err and "claude-sonnet-5" in err


def test_main_is_quiet_about_models_when_one_teacher_graded(tmp_path, capsys):
    corpus = tmp_path / "classifier-2026-09-23.jsonl"
    _write_corpus(corpus, [
        {"action": "a", "severity": 1, "source": "upstream", "kind": STAGE1, "model": "claude-opus-5-5"},
        {"action": "b", "severity": 2, "source": "upstream", "kind": STAGE1, "model": "claude-opus-5-5"},
        # A second model on a dropped row is not a mix in the output.
        {"action": "c", "severity": 2, "source": "upstream", "kind": "stage2-severity", "model": "claude-sonnet-5"},
    ])
    assert main([str(corpus), "-o", str(tmp_path / "train.jsonl")]) == 0
    assert "distinct models" not in capsys.readouterr().err


def test_main_kind_flag_is_repeatable(tmp_path):
    corpus = tmp_path / "classifier-2026-09-23.jsonl"
    output = tmp_path / "train.jsonl"
    _write_corpus(corpus, [
        {"action": "a", "severity": 1, "source": "upstream", "kind": STAGE1},
        {"action": "b", "severity": 30, "source": "upstream", "kind": "stage2-severity"},
        {"action": "c", "severity": 0, "source": "upstream", "kind": "block-prefilter"},
    ])
    assert main([
        str(corpus), "-o", str(output), "--kind", STAGE1, "--kind", "stage2-severity",
    ]) == 0
    actions = [json.loads(line)["state"]["action"] for line in output.read_text().splitlines()]
    assert actions == ["a", "b"]


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


def test_main_source_flag_is_repeatable(tmp_path):
    corpus = tmp_path / "classifier-2026-09-24.jsonl"
    output = tmp_path / "train.jsonl"
    _write_corpus(corpus, [
        {"action": "a", "severity": 1, "source": "upstream", "kind": STAGE1},
        {"action": "b", "severity": -1, "verdict_raw": "<severity>10", "source": "gateway", "kind": STAGE1},
        {"action": "c", "severity": 3, "source": "stub", "kind": STAGE1},
    ])
    assert main([
        str(corpus), "-o", str(output), "--source", "upstream", "--source", "gateway",
    ]) == 0
    actions = [json.loads(line)["state"]["action"] for line in output.read_text().splitlines()]
    assert actions == ["a", "b"]


def test_load_rows_does_not_count_blank_lines_as_loss(tmp_path):
    path = tmp_path / "classifier-2026-09-23.jsonl"
    path.write_text(
        "\n\n" + json.dumps({"action": "a", "severity": 1, "source": "upstream"}) + "\n\n\n"
    )
    rows, skipped = load_rows(path)
    assert len(rows) == 1
    assert skipped == 0


def test_main_no_rows_hint_names_the_kind_filter(tmp_path, capsys):
    corpus = tmp_path / "classifier-2026-09-23.jsonl"
    _write_corpus(corpus, [
        {"action": "a", "severity": 12, "source": "upstream", "kind": "stage2-severity"},
    ])
    assert main([str(corpus), "-o", str(tmp_path / "train.jsonl")]) == 0
    err = capsys.readouterr().err
    assert "no labelled rows" in err
    assert "--kind" in err
    assert "passthrough" not in err


def test_main_no_rows_hint_names_passthrough_when_nothing_went_upstream(tmp_path, capsys):
    corpus = tmp_path / "classifier-2026-09-23.jsonl"
    _write_corpus(corpus, [
        {"action": "a", "severity": 0, "source": "stub", "kind": STAGE1},
    ])
    assert main([str(corpus), "-o", str(tmp_path / "train.jsonl")]) == 0
    err = capsys.readouterr().err
    assert "no labelled rows" in err
    assert "passthrough" in err
    assert "--kind" not in err
