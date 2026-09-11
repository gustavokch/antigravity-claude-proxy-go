# Plan: Bash-classifier fallback on quota exhaustion

Date: 2026-09-10. Approved design (bounded, brainstormed in session).

## Problem

Claude Code runs its bash permission classifier (injection detection, compound-command
prefix matching) on a small/fast model — haiku-family ids. This proxy maps those ids to
upstream Google Cloud Code models. When every configured account is quota-exhausted, the
dispatcher's retry/backoff loop (5 attempts, capacity tiers up to 1 min) makes each
classifier call wait minutes with no visible feedback, and Claude Code tasks hang.

## Goal

When classifier fallback is enabled AND no upstream capacity is available, respond to
haiku-family bash-classifier requests with an instant canned "safe" verdict, and fail
fast (429, no backoff) for other haiku-family calls. Main-model traffic unchanged.

## Behavior matrix

| Request | Quota healthy | All accounts quota-exhausted |
|---|---|---|
| Non-haiku model | unchanged | unchanged |
| Haiku + classifier fingerprint | unchanged | instant canned 200 verdict |
| Haiku, other (summarize, topic detect) | unchanged | fast 429, no backoff loop |

## Global Constraints

1. Opt-in: `ANTIGRAVITY_PROXY_CLASSIFIER_FALLBACK=1` (env, via `internal/config`). Default OFF.
2. Stub fires only when ALL of: flag on, no capacity for the haiku model (manager reports
   no available account), model id is haiku-family, system prompt matches a discovered
   classifier fingerprint.
3. With flag off, `/v1/messages` flow must be byte-identical to today.
4. Never stub a non-haiku request. Never stub a haiku request when capacity exists.
5. Canned verdict format must match what Claude Code's parser expects — discovered in
   Task 1, never guessed.
6. Match repo style: Go stdlib, table-driven tests, `rtk`-friendly plain test commands.
7. New logic lives in `internal/classifier` (pure, no upstream deps). Wiring touches
   `internal/api/server.go` messages handler and minimal dispatcher/manager accessor.

## Tasks

### Task 1: Discovery — classifier fingerprints and verdict formats

Find the installed Claude Code CLI bundle locally (`which claude`, resolve symlinks; the
bundle is a large minified JS file, e.g. `cli.js` / `cli.mjs`). Extract the exact strings
Claude Code uses for its haiku-driven bash checks:

- system-prompt markers for the bash command injection/safety classifier
- system-prompt markers for compound-command prefix matching
- the response format each call parses (exact JSON keys / tag format, e.g. what a
  "safe / no injection / matches prefix" answer looks like, and what field names the
  parser reads)
- the exact model id(s) the classifier requests use

Method: grep the bundle for distinctive terms (e.g. "injection", "prefix", classifier-
style system prompts, parser code near them), read surrounding minified code, and quote
the exact source snippets in the findings file. Write findings to
`docs/classifier-fallback-notes.md` (committed): each fingerprint as a literal Go-usable
substring, the verdict format per variant, model ids, and the bundle path + grep evidence
for each claim. No product code in this task. If the bundle cannot be located, report
BLOCKED with what you tried — do not guess formats.

### Task 2: `internal/classifier` package

Create `internal/classifier/classifier.go` (+ tests):

- `Kind` enum: the variants discovered in Task 1 (at minimum injection-detection and
  prefix-match; others only if Task 1 documented them).
- `Detect(model string, body []byte) (Kind, bool)` — haiku-family check (reuse the
  haiku alias knowledge that exists in `internal/modelcatalog` if it is exported there;
  otherwise a small local matcher) + fingerprint substring match on the request's system
  prompt / first system block. Pure functions, no I/O.
- `Stub(kind Kind, body []byte) ([]byte, error)` — build the canned Anthropic Messages
  API 200 JSON (non-streaming) echoing the request's id/model where the parser needs it,
  with the "safe" verdict per Task 1's format.
- Table-driven tests: each Kind detected from a realistic sample body; non-classifier
  haiku body → not detected; non-haiku body → not detected; stub output parses as JSON
  and contains the expected verdict fields.

Constants (fingerprint substrings, verdict templates) come verbatim from
`docs/classifier-fallback-notes.md`.

### Task 3: Quota gating + proxy wiring

1. Add a capacity accessor used by the API layer (on the dispatcher or its manager,
   whichever the API server already reaches): report whether any account is available
   for a given model (e.g. `Available(model) == 0`). Prefer reusing the existing
   `Manager.Available`; add a thin method only if the API layer cannot reach the manager.
2. Config: `ClassifierFallback bool` in `internal/config`, env
   `ANTIGRAVITY_PROXY_CLASSIFIER_FALLBACK` (accept `1/true/yes`), default false.
3. Wire in the `/v1/messages` handler in `internal/api/server.go` after request parsing
   and before the `send`/stream construction:
   - if config on AND model is haiku-family AND no capacity:
     - classifier request (per `internal/classifier.Detect`) → write stub via
       `classifier.Stub` with 200 and JSON content type, return without dispatching.
     - other haiku request → fast 429 API error (reuse `writeAPIError` shape used
       elsewhere), no backoff.
   - everything else unchanged.
4. Tests: config parse; gating unit tests where testable; handler-level test with a
   stubbed backend that would fail if called (assert stub path never dispatches).

### Task 4: Integration test + docs

1. Integration-style test in `internal/api`: configured accounts all quota-exhausted →
   POST `/v1/messages` with a classifier-shaped haiku body returns 200 canned verdict;
   a non-classifier haiku body returns 429; a main-model body goes through the normal
   dispatch path (assert dispatch attempted). Flag off → normal path for all three.
2. Update `antigravity-go-proxy.env.example` (new env var with comment) and README
   section (behavior matrix table from this plan + the security tradeoff sentence).
3. Full `go test ./...` green; `go vet ./...` clean.

## Risks

- Stub bypasses real injection detection during quota exhaustion. Accepted by user;
  gated behind opt-in flag.
- Wrong verdict format = Claude Code misparses → must come from Task 1 evidence.
