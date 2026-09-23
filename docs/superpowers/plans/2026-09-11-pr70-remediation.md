# PR #70 Remediation Plan: Classifier UTF-8 Safety, Multi-Transcript Compaction & Schema Conformance

**Goal:** Remediate findings from code review on PR #70: rune-safe transcript compaction for multi-byte UTF-8, multi-block transcript compaction, wire-spec `stop_sequence` restoration in canned stubs, and variant property preservation in WebUI.

**Architecture:**
- `internal/classifier`: Update `CompactTranscript` to iterate through all `<transcript>` blocks in text and slice strings using `[]rune` to prevent corrupted UTF-8 sequences. Update `BuildStub` to include `"stop_sequence": nil` in JSON response.
- `internal/webui`: Update `saveConfig` in `classifier-config.js` to spread existing variant properties so unspecified overrides are preserved.

**Tech Stack:** Go (1.23+), Alpine.js, WebUI.

---

### Task 1: Rune-Safe Multi-Transcript Compaction & Stop Sequence Conformance

- **Target files:**
  - Modify: `internal/classifier/classifier.go`
  - Test: `internal/classifier/classifier_test.go`
- **Interfaces:**
  - `CompactTranscript(raw json.RawMessage) (json.RawMessage, bool)`
  - `BuildStub(kind Kind, model, verdictTmpl, thinkingTmpl string) ([]byte, error)`

- **Step 1: Write failing tests**
  - Add `TestCompactTranscript_UTF8MultiByte` with multibyte UTF-8 chars right around the 250-rune boundary.
  - Add `TestCompactTranscript_MultipleBlocks` with two `<transcript>` blocks in a single text block.
  - Add `TestBuildStub_StopSequencePresent` verifying unmarshaled JSON contains `"stop_reason": "end_turn"` and `"stop_sequence": null`.

- **Step 2: Run tests to confirm failure**
  - Command: `go test -v ./internal/classifier -run "TestCompactTranscript_|TestBuildStub_StopSequence"`

- **Step 3: Implementation**
  - In `internal/classifier/classifier.go`:
    - Loop over `<transcript>...</transcript>` instances to support multiple blocks per text element.
    - Convert lines to `[]rune` before length checking and slicing with `maxOutputRunes`.
    - Add `"stop_sequence": nil` to `BuildStub` response map.

- **Step 4: Run tests to confirm pass**
  - Command: `go test -v ./internal/classifier -run "TestCompactTranscript_|TestBuildStub_StopSequence"`

- **Step 5: Git commit**
  - `git commit -am "fix(classifier): handle multibyte utf-8 runes and multiple transcript blocks"`

---

### Task 2: Preserve Existing Variant Properties in WebUI Config Save

- **Target files:**
  - Modify: `internal/webui/public/js/components/classifier-config.js`
  - Test: `internal/webui/embed_test.go`
- **Interfaces:**
  - `saveConfig()` in `classifierConfig` Alpine component.

- **Step 1: Write test/assertion**
  - Verify `classifier-config.js` preserves existing variant attributes via object spread.

- **Step 2: Implementation**
  - Update `saveConfig()` in `internal/webui/public/js/components/classifier-config.js` to spread `...(this.config.variants[key] || {})` before overriding specific fields.

- **Step 3: Run webui tests to verify pass**
  - Command: `go test -v ./internal/webui/...`

- **Step 4: Git commit**
  - `git commit -am "fix(webui): preserve variant config attributes on save"`
