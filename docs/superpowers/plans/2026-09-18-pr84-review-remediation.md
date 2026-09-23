# PR #84 Review Remediation — classifier rule engine + podman capture harness

**Goal:** Resolve every finding from the fresh two-axis review of PR #84 (`feat/podman-claude-mitm-capture`).
**Spec:** Review comment https://github.com/gustavokch/antigravity-claude-proxy-go/pull/84#issuecomment-5736900155
**Branch head at review time:** `5d35996` (merge-base `2d5dd5d`, 19 commits, 25 files, +2847/-25).
**Tech stack:** Go 1.27rc2 (generics available), Alpine 3 WebUI with embedded JS i18n files, bash + podman capture script.

## Decisions taken with the user

| Question | Decision |
| --- | --- |
| Scope mismatch between PR body and diff | Keep one PR: retitle and rewrite the body to cover the classifier rule engine. No branch split. |
| Duplicated SSE handler and broadcaster | Extract both: one generic replay helper and one shared broadcaster type. |
| External `Containerfile` | Keep the external dependency, document the required contents and the version pin, and fail loudly when the path is missing. |
| ADR-0001 conflict | Write ADR-0003 amending ADR-0001, and add a classifier section to `CONTEXT.md`. |

## Baseline before starting

The branch is green as reviewed: `go build ./...`, `go vet ./...`, and `go test -race -count=8` on `internal/classifier`, `internal/api`, `internal/config`, `internal/logger` all pass. Every task below must end in the same state; the race detector is the gate that protects the previously fixed fan-out bugs.

## Task 0: Prepare the workspace

The primary clone is currently on `feat/throttle-calibration` with uncommitted changes in `internal/accounts/`. Do not switch it. Create a dedicated worktree:

```
git worktree add ../pr84 fork/feat/podman-claude-mitm-capture
```

Run everything below in that worktree, and push from it to `fork feat/podman-claude-mitm-capture`.

---

## Task 1: Fix the workspace leak in the capture script

**Modify:** `scripts/run-claude-mitm-sandbox.sh`

The bug: line 60 registers `trap 'rm -rf "${HOST_WORKSPACE}"' EXIT`, line 75 runs `exec podman run ...`. `exec` replaces the shell process, so the EXIT trap never fires and every capture session leaks a `mktemp -d` directory — contradicting `docs/classifier-fallback-notes.md`, which says the workspace is "discarded when the session exits".

Step 1 — write a shell-level regression check. Add `scripts/run-claude-mitm-sandbox_test.sh` (or a Go test under `internal/...` that shells out, if the repo prefers `go test` as the single gate — check for an existing bash test convention first and follow it). The test stubs `podman` on `PATH` with a script that exits 0, runs the harness with a fake `ANTHROPIC_API_KEY`, captures the printed workspace path, and asserts the directory no longer exists afterwards.

Step 2 — run it, confirm it fails on the leaked directory.

Step 3 — minimal fix: drop `exec`, keep the run in the foreground, and preserve the container's exit status:

```
podman run --rm -it \
  ... \
  "${IMAGE_NAME}" \
  claude
```

Let the EXIT trap do the cleanup, and `exit` with the captured status so the harness still reports container failure.

Step 4 — re-run the test, green. Then `go test ./...`.

Step 5 — `git commit -m "fix(scripts): run podman in-process so the workspace trap fires"`

---

## Task 2: Make the capture procedure reproducible

**Modify:** `scripts/run-claude-mitm-sandbox.sh`, `docs/classifier-fallback-notes.md`

Per the decision, the `Containerfile` stays outside this repo, but the procedure must stop being operator-private.

- In the script: before `podman build`, assert `${CONTAINER_DIR}/Containerfile` exists. On a miss, print the expected path, the `CLAUDE_CONTAINER_DIR` override, and a pointer to the doc section, then `exit 1`. Match the existing fail-closed style used for the missing API key at lines 50-52.
- In the doc: add a prerequisite subsection giving the exact `Containerfile` contents the harness expects, including the `@anthropic-ai/claude-code@2.1.267` pin, so another operator can recreate it byte-for-byte.
- Correct the sentence that claims the script pins the version. The script builds an operator-supplied image; the pin lives in that `Containerfile`.

Commit: `docs(classifier): document the container prerequisite for the capture harness`

---

## Task 3: Correct the recorded verification

**Modify:** `docs/classifier-fallback-notes.md`

The verification bullet says routing was driven by `classifier.action = "reroute_only"`. The feature that shipped routes via `classifier.rules[].action = "reroute"` plus a named entry in `classifier.backends`. The recorded evidence therefore covers a different code path from the one the diff adds.

Two acceptable outcomes; prefer the first:

1. Re-run the capture against the shipped configuration shape and replace the bullet with the real observed result.
2. If a re-run is not practical now, relabel the existing bullet as historical evidence from the earlier prototype, and state plainly that the shipped rule path has unit and end-to-end coverage (`internal/api/classifier_e2e_test.go`) but no live capture yet.

Commit: `docs(classifier): align verification notes with the shipped rule schema`

---

## Task 4: Extract one generic SSE replay helper

**Test:** extend `internal/api/classifier_config_test.go` and `internal/logger/stream_test.go`
**Modify:** `internal/api/management.go`, `internal/api/server.go` as needed

`handleClassifierAuditStream` is a line-for-line clone of `handleLogsStream`: same subscribe-before-replay ordering, same headers, same `maxSeq` dedupe closure, same select loop. The same bug was fixed twice, in `06bd389` and `9cd3608`.

Step 1 — failing test first: add a table-driven test that drives both endpoints through the same assertions — no event lost between subscribe and replay, no duplicate `seq` delivered, and clean teardown on client disconnect. Write it against the shared helper's intended signature so it does not compile yet.

Step 2 — introduce `streamSSE[T any]` (or similar) in a new `internal/api/sse.go`, parameterised by the entry type, a `seq` accessor, a history slice, and a subscribe function. Both handlers become thin adapters.

Step 3 — green, then `go test -race -count=8 ./internal/api/...`.

Commit: `refactor(api): extract one SSE replay helper for logs and audit streams`

---

## Task 5: Share one broadcaster between logger and classifier

**Test:** `internal/logger/stream_test.go`, `internal/classifier/audit_test.go`
**Modify:** `internal/logger/stream.go`, `internal/classifier/audit.go`

`classifier.Recorder` re-implements `logger.Broadcaster`: bounded ring buffer, `subscribers map[chan T]struct{}`, non-blocking fan-out under the write lock, monotonic `seq`. Same shape, two packages.

Step 1 — keep both existing test files as the behavioural contract. Add a race test that hammers `Add` against `Subscribe`/cancel concurrently (2000+ iterations) for whichever package the shared type lands in, so the previously fixed send-on-closed-channel panic cannot regress.

Step 2 — extract a generic `ring.Broadcaster[T]` into a small package (`internal/ringbuf` or similar). Keep `logger.Broadcaster` and `classifier.Recorder` as named wrappers so call sites and JSON field names stay unchanged — the fan-out send must remain inside the write lock, and cancel must close the channel under the same lock.

Step 3 — `go test -race -count=8 ./internal/logger/... ./internal/classifier/... ./internal/api/...`.

This is the highest-blast-radius task: it touches the shared logging path used by everything. If the race gate is not unambiguously green, stop and report rather than pushing.

Commit: `refactor(logger,classifier): share one bounded broadcaster implementation`

---

## Task 6: Collapse the duplicated JSON encoding and envelope construction

**Modify:** `internal/classifier/classifier.go`, `internal/classifier/translation.go`, `internal/classifier/stream.go`

The `SetEscapeHTML(false)` → `Encode` → `TrimSpace` block appears three times, and the Anthropic envelope map is built twice. Extract one `encodeCompactJSON` helper and one envelope constructor inside the `classifier` package. Existing tests in `translation_test.go` and `stream_test.go` cover the behaviour; they must stay green without edits — that is the proof the refactor is behaviour-preserving.

Commit: `refactor(classifier): extract the compact JSON encoder and anthropic envelope`

---

## Task 7: One backend-format adapter

**Test:** `internal/api/classifier_rules_test.go`
**Modify:** `internal/api/classifier_rules.go`

`callClassifierBackend` tests `backend.Format == config.BackendFormatOpenAI` three times — once for the payload, once for the auth header, once for the response. Replace with a single format adapter (an interface or a struct of three funcs selected once at the top of the call), so adding a third format touches one place.

Add a test asserting both formats produce the expected payload shape, auth header, and parsed response, then refactor until green.

Commit: `refactor(api): select one backend format adapter per classifier call`

---

## Task 8: Type the audit event fields

**Test:** `internal/classifier/audit_test.go`
**Modify:** `internal/classifier/audit.go`, `internal/api/classifier_rules.go`

`Event.Status` and `Event.Action` are bare strings whose legal values live only in a comment, while `config.RuleAction` already exists — `Action: string(rule.Action)` discards the type. Give `Status` a named string type with declared constants, and let `Action` carry `config.RuleAction` directly. Keep the JSON wire shape identical; assert that in the test so the WebUI feed is unaffected.

Commit: `refactor(classifier): give audit event status and action named types`

---

## Task 9: Bundle the classifier rule application parameters

**Modify:** `internal/api/classifier_rules.go`

`applyClassifierRule(writer, request, rule, backend, rawBody, model, streamRequested)` takes seven parameters, five of which travel onward together into the helpers below it. Introduce one `classifierRequest` struct carrying `rule`, `backend`, `rawBody`, `model`, `streamRequested`, and pass that plus the writer and request. No behaviour change; existing tests are the gate.

Commit: `refactor(api): bundle classifier rule application parameters`

---

## Task 10: Split the WebUI audit feed from the config editor

**Modify:** `internal/webui/public/js/components/classifier-config.js`, `internal/webui/public/views/settings.html`

`classifier-config.js` now owns form state, backend CRUD, rule CRUD, regex linting, and an EventSource feed. The live interception feed changes for reasons unrelated to the config form. Extract the EventSource feed into its own Alpine component (`window.Components.classifierAuditFeed`) with its own mount point in `settings.html`.

Verify manually in the browser: the feed still streams, the editor still saves, and a save failure still shows the backend's `error` message (the behaviour added in `5d35996`).

Commit: `refactor(webui): split the classifier audit feed into its own component`

---

## Task 11: Translate the remaining hardcoded hint

**Modify:** `internal/webui/public/views/settings.html`, `internal/webui/public/js/translations/en.js`, `internal/webui/public/js/translations/pt.js`

Every label in the new hunks uses `$store.global.t` except one: `Regular expression syntax, e.g. ^You are Claude`. Add a key to `en.js` and `pt.js` and reference it. If `internal/webui/translations_test.go` exists from earlier remediation work, extend its key-parity assertion to cover the new key.

Commit: `fix(webui): translate the classifier regex hint`

---

## Task 12: ADR-0003 and the CONTEXT.md glossary

**Create:** `docs/adr/0003-*.md`
**Modify:** `CONTEXT.md`, and add a supersession or amendment note to ADR-0001

ADR-0001 records as a consequence that transparent forwarding "Eliminates the need to maintain multi-provider translation adapters within the proxy core." This branch adds exactly that: `internal/classifier/translation.go` plus a second outbound forwarding path in `internal/api/classifier_rules.go`. `docs/agents/domain.md` requires the conflict be flagged, not absorbed silently.

ADR-0003 should state the context (operator-defined classifier rerouting needs a format bridge that transparent forwarding deliberately does not provide), the decision, and the explicit scope limit — the adapter is confined to the classifier path and does not apply to the main proxy forwarding path, so ADR-0001 still governs there.

Add a classifier section to `CONTEXT.md` defining Rule, TargetBackend, Interception Rule, and audit Event.

Commit: `docs(adr): record the classifier translation adapter exception to ADR-0001`

---

## Task 13: Document the rule engine for operators

**Modify:** `docs/classifier-fallback-notes.md`, or a new `docs/classifier-rules.md` if the notes file stays capture-specific

The `classifier.rules` and `classifier.backends` config keys, the supported backend formats, the key-redaction behaviour, the match semantics, the fail-open behaviour, and the audit SSE endpoint all ship undocumented. Write the operator-facing reference: a full annotated config example, one worked rerouting example, and a statement of what happens when a backend is unreachable.

Commit: `docs(classifier): document the rule engine configuration`

---

## Task 14: Retitle and rewrite the PR

`gh pr edit 84 --title "feat(classifier): operator-defined rule engine with audit stream and podman capture harness"` (adjust wording to taste), then rewrite the body to describe what the diff actually ships: rule matcher, Anthropic↔OpenAI translation, synthetic SSE re-emit, audit recorder with live fan-out, config schema with key redaction, WebUI rule editor and interception feed, the `internal/logger` `Seq` addition, and the podman capture harness. State the verification actually performed.

---

## Task 15: Verify and push

```
go build ./... && go vet ./...
go test ./...
go test -race -count=8 ./internal/classifier/... ./internal/logger/... ./internal/api/... ./internal/config/...
```

All green, no skipped packages, then push to `fork feat/podman-claude-mitm-capture` and post a short comment on PR #84 mapping each review finding to the commit that closed it. Remove the `../pr84` worktree afterwards.

## Sequencing notes

Tasks 1-3 are independent bug and documentation fixes and can land first, in any order. Task 5 is the riskiest and should land alone, with its own race-gate run, so a regression there is easy to bisect. Tasks 6-9 are behaviour-preserving Go refactors gated by existing tests. Tasks 10-11 are WebUI-only. Tasks 12-13 are documentation. Task 14 is metadata and must happen after the diff is final, so the body describes what actually shipped.
