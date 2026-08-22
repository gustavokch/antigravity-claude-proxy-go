/**
 * Logs Viewer Component
 * Registers itself to window.Components for Alpine.js to consume
 */
window.Components = window.Components || {};

let logIdCounter = 0;

window.Components.logsViewer = () => ({
    logs: [],
    isAutoScroll: true,
    eventSource: null,
    searchQuery: '',
    filters: {
        INFO: true,
        WARN: true,
        ERROR: true,
        SUCCESS: true,
        DEBUG: false
    },
    _queue: [],
    _flushScheduled: false,

    get filteredLogs() {
        const query = this.searchQuery.trim();
        if (!query) {
            return this.logs.filter(log => this.filters[log.level]);
        }

        // Try regex first, fallback to plain text search
        let matcher;
        try {
            const regex = new RegExp(query, 'i');
            matcher = (msg) => regex.test(msg);
        } catch (e) {
            // Invalid regex, fallback to case-insensitive string search
            const lowerQuery = query.toLowerCase();
            matcher = (msg) => msg.toLowerCase().includes(lowerQuery);
        }

        return this.logs.filter(log => {
            // Level Filter
            if (!this.filters[log.level]) return false;

            // Search Filter
            return matcher(log.message);
        });
    },

    getRenderedMessage(log) {
        if (log._renderedText !== undefined && log._renderedText !== null) return log._renderedText;
        const shouldRedact = Alpine.store('settings')?.redactMode && window.Redact;
        const msg = shouldRedact ? window.Redact.logMessage(log.message) : log.message;
        log._renderedText = msg;
        return log._renderedText;
    },

    init() {
        this.startLogStream();

        // Sync DEBUG filter with debugLogging sub-toggle
        const settings = Alpine.store('settings');
        if (settings) {
            this.filters.DEBUG = !!settings.debugLogging;
            this.$watch('$store.settings.debugLogging', (val) => {
                this.filters.DEBUG = !!val;
            });
        }

        // Invalidate rendered text cache if redactMode changes
        this.$watch('$store.settings.redactMode', () => {
            for (let i = 0; i < this.logs.length; i++) {
                this.logs[i]._renderedText = null;
            }
            for (let i = 0; i < this._queue.length; i++) {
                this._queue[i]._renderedText = null;
            }
        });

        this.$watch('isAutoScroll', (val) => {
            if (val) this.scrollToBottom();
        });

        // Watch filters to maintain auto-scroll if enabled
        this.$watch('searchQuery', () => { if (this.isAutoScroll) this.$nextTick(() => this.scrollToBottom()); });
        this.$watch('filters', () => { if (this.isAutoScroll) this.$nextTick(() => this.scrollToBottom()); });
    },

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

    scheduleFlush() {
        if (this._flushScheduled) return;
        this._flushScheduled = true;

        requestAnimationFrame(() => {
            this.flushQueue();
        });
    },

    flushQueue() {
        this._flushScheduled = false;
        if (this._queue.length === 0) return;

        const newItems = this._queue;
        this._queue = [];

        // Batch append
        this.logs = this.logs.concat(newItems);

        // Limit log buffer
        const limit = Alpine.store('settings')?.logLimit || window.AppConstants?.LIMITS?.DEFAULT_LOG_LIMIT || 2000;
        if (this.logs.length > limit) {
            this.logs = this.logs.slice(-limit);
        }

        if (this.isAutoScroll) {
            this.$nextTick(() => this.scrollToBottom());
        }
    },

    startLogStream() {
        if (this.eventSource) this.eventSource.close();

        const password = Alpine.store('global')?.webuiPassword;
        const url = password
            ? `/api/logs/stream?history=true&password=${encodeURIComponent(password)}`
            : '/api/logs/stream?history=true';

        this.eventSource = new EventSource(url);
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

        this.eventSource.onerror = () => {
            if (window.UILogger) window.UILogger.debug('Log stream disconnected, reconnecting...');
            setTimeout(() => this.startLogStream(), 3000);
        };
    },

    scrollToBottom() {
        const container = document.getElementById('logs-container');
        if (container) container.scrollTop = container.scrollHeight;
    },

    clearLogs() {
        this._queue = [];
        this.logs = [];
    },

    exportLogs() {
        if (this.logs.length === 0) return;

        const shouldRedact = Alpine.store('settings')?.redactMode && window.Redact;
        const lines = this.logs.map(log => {
            const ts = new Date(log.timestamp).toISOString();
            const message = shouldRedact ? window.Redact.logMessage(log.message) : log.message;
            return `[${ts}] [${log.level}] ${message}`;
        });

        const text = lines.join('\n');
        const blob = new Blob([text], { type: 'text/plain' });
        const url = URL.createObjectURL(blob);
        const a = document.createElement('a');
        a.href = url;
        a.download = `proxy-logs-${new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19)}.txt`;
        document.body.appendChild(a);
        a.click();
        document.body.removeChild(a);
        URL.revokeObjectURL(url);
    }
});
