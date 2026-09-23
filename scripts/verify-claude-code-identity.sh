#!/usr/bin/env bash
set -euo pipefail

# Claude Code wire-identity drift gate.
#
# Boots the proxy behind mitmdump, sends one foreign-headed request through the
# pooled Claude Code gateway, and diffs the headers the proxy actually put on
# the wire against the committed capture at
# .reference/claude-code-headers-20260923.jsonl.
#
# What this gate detects: the PROXY drifting away from the captured baseline —
# a refactor that drops a header, a canonicalisation bug that changes a name's
# case, an auth change that reorders anthropic-beta.
#
# What it does NOT detect: CLAUDE CODE drifting away from the baseline. A new
# Claude Code version changing its own fingerprint is invisible here, because
# both sides of this diff are frozen. Detecting that needs a fresh capture:
#   scripts/capture-claude-code-headers.sh oauth
#
# TLS is why this uses mitmdump rather than tcpdump alone, as scripts/verify-ja4.sh
# does: JA4 reads the plaintext ClientHello, but request headers are inside the
# encrypted record layer and a passive capture cannot read them. mitmdump
# terminates TLS, so the headers become observable. The skip-loudly discipline is
# taken from verify-ja4.sh unchanged.
#
# No credential is needed. The gate configures a STUB account: the upstream
# rejects it, but mitmdump records the request before the rejection, and the
# request is the whole subject of this test.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BASELINE="${REPO_ROOT}/.reference/claude-code-headers-20260923.jsonl"

MITM_PORT="${ANTIGRAVITY_CC_IDENTITY_MITM_PORT:-18081}"
PROXY_PORT="${ANTIGRAVITY_CC_IDENTITY_PROXY_PORT:-18099}"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/cc-identity-gate.XXXXXX")"
MITM_CONFDIR="${WORK_DIR}/mitm"
CONFIG_DIR="${WORK_DIR}/config"
OBSERVED="${WORK_DIR}/observed.jsonl"
MITM_LOG="${WORK_DIR}/mitmdump.log"
PROXY_LOG="${WORK_DIR}/proxy.log"

# The request the gate drives. Any allowlisted Claude model works; haiku is the
# cheapest if the stub is ever replaced by a live account.
GATE_MODEL="claude-haiku-4-5"

skip() {
  echo "SKIPPED: $1" >&2
  echo "Claude Code identity gate did not run — this is NOT a pass. Rerun where mitmproxy is available to get real assurance." >&2
  exit 0
}

fail() {
  echo "FAIL: $1" >&2
  exit 1
}

if [[ "${ANTIGRAVITY_SKIP_CC_IDENTITY_GATE:-0}" == "1" ]]; then
  skip "ANTIGRAVITY_SKIP_CC_IDENTITY_GATE=1"
fi
for tool in mitmdump jq go nc python3; do
  command -v "${tool}" >/dev/null 2>&1 || skip "${tool} not found in PATH"
done
[[ -f "${BASELINE}" ]] || skip "baseline capture missing at ${BASELINE}"

cleanup() {
  if [[ -n "${PROXY_PID:-}" ]] && kill -0 "${PROXY_PID}" 2>/dev/null; then
    kill "${PROXY_PID}" 2>/dev/null || true
    wait "${PROXY_PID}" 2>/dev/null || true
  fi
  if [[ -n "${MITM_PID:-}" ]] && kill -0 "${MITM_PID}" 2>/dev/null; then
    kill "${MITM_PID}" 2>/dev/null || true
    wait "${MITM_PID}" 2>/dev/null || true
  fi
  # Keep the work directory when the gate failed, so the observed record can be
  # read; the failure message prints the path.
  if [[ "${KEEP_WORK_DIR:-0}" != "1" ]]; then
    rm -rf "${WORK_DIR}"
  fi
}
# EXIT alone is not enough: an interrupted run leaves mitmdump holding the port,
# and the next run then fails to bind while every readiness check still passes.
trap cleanup EXIT INT TERM

echo "=== [1/6] Building the proxy ==="
mkdir -p "${REPO_ROOT}/bin"
(cd "${REPO_ROOT}" && go build -o bin/antigravity-proxy ./cmd/proxy)

echo "=== [2/6] Writing a stub configuration ==="
mkdir -p "${CONFIG_DIR}" "${MITM_CONFDIR}"
# gatewayOrder is narrowed to claudecode alone so the request cannot be answered
# by another gateway and leave this gate asserting nothing.
cat > "${CONFIG_DIR}/config.json" <<JSON
{
  "gatewayOrder": { "byModel": {}, "order": ["claudecode"] },
  "claudecode": {
    "enabled": true,
    "baseUrl": "https://api.anthropic.com",
    "accounts": [
      {
        "id": "cc-identity-gate-stub",
        "name": "identity gate stub",
        "email": "stub@example.invalid",
        "type": "oauth",
        "source": "oauth",
        "enabled": true,
        "priority": 1,
        "accountUuid": "00000000-0000-4000-8000-000000000000",
        "token": "sk-ant-oat01-gate-stub-credential-not-valid-upstream",
        "refreshToken": "sk-ant-ort01-gate-stub-credential-not-valid-upstream",
        "expiresAt": "2099-01-01T00:00:00Z"
      }
    ]
  }
}
JSON
chmod 600 "${CONFIG_DIR}/config.json"

echo "=== [3/6] Starting mitmdump on port ${MITM_PORT} ==="
if nc -z 127.0.0.1 "${MITM_PORT}" >/dev/null 2>&1; then
  fail "something is already listening on ${MITM_PORT}; free it or set ANTIGRAVITY_CC_IDENTITY_MITM_PORT"
fi

# The addon's host filter defaults to the Cloud Code hosts. Without
# MITM_DUMP_HOSTS every api.anthropic.com flow is dropped before a record can be
# written, which looks exactly like the proxy ignoring HTTPS_PROXY.
MITM_DUMP_HOSTS="api.anthropic.com" \
MITM_DUMP_REQUEST_BODY=1 \
MITM_DUMP_OUT="${OBSERVED}" \
mitmdump \
  --listen-host 127.0.0.1 \
  --listen-port "${MITM_PORT}" \
  --allow-hosts '^api\.anthropic\.com(:[0-9]+)?$' \
  --set "confdir=${MITM_CONFDIR}" \
  -s "${REPO_ROOT}/scripts/mitm_header_dump.py" \
  >"${MITM_LOG}" 2>&1 &
MITM_PID=$!

# mitmdump generates its CA on first start. Waiting for the file is the only
# reliable readiness signal; an empty capture because the CA was absent is the
# failure mode this wait exists to prevent.
CA_FILE="${MITM_CONFDIR}/mitmproxy-ca-cert.pem"
for _ in $(seq 1 100); do
  [[ -f "${CA_FILE}" ]] && break
  kill -0 "${MITM_PID}" 2>/dev/null || break
  sleep 0.2
done
if [[ ! -f "${CA_FILE}" ]]; then
  tail -20 "${MITM_LOG}" >&2 || true
  fail "mitmproxy CA never appeared at ${CA_FILE}"
fi
# A single probe races mitmdump's bind whenever the CA already exists, because
# the wait above returns immediately. Poll, and fail fast on a dead process.
for _ in $(seq 1 100); do
  nc -z 127.0.0.1 "${MITM_PORT}" >/dev/null 2>&1 && break
  if ! kill -0 "${MITM_PID}" 2>/dev/null; then
    tail -20 "${MITM_LOG}" >&2 || true
    fail "mitmdump exited on startup"
  fi
  sleep 0.2
done
nc -z 127.0.0.1 "${MITM_PORT}" >/dev/null 2>&1 \
  || fail "mitmdump is not listening on ${MITM_PORT} after 20s"

echo "=== [4/6] Starting the proxy on port ${PROXY_PORT} ==="
# The claudecode client is built with a nil *http.Client, so it uses
# http.DefaultTransport, which honours HTTPS_PROXY via ProxyFromEnvironment.
# SSL_CERT_FILE makes Go trust the mitmproxy CA for this process only.
ANTIGRAVITY_CONFIG_DIR="${CONFIG_DIR}" \
HTTPS_PROXY="http://127.0.0.1:${MITM_PORT}" \
https_proxy="http://127.0.0.1:${MITM_PORT}" \
SSL_CERT_FILE="${CA_FILE}" \
"${REPO_ROOT}/bin/antigravity-proxy" --port "${PROXY_PORT}" \
  >"${PROXY_LOG}" 2>&1 &
PROXY_PID=$!

for _ in $(seq 1 100); do
  nc -z 127.0.0.1 "${PROXY_PORT}" >/dev/null 2>&1 && break
  if ! kill -0 "${PROXY_PID}" 2>/dev/null; then
    tail -20 "${PROXY_LOG}" >&2 || true
    fail "proxy exited on startup"
  fi
  sleep 0.2
done
nc -z 127.0.0.1 "${PROXY_PORT}" >/dev/null 2>&1 \
  || fail "proxy is not listening on ${PROXY_PORT} after 20s"

echo "=== [5/6] Driving one foreign-headed request ==="
# Deliberately hostile input: a foreign User-Agent, a foreign x-app, and a beta
# the capture does not contain. Normalization has to overwrite all three. The
# response is ignored — the stub credential is rejected upstream, and the
# request mitmdump recorded on the way out is the subject of this test.
#
# ONE request, and it has to be Anthropic-shaped. Two reasons, and the first is a
# hard constraint:
#
#   - The gate gets exactly one upstream attempt. The stub credential draws a 401,
#     the pool cools the only account down, and every later request is answered
#     503 locally with nothing sent. A second driven request cannot reach the wire.
#   - The system BLOCK ARRAY is what makes this test mean anything. Normalization
#     marks system block 0, and it used to do that by OVERWRITING it, deleting the
#     caller's prompt whenever it arrived as one text block. Only an
#     Anthropic-shaped body reaches that branch: translateOpenAIRequest builds the
#     Anthropic system from `messages` entries with role system or developer and
#     emits it as a STRING, so an OpenAI-shaped request always lands in the string
#     branch, which was never destructive. A top-level `system` on an OpenAI
#     request is dropped in translation, as it should be, and then ApplyBody sees
#     no system at all.
#
# temperature rides along because it is in the omit list: Claude Code does not
# send it, so it must not reach the upstream. That was the OpenAI-shaped request's
# other job, and the Anthropic schema carries the field too, so nothing is lost by
# folding the two into one.
curl -sS -o /dev/null --max-time 60 \
  -X POST "http://127.0.0.1:${PROXY_PORT}/v1/messages" \
  -H 'Content-Type: application/json' \
  -H 'User-Agent: foreign-harness/1.0' \
  -H 'x-app: foreign-app' \
  -H 'anthropic-beta: foreign-beta-not-in-baseline' \
  -d "{\"model\":\"${GATE_MODEL}\",\"max_tokens\":16,\"temperature\":0.7,\"system\":[{\"type\":\"text\",\"text\":\"caller system prompt the proxy must not delete\"}],\"messages\":[{\"role\":\"user\",\"content\":\"ok\"}]}" \
  >/dev/null 2>&1 || true

# Give mitmdump time to flush the record.
for _ in $(seq 1 50); do
  [[ -s "${OBSERVED}" ]] && break
  sleep 0.2
done
if [[ ! -s "${OBSERVED}" ]]; then
  KEEP_WORK_DIR=1
  echo "mitmdump log:" >&2
  tail -20 "${MITM_LOG}" >&2 || true
  echo "proxy log:" >&2
  tail -20 "${PROXY_LOG}" >&2 || true
  fail "no flow reached mitmdump. Either the proxy ignored HTTPS_PROXY or it never sent an upstream request. Work dir kept at ${WORK_DIR}"
fi

echo "=== [6/6] Diffing the wire identity against the baseline ==="
if ! REPO_ROOT="${REPO_ROOT}" python3 "${REPO_ROOT}/scripts/diff_claude_code_identity.py" \
  --baseline "${BASELINE}" \
  --observed "${OBSERVED}"; then
  KEEP_WORK_DIR=1
  echo "Observed capture kept at ${OBSERVED}" >&2
  fail "the proxy's wire identity has drifted from the committed baseline"
fi

# The caller's system prompt must still be there, after the marker.
#
# This assertion lives here rather than in the differ because it is about what THIS
# request sent, and the differ only knows baseline versus observed — real Claude
# Code's own block count is not a bound on ours (the committed captures show <x4>).
#
# The body fingerprint encodes a list as [shape-of-first, "<xN>"], so "<x2>" is the
# marker plus the one block the request sent. "<x1>" means block 0 was overwritten
# and the caller's prompt was destroyed; that was the behaviour before this branch,
# and the diff above cannot see it, since system_first_block reports block 0 alone.
SYSTEM_SHAPE="$(jq -r '
  [ .[] | select(.method == "POST") | select(.path | startswith("/v1/messages")) ]
  | last | .request_body.shape.system[1] // "absent"
' -s "${OBSERVED}" 2>/dev/null || echo "unreadable")"

if [[ "${SYSTEM_SHAPE}" != "<x2>" ]]; then
  KEEP_WORK_DIR=1
  echo "ERROR: system block count is ${SYSTEM_SHAPE}, want <x2>." >&2
  echo "ERROR: the request sent one system block and normalization prepends the billing" >&2
  echo "ERROR: marker, so two must arrive. <x1> means block 0 was overwritten and the" >&2
  echo "ERROR: caller's system prompt was deleted." >&2
  echo "Observed capture kept at ${OBSERVED}" >&2
  fail "normalization destroyed the caller's system prompt"
fi

echo "Claude Code identity gate PASSED: the wire identity matches ${BASELINE##*/},"
echo "and the caller's system prompt survived alongside the billing marker."
