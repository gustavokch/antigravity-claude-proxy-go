#!/usr/bin/env bash
set -euo pipefail

echo "=== Building Proxy ==="
go build -o bin/proxy ./cmd/proxy

echo "=== Running Full Test Suite ==="
go test -v ./...

echo "=== Running Performance Benchmarks ==="
./scripts/benchmark.sh

echo "=== Running Live JA4 Packet Verification Gate ==="
./scripts/verify-ja4.sh

echo "=== Verification Complete ==="
