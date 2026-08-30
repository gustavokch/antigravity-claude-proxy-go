#!/usr/bin/env bash
set -euo pipefail

echo "=== Building Proxy ==="
go build -o bin/proxy ./cmd/proxy

echo "=== Running Full Test Suite ==="
go test -v ./...

echo "=== Running Performance Benchmarks ==="
./scripts/benchmark.sh

if [ "${ANTIGRAVITY_RUN_JA4_GATE:-0}" = "1" ]; then
	echo "=== Running Live JA4 Packet Verification Gate ==="
	./scripts/verify-ja4.sh
else
	echo "=== Skipping Live JA4 Packet Verification Gate (set ANTIGRAVITY_RUN_JA4_GATE=1 to run; needs tcpdump/tshark, capture privileges, and live network egress) ==="
fi

echo "=== Verification Complete ==="
