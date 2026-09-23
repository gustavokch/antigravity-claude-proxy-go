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
#   scripts/capture-claude-code-headers.sh oauth        # capture with an OAuth token
#   scripts/capture-claude-code-headers.sh apikey       # capture with an API key
#   scripts/capture-claude-code-headers.sh interactive  # capture cc_entrypoint=cli
#   scripts/capture-claude-code-headers.sh token        # mint an OAuth token for the above
#   scripts/capture-claude-code-headers.sh check        # verify prerequisites only
#
# `interactive` exists because cc_entrypoint differs by session kind: a --print
# run sends sdk-cli, and docs/classifier-fallback-notes.md records cli from an
# interactive session. Setting CLAUDE_CODE_ENTRYPOINT does NOT reproduce it —
# verified 2026-09-23, where forcing that variable on a --print run still left
# cc_entrypoint=sdk-cli and cc_turn_origin=sdk in every record.
#
# UNVERIFIED: that a container TTY is sufficient to produce cli. Three automated
# attempts on 2026-09-23 produced GET requests and no POST /v1/messages at all,
# because first-run key-press gates (theme, folder trust) block the prompt. The
# cli header set is still uncaptured; `interactive` therefore needs a human.
#
# Credentials come from CLAUDE_CODE_OAUTH_TOKEN / ANTHROPIC_API_KEY, or from a
# file named by CLAUDE_CODE_OAUTH_TOKEN_FILE / ANTHROPIC_API_KEY_FILE. Prefer the
# file form: a token on a command line lands in shell history and in any tool-call
# log, and the file form lets the operator create the credential without pasting
# it anywhere that records it. A file is read with newlines stripped and is never
# deleted by this script.
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
  oauth|apikey|interactive|token|check) ;;
  *)
    echo "usage: $0 {oauth|apikey|interactive|token|check}" >&2
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

if [[ "${MODE}" == "token" ]]; then
  echo "=== Minting an OAuth token in a throwaway container ==="
  echo "Follow the printed URL, then paste the code back into the prompt."
  echo "Save the resulting token to a 0600 file and pass its path as"
  echo "CLAUDE_CODE_OAUTH_TOKEN_FILE, or export it as CLAUDE_CODE_OAUTH_TOKEN."
  echo "It is not written to any artifact by this script."
  echo
  exec podman run --rm -it \
    --add-host=containers.internal:host-gateway \
    "${IMAGE_NAME}" \
    claude setup-token
fi

# Credentials are read here and handed to the container through the environment.
# Nothing below prints them.
#
# Each credential may also arrive through a file path (*_FILE), because an
# interactive shell that sets the variable and the process that needs it are
# often different shells — and passing a live token on a command line puts it
# into shell history and any tool-call log. A file also lets the operator create
# the credential without pasting it anywhere that records it.
read_secret() {
  # read_secret <ENV_NAME> [DEFAULT_FILE]; echoes the value or nothing.
  local name="$1" default_file="${2:-}" file_var="${1}_FILE" value file
  value="${!name:-}"
  file="${!file_var:-}"
  if [[ -n "${value}" ]]; then
    printf '%s' "${value}"
    return
  fi
  # Default path, used only when both the variable and its _FILE are unset. It
  # exists so `check` inspects the obvious file instead of silently reporting
  # absent because the operator did not repeat the path on the command line.
  if [[ -z "${file}" && -n "${default_file}" && -f "${default_file}" ]]; then
    file="${default_file}"
  fi
  if [[ -n "${file}" ]]; then
    if [[ ! -f "${file}" ]]; then
      # Report and return empty rather than exiting: the caller's empty-check
      # already fails closed, and check mode must be able to report a broken
      # path without aborting.
      echo "ERROR: ${file_var}=${file} does not exist." >&2
      return
    fi
    # Trailing newlines are the common accident when a secret is written with
    # an editor; a token with a newline in it is a 401 that looks like a bad
    # credential.
    printf '%s' "$(tr -d '\r\n' < "${file}")"
  fi
}

# A real credential is far longer than this. The floor exists because the most
# likely mistake is writing a placeholder or a truncated paste into the file,
# and a 401 three minutes into a container run hides that cause.
MIN_SECRET_CHARS=20

require_secret() {
  # require_secret <ENV_NAME> <LABEL> <DEFAULT_FILE>; echoes the value or exits 1.
  local name="$1" label="$2" default_file="$3" value file_var="${1}_FILE"
  value="$(read_secret "${name}" "${default_file}")"
  if [[ -z "${value}" ]]; then
    echo "ERROR: no ${label}. Set ${name}, or point ${name}_FILE at a file holding it." >&2
    # Only mention the default when nothing explicit was given: otherwise the
    # hint names a file the operator did not ask for.
    if [[ -n "${default_file}" && -z "${!name:-}" && -z "${!file_var:-}" ]]; then
      if [[ -f "${default_file}" ]]; then
        echo "ERROR: the default path ${default_file} exists but was not used." >&2
      else
        echo "ERROR: the default path ${default_file} is not present either." >&2
      fi
    fi
    exit 1
  fi
  if (( ${#value} < MIN_SECRET_CHARS )); then
    echo "ERROR: the ${label} is only ${#value} characters, which is too short to be" >&2
    echo "ERROR: a real credential. A placeholder or truncated paste is the usual cause." >&2
    exit 1
  fi
  printf '%s' "${value}"
}

# Empty default so `check` — which needs no credential and exits before any
# container runs — can still expand these under `set -u` on bash 3.2, where an
# unset array is an unbound variable rather than an empty list.
AUTH_ENV=()

case "${MODE}" in
  oauth|interactive)
    SECRET_NAME=CLAUDE_CODE_OAUTH_TOKEN
    SECRET_LABEL='OAuth token'
    SECRET_DEFAULT="${HOME}/.claude-oat"
    ;;
  apikey)
    SECRET_NAME=ANTHROPIC_API_KEY
    SECRET_LABEL='API key'
    SECRET_DEFAULT="${HOME}/.claude-key"
    ;;
esac

# The credential reaches the container through a 0600 env-file, never through
# `-e NAME=value`.
#
# An -e argument puts the live token in the container process's argv, where any
# local user reads it out of `ps` for as long as the run lasts, and where every
# tool-call log that records the command keeps a copy. That is the same exposure
# this script's own credential notes warn about for shell history, so the
# non-interactive modes should not reintroduce it — they were the modes the
# capture procedure actually tells the operator to run. Only the path appears in
# argv now.
if [[ -n "${SECRET_NAME:-}" ]]; then
  # Assigned first, not substituted straight into printf. require_secret exits 1
  # on a missing or too-short credential, and a command substitution inside a
  # simple command discards that status: printf would succeed, write
  # "NAME=" to the env-file, and the run would reach the container with an empty
  # credential. An assignment propagates the failure, so `set -e` aborts here.
  SECRET_VALUE="$(require_secret "${SECRET_NAME}" "${SECRET_LABEL}" "${SECRET_DEFAULT}")"
  AUTH_ENV_FILE="$(mktemp -t claude-capture-env)"
  chmod 600 "${AUTH_ENV_FILE}"
  printf '%s=%s\n' "${SECRET_NAME}" "${SECRET_VALUE}" > "${AUTH_ENV_FILE}"
  unset SECRET_VALUE
  AUTH_ENV=(--env-file "${AUTH_ENV_FILE}")
fi

# Kept as a separate name because the interactive branch runs under expect, which
# echoes the command it spawns: an -e NAME=value argument there would print the
# credential to the terminal and into the expect log. Both forms are the env-file
# now, so they are the same arguments.
INTERACTIVE_AUTH_ARGS=("${AUTH_ENV[@]}")

if [[ "${MODE}" == "check" ]]; then
  # Presence only, never the value: this is the fastest way to find out that a
  # token file is pointed at the wrong path, holds a placeholder, or is carrying
  # a trailing newline.
  oauth_chars=$(read_secret CLAUDE_CODE_OAUTH_TOKEN "${HOME}/.claude-oat" | wc -c | tr -d ' ')
  key_chars=$(read_secret ANTHROPIC_API_KEY "${HOME}/.claude-key" | wc -c | tr -d ' ')
  echo "=== check only: prerequisites satisfied, nothing captured ==="
  echo "  oauth token: ${oauth_chars} chars (need >= ${MIN_SECRET_CHARS}; 0 = absent)"
  echo "  api key:     ${key_chars} chars (need >= ${MIN_SECRET_CHARS}; 0 = absent)"
  exit 0
fi

echo "=== [3/5] Starting mitmdump on port ${MITM_PORT} ==="
MITM_LOG="/tmp/claude-capture-mitmdump.log"
: > "${MITM_LOG}"

# The port must be ours. A stale mitmdump left by an earlier attempt (the
# interactive watchdog kills its wrapper, not the proxy) still answers the
# readiness probe below, so this run's own mitmdump fails to bind and writes
# nothing while every check passes. That produced a silent zero-record capture
# on 2026-09-23; refusing to start is the fix.
if nc -z 127.0.0.1 "${MITM_PORT}" >/dev/null 2>&1; then
  echo "ERROR: something is already listening on ${MITM_PORT}." >&2
  echo "ERROR: a stale mitmdump from an earlier run is the usual cause. Free it with:" >&2
  echo "ERROR:   pkill -f mitm_header_dump.py" >&2
  echo "ERROR: or choose another port with ANTIGRAVITY_MITM_PORT." >&2
  exit 1
fi

# The addon's host filter defaults to the Cloud Code hosts. Without
# MITM_DUMP_HOSTS it drops every api.anthropic.com flow before it can write a
# record, which looks exactly like the container never using the proxy.
# MITM_DUMP_REQUEST_BODY turns on the body fingerprint; both must be set here,
# because the addon reads its configuration once at startup.
#
# mitmdump is deliberately NOT run with --quiet: its flow log is the only
# evidence that separates "the addon filtered everything" from "the container
# ignored HTTPS_PROXY", and those need different fixes.
MITM_DUMP_HOSTS="${MITM_DUMP_HOSTS:-api.anthropic.com}" \
MITM_DUMP_REQUEST_BODY=1 \
MITM_DUMP_OUT="${DUMP_OUT}" \
mitmdump \
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
  # Interactive mode seeds an onboarding file here. It is cleaned in this
  # function rather than by a second `trap ... EXIT`, because a second EXIT
  # trap REPLACES this one and would silently stop mitmdump from being killed.
  if [[ -n "${ONBOARD_DIR:-}" ]]; then
    rm -rf "${ONBOARD_DIR}"
  fi
  if [[ -n "${AUTH_ENV_FILE:-}" ]]; then
    rm -f "${AUTH_ENV_FILE}"
  fi
  # An EXIT trap's last command decides the script's exit status, overriding even
  # an explicit `exit 0`. Written as `[[ -n "${VAR:-}" ]] && rm ...`, the list
  # returned 1 whenever the variable was unset — which is every non-interactive
  # mode — so a completely successful capture exited 1. `if` blocks plus this
  # explicit return keep the body's status.
  return 0
}
# EXIT alone is not enough: a killed or interrupted script leaves mitmdump
# holding the port, and the next run then fails to bind while every check passes.
trap cleanup EXIT INT TERM

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
  # A single probe races mitmdump's bind whenever the CA already exists from a
  # previous run, because the CA wait above returns immediately. Poll instead,
  # and fail on a dead process rather than waiting out the whole timeout.
  for _ in $(seq 1 50); do
    if ! kill -0 "${MITM_PID}" 2>/dev/null; then
      echo "ERROR: mitmdump exited on startup. Log:" >&2
      tail -20 "${MITM_LOG}" >&2
      exit 1
    fi
    nc -z 127.0.0.1 "${MITM_PORT}" >/dev/null 2>&1 && break
    sleep 0.2
  done
fi

if ! nc -z 127.0.0.1 "${MITM_PORT}" >/dev/null 2>&1; then
  echo "ERROR: mitmdump is not listening on ${MITM_PORT} after 10s. Log:" >&2
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

# Interactive mode is for capturing cc_entrypoint=cli, which a --print run
# cannot produce. It hands the terminal to a human on purpose; see the comment
# inside the branch for why automation was abandoned.
if [[ "${MODE}" == "interactive" ]]; then
  echo "Interactive capture: this mode needs an operator at the keyboard."
  echo
  echo "A first-run session is gated twice — a theme picker, then a folder-trust"
  echo "dialog — before it accepts a prompt, so it emits only GETs until both are"
  echo "answered. Piped stdin cannot answer either: three automated attempts on"
  echo "2026-09-23 (bare -ti, a script(1) PTY wrapper, and a pre-seeded onboarding"
  echo "flag) each produced GET requests and no POST /v1/messages. The gates are"
  echo "key-press dialogs, not line input."
  echo
  echo "In the session: type a prompt, press Enter, then /exit to end it."
  echo

  # Skips the theme gate. It does not skip the folder-trust dialog, which is
  # why a human is still required.
  ONBOARD_DIR="$(mktemp -d -t claude-onboard)"
  printf '%s' '{"hasCompletedOnboarding":true,"theme":"dark","installMethod":"unknown"}' \
    > "${ONBOARD_DIR}/.claude.json"

  podman rm -f claude-mitm-interactive >/dev/null 2>&1 || true

  # expect, not piped stdin: see the driver's own header for why. The podman
  # argv is passed through as-is so this script stays the single source of the
  # container's environment.
  if ! command -v expect >/dev/null 2>&1; then
    echo "ERROR: expect is not installed; interactive capture cannot be driven." >&2
    echo "ERROR: macOS ships it at /usr/bin/expect." >&2
    exit 1
  fi

  CLAUDE_INTERACTIVE_LOG="${CLAUDE_INTERACTIVE_LOG:-/tmp/claude-interactive-tui.log}"
  : > "${CLAUDE_INTERACTIVE_LOG}"
  export CLAUDE_INTERACTIVE_LOG
  export CLAUDE_INTERACTIVE_PROMPT="${CLAUDE_INTERACTIVE_PROMPT:-Reply with exactly CLAUDE_INTERACTIVE_OK}"

  # stdin must not reach EOF: in a non-interactive shell expect's stdin closes at
  # once, and podman then fails mid-session with "Failed to write input to
  # service: EOF". /dev/zero never EOFs and costs one descriptor. A `sleep |`
  # holder was tried first and was worse, because the pipeline then outlives
  # expect by the whole sleep duration and every run looked hung.
  expect "${REPO_ROOT}/scripts/capture-claude-code-interactive.exp" \
    </dev/zero \
    podman run --rm -it \
    --name claude-mitm-interactive \
    --add-host=containers.internal:host-gateway \
    -e "HTTPS_PROXY=http://host.containers.internal:${MITM_PORT}" \
    -e "https_proxy=http://host.containers.internal:${MITM_PORT}" \
    -e CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
    -e DISABLE_AUTOUPDATER=1 \
    -e DISABLE_TELEMETRY=1 \
    -e "NODE_EXTRA_CA_CERTS=/certs/mitmproxy-ca-cert.pem" \
    -e "SSL_CERT_FILE=/certs/mitmproxy-ca-cert.pem" \
    -v "${MITM_CONFDIR}:/certs:ro" \
    -v "${ONBOARD_DIR}/.claude.json:/root/.claude.json:ro" \
    "${INTERACTIVE_AUTH_ARGS[@]}" \
    "${IMAGE_NAME}" \
    claude || true
  podman rm -f claude-mitm-interactive >/dev/null 2>&1 || true
    claude || true
else
  podman run --rm \
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

  podman run --rm \
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
fi

echo "=== [5/5] Result ==="
if [[ -f "${DUMP_OUT}" ]]; then
  HOSTS_SEEN=$(jq -r '.host' "${DUMP_OUT}" 2>/dev/null | sort -u | tr '\n' ' ')
  LINES=$(wc -l < "${DUMP_OUT}" | tr -d ' ')
else
  # The addon never opened the file, so nothing it saw matched. Distinguishing
  # that from "the container bypassed the proxy" needs the mitmdump flow log.
  HOSTS_SEEN=""
  LINES=0
fi

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
  echo "WARNING: no records captured. The mitmdump flow log is the diagnostic:" >&2
  echo "WARNING:   flows to api.anthropic.com present -> the addon's filter is wrong" >&2
  echo "WARNING:   no flows at all                     -> the container bypassed HTTPS_PROXY" >&2
  echo "WARNING: log: ${MITM_LOG}" >&2
  tail -20 "${MITM_LOG}" >&2
  exit 1
fi