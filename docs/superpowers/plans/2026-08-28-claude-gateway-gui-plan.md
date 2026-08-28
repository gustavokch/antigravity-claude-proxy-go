# Implementation Plan: Model Discovery for Anthropic / Claude Code Gateway

## Context
In the proxy Web UI (`/#settings/models`), gateway configurations exist for multiple providers:
- **OpenRouter Gateway** (Section 0a): Emerald accent (`emerald-400`), has "Discover Models" modal with live query, search, and allowlist import.
- **Kimi Gateway** (Section 0b): Cyan accent (`cyan-400`), has "Discover Models" modal with live query, search, and allowlist import.
- **Claude Code / Anthropic Gateway** (Section 0c): Violet/Purple accent (`purple-400`), currently only has "Import Defaults" and manual row entry.

This feature adds a coherent, token-consistent **"Model discovery"** capability to the Anthropic / Claude Code gateway:
1. A backend endpoint `POST /api/claudecode/models` to discover models dynamically from Anthropic's official API (`https://api.anthropic.com/v1/models`) using active Claude Code OAuth/session tokens, with a built-in standard Claude catalogue fallback.
2. A "Discover Models" button in Section 0c of `internal/webui/public/views/settings.html`.
3. An interactive, purple-themed discovery modal (`claudecode_discover_modal`) matching existing gateway modal patterns, with real-time search, family filter pills (All / Sonnet / Haiku / Opus), capability badges (Thinking, Vision, Tool Use), suggested alias mapping, and batch-import into `ccConfig.allowlist`.

---

## Design System & Token Coherence

### Color Palette (Anthropic / Claude Code Gateway Theme)
- **Primary Accent**: `#a855f7` (`text-purple-400`, `bg-purple-600`, `hover:bg-purple-500`)
- **Border / Focus**: `border-purple-500/30`, `focus:border-purple-500/60`, `border-purple-500/50` (modal frame)
- **Card / Surface Background**: `bg-purple-950/20`, `bg-space-900` (modal ground), `bg-space-800/60` (table rows)
- **Muted Text / Badges**: `text-purple-300`, `bg-purple-900/40`, `border-purple-700/40`

### Typography & Component Layout
- **Modal Dialog**: `<dialog id="claudecode_discover_modal" class="modal">`
- **Header**: Sparkle/Brain SVG icon + `text-purple-400 font-semibold text-base` + descriptive subtitle.
- **Filter Bar**:
  - Search input: `bg-space-800 border-space-border/60 text-xs rounded-lg` with search icon and clear button.
  - Family Pills: `[All]`, `[Sonnet]`, `[Haiku]`, `[Opus]` filter tabs for quick narrowing.
- **Model Table**:
  - `checkbox checkbox-xs checkbox-primary` with select-all header.
  - **Model ID**: Monospace (`font-mono text-xs font-semibold text-gray-200`) with "Latest" / "Recommended" badges.
  - **Display Name & Capabilities**: Badge chips for `Thinking / Reasoning`, `Vision`, `Tools`.
  - **Suggested Aliases**: Monospace chip preview (e.g. `claude-3.7-sonnet`, `sonnet`, `claude-3-7-sonnet-thinking`).
  - **Status**: Visual indicator if model is already imported in `ccConfig.allowlist`.
- **Footer Controls**:
  - Left: Selection summary text (`"X models selected"`).
  - Right: "Cancel" (ghost button) and "Import Selected" button (`btn-sm bg-purple-600 hover:bg-purple-500 text-white font-medium`).

---

## Proposed Changes

### 1. Backend: Anthropic / Claude Code Model Discovery API

#### File: `internal/claudecode/client.go`
- Define model discovery types:
  ```go
  type DiscoveredModel struct {
      ID           string   `json:"id"`
      DisplayName  string   `json:"display_name"`
      CreatedAt    string   `json:"created_at,omitempty"`
      Capabilities []string `json:"capabilities,omitempty"` // e.g. ["thinking", "vision", "tools"]
      Aliases      []string `json:"aliases,omitempty"`      // recommended aliases
      Family       string   `json:"family,omitempty"`       // sonnet, haiku, opus
  }
  ```
- Implement `FetchModels(ctx context.Context, token string, baseURL string) ([]DiscoveredModel, error)`:
  - If `token` is empty, query `accountManager.GetActiveAccount()`.
  - If `baseURL` is empty, use `https://api.anthropic.com`.
  - Send HTTP GET request to `{baseURL}/v1/models` with headers:
    - `Authorization: Bearer <token>`
    - `anthropic-version: 2023-06-01`
    - `User-Agent: Claude-Code/2.1.246`
  - Map response items and enrich with capability tags & alias suggestions.
  - If upstream API call fails (or token missing), return standard offline catalogue fallback:
    - `claude-3-7-sonnet-20250219` (Claude 3.7 Sonnet; Aliases: `claude-3.7-sonnet`, `claude-3-7-sonnet`, `sonnet`, `claude-3-7-sonnet-thinking`)
    - `claude-3-5-sonnet-20241022` (Claude 3.5 Sonnet v2; Aliases: `claude-3.5-sonnet`, `claude-3-5-sonnet`)
    - `claude-3-5-haiku-20241022` (Claude 3.5 Haiku; Aliases: `claude-3.5-haiku`, `claude-3-5-haiku`, `haiku`)
    - `claude-3-opus-20240229` (Claude 3 Opus; Aliases: `claude-3-opus`, `opus`)
    - `claude-3-sonnet-20240229` (Claude 3 Sonnet)
    - `claude-3-haiku-20240307` (Claude 3 Haiku)

#### File: `internal/api/claudecode_management.go` & `internal/api/management.go`
- Route registration in `internal/api/management.go`:
  ```go
  case request.Method == http.MethodPost && request.URL.Path == "/api/claudecode/models":
      server.handleClaudeCodeModelsFetch(writer, request)
  ```
- Handler `handleClaudeCodeModelsFetch`:
  - Parses request JSON (`{ "token": "...", "baseUrl": "..." }`).
  - Executes `claudecode.DefaultClient.FetchModels(ctx, token, baseURL)`.
  - Responds with `{"status": "ok", "models": [...], "total": N}`.

---

### 2. Frontend: Settings View & Modal Markup

#### File: `internal/webui/public/views/settings.html`
- **Section 0c Model Allowlist Header**:
  - Insert "Discover Models" action button adjacent to "Import Defaults":
    ```html
    <button class="btn btn-xs btn-ghost text-purple-400 hover:text-purple-300 hover:bg-purple-950/40 gap-1.5 border border-purple-500/20"
            @click="openClaudeCodeDiscoverModal()">
        <svg class="w-3.5 h-3.5 text-purple-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M21 21l-6-6m2-5a7 7 0 11-14 0 7 7 0 0114 0z"></path>
        </svg>
        <span x-text="$t('ccDiscoverModels') || 'Discover Models'"></span>
    </button>
    ```
- **Modal Component (`claudecode_discover_modal`)**:
  - Place alongside `openrouter_discover_modal` and `kimi_discover_modal` at the bottom of `settings.html`.
  - Include search bar, category pill filter (`ccDiscoverFamilyFilter`), select-all checkbox, and interactive table.
  - Badge imported rows with `"Imported"` status.

---

### 3. Frontend: Alpine Component & State

#### File: `internal/webui/public/js/components/models.js`
- **State variables**:
  ```js
  ccDiscoverLoading: false,
  ccDiscoverError: '',
  ccDiscoveredModels: [],
  ccDiscoverSearch: '',
  ccDiscoverFamilyFilter: 'all',
  ccSelectedModels: new Set(),
  ```
- **Computed / Filtered list**:
  - `filteredClaudeCodeModels`: filters by search query and family tag (`all`, `sonnet`, `haiku`, `opus`).
- **Methods**:
  - `openClaudeCodeDiscoverModal()`: loads `/api/claudecode/models`, opens dialog.
  - `closeClaudeCodeDiscoverModal()`: closes dialog.
  - `toggleClaudeCodeModelSelection(modelId)`: toggles selection set.
  - `selectAllClaudeCodeModels(select)`: selects/deselects all visible filtered models.
  - `importSelectedClaudeCodeModels()`:
    - Adds selected models into `ccConfig.allowlist`.
    - Automatically populates model ID, display name, and recommended aliases.
    - Closes modal, sets dirty state, and fires success toast `$store.global.showToast(...)`.

---

### 4. Internationalization

#### Files: `internal/webui/public/js/translations/{en,pt,zh,id,tr}.js`
Add translations:
- `ccDiscoverModels`: `'Discover Models'`
- `ccDiscoverTitle`: `'Anthropic / Claude Code Model Discovery'`
- `ccDiscoverSubtitle`: `'Query official Anthropic model registry or import standard Claude models'`
- `ccDiscoverSearchPlaceholder`: `'Search Claude models by ID, name or tag...'`
- `ccDiscoverFilterAll`: `'All Models'`
- `ccDiscoverFilterSonnet`: `'Sonnet'`
- `ccDiscoverFilterHaiku`: `'Haiku'`
- `ccDiscoverFilterOpus`: `'Opus'`
- `ccDiscoverSelectAll`: `'Select All'`
- `ccDiscoverDeselectAll`: `'Deselect All'`
- `ccDiscoverImportSelected`: `'Import Selected (%s)'`
- `ccDiscoverNoModels`: `'No Claude models found matching your search.'`
- `ccDiscoverSuccess`: `'Imported %d model(s) into Claude Code allowlist'`
- `ccDiscoverAlreadyImported`: `'Already in allowlist'`

---

## Verification Plan

### Automated Tests
1. **`internal/claudecode/client_test.go`**:
   - `TestFetchModels_LiveMock`: Verifies JSON parsing and enrichment from mock Anthropic `/v1/models` endpoint.
   - `TestFetchModels_FallbackCatalogue`: Verifies fallback list returned when upstream is unreachable.
2. **`internal/api/claudecode_management_test.go`**:
   - `TestHandleClaudeCodeModelsFetch`: Verifies `POST /api/claudecode/models` returns `200 OK` with model list.
3. **Full Suite**: Run `go test -count=1 ./...` to verify 100% test pass.

### Manual / Browser Verification
1. Start proxy: `./bin/antigravity-proxy`.
2. Open `http://localhost:8080/#settings/models`.
3. Verify "Discover Models" button rendered with purple styling in Section 0c.
4. Click button: verify modal opens, search and family filter pills work smoothly.
5. Select `claude-3-7-sonnet-20250219` and click "Import Selected".
6. Verify row is added to the Claude Code model allowlist table with aliases populated.
7. Save settings and verify persistence.
