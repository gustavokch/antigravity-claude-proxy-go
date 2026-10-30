// Starts scripts/client_drift.py when an omp session opens in this repo and an
// installed client (agy, claude, opencode) has changed since the last run.
//
// The check is cheap: `client_drift.py status` compares each client binary's
// path, size and mtime with the last run's. When one moved, the full run starts
// detached (it captures the real clients against their real upstreams, updates
// the pins in a private worktree, runs the tests and opens or refreshes the
// drift PR on the fork), and this session is told when it finishes. Results of
// a run that finished while no session was open are shown at the next start.
//
// Set CLIENT_DRIFT_DISABLED=1 in the environment that starts omp to turn it off.

import { spawn } from "node:child_process"
import { mkdirSync, openSync } from "node:fs"
import { dirname } from "node:path"
import { fileURLToPath } from "node:url"
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent"

const SCRIPT = fileURLToPath(new URL("../../scripts/client_drift.py", import.meta.url))
const REPO = fileURLToPath(new URL("../..", import.meta.url))
const POLL_MS = 60_000

type Last = { status?: string; summary?: string; pr_url?: string | null; reported?: boolean; run_dir?: string }
type Status = { due: boolean; reason: string; running: boolean; changed: string[]; last: Last; log: string }

// Subagents fire session_start too; one check per omp process is enough.
let started = false

function describe(last: Last): { text: string; level: "info" | "warning" | "error" } {
  const where = last.pr_url ?? last.run_dir ?? ""
  const text = `Client drift ${last.status}: ${last.summary ?? ""} ${where}`.trim()
  const level = last.status === "error" || last.status === "local" ? "error" : last.status === "clean" ? "info" : "warning"
  return { text, level }
}

export default function (pi: ExtensionAPI) {
  const readStatus = async (): Promise<Status | undefined> => {
    const result = await pi.exec("python3", [SCRIPT, "status", "--json"], { timeout: 15_000 })
    if (result.code !== 0 || result.killed) return undefined
    try {
      return JSON.parse(result.stdout) as Status
    } catch {
      return undefined
    }
  }

  pi.on("session_start", async (_event, ctx) => {
    if (started || process.env.CLIENT_DRIFT_DISABLED === "1") return
    started = true

    const status = await readStatus()
    if (!status) return

    const report = async (last: Last) => {
      const { text, level } = describe(last)
      ctx.ui.notify(text, level)
      await pi.exec("python3", [SCRIPT, "ack"], { timeout: 15_000 })
    }

    if (status.last?.status && status.last.reported === false && !status.running) {
      await report(status.last)
    }

    if (status.due && !status.running) {
      mkdirSync(dirname(status.log), { recursive: true })
      const out = openSync(status.log, "w")
      spawn("python3", [SCRIPT, "run"], { cwd: REPO, detached: true, stdio: ["ignore", out, out] }).unref()
      ctx.ui.notify(`Client drift check started (${status.reason}). Log: ${status.log}`, "info")
    } else if (!status.running) {
      return
    }

    const timer = ctx.setInterval(async () => {
      const now = await readStatus()
      if (!now || now.running) return
      ctx.clearTimer(timer)
      if (now.last?.reported === false) await report(now.last)
    }, POLL_MS)
  })
}
