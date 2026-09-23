# Podman Claude Code MITM Capture Harness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run containerized Claude Code against the host Go proxy so the proxy records live security-monitor (classifier) request bodies, without touching the host's own Claude Code install.

**Architecture:** A shell script builds the existing `~/Git/claude-container` image, points the container's `ANTHROPIC_BASE_URL` at the macOS host via Podman's `host-gateway` alias, and drops the operator into an interactive Claude Code session inside a throwaway workspace. The proxy's existing request logging is the capture mechanism; the script adds no interception code.

**Tech Stack:** Podman (macOS VM), bash, jq, the existing `antigravity-proxy` binary.

**Spec:** `docs/classifier-fallback-notes.md` (the verbatim fingerprints this harness exists to extend), `docs/HANDOFF-podman-claude-packy.md`

## Global Constraints

- Never commit or print live API keys. The script may read a key from the environment or config file, but must never echo it.
- The container must reach the host proxy at `http://containers.internal:8080`; `localhost` inside the container is the container itself.
- The host's own `~/.claude` directory must never be bind-mounted into the container. Captured sessions use a throwaway workspace.
- This plan produces no Go code and no changes to the proxy. It is a standalone operator tool.

---

### Task 1: Container MITM Runner Script

**Files:**
- Create: `scripts/run-claude-mitm-sandbox.sh`

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces: an executable script; no importable symbols.

- [ ] **Step 1: Verify the prerequisites the script will assume**

Run:

```bash
ls ~/Git/claude-container/Containerfile
command -v jq
command -v podman
podman machine info >/dev/null && echo "machine ok"
```

Expected: the Containerfile path prints, `jq` and `podman` resolve to paths, and `machine ok` prints. If `podman machine info` fails, run `podman machine start` first. If `jq` is missing, install it (`brew install jq`) before continuing — the script depends on it for key extraction.

- [ ] **Step 2: Write `scripts/run-claude-mitm-sandbox.sh`**

```bash
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
  CONFIG_JSON="${HOME}/.config/antigravity-proxy/config.json"
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

exec podman run --rm -it \
  --name claude-mitm-session \
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
```

Note the deliberate omission of `--dangerously-skip-permissions`. The security-monitor call is what grades a *pending* permission decision; a session that skips permission checks may never emit one. The operator approves the bash call by hand, which is the event being captured.

- [ ] **Step 3: Make the script executable and check its syntax**

Run:

```bash
chmod +x scripts/run-claude-mitm-sandbox.sh
bash -n scripts/run-claude-mitm-sandbox.sh
```

Expected: both commands exit 0 with no output.

- [ ] **Step 4: Verify the script fails closed without a key**

Run: `env -u ANTHROPIC_API_KEY HOME=/tmp/nonexistent-home bash scripts/run-claude-mitm-sandbox.sh; echo "exit=$?"`

Expected: prints `ERROR: ANTHROPIC_API_KEY not set and no apiKey in config.json.` (or an earlier prerequisite error) and `exit=1`. It must not reach the `podman build` step.

- [ ] **Step 5: Live capture run (manual, requires the proxy running)**

1. Start the proxy on port 8080 in another terminal.
2. Run `./scripts/run-claude-mitm-sandbox.sh`.
3. At the Claude Code prompt, type: `run ls -la and tell me what you see`
4. Approve the bash tool call when prompted.
5. In the proxy's logs or WebUI log stream, confirm at least one request body containing `You are a security monitor for autonomous AI coding agents.` was received.

Expected: the monitor prompt appears in the proxy's recorded traffic. If it does not, the classifier did not fire — record that outcome in `docs/classifier-fallback-notes.md` rather than guessing at a fix.

- [ ] **Step 6: Commit**

```bash
git add scripts/run-claude-mitm-sandbox.sh
git commit -m "feat(scripts): add podman claude code MITM capture harness"
```

---

### Task 2: Document the Harness

**Files:**
- Modify: `docs/classifier-fallback-notes.md`

**Interfaces:**
- Consumes: the script from Task 1.
- Produces: nothing importable.

- [ ] **Step 1: Append a capture-procedure section**

Add to the end of `docs/classifier-fallback-notes.md`:

```markdown
## Re-capturing fingerprints

`scripts/run-claude-mitm-sandbox.sh` runs Claude Code inside a Podman
container pointed at the host proxy, so new classifier variants can be
captured without disturbing the host's Claude Code install.

Procedure:

1. Start `antigravity-proxy` on port 8080.
2. Run `./scripts/run-claude-mitm-sandbox.sh`.
3. Ask the session to run a shell command and approve the tool call.
4. Read the captured bodies from the proxy log stream.

The container never mounts `~/.claude`, and its workspace is a `mktemp -d`
directory discarded when the session exits. Fingerprints recorded here are
verbatim quotes from observed traffic; nothing in this file is inferred.
```

- [ ] **Step 2: Commit**

```bash
git add docs/classifier-fallback-notes.md
git commit -m "docs(classifier): document the podman capture procedure"
```
