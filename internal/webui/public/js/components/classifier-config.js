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
        }
    },
    init() {
        this.loadConfig();
        if (this.$watch) {
            this.$watch('$store.global.settingsTab', (tab) => {
                if (tab === 'classifier') {
                    this.loadConfig();
                }
            });
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
                    }
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
            if (Alpine.store('settings')?.config) {
                Alpine.store('settings').config.classifier = JSON.parse(JSON.stringify(this.config));
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
