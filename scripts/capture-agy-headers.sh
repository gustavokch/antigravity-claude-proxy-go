#!/usr/bin/env bash
set -euo pipefail

# Local MITM header-capture harness.
#
# Boots mitmdump on 127.0.0.1 with the header-dump addon
# (scripts/mitm_header_dump.py), scoped via allow_hosts to the two Cloud Code
# endpoints ONLY, runs the given command, then stops mitmdump. Intercepted
# requests land in $MITM_DUMP_OUT as ordered JSONL.
#
# The caller puts the proxy on the command's environment, e.g.:
#   scripts/capture-agy-headers.sh env \
#     HTTPS_PROXY=http://127.0.0.1:18080 HTTP_PROXY=http://127.0.0.1:18080 \
#     agy --print='hi'
#
# The mitmproxy CA must be trusted on this machine for TLS to succeed
# (see docs/superpowers/plans/2026-09-03-agy-mitm-header-capture.md Task 2).
# ANTIGRAVITY_MITM_PORT overrides the listen port (default 18080).
# MITM_DUMP_OUT overrides the JSONL output path.

PORT="${ANTIGRAVITY_MITM_PORT:-18080}"
LISTEN_HOST="127.0.0.1"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ADDON="$SCRIPT_DIR/mitm_header_dump.py"
CONFDIR="${ANTIGRAVITY_MITM_CONFDIR:-$HOME/.mitmproxy}"
# Fresh capture per run: the addon appends, so a fixed default would mix
# stale entries into a new capture. Override via MITM_DUMP_OUT.
DUMP_OUT="${MITM_DUMP_OUT:-/tmp/agy-headers-mitm-$(date +%Y%m%d-%H%M%S).jsonl}"
export MITM_DUMP_OUT="$DUMP_OUT"

if [ "$#" -eq 0 ]; then
  echo "usage: $0 <command> [args...]" >&2
  echo "" >&2
  echo "  Runs <command> through a local mitmdump intercepting ONLY the" >&2
  echo "  cloudcode-pa / daily-cloudcode-pa endpoints. Put the proxy on the" >&2
  echo "  command yourself: HTTPS_PROXY=http://$LISTEN_HOST:$PORT." >&2
  echo "  JSONL lands in \$MITM_DUMP_OUT (default /tmp/agy-headers-mitm-<ts>.jsonl)." >&2
  exit 2
fi

if ! command -v mitmdump >/dev/null 2>&1; then
  echo "mitmdump not found in PATH (brew install mitmproxy)" >&2
  exit 1
fi

MITMDUMP_PID=""
cleanup() {
  if [ -n "$MITMDUMP_PID" ] && kill -0 "$MITMDUMP_PID" 2>/dev/null; then
    kill "$MITMDUMP_PID" 2>/dev/null || true
    wait "$MITMDUMP_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# mitmproxy 12 matches allow_hosts against "host:port", so the pattern must
# tolerate an optional ":443" suffix (a bare anchored host regex silently
# passes everything through as a raw tunnel).
echo "=== Starting mitmdump on $LISTEN_HOST:$PORT (allow_hosts: cloudcode) ==="
mitmdump --listen-host "$LISTEN_HOST" --listen-port "$PORT" \
  -s "$ADDON" \
  --set "allow_hosts=^(cloudcode|daily-cloudcode)-pa\\.googleapis\\.com(:443)?$" \
  --set "confdir=$CONFDIR" \
  > "$DUMP_OUT.mitmdump.log" 2>&1 &
MITMDUMP_PID=$!

port_open() { nc -z "$LISTEN_HOST" "$PORT" >/dev/null 2>&1; }

for _ in $(seq 1 100); do
  if port_open; then break; fi
  if ! kill -0 "$MITMDUMP_PID" 2>/dev/null; then
    echo "mitmdump exited during startup; log:" >&2
    cat "$DUMP_OUT.mitmdump.log" >&2
    exit 1
  fi
  sleep 0.1
done

if ! port_open; then
  echo "mitmdump did not open port $PORT in time" >&2
  exit 1
fi

echo "=== Running: $* ==="
set +e
"$@"
CMD_RC=$?
set -e

echo "=== Command exit: $CMD_RC; stopping mitmdump ==="
exit "$CMD_RC"
