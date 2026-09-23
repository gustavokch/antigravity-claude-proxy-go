#!/usr/bin/env bash
#
# Capture vanilla Claude Code's own wire traffic to api.anthropic.com.
#
# The proxy cannot show this: by the time a request reaches the proxy it may
# already have been rewritten. This harness intercepts Claude Code's traffic
# directly, so the headers and body fingerprint recorded here are the client's,
# not ours.
#
# Usage:
#   scripts/capture-claude-code-headers.sh oauth     # capture with an OAuth token
#   scripts/capture-claude-code-headers.sh apikey    # capture with an API key
#   scripts/capture-claude-code-headers.sh token     # mint an OAuth token for the above
#   scripts/capture-claude-code-headers.sh check     # verify prerequisites only
#
# The OAuth and API-key header sets differ, and the OAuth one is the target
# fingerprint, so capture both and label the artifacts.
#
# Live credentials:
#   - The mitmproxy addon redacts credentials before writing (scripts/mitm_header_dump.py).
#   - Request bodies are never dumped. The addon records a body *fingerprint*
#     (key tree, redacted metadata, and the first system block's identity
#     marker). Prompts, file contents and tool output stay out by construction.
#   - This script never echoes a token.
#
# Network exposure, read before running:
#   mitmdump must listen on an address the Podman VM can reach, so it binds
#   0.0.0.0 on a high port for the duration of the capture. --allow-hosts
#   restricts it to api.anthropic.com, so traffic to any other host is refused
#   rather than intercepted, but the listener does exist while this runs.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONTAINER_DIR="${CLAUDE_CONTAINER_DIR:-${HOME}/Git/claude-container}"
IMAGE_NAME="${CLAUDE_CAPTURE_IMAGE:-claude-box:mitm}"
MITM_PORT="${ANTIGRAVITY_MITM_PORT:-18080}"
MITM_CONFDIR="${MITM_CONFDIR:-${HOME}/.mitmproxy}"
CAPTURE_DIR="${CLAUDE_CAPTURE_DIR:-${REPO_ROOT}/.reference}"
CAPTURE_TAG="${CLAUDE_CAPTURE_TAG:-$(date +%Y%m%d)}"
# mitmproxy matches --allow-hosts against the host:port form, so the port is
# optional in the pattern rather than absent from it.
ALLOW_HOSTS='^api\.anthropic\.com(:\d+)?$'

DUMP_OUT="${MITM_DUMP_OUT:-${CAPTURE_DIR}/claude-code-headers-${CAPTURE_TAG}.jsonl}"
META_OUT="${CAPTURE_DIR}/claude-code-headers-${CAPTURE_TAG}.meta.txt"

MODE="${1:-}"
case "${MODE}" in
  oauth|apikey|token|check) ;;
  *)
    echo "usage: $0 {oauth|apikey|token|check}" >&2
    exit 2
    ;;
esac

require_tool() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "ERROR: $1 is not installed.${2:+ $2}" >&2
    exit 1
  }
}

echo "=== [1/5] Validating prerequisites ==="
require_tool podman
require_tool jq
require_tool mitmdump "Install it with: pipx install mitmproxy"

if [[ ! -f "${CONTAINER_DIR}/Containerfile" ]]; then
  echo "ERROR: no Containerfile at ${CONTAINER_DIR}. Set CLAUDE_CONTAINER_DIR." >&2
  exit 1
fi

podman machine info >/dev/null 2>&1 || {
  echo "ERROR: Podman machine is not running. Run: podman machine start" >&2
  exit 1
}

if ! podman image exists "${IMAGE_NAME}"; then
  echo "ERROR: image ${IMAGE_NAME} not built yet." >&2
  echo "ERROR: build it: podman build -t ${IMAGE_NAME} -f ${CONTAINER_DIR}/Containerfile ${CONTAINER_DIR}" >&2
  exit 1
fi

mkdir -p "${CAPTURE_DIR}" "${MITM_CONFDIR}"

# The pinned version is the whole point of the artifact: a capture that does
# not name its client cannot be implemented against.
echo "=== [2/5] Recording the capture client version ==="
CLIENT_VERSION="$(podman run --rm "${IMAGE_NAME}" claude --version 2>&1 | tr -d '\r')" || {
  echo "ERROR: could not read claude --version from ${IMAGE_NAME}." >&2
  exit 1
}
echo "  ${CLIENT_VERSION}"

if [[ "${MODE}" == "check" ]]; then
  echo "=== check only: prerequisites satisfied, nothing captured ==="
  exit 0
fi

if [[ "${MODE}" == "token" ]]; then
  echo "=== Minting an OAuth token in a throwaway container ==="
  echo "Follow the printed URL, then paste the code back into the prompt."
  echo "Export the resulting token as CLAUDE_CODE_OAUTH_TOKEN. It is not"
  echo "written to any artifact by this script."
  echo
  exec podman run --rm -it \
    --add-host=containers.internal:host-gateway \
    "${IMAGE_NAME}" \
    claude setup-token
fi

# Credentials are read here and handed to the container through the environment.
# Nothing below prints them.
case "${MODE}" in
  oauth)
    if [[ -z "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]]; then
      echo "ERROR: CLAUDE_CODE_OAUTH_TOKEN is unset." >&2
      echo "ERROR: run '$0 token' first, then export the token it prints." >&2
      exit 1
    fi
    AUTH_ENV=(-e "CLAUDE_CODE_OAUTH_TOKEN=${CLAUDE_CODE_OAUTH_TOKEN}")
    ;;
  apikey)
    if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
      echo "ERROR: ANTHROPIC_API_KEY is unset." >&2
      exit 1
    fi
    AUTH_ENV=(-e "ANTHROPIC_API_KEY=${ANTHROPIC_API_KEY}")
    ;;
esac

echo "=== [3/5] Starting mitmdump on port ${MITM_PORT} ==="
MITM_LOG="/tmp/claude-capture-mitmdump.log"
: > "${MITM_LOG}"

mitmdump \
  --quiet \
  --listen-host 0.0.0.0 \
  --listen-port "${MITM_PORT}" \
  --allow-hosts "${ALLOW_HOSTS}" \
  --set "confdir=${MITM_CONFDIR}" \
  -s "${REPO_ROOT}/scripts/mitm_header_dump.py" \
  >"${MITM_LOG}" 2>&1 &
MITM_PID=$!

cleanup() {
  if kill -0 "${MITM_PID}" 2>/dev/null; then
    kill "${MITM_PID}" 2>/dev/null || true
    wait "${MITM_PID}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# mitmdump generates its CA on first start. Waiting for the file is the only
# reliable readiness signal: an empty capture because the CA was absent is the
# failure mode this wait exists to prevent.
CA_FILE="${MITM_CONFDIR}/mitmproxy-ca-cert.pem"
for _ in $(seq 1 50); do
  [[ -f "${CA_FILE}" ]] && break
  sleep 0.2
done
if [[ ! -f "${CA_FILE}" ]]; then
  echo "ERROR: mitmproxy CA never appeared at ${CA_FILE}. Log:" >&2
  tail -20 "${MITM_LOG}" >&2
  exit 1
fi

if ! nc -z 127.0.0.1 "${MITM_PORT}" >/dev/null 2>&1; then
  echo "ERROR: mitmdump is not listening on ${MITM_PORT}. Log:" >&2
  tail -20 "${MITM_LOG}" >&2
  exit 1
fi

echo "=== [4/5] Running Claude Code through the interceptor ==="
echo "Capture: ${DUMP_OUT}"
echo "Auth mode: ${MODE}"
echo
echo "Two prompts run: a trivial one for the plain messages shape, and one"
echo "that drives a tool call for the security-monitor shape. If the monitor"
echo "request does not appear, record that outcome rather than forcing it."
echo

RUN_STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

podman run --rm -it \
  --add-host=containers.internal:host-gateway \
  -e "HTTPS_PROXY=http://host.containers.internal:${MITM_PORT}" \
  -e "https_proxy=http://host.containers.internal:${MITM_PORT}" \
  -e CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
  -e DISABLE_AUTOUPDATER=1 \
  -e DISABLE_TELEMETRY=1 \
  -e "NODE_EXTRA_CA_CERTS=/certs/mitmproxy-ca-cert.pem" \
  -e "SSL_CERT_FILE=/certs/mitmproxy-ca-cert.pem" \
  -v "${MITM_CONFDIR}:/certs:ro" \
  "${AUTH_ENV[@]}" \
  "${IMAGE_NAME}" \
  claude --print 'Reply with exactly CLAUDE_CAPTURE_OK'

podman run --rm -it \
  --add-host=containers.internal:host-gateway \
  -e "HTTPS_PROXY=http://host.containers.internal:${MITM_PORT}" \
  -e "https_proxy=http://host.containers.internal:${MITM_PORT}" \
  -e CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
  -e DISABLE_AUTOUPDATER=1 \
  -e DISABLE_TELEMETRY=1 \
  -e "NODE_EXTRA_CA_CERTS=/certs/mitmproxy-ca-cert.pem" \
  -e "SSL_CERT_FILE=/certs/mitmproxy-ca-cert.pem" \
  -v "${MITM_CONFDIR}:/certs:ro" \
  "${AUTH_ENV[@]}" \
  "${IMAGE_NAME}" \
  claude --print --permission-mode=acceptEdits \
    'Run `ls -la` with the Bash tool and tell me what you see.'

echo "=== [5/5] Result ==="
HOSTS_SEEN=$(jq -r '.host' "${DUMP_OUT}" 2>/dev/null | sort -u | tr '\n' ' ' || true)
LINES=$(wc -l < "${DUMP_OUT}" 2>/dev/null | tr -d ' ' || echo 0)

cat > "${META_OUT}" <<EOF
Claude Code wire capture — ${MODE} auth
========================================

Captured: ${RUN_STARTED_AT}
Client:   ${CLIENT_VERSION}
Image:    ${IMAGE_NAME}
Script:   scripts/capture-claude-code-headers.sh ${MODE}
Hosts:    ${HOSTS_SEEN}
Records:  ${LINES}
JSONL:    $(basename "${DUMP_OUT}")

Headline requests observed (path only; bodies are never dumped):

$(jq -r '"  \(.method) \(.host)\(.path) -> \(.status // "-")"' "${DUMP_OUT}" 2>/dev/null | sort | uniq -c | sed 's/^/ /' || echo "  (none)")

Provenance note: every value in this capture is a verbatim quote from observed
traffic. Credential values are redacted before writing. Anything not confirmed
byte-for-byte is marked unknown rather than inferred.
EOF

echo "Records written: ${LINES}"
echo "Hosts seen:      ${HOSTS_SEEN:-none}"
echo "Baseline meta:   ${META_OUT}"
if [[ "${LINES}" -eq 0 ]]; then
  echo "WARNING: no records captured. Check ${MITM_LOG} and that the container" >&2
  echo "WARNING: reached host.containers.internal:${MITM_PORT}." >&2
  exit 1
fi