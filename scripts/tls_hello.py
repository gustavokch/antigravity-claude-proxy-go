"""Parse a TLS ClientHello and compute its JA3 and JA4 fingerprints.

Used by the capture addon (mitm_header_dump.py) so a capture says which TLS
stack sent each request, without root and without a packet capture. The
algorithms follow the published JA3 and JA4 specifications; the unit tests pin
them to values tshark computed for the same bytes.
"""

from __future__ import annotations

import hashlib
import struct

SNI, ALPN, SIGALGS, SUPPORTED_GROUPS, EC_POINT_FORMATS, SUPPORTED_VERSIONS = (
    0x0000, 0x0010, 0x000D, 0x000A, 0x000B, 0x002B)


def _is_grease(value: int) -> bool:
    return value & 0x0F0F == 0x0A0A and value >> 8 == value & 0xFF


def parse(data: bytes) -> dict:
    """Parse a ClientHello given as a TLS record or a bare handshake message."""
    if data[:1] == b"\x16":
        data = data[5:]
    if data[0] != 1:
        raise ValueError("not a ClientHello")
    pos = 4 + 2 + 32  # handshake header, legacy version, random
    pos += 1 + data[pos]  # session id
    (cipher_len,) = struct.unpack_from(">H", data, pos)
    ciphers = list(struct.unpack_from(f">{cipher_len // 2}H", data, pos + 2))
    pos += 2 + cipher_len
    pos += 1 + data[pos]  # compression methods
    (ext_total,) = struct.unpack_from(">H", data, pos)
    pos += 2
    end = pos + ext_total
    extensions: list[tuple[int, bytes]] = []
    while pos < end:
        kind, length = struct.unpack_from(">HH", data, pos)
        extensions.append((kind, data[pos + 4:pos + 4 + length]))
        pos += 4 + length
    return {"ciphers": ciphers, "extensions": extensions}


def _u16_list(body: bytes, skip: int = 2) -> list[int]:
    body = body[skip:]
    return list(struct.unpack(f">{len(body) // 2}H", body))


def details(hello: dict) -> dict:
    ext = dict(hello["extensions"])
    out: dict = {"alpn": [], "sigalgs": [], "groups": [], "versions": [], "points": []}
    if ALPN in ext:
        body, pos = ext[ALPN], 2
        while pos < len(body):
            size = body[pos]
            out["alpn"].append(body[pos + 1:pos + 1 + size].decode("latin1"))
            pos += 1 + size
    if SIGALGS in ext:
        out["sigalgs"] = _u16_list(ext[SIGALGS])
    if SUPPORTED_GROUPS in ext:
        out["groups"] = _u16_list(ext[SUPPORTED_GROUPS])
    if SUPPORTED_VERSIONS in ext:
        out["versions"] = _u16_list(ext[SUPPORTED_VERSIONS], skip=1)
    if EC_POINT_FORMATS in ext:
        out["points"] = list(ext[EC_POINT_FORMATS][1:])
    return out


def ja3(hello: dict) -> str:
    info = details(hello)
    fields = [
        "771",
        "-".join(str(c) for c in hello["ciphers"] if not _is_grease(c)),
        "-".join(str(k) for k, _ in hello["extensions"] if not _is_grease(k)),
        "-".join(str(g) for g in info["groups"] if not _is_grease(g)),
        "-".join(str(p) for p in info["points"]),
    ]
    return hashlib.md5(",".join(fields).encode()).hexdigest()


def ja4(hello: dict) -> str:
    info = details(hello)
    ext = hello["extensions"]
    ciphers = [c for c in hello["ciphers"] if not _is_grease(c)]
    kinds = [k for k, _ in ext if not _is_grease(k)]
    versions = [v for v in info["versions"] if not _is_grease(v)]
    version = {0x0304: "13", 0x0303: "12", 0x0302: "11", 0x0301: "10"}.get(
        max(versions) if versions else 0x0303, "00")
    alpn = info["alpn"][0] if info["alpn"] else ""
    alpn_code = (alpn[0] + alpn[-1]) if alpn else "00"
    head = (f"t{version}{'d' if SNI in kinds else 'i'}"
            f"{min(len(ciphers), 99):02d}{min(len(kinds), 99):02d}{alpn_code}")

    def digest(text: str) -> str:
        return hashlib.sha256(text.encode()).hexdigest()[:12]

    cipher_part = digest(",".join(f"{c:04x}" for c in sorted(ciphers)))
    hashed = ",".join(f"{k:04x}" for k in sorted(kinds) if k not in (SNI, ALPN))
    if info["sigalgs"]:
        hashed += "_" + ",".join(f"{s:04x}" for s in info["sigalgs"])
    return f"{head}_{cipher_part}_{digest(hashed)}"


def fingerprints(data: bytes) -> dict:
    """JA4, JA3 and ALPN of a ClientHello; ValueError when it is malformed."""
    try:
        hello = parse(data)
        return {"ja4": ja4(hello), "ja3": ja3(hello),
                "alpn": details(hello)["alpn"]}
    except (struct.error, IndexError) as error:
        raise ValueError(f"malformed ClientHello: {error}") from error
