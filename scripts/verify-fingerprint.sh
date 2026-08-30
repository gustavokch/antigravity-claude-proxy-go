#!/usr/bin/env bash
set -euo pipefail

echo "=== Building Proxy ==="
go build -o bin/proxy ./cmd/proxy

echo "=== Running Full Test Suite ==="
go test -v ./...

echo "=== Running Performance Benchmarks ==="
./scripts/benchmark.sh

echo "=== Running Live JA4 Packet Verification Gate ==="
# Runs by default; degrades to a loud SKIP (not a silent pass) in
# environments without tcpdump/tshark or capture privileges. Set
# ANTIGRAVITY_SKIP_JA4_GATE=1 to skip intentionally.
./scripts/verify-ja4.sh

echo "=== Verification Complete ==="
