# PR #84 Remediation Plan — classifier audit race + review nits

**Goal:** Resolve all findings from the PR #84 review comment, keeping `go test ./...` green.
**Architecture:** `internal/classifier` (Recorder, matcher, translation, synthetic SSE) + `internal/api` (management handlers) + WebUI Alpine component + `scripts/` harness.
**Tech Stack:** Go 1.27, stdlib-only concurrency, Alpine.js WebUI, Podman script.
**Spec reference:** Review comment https://github.com/gustavokch/antigravity-claude-proxy-go/pull/84#issuecomment-5735744672

---

## Task 1: Fix Recorder send-on-closed-channel race (🔴)

**Files:**
- Modify: `internal/classifier/audit.go`
- Test: `internal/classifier/audit_test.go`

**Consumes:** existing `Recorder` API (`Add`, `History`, `Subscribe`).
**Produces:** race-free fan-out; `go test -race ./internal/classifier/` clean.

**Step 1 — failing test:**

```go
func TestRecorderAddDoesNotRaceWithCancel(t *testing.T) {
	for i := 0; i < 2000; i++ {
		recorder := NewRecorder(1)
		_, cancel := recorder.Subscribe(1)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); cancel() }()
		go func() { defer wg.Done(); recorder.Add(Event{RuleID: "x"}) }()
		wg.Wait()
	}
}
```

**Step 2 — confirm failure:** `go test -race -run TestRecorderAddDoesNotRaceWithCancel ./internal/classifier/` — must report a data race (verified on PR head c20f5ad).

**Step 3 — minimal implementation:** in `Add`, move the non-blocking send loop inside the `RLock` critical section (snapshot + send under one `RLock`); in `Subscribe`'s cancel func, perform `delete` + `close(channel)` while holding the write `Lock`. Close then cannot interleave with a send.

**Step 4 — confirm pass:** same command with `-race`, plus existing `audit_test.go`.

**Step 5 — commit:**
```bash
git add internal/classifier/audit.go internal/classifier/audit_test.go
git commit -m "fix(classifier): close audit subscriber channels under lock to avoid send-on-closed-channel panic"
```

---

## Task 2: Close history-replay/subscribe gap in audit stream (🟡)

**Files:**
- Modify: `internal/api/management.go` (`handleClassifierAuditStream`)
- Test: `internal/api/classifier_config_test.go` (`TestClassifierAuditStreamEmitsHistoryAndLiveEvents`)

**Consumes:** `classifierAudit.Subscribe`, `classifierAudit.History`.
**Produces:** SSE stream where no event added after connection open is lost.

**Step 1 — failing test:** extend the SSE test to push an event *between* connection start and first frame read; assert it appears exactly once.

**Step 2 — confirm failure:** `go test -run TestClassifierAuditStream ./internal/api/`.

**Step 3 — minimal implementation:** call `Subscribe` before replaying `History()`; write history first, then drain the live channel; dedupe by `Timestamp`+`RuleID` if an event lands in both.

**Step 4 — confirm pass:** same test command.

**Step 5 — commit:**
```bash
git add internal/api/management.go internal/api/classifier_config_test.go
git commit -m "fix(api): subscribe before history replay in classifier audit stream"
```

---

## Task 3: WebUI system-pattern type safety (🔵)

**Files:**
- Modify: `internal/webui/public/js/components/classifier-config.js` (`setPattern` call sites / `settings.html`)

**Step 1 — change:** relabel the field to make regex explicit (already "System prompt regex") and add client-side guard: on save, try `new RegExp(value)` for regex-type patterns and surface the parse error inline instead of a bare 400.

**Step 2 — manual verify:** enter `(`, observe inline error before save; enter literal text, save succeeds.

**Step 3 — commit:**
```bash
git add internal/webui/public/js/components/classifier-config.js internal/webui/public/views/settings.html
git commit -m "fix(webui): validate classifier system regex before config save"
```

---

## Task 4: Sandbox script container-name collision (🔵)

**Files:**
- Modify: `scripts/run-claude-mitm-sandbox.sh`

**Step 1 — change:** add `--replace` to `podman run` (Podman ≥ 4 supports it) so a stale `claude-mitm-session` is swapped instead of erroring.

**Step 2 — verify:** run script twice without cleanup; second run starts.

**Step 3 — commit:**
```bash
git add scripts/run-claude-mitm-sandbox.sh
git commit -m "fix(scripts): replace stale claude-mitm-session container on rerun"
```

---

## Task 5: Deflake SSE audit test sleeps (🔵)

**Files:**
- Modify: `internal/api/classifier_config_test.go`

**Step 1 — change:** replace `time.Sleep(100 * time.Millisecond)` subscription handshake with a poll loop on subscriber registration (e.g. expose subscriber count on Recorder for tests, or retry-read the stream body until the event appears with a 2s deadline).

**Step 2 — verify:** `go test -race -count=20 -run TestClassifierAuditStream ./internal/api/`.

**Step 3 — commit:**
```bash
git add internal/api/classifier_config_test.go
git commit -m "test(api): poll instead of sleep in classifier audit stream test"
```

---

## Final gate

```bash
go test -race ./...
git push fork feat/podman-claude-mitm-capture
```

Out of scope (no code change): PR title/body retitle — handled in the PR UI.
