/**
 * Models Component
 * Displays model quota/status list
 * Registers itself to window.Components for Alpine.js to consume
 */
window.Components = window.Components || {};

window.Components.models = () => ({
    // Color palette for per-account threshold markers
    thresholdColors: [
        { bg: '#eab308', shadow: 'rgba(234,179,8,0.5)' },    // yellow
        { bg: '#06b6d4', shadow: 'rgba(6,182,212,0.5)' },     // cyan
        { bg: '#a855f7', shadow: 'rgba(168,85,247,0.5)' },    // purple
        { bg: '#22c55e', shadow: 'rgba(34,197,94,0.5)' },     // green
        { bg: '#ef4444', shadow: 'rgba(239,68,68,0.5)' },     // red
        { bg: '#f97316', shadow: 'rgba(249,115,22,0.5)' },    // orange
        { bg: '#ec4899', shadow: 'rgba(236,72,153,0.5)' },    // pink
        { bg: '#8b5cf6', shadow: 'rgba(139,92,246,0.5)' },    // violet
    ],

    getThresholdColor(index) {
        return this.thresholdColors[index % this.thresholdColors.length];
    },

    // Drag state for threshold markers
    dragging: {
        active: false,
        email: null,
        modelId: null,
        barRect: null,
        currentPct: 0,
        originalPct: 0
    },

    // Tracks which model rows have their per-account breakdown expanded beyond the cap
    expandedModels: new Set(),

    isExpanded(modelId) {
        return this.expandedModels.has(modelId);
    },

    toggleExpanded(modelId) {
        if (this.expandedModels.has(modelId)) {
            this.expandedModels.delete(modelId);
        } else {
            this.expandedModels.add(modelId);
        }
        // Force Alpine reactivity
        this.expandedModels = new Set(this.expandedModels);
    },

    /**
     * Get visible account rows for a model's breakdown, respecting the cap
     */
    getVisibleAccounts(row) {
        const all = row.quotaInfo || [];
        if (Alpine.store('settings').showAllAccounts || this.isExpanded(row.modelId)) {
            return all;
        }
        const limit = window.AppConstants.LIMITS.ACCOUNT_BREAKDOWN_LIMIT;
        return all.slice(0, limit);
    },

    /**
     * Get the number of hidden accounts for a model row
     */
    getHiddenCount(row) {
        const all = row.quotaInfo || [];
        const limit = window.AppConstants.LIMITS.ACCOUNT_BREAKDOWN_LIMIT;
        if (Alpine.store('settings').showAllAccounts || this.isExpanded(row.modelId)) {
            return 0;
        }
        return Math.max(0, all.length - limit);
    },

    // Model editing state (from main)
    editingModelId: null,
    newMapping: '',

    isEditing(modelId) {
        return this.editingModelId === modelId;
    },

    startEditing(modelId) {
        this.editingModelId = modelId;
    },

    stopEditing() {
        this.editingModelId = null;
    },

    /**
     * Start dragging a threshold marker
     */
    startDrag(event, q, row) {
        // Find the progress bar element (closest .relative container)
        const markerEl = event.currentTarget;
        const barContainer = markerEl.parentElement;
        const barRect = barContainer.getBoundingClientRect();

        this.dragging = {
            active: true,
            email: q.fullEmail,
            modelId: row.modelId,
            barRect,
            currentPct: q.thresholdPct,
            originalPct: q.thresholdPct
        };

        // Prevent text selection while dragging
        document.body.classList.add('select-none');

        // Bind document-level listeners for smooth dragging outside the marker
        this._onDrag = (e) => this.onDrag(e);
        this._endDrag = () => this.endDrag();
        document.addEventListener('mousemove', this._onDrag);
        document.addEventListener('mouseup', this._endDrag);
        document.addEventListener('touchmove', this._onDrag, { passive: false });
        document.addEventListener('touchend', this._endDrag);
    },

    /**
     * Handle drag movement — compute percentage from mouse position
     */
    onDrag(event) {
        if (!this.dragging.active) return;
        event.preventDefault();

        const clientX = event.touches ? event.touches[0].clientX : event.clientX;
        const { left, width } = this.dragging.barRect;
        let pct = Math.round((clientX - left) / width * 100);
        pct = Math.max(0, Math.min(99, pct));

        this.dragging.currentPct = pct;
    },

    /**
     * End drag — save the new threshold value
     */
    endDrag() {
        if (!this.dragging.active) return;

        // Clean up listeners
        document.removeEventListener('mousemove', this._onDrag);
        document.removeEventListener('mouseup', this._endDrag);
        document.removeEventListener('touchmove', this._onDrag);
        document.removeEventListener('touchend', this._endDrag);
        document.body.classList.remove('select-none');

        const { email, modelId, currentPct, originalPct } = this.dragging;

        // Only save if value actually changed
        if (currentPct !== originalPct) {
            // Optimistic in-place update: mutate existing quotaInfo entries directly
            // to avoid full DOM rebuild from computeQuotaRows()
            const dataStore = Alpine.store('data');
            const account = dataStore.accounts.find(a => a.email === email);
            if (account) {
                if (!account.modelQuotaThresholds) account.modelQuotaThresholds = {};
                if (currentPct === 0) {
                    delete account.modelQuotaThresholds[modelId];
                } else {
                    account.modelQuotaThresholds[modelId] = currentPct / 100;
                }
            }
            // Patch quotaRows in-place so Alpine updates without tearing down DOM
            const rows = dataStore.quotaRows || [];
            for (const row of rows) {
                if (row.modelId !== modelId) continue;
                for (const q of row.quotaInfo) {
                    if (q.fullEmail !== email) continue;
                    q.thresholdPct = currentPct;
                }
                // Recompute row-level threshold stats
                const activePcts = row.quotaInfo.map(q => q.thresholdPct).filter(t => t > 0);
                row.effectiveThresholdPct = activePcts.length > 0 ? Math.max(...activePcts) : 0;
                row.hasVariedThresholds = new Set(activePcts).size > 1;
            }
            this.dragging.active = false;
            this.saveModelThreshold(email, modelId, currentPct);
        } else {
            this.dragging.active = false;
        }
    },

    /**
     * Save a per-model threshold for an account via PATCH
     */
    async saveModelThreshold(email, modelId, pct) {
        const store = Alpine.store('global');
        const dataStore = Alpine.store('data');

        const account = dataStore.accounts.find(a => a.email === email);
        if (!account) return;

        // Snapshot for rollback on failure
        const previousModelThresholds = account.modelQuotaThresholds ? { ...account.modelQuotaThresholds } : {};

        // Build full modelQuotaThresholds for API (full replacement, not merge)
        const existingModelThresholds = { ...(account.modelQuotaThresholds || {}) };

        // Preserve the account-level quotaThreshold
        const quotaThreshold = account.quotaThreshold !== undefined ? account.quotaThreshold : null;

        try {
            const { response, newPassword } = await window.utils.request(
                `/api/accounts/${encodeURIComponent(email)}`,
                {
                    method: 'PATCH',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ quotaThreshold, modelQuotaThresholds: existingModelThresholds })
                },
                store.webuiPassword
            );
            if (newPassword) store.webuiPassword = newPassword;

            const data = await response.json();
            if (data.status === 'ok') {
                const label = pct === 0 ? 'removed' : pct + '%';
                store.showToast(`${email.split('@')[0]} ${modelId} threshold: ${label}`, 'success');
                // Skip fetchData() — optimistic update is already applied,
                // next polling cycle will sync server state
            } else {
                throw new Error(data.error || 'Failed to save threshold');
            }
        } catch (e) {
            // Revert optimistic update on failure
            account.modelQuotaThresholds = previousModelThresholds;
            dataStore.computeQuotaRows();
            store.showToast('Failed to save threshold: ' + e.message, 'error');
        }
    },

    /**
     * Check if a specific marker is currently being dragged
     */
    isDragging(q, row) {
        return this.dragging.active && this.dragging.email === q.fullEmail && this.dragging.modelId === row.modelId;
    },

    /**
     * Get the display percentage for a marker (live during drag, stored otherwise)
     */
    getMarkerPct(q, row) {
        if (this.isDragging(q, row)) return this.dragging.currentPct;
        return q.thresholdPct;
    },

    /**
     * Compute pixel offset for overlapping markers so stacked ones fan out.
     * Markers within 2% of each other are considered overlapping.
     * Returns a CSS pixel offset string (e.g., '6px' or '-6px').
     */
    getMarkerOffset(q, row, qIdx) {
        const pct = this.getMarkerPct(q, row);
        const visible = row.quotaInfo.filter(item => item.thresholdPct > 0 || this.isDragging(item, row));
        // Find all markers within 2% of this one
        const cluster = [];
        visible.forEach((item, idx) => {
            const itemPct = this.getMarkerPct(item, row);
            if (Math.abs(itemPct - pct) <= 2) {
                cluster.push({ item, idx });
            }
        });
        if (cluster.length <= 1) return '0px';
        // Find position of this marker within its cluster
        const posInCluster = cluster.findIndex(c => c.item.fullEmail === q.fullEmail);
        // Spread markers 10px apart, centered on the base position
        const spread = 10;
        const totalWidth = (cluster.length - 1) * spread;
        return (posInCluster * spread - totalWidth / 2) + 'px';
    },

    init() {
        // Ensure data is fetched when this tab becomes active (skip initial trigger)
        this.$watch('$store.global.activeTab', (val, oldVal) => {
            if (val === 'models' && oldVal !== undefined) {
                // Trigger recompute to ensure filters are applied
                this.$nextTick(() => {
                    Alpine.store('data').computeQuotaRows();
                });
            }
        });

        this.$watch('$store.global.settingsTab', (val) => {
            if (val === 'models') {
                this.fetchOpenRouterConfig();
                this.fetchKimiConfig();
                this.loadCCConfig();
            }
        });

        // Initial compute if already on models tab
        if (this.$store.global.activeTab === 'models') {
            this.$nextTick(() => {
                Alpine.store('data').computeQuotaRows();
            });
        }
        if (this.$store.global.settingsTab === 'models') {
            this.fetchOpenRouterConfig();
            this.fetchKimiConfig();
            this.loadCCConfig();
        }
    },

    // OpenRouter State & Methods
    openRouterConfig: {
        enabled: false,
        baseUrl: 'https://openrouter.ai/api',
        apiKey: '',
        hasApiKey: false,
        allowlist: [],
        routing: null,
        responseCache: null,
        appSpoof: {
            title: '',
            categories: '',
            referer: ''
        }
    },
    openRouterSaving: false,
    openRouterError: '',
    discoveredModels: [],
    discoveryLoading: false,
    discoveryError: '',
    discoverySearch: '',
    discoveryProvider: 'all',
    addAllowlistAliasMap: {},

    // Kimi Code Gateway State & Methods
    kimiConfig: {
        enabled: false,
        baseUrl: 'https://api.kimi.com/coding',
        apiKey: '',
        hasApiKey: false,
        allowlist: []
    },
    kimiSaving: false,
    kimiError: '',
    kimiDiscovered: [],
    kimiNewId: '',
    kimiNewAlias: '',
    kimiNewDisplay: '',

    // Per-model provider routing panel state
    expandedRouting: new Set(),
    routingPanels: {}, // modelId -> { loading, error, data }

    isRoutingExpanded(modelId) {
        return this.expandedRouting.has(modelId);
    },

    toggleRoutingExpanded(modelId) {
        if (this.expandedRouting.has(modelId)) {
            this.expandedRouting.delete(modelId);
        } else {
            this.expandedRouting.add(modelId);
            this.fetchRoutingProviders(modelId);
        }
        this.expandedRouting = new Set(this.expandedRouting);
    },

    // Per-model limits panel state for the Kimi allowlist (no routing panel there)
    expandedKimi: new Set(),

    isKimiExpanded(modelId) {
        return this.expandedKimi.has(modelId);
    },

    toggleKimiExpanded(modelId) {
        if (this.expandedKimi.has(modelId)) {
            this.expandedKimi.delete(modelId);
        } else {
            this.expandedKimi.add(modelId);
        }
        this.expandedKimi = new Set(this.expandedKimi);
    },

    async fetchRoutingProviders(modelId) {
        const password = Alpine.store('global').webuiPassword;
        this.routingPanels = { ...this.routingPanels, [modelId]: { loading: true, error: '', data: null } };
        try {
            const { response, newPassword } = await window.utils.request('/api/openrouter/providers?model=' + encodeURIComponent(modelId), {}, password);
            if (newPassword) Alpine.store('global').webuiPassword = newPassword;
            if (!response.ok) {
                const errData = await response.json().catch(() => ({}));
                throw new Error(errData.error || `HTTP ${response.status}`);
            }
            const data = await response.json();
            this.routingPanels = { ...this.routingPanels, [modelId]: { loading: false, error: '', data } };
        } catch (e) {
            this.routingPanels = { ...this.routingPanels, [modelId]: { loading: false, error: e.message || 'Failed to load providers', data: null } };
        }
    },

    getRoutingPanel(modelId) {
        return this.routingPanels[modelId] || { loading: false, error: '', data: null };
    },

    // Set routing mode for an allowlist item: auto | pinned | custom
    setProviderMode(item, mode) {
        item.providerMode = mode;
        if (mode === 'custom' && (!item.providerOrder || item.providerOrder.length === 0)) {
            const panel = this.getRoutingPanel(item.id);
            if (panel.data && panel.data.providers) {
                item.providerOrder = panel.data.providers.map(p => p.provider);
            }
        }
        this.saveOpenRouterConfigDebounced();
    },

    // Pin a provider (pinned mode)
    pinProvider(item, provider) {
        item.providerMode = 'pinned';
        item.pinnedProvider = provider;
        this.saveOpenRouterConfigDebounced();
    },

    // Reorder within custom mode; dir = -1 (up) or +1 (down)
    moveProvider(item, index, dir) {
        const order = item.providerOrder || [];
        const target = index + dir;
        if (target < 0 || target >= order.length) return;
        const next = order.slice();
        const tmp = next[index];
        next[index] = next[target];
        next[target] = tmp;
        item.providerOrder = next;
        this.saveOpenRouterConfigDebounced();
    },

    // Trailing debounce: pin/reorder clicks fire in bursts; coalesce them
    // into one config POST.
    saveOpenRouterConfigDebounced() {
        if (this._routingSaveTimer) clearTimeout(this._routingSaveTimer);
        this._routingSaveTimer = setTimeout(() => {
            this._routingSaveTimer = null;
            this.saveOpenRouterConfig();
        }, 500);
    },

    // Same trailing debounce for the Kimi allowlist limits input.
    saveKimiConfigDebounced() {
        if (this._kimiSaveTimer) clearTimeout(this._kimiSaveTimer);
        this._kimiSaveTimer = setTimeout(() => {
            this._kimiSaveTimer = null;
            this.saveKimiConfig();
        }, 500);
    },

    formatUptime(entry) {
        if (!entry) return '-';
        // Prefer the server-computed blend (single source of weights). Any
        // numeric value is authoritative — 0 means measured-down, not unknown.
        if (typeof entry.uptime === 'number') {
            return (entry.uptime * 100).toFixed(1) + '%';
        }
        const ep = entry.endpoint;
        if (!ep) return '-';
        const u5 = ep.uptime_last_5m, u30 = ep.uptime_last_30m, u1d = ep.uptime_last_1d;
        if (!u5 && !u30 && !u1d) return '-';
        const blend = (u5 || 0) * 0.5 + (u30 || 0) * 0.3 + (u1d || 0) * 0.2;
        return (blend * 100).toFixed(1) + '%';
    },

    // Format a provider's OpenRouter pricing (USD per token) as
    // "$in / $out" per 1M tokens. Missing pricing renders '-' and a zero
    // price renders 'FREE' (OpenRouter free variants). Prices below the
    // 4-decimal render floor clamp to '<$0.0001' so they never read as
    // zero (and stay distinct from FREE).
    formatPrice(entry) {
        const pr = entry && entry.endpoint && entry.endpoint.pricing;
        if (!pr) return '-';
        const perM = (v) => {
            if (typeof v !== 'number' || !isFinite(v)) return '-';
            const m = v * 1e6;
            if (m === 0) return 'FREE';
            if (m >= 0.1) return '$' + m.toFixed(2);
            if (m < 0.00005) return '<$0.0001';
            return '$' + parseFloat(m.toFixed(4));
        };
        return perM(pr.prompt) + ' / ' + perM(pr.completion);
    },

    async fetchOpenRouterConfig() {
        const password = Alpine.store('global').webuiPassword;
        try {
            const { response, newPassword } = await window.utils.request('/api/openrouter/config', {}, password);
            if (newPassword) Alpine.store('global').webuiPassword = newPassword;
            if (!response.ok) throw new Error(`HTTP ${response.status}`);
            const data = await response.json();
            if (data.config) {
                this.openRouterConfig = {
                    enabled: !!data.config.enabled,
                    baseUrl: data.config.baseUrl || 'https://openrouter.ai/api',
                    apiKey: '',
                    hasApiKey: !!data.config.hasApiKey,
                    allowlist: data.config.allowlist || [],
                    routing: data.config.routing || null,
                    responseCache: data.config.responseCache || null,
                    appSpoof: {
                        title: (data.config.appSpoof && data.config.appSpoof.title) || '',
                        categories: (data.config.appSpoof && data.config.appSpoof.categories) || '',
                        referer: (data.config.appSpoof && data.config.appSpoof.referer) || ''
                    }
                };
                Alpine.store('data').openrouter = this.openRouterConfig;
            }
        } catch (e) {
            console.error('Failed to fetch OpenRouter config:', e);
        }
    },

    async saveOpenRouterConfig() {
        const store = Alpine.store('global');
        const password = store.webuiPassword;
        this.openRouterSaving = true;
        this.openRouterError = '';
        try {
            const payload = {
                enabled: this.openRouterConfig.enabled,
                baseUrl: this.openRouterConfig.baseUrl,
                hasApiKey: this.openRouterConfig.hasApiKey,
                allowlist: this.openRouterConfig.allowlist || []
            };
            // Routing section rides through the whole-object replace; omitting
            // it would wipe the saved routing config.
            if (this.openRouterConfig.routing) {
                payload.routing = this.openRouterConfig.routing;
            }
            // Response cache config has no panel yet, so it only ever arrives
            // from the GET. It still has to ride through the whole-object
            // replace or saving any other field would wipe it.
            if (this.openRouterConfig.responseCache) {
                payload.responseCache = this.openRouterConfig.responseCache;
            }
            // App Spoof rides through the whole-object replace too; only sent
            // when at least one field is non-empty so the saved values aren't
            // clobbered by blanks from a never-touched panel.
            const spoof = this.openRouterConfig.appSpoof || {};
            if ((spoof.title && spoof.title.trim()) || (spoof.categories && spoof.categories.trim()) || (spoof.referer && spoof.referer.trim())) {
                payload.appSpoof = {
                    title: (spoof.title || '').trim(),
                    categories: (spoof.categories || '').trim(),
                    referer: (spoof.referer || '').trim()
                };
            }
            if (this.openRouterConfig.apiKey && this.openRouterConfig.apiKey.trim()) {
                payload.apiKey = this.openRouterConfig.apiKey.trim();
            }
            const { response, newPassword } = await window.utils.request('/api/openrouter/config', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(payload)
            }, password);
            if (newPassword) store.webuiPassword = newPassword;
            if (!response.ok) {
                const errData = await response.json().catch(() => ({}));
                throw new Error(errData.error || `HTTP ${response.status}`);
            }
            const data = await response.json();
            if (data.config) {
                this.openRouterConfig.hasApiKey = !!data.config.hasApiKey;
                if (payload.apiKey) {
                    this.openRouterConfig.apiKey = '';
                }
                Alpine.store('data').openrouter = this.openRouterConfig;
            }
            store.showToast(store.t('openRouterSavedSuccess') || 'OpenRouter settings saved', 'success');
        } catch (e) {
            this.openRouterError = e.message || 'Failed to save OpenRouter settings';
            store.showToast(this.openRouterError, 'error');
        } finally {
            this.openRouterSaving = false;
        }
    },

    async fetchKimiConfig() {
        const password = Alpine.store('global').webuiPassword;
        try {
            const { response, newPassword } = await window.utils.request('/api/kimi/config', {}, password);
            if (newPassword) Alpine.store('global').webuiPassword = newPassword;
            if (!response.ok) throw new Error(`HTTP ${response.status}`);
            const data = await response.json();
            if (data.config) {
                this.kimiConfig = {
                    enabled: !!data.config.enabled,
                    baseUrl: data.config.baseUrl || 'https://api.kimi.com/coding',
                    apiKey: '',
                    hasApiKey: !!data.config.hasApiKey,
                    allowlist: data.config.allowlist || []
                };
                Alpine.store('data').kimi = this.kimiConfig;
            }
        } catch (e) {
            console.error('Failed to fetch Kimi config:', e);
        }
    },

    async saveKimiConfig() {
        const store = Alpine.store('global');
        const password = store.webuiPassword;
        this.kimiSaving = true;
        this.kimiError = '';
        try {
            const payload = {
                enabled: this.kimiConfig.enabled,
                baseUrl: this.kimiConfig.baseUrl || 'https://api.kimi.com/coding',
                hasApiKey: this.kimiConfig.hasApiKey,
                allowlist: this.kimiConfig.allowlist || []
            };
            if (this.kimiConfig.apiKey && this.kimiConfig.apiKey.trim()) {
                payload.apiKey = this.kimiConfig.apiKey.trim();
            }
            const { response, newPassword } = await window.utils.request('/api/kimi/config', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(payload)
            }, password);
            if (newPassword) store.webuiPassword = newPassword;
            if (!response.ok) {
                const errData = await response.json().catch(() => ({}));
                throw new Error(errData.error || `HTTP ${response.status}`);
            }
            const data = await response.json();
            if (data.config) {
                this.kimiConfig.hasApiKey = !!data.config.hasApiKey;
                if (payload.apiKey) {
                    this.kimiConfig.apiKey = '';
                }
                Alpine.store('data').kimi = this.kimiConfig;
            }
            store.showToast(store.t('kimiSavedSuccess') || 'Kimi settings saved', 'success');
        } catch (e) {
            this.kimiError = e.message || 'Failed to save Kimi settings';
            store.showToast(this.kimiError, 'error');
        } finally {
            this.kimiSaving = false;
        }
    },

    async discoverKimiModels() {
        const password = Alpine.store('global').webuiPassword;
        try {
            const { response, newPassword } = await window.utils.request('/api/kimi/models/fetch', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({})
            }, password);
            if (newPassword) Alpine.store('global').webuiPassword = newPassword;
            if (!response.ok) {
                const errData = await response.json().catch(() => ({}));
                throw new Error(errData.error || `HTTP ${response.status}`);
            }
            const data = await response.json();
            this.kimiDiscovered = (data.models || []).map(m => ({
                id: m.id,
                displayName: m.display_name || m.id,
                contextLength: m.context_length || 0,
                maxOutputTokens: m.max_tokens || 0,
                selected: true
            }));
        } catch (e) {
            const store = Alpine.store('global');
            store.showToast(e.message || 'Failed to fetch Kimi models', 'error');
        }
    },

    async openKimiDiscoverModal() {
        await this.discoverKimiModels();
        const dialog = document.getElementById('kimi_discover_modal');
        if (dialog && typeof dialog.showModal === 'function') {
            dialog.showModal();
        }
    },

    importKimiDiscovered() {
        const picked = this.kimiDiscovered.filter(m => m.selected);
        const existing = new Set(this.kimiConfig.allowlist.map(m => m.id));
        picked.forEach(p => {
            if (!existing.has(p.id)) {
                this.kimiConfig.allowlist.push({
                    id: p.id,
                    alias: '',
                    displayName: p.displayName,
                    contextLength: p.contextLength,
                    // 0 = auto: the proxy derives max_tokens from the model's
                    // own limits instead of a webUI-baked default.
                    maxOutputTokens: 0,
                    enabled: true
                });
            }
        });
        this.saveKimiConfig();
        const dialog = document.getElementById('kimi_discover_modal');
        if (dialog) dialog.close();
    },

    addKimiAllowlistRow() {
        if (!this.kimiNewId) return;
        this.kimiConfig.allowlist.push({
            id: this.kimiNewId,
            alias: this.kimiNewAlias || '',
            displayName: this.kimiNewDisplay || '',
            contextLength: 0,
            maxOutputTokens: 0,
            enabled: true
        });
        this.kimiNewId = '';
        this.kimiNewAlias = '';
        this.kimiNewDisplay = '';
        this.saveKimiConfig();
    },

    // Claude Code Official API Gateway State & Methods
    ccConfig: {
        enabled: false,
        baseUrl: 'https://api.anthropic.com',
        mode: 'pool',
        autoImport: false,
        accounts: [],
        allowlist: [],
        routing: {}
    },
    ccAccounts: [],
    ccSaving: false,
    ccImporting: false,
    ccError: '',
    ccSuccess: '',
    ccNewToken: '',
    ccNewName: '',
    ccNewModelId: '',
    ccNewModelAlias: '',
    ccNewModelDisplay: '',

    // Claude Code Model Discovery State
    ccDiscoverLoading: false,
    ccDiscoverError: '',
    ccDiscoveredModels: [],
    ccDiscoverSearch: '',
    ccDiscoverFamilyFilter: 'all',
    ccSelectedModels: [],

    get filteredClaudeCodeModels() {
        let list = this.ccDiscoveredModels || [];
        const search = (this.ccDiscoverSearch || '').trim().toLowerCase();
        if (search) {
            list = list.filter(m => {
                const id = (m.id || '').toLowerCase();
                const name = (m.display_name || '').toLowerCase();
                const aliases = (m.aliases || []).join(' ').toLowerCase();
                return id.includes(search) || name.includes(search) || aliases.includes(search);
            });
        }
        if (this.ccDiscoverFamilyFilter && this.ccDiscoverFamilyFilter !== 'all') {
            list = list.filter(m => (m.family || '').toLowerCase() === this.ccDiscoverFamilyFilter.toLowerCase());
        }
        return list;
    },

    isClaudeCodeModelImported(modelId) {
        if (!this.ccConfig || !this.ccConfig.allowlist) return false;
        return this.ccConfig.allowlist.some(m => m.id === modelId);
    },

    isClaudeCodeModelSelected(modelId) {
        return this.ccSelectedModels.includes(modelId);
    },

    toggleClaudeCodeModelSelection(modelId) {
        const idx = this.ccSelectedModels.indexOf(modelId);
        if (idx > -1) {
            this.ccSelectedModels.splice(idx, 1);
        } else {
            this.ccSelectedModels.push(modelId);
        }
    },

    toggleSelectAllClaudeCodeModels() {
        const visible = this.filteredClaudeCodeModels;
        const allSelected = visible.length > 0 && visible.every(m => this.isClaudeCodeModelSelected(m.id));
        if (allSelected) {
            const visibleIds = new Set(visible.map(m => m.id));
            this.ccSelectedModels = this.ccSelectedModels.filter(id => !visibleIds.has(id));
        } else {
            const set = new Set(this.ccSelectedModels);
            visible.forEach(m => set.add(m.id));
            this.ccSelectedModels = Array.from(set);
        }
    },

    async openClaudeCodeDiscoverModal() {
        this.ccDiscoverError = '';
        this.ccDiscoverSearch = '';
        this.ccDiscoverFamilyFilter = 'all';
        this.ccSelectedModels = [];
        const dialog = document.getElementById('claudecode_discover_modal');
        if (dialog && typeof dialog.showModal === 'function') {
            dialog.showModal();
        }
        if (!this.ccDiscoveredModels || this.ccDiscoveredModels.length === 0) {
            await this.fetchDiscoveredClaudeCodeModels();
        }
    },

    closeClaudeCodeDiscoverModal() {
        const dialog = document.getElementById('claudecode_discover_modal');
        if (dialog && typeof dialog.close === 'function') {
            dialog.close();
        }
    },

    async fetchDiscoveredClaudeCodeModels() {
        this.ccDiscoverLoading = true;
        this.ccDiscoverError = '';
        const password = Alpine.store('global').webuiPassword;

        try {
            const payload = {
                baseUrl: this.ccConfig.baseUrl || ''
            };
            const { response, newPassword } = await window.utils.request('/api/claudecode/models', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(payload)
            }, password);
            if (newPassword) Alpine.store('global').webuiPassword = newPassword;

            if (!response.ok) {
                const errData = await response.json().catch(() => ({}));
                throw new Error(errData.error || `HTTP ${response.status}`);
            }

            const data = await response.json();
            this.ccDiscoveredModels = data.models || [];
        } catch (e) {
            this.ccDiscoverError = e.message || 'Failed to fetch models from Anthropic API';
        } finally {
            this.ccDiscoverLoading = false;
        }
    },

    importSelectedClaudeCodeModels() {
        if (!this.ccConfig.allowlist) {
            this.ccConfig.allowlist = [];
        }
        const existing = new Set(this.ccConfig.allowlist.map(m => m.id));
        const selected = this.ccDiscoveredModels.filter(m => this.ccSelectedModels.includes(m.id));
        let addedCount = 0;

        selected.forEach(m => {
            if (!existing.has(m.id)) {
                this.ccConfig.allowlist.push({
                    id: m.id,
                    alias: (m.aliases && m.aliases.length > 0) ? m.aliases.join(', ') : '',
                    displayName: m.display_name || '',
                    enabled: true,
                    aliases: m.aliases || []
                });
                existing.add(m.id);
                addedCount++;
            }
        });

        if (addedCount > 0) {
            this.saveCCConfig();
            const store = Alpine.store('global');
            store.showToast(`Imported ${addedCount} model(s) to allowlist`, 'success');
        }

        this.closeClaudeCodeDiscoverModal();
    },

    addCCAllowlistRow() {
        if (!this.ccNewModelId) return;
        if (!this.ccConfig.allowlist) this.ccConfig.allowlist = [];
        this.ccConfig.allowlist.push({
            id: this.ccNewModelId,
            alias: this.ccNewModelAlias || '',
            displayName: this.ccNewModelDisplay || '',
            enabled: true
        });
        this.ccNewModelId = '';
        this.ccNewModelAlias = '';
        this.ccNewModelDisplay = '';
        this.saveCCConfig();
    },

    importCCDefaults() {
        const defaults = [
            { id: 'claude-fable-5', displayName: 'Claude Fable 5', alias: 'claude-fable-5, fable, claude-fable', enabled: true },
            { id: 'claude-opus-5', displayName: 'Claude Opus 5', alias: 'claude-opus-5, opus, claude-5-opus', enabled: true },
            { id: 'claude-sonnet-5', displayName: 'Claude Sonnet 5', alias: 'claude-sonnet-5, sonnet, claude-5-sonnet', enabled: true },
            { id: 'claude-haiku-4-5-20251001', displayName: 'Claude Haiku 4.5', alias: 'claude-haiku-4-5, claude-haiku-4.5, haiku', enabled: true },
            { id: 'claude-opus-4-8', displayName: 'Claude Opus 4.8', alias: 'claude-opus-4.8', enabled: true },
            { id: 'claude-opus-4-7', displayName: 'Claude Opus 4.7', alias: 'claude-opus-4.7', enabled: true },
            { id: 'claude-sonnet-4-6', displayName: 'Claude Sonnet 4.6', alias: 'claude-sonnet-4.6', enabled: true },
            { id: 'claude-opus-4-6', displayName: 'Claude Opus 4.6', alias: 'claude-opus-4.6', enabled: true },
            { id: 'claude-3-7-sonnet-20250219', displayName: 'Claude 3.7 Sonnet', alias: 'claude-3.7-sonnet, claude-3-7-sonnet, claude-3-7-sonnet-thinking', enabled: true },
            { id: 'claude-3-5-sonnet-20241022', displayName: 'Claude 3.5 Sonnet v2', alias: 'claude-3.5-sonnet, claude-3-5-sonnet', enabled: true },
            { id: 'claude-3-5-haiku-20241022', displayName: 'Claude 3.5 Haiku', alias: 'claude-3.5-haiku, claude-3-5-haiku', enabled: true },
            { id: 'claude-3-opus-20240229', displayName: 'Claude 3 Opus', alias: 'claude-3-opus', enabled: true }
        ];
        if (!this.ccConfig.allowlist) this.ccConfig.allowlist = [];
        const existing = new Set(this.ccConfig.allowlist.map(m => m.id));
        defaults.forEach(d => {
            if (!existing.has(d.id)) {
                this.ccConfig.allowlist.push(d);
                existing.add(d.id);
            }
        });
        this.saveCCConfig();
        Alpine.store('global').showToast('Claude Code default models imported', 'success');
    },

    async loadCCConfig() {
        const password = Alpine.store('global').webuiPassword;
        try {
            const { response, newPassword } = await window.utils.request('/api/claudecode/config', {}, password);
            if (newPassword) Alpine.store('global').webuiPassword = newPassword;
            if (!response.ok) return;
            const data = await response.json();
            if (data.config) {
                this.ccConfig = { ...this.ccConfig, ...data.config };
            }
        } catch (_) {}
        await this.loadCCAccounts();
    },

    async loadCCAccounts() {
        const password = Alpine.store('global').webuiPassword;
        try {
            const { response } = await window.utils.request('/api/claudecode/accounts', {}, password);
            if (!response.ok) return;
            const data = await response.json();
            this.ccAccounts = data.accounts || [];
        } catch (_) {}
    },

    async saveCCConfig() {
        const store = Alpine.store('global');
        this.ccSaving = true;
        this.ccError = '';
        this.ccSuccess = '';
        try {
            const { response, newPassword } = await window.utils.request('/api/claudecode/config', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({
                    enabled: this.ccConfig.enabled,
                    baseUrl: this.ccConfig.baseUrl,
                    mode: this.ccConfig.mode,
                    autoImport: this.ccConfig.autoImport,
                    allowlist: this.ccConfig.allowlist,
                    routing: this.ccConfig.routing
                })
            }, store.webuiPassword);
            if (newPassword) store.webuiPassword = newPassword;
            if (!response.ok) {
                const err = await response.json().catch(() => ({}));
                throw new Error(err.error || `HTTP ${response.status}`);
            }
            this.ccSuccess = 'Saved';
            setTimeout(() => { this.ccSuccess = ''; }, 3000);
        } catch (e) {
            this.ccError = e.message || 'Failed to save';
        } finally {
            this.ccSaving = false;
        }
    },

    async addCCAccount() {
        if (!this.ccNewToken.trim()) return;
        const password = Alpine.store('global').webuiPassword;
        this.ccError = '';
        try {
            const id = 'cc-' + Date.now();
            const { response, newPassword } = await window.utils.request('/api/claudecode/accounts', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({
                    id,
                    name: this.ccNewName || id,
                    token: this.ccNewToken.trim(),
                    type: 'api_key',
                    enabled: true
                })
            }, password);
            if (newPassword) Alpine.store('global').webuiPassword = newPassword;
            if (!response.ok) {
                const err = await response.json().catch(() => ({}));
                throw new Error(err.error || `HTTP ${response.status}`);
            }
            this.ccNewToken = '';
            this.ccNewName = '';
            await this.loadCCAccounts();
        } catch (e) {
            this.ccError = e.message || 'Failed to add account';
        }
    },

    async saveCCAccount(acc) {
        const password = Alpine.store('global').webuiPassword;
        try {
            await window.utils.request('/api/claudecode/accounts', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ id: acc.id, name: acc.name, enabled: acc.enabled, hasToken: true, token: '' })
            }, password);
        } catch (_) {}
    },

    async deleteCCAccount(id) {
        const password = Alpine.store('global').webuiPassword;
        try {
            await window.utils.request('/api/claudecode/accounts/' + id, { method: 'DELETE' }, password);
            await this.loadCCAccounts();
        } catch (_) {}
    },

    async testCCAccount(acc) {
        const password = Alpine.store('global').webuiPassword;
        try {
            const { response } = await window.utils.request('/api/claudecode/accounts/' + acc.id + '/test', { method: 'POST' }, password);
            const data = await response.json();
            const store = Alpine.store('global');
            if (data.valid) {
                store.showToast('Account valid ✓', 'success');
            } else {
                store.showToast('Account invalid: ' + (data.error || 'unknown'), 'error');
            }
        } catch (e) {
            Alpine.store('global').showToast('Test failed: ' + e.message, 'error');
        }
    },

    async ccAutoImport() {
        const password = Alpine.store('global').webuiPassword;
        this.ccImporting = true;
        this.ccError = '';
        this.ccSuccess = '';
        try {
            const { response, newPassword } = await window.utils.request('/api/claudecode/import', { method: 'POST' }, password);
            if (newPassword) Alpine.store('global').webuiPassword = newPassword;
            const data = await response.json();
            if (!response.ok) throw new Error(data.error || `HTTP ${response.status}`);
            this.ccSuccess = `Imported ${data.imported} account(s)`;
            setTimeout(() => { this.ccSuccess = ''; }, 4000);
            await this.loadCCAccounts();
        } catch (e) {
            this.ccError = e.message || 'Import failed';
        } finally {
            this.ccImporting = false;
        }
    },

        async openDiscoverModal() {
        this.discoveryError = '';
        const dialog = document.getElementById('openrouter_discover_modal');
        if (dialog && typeof dialog.showModal === 'function') {
            dialog.showModal();
        }
        if (!this.discoveredModels || this.discoveredModels.length === 0) {
            await this.fetchDiscoveredModels(false);
        }
    },

    closeDiscoverModal() {
        const dialog = document.getElementById('openrouter_discover_modal');
        if (dialog && typeof dialog.close === 'function') {
            dialog.close();
        }
    },

    async fetchDiscoveredModels(forceFresh = false) {
        this.discoveryLoading = true;
        this.discoveryError = '';
        const password = Alpine.store('global').webuiPassword;

        try {
            if (!forceFresh) {
                const { response: cachedResp } = await window.utils.request('/api/openrouter/models/cached', {}, password);
                if (cachedResp.ok) {
                    const cachedData = await cachedResp.json();
                    if (cachedData.models && cachedData.models.length > 0) {
                        this.discoveredModels = cachedData.models;
                        this.discoveryLoading = false;
                        return;
                    }
                }
            }

            const payload = {
                baseUrl: this.openRouterConfig.baseUrl
            };
            if (this.openRouterConfig.apiKey) {
                payload.apiKey = this.openRouterConfig.apiKey;
            }

            const { response, newPassword } = await window.utils.request('/api/openrouter/models/fetch', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(payload)
            }, password);
            if (newPassword) Alpine.store('global').webuiPassword = newPassword;

            if (!response.ok) {
                const errData = await response.json().catch(() => ({}));
                throw new Error(errData.error || `HTTP ${response.status}`);
            }

            const data = await response.json();
            this.discoveredModels = data.models || [];
        } catch (e) {
            this.discoveryError = e.message || 'Failed to fetch models from OpenRouter';
        } finally {
            this.discoveryLoading = false;
        }
    },

    get filteredDiscoveredModels() {
        let list = this.discoveredModels || [];
        if (this.discoveryProvider !== 'all') {
            const prov = this.discoveryProvider.toLowerCase();
            list = list.filter(m => m.id.toLowerCase().startsWith(prov + '/'));
        }
        if (this.discoverySearch && this.discoverySearch.trim()) {
            const q = this.discoverySearch.trim().toLowerCase();
            list = list.filter(m =>
                (m.id && m.id.toLowerCase().includes(q)) ||
                (m.name && m.name.toLowerCase().includes(q)) ||
                (m.description && m.description.toLowerCase().includes(q))
            );
        }
        return list;
    },

    isAllowlisted(modelId) {
        return (this.openRouterConfig.allowlist || []).some(m => m.id === modelId);
    },

    formatDiscoverTokens(value) {
        if (!value) return '-';
        if (value < 1000) return String(value);
        return Math.round(value / 1000) + 'k';
    },

    async addToAllowlist(discoveredModel, alias = '') {
        if (this.isAllowlisted(discoveredModel.id)) return;
        const newItem = {
            id: discoveredModel.id,
            alias: (alias || '').trim(),
            displayName: discoveredModel.name || discoveredModel.id,
            contextLength: discoveredModel.context_length || 0,
            // Pre-fill from the catalog's advertised completion cap when
            // known; 0 = auto, the proxy derives max_tokens at request time.
            maxOutputTokens: (discoveredModel.top_provider && discoveredModel.top_provider.max_completion_tokens) || 0,
            enabled: true
        };
        this.openRouterConfig.allowlist.push(newItem);
        await this.saveOpenRouterConfig();
    },

    async removeFromAllowlist(modelId) {
        const store = Alpine.store('global');
        if (!confirm(store.t('confirmRemoveAllowlist') || `Remove ${modelId} from allowlist?`)) return;
        this.openRouterConfig.allowlist = (this.openRouterConfig.allowlist || []).filter(m => m.id !== modelId);
        await this.saveOpenRouterConfig();
    },

    async toggleAllowlistModel(modelId) {
        const item = (this.openRouterConfig.allowlist || []).find(m => m.id === modelId);
        if (item) {
            item.enabled = !item.enabled;
            await this.saveOpenRouterConfig();
        }
    },

    async updateAllowlistAlias(modelId, alias) {
        const item = (this.openRouterConfig.allowlist || []).find(m => m.id === modelId);
        if (item) {
            item.alias = (alias || '').trim();
            await this.saveOpenRouterConfig();
        }
    },

    // Coerce a limits input to a non-negative integer. Empty/invalid becomes 0,
    // which the config omits and the proxy treats as "auto: derive from the
    // model's own limits".
    asInt(value) {
        const n = parseInt(value, 10);
        return Number.isFinite(n) && n > 0 ? n : 0;
    },

    async updateAllowlistContextLength(modelId, value) {
        const item = (this.openRouterConfig.allowlist || []).find(m => m.id === modelId);
        if (item) {
            item.contextLength = this.asInt(value);
            this.saveOpenRouterConfigDebounced();
        }
    },

    async updateAllowlistMaxOutputTokens(modelId, value) {
        const item = (this.openRouterConfig.allowlist || []).find(m => m.id === modelId);
        if (item) {
            item.maxOutputTokens = this.asInt(value);
            this.saveOpenRouterConfigDebounced();
        }
    },

    updateKimiMaxOutput(idx, value) {
        const item = (this.kimiConfig.allowlist || [])[idx];
        if (item) {
            item.maxOutputTokens = this.asInt(value);
            this.saveKimiConfigDebounced();
        }
    },

    /**
     * Update model configuration (delegates to shared utility)
     * @param {string} modelId - The model ID to update
     * @param {object} configUpdates - Configuration updates (pinned, hidden)
     */
    async updateModelConfig(modelId, configUpdates) {
        return window.ModelConfigUtils.updateModelConfig(modelId, configUpdates);
    },

    // Custom Endpoints state and methods
    endpointModal: {
        show: false,
        isEdit: false,
        modelId: '',
        url: '',
        apiKey: '',
        hasApiKey: false,
        saving: false,
        error: ''
    },

    openAddEndpointModal() {
        this.endpointModal = {
            show: true,
            isEdit: false,
            modelId: '',
            url: '',
            apiKey: '',
            hasApiKey: false,
            saving: false,
            error: ''
        };
        const dialog = document.getElementById('custom_endpoint_modal');
        if (dialog && typeof dialog.showModal === 'function') {
            dialog.showModal();
        }
    },

    openEditEndpointModal(modelId, ep) {
        this.endpointModal = {
            show: true,
            isEdit: true,
            modelId: modelId,
            url: ep?.url || '',
            apiKey: '',
            hasApiKey: !!ep?.hasApiKey,
            saving: false,
            error: ''
        };
        const dialog = document.getElementById('custom_endpoint_modal');
        if (dialog && typeof dialog.showModal === 'function') {
            dialog.showModal();
        }
    },

    closeEndpointModal() {
        this.endpointModal.show = false;
        const dialog = document.getElementById('custom_endpoint_modal');
        if (dialog && typeof dialog.close === 'function') {
            dialog.close();
        }
    },

    async saveEndpoint() {
        const { modelId, url, apiKey, hasApiKey, isEdit } = this.endpointModal;
        const store = Alpine.store('global');
        const dataStore = Alpine.store('data');

        const cleanModelId = (modelId || '').trim();
        const cleanUrl = (url || '').trim();

        if (!cleanModelId) {
            this.endpointModal.error = store.t('modelIdRequired') || 'Model ID is required';
            return;
        }
        if (!cleanUrl) {
            this.endpointModal.error = store.t('urlRequired') || 'Endpoint URL is required';
            return;
        }

        this.endpointModal.saving = true;
        this.endpointModal.error = '';

        try {
            const currentEndpoints = { ...(dataStore.customEndpoints || {}) };
            const epData = {
                url: cleanUrl
            };
            if (apiKey && apiKey.trim()) {
                epData.apiKey = apiKey.trim();
            } else if (hasApiKey && isEdit) {
                epData.hasApiKey = true;
                epData.apiKey = '';
            }

            currentEndpoints[cleanModelId] = epData;

            await window.ModelConfigUtils.saveCustomEndpoints(currentEndpoints);
            store.showToast(store.t('endpointSavedSuccess') || `Endpoint for ${cleanModelId} saved`, 'success');
            this.closeEndpointModal();
        } catch (e) {
            this.endpointModal.error = e.message || 'Failed to save endpoint';
        } finally {
            this.endpointModal.saving = false;
        }
    },

    async deleteEndpoint(modelId) {
        const store = Alpine.store('global');
        const dataStore = Alpine.store('data');

        if (!confirm(store.t('confirmDeleteEndpoint') || `Are you sure you want to remove the custom endpoint for ${modelId}?`)) {
            return;
        }

        try {
            const currentEndpoints = { ...(dataStore.customEndpoints || {}) };
            delete currentEndpoints[modelId];

            await window.ModelConfigUtils.saveCustomEndpoints(currentEndpoints);
            store.showToast(store.t('endpointDeletedSuccess') || `Endpoint for ${modelId} removed`, 'success');
        } catch (e) {
            store.showToast((store.t('failedToDeleteEndpoint') || 'Failed to delete endpoint') + ': ' + e.message, 'error');
        }
    },

    // Model Alias state and methods
    aliasModal: {
        show: false,
        sourceModel: '',
        targetModel: '',
        saving: false,
        error: ''
    },

    openAddAliasModal() {
        this.aliasModal = {
            show: true,
            sourceModel: '',
            targetModel: '',
            saving: false,
            error: ''
        };
        const dialog = document.getElementById('add_model_alias_modal');
        if (dialog && typeof dialog.showModal === 'function') {
            dialog.showModal();
        }
    },

    closeAliasModal() {
        this.aliasModal.show = false;
        const dialog = document.getElementById('add_model_alias_modal');
        if (dialog && typeof dialog.close === 'function') {
            dialog.close();
        }
    },

    async saveAlias() {
        const { sourceModel, targetModel } = this.aliasModal;
        const store = Alpine.store('global');

        const cleanSource = (sourceModel || '').trim();
        const cleanTarget = (targetModel || '').trim();

        if (!cleanSource) {
            this.aliasModal.error = store.t('sourceModelRequired') || 'Source Model ID is required';
            return;
        }
        if (!cleanTarget) {
            this.aliasModal.error = store.t('targetModelRequired') || 'Target Model ID is required';
            return;
        }

        this.aliasModal.saving = true;
        this.aliasModal.error = '';

        try {
            await window.ModelConfigUtils.updateModelConfig(cleanSource, { mapping: cleanTarget });
            store.showToast(store.t('aliasSavedSuccess') || `Alias ${cleanSource} -> ${cleanTarget} saved`, 'success');
            this.closeAliasModal();
        } catch (e) {
            this.aliasModal.error = e.message || 'Failed to save alias';
        } finally {
            this.aliasModal.saving = false;
        }
    },

    async deleteModelAlias(modelId) {
        const store = Alpine.store('global');

        if (!confirm(store.t('confirmDeleteAlias') || `Are you sure you want to remove model alias ${modelId}?`)) {
            return;
        }

        try {
            await window.ModelConfigUtils.deleteModelConfig(modelId);
            store.showToast(store.t('aliasDeletedSuccess') || `Alias ${modelId} removed`, 'success');
        } catch (e) {
            store.showToast((store.t('failedToDeleteModelAlias') || 'Failed to delete model alias') + ': ' + e.message, 'error');
        }
    },

    /**
     * Get list of all models (discovered + custom endpoints + openrouter allowlist + custom alias keys)
     */
    get allConfiguredModels() {
        const dataStore = Alpine.store('data');
        const modelSet = new Set(dataStore.models || []);

        if (dataStore.customEndpoints) {
            Object.keys(dataStore.customEndpoints).forEach(m => modelSet.add(m));
        }

        if (dataStore.openrouter && dataStore.openrouter.allowlist) {
            dataStore.openrouter.allowlist.forEach(m => {
                if (m.enabled !== false) {
                    if (m.id) modelSet.add(m.id);
                    if (m.alias) modelSet.add(m.alias);
                }
            });
        }

        if (dataStore.kimi && dataStore.kimi.allowlist) {
            dataStore.kimi.allowlist.forEach(m => {
                if (m.enabled !== false) {
                    if (m.id) modelSet.add(m.id);
                    if (m.alias) modelSet.add(m.alias);
                }
            });
        }

        if (dataStore.modelConfig) {
            Object.keys(dataStore.modelConfig).forEach(m => modelSet.add(m));
        }

        return Array.from(modelSet).sort();
    },

    /**
     * Get list of custom endpoints as an array
     */
    get customEndpointsList() {
        const dataStore = Alpine.store('data');
        const endpoints = dataStore.customEndpoints || {};
        return Object.keys(endpoints).map(modelId => ({
            modelId,
            ...endpoints[modelId]
        }));
    },

    /**
     * Check if a model is a purely custom alias (not in discovered cloudcode models, custom endpoints, or openrouter allowlist)
     */
    isCustomAlias(modelId) {
        const dataStore = Alpine.store('data');
        const discovered = dataStore.models || [];
        if (discovered.includes(modelId)) return false;
        if (dataStore.customEndpoints && dataStore.customEndpoints[modelId]) return false;
        if (dataStore.openrouter && dataStore.openrouter.allowlist && dataStore.openrouter.allowlist.some(m => m.enabled !== false && (m.id === modelId || m.alias === modelId))) return false;
        return true;
    }
});
