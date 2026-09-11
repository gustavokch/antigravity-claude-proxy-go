# Design Specification: Configurable Classifier Interception & WebUI Management

- **Date:** 2026-09-10
- **Status:** Draft (Pending Review)
- **Author:** Claude & Gustavo
- **Target File:** `docs/superpowers/specs/2026-09-10-classifier-model-config-design.md`

---

## 1. Overview & Goal

In PR #68 (`feat: bash-classifier fallback on quota exhaustion`), the proxy introduced detection of Claude Code's bash-action security monitor (classifier) requests and canned allow verdicts when accounts reach quota exhaustion.

This specification expands that mechanism into a fully configurable subsystem. Operators can control every aspect of classifier request handling through the WebUI and JSON configuration:
1. **Action Policy:** Immediately stub with canned response, forward to an overridden model, fallback to stub on quota exhaustion, or pass through unmutated.
2. **Model Rerouting:** Direct classifier requests to designated fast/cheap models (e.g., Haiku, Gemini Flash, or custom endpoints) instead of inheriting the main session model.
3. **Payload Control:** Override generation limits (`max_tokens`), sampling temperature, compact bloated transcript context to cut token expenditure, and customize verdict and reasoning templates.
4. **Granular Variant Overrides:** Configure global defaults with targeted overrides for Stage 1 (harm-only severity), Stage 2 (full verdict with thinking), and Block pre-filter variants.
5. **WebUI Management:** Manage all classifier settings via a dedicated tab in System Settings (`views/settings.html`) with real-time saving and validation.

---

## 2. Architecture & Data Flow

### 2.1 Request Lifecycle

```
[Claude Code Client]
        |
        v
POST /v1/messages
        |
        v
[Server Read & Parse Body]
        |
        v
[Classifier Detection Gate] ---> (Not a classifier or streaming?) ---> [Standard Dispatch]
        |
        v (Matches monitorPromptPrefix)
[Resolve Variant & Config Profile]
 (Merge Global Defaults + Variant Overrides)
        |
        +---> Action: "always_stub" ---> [Generate Canned Response] ---> Return HTTP 200 JSON
        |
        +---> Action: "passthrough" ---> [Standard Dispatch (Unmutated)]
        |
        +---> Action: "reroute_only" | "fallback_on_exhaustion"
                    |
                    +--> [Apply Mutations]
                    |    - Rewrite model to targetModel (if set)
                    |    - Clamp/override max_tokens (if > 0)
                    |    - Set temperature (if specified)
                    |    - Compact <transcript> (if enabled)
                    |
                    +--> (If "fallback_on_exhaustion" AND route uses AccountManager AND Available() == 0)
                    |         |
                    |         +--> Has valid verdict template? ---> Return HTTP 200 JSON Stub
                    |         +--> No verdict template? ---> Return HTTP 400 Fast-Fail
                    |
                    +--> [Forward via Dispatch Engine]
                         (Kimi -> ClaudeCode -> OpenRouter -> CustomEndpoints -> CloudCode)
```

---

## 3. Detailed Component Specifications

### 3.1 Configuration Schema (`internal/config/config.go`)

```go
type ClassifierActionMode string

const (
    // ActionAlwaysStub immediately returns canned verdict without upstream dispatch.
    ActionAlwaysStub ClassifierActionMode = "always_stub"
    // ActionFallbackOnExhaustion forwards to target/original model, stubs if account quota exhausted.
    ActionFallbackOnExhaustion ClassifierActionMode = "fallback_on_exhaustion"
    // ActionRerouteOnly rewrites model/parameters and forwards; fails if no capacity.
    ActionRerouteOnly ClassifierActionMode = "reroute_only"
    // ActionPassthrough keeps original behavior (no rewrite or stub).
    ActionPassthrough ClassifierActionMode = "passthrough"
)

type ClassifierVariantConfig struct {
    TargetModel       string   `json:"targetModel,omitempty"`       // Model alias/ID override (empty = inherit default)
    MaxTokens         int      `json:"maxTokens,omitempty"`         // Override max_tokens (0 = inherit default)
    Temperature       *float64 `json:"temperature,omitempty"`       // Temperature override
    CompactTranscript *bool    `json:"compactTranscript,omitempty"` // Override compact transcript toggle
    CannedVerdict     string   `json:"cannedVerdict,omitempty"`     // Custom verdict template (e.g. "<severity>0</severity>")
    ThinkingText      string   `json:"thinkingText,omitempty"`      // Custom thinking block content
}

type ClassifierConfig struct {
    Enabled           bool                               `json:"enabled"`
    Action            ClassifierActionMode               `json:"action"`            // Default: fallback_on_exhaustion
    DefaultModel      string                             `json:"defaultModel"`      // Default rewrite model, e.g. "claude-haiku-4-5-20251001" or empty
    DefaultMaxTokens  int                                `json:"defaultMaxTokens"`  // Default max_tokens (0 = keep request)
    DefaultTemp       *float64                           `json:"defaultTemperature,omitempty"`
    CompactTranscript bool                               `json:"compactTranscript"` // Cut raw tool outputs inside <transcript>
    DefaultVerdict    string                             `json:"defaultVerdict"`    // Fallback template
    DefaultThinking   string                             `json:"defaultThinking"`   // Fallback thinking text
    Variants          map[string]ClassifierVariantConfig `json:"variants,omitempty"` // "stage1-severity", "stage2-severity", "block-prefilter"
}
```

#### Defaults:
- `Enabled`: `false` (or derived from `CLASSIFIER_FALLBACK` environment variable if absent from `config.json`).
- `Action`: `fallback_on_exhaustion`.
- `DefaultModel`: `""` (keep client requested model).
- `DefaultMaxTokens`: `0` (keep client requested limit).
- `DefaultVerdict`: `"<severity>0</severity>"`.
- `DefaultThinking`: `"Routine action, no policy match."`.
- `CompactTranscript`: `false`.
- Variants defaults:
  - `stage1-severity`: `MaxTokens: 64`.
  - `stage2-severity`: `MaxTokens: 8192`.

---

### 3.2 Detection, Mutation & Stubbing Engine (`internal/classifier`)

#### Stub Generation:
`BuildStub(kind Kind, model, verdictTmpl, thinkingTmpl string) ([]byte, error)`:
- If `verdictTmpl` is specified, uses that template.
- If `verdictTmpl` is empty, falls back to default confirmed stubs:
  - `KindStage1Severity`: `<severity>0</severity>`
  - `KindStage2Severity`: `<thinking>{thinkingTmpl}</thinking><severity>0</severity>` (defaults to `"Routine action, no policy match."` if empty)
  - `KindBlockPrefilter`: returns `ErrUnsupportedKind` unless custom `verdictTmpl` is provided.

#### Transcript Compaction:
`CompactTranscript(rawContent json.RawMessage) (json.RawMessage, bool)`:
- Scans content blocks inside user message.
- Identifies text blocks between `<transcript>` and `</transcript>`.
- For serialized tool results (`{"Bash":"..."}` or similar), if output exceeds threshold (e.g. 500 chars), truncates middle lines with `[...truncated X bytes...]`, retaining head and tail.
- Returns compacted raw JSON message and a boolean indicating whether mutation occurred.

---

### 3.3 Server Interception Pipeline (`internal/api/server.go`)

In `server.messages`:
1. Check `cfg.Classifier.Enabled` and `!streamRequested`.
2. Detect classifier request: `kind, detected := classifier.Detect(rawBody)`.
3. If detected:
   - Resolve effective settings by merging `cfg.Classifier` and `cfg.Classifier.Variants[kind.String()]`.
   - If action == `always_stub`:
     - Construct stub via `classifier.BuildStub(...)`.
     - Write HTTP 200 JSON response and return immediately.
   - If action == `reroute_only` or `fallback_on_exhaustion`:
     - Apply model override: `anthropicRequest["model"] = effectiveModel`.
     - Apply `max_tokens` override: `anthropicRequest["max_tokens"] = effectiveMaxTokens`.
     - Apply temperature if set: `anthropicRequest["temperature"] = *effectiveTemp`.
     - If `compactTranscript` is enabled: compact user message content.
     - Re-serialize `anthropicRequest` to `rawBody` and mark `bodyMutated = true`.
     - If action == `fallback_on_exhaustion`:
       - Before final dispatch to CloudCode account pool, check `server.accountManager.Available(effectiveModel) == 0`.
       - If zero capacity: stub if canned verdict available, else HTTP 400 fast-fail.

---

### 3.4 REST API & Management (`internal/api/management.go`)

- `GET /api/config`: Returns full configuration including `classifier` block.
- `POST /api/config`:
  - Validates `classifier.action` against allowed enum values.
  - Validates `maxTokens >= 0`.
  - Validates `temperature` range (0.0 to 2.0).
  - Updates in-memory configuration atomically and persists to `config.json`.

---

### 3.5 WebUI Interface

#### View (`internal/webui/public/views/settings.html`):
- New tab button labeled **Security Monitor** (`tabClassifier`).
- Tab pane contains:
  1. **Master Control Card**:
     - Enable switch.
     - Action Mode selector:
       - `Fallback on Quota Exhaustion` (default)
       - `Always Return Canned Stub (Fast Allow)`
       - `Reroute Only (Custom Model/Endpoint)`
       - `Passthrough (Audit/Original)`
     - Explanatory description card dynamically updating based on selected action.
  2. **Global Defaults Card**:
     - Target Model dropdown / text input (with autocompletion from active models).
     - Max Tokens input.
     - Temperature input.
     - Compact Transcript toggle.
     - Canned Verdict template textarea.
     - Thinking Text textarea.
  3. **Variant Overrides Card (Accordion)**:
     - **Stage 1 (Harm-Only Severity)**: model, max_tokens, verdict template.
     - **Stage 2 (Full Verdict & Reasoning)**: model, max_tokens, thinking text, verdict template.
     - **Block Pre-filter**: target model, custom block template.
  4. **Save & Reset Actions**:
     - Save button (with loading spinner and toast notifications).
     - Reset to Defaults button.

#### Controller (`internal/webui/public/js/components/classifier-config.js`):
- Alpine.js component initializing state from `$store.settings.config.classifier`.
- Handles dirty state tracking, input validation, and submission via `POST /api/config`.

#### Localization (`en.js` & `pt.js`):
- Complete set of translation keys for tab labels, actions, fields, tooltips, and descriptions.

---

## 4. Testing & Validation Plan

1. **Unit Tests**:
   - `internal/classifier/classifier_test.go`:
     - Test custom template stubs and fallback defaults.
     - Test transcript compaction logic with varying payload sizes.
   - `internal/config/config_test.go`:
     - Test defaults, JSON marshaling/unmarshaling, and environment variable fallbacks.
   - `internal/api/classifier_fallback_test.go`:
     - Verify all 4 action modes (`always_stub`, `fallback_on_exhaustion`, `reroute_only`, `passthrough`).
     - Verify parameter rewrites (`model`, `max_tokens`, `temperature`).
     - Verify transcript compaction across routes.
   - `internal/api/management_test.go`:
     - Verify `GET /api/config` and `POST /api/config` validation and persistence.
   - `internal/webui/translations_test.go`:
     - Verify parity of all new translation keys between `en.js` and `pt.js`.

2. **Manual & UI Verification**:
   - Verify Settings tab rendering in browser.
   - Test saving changes and reload to verify persistence in `config.json`.
   - Send simulated classifier requests and verify proxy behavior (stub vs reroute).
