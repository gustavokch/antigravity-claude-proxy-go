#!/usr/bin/env bash
set -euo pipefail

# Live JA4 fingerprint re-verification gate.
#
# Sends one request using the production Cloud Code client with an invalid
# token, captures its ClientHello, and asserts the upstream agy baseline:
#   t13d1312h2_f57a46bbacb6_f50d94e863eb
#
# Runs by default. Exits non-zero on an actual fingerprint mismatch.
# Environments without tcpdump/tshark or capture privileges (no sudo,
# no raw-socket access, no network egress) cannot produce a capture to
# assert against; those SKIP (exit 0, loudly) rather than hard-fail, so
# the gate degrades gracefully instead of blocking every contributor
# who lacks packet-capture tooling. Set ANTIGRAVITY_SKIP_JA4_GATE=1 to
# skip intentionally without attempting a capture at all.

PCAP_FILE="/tmp/antigravity-go.pcap"
EXPECTED_JA4="t13d1312h2_f57a46bbacb6_f50d94e863eb"
PROBE_LOG="/tmp/clientprobe-ja4.log"

skip() {
  echo "SKIPPED: $1" >&2
  echo "JA4 gate did not run — this is NOT a pass. Rerun where tcpdump/tshark and capture privileges are available to get real assurance." >&2
  exit 0
}

if [ "${ANTIGRAVITY_SKIP_JA4_GATE:-0}" = "1" ]; then
  skip "ANTIGRAVITY_SKIP_JA4_GATE=1"
fi

if ! command -v tcpdump >/dev/null 2>&1; then
  skip "tcpdump not found in PATH"
fi
if ! command -v tshark >/dev/null 2>&1; then
  skip "tshark not found in PATH"
fi

echo "=== 1. Building Production Transport Probe ==="
mkdir -p bin
go build -o bin/clientprobe ./tools/clientprobe

echo "=== 2. Cleaning Previous PCAP ==="
rm -f "$PCAP_FILE"


cleanup() {
  echo "=== Stopping processes ==="
  if [ -n "${TCPDUMP_PID:-}" ] && kill -0 "$TCPDUMP_PID" 2>/dev/null; then
    kill "$TCPDUMP_PID" 2>/dev/null || true
    wait "$TCPDUMP_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT


echo "=== 3. Starting Packet Capture ==="
# macOS rejects -i any for unprivileged users; pick the egress interface for
# Cloud Code. Default to en0 (Wi-Fi) and let ANTIGRAVITY_CAPTURE_IFACE override.
if [[ "$(uname -s)" == "Darwin" ]]; then
  CAP_IFACE="${ANTIGRAVITY_CAPTURE_IFACE:-en0}"
else
  CAP_IFACE="any"
fi
tcpdump -i "$CAP_IFACE" -w "$PCAP_FILE" \
  "tcp port 443 and (host daily-cloudcode-pa.googleapis.com or host cloudcode-pa.googleapis.com)" \
  >/dev/null 2>&1 &
TCPDUMP_PID=$!
sleep 1

echo "=== 4. Triggering Credential-Free Cloud Code Request ==="
# An HTTP rejection still completes the handshake. Never load accounts here:
# a failed OAuth refresh prevents /v1/usage from reaching Cloud Code at all.
if ! ./bin/clientprobe ja4 > "$PROBE_LOG" 2>&1; then
  cat "$PROBE_LOG" >&2
fi

# Allow packets to flush to disk
sleep 2

# Stop packet capture
if kill -0 "$TCPDUMP_PID" 2>/dev/null; then
  kill "$TCPDUMP_PID" 2>/dev/null || true
  wait "$TCPDUMP_PID" 2>/dev/null || true
fi

echo "=== 5. Extracting JA4 with tshark ==="
if [ ! -s "$PCAP_FILE" ]; then
  skip "packet capture file $PCAP_FILE is empty or missing (likely no capture privileges — tcpdump needs root/BPF access)"
fi

OUTPUT=$(tshark -r "$PCAP_FILE" \
  -Y 'tls.handshake.type==1 && (tls.handshake.extensions_server_name contains "cloudcode")' \
  -T fields \
  -e tls.handshake.extensions_server_name \
  -e tls.handshake.extensions_alpn_str \
  -e tls.handshake.ja4 \
  -e tls.handshake.ciphersuite \
  -e tls.handshake.sig_hash_alg | head -n 1)

if [ -z "$OUTPUT" ]; then
  skip "no TLS ClientHello matching cloudcode found in capture (check ANTIGRAVITY_CAPTURE_IFACE, or see probe log at $PROBE_LOG)"
fi

echo "Observed ClientHello Row:"
echo "$OUTPUT"

SNI=$(echo "$OUTPUT" | awk -F'\t' '{print $1}')
ALPN=$(echo "$OUTPUT" | awk -F'\t' '{print $2}')
JA4=$(echo "$OUTPUT" | awk -F'\t' '{print $3}')

echo "=== 6. Asserting JA4 Fingerprint ==="
echo "Observed SNI: $SNI"
echo "Observed JA4: $JA4"
echo "Expected JA4: $EXPECTED_JA4"

if [ "$JA4" != "$EXPECTED_JA4" ]; then
  echo "Fingerprint mismatch! Expected $EXPECTED_JA4 but got $JA4" >&2
  exit 1
fi

echo "Fingerprint verification SUCCESS! JA4 matches agy baseline exact."
