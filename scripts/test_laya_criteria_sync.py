"""The A-D criteria text must stay identical in Go and Python, and must
describe the severity bands the exporter buckets by.

Three copies of the criteria exist: the Go default (config.go
defaultLayaCriteria, used when serving prompts to laya-serve), the Python
exporter (corpus_to_laya.py CRITERIA, baked into every training example),
and the docs. A drift between the prompt text and the training labels
teaches the checkpoint a confused mapping, so this test fails the build
on any mismatch rather than letting it reach a training run.
"""
import re
from pathlib import Path

import corpus_to_laya

REPO_ROOT = Path(__file__).resolve().parent.parent
CONFIG_GO = REPO_ROOT / "internal" / "config" / "config.go"


def go_default_criteria():
    """Parse defaultLayaCriteria out of config.go as an {label: text} dict."""
    source = CONFIG_GO.read_text(encoding="utf-8")
    block = re.search(
        r"var defaultLayaCriteria = map\[string\]string\{(.*?)\n\}", source, re.S
    )
    assert block, "defaultLayaCriteria map not found in config.go"
    return dict(re.findall(r'"([A-D])":\s*"([^"]+)"', block.group(1)))


def test_go_and_python_criteria_are_identical():
    assert go_default_criteria() == corpus_to_laya.CRITERIA


def test_criteria_wording_cites_its_severity_band():
    """Every criterion must name the severity range it labels, so the
    prompt text and the exporter's bucket_for() cannot drift apart in
    meaning even if both are edited by hand."""
    for label, low, high in corpus_to_laya.BUCKETS:
        text = corpus_to_laya.CRITERIA[label]
        assert str(low) in text, f"{label} criterion does not cite band floor {low}: {text!r}"
        assert str(high) in text, f"{label} criterion does not cite band ceiling {high}: {text!r}"


def test_serving_severity_map_stays_sub_block_and_monotonic():
    """defaultLayaSeverityMap is a serving-time choice, not a training
    label: it must stay below the block boundary of 50 (a laya verdict
    must never block) and stay monotonic with the label order, but it
    need not sit inside the exporter's band for each label."""
    source = CONFIG_GO.read_text(encoding="utf-8")
    block = re.search(
        r"var defaultLayaSeverityMap = map\[string\]int\{(.*?)\}", source, re.S
    )
    assert block, "defaultLayaSeverityMap not found in config.go"
    severity_map = {k: int(v) for k, v in re.findall(r'"([A-D])":\s*(\d+)', block.group(1))}
    assert severity_map, "no entries parsed from defaultLayaSeverityMap"
    ordered = [severity_map[label] for label, _, _ in corpus_to_laya.BUCKETS]
    assert ordered == sorted(ordered), f"serving severities not monotonic: {ordered}"
    assert all(severity < 50 for severity in ordered), (
        f"a laya verdict must never reach the block boundary 50: {severity_map}"
    )
