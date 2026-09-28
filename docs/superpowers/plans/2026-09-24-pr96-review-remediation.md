# PR #96 Review Remediation: Laya-serve Escalation

- **Goal:** Resolve the six review findings on PR #96 (`feat/laya-serve-escalation`, stacked on `feat/laya-finetune-smoke`).
- **Architecture:** The laya checks move out of the save handler into `config.TargetBackend.ValidateLaya()`. The save handler and `classifier.ConfigurableMatcher.UpdateRules` both call it, so a hand-edited `config.json` gets the same checks as a WebUI save. `scripts/check_laya.py` treats `answer_confidence` as part of the wire contract and gains `--model`. The docs and the WebUI laya hint carry the measured result from the PR's live replay.
- **Tech Stack:** Go 1.27rc2 `testing`, Python 3 + pytest (`scripts/`), Alpine.js translations.
- **Spec:** PR review comment https://github.com/gustavokch/antigravity-claude-proxy-go/pull/96#issuecomment-5823171267. Design reference: `docs/superpowers/plans/2026-09-24-laya-serve-escalation.md` and spec §8 of `docs/superpowers/specs/2026-09-23-classifier-corpus-and-laya-design.md`.
- **Test runners:** `go test ./...` and `python3 -m pytest scripts/`.

## Global Constraints

- Work on the existing branch `feat/laya-serve-escalation`. Push remote: `fork`. The PR base stays `feat/laya-finetune-smoke`.
- Do not touch TLS or transport code (AGENTS.md). Nothing here comes near it.
- Staged Go files must be gofmt-clean: `gofmt -l internal cmd` prints nothing.
- Leave the user's untracked files alone: `.gemini/`, `.gocache/`, `.ignore`, `.mcp.json`, `GEMINI.md`, `docs/superpowers/plans/2026-09-24-pr94-review-remediation-round2.md`, `opencode.json`, `pr.sh`, `scripts/__pycache__/`, `tools/quotaprobe/`. Stage files by name, never `git add -A`.
- Tests defend observable behavior: validation verdicts, whether the previous rule set stays active, the check script's verdict and exit code, and the request body it sends. Never pin log wording.
- Baseline (verified before planning): `go test ./internal/api/ ./internal/config/ ./internal/classifier/... ./internal/webui/` is green, and `python3 -m pytest scripts/test_check_laya.py` reports 7 passed.

## Findings → Tasks

| # | Finding | Severity | Task |
|---|---|---|---|
| 1 | `docs/classifier-rules.md:L164`: the example rule is `"enabled": true`, and the old "plausible allow" caveat is gone, even though the PR's own replay shows the stock checkpoint allowing 5/5 teacher refusals | 🔴 | 5 |
| 2 | `internal/api/management.go:L1203`: escalation fields are validated only on WebUI save, so a hand-edited typo fails open | 🟡 | 1, 2 |
| 3 | `scripts/check_laya.py:L83`: `answer_confidence` is not checked, and a string value crashes `:.2f` (L123) with exit 1 | 🟡 | 3 |
| 4 | `docs/classifier-rules.md:L193`: "never weakens a verdict" is true only under the default clamp | 🔵 | 5 |
| 5 | `internal/api/classifier_config_test.go:L421`: no custom-criteria validation cases | 🔵 | 1 |
| 6 | `scripts/check_laya.py:L34`: no way to check a non-default `model` | 🔵 | 4 |

Evidence checked before planning:

- Finding 2: `grep LayaEscalateLabels internal/` finds only the struct, `LayaSettings`, the save handler and tests. `config.Load` has no validator. `applyClassifierConfig` (`internal/api/classifier_rules.go:41-68`) passes `cfg.Backends` directly to `NewConfigurableMatcher`/`UpdateRules` (`internal/classifier/matcher.go:46-103`), and neither function looks at laya fields. With the escalate list `["d"]`, `slices.Contains` never matches, so D is answered as severity 35: the pre-PR behavior, silently.
- Finding 3: `main` runs `f"{result.answer_confidence:.2f}"` (L123). On a `str` that raises `ValueError: Unknown format code 'f'`, the exception is uncaught, and Python exits 1. The module docstring (L19) promises exit 2.
- Finding 1: the removed sentence (base branch, `docs/classifier-rules.md:153`) was "Until a fine-tuned checkpoint and a measured agreement rate exist, treat the verdict as a locally computed, plausible allow." The PR body now has that measured rate (22.1%, 5/5 refusals allowed), but the doc doesn't mention it.

Considered and dropped: the escalated audit `detail` repeats the `escalated to built-in handling:` prefix (cosmetic). The committed plan uses absolute `/Users/gus/...` paths (repo convention: 12 other committed plans do the same).

## File Structure

| File | Change |
|---|---|
| `internal/config/config.go` | `layaQuestionNamePattern` (moved), `TargetBackend.ValidateLaya()`, `regexp` import |
| `internal/config/config_test.go` | `TestValidateLayaChecksEscalateLabelsAgainstTheResolvedCriteria` |
| `internal/api/management.go` | Laya loop body replaced by one `ValidateLaya` call; pattern var removed |
| `internal/classifier/matcher.go` | `UpdateRules` validates backends first |
| `internal/classifier/matcher_test.go` | `TestUpdateRulesRejectsAnInvalidLayaBackend` |
| `scripts/check_laya.py`, `scripts/test_check_laya.py` | `answer_confidence` contract; `--model` |
| `docs/classifier-rules.md` | Warning, disabled example, scoped "never weakens", exit codes, `--model` |
| `internal/webui/public/js/translations/en.js`, `pt.js` | `classifierLayaHint` warning |

---

### Task 1: `TargetBackend.ValidateLaya`, one validator for every laya check

**Files:**
- Modify: `internal/config/config.go` (imports L3-16; insert after `LayaSettings`, which ends at L372)
- Modify: `internal/api/management.go` (L1105-1108 pattern var; L1177-1231 laya loop)
- Test: `internal/config/config_test.go` (append after `TestLayaSettingsKeepsAnExplicitEmptyEscalationListThroughASave`, ends ~L907)

**Interfaces:**
- Consumes: `TargetBackend.LayaSettings()` (resolved criteria).
- Produces: `func (backend TargetBackend) ValidateLaya() error`. It returns nil for non-laya formats and returns the save handler's current message text without the `backend %q:` prefix. Task 2 depends on it.

- [ ] **Step 1: Write the failing test**

Append to `internal/config/config_test.go`:

```go
func TestValidateLayaChecksEscalateLabelsAgainstTheResolvedCriteria(t *testing.T) {
	custom := `"layaCriteria":{"X":"one","Y":"two"},"layaSeverityMap":{"X":1,"Y":2}`
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "default criteria accept D", raw: `{"format":"laya","layaEscalateLabels":["D"]}`},
		{name: "default criteria reject a lowercase typo", raw: `{"format":"laya","layaEscalateLabels":["d"]}`, wantErr: true},
		{name: "custom criteria accept their own label", raw: `{"format":"laya",` + custom + `,"layaEscalateLabels":["X"]}`},
		{name: "custom criteria reject a default label", raw: `{"format":"laya",` + custom + `,"layaEscalateLabels":["D"]}`, wantErr: true},
		// The WebUI leaves laya fields on a backend switched to another
		// format; they are inert there and must not block the save.
		{name: "other formats are not laya-checked", raw: `{"format":"openai","layaEscalateLabels":["d"]}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var backend TargetBackend
			if err := json.Unmarshal([]byte(testCase.raw), &backend); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if err := backend.ValidateLaya(); (err != nil) != testCase.wantErr {
				t.Errorf("ValidateLaya() = %v, wantErr %v", err, testCase.wantErr)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test and see it fail**

Run: `go test ./internal/config/ -run TestValidateLaya -v`
Expected: FAIL to compile: `backend.ValidateLaya undefined (type TargetBackend has no field or method ValidateLaya)`.

- [ ] **Step 3: Implement**

In `internal/config/config.go`, add `"regexp"` to the standard-library import block (between `"path/filepath"` and `"strings"`). After the closing brace of `LayaSettings` (L372), insert:

```go
// layaQuestionNamePattern is spec §4.6's rule for layaQuestionName. The name
// keys both the question sent to Laya and the answer read back, so it stays a
// short identifier.
var layaQuestionNamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)

// ValidateLaya reports the first Laya override that cannot be served safely,
// or nil for any other format. The config save handler and the rule matcher
// both call it, so a hand-edited config.json gets the same checks as a WebUI
// save: an escalate-label typo must not silently turn escalation off.
func (backend TargetBackend) ValidateLaya() error {
	if backend.Format != BackendFormatLaya {
		return nil
	}
	if backend.LayaMaxSeverity != nil && (*backend.LayaMaxSeverity < 0 || *backend.LayaMaxSeverity > 100) {
		return errors.New("layaMaxSeverity must be between 0 and 100")
	}
	if backend.LayaStateChars != 0 && (backend.LayaStateChars < 200 || backend.LayaStateChars > 8000) {
		return errors.New("layaStateChars must be between 200 and 8000")
	}
	if backend.LayaQuestionName != "" && !layaQuestionNamePattern.MatchString(backend.LayaQuestionName) {
		return errors.New("layaQuestionName must be 1 to 32 letters, digits or underscores")
	}
	if backend.LayaInstructions != "" && strings.TrimSpace(backend.LayaInstructions) == "" {
		return errors.New("layaInstructions must not be blank")
	}
	if backend.LayaMinConfidence < 0 || backend.LayaMinConfidence >= 1 {
		return errors.New("layaMinConfidence must be at least 0 and below 1")
	}
	// Checked against the resolved criteria, so a label typo cannot
	// silently turn escalation off under either criteria set.
	criteria := backend.LayaSettings().Criteria
	for _, label := range backend.LayaEscalateLabels {
		if _, exists := criteria[label]; !exists {
			return fmt.Errorf("layaEscalateLabels has label %q with no matching criteria entry", label)
		}
	}
	if len(backend.LayaCriteria) == 0 && len(backend.LayaSeverityMap) == 0 {
		return nil
	}
	if len(backend.LayaCriteria) < 2 {
		return errors.New("layaCriteria needs at least 2 options")
	}
	if len(backend.LayaCriteria) != len(backend.LayaSeverityMap) {
		return errors.New("layaCriteria and layaSeverityMap must have the same keys")
	}
	for label, severity := range backend.LayaSeverityMap {
		if _, exists := backend.LayaCriteria[label]; !exists {
			return fmt.Errorf("layaSeverityMap has label %q with no matching criteria entry", label)
		}
		if severity < 0 || severity > 100 {
			return fmt.Errorf("layaSeverityMap[%q] must be between 0 and 100", label)
		}
	}
	return nil
}
```

In `internal/api/management.go`:
- Delete L1105-1108 (the `layaQuestionNamePattern` comment and var). Keep the `"regexp"` import: L1301 still uses `regexp.Compile`.
- Replace the whole laya loop at L1177-1231 (from `for backendKey, backend := range classifierReq.Backends {` through its closing brace, just before the URL loop at L1233) with:

```go
		for backendKey, backend := range classifierReq.Backends {
			if err := backend.ValidateLaya(); err != nil {
				writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("backend %q: %v", backendKey, err)})
				return
			}
		}
```

The response text is unchanged (`backend "local": layaMinConfidence must be at least 0 and below 1`), so the existing save tests are the regression check for the move.

- [ ] **Step 4: Run the tests and see them pass**

Run: `go test ./internal/config/ -run 'TestValidateLaya|TestLayaSettings' -v && go test ./internal/api/ -run 'TestConfigSave.*Laya' -v`
Expected: PASS. The API run must include every existing `TestConfigSaveRejects*Laya*` / `TestConfigSaveAccepts*Laya*` test, all green. That proves the move kept every message and verdict.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal cmd
git add internal/config/config.go internal/config/config_test.go internal/api/management.go
git commit -m "refactor(config): one validator for laya backends" -m "The laya checks lived inline in the save handler, so nothing else could reuse them. ValidateLaya is the single copy; the matcher calls it next. Adds custom-criteria cases: labels are checked against the resolved criteria, not the defaults."
```

---

### Task 2: The matcher rejects an invalid laya backend from any source

**Files:**
- Modify: `internal/classifier/matcher.go` (`NewConfigurableMatcher` doc L44-45; `UpdateRules` L73-91)
- Test: `internal/classifier/matcher_test.go` (append after `TestUpdateRulesLeavesPreviousSetOnError`, L159-175)

**Interfaces:**
- Consumes: `config.TargetBackend.ValidateLaya()` (Task 1).
- Produces: `UpdateRules` returns `classifier: backend "<key>": <reason>` for an invalid laya backend and leaves the previous rule set active. At startup, `applyClassifierConfig` already logs `classifier rules rejected at startup; rule routing is inactive` and runs with no rules. All classifier traffic then takes built-in handling, which is the safe direction.

**Behavior change to note in the PR:** a `config.json` that the save handler would already reject (for example `layaStateChars: 100`) now also disables rule routing at startup. Before this change it loaded, and the invalid override was quietly replaced by its default.

- [ ] **Step 1: Write the failing test**

Append to `internal/classifier/matcher_test.go`:

```go
func TestUpdateRulesRejectsAnInvalidLayaBackend(t *testing.T) {
	matcher, err := NewConfigurableMatcher([]config.Rule{stage1Rule()}, testBackends())
	if err != nil {
		t.Fatalf("NewConfigurableMatcher: %v", err)
	}

	// A hand-edited config.json never passes the save handler, so this is
	// the only place its escalate-label typo can be caught.
	handEdited := map[string]config.TargetBackend{
		"local": {
			Name:               "Local laya",
			URL:                "http://127.0.0.1:8000/v1/systemone",
			Format:             config.BackendFormatLaya,
			LayaEscalateLabels: []string{"d"},
		},
	}
	if err := matcher.UpdateRules([]config.Rule{stage1Rule()}, handEdited); err == nil {
		t.Fatal(`UpdateRules accepted escalate label "d": a typo would silently turn D escalation off`)
	}
	_, backend, matched := matcher.Match([]byte(stage1Body))
	if !matched || backend == nil || backend.Format != config.BackendFormatOpenAI {
		t.Errorf("Match = %v, backend %+v; want the previous openai backend still active", matched, backend)
	}
}
```

- [ ] **Step 2: Run the test and see it fail**

Run: `go test ./internal/classifier/ -run TestUpdateRulesRejectsAnInvalidLayaBackend -v`
Expected: FAIL with `UpdateRules accepted escalate label "d": a typo would silently turn D escalation off`.

- [ ] **Step 3: Implement**

In `internal/classifier/matcher.go`, change the `NewConfigurableMatcher` doc (L44-45) to:

```go
// NewConfigurableMatcher builds a matcher, returning an error if any rule
// carries a regex that does not compile or any laya backend is invalid.
```

At the top of `UpdateRules`, before `compiled := make(...)` (L77), insert:

```go
	// config.json can be edited by hand and never pass the save handler, so
	// the backends get the same checks a WebUI save applies.
	for name, backend := range backends {
		if err := backend.ValidateLaya(); err != nil {
			return fmt.Errorf("classifier: backend %q: %w", name, err)
		}
	}
```

`fmt` is already imported (L63 uses it).

- [ ] **Step 4: Run the tests and see them pass**

Run: `go test ./internal/classifier/ -v -run 'TestUpdateRules|TestMatch|TestNewConfigurableMatcher' && go test ./internal/api/ -run 'Classifier|Laya'`
Expected: PASS. The API run covers `applyClassifierConfig` callers. None of their fixtures uses an invalid laya value (checked: `LayaStateChars = 200`, valid custom criteria/maps, and `LayaMinConfidence = 0.6` are all within bounds).

- [ ] **Step 5: Commit**

```bash
gofmt -l internal cmd
git add internal/classifier/matcher.go internal/classifier/matcher_test.go
git commit -m "fix(classifier): validate laya backends on every load" -m "Only the WebUI save checked layaEscalateLabels, so a hand-edited config.json typo like [\"d\"] silently turned D escalation off: fail-open on the safety control. UpdateRules now rejects the set and keeps the previous rules; at startup, rule routing stays off and built-in handling answers."
```

---

### Task 3: `check_laya.py` enforces the `answer_confidence` contract

**Files:**
- Modify: `scripts/check_laya.py` (`Result` L42-45; `parse_answer` L65-83; `main` L123-124)
- Test: `scripts/test_check_laya.py` (`_typed_answer` L40-43; `test_check_accepts_valid_choice_answer` L46-55; append new test)

**Interfaces:**
- Produces: `Result.answer_confidence: float`, always set. `parse_answer` raises `CheckError` (so `main` exits 2) when `answer_confidence` is missing, is not a number, or falls outside [0, 1].

- [ ] **Step 1: Write the failing test**

In `scripts/test_check_laya.py`, replace `_typed_answer` (L40-43) so that the happy-path fixture carries the field the proxy now reads:

```python
def _typed_answer(label):
    return json.dumps(
        {"answers": {"risk": {"choice": label, "confidence": 0.9, "answer_confidence": 0.49}}}
    ).encode()
```

In `test_check_accepts_valid_choice_answer`, after `assert result.label == "C"`, add:

```python
    assert result.answer_confidence == 0.49
```

Append:

```python
@pytest.mark.parametrize(
    "answer",
    [
        pytest.param({"choice": "A", "confidence": 0.9}, id="missing"),
        pytest.param({"choice": "A", "answer_confidence": "high"}, id="not-a-number"),
        pytest.param({"choice": "A", "answer_confidence": 1.5}, id="above-one"),
    ],
)
def test_check_rejects_an_unusable_answer_confidence(laya_server, answer):
    """layaMinConfidence compares answer_confidence. A server that drops or
    mangles it makes every answer escalate under a floor, silently, so the
    smoke check must fail loudly instead."""
    _Handler.response_body = json.dumps({"answers": {"risk": answer}}).encode()
    _Handler.status = 200

    with pytest.raises(check_laya.CheckError, match="answer_confidence"):
        check_laya.check(laya_server, action="ls", timeout=5)
```

- [ ] **Step 2: Run the tests and see them fail**

Run: `python3 -m pytest scripts/test_check_laya.py -v`
Expected: the three `test_check_rejects_an_unusable_answer_confidence[...]` cases FAIL with `Failed: DID NOT RAISE <class 'check_laya.CheckError'>`. Everything else passes (`answer.get` already returns 0.49).

- [ ] **Step 3: Implement**

In `scripts/check_laya.py`:

`Result` (L42-45):

```python
@dataclass
class Result:
    label: str
    answer_confidence: float
```

Replace the tail of `parse_answer` (the comment and `return` at L81-83) with:

```python
    # answer_confidence is what layaMinConfidence compares; "confidence" is
    # laya's entropy score on another scale. Under a floor the proxy
    # escalates every answer without a usable one, so a missing or mangled
    # value breaks the contract even though the label parsed.
    answer_confidence = answer.get("answer_confidence")
    if isinstance(answer_confidence, bool) or not isinstance(answer_confidence, (int, float)):
        raise CheckError(f"answer_confidence {answer_confidence!r} is not a number")
    if not 0 <= answer_confidence <= 1:
        raise CheckError(f"answer_confidence {answer_confidence} is outside [0, 1]")
    return Result(label=label, answer_confidence=float(answer_confidence))
```

In `main`, replace the two lines at L123-124 with:

```python
    print(f"OK: label={result.label} answer_confidence={result.answer_confidence:.2f}")
```

In the docstring after the `into training examples` line (L10-11), change "verifies the response carries a known A-D choice for the question asked" to "verifies the response carries a known A-D choice and a calibrated answer_confidence for the question asked".

- [ ] **Step 4: Run the tests and see them pass**

Run: `python3 -m pytest scripts/test_check_laya.py -v`
Expected: PASS, 10 tests (7 existing + 3 new cases).

- [ ] **Step 5: Commit**

```bash
git add scripts/check_laya.py scripts/test_check_laya.py
git commit -m "fix(scripts): check_laya requires answer_confidence" -m "The proxy escalates every answer without answer_confidence once layaMinConfidence is set, yet the smoke check passed such a server, and a non-numeric value crashed the :.2f format with exit 1 instead of the documented 2."
```

---

### Task 4: `check_laya.py --model`

**Files:**
- Modify: `scripts/check_laya.py` (usage docstring L15-17; `build_payload` L48-62; `check` L86-108; `main` L111-119)
- Test: `scripts/test_check_laya.py` (append)

**Interfaces:**
- Produces: `build_payload(action, model=MODEL)`, `check(url, action, timeout, model=MODEL)`, and the CLI flag `--model` (default `MODEL`). `test_check_pins_the_proxy_default_checkpoint` keeps the default in sync with `config.DefaultLayaModel`.

- [ ] **Step 1: Write the failing test**

Append to `scripts/test_check_laya.py`:

```python
def test_main_sends_the_model_it_is_given(laya_server):
    """A backend may set model to another checkpoint the server preloads;
    the check must be able to ask for that one, or it proves a model the
    proxy never requests."""
    _Handler.response_body = _typed_answer("A")
    _Handler.status = 200

    assert check_laya.main(["--url", laya_server, "--model", "multilingual"]) == 0
    assert json.loads(_Handler.last_body)["model"] == "multilingual"
```

- [ ] **Step 2: Run the test and see it fail**

Run: `python3 -m pytest scripts/test_check_laya.py -k model -v`
Expected: `test_main_sends_the_model_it_is_given` FAILS with `SystemExit: 2` (argparse: `unrecognized arguments: --model multilingual`).

- [ ] **Step 3: Implement**

In `scripts/check_laya.py`:

```python
def build_payload(action, model=MODEL):
    """Return the request body the proxy's buildLayaPayload sends, with the
    same instructions and criteria text as the exporter's training
    examples."""
    return {
        "model": model,
        # ... state and questions unchanged
    }
```

In `check`, change the signature to `def check(url, action, timeout, model=MODEL):` and the request body to `data=json.dumps(build_payload(action, model)).encode(),`.

In `main`, after the `--action` argument:

```python
    parser.add_argument(
        "--model",
        default=MODEL,
        help="checkpoint to request; pass the backend's model when it sets one (default: %(default)s, the proxy's default)",
    )
```

and call `result = check(args.url, args.action, args.timeout, args.model)`.

Add a usage line to the docstring (after L17):

```
    python3 scripts/check_laya.py --url ... --model multilingual
```

- [ ] **Step 4: Run the tests and see them pass**

Run: `python3 -m pytest scripts/test_check_laya.py -v`
Expected: PASS, 11 tests. `test_check_pins_the_proxy_default_checkpoint` still passes, which proves the default is unchanged.

- [ ] **Step 5: Commit**

```bash
git add scripts/check_laya.py scripts/test_check_laya.py
git commit -m "feat(scripts): check_laya --model" -m "The docs let a backend pin another preloaded checkpoint, but the smoke check could only ask for english, so it could not prove the checkpoint the proxy would actually request."
```

---

### Task 5: The docs and WebUI hint carry the measured result

**Files:**
- Modify: `docs/classifier-rules.md` (L148-157 intro; L164 example rule; L193 "never weakens"; L199-205 check section)
- Modify: `internal/webui/public/js/translations/en.js` (L822 `classifierLayaHint`)
- Modify: `internal/webui/public/js/translations/pt.js` (L765 `classifierLayaHint`)

**Interfaces:** none. Docs and one existing i18n key; `internal/webui/translations_test.go` already lists `classifierLayaHint`.

This task has no failing test: the change is prose. Step 2 is a mechanical check instead.

- [ ] **Step 1: Edit `docs/classifier-rules.md`**

After the intro sentence at L148 ("A Laya backend answers low-risk Stage 1 requests on your machine and hands everything else to the teacher. Serve laya's stock English checkpoint:"), and before the install block, insert:

```markdown
> **Keep a Laya rule disabled with the stock checkpoint.** A laya Stage 1 answer stands in for the teacher's, and a low Stage 1 grade ends the check. Replayed over 217 teacher-labelled Stage 1 rows (laya-serve 0.3.20, `english`), the stock checkpoint agreed with the teacher on 22.1% of them (chance is 25%) and graded all 5 teacher refusals A to C. Neither the default `["D"]` escalation nor a confidence floor sent them back: the median `answer_confidence` was 0.36 when laya was right and 0.38 when it was wrong. Zero-shot, it grades `rm -rf / --no-preserve-root` and `curl … | sudo bash` A. Serve it to measure a checkpoint, and enable the rule only after a fine-tuned checkpoint closes the refusal gap.
```

Change L157 to:

```markdown
Add a backend and a rule that matches the Stage 1 footer. The rule ships disabled; see the warning above:
```

Change L164 `"enabled": true,` to `"enabled": false,`.

Replace the first sentence of L193 ("An escalation only turns a Laya allow into a teacher call, so it never weakens a verdict.") with:

```markdown
With the default clamp every Laya answer is an allow, so an escalation only turns a Laya allow into a teacher call and never weakens a verdict. An operator who lets a label block (see above) and also escalates that label hands the block to the teacher instead.
```

Keep the rest of L193 unchanged.

In the "Checking a live laya-serve" section, after the `check_laya.py --url` code block (L201-203), insert:

```markdown
If the backend sets `model`, pass the same value with `--model`.
```

Change L205 to:

```markdown
Exit 0 means the server accepted the proxy's request shape and returned a parseable A-D choice with an `answer_confidence` in [0, 1]. Exit 2 names the break: unreachable host, non-200, malformed body, an unknown label, or a missing or out-of-range `answer_confidence`.
```

- [ ] **Step 2: Edit the WebUI hint**

`en.js` L822:

```js
    classifierLayaHint: "Answers are computed by a local laya-serve instance at POST /v1/systemone. Start it yourself; the proxy does not manage the process. The stock checkpoint graded every teacher refusal in a 217-row replay as allowable, so keep a Laya rule disabled until a fine-tuned checkpoint is measured.",
```

`pt.js` L765:

```js
    classifierLayaHint: "As respostas são calculadas por uma instância local do laya-serve em POST /v1/systemone. Inicie-a você mesmo; o proxy não gerencia esse processo. O checkpoint padrão classificou como permitida toda recusa do professor numa reprodução de 217 linhas, então mantenha uma regra Laya desativada até que um checkpoint ajustado seja medido.",
```

- [ ] **Step 3: Check**

Run: `go test ./internal/webui/ && grep -n '"enabled": false' docs/classifier-rules.md && grep -c 'Keep a Laya rule disabled' docs/classifier-rules.md`
Expected: `ok`. The example rule line shows `"enabled": false`, and the count is `1`.

- [ ] **Step 4: Commit**

```bash
git add docs/classifier-rules.md internal/webui/public/js/translations/en.js internal/webui/public/js/translations/pt.js
git commit -m "docs(classifier): warn before enabling stock laya" -m "The example rule shipped enabled, and the old plausible-allow caveat was dropped, although this PR's replay shows the stock checkpoint allowing 5/5 teacher refusals. The warning now carries the numbers, the example ships disabled, and 'never weakens' is limited to the default clamp."
```

---

## Verification & Push

- [ ] **Full gate**

```bash
gofmt -l internal cmd
go vet ./internal/...
go test ./...
python3 -m pytest scripts/
go build -o bin/proxy ./cmd/proxy
```

Expected: `gofmt` prints nothing, vet is clean, `go test` ends with every package `ok`, pytest reports 134 passed (130 at PR head + 3 answer-confidence cases + 1 model test), and the build succeeds. 100% green is required before pushing.

- [ ] **Push**

```bash
git push fork feat/laya-serve-escalation
```

- [ ] **Report on the PR**

Reply to the review comment with one line per finding (fixed in `<sha>`), the final Go/pytest counts, and the Task 2 behavior note: a config that the save handler would reject now disables rule routing at startup.
