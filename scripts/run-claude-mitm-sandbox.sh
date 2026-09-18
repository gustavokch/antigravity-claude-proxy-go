#!/usr/bin/env bash
#
# Run containerized Claude Code against the host antigravity-proxy so the
# proxy records live security-monitor (classifier) request bodies.
#
# The container reaches the host through Podman's host-gateway alias. The
# host's own ~/.claude is deliberately NOT mounted: a captured session must
# not be able to read or corrupt the operator's real Claude Code state.
set -euo pipefail

CONTAINER_DIR="${CLAUDE_CONTAINER_DIR:-${HOME}/Git/claude-container}"
IMAGE_NAME="claude-box:mitm"
PROXY_PORT="${PROXY_PORT:-8080}"
TARGET_HOST_URL="http://containers.internal:${PROXY_PORT}"

echo "=== [1/4] Validating prerequisites ==="

for tool in podman jq; do
  if ! command -v "${tool}" >/dev/null 2>&1; then
    echo "ERROR: ${tool} is not installed." >&2
    exit 1
  fi
done

if [[ ! -f "${CONTAINER_DIR}/Containerfile" ]]; then
  echo "ERROR: No Containerfile at ${CONTAINER_DIR}. Set CLAUDE_CONTAINER_DIR." >&2
  exit 1
fi

if ! podman machine info >/dev/null 2>&1; then
  echo "ERROR: Podman machine is not running. Run: podman machine start" >&2
  exit 1
fi

# nc -z is the portable listener probe on macOS; a missing listener is a
# warning, not an error, because the operator may start the proxy after this.
if ! nc -z localhost "${PROXY_PORT}" >/dev/null 2>&1; then
  echo "WARNING: nothing is listening on localhost:${PROXY_PORT}." >&2
  echo "WARNING: start antigravity-proxy before sending a prompt." >&2
fi

if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
  CONFIG_JSON="${CONFIG_JSON:-${HOME}/.config/antigravity-proxy/config.json}"
  if [[ -f "${CONFIG_JSON}" ]]; then
    ANTHROPIC_API_KEY=$(jq -r '.apiKey // empty' "${CONFIG_JSON}" 2>/dev/null || true)
  fi
fi

if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
  echo "ERROR: ANTHROPIC_API_KEY not set and no apiKey in config.json." >&2
  echo "ERROR: export ANTHROPIC_API_KEY before running this script." >&2
  exit 1
fi

echo "=== [2/4] Building container image ==="
podman build -t "${IMAGE_NAME}" -f "${CONTAINER_DIR}/Containerfile" "${CONTAINER_DIR}"

echo "=== [3/4] Creating throwaway workspace ==="
HOST_WORKSPACE=$(mktemp -d -t claude-mitm-workspace)
trap 'rm -rf "${HOST_WORKSPACE}"' EXIT
# Give the session something to reason about, so a bash-tool call — and
# therefore a classifier request — is reachable in one prompt.
printf 'placeholder file for the capture session\n' > "${HOST_WORKSPACE}/README.md"

echo "=== [4/4] Starting capture session ==="
echo "Container proxy target: ${TARGET_HOST_URL}"
echo "Workspace (discarded on exit): ${HOST_WORKSPACE}"
echo
echo "Inside the session, ask Claude to run a shell command (for example:"
echo "  'run ls -la and tell me what you see')."
echo "Approving the bash tool call is what triggers the security-monitor"
echo "request this harness exists to capture."
echo

podman run --rm -it \
  --name claude-mitm-session \
  --replace \
  --add-host=containers.internal:host-gateway \
  -e ANTHROPIC_BASE_URL="${TARGET_HOST_URL}" \
  -e ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY}" \
  -e CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
  -e DISABLE_AUTOUPDATER=1 \
  -e DISABLE_TELEMETRY=1 \
  -v "${HOST_WORKSPACE}:/workspace:z" \
  -w /workspace \
  "${IMAGE_NAME}" \
  claude
EXIT_STATUS=$?
exit "${EXIT_STATUS}"
