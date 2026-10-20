"""Derive the two hashed fields of Claude Code's billing header from a body.

Claude Code 2.1.304 writes system[0] as

    x-anthropic-billing-header: cc_version=<version>.<suffix>; ...; cch=<cch>; ...

and both hashed values can be recomputed from the request alone:

  suffix  sha256(SALT + text[4] + text[7] + text[20] + version).hex()[:3], where
          text is the first text block of the first user prompt that is not a
          <system-reminder>, indexed in UTF-16 code units, "0" when missing.
  cch     xxHash64(preimage, SEED) & 0xFFFFF as five hex digits, where the
          preimage is the body as sent with cch=00000 in place, the first
          "model" value emptied and the first "max_tokens" field removed with
          one adjacent comma.

Both rules reproduce every body captured from the real client
(internal/ccidentity/testdata/claude-code-2.1.304-bodies.jsonl.gz). The Go
implementation is internal/ccidentity/cch.go; this copy lets a capture of ANY
client be checked, so a new Claude Code release that changes the seed or the
salt shows up as a mismatch in its own capture.
"""

from __future__ import annotations

import hashlib
import json
import re

SEED = 0x4D659218E32A3268
SALT = "59cf53e54c78"
PLACEHOLDER = b"cch=00000"

_M = (1 << 64) - 1
_P1 = 11400714785074694791
_P2 = 14029467366897019727
_P3 = 1609587929392839161
_P4 = 9650029242287828579
_P5 = 2870177450012600261


def _rotl(x: int, r: int) -> int:
    return ((x << r) | (x >> (64 - r))) & _M


def _round(acc: int, lane: int) -> int:
    acc = (acc + lane * _P2) & _M
    return (_rotl(acc, 31) * _P1) & _M


def _merge(acc: int, value: int) -> int:
    acc ^= _round(0, value)
    return (acc * _P1 + _P4) & _M


def xxh64(data: bytes, seed: int = 0) -> int:
    """XXH64 of data, the reference algorithm."""
    n, i = len(data), 0
    lane = lambda at, size: int.from_bytes(data[at:at + size], "little")  # noqa: E731
    if n >= 32:
        v = [(seed + _P1 + _P2) & _M, (seed + _P2) & _M, seed & _M, (seed - _P1) & _M]
        while i + 32 <= n:
            v = [_round(v[k], lane(i + 8 * k, 8)) for k in range(4)]
            i += 32
        h = (_rotl(v[0], 1) + _rotl(v[1], 7) + _rotl(v[2], 12) + _rotl(v[3], 18)) & _M
        for value in v:
            h = _merge(h, value)
    else:
        h = (seed + _P5) & _M
    h = (h + n) & _M
    while i + 8 <= n:
        h ^= _round(0, lane(i, 8))
        h = (_rotl(h, 27) * _P1 + _P4) & _M
        i += 8
    if i + 4 <= n:
        h ^= (lane(i, 4) * _P1) & _M
        h = (_rotl(h, 23) * _P2 + _P3) & _M
        i += 4
    while i < n:
        h ^= (data[i] * _P5) & _M
        h = (_rotl(h, 11) * _P1) & _M
        i += 1
    h ^= h >> 33
    h = (h * _P2) & _M
    h ^= h >> 29
    h = (h * _P3) & _M
    return h ^ (h >> 32)


_MODEL_VALUE = re.compile(rb'("model":")[^"]*(")')
_MAX_TOKENS = re.compile(rb'"max_tokens":\d+,|,"max_tokens":\d+')


def cch_preimage(body: bytes) -> bytes:
    """The bytes the real client hashes, given the body with the placeholder."""
    return _MAX_TOKENS.sub(b"", _MODEL_VALUE.sub(rb"\1\2", body, count=1), count=1)


def _header_cch_span(body: bytes, header: str) -> tuple[int, int] | None:
    """Byte span of the five cch digits inside system[0] of body."""
    anchor = b'"system":[{"type":"text","text":' + json.dumps(header).encode()
    at = body.find(anchor)
    if at < 0:
        return None
    match = re.search(rb"cch=([0-9a-f]{5});", body[at:at + len(anchor)])
    if match is None:
        return None
    return at + match.start(1), at + match.end(1)


def first_prompt_text(body: dict) -> str:
    """The text the cc_version suffix hashes."""
    for message in body.get("messages") or []:
        if not isinstance(message, dict) or message.get("role") != "user":
            continue
        content = message.get("content")
        if isinstance(content, str):
            if _is_prompt_text(content):
                return content
            continue
        if not isinstance(content, list):
            continue
        texts = [
            block.get("text", "")
            for block in content
            if isinstance(block, dict) and block.get("type") == "text"
            and _is_prompt_text(block.get("text", ""))
        ]
        attached = any(
            isinstance(block, dict) and block.get("type") not in ("text", "tool_result")
            for block in content
        )
        if texts or attached:
            return texts[0] if texts else ""
    return ""


def _is_prompt_text(text: object) -> bool:
    if not isinstance(text, str):
        return False
    text = text.strip()
    return bool(text) and not text.startswith("<system-reminder>")


def version_suffix(prompt: str, version: str) -> str:
    units = prompt.encode("utf-16-le", "surrogatepass")

    def char(index: int) -> str:
        if 2 * index + 2 > len(units):
            return "0"
        return units[2 * index:2 * index + 2].decode("utf-16-le", "replace")

    seed = SALT + "".join(char(i) for i in (4, 7, 20)) + version
    return hashlib.sha256(seed.encode("utf-8", "replace")).hexdigest()[:3]


_VERSION = re.compile(r"cc_version=(\d+\.\d+\.\d+)\.([0-9a-f]{3});")


def billing_verdict(content: bytes) -> dict | None:
    """Sent and expected values of both hashed fields, or None when the body
    carries no billing header. A value that cannot be located is None."""
    try:
        body = json.loads(content.decode("utf-8"))
    except (UnicodeDecodeError, ValueError):
        return None
    if not isinstance(body, dict):
        return None
    system = body.get("system")
    if not isinstance(system, list) or not system or not isinstance(system[0], dict):
        return None
    header = system[0].get("text")
    if not isinstance(header, str) or not header.startswith("x-anthropic-billing-header: "):
        return None

    verdict: dict = {"cch": None, "cch_expected": None,
                     "version_suffix": None, "version_suffix_expected": None}
    version = _VERSION.search(header)
    if version:
        verdict["version_suffix"] = version.group(2)
        verdict["version_suffix_expected"] = version_suffix(
            first_prompt_text(body), version.group(1))

    span = _header_cch_span(content, header)
    if span is not None:
        start, end = span
        verdict["cch"] = content[start:end].decode()
        zeroed = content[:start] + b"00000" + content[end:]
        verdict["cch_expected"] = format(xxh64(cch_preimage(zeroed), SEED) & 0xFFFFF, "05x")
    return verdict
