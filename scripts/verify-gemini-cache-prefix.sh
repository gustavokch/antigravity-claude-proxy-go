#!/usr/bin/env bash
# Two-turn Gemini tool loop with no thought signatures, the shape a strict
# Anthropic client sends. Turn 1 must return 200. Turn 2 must return 200 and
# should report a non-zero cache_read_input_tokens once the prompt clears the
# ~32k implicit-cache floor.
set -euo pipefail

PROXY_URL="${PROXY_URL:-http://127.0.0.1:8080}"
MODEL="${MODEL:-gemini-3.0-flash-high}"
PAD="$(head -c 60000 /dev/urandom | base64 | tr -d '\n' | head -c 60000)"

turn() {
  local payload="$1" label="$2"
  local body status
  body="$(mktemp)"
  status="$(curl -sS -o "$body" -w '%{http_code}' \
    -X POST "$PROXY_URL/v1/messages" \
    -H 'content-type: application/json' \
    -H 'anthropic-version: 2023-06-01' \
    --data-binary "$payload")"
  echo "== $label: HTTP $status"
  if [ "$status" != "200" ]; then
    echo "-- error body:"
    head -c 2000 "$body"
    echo
    rm -f "$body"
    return 1
  fi
  grep -o '"usage":{[^}]*}' "$body" || echo "-- no usage block in response"
  rm -f "$body"
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

turn "$TURN1" "turn 1"
sleep 2
turn "$TURN2" "turn 2 (prefix of turn 1 must be reused)"
