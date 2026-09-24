"""Tests for check_laya.py, the live laya-serve wire-contract smoke check."""
import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

import check_laya


class _Handler(BaseHTTPRequestHandler):
    """Serves one canned response, records the last request body."""

    response_body = b"{}"
    status = 200
    last_body = None

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        type(self).last_body = self.rfile.read(length)
        self.send_response(self.status)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(self.response_body)

    def log_message(self, *args):
        pass


@pytest.fixture
def laya_server():
    server = HTTPServer(("127.0.0.1", 0), _Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    yield f"http://127.0.0.1:{server.server_port}"
    server.shutdown()
    thread.join()


def _typed_answer(label):
    return json.dumps(
        {"answers": {"risk": {"choice": label, "confidence": 0.9, "answer_confidence": 0.49}}}
    ).encode()


def test_check_accepts_valid_choice_answer(laya_server):
    _Handler.response_body = _typed_answer("C")
    _Handler.status = 200

    result = check_laya.check(laya_server, action="git status", timeout=5)

    assert result.label == "C"
    assert result.answer_confidence == 0.49
    request = json.loads(_Handler.last_body)
    assert request["state"]["action"] == "git status"
    assert request["questions"]["risk"]["type"] == "choice"


def test_check_payload_matches_exporter_criteria(laya_server):
    """The smoke check must send the same question text the exporter bakes
    into training examples, or a green check proves nothing about the
    contract the checkpoint was trained on."""
    import corpus_to_laya

    _Handler.response_body = _typed_answer("A")
    _Handler.status = 200

    check_laya.check(laya_server, action="ls", timeout=5)

    request = json.loads(_Handler.last_body)
    question = request["questions"]["risk"]
    assert question["criteria"] == corpus_to_laya.CRITERIA
    assert question["instructions"] == corpus_to_laya.INSTRUCTIONS


def test_check_rejects_unknown_label(laya_server):
    _Handler.response_body = _typed_answer("E")
    _Handler.status = 200

    with pytest.raises(check_laya.CheckError, match="label"):
        check_laya.check(laya_server, action="ls", timeout=5)


def test_check_rejects_malformed_response(laya_server):
    _Handler.response_body = b"not json"
    _Handler.status = 200

    with pytest.raises(check_laya.CheckError):
        check_laya.check(laya_server, action="ls", timeout=5)


def test_check_rejects_http_error(laya_server):
    _Handler.response_body = b"{}"
    _Handler.status = 500

    with pytest.raises(check_laya.CheckError, match="500"):
        check_laya.check(laya_server, action="ls", timeout=5)


def test_check_unreachable_server_reports_cleanly():
    with pytest.raises(check_laya.CheckError, match="connect"):
        check_laya.check("http://127.0.0.1:1", action="ls", timeout=2)


def test_check_pins_the_proxy_default_checkpoint(laya_server):
    """The smoke check must name the checkpoint the proxy names, or a green
    check proves a model the proxy never asks for."""
    import re
    from pathlib import Path

    config_go = Path(__file__).resolve().parent.parent / "internal" / "config" / "config.go"
    match = re.search(r'DefaultLayaModel\s*=\s*"([^"]+)"', config_go.read_text(encoding="utf-8"))
    assert match, "DefaultLayaModel not found in config.go"

    _Handler.response_body = _typed_answer("A")
    _Handler.status = 200
    check_laya.check(laya_server, action="ls", timeout=5)

    assert json.loads(_Handler.last_body)["model"] == match.group(1)


@pytest.mark.parametrize(
    "answer",
    [
        pytest.param({"choice": "A", "confidence": 0.9}, id="missing"),
        pytest.param({"choice": "A", "answer_confidence": "high"}, id="not-a-number"),
        pytest.param({"choice": "A", "answer_confidence": 1.5}, id="above-one"),
    ],
)
def test_check_rejects_an_unusable_answer_confidence(laya_server, answer):
    """layaMinConfidence compares answer_confidence. A server that drops or
    mangles it makes every answer escalate under a floor, silently, so the
    smoke check must fail loudly instead."""
    _Handler.response_body = json.dumps({"answers": {"risk": answer}}).encode()
    _Handler.status = 200

    with pytest.raises(check_laya.CheckError, match="answer_confidence"):
        check_laya.check(laya_server, action="ls", timeout=5)


def test_main_sends_the_model_it_is_given(laya_server):
    """A backend may set model to another checkpoint the server preloads;
    the check must be able to ask for that one, or it proves a model the
    proxy never requests."""
    _Handler.response_body = _typed_answer("A")
    _Handler.status = 200

    assert check_laya.main(["--url", laya_server, "--model", "multilingual"]) == 0
    assert json.loads(_Handler.last_body)["model"] == "multilingual"
