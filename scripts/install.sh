#!/usr/bin/env bash
set -euo pipefail

# Directory of this script
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

TARGET_DIR="${HOME}/.local/bin"
BINARY_NAME="antigravity-proxy"
VERSION="${VERSION:-$(git -C "${REPO_ROOT}" describe --tags --always --dirty 2>/dev/null || echo "1.0.0")}"

echo "==> Cleaning previous build artifacts..."
rm -rf "${REPO_ROOT}/bin" "${REPO_ROOT}/proxy"

echo "==> Running tests..."
go test ./...

echo "==> Building release binary (version: ${VERSION})..."
mkdir -p "${REPO_ROOT}/bin"
CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o "${REPO_ROOT}/bin/${BINARY_NAME}" \
    "${REPO_ROOT}/cmd/proxy"

echo "==> Installing ${BINARY_NAME} to ${TARGET_DIR}..."
mkdir -p "${TARGET_DIR}"
# Atomic swap: stage beside the target, then rename(2). A process starting
# mid-install can never exec a partially-written binary.
TMP_TARGET="${TARGET_DIR}/${BINARY_NAME}.tmp.$$"
cp -f "${REPO_ROOT}/bin/${BINARY_NAME}" "${TMP_TARGET}"
chmod +x "${TMP_TARGET}"
mv -f "${TMP_TARGET}" "${TARGET_DIR}/${BINARY_NAME}"

echo "==> Verifying installation..."
"${TARGET_DIR}/${BINARY_NAME}" --help >/dev/null 2>&1 || true

echo "==> Successfully installed ${BINARY_NAME} to ${TARGET_DIR}/${BINARY_NAME}"

if [[ ":${PATH}:" != *":${TARGET_DIR}:"* ]]; then
    echo "Note: ${TARGET_DIR} is not in your PATH. Add it to your shell configuration file:"
    echo "    export PATH=\"\${HOME}/.local/bin:\${PATH}\""
fi

# A freshly installed binary does nothing while the previous process keeps
# running — the old code stays live until restart. Default: restart it.
if [[ "${SKIP_RESTART:-0}" == "1" ]]; then
    echo "==> SKIP_RESTART=1 — leaving any running instance untouched."
    exit 0
fi

echo "==> Restarting running instance (if any)..."
PIDS="$(pgrep -f "${TARGET_DIR}/${BINARY_NAME}" 2>/dev/null || true)"
if [[ -z "${PIDS}" ]]; then
    echo "    No running instance found. Start it manually if needed:"
    echo "    ${TARGET_DIR}/${BINARY_NAME} --port 8080"
    exit 0
fi

FIRST_PID="$(echo "${PIDS}" | head -n 1)"
# Preserve the original launch flags (everything after argv[0]).
OLD_ARGS="$(ps -o command= -p "${FIRST_PID}" | cut -d' ' -f2-)"
echo "    Stopping pid(s): ${PIDS//$'\n'/ } (flags: ${OLD_ARGS:-<none>})"
kill ${PIDS} 2>/dev/null || true
for _ in $(seq 1 20); do
    pgrep -f "${TARGET_DIR}/${BINARY_NAME}" >/dev/null 2>&1 || break
    sleep 0.25
done
# Graceful window expired — force.
PIDS="$(pgrep -f "${TARGET_DIR}/${BINARY_NAME}" 2>/dev/null || true)"
if [[ -n "${PIDS}" ]]; then
    kill -9 ${PIDS} 2>/dev/null || true
fi

LOG_FILE="${HOME}/.local/state/${BINARY_NAME}.log"
mkdir -p "$(dirname "${LOG_FILE}")"
# shellcheck disable=SC2086 — OLD_ARGS is intentionally word-split back into argv.
nohup "${TARGET_DIR}/${BINARY_NAME}" ${OLD_ARGS} >>"${LOG_FILE}" 2>&1 &

sleep 1
NEW_PID="$(pgrep -f "${TARGET_DIR}/${BINARY_NAME}" 2>/dev/null | head -n 1 || true)"
if [[ -z "${NEW_PID}" ]]; then
    echo "    ERROR: process did not start — check ${LOG_FILE}" >&2
    exit 1
fi
echo "    Running: pid ${NEW_PID} (was ${FIRST_PID}) — log: ${LOG_FILE}"
