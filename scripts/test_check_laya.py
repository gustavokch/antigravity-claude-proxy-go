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
        {"answers": {"risk": {"choice": label, "confidence": 0.9}}}
    ).encode()


def test_check_accepts_valid_choice_answer(laya_server):
    _Handler.response_body = _typed_answer("C")
    _Handler.status = 200

    result = check_laya.check(laya_server, action="git status", timeout=5)

    assert result.label == "C"
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
