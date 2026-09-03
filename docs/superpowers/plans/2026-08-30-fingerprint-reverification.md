# TLS ClientHello Fingerprint Re-verification Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Re-run and automate packet-level TLS ClientHello JA4 fingerprint verification to ensure the Go proxy matches upstream `agy` baseline (`t13d131100_f57a46bbacb6_f50d94e863eb`).

**Architecture:** Use native Go standard library TLS transport (`crypto/tls` with default zero-config) to send upstream requests to Cloud Code endpoints. Validate live network packets using `tcpdump` and `tshark` packet inspection across SNI, ALPN, JA4, cipher suites, and signature algorithm lists.

**Tech Stack:** Go 1.27rc2/1.24+, `net/http`, `crypto/tls`, `tcpdump`, `tshark` (Wireshark CLI), `curl`, Bash.

**Spec:** Section `## Fingerprint Re-verification Gate` in README.md / AGENTS.md / `.reference/agy-current-baseline.txt`.

## Global Constraints

- **JA4 Hash Target:** MUST match `t13d131100_f57a46bbacb6_f50d94e863eb` exactly.
- **ALPN State:** MUST be absent (empty string) for Cloud Code ClientHello.
- **SNI Target:** `daily-cloudcode-pa.googleapis.com` (or `cloudcode-pa.googleapis.com`).
- **Zero TLS Customization:** Never set custom cipher suites, curves, ALPN, or TLS versions in `tls.Config{}`.
- **Deterministic Verification:** Automated script must exit non-zero on mismatch and zero on exact verification pass.

---

### Task 1: Environment & Tooling Pre-flight Verification

**Files:**
- Test: `scripts/verify-ja4.sh` (tooling probe)

**Interfaces:**
- Consumes: System PATH tools (`go`, `tcpdump`, `tshark`, `curl`)
- Produces: Verified environment capabilities and packet capture permissions

- [ ] **Step 1: Verify installed CLI tools and versions**

Run commands to verify required tools exist:
```bash
go version
which tcpdump
which tshark
which curl
```
Expected output: Valid paths for `tcpdump`, `tshark`, `curl`, and active Go toolchain.

- [ ] **Step 2: Verify packet capture device and permissions**

Run a dry-run 1-packet capture probe:
```bash
tcpdump --version
tshark --version | head -n 2
```
Expected: `tcpdump` and `tshark` print version info cleanly. (If permission denied on Darwin/Linux, ensure user has BPF/sudo packet capture access).

- [ ] **Step 3: Verify DNS resolution for Cloud Code endpoint**

Run:
```bash
host daily-cloudcode-pa.googleapis.com || ping -c 1 daily-cloudcode-pa.googleapis.com
```
Expected: Host resolves to Google IP address.

---

### Task 2: Build Native Go Proxy Binary

**Files:**
- Modify: `cmd/proxy/main.go` (if build fixes needed)
- Binary: `bin/antigravity-proxy`

**Interfaces:**
- Consumes: Go source code under `cmd/proxy/`, `internal/`
- Produces: Standalone binary `bin/antigravity-proxy`

- [ ] **Step 1: Compile proxy binary with release flags**

Run:
```bash
mkdir -p bin
go build -o bin/antigravity-proxy ./cmd/proxy
```
Expected: Zero compilation errors; binary generated at `bin/antigravity-proxy`.

- [ ] **Step 2: Verify binary execution and help output**

Run:
```bash
./bin/antigravity-proxy -h || ./bin/antigravity-proxy --help
```
Expected: Displays command-line options (`-listen`, `-api-key`, etc.).

---

### Task 3: Implement Automated JA4 Fingerprint Verification Script

**Files:**
- Create: `scripts/verify-ja4.sh`

**Interfaces:**
- Consumes: `bin/antigravity-proxy`, `tcpdump`, `tshark`, `curl`
- Produces: Executable verification script asserting JA4 `t13d131100_f57a46bbacb6_f50d94e863eb`

- [ ] **Step 1: Write `scripts/verify-ja4.sh`**

Create `scripts/verify-ja4.sh` with exact gate logic and timeout handling:
```bash
#!/usr/bin/env bash
set -euo pipefail

PCAP_FILE="/tmp/antigravity-go.pcap"
EXPECTED_JA4="t13d131100_f57a46bbacb6_f50d94e863eb"
EXPECTED_SNI="daily-cloudcode-pa.googleapis.com"
PROXY_PORT="8099"
PROXY_KEY="${ANTIGRAVITY_PROXY_API_KEY:-test-verification-key}"

echo "=== 1. Building Proxy Binary ==="
mkdir -p bin
go build -o bin/antigravity-proxy ./cmd/proxy

echo "=== 2. Cleaning Previous PCAP ==="
rm -f "$PCAP_FILE"

echo "=== 3. Starting Proxy on port $PROXY_PORT ==="
ANTIGRAVITY_PROXY_LISTEN="127.0.0.1:$PROXY_PORT" \
ANTIGRAVITY_PROXY_API_KEY="$PROXY_KEY" \
./bin/antigravity-proxy > /tmp/proxy-verification.log 2>&1 &
PROXY_PID=$!

cleanup() {
  echo "=== Stopping processes ==="
  if kill -0 "$PROXY_PID" 2>/dev/null; then
    kill "$PROXY_PID" 2>/dev/null || true
  fi
  if [ -n "${TCPDUMP_PID:-}" ] && kill -0 "$TCPDUMP_PID" 2>/dev/null; then
    kill "$TCPDUMP_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# Wait for proxy readiness
sleep 1

echo "=== 4. Starting Packet Capture ==="
# Capture TLS traffic to Cloud Code port 443
if command -v tcpdump >/dev/null 2>&1; then
  tcpdump -i any -w "$PCAP_FILE" "tcp port 443 and (host daily-cloudcode-pa.googleapis.com or host cloudcode-pa.googleapis.com)" >/dev/null 2>&1 &
  TCPDUMP_PID=$!
  sleep 1
else
  echo "Error: tcpdump not found" >&2
  exit 1
fi

echo "=== 5. Triggering Usage Request ==="
# Hit usage endpoint to trigger upstream fetchAvailableModels call
curl -sS -H "x-api-key: $PROXY_KEY" "http://127.0.0.1:$PROXY_PORT/v1/usage" >/dev/null || true

# Allow packets to flush to disk
sleep 2

# Stop packet capture
if kill -0 "$TCPDUMP_PID" 2>/dev/null; then
  kill "$TCPDUMP_PID" 2>/dev/null || true
  wait "$TCPDUMP_PID" 2>/dev/null || true
fi

echo "=== 6. Extracting JA4 with tshark ==="
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
  exit 1
fi

echo "Observed ClientHello Row:"
echo "$OUTPUT"

SNI=$(echo "$OUTPUT" | awk '{print $1}')
JA4=$(echo "$OUTPUT" | awk '{print $NF}')

echo "=== 7. Asserting JA4 Fingerprint ==="
echo "Observed SNI: $SNI"
echo "Observed JA4: $JA4"
echo "Expected JA4: $EXPECTED_JA4"

if [ "$JA4" != "$EXPECTED_JA4" ]; then
  echo "❌ Fingerprint mismatch! Expected $EXPECTED_JA4 but got $JA4" >&2
  exit 1
fi

echo "✅ Fingerprint verification SUCCESS! JA4 matches agy baseline exact."
```

- [ ] **Step 2: Make script executable**

Run:
```bash
chmod +x scripts/verify-ja4.sh
```
Expected: Permissions updated (`-rwxr-xr-x`).

- [ ] **Step 3: Update `scripts/verify-fingerprint.sh` to include `scripts/verify-ja4.sh`**

Modify `scripts/verify-fingerprint.sh` to invoke `scripts/verify-ja4.sh` as part of the complete gate.

- [ ] **Step 4: Commit verification script**

Run:
```bash
git add scripts/verify-ja4.sh scripts/verify-fingerprint.sh
git commit -m "test(fingerprint): add automated JA4 packet verification gate script"
```

---

### Task 4: Execute Live Fingerprint Capture & Gate Assertion

**Files:**
- Read/Inspect: `/tmp/antigravity-go.pcap`
- Output: Terminal verification results

**Interfaces:**
- Consumes: Live network connection, `scripts/verify-ja4.sh`
- Produces: Verified JA4 output and pcap hash

- [ ] **Step 1: Run automated verification gate**

Run:
```bash
./scripts/verify-ja4.sh
```
Expected output:
```text
=== 1. Building Proxy Binary ===
=== 2. Cleaning Previous PCAP ===
=== 3. Starting Proxy on port 8099 ===
=== 4. Starting Packet Capture ===
=== 5. Triggering Usage Request ===
=== 6. Extracting JA4 with tshark ===
Observed ClientHello Row:
daily-cloudcode-pa.googleapis.com		t13d131100_f57a46bbacb6_f50d94e863eb	...
=== 7. Asserting JA4 Fingerprint ===
Observed SNI: daily-cloudcode-pa.googleapis.com
Observed JA4: t13d131100_f57a46bbacb6_f50d94e863eb
Expected JA4: t13d131100_f57a46bbacb6_f50d94e863eb
✅ Fingerprint verification SUCCESS! JA4 matches agy baseline exact.
```

- [ ] **Step 2: Inspect packet details with tshark manually**

Run:
```bash
tshark -r /tmp/antigravity-go.pcap \
  -Y 'tls.handshake.type==1 && tls.handshake.extensions_server_name contains "cloudcode"' \
  -T fields \
  -e tls.handshake.extensions_server_name \
  -e tls.handshake.extensions_alpn_str \
  -e tls.handshake.ja4
```
Expected exact row output:
```text
daily-cloudcode-pa.googleapis.com	t13d131100_f57a46bbacb6_f50d94e863eb
```

---

### Task 5: Document Baseline Verification Evidence

**Files:**
- Create: `.reference/fingerprint-recheck-20260830.txt`
- Modify: `README.md` (if reference timestamps updated)

**Interfaces:**
- Consumes: `/tmp/antigravity-go.pcap`, tshark output, sha256sum
- Produces: Checked-in baseline evidence file

- [ ] **Step 1: Calculate PCAP SHA-256 hash**

Run:
```bash
shasum -a 256 /tmp/antigravity-go.pcap
```

- [ ] **Step 2: Create `.reference/fingerprint-recheck-20260830.txt`**

Write evidence log with toolchain, SHA256, JA4, cipher list, and signature algorithms:
```text
Live agy/proxy fingerprint recheck
==================================

Captured: 2026-08-30
Toolchain: Go toolchain $(go version)
Capture: /tmp/antigravity-go.pcap

Observed ClientHello:
  SNI:  daily-cloudcode-pa.googleapis.com
  ALPN: absent
  JA4:  t13d131100_f57a46bbacb6_f50d94e863eb

Cipher suites:
  0xc02b,0xc02f,0xc02c,0xc030,0xcca9,0xcca8,0xc009,0xc013,0xc00a,0xc014,0x1301,0x1302,0x1303

Conclusion:
  Go proxy produces exact match with upstream agy JA4 baseline.
```

- [ ] **Step 3: Commit verification evidence**

Run:
```bash
git add .reference/fingerprint-recheck-20260830.txt
git commit -m "docs(fingerprint): add live JA4 verification baseline for 2026-08-30"
```

---
