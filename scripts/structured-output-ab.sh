#!/usr/bin/env bash
#
# A/B harness for structured output and tool-call bypass rates.
#
# Measures how often a model actually produces the constrained output the
# client asked for, across the request shapes that behave differently on the
# proxy. Go tests prove the wire format; only a live run against a real model
# proves the bypass rate, so this is the tool for that half.
#
# Arms:
#   tools-plain      tools + forced tool_choice, schema without additionalProperties
#   tools-strict     tools + forced tool_choice, schema with additionalProperties:false
#                    (what an OpenAI client emits under strict:true)
#   response-format  response_format json_schema, no tools
#
# A trial counts as a HIT when the arm's contract is satisfied: a tool_calls
# entry naming the tool with parseable arguments for the tools-* arms, or a
# message.content that parses as a JSON object for response-format. Anything
# else is a MISS (bypass), including prose, fenced JSON, and truncation.
#
# Usage:
#   scripts/structured-output-ab.sh [-n TRIALS] [-m MODEL] [-u BASE_URL] [-a ARM]...
#
# Example:
#   scripts/structured-output-ab.sh -n 30 -m 'inclusionai/ling-3.0-flash-sante:free'
#
# Env:
#   PROXY_API_KEY   Bearer token for the proxy (default: empty)

set -euo pipefail

TRIALS=30
MODEL='inclusionai/ling-3.0-flash-sante:free'
BASE_URL='http://localhost:8080/v1'
ARMS=()

usage() { sed -n '2,29p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }

while getopts ':n:m:u:a:h' opt; do
  case "$opt" in
    n) TRIALS="$OPTARG" ;;
    m) MODEL="$OPTARG" ;;
    u) BASE_URL="${OPTARG%/}" ;;
    a) ARMS+=("$OPTARG") ;;
    h) usage 0 ;;
    *) echo "unknown option: -$OPTARG" >&2; usage 1 ;;
  esac
done

if [ "${#ARMS[@]}" -eq 0 ]; then
  ARMS=(tools-plain tools-strict response-format)
fi

for dep in curl jq; do
  command -v "$dep" >/dev/null 2>&1 || { echo "missing dependency: $dep" >&2; exit 1; }
done

API_KEY="${PROXY_API_KEY:-}"
TOOL_NAME='final_answer_mcq'

# One deterministic MCQ. The task is trivial on purpose: a miss measures the
# constraint failing, not the model failing to know the answer.
QUESTION='Which planet is closest to the Sun? A) Mars B) Mercury C) Venus D) Earth. Answer with the letter.'

# schema_body <closed>  ->  the answer schema, optionally closed to extra keys.
schema_body() {
  local closed="$1" extra=''
  [ "$closed" = 'closed' ] && extra=',"additionalProperties":false'
  cat <<JSON
{"type":"object","properties":{"answer":{"type":"string","enum":["A","B","C","D"]}},"required":["answer"]${extra}}
JSON
}

# payload <arm>  ->  the request body for that arm.
payload() {
  local arm="$1" schema
  case "$arm" in
    tools-plain)  schema="$(schema_body open)" ;;
    tools-strict) schema="$(schema_body closed)" ;;
    response-format) schema="$(schema_body closed)" ;;
    *) echo "unknown arm: $arm" >&2; return 1 ;;
  esac

  if [ "$arm" = 'response-format' ]; then
    jq -nc --arg model "$MODEL" --arg q "$QUESTION" --arg name "$TOOL_NAME" --argjson schema "$schema" '{
      model: $model, max_tokens: 256, temperature: 0,
      messages: [{role: "user", content: $q}],
      response_format: {type: "json_schema", json_schema: {name: $name, strict: true, schema: $schema}}
    }'
    return
  fi

  local strict='false'
  [ "$arm" = 'tools-strict' ] && strict='true'
  jq -nc --arg model "$MODEL" --arg q "$QUESTION" --arg name "$TOOL_NAME" \
         --argjson schema "$schema" --argjson strict "$strict" '{
    model: $model, max_tokens: 256, temperature: 0,
    messages: [{role: "user", content: $q}],
    tools: [{type: "function", function: ({name: $name, description: "Return the final answer.", parameters: $schema}
             + (if $strict then {strict: true} else {} end))}],
    tool_choice: {type: "function", function: {name: $name}}
  }'
}

# verdict <arm> <response-json>  ->  "hit <answer>" or "miss <reason>".
verdict() {
  local arm="$1" body="$2"

  if ! jq -e . >/dev/null 2>&1 <<<"$body"; then
    echo 'miss non-json-response'; return
  fi
  if jq -e '.error' >/dev/null 2>&1 <<<"$body"; then
    echo "miss api-error:$(jq -rc '.error.message // "unknown"' <<<"$body" | tr -d '\n' | cut -c1-60)"; return
  fi

  local args content
  if [ "$arm" = 'response-format' ]; then
    content="$(jq -r '.choices[0].message.content // ""' <<<"$body")"
    if [ -z "$content" ]; then echo 'miss empty-content'; return; fi
    if ! jq -e 'type == "object"' >/dev/null 2>&1 <<<"$content"; then
      echo 'miss content-not-json-object'; return
    fi
    args="$content"
  else
    args="$(jq -r --arg n "$TOOL_NAME" \
      '[.choices[0].message.tool_calls // [] | .[] | select(.function.name == $n) | .function.arguments] | first // ""' \
      <<<"$body")"
    if [ -z "$args" ]; then echo 'miss no-tool-call'; return; fi
    if ! jq -e . >/dev/null 2>&1 <<<"$args"; then echo 'miss unparseable-arguments'; return; fi
  fi

  local answer
  answer="$(jq -r '.answer // ""' <<<"$args")"
  if [ -z "$answer" ]; then echo 'miss missing-answer-field'; return; fi
  echo "hit $answer"
}

printf 'model:   %s\n' "$MODEL"
printf 'proxy:   %s\n' "$BASE_URL"
printf 'trials:  %d per arm\n\n' "$TRIALS"

# Exit status reports whether the measurement ran, not whether the model did
# well: bypasses are the thing being measured. A non-zero status means an arm
# produced no usable trial at all, which points at the proxy or the model being
# unreachable rather than at a bypass rate worth reading.
dead_arms=0

for arm in "${ARMS[@]}"; do
  body="$(payload "$arm")" || exit 1
  hits=0
  declare -A reasons=()

  printf '%-16s ' "$arm"
  for _ in $(seq 1 "$TRIALS"); do
    args=(-sS --max-time 120 -X POST "$BASE_URL/chat/completions"
          -H 'Content-Type: application/json' -d "$body")
    [ -n "$API_KEY" ] && args+=(-H "Authorization: Bearer $API_KEY")

    response="$(curl "${args[@]}" 2>/dev/null || echo '{"error":{"message":"transport failure"}}')"
    result="$(verdict "$arm" "$response")"

    case "$result" in
      hit\ *) hits=$((hits + 1)); printf '.' ;;
      *)      reasons["${result#miss }"]=$(( ${reasons["${result#miss }"]:-0} + 1 )); printf 'x' ;;
    esac
  done

  misses=$((TRIALS - hits))
  rate=$(awk -v m="$misses" -v t="$TRIALS" 'BEGIN{printf "%.1f", (m/t)*100}')
  printf '  bypass %s%% (%d/%d)\n' "$rate" "$misses" "$TRIALS"
  for reason in "${!reasons[@]}"; do
    printf '%-16s   %-32s %d\n' '' "$reason" "${reasons[$reason]}"
  done
  if [ "$hits" -eq 0 ]; then
    dead_arms=$((dead_arms + 1))
  fi
  unset reasons
done

printf '\nLower bypass is better. Compare tools-plain against tools-strict to see\n'
printf 'whether the strict-mode schema still costs accuracy, and response-format\n'
printf 'to see whether the emulation holds.\n'

if [ "$dead_arms" -gt 0 ]; then
  printf '\n%d arm(s) produced no usable trial — check the proxy and the model.\n' "$dead_arms" >&2
  exit 1
fi
exit 0
