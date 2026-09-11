#!/usr/bin/env bash
# Turns 1 and 2 are a two-turn Gemini tool loop with no thought signatures, the
# shape a strict Anthropic client sends. Turn 1 must return 200. Turn 2 must
# return 200 and must report a non-zero cache_read_input_tokens. Raise the
# padding first if the prompt sits below the ~32k implicit-cache floor.
#
# Turn 3 is a separate provenance probe, not part of that gate. It sends a
# thinking block carrying a signature this proxy never issued, which resolves to
# FamilyUnknown and is forwarded to the backend verbatim (see
# TestUnknownThinkingSignatureReachesGeminiVerbatim). It answers one question:
# does Gemini validate thought signatures that sit in history rather than on a
# current-turn function call? If it does not, a stale cross-family signature can
# never provoke a 400 and the keep-on-unknown rule is safe as written. If it
# does, provenance has to travel inside the signature value instead of in the
# process-local cache.
#
# Exit codes: 0 all good. 1 the cache-prefix gate failed. 2 the gate passed but
# the probe shows Gemini rejects foreign history signatures.
set -euo pipefail

PROXY_URL="${PROXY_URL:-http://127.0.0.1:8080}"
MODEL="${MODEL:-gemini-3.0-flash-high}"

if ! curl -s -m 2 "$PROXY_URL/" >/dev/null 2>&1; then
  echo "Error: proxy not reachable at $PROXY_URL. Start proxy first." >&2
  exit 1
fi

PAD="$(head -c 120000 /dev/urandom | base64 | tr -d '\n')"
# 64 chars, comfortably over MinSignatureLength (50), and self-describing in a
# backend log if one ever surfaces it.
FOREIGN_SIG="notarealgeminithoughtsignature_provenanceprobe_000000000000000000"
body=""
trap 'rm -f "${body:-}"' EXIT

turn() {
  local payload="$1" label="$2" require_cache_read="${3:-0}"
  local status cache_read
  body="$(mktemp)"
  status="$(curl -sS -o "$body" -w '%{http_code}' \
    -X POST "$PROXY_URL/v1/messages" \
    -H 'content-type: application/json' \
    -H 'anthropic-version: 2023-06-01' \
    -H "x-api-key: ${ANTIGRAVITY_PROXY_API_KEY:-}" \
    --data-binary "$payload")"
  echo "== $label: HTTP $status"
  if [ "$status" != "200" ]; then
    echo "-- error body:"
    head -c 2000 "$body"
    echo
    rm -f "$body"
    body=""
    return 1
  fi
  grep -oE '"usage"[[:space:]]*:[[:space:]]*\{[^}]*\}' "$body" || echo "-- no usage block in response"
  cache_read="$(grep -oE '"cache_read_input_tokens"[[:space:]]*:[[:space:]]*[0-9]+' "$body" |
    head -1 | grep -oE '[0-9]+$' || true)"
  rm -f "$body"
  body=""
  if [ "$require_cache_read" = "1" ]; then
    if [ -z "$cache_read" ]; then
      echo "-- FAIL: response carried no cache_read_input_tokens field"
      return 1
    fi
    if [ "$cache_read" -eq 0 ]; then
      echo "-- FAIL: cache_read_input_tokens is 0, so the prefix was not reused."
      echo "   A benign cause is a prompt below the ~32k implicit-cache floor:"
      echo "   raise the padding and retry before treating this as a regression."
      return 1
    fi
    echo "-- OK: cache_read_input_tokens = $cache_read"
  fi
}

probe_foreign_signature() {
  local payload="$1" status
  body="$(mktemp)"
  status="$(curl -sS -o "$body" -w '%{http_code}' \
    -X POST "$PROXY_URL/v1/messages" \
    -H 'content-type: application/json' \
    -H 'anthropic-version: 2023-06-01' \
    -H "x-api-key: ${ANTIGRAVITY_PROXY_API_KEY:-}" \
    --data-binary "$payload")"
  echo "== turn 3 (provenance probe): HTTP $status"
  case "$status" in
    200)
      rm -f "$body"; body=""
      echo "-- VERDICT: accepted. Gemini did not validate a thought signature it"
      echo "   never issued, sitting in history. The keep-on-unknown rule in"
      echo "   convertContentToParts cannot provoke a 400, so provenance may stay"
      echo "   in the process-local cache."
      return 0
      ;;
    400)
      echo "-- response body:"
      head -c 2000 "$body"; echo
      rm -f "$body"; body=""
      echo "-- VERDICT: rejected. Gemini validates thought signatures in history,"
      echo "   so an expired cache entry on a cross-model handoff turns into a 400."
      echo "   Provenance has to travel inside the signature value rather than in"
      echo "   the cache. Confirm the body above names the signature before acting."
      return 2
      ;;
    *)
      echo "-- response body:"
      head -c 2000 "$body"; echo
      rm -f "$body"; body=""
      echo "-- INCONCLUSIVE: HTTP $status is not a signature verdict. Auth, quota"
      echo "   and upstream faults all land here. Fix that and re-run."
      return 0
      ;;
  esac
}

read -r -d '' TURN1 <<JSON || true
{"model":"$MODEL","max_tokens":256,"tools":[{"name":"read","description":"read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}],"messages":[
 {"role":"user","content":"Context padding: $PAD\n\nCall the read tool on file.go, then summarise."},
 {"role":"assistant","content":[{"type":"tool_use","id":"toolu_a","name":"read","input":{"path":"file.go"}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a","content":"package main"}]}
]}
JSON

read -r -d '' TURN2 <<JSON || true
{"model":"$MODEL","max_tokens":256,"tools":[{"name":"read","description":"read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}],"messages":[
 {"role":"user","content":"Context padding: $PAD\n\nCall the read tool on file.go, then summarise."},
 {"role":"assistant","content":[{"type":"tool_use","id":"toolu_a","name":"read","input":{"path":"file.go"}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a","content":"package main"}]},
 {"role":"assistant","content":[{"type":"tool_use","id":"toolu_b","name":"read","input":{"path":"other.go"}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_b","content":"package other"}]}
]}
JSON

read -r -d '' TURN3 <<JSON || true
{"model":"$MODEL","max_tokens":256,"messages":[
 {"role":"user","content":"Name one Go standard library package."},
 {"role":"assistant","content":[{"type":"thinking","thinking":"The user wants one package name.","signature":"$FOREIGN_SIG"},{"type":"text","text":"net/http"}]},
 {"role":"user","content":"Name one more."}
]}
JSON

turn "$TURN1" "turn 1"
sleep 2
turn "$TURN2" "turn 2 (prefix of turn 1 must be reused)" 1
sleep 2
probe_status=0
probe_foreign_signature "$TURN3" || probe_status=$?
exit "$probe_status"
