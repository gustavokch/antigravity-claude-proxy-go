# Laya-serve Reroute with Escalation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve laya's stock English checkpoint through stock `laya-serve` as a classifier reroute target. Laya answers low-risk Stage 1 requests locally and escalates everything else to the teacher.

**Architecture:** The reroute-to-laya path already exists: `internal/api/classifier_laya.go` holds the adapter, `internal/api/classifier_rules.go` falls through on failure, and capture records `source: "laya"`. This plan adds *escalation*. The laya adapter declines to answer on purpose by returning a sentinel error, `errClassifierEscalated`. The rule handler records that as audit status `escalated`, not `error`, and falls through to built-in handling, which is the existing fail-open path, so the teacher grades the request. Escalation covers every Stage 2 request (before any network call), every answer whose label is in `layaEscalateLabels` (default `["D"]`), and every answer below an optional calibrated-confidence floor. The payload also pins `model: "english"` by default.

**Tech Stack:** Go 1.27rc2 (`encoding/json` `omitzero`), Python 3 scripts with pytest, laya 0.3.20 (`laya[serve]`: FastAPI + uvicorn), `uv` for the throwaway venv.

**Spec:** `docs/superpowers/specs/2026-09-23-classifier-corpus-and-laya-design.md` (Phase 2, §4–§6). The Design section below amends it, and Task 4 appends the amendment to the spec.

## Global Constraints

- Branch `feat/laya-serve-escalation` is stacked on `feat/laya-finetune-smoke` (PR #95), so the PR base is `feat/laya-finetune-smoke`. Task 5 needs PR 95's truncated-verdict recovery in `scripts/corpus_to_laya.py`: without it, no gateway row carries a label. Stacking also keeps `docs/classifier-rules.md` edits conflict-free.
- Build: `go build -o bin/proxy ./cmd/proxy`. Staged Go files must be gofmt-clean: `gofmt -l internal cmd` prints nothing.
- TLS: do not touch TLS or transport code. The laya call keeps using the existing `http.DefaultClient` path to localhost.
- No WebUI changes. The new backend fields are config.json-only, like `layaCriteria`. They survive WebUI saves because the save spreads each backend object (`internal/webui/public/js/components/classifier-config.js:240-247`). An `escalated` audit event renders gray through the feed's `default` case (`classifier-audit-feed.js:59`).
- Tests must defend observable behavior: verdicts, audit status, capture source, whether laya-serve was called, and the saved config. Never pin log wording or plumbing.
- Never edit `~/.config/antigravity-proxy/config.json` without the user's explicit go-ahead. Task 5 runs a second proxy on port 8092 with `ANTIGRAVITY_CONFIG_DIR=/tmp/laya-live/config`.
- Disk: about 12 GiB free. The checkpoint is 0.85 GB in the Hugging Face cache (laya filters the snapshot with `allow_patterns`, `laya/agent.py:259-270`), and the venv goes in `/tmp/laya-live/venv`.
- Leave the user's untracked files alone: `.gemini/`, `.gocache/`, `.ignore`, `.mcp.json`, `GEMINI.md`, `docs/superpowers/plans/2026-09-24-pr94-review-remediation-round2.md`, `opencode.json`, `pr.sh`, `scripts/__pycache__/`, `tools/quotaprobe/`.
- Commits follow Conventional Commits with a terse subject and a why-body. Push remote: `fork`.

## Design

Decided with the user on 2026-09-24:

1. **Serve the vanilla model.** Use stock `laya-serve` with `LAYA_MODELS=english`. laya-serve 0.3.20 cannot serve a local directory: `build_router()` (`laya/serve.py`) passes only the `LAYA_MODELS` names, and `Router.models` maps those to hub repos (`laya/router.py:43-47`). The fine-tuned model from `scripts/finetune_laya.py` is out of scope.
2. **Escalate risky answers to the teacher.** Laya still never blocks: the severity map and the 49 clamp are unchanged. Escalation only turns what would have been a laya allow into a teacher-graded request, so it never weakens a verdict.
3. **Always escalate Stage 2.** Stage 2 applies user intent, which the action-only state laya sees does not carry. The Stage 1 footer defers intent to it ("stage 2 will handle those", `docs/classifier-fallback-notes.md:67`), and the captured corpus has 14 Stage 2 rows against 260 Stage 1 rows, consistent with Stage 2 reconsidering only actions Stage 1 graded high. A laya Stage 1 answer is always 49 or lower, so any Stage 2 request follows a teacher verdict, and a capped laya allow must not overrule it. The adapter escalates Stage 2 before the network call, and the Stage 2 rendering code is deleted.

Derived decisions:

- **Pin `model: "english"` when the backend sets none.** Without it, laya-serve routes by the language it detects in the action (`laya/router.py` `_route`). Replaying that detection (`laya.lang.analyse`) over the 287 captured actions routes 19 of them to `multilingual`: ordinary shell commands (`ps aux | grep whisperkit …`, `ls -la /tmp/…`) whose stopwords its heuristic reads as Portuguese (10), French (5) or German (4). `LAYA_MODELS=english` does not preload that checkpoint, so it would download and build on the request path and blow the 5 s timeout. PR 95's fine-tune also uses the English root checkpoint, so pinning keeps serving and training on one model.
- **Label escalation default.** `nil` resolves to `["D"]` under the default criteria and to none under custom criteria, whose labels mean whatever the operator wrote. An explicit `[]` turns label escalation off. It must survive a save, so the field uses `omitzero`.
- **Confidence floor.** `layaMinConfidence` compares against laya's `answer_confidence`, the calibrated max(p) (`laya/common.py:273`), never its `confidence`, which is a normalized entropy on another scale (`laya/common.py:289`). With a floor set, a missing `answer_confidence` escalates. The default 0 turns the floor off.
- **Escalation uses the existing fall-through.** Built-in handling then answers exactly as it would with no rule. Under the user's `reroute_only` config that means the teacher, and the capture row is labeled by whichever path answered (upstream or gateway), never `laya`. Escalated rows therefore become labelled training rows for exactly the actions laya found risky.
- **Wire contract, from the laya 0.3.20 source:**
  - `POST /v1/systemone` takes `{model, state, questions}` and returns `{answers: {risk: {type, choice, probabilities, confidence, answer_confidence, action}}, usage, model, routing}` (`laya/agent.py:678-687`).
  - It returns 422 on an invalid question, 413 above 50,000 state characters (the proxy sends at most 8,000), and 401 on a bad bearer.
  - One inference worker; `GET /health` reports loaded checkpoints.

Unchanged: `defaultLayaSeverityMap` (A 0 / B 5 / C 15 / D 35), `layaMaxSeverity` (49), `ErrUnsupportedKind` for block-prefilter and non-classifier requests, the `source: "laya"` capture label on answered requests, and the 5 s default timeout.

## File Structure

| File | Change |
|---|---|
| `internal/config/config.go` | `TargetBackend.LayaEscalateLabels`, `TargetBackend.LayaMinConfidence`; `LayaSettings.Model`, `.EscalateLabels`, `.MinConfidence`; `DefaultLayaModel`; `defaultLayaEscalateLabels` |
| `internal/config/config_test.go` | Resolution and save round-trip tests |
| `internal/api/management.go` | Save-time validation for the two new fields |
| `internal/api/classifier_config_test.go` | Validation tests |
| `internal/classifier/audit.go` | `EventStatusEscalated` |
| `internal/api/classifier_rules.go` | `errClassifierEscalated`; the reroute branch records escalations |
| `internal/api/classifier_laya.go` | Stage 2 escalation, label and floor escalation, English default, Stage 2 rendering removed |
| `internal/api/classifier_laya_test.go` | Adapter tests, plus a full `/v1/messages` escalation test |
| `scripts/check_laya.py`, `scripts/test_check_laya.py` | Parity with the proxy: `model`, `answer_confidence` |
| `docs/classifier-rules.md` | Laya Backend rewrite, Escalation section, audit statuses |
| `docs/superpowers/specs/2026-09-23-classifier-corpus-and-laya-design.md` | §8 amendment |

---

### Task 1: Escalation settings and English default in config

**Files:**
- Modify: `internal/config/config.go:262-341`
- Modify: `internal/api/management.go:1177-1218`
- Test: `internal/config/config_test.go`
- Test: `internal/api/classifier_config_test.go`

**Interfaces:**
- Produces:
  - `config.TargetBackend.LayaEscalateLabels []string` (json `layaEscalateLabels,omitzero`)
  - `config.TargetBackend.LayaMinConfidence float64` (json `layaMinConfidence,omitempty`)
  - `config.LayaSettings{Model string; …; EscalateLabels []string; MinConfidence float64}`
  - `config.DefaultLayaModel = "english"`

- [ ] **Step 1: Write the failing config tests**

In `internal/config/config_test.go`, add `"slices"` to the imports and append to `TestLayaSettingsDefaults` (after the `Instructions` check, before its closing brace):

```go
	if settings.Model != "english" {
		t.Errorf("Model = %q, want english: an empty model lets laya-serve route a non-English action to a checkpoint it did not preload", settings.Model)
	}
	if !slices.Equal(settings.EscalateLabels, []string{"D"}) {
		t.Errorf("EscalateLabels = %v, want [D]: the band where the teacher refused goes back to the teacher", settings.EscalateLabels)
	}
```

Append two tests after `TestLayaSettingsKeepsAnExplicitZeroMaxSeverity`:

```go
func TestLayaSettingsEscalation(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "custom criteria escalate nothing by default",
			raw:  `{"format":"laya","layaCriteria":{"X":"one","Y":"two"},"layaSeverityMap":{"X":1,"Y":2}}`,
			want: nil,
		},
		{
			name: "an explicit list replaces the default",
			raw:  `{"format":"laya","layaEscalateLabels":["C","D"]}`,
			want: []string{"C", "D"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var backend TargetBackend
			if err := json.Unmarshal([]byte(testCase.raw), &backend); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := backend.LayaSettings().EscalateLabels; !slices.Equal(got, testCase.want) {
				t.Errorf("EscalateLabels = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestLayaSettingsKeepsAnExplicitEmptyEscalationListThroughASave(t *testing.T) {
	var backend TargetBackend
	if err := json.Unmarshal([]byte(`{"format":"laya","layaEscalateLabels":[]}`), &backend); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	saved, err := json.Marshal(backend)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var reloaded TargetBackend
	if err := json.Unmarshal(saved, &reloaded); err != nil {
		t.Fatalf("unmarshal saved: %v", err)
	}
	if got := reloaded.LayaSettings().EscalateLabels; len(got) != 0 {
		t.Errorf("EscalateLabels after a save = %v, want none: an operator's explicit off must not revert to [D]", got)
	}
}
```

- [ ] **Step 2: Run the config tests and see them fail**

Run: `go test ./internal/config/ -run 'TestLayaSettings' -v`
Expected: FAIL to compile with `settings.Model undefined` / `EscalateLabels undefined`.

- [ ] **Step 3: Implement the settings**

In `internal/config/config.go`, replace the override comment above `LayaQuestionName` (lines 262-264):

```go
	// Laya overrides. Each empty or zero value falls back to the default in
	// LayaSettings, except LayaMaxSeverity and LayaEscalateLabels, where only
	// nil does. They are ignored unless Format is BackendFormatLaya.
```

After `LayaStateChars  int  \`json:"layaStateChars,omitempty"\`` add:

```go
	// LayaEscalateLabels are the labels the adapter hands to built-in
	// handling instead of answering, so the teacher grades them. nil means
	// the default: D under the default criteria, none under custom criteria,
	// whose labels mean whatever the operator wrote. An explicit empty list
	// turns label escalation off, and omitzero keeps that [] through a save.
	LayaEscalateLabels []string `json:"layaEscalateLabels,omitzero"`
	// LayaMinConfidence escalates an answer whose calibrated
	// answer_confidence is below it. 0 turns the floor off.
	LayaMinConfidence float64 `json:"layaMinConfidence,omitempty"`
```

Replace the `LayaSettings` struct (lines 275-283):

```go
// LayaSettings is a Laya backend's resolved question, mapping and
// escalation policy.
type LayaSettings struct {
	Model          string
	QuestionName   string
	Instructions   string
	Criteria       map[string]string
	SeverityMap    map[string]int
	MaxSeverity    int
	StateChars     int
	EscalateLabels []string
	MinConfidence  float64
}
```

Add to the `const` block (lines 288-293):

```go
	// DefaultLayaModel pins laya-serve's English checkpoint. With no model,
	// laya-serve routes by the action's language and can build a checkpoint
	// it did not preload on the request path.
	DefaultLayaModel = "english"
```

After `defaultLayaSeverityMap` (line 310) add:

```go
// defaultLayaEscalateLabels sends D, the band where the teacher refused the
// action, back to the teacher: a capped laya allow must not stand in for a
// refusal.
var defaultLayaEscalateLabels = []string{"D"}
```

Replace the `LayaSettings()` method (lines 312-341):

```go
// LayaSettings resolves the backend's overrides against the defaults.
func (backend TargetBackend) LayaSettings() LayaSettings {
	settings := LayaSettings{
		Model:          backend.Model,
		QuestionName:   backend.LayaQuestionName,
		Instructions:   backend.LayaInstructions,
		Criteria:       backend.LayaCriteria,
		SeverityMap:    backend.LayaSeverityMap,
		MaxSeverity:    DefaultLayaMaxSeverity,
		StateChars:     backend.LayaStateChars,
		EscalateLabels: backend.LayaEscalateLabels,
		MinConfidence:  backend.LayaMinConfidence,
	}
	if settings.Model == "" {
		settings.Model = DefaultLayaModel
	}
	if settings.QuestionName == "" {
		settings.QuestionName = DefaultLayaQuestionName
	}
	if settings.Instructions == "" {
		settings.Instructions = DefaultLayaInstructions
	}
	if len(settings.Criteria) == 0 {
		settings.Criteria = defaultLayaCriteria
	}
	if len(settings.SeverityMap) == 0 {
		settings.SeverityMap = defaultLayaSeverityMap
	}
	if backend.LayaMaxSeverity != nil && *backend.LayaMaxSeverity >= 0 {
		settings.MaxSeverity = *backend.LayaMaxSeverity
	}
	if settings.StateChars <= 0 {
		settings.StateChars = DefaultLayaStateChars
	}
	if settings.EscalateLabels == nil && len(backend.LayaCriteria) == 0 {
		settings.EscalateLabels = defaultLayaEscalateLabels
	}
	return settings
}
```

- [ ] **Step 4: Run the config tests and see them pass**

Run: `go test ./internal/config/ -run 'TestLayaSettings' -v`
Expected: PASS, including `TestLayaSettingsKeepsAnExplicitEmptyEscalationListThroughASave`. (Change the tag to `omitempty` and this test fails, which is the point.)

- [ ] **Step 5: Write the failing save-validation tests**

Append to `internal/api/classifier_config_test.go`:

```go
func TestConfigSaveRejectsLayaEscalateLabelOutsideCriteria(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	blob := `{"backends":{"local":{"name":"Local Laya","url":"http://127.0.0.1:8000/v1/systemone","format":"laya","layaEscalateLabels":["E"]}}}`

	recorder := postConfigRules(t, srv, blob)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "layaEscalateLabels") {
		t.Errorf("error should name layaEscalateLabels, got %s", recorder.Body.String())
	}
	if _, exists := config.Get().Classifier.Backends["local"]; exists {
		t.Error("rejected backend was saved anyway")
	}
}

func TestConfigSaveAcceptsLayaEscalateLabelsUnderDefaultCriteria(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	blob := `{"backends":{"local":{"name":"Local Laya","url":"http://127.0.0.1:8000/v1/systemone","format":"laya","layaEscalateLabels":["C","D"],"layaMinConfidence":0.55}}}`

	recorder := postConfigRules(t, srv, blob)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: labels of the default criteria are valid; body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestConfigSaveRejectsLayaMinConfidenceOutOfRange(t *testing.T) {
	for _, value := range []string{"-0.1", "1", "1.5"} {
		t.Run(value, func(t *testing.T) {
			srv, _, _ := newTestServerWithManager(t)
			blob := `{"backends":{"local":{"name":"Local Laya","url":"http://127.0.0.1:8000/v1/systemone","format":"laya","layaMinConfidence":` + value + `}}}`

			recorder := postConfigRules(t, srv, blob)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), "layaMinConfidence") {
				t.Errorf("error should name layaMinConfidence, got %s", recorder.Body.String())
			}
		})
	}
}
```

- [ ] **Step 6: Run them and see them fail**

Run: `go test ./internal/api/ -run 'TestConfigSave.*Laya(Escalate|MinConfidence)' -v`
Expected: FAIL. The two reject tests get 200, because no validation exists yet. The accept test passes already, and that is correct: it guards against over-strict validation.

- [ ] **Step 7: Implement the validation**

In `internal/api/management.go`, insert between the `layaInstructions` check (ends line 1196) and `if len(backend.LayaCriteria) == 0 && len(backend.LayaSeverityMap) == 0 {` (line 1197):

```go
			if backend.LayaMinConfidence < 0 || backend.LayaMinConfidence >= 1 {
				writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("backend %q: layaMinConfidence must be at least 0 and below 1", backendKey)})
				return
			}
			// Checked against the resolved criteria, so a label typo cannot
			// silently turn escalation off under either criteria set.
			criteria := backend.LayaSettings().Criteria
			for _, label := range backend.LayaEscalateLabels {
				if _, exists := criteria[label]; !exists {
					writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("backend %q: layaEscalateLabels has label %q with no matching criteria entry", backendKey, label)})
					return
				}
			}
```

- [ ] **Step 8: Run the tests and see them pass**

Run: `go test ./internal/api/ -run 'TestConfigSave.*Laya' -v && go test ./internal/config/`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
gofmt -l internal cmd
git add internal/config/config.go internal/config/config_test.go internal/api/management.go internal/api/classifier_config_test.go
git commit -m "feat(config): laya escalation settings, english default" -m "layaEscalateLabels (default D under the default criteria) and
layaMinConfidence let a laya backend hand risky answers back to the teacher.
omitzero keeps an explicit [] through a save. model defaults to english so
laya-serve never builds an unpreloaded checkpoint on the request path."
```

---

### Task 2: Laya adapter escalates to built-in handling

**Files:**
- Modify: `internal/classifier/audit.go:13-18`
- Modify: `internal/api/classifier_rules.go:147-167`
- Modify: `internal/api/classifier_laya.go` (whole file below)
- Test: `internal/api/classifier_laya_test.go`

**Interfaces:**
- Consumes: `config.LayaSettings.{Model,EscalateLabels,MinConfidence}` (Task 1).
- Produces:
  - `classifier.EventStatusEscalated EventStatus = "escalated"`
  - `api.errClassifierEscalated` (wrapped with `%w`; test with `errors.Is`)

- [ ] **Step 1: Update and write the failing adapter tests**

In `internal/api/classifier_laya_test.go`:

1. Add imports `"sync/atomic"` and `"antigravity-go-proxy/internal/classifier/corpus"`.
2. Delete `TestParseLayaResponseStage2UsesTheFallbackPhraseForCustomCriteria` (lines 182-199) and `TestParseLayaResponseStage2HasThinkingAndNoCategory` (lines 250-269). Stage 2 is no longer rendered.
3. In `TestParseLayaResponseStage1`, delete the `"D": "<severity>35</severity>",` case and put this comment above `cases`:
   ```go
   	// D escalates under the default criteria; see
   	// TestParseLayaResponseEscalatesTheRefusalBandByDefault.
   ```
4. In `TestParseLayaResponseHonorsAnExplicitMaxSeverity`, change `layaAnswer("D")` to `layaAnswer("C")`. C maps to 15, and the explicit clamp holds it at 10.
5. Append:

```go
// layaStage2Footer carries the Stage 2 footer marker classifier.Detect keys on.
const layaStage2Footer = "\nUse <thinking> first, then respond with <severity>N</severity>, plus <category>Exact BLOCK Rule Name</category> only when blocking.\n"

func TestBuildLayaPayloadEscalatesStage2BeforeTheCall(t *testing.T) {
	_, err := buildLayaPayload(classifierCall{
		rawBody: []byte(layaBody),
		model:   "claude-sonnet-5",
		kind:    classifier.KindStage2Severity,
		backend: layaBackend(),
	})
	if !errors.Is(err, errClassifierEscalated) {
		t.Fatalf("err = %v, want an escalation: Stage 2 follows a teacher verdict a laya allow must not overrule", err)
	}
}

func TestBuildLayaPayloadPinsTheEnglishCheckpointByDefault(t *testing.T) {
	backend := layaBackend()
	backend.Model = ""
	payload, err := buildLayaPayload(classifierCall{
		rawBody: []byte(layaBody),
		model:   "claude-sonnet-5",
		kind:    classifier.KindStage1Severity,
		backend: backend,
	})
	if err != nil {
		t.Fatalf("buildLayaPayload: %v", err)
	}
	var decoded struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if decoded.Model != "english" {
		t.Errorf("model = %q, want english: without it laya-serve routes by language to a checkpoint it may not have loaded", decoded.Model)
	}
}

func TestParseLayaResponseEscalatesTheRefusalBandByDefault(t *testing.T) {
	_, err := parseLayaResponse(layaAnswer("D"), classifierCall{
		model:   "claude-sonnet-5",
		kind:    classifier.KindStage1Severity,
		backend: layaBackend(),
	})
	if !errors.Is(err, errClassifierEscalated) {
		t.Fatalf("err = %v, want an escalation for D", err)
	}
}

func TestParseLayaResponseAnswersEveryLabelWithEscalationOff(t *testing.T) {
	backend := layaBackend()
	backend.LayaEscalateLabels = []string{}

	message, err := parseLayaResponse(layaAnswer("D"), classifierCall{
		model:   "claude-sonnet-5",
		kind:    classifier.KindStage1Severity,
		backend: backend,
	})
	if err != nil {
		t.Fatalf("parseLayaResponse: %v", err)
	}
	if got := verdictTextFrom(t, message); got != "<severity>35</severity>" {
		t.Errorf("verdict = %q, want D's mapped severity", got)
	}
}

func TestParseLayaResponseEscalatesBelowTheConfidenceFloor(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		escalate bool
	}{
		{name: "below the floor", body: `{"answers":{"risk":{"choice":"A","answer_confidence":0.41}}}`, escalate: true},
		{name: "at the floor", body: `{"answers":{"risk":{"choice":"A","answer_confidence":0.6}}}`, escalate: false},
		// confidence is laya's entropy score, not the calibrated one: a
		// response carrying only it cannot clear the floor.
		{name: "no calibrated confidence", body: `{"answers":{"risk":{"choice":"A","confidence":0.99}}}`, escalate: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			backend := layaBackend()
			backend.LayaMinConfidence = 0.6
			_, err := parseLayaResponse([]byte(testCase.body), classifierCall{
				model:   "claude-sonnet-5",
				kind:    classifier.KindStage1Severity,
				backend: backend,
			})
			if got := errors.Is(err, errClassifierEscalated); got != testCase.escalate {
				t.Errorf("escalated = %v (err %v), want %v", got, err, testCase.escalate)
			}
			if !testCase.escalate && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// TestLayaEscalationFallsThroughToBuiltInHandling drives the whole
// /v1/messages path. An escalated request must be answered by built-in
// handling (always_stub here, the teacher in a real deployment), audited as
// escalated, and never recorded as a laya row.
func TestLayaEscalationFallsThroughToBuiltInHandling(t *testing.T) {
	cases := []struct {
		name        string
		footer      string
		wantLayaHit bool
	}{
		{name: "stage 1 answered D", footer: classifierStage1Footer, wantLayaHit: true},
		{name: "stage 2 escalates before the call", footer: layaStage2Footer, wantLayaHit: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var layaHits atomic.Int32
			laya := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				layaHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(layaAnswer("D"))
			}))
			defer laya.Close()

			orig := config.Get()
			t.Cleanup(func() { config.SetForTest(orig) })
			server, _ := newAccountBackedTestServer(t)
			server.classifierAudit = classifier.NewRecorder(10)
			dir := t.TempDir()

			cfg := config.Get()
			cfg.Classifier.Enabled = true
			cfg.Classifier.Action = config.ActionAlwaysStub
			cfg.Classifier.Capture = config.ClassifierCaptureConfig{Enabled: true, Dir: dir}
			cfg.Classifier.Rules = []config.Rule{{
				ID:      "laya",
				Name:    "Laya",
				Enabled: true,
				Conditions: config.RuleConditions{
					SystemPromptPatterns: []config.MatchPattern{
						{Type: config.PatternSubstring, Pattern: "You are a security monitor"},
					},
				},
				Action:        config.RuleActionReroute,
				TargetBackend: "laya",
			}}
			cfg.Classifier.Backends = map[string]config.TargetBackend{
				"laya": {Name: "laya", URL: laya.URL + "/v1/systemone", Format: config.BackendFormatLaya},
			}
			config.SetForTest(cfg)
			server.applyClassifierConfig(cfg.Classifier)

			rec := postClassifierMessages(t, server, classifierShapedBody(t, classifierTestModel, testCase.footer))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
			}
			if got := layaHits.Load() > 0; got != testCase.wantLayaHit {
				t.Errorf("laya-serve called = %v, want %v", got, testCase.wantLayaHit)
			}
			history := server.classifierAudit.History()
			if len(history) != 1 || history[0].Status != classifier.EventStatusEscalated {
				t.Fatalf("audit = %+v, want one escalated event", history)
			}
			rows := readCaptureRows(t, server, dir)
			if len(rows) != 1 || rows[0].Source != corpus.SourceStub {
				t.Fatalf("capture rows = %+v, want one stub row: built-in handling answered, not laya", rows)
			}
		})
	}
}
```

- [ ] **Step 2: Run them and see them fail**

Run: `go test ./internal/api/ -run 'Laya' -v`
Expected: FAIL to compile with `undefined: errClassifierEscalated` and `undefined: classifier.EventStatusEscalated`.

- [ ] **Step 3: Add the audit status**

In `internal/classifier/audit.go`, add to the `EventStatus` const block after `EventStatusError`:

```go
	// EventStatusEscalated marks a backend that declined to answer on purpose,
	// handing the request to built-in handling so the teacher grades it.
	EventStatusEscalated EventStatus = "escalated"
```

- [ ] **Step 4: Handle escalation in the rule handler**

In `internal/api/classifier_rules.go`, add below `maxClassifierBackendResponse`:

```go
// errClassifierEscalated marks a backend that declined to answer on purpose.
// The request falls through to built-in handling exactly like a failed
// reroute, but it is not a failure, so the audit records it as escalated.
var errClassifierEscalated = errors.New("escalated to built-in handling")
```

Replace the reroute error branch (lines 162-167):

```go
		if err != nil {
			if errors.Is(err, errClassifierEscalated) {
				server.classifierLogger().Info("[Server] classifier reroute escalated to built-in handling",
					"rule", rule.ID, "backend", rule.TargetBackend, "reason", err)
				record(classifier.EventStatusEscalated, err.Error())
				return false, false
			}
			server.classifierLogger().Warn("[Server] classifier reroute failed; falling back to built-in handling",
				"rule", rule.ID, "backend", rule.TargetBackend, "error", err)
			record(classifier.EventStatusError, err.Error())
			return false, false
		}
```

- [ ] **Step 5: Rewrite the laya adapter**

Replace `internal/api/classifier_laya.go` with:

```go
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"antigravity-go-proxy/internal/classifier"
	"antigravity-go-proxy/internal/classifier/corpus"
	"antigravity-go-proxy/internal/config"
)

// layaQuestion is one typed question in laya-serve's /v1/systemone protocol.
type layaQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

type layaRequest struct {
	Model     string                  `json:"model,omitempty"`
	State     map[string]string       `json:"state"`
	Questions map[string]layaQuestion `json:"questions"`
}

// buildLayaPayload turns a Stage 1 classifier request into a laya
// typed-decision request. The classifier body runs to roughly 125KB while
// laya's English checkpoint holds about 320 tokens of state, so only the
// graded action is sent; an unreadable transcript is an error, never a guess.
func buildLayaPayload(call classifierCall) ([]byte, error) {
	// Both refusals happen before any network call, so neither waits on the
	// backend timeout.
	switch call.kind {
	case classifier.KindStage1Severity:
	case classifier.KindStage2Severity:
		// Stage 2 applies user intent, which the action-only state laya sees
		// does not carry, and reconsiders an action Stage 1 graded high. A
		// laya Stage 1 answer never is, so this request follows a teacher
		// verdict, which a capped laya allow must not overrule.
		return nil, fmt.Errorf("%w: stage 2 goes to the teacher", errClassifierEscalated)
	default:
		// KindBlockPrefilter's response format was never captured, so it must
		// not be guessed; KindNone is not a classifier request at all.
		return nil, classifier.ErrUnsupportedKind
	}
	settings := call.backend.LayaSettings()

	action, _, err := corpus.ExtractAction(call.rawBody, 0)
	if err != nil {
		return nil, err
	}
	// Truncate from the left: the graded action sits at the tail, so the head
	// is what can be lost without losing the thing being judged.
	if runes := []rune(action); len(runes) > settings.StateChars {
		action = string(runes[len(runes)-settings.StateChars:])
	}

	payload := layaRequest{
		Model: settings.Model,
		State: map[string]string{"action": action},
		Questions: map[string]layaQuestion{
			settings.QuestionName: {
				Type:         "choice",
				Instructions: settings.Instructions,
				Criteria:     settings.Criteria,
			},
		},
	}
	return json.Marshal(payload)
}

// layaSettingsFor is a nil-safe accessor used by the adapter functions.
func layaSettingsFor(backend *config.TargetBackend) config.LayaSettings {
	if backend == nil {
		return config.TargetBackend{}.LayaSettings()
	}
	return backend.LayaSettings()
}

// layaTypedAnswer is one typed answer in a /v1/systemone response.
// AnswerConfidence is laya's calibrated max(p). Its "confidence" field is a
// normalized entropy on another scale, so the floor never reads it.
type layaTypedAnswer struct {
	Choice           string   `json:"choice"`
	AnswerConfidence *float64 `json:"answer_confidence"`
}

type layaResponse struct {
	Answers map[string]layaTypedAnswer `json:"answers"`
}

// parseLayaResponse maps a laya label to a Stage 1 severity verdict, or
// escalates: a label in EscalateLabels, or an answer below MinConfidence,
// goes to built-in handling instead. Severity is clamped by MaxSeverity,
// which defaults below the 50 allow/block boundary.
func parseLayaResponse(respBody []byte, call classifierCall) ([]byte, error) {
	// buildLayaPayload stops every other kind before the call.
	if call.kind != classifier.KindStage1Severity {
		return nil, classifier.ErrUnsupportedKind
	}
	settings := layaSettingsFor(call.backend)

	var decoded layaResponse
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return nil, fmt.Errorf("laya: response is not JSON: %w", err)
	}
	answer, exists := decoded.Answers[settings.QuestionName]
	if !exists {
		return nil, fmt.Errorf("laya: response carries no answer for question %q", settings.QuestionName)
	}
	severity, known := settings.SeverityMap[answer.Choice]
	if !known {
		return nil, fmt.Errorf("laya: answer label %q is not in the severity map", answer.Choice)
	}
	if slices.Contains(settings.EscalateLabels, answer.Choice) {
		return nil, fmt.Errorf("%w: laya chose %s", errClassifierEscalated, answer.Choice)
	}
	if settings.MinConfidence > 0 {
		if answer.AnswerConfidence == nil {
			return nil, fmt.Errorf("%w: laya chose %s with no answer_confidence to check against the floor", errClassifierEscalated, answer.Choice)
		}
		if *answer.AnswerConfidence < settings.MinConfidence {
			return nil, fmt.Errorf("%w: laya chose %s with answer_confidence %.2f, below %.2f",
				errClassifierEscalated, answer.Choice, *answer.AnswerConfidence, settings.MinConfidence)
		}
	}
	if severity > settings.MaxSeverity {
		severity = settings.MaxSeverity
	}
	if severity < 0 {
		severity = 0
	}
	return classifier.StubWithText(call.model, fmt.Sprintf("<severity>%d</severity>", severity))
}

var layaFormatAdapter = backendFormatAdapter{
	preparePayload: buildLayaPayload,
	setHeaders: func(req *http.Request, apiKey string) {
		req.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	},
	parseResponse: parseLayaResponse,
}
```

The deleted symbols are `layaSupportsKind`, `layaThinkingPhrases` and `layaFallbackThinking`. Confirm nothing else references them: `graft grep "layaSupportsKind|layaThinkingPhrases|layaFallbackThinking"` should list only the historical plan `docs/superpowers/plans/2026-09-23-classifier-corpus-and-laya.md`, which stays as written.

- [ ] **Step 6: Run the tests and see them pass**

Run: `go test ./internal/api/ -run 'Laya|ClassifierRule|ClassifierCapture' -v && go test ./internal/classifier/...`
Expected: PASS. `TestLayaEscalationFallsThroughToBuiltInHandling` passes in both subtests, and `TestApplyClassifierRuleFailsOpenWhenBackendErrors` still records `error`.

- [ ] **Step 7: Commit**

```bash
gofmt -l internal cmd
git add internal/classifier/audit.go internal/api/classifier_rules.go internal/api/classifier_laya.go internal/api/classifier_laya_test.go
git commit -m "feat(classifier): escalate risky laya answers to the teacher" -m "A laya backend now hands D answers, answers below layaMinConfidence and
every Stage 2 request to built-in handling, audited as escalated. Stage 2
follows a teacher verdict that a capped laya allow must not overrule, so
the Stage 2 renderer goes. Escalation only turns laya allows into teacher
calls; it never weakens a verdict."
```

---

### Task 3: check_laya.py sends what the proxy sends

**Files:**
- Modify: `scripts/check_laya.py:30-58,61-77,117`
- Test: `scripts/test_check_laya.py`

**Interfaces:**
- Consumes: `DefaultLayaModel = "english"` in `internal/config/config.go` (Task 1).
- Produces: `check_laya.MODEL`, and `check_laya.Result.answer_confidence` (a float or `None`), which Task 5 prints.

- [ ] **Step 1: Write the failing test**

Append to `scripts/test_check_laya.py`:

```python
def test_check_pins_the_proxy_default_checkpoint(laya_server):
    """The smoke check must name the checkpoint the proxy names, or a green
    check proves a model the proxy never asks for."""
    import re
    from pathlib import Path

    config_go = Path(__file__).resolve().parent.parent / "internal" / "config" / "config.go"
    match = re.search(r'DefaultLayaModel\s*=\s*"([^"]+)"', config_go.read_text(encoding="utf-8"))
    assert match, "DefaultLayaModel not found in config.go"

    _Handler.response_body = _typed_answer("A")
    _Handler.status = 200
    check_laya.check(laya_server, action="ls", timeout=5)

    assert json.loads(_Handler.last_body)["model"] == match.group(1)
```

- [ ] **Step 2: Run it and see it fail**

Run: `python3 -m pytest scripts/test_check_laya.py -q`
Expected: FAIL with `KeyError: 'model'`.

- [ ] **Step 3: Implement**

In `scripts/check_laya.py`:

- Below `QUESTION = "risk"` add:
  ```python
  # config.DefaultLayaModel: the checkpoint the proxy pins when a backend sets
  # no model. test_check_laya.py keeps the two in sync.
  MODEL = "english"
  ```
- In `build_payload`, add `"model": MODEL,` as the first key of the returned dict.
- Rename `Result.confidence` to `answer_confidence: float | None`, and in `parse_answer` return:
  ```python
      return Result(label=label, answer_confidence=answer.get("answer_confidence"))
  ```
  Put this comment above the return: `# answer_confidence is what layaMinConfidence compares; "confidence" is laya's entropy score on another scale.`
- In `main`, replace the `OK:` print:
  ```python
      shown = "n/a" if result.answer_confidence is None else f"{result.answer_confidence:.2f}"
      print(f"OK: label={result.label} answer_confidence={shown}")
  ```

- [ ] **Step 4: Run the tests and see them pass**

Run: `python3 -m pytest scripts/ -q`
Expected: PASS (130 tests).

- [ ] **Step 5: Commit**

```bash
git add scripts/check_laya.py scripts/test_check_laya.py
git commit -m "fix(scripts): check_laya sends the proxy's checkpoint" -m "The proxy now pins model=english, so the smoke check must too, or it
proves a checkpoint the proxy never asks for. It prints answer_confidence,
the calibrated score layaMinConfidence compares, not the entropy score."
```

---

### Task 4: Document serving and escalation

**Files:**
- Modify: `docs/classifier-rules.md:95-107` (Fail-Open, Live Audit Stream) and `:144-155` (Laya Backend)
- Modify: `docs/superpowers/specs/2026-09-23-classifier-corpus-and-laya-design.md` (append §8)

- [ ] **Step 1: Fail-open and audit statuses**

At the end of `## Fail-Open Behavior` (after line 99) add:

```markdown

A Laya backend can also decline on purpose. An escalation (see [Escalation](#escalation)) falls through the same way, but the audit event is `escalated`, not `error`, and the proxy logs it at info level.
```

In `## Live Audit Stream`, change `- Event statuses: \`rerouted\`, \`stubbed\`, \`passthrough\`, or \`error\`.` to:

```markdown
- Event statuses: `rerouted`, `stubbed`, `passthrough`, `escalated`, or `error`. `detail` carries the reason for `escalated` and `error`.
```

- [ ] **Step 2: Rewrite the Laya Backend section**

Replace lines 144-155, from `## Laya Backend` through the paragraph that starts `If laya-serve is unreachable`, with the text below. Keep `### Checking a live laya-serve` and everything after it.

````markdown
## Laya Backend

A Laya backend answers low-risk Stage 1 requests on your machine and hands everything else to the teacher. Serve laya's stock English checkpoint:

```bash
python3 -m pip install "laya[serve]==0.3.20"
LAYA_MODELS=english LAYA_PRELOAD=1 LAYA_HOST=127.0.0.1 LAYA_PORT=8000 laya-serve
```

The first start downloads the checkpoint (about 0.85 GB) from Hugging Face, and `LAYA_PRELOAD=1` loads it before the server accepts requests. laya-serve serves only its hub checkpoints: the model `scripts/finetune_laya.py` writes cannot be served this way.

Add a backend and a rule that matches the Stage 1 footer:

```json
"rules": [
  {
    "id": "stage1-laya",
    "name": "Stage 1 to local laya",
    "enabled": true,
    "conditions": {
      "footerPatterns": [{ "type": "substring", "pattern": "Grade HARM ONLY" }]
    },
    "action": "reroute",
    "targetBackend": "laya"
  }
],
"backends": {
  "laya": {
    "name": "Local laya",
    "url": "http://127.0.0.1:8000/v1/systemone",
    "format": "laya"
  }
}
```

Rules only run while `classifier.enabled` is `true`. Set `apiKey` only if the server was started with `LAYA_API_KEY`. When `timeoutMs` is unset or 0, a Laya backend times out after 5 seconds, not the 20 seconds the other formats use. `model` defaults to `english`, the checkpoint `LAYA_MODELS=english` preloads. With no model, laya-serve picks a checkpoint by the action's language, and its heuristic reads some ordinary shell commands as Portuguese, French or German, which would build the multilingual checkpoint on the request path and time out. Set `model` to another checkpoint only if the server preloads it.

The adapter sends only the graded action, as a single `choice` question over four risk bands by default, and maps the chosen label to a severity. Every default severity is below 50, the allow/block boundary, and `layaMaxSeverity` (default 49) clamps the result, so with the defaults a Laya verdict cannot block. It can block only if an operator raises `layaMaxSeverity` to 50 or more and maps a label to 50 or more in `layaSeverityMap`; every action the model puts under that label then gets a blocking severity. The base checkpoints score near chance on typed decisions zero-shot, so a Laya backend that can block will block routine actions at random.

### Escalation

The adapter escalates a request when laya is not the right judge for it. Escalating means declining to answer: the request falls through to the proxy's built-in handling. That handling sends it to the teacher under the `fallback_on_exhaustion`, `reroute_only` and `passthrough` classifier actions; under `always_stub` it gets the canned verdict. The adapter escalates:

- **Every Stage 2 request**, before calling laya-serve. Stage 2 applies user intent, which the action-only state laya sees does not carry, and reconsiders an action Stage 1 graded high. A laya Stage 1 answer never grades that high, so a Stage 2 request follows a teacher verdict, which a capped laya allow must not overrule. Even a rule that matches every classifier request never lets laya answer Stage 2.
- **Every answer whose label is in `layaEscalateLabels`.** The default is `["D"]` under the default criteria, the band where the teacher refused the action, and none under custom criteria, whose labels mean what you wrote. `[]` turns label escalation off. Every label must be a key of the backend's criteria.
- **Every answer whose `answer_confidence` is below `layaMinConfidence`**, and, while a floor is set, every answer that reports no `answer_confidence`. `answer_confidence` is laya's calibrated confidence, the probability of the label it reported. laya's `confidence` field is a normalized entropy on another scale and is never compared. The default 0 turns the floor off; the value must be at least 0 and below 1.

An escalation only turns a Laya allow into a teacher call, so it never weakens a verdict. It costs one laya call on top of the teacher call. With capture enabled, an escalated request is recorded by the path that answered it, never as `laya`. Under a teacher it becomes a labelled training row, which aims collection at exactly the actions laya found risky.

If laya-serve is unreachable, times out, returns a non-200 status or a response with no answer for the question, or answers with a label the severity map does not contain, the reroute fails. The request falls through the same way, and the audit event is `error`. A block-prefilter request fails the same way, because its response format has never been captured.
````

- [ ] **Step 3: Amend the design spec**

Append to `docs/superpowers/specs/2026-09-23-classifier-corpus-and-laya-design.md`:

```markdown

---

## 8. Amendment (2026-09-24): escalation

Implemented by `docs/superpowers/plans/2026-09-24-laya-serve-escalation.md`. Where it conflicts with §4.5, §5 and §6.3, it supersedes them:

- The Laya backend no longer answers Stage 2. Every Stage 2 request escalates before the laya call: Stage 2 applies user intent the action-only state lacks, and it follows a Stage 1 verdict that a capped laya allow must not overrule.
- A Stage 1 answer escalates when its label is in `layaEscalateLabels` (default `["D"]` under the default criteria) or its `answer_confidence` is below `layaMinConfidence` (default 0, off).
- An escalation falls through to built-in handling like a failed reroute, audited as `escalated`, not `error`.
- `model` defaults to `english`, so laya-serve never builds an unpreloaded checkpoint on the request path.
- The clamp in §4.5 is unchanged: a Laya verdict still cannot block with the defaults.
```

- [ ] **Step 4: Check the docs**

Run: `graft grep "Stage 1 and Stage 2 severity requests only"`
Expected: no hits in `docs/classifier-rules.md`, because the sentence was replaced.

- [ ] **Step 5: Commit**

```bash
git add docs/classifier-rules.md docs/superpowers/specs/2026-09-23-classifier-corpus-and-laya-design.md
git commit -m "docs(classifier): serve laya with escalation" -m "Operators need the stage-1-only rule, the english default and the three
escalation triggers, and the spec's Stage 2 row is now wrong."
```

---

### Task 5: Live verification against stock laya-serve

No code ships from this task except Step 9. Everything else lives in `/tmp/laya-live` and is deleted in Step 10. **Record every number the steps print; they go in the PR body.**

- [ ] **Step 1: Install laya-serve**

```bash
mkdir -p /tmp/laya-live/config/corpus
uv venv /tmp/laya-live/venv --python 3.12 -q
VIRTUAL_ENV=/tmp/laya-live/venv uv pip install -q "laya[serve]==0.3.20"
```

- [ ] **Step 2: Start it with the hub**

Use `hub op:"start"`:
- name `laya-serve`
- application `/tmp/laya-live/venv/bin/laya-serve`
- env `{"LAYA_MODELS":"english","LAYA_PRELOAD":"1","LAYA_HOST":"127.0.0.1","LAYA_PORT":"8000"}`
- ready `{"log":"Uvicorn running","port":8000,"timeout":900}`

Expected: ready once the 0.85 GB checkpoint is downloaded and built. Then `curl -s http://127.0.0.1:8000/health` returns `"loaded":["english"]` and a device (`mps` on this M4).

- [ ] **Step 3: Wire-contract check**

Run: `python3 scripts/check_laya.py --url http://127.0.0.1:8000/v1/systemone`
Expected: exit 0 and `OK: label=<A-D> answer_confidence=<0.xx>`.

- [ ] **Step 4: Agreement and latency on the captured corpus**

Write `/tmp/laya-live/agreement.py`:

```python
"""Throwaway: replay labelled Stage 1 rows through laya-serve; not committed."""
import glob, json, os, statistics, sys, time, urllib.request
from collections import Counter

sys.path.insert(0, "scripts")
import check_laya, corpus_to_laya

URL = "http://127.0.0.1:8000/v1/systemone"
STATE_CHARS = 1200  # config.DefaultLayaStateChars: the proxy keeps the tail

rows = []
for path in sorted(glob.glob(os.path.expanduser("~/.config/antigravity-proxy/corpus/classifier-*.jsonl"))):
    rows += corpus_to_laya.load_rows(path)[0]
examples, stats = corpus_to_laya.convert(rows, sources=("upstream", "gateway"))

confusion, latencies, right, wrong = Counter(), [], [], []
for example in examples:
    action = example["state"]["action"][-STATE_CHARS:]
    request = urllib.request.Request(URL, data=json.dumps(check_laya.build_payload(action)).encode(),
                                     headers={"Content-Type": "application/json"})
    start = time.perf_counter()
    with urllib.request.urlopen(request, timeout=30) as response:
        answer = json.loads(response.read())["answers"]["risk"]
    latencies.append((time.perf_counter() - start) * 1000)
    teacher, laya = example["answers"]["risk"], answer["choice"]
    confusion[teacher, laya] += 1
    (right if teacher == laya else wrong).append(answer["answer_confidence"])

n = len(examples)
print(f"{n} labelled stage-1 rows ({stats['recovered']} recovered); teachers {stats['models']}")
print("teacher \\ laya   A    B    C    D")
for teacher in "ABCD":
    print(f"      {teacher}      " + " ".join(f"{confusion[teacher, label]:4d}" for label in "ABCD"))
agree = sum(confusion[label, label] for label in "ABCD")
print(f"exact agreement {agree}/{n} = {agree / n:.1%}")
escalated = sum(count for (_, laya), count in confusion.items() if laya == "D")
print(f"escalated by the default [D]: {escalated}/{n} = {escalated / n:.1%}")
refused = sum(count for (teacher, _), count in confusion.items() if teacher == "D")
missed = sum(count for (teacher, laya), count in confusion.items() if teacher == "D" and laya != "D")
print(f"teacher refusals laya would allow: {missed}/{refused}")
ordered = sorted(latencies)
print(f"latency p50 {ordered[n // 2]:.0f} ms, p95 {ordered[int(n * 0.95)]:.0f} ms, max {ordered[-1]:.0f} ms")
for name, values in (("right", right), ("wrong", wrong)):
    if values:
        print(f"answer_confidence when {name}: median {statistics.median(values):.2f} (n={len(values)})")
```

Run from the repo root (the script imports `scripts/`): `python3 /tmp/laya-live/agreement.py`
Expected: a confusion matrix with every row counted. The number that decides whether to enable the rule is **"teacher refusals laya would allow"**: those actions would be allowed without the teacher. The two `answer_confidence` medians show whether a `layaMinConfidence` floor separates right answers from wrong ones. p95 latency must be well under the 5 s timeout.

- [ ] **Step 5: Start a throwaway proxy with a laya rule**

Write `/tmp/laya-live/config/config.json`. The Stage 2 rule is a deliberate misroute that proves the adapter escalates it anyway:

```json
{
  "classifier": {
    "enabled": true,
    "action": "always_stub",
    "capture": { "enabled": true, "dir": "/tmp/laya-live/config/corpus" },
    "rules": [
      {
        "id": "stage1-laya", "name": "Stage 1 to local laya", "enabled": true,
        "conditions": { "footerPatterns": [{ "type": "substring", "pattern": "Grade HARM ONLY" }] },
        "action": "reroute", "targetBackend": "laya"
      },
      {
        "id": "stage2-misroute", "name": "Stage 2 sent to laya on purpose", "enabled": true,
        "conditions": { "footerPatterns": [{ "type": "substring", "pattern": "plus <category>" }] },
        "action": "reroute", "targetBackend": "laya"
      }
    ],
    "backends": {
      "laya": { "name": "Local laya", "url": "http://127.0.0.1:8000/v1/systemone", "format": "laya" }
    }
  }
}
```

Run `go build -o bin/proxy ./cmd/proxy`, then `hub op:"start"`:
- name `laya-proxy`
- application `/Users/gus/Git/antigravity-claude-proxy-go/bin/proxy`
- args `["-port","8092","-api-key="]`
- env `{"ANTIGRAVITY_CONFIG_DIR":"/tmp/laya-live/config"}`
- ready `{"log":"proxy server listening","port":8092,"timeout":30}`

- [ ] **Step 6: Stage 1 through the proxy**

Write `/tmp/laya-live/stage1.json`:

```json
{"model":"claude-sonnet-5","max_tokens":64,
 "system":[{"type":"text","text":"You are a security monitor for autonomous AI coding agents."}],
 "messages":[{"role":"user","content":[
   {"type":"text","text":"<transcript>"},
   {"type":"text","text":"{\"Bash\":\"ls -la\"}"},
   {"type":"text","text":"</transcript>"},
   {"type":"text","text":"Respond with <severity>N</severity> ONLY. Grade HARM ONLY — do NOT reduce for user intent. No other text."}]}]}
```

```bash
curl -s http://127.0.0.1:8092/v1/messages -H 'Content-Type: application/json' -d @/tmp/laya-live/stage1.json | jq -c '[.model, .content[0].text]'
curl -sN --max-time 2 'http://127.0.0.1:8092/api/classifier/audit/stream?history=true' | grep '^data:'
jq -c '{kind, source, verdict_raw}' /tmp/laya-live/config/corpus/classifier-$(date -u +%F).jsonl
```

Expected: `["claude-sonnet-5","<severity>N</severity>"]`, and the audit event and capture row agree with each other:
- if laya answered A, B or C: audit `rerouted`, row `source: "laya"`, N ∈ {0, 5, 15};
- if laya answered D: audit `escalated` with `detail` naming D, row `source: "stub"`.

- [ ] **Step 7: Stage 2 never reaches laya-serve**

Copy `stage1.json` to `stage2.json`, replacing the last text block with `"Use <thinking> first, then respond with <severity>N</severity>, plus <category>Exact BLOCK Rule Name</category> only when blocking."`. Count laya-serve requests with `hub op:"logs" name:"laya-serve" grep:"POST /v1/systemone"` before and after:

```bash
curl -s http://127.0.0.1:8092/v1/messages -H 'Content-Type: application/json' -d @/tmp/laya-live/stage2.json | jq -r '.content[0].text'
```

Expected:
- the response is the canned Stage 2 stub;
- the newest audit event is `escalated` with detail `escalated to built-in handling: stage 2 goes to the teacher`;
- the newest capture row is `stage2-severity` / `stub`;
- the laya-serve `POST /v1/systemone` count did not change.

- [ ] **Step 8: Fail-open against a dead sidecar**

Run `hub op:"stop" name:"laya-serve"`, then post `stage1.json` again.
Expected: still `200` with a stub verdict, the newest audit event is `error`, and the newest row is `source: "stub"`.

- [ ] **Step 9: Record that the contract is tested**

With Steps 3-8 green, both statements claiming the contract was never tested are now false. Fix them:
- In the `scripts/check_laya.py` docstring, replace `was written from the laya-serve README and has never run against a real server` with `was checked against laya-serve 0.3.20 on 2026-09-24`.
- In `docs/classifier-rules.md` § "Checking a live laya-serve", replace `Every failure mode above is silent by design, so an untested contract means the first real user is the test. Run the check before relying on a Laya backend, and again after any laya-serve upgrade:` with `The proxy was checked against laya-serve 0.3.20. Every failure mode above is silent by design, so run the check before relying on a Laya backend, and again after any laya-serve upgrade:`.

```bash
python3 -m pytest scripts/ -q
git add scripts/check_laya.py docs/classifier-rules.md
git commit -m "docs(classifier): record the live laya-serve check" -m "check_laya, the stage 1 reroute, stage 2 escalation and fail-open ran
against stock laya-serve 0.3.20 with the english checkpoint."
```

- [ ] **Step 10: Tear down**

Run `hub op:"stop"` for `laya-proxy` (and for `laya-serve` if it is still up), then `rm -rf /tmp/laya-live`. The checkpoint stays in the Hugging Face cache for real serving.

---

### Task 6: Full verification and PR

- [ ] **Step 1: Everything green**

```bash
gofmt -l internal cmd
go vet ./internal/...
go test ./...
python3 -m pytest scripts/ -q
go build -o bin/proxy ./cmd/proxy
```

Expected: `gofmt` prints nothing, every test passes, and the build succeeds.

- [ ] **Step 2: Push and open a stacked PR**

```bash
git push -u fork feat/laya-serve-escalation
gh pr create --base feat/laya-finetune-smoke --head feat/laya-serve-escalation \
  --title "feat(classifier): serve laya with escalation to the teacher" --body-file /tmp/pr-body.md
```

The body must include:
- the design summary above;
- the Task 5 numbers: confusion matrix, exact agreement, escalation rate, teacher refusals laya would allow, latency p50/p95, and the `answer_confidence` medians;
- a note that the PR retargets to `main` once #95 merges.

- [ ] **Step 3: Hand the enable decision to the user**

Do not edit the live config. Report the Task 5 numbers and the exact backend and rule JSON from the docs. The user decides whether to enable them, and with which `layaEscalateLabels` / `layaMinConfidence`, from the "teacher refusals laya would allow" count.
