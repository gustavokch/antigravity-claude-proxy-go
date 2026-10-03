"""The A-D criteria text must stay identical in Go and Python, and must
describe the severity bands the exporter buckets by.

Three copies of the criteria exist: the Go default (config.go
defaultLayaCriteria, used when serving prompts to laya-serve), the Python
exporter (corpus_to_laya.py CRITERIA, baked into every training example),
and the docs. A drift between the prompt text and the training labels
teaches the checkpoint a confused mapping, so this test fails the build
on any mismatch rather than letting it reach a training run.

The question text and the constants replay_jev.py sends must follow the Go
defaults too.
"""
import json
import re
from pathlib import Path

import corpus_to_laya
import replay_jev

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

def go_const(name):
    """A string or integer constant out of config.go, decoded."""
    source = CONFIG_GO.read_text(encoding="utf-8")
    match = re.search(rf'\b{name}\s*=\s*("(?:[^"\\]|\\.)*"|\d+)', source)
    assert match, f"{name} not found in config.go"
    token = match.group(1)
    return json.loads(token) if token.startswith('"') else int(token)

def go_escalate_labels():
    """defaultLayaEscalateLabels out of config.go, as a tuple."""
    source = CONFIG_GO.read_text(encoding="utf-8")
    block = re.search(r"var defaultLayaEscalateLabels = \[\]string\{(.*?)\}", source, re.S)
    assert block, "defaultLayaEscalateLabels not found in config.go"
    return tuple(re.findall(r'"([^"]+)"', block.group(1)))

def test_go_and_python_question_instructions_are_identical():
    assert corpus_to_laya.INSTRUCTIONS == go_const("DefaultLayaInstructions")

def test_replay_defaults_follow_the_go_defaults():
    """replay_jev.py must send what a default jev backend sends. A constant
    that drifts measures a request the proxy never makes, and the enablement
    gate would then pass on evidence about something else."""
    assert replay_jev.MODEL == go_const("DefaultJevModel")
    assert replay_jev.QUESTION == go_const("DefaultLayaQuestionName")
    assert replay_jev.STATE_CHARS == go_const("DefaultLayaStateChars")
    assert replay_jev.ESCALATE == go_escalate_labels()
