# Fix PR #17 Review Findings Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Address and resolve all code review findings on PR #17 (bounded SSE queue, timestamp validation, safe text rendering / XSS prevention, and redactMode cache invalidation).

**Architecture:** Refactor `logs-viewer.js` and `logs.html` to bound queue memory during background tab buffering, validate date parsing to prevent `"Invalid Date"` strings, replace `x-html` with `x-text` and CSS `whitespace-pre-wrap` for safe message rendering without HTML escaping overhead or XSS vulnerabilities, and invalidate pending queue caches on redact mode toggle.

**Tech Stack:** JavaScript (ES6), Alpine.js, Tailwind CSS / HTML5

**Spec:** PR #17 review comments (https://github.com/gustavokch/antigravity-claude-proxy-go/pull/17#issuecomment-5382217914)

## Global Constraints

- Preserve zero unnecessary GPU compositor redraws and low CPU overhead.
- No external JS libraries or CDN dependencies.
- Ensure backwards compatibility with Alpine.js reactivity and existing redact mode.
- Maintain existing filter and search behavior.

---

### Task 1: Bound SSE queue buffer size during background tab buffering

**Files:**
- Modify: `internal/webui/public/js/components/logs-viewer.js:144-154`

**Interfaces:**
- Consumes: `this._queue`, `this.scheduleFlush()`, `this.processIncomingLog(rawLog)`
- Produces: Bounded `this._queue` length capped to `2 * limit`

- [ ] **Step 1: Inspect `startLogStream` in `logs-viewer.js`**

Locate `eventSource.onmessage` handler:
```javascript
        this.eventSource.onmessage = (event) => {
            try {
                const rawLog = JSON.parse(event.data);
                const log = this.processIncomingLog(rawLog);
                this._queue.push(log);
                this.scheduleFlush();
            } catch (e) {
                if (window.UILogger) window.UILogger.debug('Log parse error:', e.message);
            }
        };
```

- [ ] **Step 2: Add buffer capping to `_queue` on push**

When the browser tab is hidden or backgrounded, `requestAnimationFrame` does not fire and `_queue` can grow unbounded. Add a buffer cap:
```javascript
        this.eventSource.onmessage = (event) => {
            try {
                const rawLog = JSON.parse(event.data);
                const log = this.processIncomingLog(rawLog);
                this._queue.push(log);

                // Cap pending queue to prevent memory leak when tab is backgrounded
                const limit = Alpine.store('settings')?.logLimit || window.AppConstants?.LIMITS?.DEFAULT_LOG_LIMIT || 2000;
                if (this._queue.length > limit * 2) {
                    this._queue = this._queue.slice(-limit);
                }

                this.scheduleFlush();
            } catch (e) {
                if (window.UILogger) window.UILogger.debug('Log parse error:', e.message);
            }
        };
```

- [ ] **Step 3: Verify queue capping logic**

Review `logs-viewer.js` to ensure `this._queue` never exceeds `2 * limit` even if `requestAnimationFrame` is delayed or inactive.

- [ ] **Step 4: Commit**

```bash
git add internal/webui/public/js/components/logs-viewer.js
git commit -m "fix(webui): cap pending log queue to prevent unbounded memory growth in background tabs"
```

---

### Task 2: Validate timestamp parsing and handle invalid dates safely

**Files:**
- Modify: `internal/webui/public/js/components/logs-viewer.js:86-103`

**Interfaces:**
- Consumes: `rawLog.timestamp`
- Produces: `formattedTime` (valid 24h time string or fallback to `rawLog.timestamp || ''`)

- [ ] **Step 1: Inspect `processIncomingLog` in `logs-viewer.js`**

Current implementation:
```javascript
    processIncomingLog(rawLog) {
        logIdCounter++;
        let formattedTime = '';
        try {
            formattedTime = new Date(rawLog.timestamp).toLocaleTimeString([], { hour12: false });
        } catch (_) {
            formattedTime = rawLog.timestamp || '';
        }
```
`new Date("invalid")` returns `Invalid Date` without throwing, so `toLocaleTimeString()` evaluates to the string `"Invalid Date"`.

- [ ] **Step 2: Implement strict `isNaN(d.getTime())` date validation**

Update `processIncomingLog`:
```javascript
    processIncomingLog(rawLog) {
        logIdCounter++;
        let formattedTime = '';
        if (rawLog.timestamp) {
            const d = new Date(rawLog.timestamp);
            formattedTime = !isNaN(d.getTime())
                ? d.toLocaleTimeString([], { hour12: false })
                : String(rawLog.timestamp);
        }

        return {
            id: logIdCounter,
            timestamp: rawLog.timestamp,
            formattedTime: formattedTime,
            level: rawLog.level || 'INFO',
            message: rawLog.message || '',
            _renderedText: null
        };
    },
```

- [ ] **Step 3: Verify invalid date fallback**

Verify that:
1. Valid ISO timestamps (e.g. `"2026-08-22T19:27:19Z"`) produce formatted local time (`HH:mm:ss`).
2. Invalid date strings (e.g. `"not-a-date"`) produce `"not-a-date"`.
3. Null or undefined timestamps produce `""`.

- [ ] **Step 4: Commit**

```bash
git add internal/webui/public/js/components/logs-viewer.js
git commit -m "fix(webui): validate timestamp parsing and avoid Invalid Date strings"
```

---

### Task 3: Safe text rendering & XSS prevention with CSS whitespace preservation

**Files:**
- Modify: `internal/webui/public/views/logs.html:90-94`
- Modify: `internal/webui/public/js/components/logs-viewer.js:50-56`

**Interfaces:**
- Consumes: `log.message`, `Alpine.store('settings').redactMode`, `window.Redact.logMessage`
- Produces: `getRenderedMessage(log)` returning plain text, rendered via `x-text` inside a `whitespace-pre-wrap` container

- [ ] **Step 1: Update `getRenderedMessage` in `logs-viewer.js`**

Switch caching property from `_renderedHtml` to `_renderedText` and return raw/redacted plain text without `<br>` injection:
```javascript
    getRenderedMessage(log) {
        if (log._renderedText !== null && log._renderedText !== undefined) return log._renderedText;
        const shouldRedact = Alpine.store('settings')?.redactMode && window.Redact;
        const msg = shouldRedact ? window.Redact.logMessage(log.message) : log.message;
        log._renderedText = msg;
        return log._renderedText;
    },
```

- [ ] **Step 2: Update `logs.html` to use `x-text` and `whitespace-pre-wrap`**

Replace `x-html` with `x-text` in `internal/webui/public/views/logs.html`:
```html
                <!-- Message: Clean & High Contrast -->
                <span class="text-zinc-300 break-all whitespace-pre-wrap group-hover:text-white flex-1"
                    x-text="getRenderedMessage(log)"></span>
```

- [ ] **Step 3: Verify message formatting and XSS safety**

Verify that:
1. Multi-line log messages preserve newline breaks correctly via CSS `whitespace-pre-wrap`.
2. Angle brackets and HTML tags in log messages (e.g., `<nil>`, `<unknown>`, `<tool_call>`) render safely as literal text without being parsed as HTML elements or stripped.
3. Redact mode works as expected with plain text.

- [ ] **Step 4: Commit**

```bash
git add internal/webui/public/views/logs.html internal/webui/public/js/components/logs-viewer.js
git commit -m "fix(webui): use x-text with whitespace-pre-wrap to eliminate XSS and tag parsing issues"
```

---

### Task 4: Invalidate pending queue cache on `redactMode` changes

**Files:**
- Modify: `internal/webui/public/js/components/logs-viewer.js:70-76`

**Interfaces:**
- Consumes: `$store.settings.redactMode` watcher
- Produces: Reset `_renderedText = null` on both `this.logs` and `this._queue` items

- [ ] **Step 1: Inspect `redactMode` watcher in `logs-viewer.js`**

Current implementation:
```javascript
        // Invalidate rendered HTML cache if redactMode changes
        this.$watch('$store.settings.redactMode', () => {
            for (let i = 0; i < this.logs.length; i++) {
                this.logs[i]._renderedHtml = null;
            }
        });
```

- [ ] **Step 2: Invalidate both `this.logs` and `this._queue`**

Update watcher to clear `_renderedText` across both active logs and pending queued logs:
```javascript
        // Invalidate rendered text cache if redactMode changes
        this.$watch('$store.settings.redactMode', () => {
            for (let i = 0; i < this.logs.length; i++) {
                this.logs[i]._renderedText = null;
            }
            for (let i = 0; i < this._queue.length; i++) {
                this._queue[i]._renderedText = null;
            }
        });
```

- [ ] **Step 3: Commit**

```bash
git add internal/webui/public/js/components/logs-viewer.js
git commit -m "fix(webui): invalidate pending queue text cache when redactMode changes"
```

---

### Task 5: End-to-End Verification

**Files:**
- Test/Verify: `internal/webui/public/js/components/logs-viewer.js`, `internal/webui/public/views/logs.html`

- [ ] **Step 1: Run Go tests**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 2: Verify WebUI static files syntax**

Run: `git status` and verify clean syntax in modified JavaScript and HTML files.

- [ ] **Step 3: Verification summary**

Confirm all 4 review findings from PR #17 are completely addressed.
