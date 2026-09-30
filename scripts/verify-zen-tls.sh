#!/usr/bin/env bash
set -euo pipefail

# Live Zen TLS fingerprint gate (utls ClientHello replay).
#
# Boots the proxy from a THROWAWAY config copy (user config untouched) with
# zen.harness.tls=true, captures the upstream ClientHello to opencode.ai,
# and asserts both fingerprints against the genuine OpenCode capture
# (internal/zen/opencode-clienthello.bin):
#   JA4 t13d1713h1_5b57614c22b0_6a3d802a7139
#   JA3 1523504b38f0fae0d881d4b6554aac1b   (md5 of ja3_full)
#
# Run with sudo (tcpdump needs BPF/root):
#   sudo ./scripts/verify-zen-tls.sh
#
# A 403 free-tier gate response is EXPECTED-OK here: the TLS fingerprint is
# what this script verifies; the request secret is a separate problem.
# Exit: 0 = fingerprints match, 1 = mismatch or zen hop missing,
# 2 = skip (no capture tooling/privileges — NOT a pass).

OUT_DIR="${ANTIGRAVITY_ZEN_VERIFY_DIR:-/tmp/zen-tls-verify}"
PCAP_FILE="$OUT_DIR/opencode.pcap"
PROXY_PORT="${ANTIGRAVITY_ZEN_VERIFY_PORT:-18099}"
PROXY_LOG="$OUT_DIR/proxy.log"
MODEL="${ANTIGRAVITY_ZEN_VERIFY_MODEL:-mimo-v2.6-flash-free}"

EXPECTED_JA4="t13d1713h1_5b57614c22b0_6a3d802a7139"
EXPECTED_JA3="1523504b38f0fae0d881d4b6554aac1b"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

CONFIG_SRC="${ANTIGRAVITY_CONFIG_DIR:-$HOME/.config/antigravity-proxy}/config.json"

skip() {
  echo "SKIPPED: $1" >&2
  echo "Zen TLS gate did not run — this is NOT a pass." >&2
  exit 2
}
fail() {
  echo "FAIL: $1" >&2
  exit 1
}

if [ "$(id -u)" -ne 0 ]; then
  skip "not root — tcpdump needs sudo"
fi
command -v tcpdump >/dev/null 2>&1 || skip "tcpdump not found in PATH"
command -v tshark  >/dev/null 2>&1 || skip "tshark not found in PATH"
command -v jq      >/dev/null 2>&1 || skip "jq not found in PATH"
[ -f "$CONFIG_SRC" ] || skip "no config at $CONFIG_SRC"
[ -f internal/zen/opencode-clienthello.bin ] || fail "missing internal/zen/opencode-clienthello.bin"

mkdir -p "$OUT_DIR"
rm -f "$PCAP_FILE"

echo "=== 1. Building proxy (output outside repo) ==="
PROXY_BIN="$OUT_DIR/zen-verify-proxy"
if [ -x bin/proxy ]; then
  cp bin/proxy "$PROXY_BIN"
else
  go build -o "$PROXY_BIN" ./cmd/proxy
fi

echo "=== 2. Preparing throwaway config (harness.tls=true) ==="
CFG_DIR="$(mktemp -d "$OUT_DIR/cfg.XXXXXX")"
jq '.zen.harness = ((.zen.harness // {}) + {enabled: true, tls: true})' \
  "$CONFIG_SRC" > "$CFG_DIR/config.json"

PROXY_PID=""
TCPDUMP_PID=""
cleanup() {
  if [ -n "$PROXY_PID" ] && kill -0 "$PROXY_PID" 2>/dev/null; then
    kill "$PROXY_PID" 2>/dev/null || true
    wait "$PROXY_PID" 2>/dev/null || true
  fi
  if [ -n "$TCPDUMP_PID" ] && kill -0 "$TCPDUMP_PID" 2>/dev/null; then
    kill -INT "$TCPDUMP_PID" 2>/dev/null || true
    wait "$TCPDUMP_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

echo "=== 3. Starting proxy on 127.0.0.1:$PROXY_PORT ==="
ANTIGRAVITY_CONFIG_DIR="$CFG_DIR" \
  "$PROXY_BIN" -listen "127.0.0.1:$PROXY_PORT" > "$PROXY_LOG" 2>&1 &
PROXY_PID=$!

ready=0
for _ in $(seq 1 60); do
  if curl -sS -m 2 "http://127.0.0.1:$PROXY_PORT/health" >/dev/null 2>&1; then
    ready=1
    break
  fi
  kill -0 "$PROXY_PID" 2>/dev/null || break
  sleep 0.5
done
if [ "$ready" -ne 1 ]; then
  tail -n 40 "$PROXY_LOG" >&2 || true
  fail "proxy did not become ready (see $PROXY_LOG)"
fi

echo "=== 4. Starting packet capture (tcp port 443) ==="
# macOS: pktap,all captures all interfaces as root (same as AGENTS.md
# fingerprint-recheck); Linux: any.
if [ "$(uname -s)" = "Darwin" ]; then
  CAP_IFACE="${ANTIGRAVITY_CAPTURE_IFACE:-pktap,all}"
else
  CAP_IFACE="${ANTIGRAVITY_CAPTURE_IFACE:-any}"
fi
tcpdump -i "$CAP_IFACE" -P -w "$PCAP_FILE" "tcp port 443" >/dev/null 2>&1 &
TCPDUMP_PID=$!
sleep 2

echo "=== 5. Sending Zen request (model=$MODEL) ==="
HTTP_STATUS=$(curl -sS -m 90 -o "$OUT_DIR/response.json" -w '%{http_code}' \
  -H 'content-type: application/json' \
  -H 'anthropic-version: 2023-06-01' \
  "http://127.0.0.1:$PROXY_PORT/v1/messages" \
  -d "{\"model\":\"$MODEL\",\"max_tokens\":32,\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}]}") || true
echo "HTTP status: $HTTP_STATUS (403 gate is OK for this gate — TLS is what we assert)"
if grep -q 'zen forward' "$PROXY_LOG"; then
  echo "proxy log confirms zen hop:"
  grep 'zen forward' "$PROXY_LOG" | tail -n 2 | sed 's/^/  /'
else
  tail -n 40 "$PROXY_LOG" >&2
  fail "request never reached the zen backend (model not dispatched to zen?)"
fi

sleep 2
if kill -0 "$TCPDUMP_PID" 2>/dev/null; then
  kill -INT "$TCPDUMP_PID" 2>/dev/null || true
  wait "$TCPDUMP_PID" 2>/dev/null || true
  TCPDUMP_PID=""
fi

echo "=== 6. Extracting ClientHello with tshark ==="
[ -s "$PCAP_FILE" ] || skip "capture file $PCAP_FILE empty — tcpdump lost privileges?"

HELLO_ROWS=$(tshark -r "$PCAP_FILE" \
  -Y 'tls.handshake.type==1 && tls.handshake.extensions_server_name contains "opencode"' \
  -T fields \
  -e frame.number \
  -e tls.handshake.extensions_server_name \
  -e tls.handshake.extensions_alpn_str \
  -e tls.handshake.ja4 \
  -e tls.handshake.ja3_full \
  -e tls.handshake.ja3 2>/dev/null) || true

if [ -z "$HELLO_ROWS" ]; then
  fail "no opencode.ai ClientHello in capture (proxy never dialed upstream? see $PROXY_LOG)"
fi
echo "Observed ClientHello rows (frame, SNI, ALPN, JA4, ja3_full, ja3):"
echo "$HELLO_ROWS" | sed 's/^/  /'

FIRST=$(echo "$HELLO_ROWS" | head -n 1)
SNI=$(echo "$FIRST"  | awk -F'\t' '{print $2}')
ALPN=$(echo "$FIRST" | awk -F'\t' '{print $3}')
JA4=$(echo "$FIRST"  | awk -F'\t' '{print $4}')
JA3_MD5=$(echo "$FIRST" | awk -F'\t' '{print $6}')

echo "=== 7. Asserting fingerprints ==="
echo "SNI:  $SNI   (expect opencode.ai)"
echo "ALPN: $ALPN  (expect http/1.1)"
echo "JA4:  $JA4"
echo "want: $EXPECTED_JA4"
echo "JA3:  $JA3_MD5"
echo "want: $EXPECTED_JA3"

[ "$SNI" = "opencode.ai" ] || fail "SNI = $SNI, want opencode.ai"
[ "$ALPN" = "http/1.1" ] || echo "note: ALPN = $ALPN, expected http/1.1" >&2
[ "$JA4" = "$EXPECTED_JA4" ] || fail "JA4 mismatch: got $JA4, want $EXPECTED_JA4"
[ "$JA3_MD5" = "$EXPECTED_JA3" ] || fail "JA3 mismatch: got $JA3_MD5, want $EXPECTED_JA3"

echo "PASS: Zen TLS ClientHello matches the genuine OpenCode fingerprint."
echo "Artifacts: $OUT_DIR (pcap, proxy log, response.json, config copy)"
