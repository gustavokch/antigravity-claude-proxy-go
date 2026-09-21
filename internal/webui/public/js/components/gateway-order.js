/**
 * Gateway Order Component
 * Configurable gateway precedence for colliding model IDs: a global order
 * list plus per-model overrides. The list is a hint, never a disable list —
 * providers omitted from the persisted order are tried after the listed
 * ones, in default order.
 */
window.Components = window.Components || {};

window.Components.gatewayOrder = () => ({
    loading: false,
    saving: false,
    // Persisted order (may be partial). The rendered list is effectiveOrder().
    order: [],
    byModel: {},
    // Server vocabulary in default precedence order (GET /api/config
    // knownProviders). Labels render through t(gatewayLabelKey(id)).
    knownProviders: [],
    newModel: '',
    error: '',

    init() {
        this.loadConfig();
        if (this.$watch) {
            this.$watch('$store.global.settingsTab', (tab) => {
                if (tab === 'models') {
                    this.loadConfig();
                }
            });
        }
    },

    gatewayLabelKey(id) {
        return 'gateway' + id.charAt(0).toUpperCase() + id.slice(1);
    },

    // Configured providers first, then the omitted ones in default order.
    // Omitted providers render muted: they are tried after the listed ones,
    // not disabled.
    effectiveOrder() {
        const listed = (this.order || []).filter((id) => this.knownProviders.includes(id));
        const rest = (this.knownProviders || []).filter((id) => !listed.includes(id));
        return [...listed, ...rest];
    },

    isListed(id) {
        return (this.order || []).includes(id);
    },

    overrideNames() {
        return Object.keys(this.byModel || {}).sort();
    },

    move(id, dir) {
        const eff = this.effectiveOrder();
        const index = eff.indexOf(id);
        const target = index + dir;
        if (index < 0 || target < 0 || target >= eff.length) return;
        const next = eff.slice();
        next[index] = next[target];
        next[target] = id;
        // Moving a provider is what adds it to the persisted order.
        this.order = next;
        this.saveConfigDebounced();
    },

    moveOverride(model, id, dir) {
        const list = this.byModel[model] || [];
        const index = list.indexOf(id);
        const target = index + dir;
        if (index < 0 || target < 0 || target >= list.length) return;
        const next = list.slice();
        next[index] = next[target];
        next[target] = id;
        this.byModel = { ...this.byModel, [model]: next };
        this.saveConfigDebounced();
    },

    addOverride() {
        const model = (this.newModel || '').trim();
        if (!model) return;
        if (!this.byModel[model]) {
            // A new override starts as a copy of the global order so the
            // operator reorders from today's behaviour, not from scratch.
            this.byModel = { ...this.byModel, [model]: this.effectiveOrder() };
        }
        this.newModel = '';
        this.saveConfig();
    },

    removeOverride(model) {
        const next = { ...this.byModel };
        delete next[model];
        this.byModel = next;
        this.saveConfig();
    },

    reset() {
        this.order = [...this.knownProviders];
        this.byModel = {};
        this.saveConfig();
    },

    // Trailing debounce: up/down clicks fire in bursts; coalesce them into
    // one config POST (models.js moveProvider precedent).
    saveConfigDebounced() {
        if (this._saveTimer) clearTimeout(this._saveTimer);
        this._saveTimer = setTimeout(() => {
            this._saveTimer = null;
            this.saveConfig();
        }, 500);
    },

    async loadConfig() {
        this.loading = true;
        this.error = '';
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
                const section = data?.config?.gatewayOrder || {};
                this.order = Array.isArray(section.order) ? section.order.slice() : [];
                this.byModel = section.byModel && typeof section.byModel === 'object' ? JSON.parse(JSON.stringify(section.byModel)) : {};
                const known = data?.config?.knownProviders;
                if (Array.isArray(known) && known.length > 0) {
                    this.knownProviders = known.slice();
                }
            }
        } catch (err) {
            this.error = err.message;
            console.error('Failed to load gateway order:', err);
        } finally {
            this.loading = false;
        }
    },

    async saveConfig() {
        // Send the whole section: a partial {"order":[...]} POST would rely
        // on the server merge to preserve byModel. Full-section saves keep
        // no such coupling.
        const payload = {
            gatewayOrder: {
                order: this.effectiveOrder(),
                byModel: JSON.parse(JSON.stringify(this.byModel || {}))
            }
        };
        this.saving = true;
        try {
            const password = Alpine.store('global')?.webuiPassword;
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
                let message = errText;
                try {
                    const parsed = JSON.parse(errText);
                    if (parsed && parsed.error) message = parsed.error;
                } catch { /* body was not JSON; show it as-is */ }
                throw new Error(message);
            }
            const data = await res.json().catch(() => null);
            const section = data?.config?.gatewayOrder;
            if (section) {
                if (Array.isArray(section.order)) this.order = section.order.slice();
                if (section.byModel && typeof section.byModel === 'object') this.byModel = section.byModel;
            }
            Alpine.store('global').showToast(Alpine.store('global').t('gatewayOrderSaved') || 'Gateway precedence saved', 'success');
        } catch (err) {
            Alpine.store('global').showToast(err.message, 'error');
        } finally {
            this.saving = false;
        }
    }
});

document.addEventListener('alpine:init', () => {
    Alpine.data('gatewayOrder', window.Components.gatewayOrder);
});
