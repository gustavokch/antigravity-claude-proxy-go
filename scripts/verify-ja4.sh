#!/usr/bin/env bash
set -euo pipefail

# Live JA4 fingerprint re-verification gate.
#
# Boots the proxy, captures the upstream ClientHello to Cloud Code,
# and asserts the JA4 fingerprint matches the upstream agy baseline:
#   t13d131100_f57a46bbacb6_f50d94e863eb
#
# Exits non-zero on any mismatch or missing dependency.

PCAP_FILE="/tmp/antigravity-go.pcap"
EXPECTED_JA4="t13d131100_f57a46bbacb6_f50d94e863eb"
EXPECTED_SNI="daily-cloudcode-pa.googleapis.com"
PROXY_PORT="8099"
PROXY_KEY="${ANTIGRAVITY_PROXY_API_KEY:-test-verification-key}"
PROXY_LOG="/tmp/proxy-verification.log"

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
if ! command -v tcpdump >/dev/null 2>&1; then
  echo "Error: tcpdump not found in PATH" >&2
  exit 1
fi
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
if ! command -v tshark >/dev/null 2>&1; then
  echo "Error: tshark not found in PATH" >&2
  exit 1
fi

if [ ! -s "$PCAP_FILE" ]; then
  echo "Error: Packet capture file $PCAP_FILE is empty or missing" >&2
  exit 1
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
  echo "Error: No TLS ClientHello matching cloudcode found in capture" >&2
  echo "Proxy log tail:" >&2
  tail -n 50 "$PROXY_LOG" >&2 || true
  exit 1
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