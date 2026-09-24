"""Tests for finetune_laya. Run: python3 -m pytest scripts/test_finetune_laya.py

Only the pure-python seams are tested: torch/laya/transformers are imported
lazily inside stages, and this environment has none of them.
"""
import json
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).parent))

import finetune_laya
from finetune_laya import (
    corpus_sha256,
    load_state,
    main,
    one_hot_target,
    save_state,
    stage_export,
)


def test_import_pulls_in_no_heavy_dependencies():
    assert "torch" not in sys.modules
    assert "laya" not in sys.modules


def test_one_hot_target_marks_the_label():
    assert one_hot_target("B", ["A", "B", "C", "D"]) == [0.0, 1.0, 0.0, 0.0]


def test_one_hot_target_rejects_an_unknown_label():
    with pytest.raises(ValueError, match="Z"):
        one_hot_target("Z", ["A", "B", "C", "D"])


def test_corpus_sha256_tracks_content(tmp_path):
    path = tmp_path / "c.jsonl"
    path.write_text('{"a": 1}\n')
    before = corpus_sha256([path])
    path.write_text('{"a": 2}\n')
    assert corpus_sha256([path]) != before


def test_state_roundtrip(tmp_path):
    save_state(tmp_path, {"corpus_sha256": "abc", "config": {"epochs": 2}})
    assert load_state(tmp_path)["corpus_sha256"] == "abc"


def test_load_state_defaults_when_missing(tmp_path):
    assert load_state(tmp_path) == {}


def _args(corpus, tmp_path, **overrides):
    defaults = dict(
        corpus=[corpus],
        out=tmp_path,
        epochs=2,
        max_items=0,
        seed=42,
        base_model="convaiinnovations/laya",
        device="auto",
        kind=None,
        source=None,
        fresh=False,
        dry_run=True,
    )
    defaults.update(overrides)
    return type("Args", (), defaults)()


def test_dry_run_exports_and_prints_the_plan(tmp_path, capsys):
    corpus = tmp_path / "classifier-2026-09-24.jsonl"
    corpus.write_text(
        json.dumps({"action": "a", "severity": 1, "source": "upstream", "kind": "stage1-severity"}) + "\n"
    )
    assert main([str(corpus), "--out", str(tmp_path), "--dry-run"]) == 0
    out = capsys.readouterr().out
    assert "dry-run" in out
    dataset = (tmp_path / "dataset.jsonl").read_text().splitlines()
    assert len(dataset) == 1
    assert json.loads(dataset[0])["answers"]["risk"] == "A"


def test_dry_run_recovers_unclosed_tags_through_the_gateway_source(tmp_path, capsys):
    corpus = tmp_path / "classifier-2026-09-24.jsonl"
    corpus.write_text(
        json.dumps(
            {
                "action": "b",
                "severity": -1,
                "verdict_raw": "<severity>10",
                "source": "gateway",
                "kind": "stage1-severity",
            }
        )
        + "\n"
    )
    assert main([str(corpus), "--out", str(tmp_path), "--source", "gateway", "--dry-run"]) == 0
    assert "1 recovered" in capsys.readouterr().out
    example = json.loads((tmp_path / "dataset.jsonl").read_text().splitlines()[0])
    assert example["answers"]["risk"] == "B"


def test_dry_run_fails_loud_when_no_rows_survive(tmp_path):
    corpus = tmp_path / "classifier-2026-09-24.jsonl"
    corpus.write_text(json.dumps({"action": "a", "severity": 0, "source": "stub", "kind": "stage1-severity"}) + "\n")
    with pytest.raises(SystemExit, match="no labelled rows"):
        main([str(corpus), "--out", str(tmp_path), "--dry-run"])


def test_export_skips_when_corpus_is_unchanged(tmp_path, capsys):
    corpus = tmp_path / "c.jsonl"
    corpus.write_text(
        json.dumps({"action": "a", "severity": 1, "source": "upstream", "kind": "stage1-severity"}) + "\n"
    )
    args = _args(corpus, tmp_path)
    digest = corpus_sha256([corpus])
    stage_export(args, tmp_path, {}, digest)
    capsys.readouterr()
    examples = stage_export(args, tmp_path, {"corpus_sha256": digest}, digest)
    assert len(examples) == 1
    assert "unchanged" in capsys.readouterr().out


def test_corpus_sha256_ignores_how_a_path_is_spelled(tmp_path, monkeypatch):
    corpus = tmp_path / "c.jsonl"
    corpus.write_text('{"a": 1}\n')
    monkeypatch.chdir(tmp_path)
    assert corpus_sha256([Path("c.jsonl")]) == corpus_sha256([corpus])


class _HeavyBoundary(Exception):
    """Raised where a real run would import torch: everything before it ran for real."""


def _real_run_until_torch(monkeypatch, argv):
    def boundary():
        raise _HeavyBoundary

    monkeypatch.setattr(finetune_laya, "require_heavy", boundary)
    with pytest.raises(_HeavyBoundary):
        main(argv)


PROGRESS = ["checkpoint_latest", "eval.json", "items.pt", "model"]


def _seed_progress(out):
    (out / "checkpoint_latest").mkdir()
    (out / "checkpoint_latest" / "checkpoint_meta.json").write_text('{"epoch": 2}')
    (out / "items.pt").write_bytes(b"items")
    (out / "eval.json").write_text("{}")
    (out / "model").mkdir()


def _progress(out):
    return sorted(name for name in PROGRESS if (out / name).exists())


def _upstream_row(action):
    return json.dumps({"action": action, "severity": 1, "source": "upstream", "kind": "stage1-severity"}) + "\n"


def test_corpus_change_discards_progress(tmp_path, monkeypatch, capsys):
    # The proxy appends to today's file while it runs; a model trained on the
    # old rows must not be reported as the result for the new ones.
    corpus, out = tmp_path / "c.jsonl", tmp_path / "run"
    corpus.write_text(_upstream_row("a"))
    _real_run_until_torch(monkeypatch, [str(corpus), "--out", str(out)])
    _seed_progress(out)
    with open(corpus, "a") as handle:
        handle.write(_upstream_row("b"))
    capsys.readouterr()
    _real_run_until_torch(monkeypatch, [str(corpus), "--out", str(out)])
    assert _progress(out) == []
    assert "corpus changed" in capsys.readouterr().out
    assert load_state(out)["corpus_sha256"] == corpus_sha256([corpus])


def test_config_change_discards_progress(tmp_path, monkeypatch):
    corpus, out = tmp_path / "c.jsonl", tmp_path / "run"
    corpus.write_text(_upstream_row("a"))
    _real_run_until_torch(monkeypatch, [str(corpus), "--out", str(out)])
    _seed_progress(out)
    _real_run_until_torch(monkeypatch, [str(corpus), "--out", str(out), "--epochs", "3"])
    assert _progress(out) == []
    assert load_state(out)["config"]["epochs"] == 3


def test_unchanged_rerun_keeps_progress(tmp_path, monkeypatch):
    corpus, out = tmp_path / "c.jsonl", tmp_path / "run"
    corpus.write_text(_upstream_row("a"))
    _real_run_until_torch(monkeypatch, [str(corpus), "--out", str(out)])
    _seed_progress(out)
    _real_run_until_torch(monkeypatch, [str(corpus), "--out", str(out)])
    assert _progress(out) == PROGRESS


def test_dry_run_keeps_progress_when_the_config_changed(tmp_path, monkeypatch, capsys):
    corpus, out = tmp_path / "c.jsonl", tmp_path / "run"
    corpus.write_text(_upstream_row("a"))
    _real_run_until_torch(monkeypatch, [str(corpus), "--out", str(out)])
    _seed_progress(out)
    dataset = (out / "dataset.jsonl").read_text()
    capsys.readouterr()
    assert main([str(corpus), "--out", str(out), "--epochs", "3", "--dry-run"]) == 0
    assert _progress(out) == PROGRESS
    assert load_state(out)["config"]["epochs"] == 2
    assert (out / "dataset.jsonl").read_text() == dataset
    assert "configuration changed" in capsys.readouterr().out
