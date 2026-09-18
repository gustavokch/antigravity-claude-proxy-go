/**
 * Classifier Config Component
 * Interception, rerouting, parameter tuning, and stubbing for Claude Code safety classification requests.
 */
window.Components = window.Components || {};

window.Components.classifierConfig = () => ({
    loading: false,
    saving: false,
    config: {
        enabled: false,
        action: 'fallback_on_exhaustion',
        defaultModel: '',
        defaultMaxTokens: 0,
        defaultTemperature: null,
        compactTranscript: false,
        defaultVerdict: '<severity>0</severity>',
        defaultThinking: 'Routine action, no policy match.',
        variants: {
            'stage1-severity': { maxTokens: 64, cannedVerdict: '' },
            'stage2-severity': { maxTokens: 8192, thinkingText: '', cannedVerdict: '' },
            'block-prefilter': { targetModel: '', cannedVerdict: '' }
        },
        rules: [],
        backends: {}
    },
    auditEvents: [],
    eventSource: null,
    AUDIT_LIMIT: 50,
    init() {
        this.loadConfig();
        if (this.$watch) {
            this.$watch('$store.global.settingsTab', (tab) => {
                if (tab === 'classifier') {
                    this.loadConfig();
                    this.connectAuditStream();
                } else {
                    // Leaving the tab must drop the connection; otherwise
                    // every tab switch leaks another open EventSource.
                    this.disconnectAuditStream();
                }
            });
        }
        if (Alpine.store('global')?.settingsTab === 'classifier') {
            this.connectAuditStream();
        }
    },
    ensureVariants() {
        if (!this.config.variants) this.config.variants = {};
        if (!this.config.variants['stage1-severity']) {
            this.config.variants['stage1-severity'] = { maxTokens: 64, cannedVerdict: '' };
        }
        if (!this.config.variants['stage2-severity']) {
            this.config.variants['stage2-severity'] = { maxTokens: 8192, thinkingText: '', cannedVerdict: '' };
        }
        if (!this.config.variants['block-prefilter']) {
            this.config.variants['block-prefilter'] = { targetModel: '', cannedVerdict: '' };
        }
        if (!Array.isArray(this.config.rules)) this.config.rules = [];
        if (!this.config.backends || typeof this.config.backends !== 'object') this.config.backends = {};
    },
    get backendNames() {
        return Object.keys(this.config.backends || {});
    },
    addRule() {
        this.config.rules.push({
            id: `rule-${Date.now()}`,
            name: '',
            enabled: true,
            conditions: {
                systemPromptPatterns: [],
                footerPatterns: [],
                models: [],
                maxTokensMin: 0,
                maxTokensMax: 0
            },
            action: 'passthrough',
            targetBackend: '',
            verdictTemplate: ''
        });
    },
    removeRule(index) {
        this.config.rules.splice(index, 1);
    },
    addBackend() {
        const key = `backend-${Object.keys(this.config.backends).length + 1}`;
        this.config.backends[key] = {
            name: key,
            url: 'http://127.0.0.1:8000/v1/chat/completions',
            format: 'openai',
            model: '',
            maxTokens: 0,
            timeoutMs: 20000
        };
    },
    removeBackend(key) {
        delete this.config.backends[key];
        // A rule pointing at a deleted backend would be rejected on save, so
        // clear the reference here rather than surfacing a 400 later.
        this.config.rules.forEach((rule) => {
            if (rule.targetBackend === key) rule.targetBackend = '';
        });
    },
    // The editor exposes one pattern per field, which is the shape the
    // captured fingerprints actually need. Patterns are stored as arrays so
    // the backend schema does not have to change when multi-pattern editing
    // is added later.
    patternValue(rule, field) {
        const list = rule.conditions?.[field];
        return Array.isArray(list) && list.length > 0 ? list[0].pattern : '';
    },
    setPattern(rule, field, type, value) {
        if (!rule.conditions) rule.conditions = {};
        rule.conditions[field] = value ? [{ type, pattern: value }] : [];
    },
    connectAuditStream() {
        this.disconnectAuditStream();
        const password = Alpine.store('global')?.webuiPassword;
        const url = password
            ? `/api/classifier/audit/stream?history=true&password=${encodeURIComponent(password)}`
            : '/api/classifier/audit/stream?history=true';

        this.eventSource = new EventSource(url);
        this.eventSource.onmessage = (event) => {
            try {
                const parsed = JSON.parse(event.data);
                this.auditEvents.unshift(parsed);
                if (this.auditEvents.length > this.AUDIT_LIMIT) {
                    this.auditEvents.length = this.AUDIT_LIMIT;
                }
            } catch (err) {
                console.error('Failed to parse classifier audit event:', err);
            }
        };
        this.eventSource.onerror = () => {
            // EventSource reconnects on its own; closing here would leave the
            // feed permanently dead after one transient blip.
        };
    },
    disconnectAuditStream() {
        if (this.eventSource) {
            this.eventSource.close();
            this.eventSource = null;
        }
    },
    statusClass(status) {
        switch (status) {
            case 'rerouted': return 'text-green-400 border-green-400/40 bg-green-400/10';
            case 'stubbed': return 'text-yellow-400 border-yellow-400/40 bg-yellow-400/10';
            case 'error': return 'text-red-400 border-red-400/40 bg-red-400/10';
            default: return 'text-gray-400 border-gray-400/40 bg-gray-400/10';
        }
    },
    async loadConfig() {
        const raw = Alpine.store('settings')?.config?.classifier;
        if (raw) {
            this.config = JSON.parse(JSON.stringify(raw));
            this.ensureVariants();
            return;
        }

        this.loading = true;
        try {
            const password = Alpine.store('global')?.webuiPassword;
            let res;
            if (window.utils?.request) {
                const req = await window.utils.request('/api/config', {}, password);
                res = req.response;
                if (req.newPassword && Alpine.store('global')) Alpine.store('global').webuiPassword = req.newPassword;
            } else {
                res = await fetch('/api/config');
            }
            if (res && res.ok) {
                const data = await res.json();
                if (data?.config?.classifier) {
                    this.config = JSON.parse(JSON.stringify(data.config.classifier));
                    this.ensureVariants();
                }
            }
        } catch (err) {
            console.error('Failed to load classifier config:', err);
        } finally {
            this.loading = false;
        }
    },
    async saveConfig() {
        this.saving = true;
        try {
            const password = Alpine.store('global')?.webuiPassword;
            const payload = {
                classifier: {
                    ...this.config,
                    defaultMaxTokens: Number(this.config.defaultMaxTokens) || 0,
                    defaultTemperature: this.config.defaultTemperature !== null && this.config.defaultTemperature !== '' && !isNaN(Number(this.config.defaultTemperature)) ? Number(this.config.defaultTemperature) : null,
                    variants: {
                        ...(this.config.variants || {}),
                        'stage1-severity': {
                            ...(this.config.variants?.['stage1-severity'] || {}),
                            maxTokens: Number(this.config.variants?.['stage1-severity']?.maxTokens) || 0,
                            cannedVerdict: this.config.variants?.['stage1-severity']?.cannedVerdict || ''
                        },
                        'stage2-severity': {
                            ...(this.config.variants?.['stage2-severity'] || {}),
                            maxTokens: Number(this.config.variants?.['stage2-severity']?.maxTokens) || 0,
                            thinkingText: this.config.variants?.['stage2-severity']?.thinkingText || '',
                            cannedVerdict: this.config.variants?.['stage2-severity']?.cannedVerdict || ''
                        },
                        'block-prefilter': {
                            ...(this.config.variants?.['block-prefilter'] || {}),
                            targetModel: this.config.variants?.['block-prefilter']?.targetModel || '',
                            cannedVerdict: this.config.variants?.['block-prefilter']?.cannedVerdict || ''
                        }
                    },
                    rules: (this.config.rules || []).map((rule) => ({
                        ...rule,
                        enabled: !!rule.enabled,
                        conditions: {
                            ...(rule.conditions || {}),
                            maxTokensMin: Number(rule.conditions?.maxTokensMin) || 0,
                            maxTokensMax: Number(rule.conditions?.maxTokensMax) || 0
                        }
                    })),
                    backends: Object.fromEntries(
                        Object.entries(this.config.backends || {}).map(([key, backend]) => [key, {
                            ...backend,
                            maxTokens: Number(backend.maxTokens) || 0,
                            timeoutMs: Number(backend.timeoutMs) || 0,
                            // An empty apiKey means "unchanged" server-side,
                            // which is what the redacted GET forces here.
                            apiKey: backend.apiKey || ''
                        }])
                    )
                }
            };

            let res;
            if (window.utils?.request) {
                const req = await window.utils.request('/api/config', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(payload)
                }, password);
                res = req.response;
                if (req.newPassword && Alpine.store('global')) Alpine.store('global').webuiPassword = req.newPassword;
            } else {
                res = await fetch('/api/config', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(payload)
                });
            }

            if (!res.ok) {
                const errText = await res.text();
                throw new Error(errText);
            }
            this.config = JSON.parse(JSON.stringify(payload.classifier));
            if (Alpine.store('settings')?.config) {
                Alpine.store('settings').config.classifier = JSON.parse(JSON.stringify(payload.classifier));
            }
            Alpine.store('global').showToast(Alpine.store('global').t('configSaved') || 'Config saved', 'success');
        } catch (err) {
            Alpine.store('global').showToast(err.message, 'error');
        } finally {
            this.saving = false;
        }
    }
});

document.addEventListener('alpine:init', () => {
    Alpine.data('classifierConfig', window.Components.classifierConfig);
});
