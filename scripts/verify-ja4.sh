#!/usr/bin/env bash
set -euo pipefail

# Live JA4 fingerprint re-verification gate.
#
# Boots the proxy, captures the upstream ClientHello to Cloud Code,
# and asserts the JA4 fingerprint matches the upstream agy baseline:
#   t13d131100_f57a46bbacb6_f50d94e863eb
#
# Runs by default. Exits non-zero on an actual fingerprint mismatch.
# Environments without tcpdump/tshark or capture privileges (no sudo,
# no raw-socket access, no network egress) cannot produce a capture to
# assert against; those SKIP (exit 0, loudly) rather than hard-fail, so
# the gate degrades gracefully instead of blocking every contributor
# who lacks packet-capture tooling. Set ANTIGRAVITY_SKIP_JA4_GATE=1 to
# skip intentionally without attempting a capture at all.

PCAP_FILE="/tmp/antigravity-go.pcap"
EXPECTED_JA4="t13d131100_f57a46bbacb6_f50d94e863eb"
PROXY_PORT="8099"
PROXY_KEY="${ANTIGRAVITY_PROXY_API_KEY:-test-verification-key}"
PROXY_LOG="/tmp/proxy-verification.log"

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

echo "=== 1. Building Proxy Binary ==="
mkdir -p bin
go build -o bin/antigravity-proxy ./cmd/proxy

echo "=== 2. Cleaning Previous PCAP ==="
rm -f "$PCAP_FILE"

echo "=== 3. Starting Proxy on port $PROXY_PORT ==="
ANTIGRAVITY_PROXY_LISTEN="127.0.0.1:$PROXY_PORT" \
ANTIGRAVITY_PROXY_API_KEY="$PROXY_KEY" \
./bin/antigravity-proxy > "$PROXY_LOG" 2>&1 &
PROXY_PID=$!

cleanup() {
  echo "=== Stopping processes ==="
  if kill -0 "$PROXY_PID" 2>/dev/null; then
    kill "$PROXY_PID" 2>/dev/null || true
    wait "$PROXY_PID" 2>/dev/null || true
  fi
  if [ -n "${TCPDUMP_PID:-}" ] && kill -0 "$TCPDUMP_PID" 2>/dev/null; then
    kill "$TCPDUMP_PID" 2>/dev/null || true
    wait "$TCPDUMP_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# Wait for proxy readiness
sleep 1

echo "=== 4. Starting Packet Capture ==="
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

echo "=== 5. Triggering Usage Request ==="
# Hit usage endpoint to trigger upstream fetchAvailableModels call
curl -sS -H "x-api-key: $PROXY_KEY" \
  "http://127.0.0.1:$PROXY_PORT/v1/usage" >/dev/null || true

# Allow packets to flush to disk
sleep 2

# Stop packet capture
if kill -0 "$TCPDUMP_PID" 2>/dev/null; then
  kill "$TCPDUMP_PID" 2>/dev/null || true
  wait "$TCPDUMP_PID" 2>/dev/null || true
fi

echo "=== 6. Extracting JA4 with tshark ==="
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
  skip "no TLS ClientHello matching cloudcode found in capture (check ANTIGRAVITY_CAPTURE_IFACE, or see proxy log at $PROXY_LOG)"
fi

echo "Observed ClientHello Row:"
echo "$OUTPUT"

SNI=$(echo "$OUTPUT" | awk -F'\t' '{print $1}')
ALPN=$(echo "$OUTPUT" | awk -F'\t' '{print $2}')
JA4=$(echo "$OUTPUT" | awk -F'\t' '{print $3}')

echo "=== 7. Asserting JA4 Fingerprint ==="
echo "Observed SNI: $SNI"
echo "Observed JA4: $JA4"
echo "Expected JA4: $EXPECTED_JA4"

if [ "$JA4" != "$EXPECTED_JA4" ]; then
  echo "Fingerprint mismatch! Expected $EXPECTED_JA4 but got $JA4" >&2
  exit 1
fi

echo "Fingerprint verification SUCCESS! JA4 matches agy baseline exact."