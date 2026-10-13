// Blocks `git commit` and `git push` issued through omp's bash tool when
// trufflehog finds a verified or unverifiable secret in what would be recorded
// or published. All scanning logic lives in scripts/secret-scan.sh, which is
// also the local entry point; CI runs the trufflehog action
// (.github/workflows/secret-scan.yml).
//
// Fail-closed on purpose: a scan that errors, times out, or cannot run because
// trufflehog is missing blocks the command, because letting it through is
// exactly the leak this guards against. Set SECRET_SCAN_DISABLED=1 in the
// environment that starts omp to turn the guard off.

import { fileURLToPath } from "node:url"
import type {
  BashToolCallEvent,
  ExtensionAPI,
  ToolCallEvent,
} from "@earendil-works/pi-coding-agent"

const SCAN_SCRIPT = fileURLToPath(new URL("../../scripts/secret-scan.sh", import.meta.url))

// omp's tool_call budget is 30 s (extensionHandlers.toolCallTimeoutMs); stay under it.
const SCAN_TIMEOUT_MS = 25_000

// `git [-C dir] [-c k=v] [--opt] commit|push`. A match inside a quoted string
// only costs an extra scan.
const GIT_PUBLISH =
  /\bgit\b(?:\s+(?:-C\s+\S+|-c\s+\S+|--[\w-]+(?:=\S+)?))*\s+(commit|push)\b([^;&|\n]*)/g

// -a, --all, or a short cluster containing a ("-am", "-sa"): the commit takes
// tracked working-tree edits that are not staged yet.
const COMMIT_ALL = /(?:^|\s)(?:--all\b|-[A-Za-z]*a[A-Za-z]*\b)/

// Local reimplementation of the package's `isToolCallEventType("bash", event)`
// guard: a value import would load the whole package barrel at extension load.
function isBashToolCallEvent(event: ToolCallEvent): event is BashToolCallEvent {
  return event.toolName === "bash"
}

type Scan = { mode: "staged" | "push"; all: boolean }

function scansFor(command: string): Scan[] {
  const scans: Scan[] = []
  for (const match of command.matchAll(GIT_PUBLISH)) {
    if (match[1] === "push") {
      scans.push({ mode: "push", all: false })
    } else {
      scans.push({ mode: "staged", all: COMMIT_ALL.test(match[2] ?? "") })
    }
  }
  return scans
}

export default function (pi: ExtensionAPI) {
  pi.on("tool_call", async (event, ctx) => {
    if (!isBashToolCallEvent(event)) return
    if (process.env.SECRET_SCAN_DISABLED === "1") return
    const command = event.input.command
    if (typeof command !== "string") return

    const scans = scansFor(command)
    if (scans.length === 0) return

    const cwd =
      typeof event.input.cwd === "string" && event.input.cwd !== "" ? event.input.cwd : ctx.cwd
    const seen = new Set<string>()
    for (const scan of scans) {
      const args = scan.all ? [scan.mode, "--all"] : [scan.mode]
      const key = args.join(" ")
      if (seen.has(key)) continue
      seen.add(key)

      // `cd` first: omp may run the command from a different directory, and
      // the script scans the repository that contains its working directory.
      const result = await pi.exec(
        "bash",
        ["-c", 'cd "$1" && shift && exec "$@"', "_", cwd, SCAN_SCRIPT, ...args],
        { timeout: SCAN_TIMEOUT_MS, signal: ctx.signal },
      )
      if (result.code === 0 && !result.killed) continue

      const output = `${result.stdout}\n${result.stderr}`.trim()
      const why = result.killed
        ? `trufflehog scan did not finish within ${SCAN_TIMEOUT_MS / 1000} s`
        : result.code === 1
          ? "trufflehog found a secret"
          : `secret scan could not run (exit ${result.code})`
      return {
        block: true,
        reason:
          `Blocked \`git ${scan.mode === "push" ? "push" : "commit"}\`: ${why}.\n` +
          `${output}\n\n` +
          "Remove the secret from the change (rotate it if it is real), then retry. " +
          "To scan by hand: scripts/secret-scan.sh " +
          `${args.join(" ")}.`,
      }
    }
  })
}
