# PR #70 Second Pass Remediation Plan: Content Block Property Preservation, String-Content Compaction & WebUI Store Sync

**Goal:** Remediate second-pass review findings on PR #70: preserve auxiliary block fields (e.g. `cache_control`) during transcript compaction, support plain JSON string content compaction, synchronize normalized payload into WebUI store cache, and clarify block pre-filter canned verdict placeholder.

**Architecture:**
- `internal/classifier/classifier.go`: Refactor `CompactTranscript` to unmarshal into `[]map[string]any` so arbitrary fields (e.g. `cache_control`, `citations`, etc.) are preserved verbatim when updating `text`. Add fallback unmarshaling for plain string content.
- `internal/webui/public/js/components/classifier-config.js`: Update `saveConfig` to assign `payload.classifier` to `Alpine.store('settings').config.classifier` and `this.config`.
- `internal/webui/public/views/settings.html`: Update block pre-filter placeholder text.

**Tech Stack:** Go (1.23+), Alpine.js, WebUI.

---

### Task 1: Preserve Content Block Properties and Support String Content in CompactTranscript

- **Target files:**
  - Modify: `internal/classifier/classifier.go`
  - Test: `internal/classifier/classifier_test.go`
- **Interfaces:**
  - `CompactTranscript(raw json.RawMessage) (json.RawMessage, bool)`

- **Step 1: Write failing tests**
  - Add `TestCompactTranscript_PreservesAuxiliaryProperties`: message block includes `"cache_control": {"type": "ephemeral"}` and extra attributes; verify compacted output retains them.
  - Add `TestCompactTranscript_StringContent`: content is a raw JSON string containing `<transcript>...</transcript>`; verify it compacts correctly.

- **Step 2: Run tests to confirm failure**
  - Command: `go test -v ./internal/classifier -run "TestCompactTranscript_PreservesAuxiliaryProperties|TestCompactTranscript_StringContent"`

- **Step 3: Implementation**
  - In `internal/classifier/classifier.go`:
    - Check if `raw` unmarshals as `string`. If so, compact transcript blocks in the string and marshal back.
    - If `raw` unmarshals as `[]map[string]any`, inspect each block where `"text"` is a string, compact `<transcript>` blocks, and assign back to `block["text"]`.
    - Extract helper `compactTranscriptText(text string) (string, bool)`.

- **Step 4: Run tests to confirm pass**
  - Command: `go test -v ./internal/classifier -run "TestCompactTranscript_"`

- **Step 5: Git commit**
  - `git commit -am "fix(classifier): preserve content block attributes and support string content in compaction"`

---

### Task 2: Synchronize Normalized Config in WebUI Store and Fix Placeholder

- **Target files:**
  - Modify: `internal/webui/public/js/components/classifier-config.js`
  - Modify: `internal/webui/public/views/settings.html`
  - Test: `internal/webui/embed_test.go`
- **Interfaces:**
  - `saveConfig()` in `classifierConfig` Alpine component.

- **Step 1: Write failing test**
  - Update `internal/webui/embed_test.go` to assert that `classifier-config.js` sets store config to `payload.classifier` and `settings.html` does not contain misleading placeholder `"Leave blank to use empty string"`.

- **Step 2: Run test to confirm failure**
  - Command: `go test -v ./internal/webui/...`

- **Step 3: Implementation**
  - In `internal/webui/public/js/components/classifier-config.js`:
    - Assign `JSON.parse(JSON.stringify(payload.classifier))` to `Alpine.store('settings').config.classifier` and `this.config`.
  - In `internal/webui/public/views/settings.html`:
    - Change placeholder from `Leave blank to use empty string (allow)` to `e.g. <block>false</block> (No default; blank disables stubbing)`.

- **Step 4: Run tests to confirm pass**
  - Command: `go test -v ./internal/webui/...`

- **Step 5: Git commit**
  - `git commit -am "fix(webui): sync normalized payload to store cache and fix prefilter placeholder"`
