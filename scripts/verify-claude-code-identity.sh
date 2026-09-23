#!/usr/bin/env bash
set -euo pipefail

# Claude Code wire-identity drift gate.
#
# Boots the proxy behind mitmdump, drives four requests across the three paths
# that carry a wire identity, and checks each against the committed capture at
# .reference/claude-code-headers-20260923.jsonl:
#
#   pooled gateway, normalized      full header and body diff against the capture
#   custom endpoint, no apiKey      must present the captured identity
#   custom endpoint with an apiKey  must present the CALLER's identity and keep
#                                   the key, because x-api-key is on the omit list
#   GET /v1/models, three callers   must carry the captured discovery User-Agent
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
OBSERVED_POOLED="${WORK_DIR}/observed-pooled.jsonl"
OBSERVED_CUSTOM_KEYLESS="${WORK_DIR}/observed-custom-keyless.jsonl"
OBSERVED_CUSTOM_KEYED="${WORK_DIR}/observed-custom-keyed.jsonl"
OBSERVED_DISCOVERY="${WORK_DIR}/observed-discovery.jsonl"
MITM_LOG="${WORK_DIR}/mitmdump.log"
PROXY_LOG="${WORK_DIR}/proxy.log"

# The request the gate drives. Any allowlisted Claude model works; haiku is the
# cheapest if the stub is ever replaced by a live account.
GATE_MODEL="claude-haiku-4-5"

# Two custom endpoints, identical but for the credential. Both are
# Anthropic-shaped, so isAnthropicEndpoint matches and normalization is in
# scope for both; only the configured apiKey decides. Neither model name is in
# the Claude Code allowlist, so the pooled gateway declines them and this gate's
# first phase is untouched.
GATE_CUSTOM_KEYLESS_MODEL="gate-custom-keyless"
GATE_CUSTOM_KEYED_MODEL="gate-custom-keyed"
GATE_CUSTOM_API_KEY="gate-custom-endpoint-credential-not-valid-upstream"
# The caller's own fingerprint, which a non-normalized endpoint must see
# unchanged. Kept in one place because both the driven request and the
# passthrough assertion need the same value.
GATE_CALLER_USER_AGENT="foreign-harness/1.0"
# GATE_CUSTOM_API_KEY_SHA256 is computed AFTER the tool preflight, because it
# needs shasum and a missing tool has to reach skip() rather than die on a
# failed command substitution under `set -o pipefail`.

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
for tool in mitmdump jq go nc python3 shasum; do
  command -v "${tool}" >/dev/null 2>&1 || skip "${tool} not found in PATH"
done
[[ -f "${BASELINE}" ]] || skip "baseline capture missing at ${BASELINE}"

# The mitm addon redacts any header whose name matches api[_-]?key, hashing the
# whole value. Comparing that digest proves the exact key survived without the
# key ever appearing in the capture.
GATE_CUSTOM_API_KEY_SHA256="$(printf '%s' "${GATE_CUSTOM_API_KEY}" | shasum -a 256 | cut -d' ' -f1)"

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

# Each phase is diffed against its own records alone. The differ reads the
# FIRST POST /v1/messages record it finds, so a later phase's request would
# otherwise change what the earlier assertion examines. The mitm addon opens
# the output per record with mode "a" and closes it again, so truncating here
# cannot corrupt a write in flight.
snapshot_phase() {
  local destination="$1"
  local what="$2"
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
    fail "no flow reached mitmdump for ${what}. Work dir kept at ${WORK_DIR}"
  fi
  cp "${OBSERVED}" "${destination}"
  : > "${OBSERVED}"
}

echo "=== [1/9] Building the proxy ==="
mkdir -p "${REPO_ROOT}/bin"
(cd "${REPO_ROOT}" && go build -o bin/antigravity-proxy ./cmd/proxy)

echo "=== [2/9] Writing a stub configuration ==="
mkdir -p "${CONFIG_DIR}" "${MITM_CONFDIR}"
# gatewayOrder is narrowed to claudecode and custom alone so no request can be
# answered by another gateway and leave this gate asserting nothing. The two
# custom models are not in the Claude Code allowlist, and tryCustomEndpointGateway
# keys on the exact model name, so the pooled phase and the custom phases cannot
# answer each other's requests.
cat > "${CONFIG_DIR}/config.json" <<JSON
{
  "gatewayOrder": { "byModel": {}, "order": ["claudecode", "custom"] },
  "customEndpoints": {
    "${GATE_CUSTOM_KEYLESS_MODEL}": {
      "url": "https://api.anthropic.com/v1/messages"
    },
    "${GATE_CUSTOM_KEYED_MODEL}": {
      "url": "https://api.anthropic.com/v1/messages",
      "apiKey": "${GATE_CUSTOM_API_KEY}"
    }
  },
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

echo "=== [3/9] Starting mitmdump on port ${MITM_PORT} ==="
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

echo "=== [4/9] Starting the proxy on port ${PROXY_PORT} ==="
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

echo "=== [5/9] Driving one foreign-headed request ==="
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
  -H "User-Agent: ${GATE_CALLER_USER_AGENT}" \
  -H 'x-app: foreign-app' \
  -H 'anthropic-beta: foreign-beta-not-in-baseline' \
  -d "{\"model\":\"${GATE_MODEL}\",\"max_tokens\":16,\"temperature\":0.7,\"system\":[{\"type\":\"text\",\"text\":\"caller system prompt the proxy must not delete\"}],\"messages\":[{\"role\":\"user\",\"content\":\"ok\"}]}" \
  >/dev/null 2>&1 || true

snapshot_phase "${OBSERVED_POOLED}" "the pooled gateway request"

echo "=== [6/9] Diffing the wire identity against the baseline ==="
if ! REPO_ROOT="${REPO_ROOT}" python3 "${REPO_ROOT}/scripts/diff_claude_code_identity.py" \
  --baseline "${BASELINE}" \
  --observed "${OBSERVED_POOLED}"; then
  KEEP_WORK_DIR=1
  echo "Observed capture kept at ${OBSERVED_POOLED}" >&2
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
' -s "${OBSERVED_POOLED}" 2>/dev/null || echo "unreadable")"

if [[ "${SYSTEM_SHAPE}" != "<x2>" ]]; then
  KEEP_WORK_DIR=1
  echo "ERROR: system block count is ${SYSTEM_SHAPE}, want <x2>." >&2
  echo "ERROR: the request sent one system block and normalization prepends the billing" >&2
  echo "ERROR: marker, so two must arrive. <x1> means block 0 was overwritten and the" >&2
  echo "ERROR: caller's system prompt was deleted." >&2
  echo "Observed capture kept at ${OBSERVED_POOLED}" >&2
  fail "normalization destroyed the caller's system prompt"
fi

echo "=== [7/9] Driving a keyless custom endpoint ==="
# Anthropic-shaped and carrying no credential of its own, so normalization
# applies: this is the half of T1 that must keep working. Same hostile input as
# the pooled request, so a regression shows up as the caller's own values
# reaching the wire.
curl -sS -o /dev/null --max-time 60 \
  -X POST "http://127.0.0.1:${PROXY_PORT}/v1/messages" \
  -H 'Content-Type: application/json' \
  -H "User-Agent: ${GATE_CALLER_USER_AGENT}" \
  -H 'x-app: foreign-app' \
  -d "{\"model\":\"${GATE_CUSTOM_KEYLESS_MODEL}\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"ok\"}]}" \
  >/dev/null 2>&1 || true

snapshot_phase "${OBSERVED_CUSTOM_KEYLESS}" "the keyless custom endpoint"

if ! python3 "${REPO_ROOT}/scripts/check_identity_policy.py" normalized \
  --baseline "${BASELINE}" \
  --observed "${OBSERVED_CUSTOM_KEYLESS}"; then
  KEEP_WORK_DIR=1
  echo "Observed capture kept at ${OBSERVED_CUSTOM_KEYLESS}" >&2
  fail "a keyless Anthropic-shaped custom endpoint was not normalized"
fi

echo "=== [8/9] Driving a custom endpoint authenticated by API key ==="
# The T1 regression, live. x-api-key is on the omit list and ApplyHeaders
# deletes every omitted name, so normalizing this endpoint would send it
# authenticated by nothing. internal/api/server.go refuses to normalize it at
# all; the wire must therefore show the caller's identity and the configured
# key, not the captured identity.
#
# The caller's User-Agent survives because this endpoint is forwarded by the
# ReverseProxy path, which clones the inbound headers. The CCR branch in
# forwardToCustomEndpoint builds a fresh request instead and copies none of
# them, so if headroom.ccr.enabled ever defaults to true this phase fails with
# Go-http-client/1.1 for a reason that has nothing to do with T1. The stub
# configuration sets no headroom block, and the default is false.
curl -sS -o /dev/null --max-time 60 \
  -X POST "http://127.0.0.1:${PROXY_PORT}/v1/messages" \
  -H 'Content-Type: application/json' \
  -H "User-Agent: ${GATE_CALLER_USER_AGENT}" \
  -H 'x-app: foreign-app' \
  -d "{\"model\":\"${GATE_CUSTOM_KEYED_MODEL}\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"ok\"}]}" \
  >/dev/null 2>&1 || true

snapshot_phase "${OBSERVED_CUSTOM_KEYED}" "the keyed custom endpoint"

if ! python3 "${REPO_ROOT}/scripts/check_identity_policy.py" passthrough \
  --baseline "${BASELINE}" \
  --observed "${OBSERVED_CUSTOM_KEYED}" \
  --caller-user-agent "${GATE_CALLER_USER_AGENT}" \
  --api-key-sha256 "${GATE_CUSTOM_API_KEY_SHA256}"; then
  KEEP_WORK_DIR=1
  echo "ERROR: an endpoint configured with an apiKey must reach the upstream carrying it." >&2
  echo "ERROR: x-api-key is on the omit list, so normalizing such an endpoint deletes the" >&2
  echo "ERROR: credential and the endpoint authenticates by nothing." >&2
  echo "Observed capture kept at ${OBSERVED_CUSTOM_KEYED}" >&2
  fail "the custom-endpoint credential rule was broken"
fi

echo "=== [9/9] Driving the three discovery callers ==="
# Three of the five sites T4 corrected, one per client method:
#
#   .../test         ValidateAccount    (sent no User-Agent at all, so Go's
#                                        transport supplied Go-http-client/<ver>,
#                                        2.0 over the h2 connection mitm negotiates)
#   .../ratelimits   FetchRateLimits    (sent Claude-Code/2.1.246)
#   models/fetch     FetchModels        (sent Claude-Code/2.1.246)
#
# The other two are in internal/auth and are not driven here: the refresh route
# reaches them only when the pool happens to have been created through the
# server-bound getOrCreateCCPool rather than the package-level nil-server one,
# and FetchProfile needs a real authorization code. The policy selector is wider
# than /v1/models, so either is still checked if it fires.
#
# No request body is needed: each handler resolves the stub token out of the
# configuration. The upstream rejects that token, which is irrelevant — the
# request mitmdump recorded on the way out is the whole subject.
#
# The management API is unauthenticated here because the stub configuration
# sets no webuiPassword.
GATE_ACCOUNT_ID="cc-identity-gate-stub"
for route in \
  "/api/claudecode/accounts/${GATE_ACCOUNT_ID}/test" \
  "/api/claudecode/accounts/${GATE_ACCOUNT_ID}/ratelimits" \
  "/api/claudecode/models/fetch"
do
  curl -sS -o /dev/null --max-time 60 \
    -X POST "http://127.0.0.1:${PROXY_PORT}${route}" \
    -H 'Content-Type: application/json' \
    -d '{}' \
    >/dev/null 2>&1 || true
done

snapshot_phase "${OBSERVED_DISCOVERY}" "the discovery requests"

if ! python3 "${REPO_ROOT}/scripts/check_identity_policy.py" discovery \
  --baseline "${BASELINE}" \
  --observed "${OBSERVED_DISCOVERY}" \
  --min-records 3; then
  KEEP_WORK_DIR=1
  echo "ERROR: the capture records claude-code/<version> on every GET it observed," >&2
  echo "ERROR: lowercase and with no parenthesised mode. Go's transport supplies" >&2
  echo "ERROR: Go-http-client/<negotiated HTTP version> when no User-Agent is set" >&2
  echo "ERROR: at all, so a 2.0 there means the header is missing, not stale." >&2
  echo "Observed capture kept at ${OBSERVED_DISCOVERY}" >&2
  fail "a discovery request did not carry the captured User-Agent"
fi

echo "Claude Code identity gate PASSED: the wire identity matches ${BASELINE##*/},"
echo "the caller's system prompt survived alongside the billing marker,"
echo "a keyed custom endpoint kept its credential and was not normalized,"
echo "and every discovery request carried the captured User-Agent."
