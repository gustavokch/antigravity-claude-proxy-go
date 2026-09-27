/**
 * Account Manager Component
 * Registers itself to window.Components for Alpine.js to consume
 */
window.Components = window.Components || {};

/**
 * Pure helpers for quota pools and Claude usage windows. They take plain
 * values, touch no DOM or store, and tolerate missing or null fields, so the
 * Node harness in tests/ can exercise them directly.
 */
window.QuotaView = (() => {
    const FAMILY_RANK = { gemini: 0, claude: 1, '3p': 2 };
    const FAMILY_LABEL = { gemini: 'Gemini', claude: 'Claude', '3p': '3P' };
    const WINDOW_RANK = { '5h': 0, weekly: 1, '7d': 1 };

    const isNum = (v) => typeof v === 'number' && Number.isFinite(v);

    // "gemini-5h" -> { family: "gemini", window: "5h" }. An id without a
    // dash is treated as a family with an unknown window.
    function splitPoolId(id) {
        const s = String(id || '');
        const i = s.lastIndexOf('-');
        if (i <= 0) return { family: s, window: '' };
        return { family: s.slice(0, i), window: s.slice(i + 1) };
    }

    // Remaining fraction as a whole percent clamped to 0-100, or null when
    // the value is unknown.
    function poolPercent(pool) {
        if (!pool || !isNum(pool.remainingFraction)) return null;
        return Math.max(0, Math.min(100, Math.round(pool.remainingFraction * 100)));
    }

    // Pools as an ordered array: grouped by family (Gemini, Claude, 3P, then
    // the rest alphabetically), with the 5h window before the weekly one.
    function sortedPools(pools) {
        if (!pools || typeof pools !== 'object') return [];
        const rows = Object.entries(pools)
            .filter(([, p]) => p && typeof p === 'object')
            .map(([id, p]) => {
                const { family, window } = splitPoolId(id);
                return {
                    id,
                    family,
                    window,
                    remainingFraction: isNum(p.remainingFraction) ? p.remainingFraction : null,
                    resetTime: p.resetTime || null,
                    source: p.source || null,
                    percent: poolPercent(p),
                };
            });
        const famRank = (f) => (f in FAMILY_RANK ? FAMILY_RANK[f] : 10);
        const winRank = (w) => (w in WINDOW_RANK ? WINDOW_RANK[w] : 5);
        rows.sort((a, b) =>
            famRank(a.family) - famRank(b.family) ||
            a.family.localeCompare(b.family) ||
            winRank(a.window) - winRank(b.window) ||
            a.window.localeCompare(b.window));
        return rows;
    }

    function poolFamilyLabel(family) {
        return FAMILY_LABEL[family] || family;
    }

    // Bar colour for a remaining percent (null means unknown).
    function remainingBarClass(percent) {
        if (percent === null || percent === undefined) return 'bg-gray-600';
        if (percent > 50) return 'bg-emerald-500';
        if (percent > 20) return 'bg-yellow-500';
        return 'bg-red-500';
    }

    // Burn level from tokensPerMinuteForIndicator: above 1000 HIGH, above
    // 500 MODERATE, otherwise NORMAL. Null when the rate is unknown.
    function burnLevel(burnRate) {
        const tpm = burnRate && burnRate.tokensPerMinuteForIndicator;
        if (!isNum(tpm)) return null;
        if (tpm > 1000) return 'HIGH';
        if (tpm > 500) return 'MODERATE';
        return 'NORMAL';
    }

    // Level for a projected utilization fraction: above 1.0 exceeds, above
    // 0.8 warning. Falls back to the server's status when the projection is
    // missing.
    function projectedLevel(win) {
        if (!win) return null;
        const p = win.projectedUtilization;
        if (isNum(p)) {
            if (p > 1) return 'exceeds';
            if (p > 0.8) return 'warning';
            return 'ok';
        }
        return ['ok', 'warning', 'exceeds'].includes(win.status) ? win.status : null;
    }

    // Used fraction as a whole percent (not clamped, so 1.32 reads as 132%).
    function usedPercent(fraction) {
        return isNum(fraction) ? Math.round(fraction * 100) : null;
    }

    // Position for a bar fill or marker, clamped to 0-100.
    function barPosition(fraction) {
        return isNum(fraction) ? Math.max(0, Math.min(100, fraction * 100)) : 0;
    }

    function formatUSD(v) {
        if (!isNum(v)) return '-';
        return '$' + v.toFixed(2);
    }

    function formatTokens(v) {
        if (!isNum(v)) return '-';
        const abs = Math.abs(v);
        if (abs >= 1e9) return (v / 1e9).toFixed(2) + 'B';
        if (abs >= 1e6) return (v / 1e6).toFixed(2) + 'M';
        if (abs >= 1e3) return (v / 1e3).toFixed(1) + 'K';
        return String(Math.round(v));
    }

    // Rows of a ccusage-shaped report ({daily:[…]}, {weekly:[…]}, …).
    function reportRows(data, report) {
        const rows = data && data[report];
        return Array.isArray(rows) ? rows : [];
    }

    // The period label of a report row: its date, week or month.
    function reportPeriod(row) {
        if (!row) return '';
        return row.date || row.week || row.month || '';
    }

    function usageURL(accountId, report) {
        let url = '/api/claudecode/usage?report=' + encodeURIComponent(report);
        if (accountId) url += '&account=' + encodeURIComponent(accountId);
        return url;
    }

    function escapeHTML(s) {
        return String(s ?? '').replace(/[&<>"']/g, (c) => ({
            '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
        })[c]);
    }

    const SOURCE_KEYS = {
        headers: 'poolSourceHeaders', calibrated: 'poolSourceCalibrated',
        config: 'poolSourceConfig', max: 'poolSourceMax',
    };

    // "Gemini · 5-hour" style label; t translates the window names.
    function poolLabel(pool, t) {
        const fam = poolFamilyLabel(pool.family);
        let win = pool.window;
        if (win === '5h') win = t('poolWindow5h');
        else if (win === 'weekly' || win === '7d') win = t('poolWindowWeekly');
        return win ? `${fam} · ${win}` : fam;
    }

    function poolSourceLabel(source, t) {
        return SOURCE_KEYS[source] ? t(SOURCE_KEYS[source]) : (source || '');
    }

    // The shared pool-bar renderer used by the Google and Claude account rows
    // and by the quota modal. pools is the raw quota.pools object; t is the
    // translator and timeUntil formats a reset time. Returns escaped HTML, or
    // '' when there are no pools. compact selects the table-cell layout.
    function renderPoolBars(pools, { t = (k) => k, timeUntil = () => '', compact = false } = {}) {
        const rows = sortedPools(pools);
        if (rows.length === 0) return '';
        const items = rows.map((p) => {
            const pct = p.percent === null ? 'N/A' : p.percent + '%';
            const reset = p.resetTime
                ? (compact ? timeUntil(p.resetTime) : t('resetsIn', { time: timeUntil(p.resetTime) }))
                : '';
            const badge = p.source
                ? `<span class="px-1 rounded border border-space-border text-gray-400 uppercase" data-pool-source="${escapeHTML(p.source)}">${escapeHTML(poolSourceLabel(p.source, t))}</span>`
                : '';
            const pctClass = p.percent === null ? 'text-gray-500' : 'text-gray-300';
            if (compact) {
                return `<div class="leading-tight" data-pool-id="${escapeHTML(p.id)}">`
                    + `<div class="flex items-center justify-between gap-1 text-[10px] font-mono text-gray-500">`
                    + `<span class="truncate">${escapeHTML(poolLabel(p, t))}</span>`
                    + `<span class="${pctClass}">${escapeHTML(pct)}</span></div>`
                    + `<div class="w-full bg-gray-700 rounded-full overflow-hidden" style="height:4px">`
                    + `<div class="h-full rounded-full ${remainingBarClass(p.percent)}" style="width:${p.percent ?? 0}%"></div></div>`
                    + ((reset || badge)
                        ? `<div class="flex items-center justify-between gap-1 text-[10px] font-mono text-gray-500" style="font-size:9px">`
                          + `<span class="text-yellow-500/80">${escapeHTML(reset)}</span>${badge}</div>`
                        : '')
                    + `</div>`;
            }
            return `<div class="p-3 bg-space-800/50 border border-space-border/30 rounded-lg" data-pool-id="${escapeHTML(p.id)}">`
                + `<div class="flex justify-between items-center mb-2">`
                + `<span class="text-sm font-semibold text-gray-200">${escapeHTML(poolLabel(p, t))}</span>`
                + `<div class="flex items-center gap-2 text-[10px] font-mono">${badge}`
                + `<span class="text-xs font-mono ${pctClass}">${escapeHTML(pct)}</span></div></div>`
                + `<div class="w-full bg-gray-700 rounded-full h-2.5 mb-2 overflow-hidden">`
                + `<div class="h-full rounded-full transition-all duration-500 ${remainingBarClass(p.percent)}" style="width:${p.percent ?? 0}%"></div></div>`
                + `<div class="flex justify-between items-center">`
                + `<span class="text-[10px] text-gray-500 font-mono">${escapeHTML(p.id)}</span>`
                + (reset ? `<span class="text-[10px] text-yellow-500/80 font-mono italic">${escapeHTML(reset)}</span>` : '')
                + `</div></div>`;
        });
        return `<div class="${compact ? 'mt-1.5 space-y-1' : 'grid grid-cols-1 gap-3'}" data-pool-bars>${items.join('')}</div>`;
    }

    return {
        splitPoolId, poolPercent, sortedPools, poolFamilyLabel, remainingBarClass,
        burnLevel, projectedLevel, usedPercent, barPosition, formatUSD, formatTokens,
        reportRows, reportPeriod, usageURL, escapeHTML, poolLabel, poolSourceLabel,
        renderPoolBars,
    };
})();

window.Components.accountManager = () => ({
    accountTab: 'google', // 'google' | 'claudecode'
    searchQuery: '',
    deleteTarget: '',
    refreshing: false,
    toggling: false,
    deleting: false,
    reloading: false,
    selectedAccountEmail: '',
    selectedAccountName: '',
    selectedAccountLimits: {},
    selectedAccountId: '',
    selectedAccountProvider: '',
    selectedAccountPools: null,
    selectedAccountUsage: null,

    // Claude Code usage history per account id:
    // { open, report: 'daily'|'weekly', loading, error, data: { daily, weekly } }
    ccHistory: {},

    // Claude Code Accounts & Gateway State
    ccAccounts: [],
    ccConfig: {
        enabled: false,
        baseUrl: 'https://api.anthropic.com',
        mode: 'pool',
        autoImport: false,
        accounts: []
    },
    ccLoading: false,
    ccSaving: false,
    ccImporting: false,
    ccError: '',
    ccSuccess: '',
    ccNewToken: '',
    ccNewName: '',
    ccTesting: {},

    // Health Inspector (Developer Mode)
    healthData: {},
    healthLoading: false,

    init() {
        if (Alpine.store('data').devMode && Alpine.store('settings').healthInspectorOpen) {
            this.fetchHealthData();
        }
        this.loadCCAccounts();
        this.loadCCConfig();
    },

    get googleAccounts() {
        return (Alpine.store('data').accounts || []).filter(acc => (acc.provider || 'google') === 'google');
    },

    get claudeStoreAccounts() {
        return (Alpine.store('data').accounts || []).filter(acc => acc.provider === 'claudecode');
    },

    get filteredAccounts() {
        const accounts = this.googleAccounts;
        if (!this.searchQuery || this.searchQuery.trim() === '') {
            return accounts;
        }

        const query = this.searchQuery.toLowerCase().trim();
        return accounts.filter(acc => {
            return (acc.email || '').toLowerCase().includes(query) ||
                   (acc.projectId && acc.projectId.toLowerCase().includes(query)) ||
                   (acc.source && acc.source.toLowerCase().includes(query));
        });
    },

    get filteredCCAccounts() {
        const storeAccounts = this.claudeStoreAccounts;
        const byId = new Map();
        (this.ccAccounts || []).forEach(acc => {
            if (acc && acc.id) byId.set(acc.id, acc);
        });
        storeAccounts.forEach(acc => {
            const id = acc.id || acc.email;
            if (id && !byId.has(id)) byId.set(id, acc);
        });
        const merged = [];
        byId.forEach((snapshot, id) => {
            const storeAcc = storeAccounts.find(a => (a.id || a.email) === id);
            merged.push(storeAcc ? { ...snapshot, ...storeAcc } : snapshot);
        });
        if (!this.searchQuery || this.searchQuery.trim() === '') {
            return merged;
        }

        const query = this.searchQuery.toLowerCase().trim();
        return merged.filter(acc => {
            return (acc.name && acc.name.toLowerCase().includes(query)) ||
                   (acc.id && acc.id.toLowerCase().includes(query)) ||
                   (acc.type && acc.type.toLowerCase().includes(query));
        });
    },

    formatEmail(email) {
        if (!email || email.length <= 40) return email;

        const [user, domain] = email.split('@');
        if (!domain) return email;

        // Preserve domain integrity, truncate username if needed
        if (user.length > 20) {
            return `${user.substring(0, 10)}...${user.slice(-5)}@${domain}`;
        }
        return email;
    },

    async refreshAccount(email) {
        return await window.ErrorHandler.withLoading(async () => {
            const store = Alpine.store('global');
            store.showToast(store.t('refreshingAccount', { email: Redact.email(email) }), 'info');

            const { response, newPassword } = await window.utils.request(
                `/api/accounts/${encodeURIComponent(email)}/refresh`,
                { method: 'POST' },
                store.webuiPassword
            );
            if (newPassword) store.webuiPassword = newPassword;

            const data = await response.json();
            if (data.status === 'ok') {
                store.showToast(store.t('refreshedAccount', { email: Redact.email(email) }), 'success');
                Alpine.store('data').fetchData();
            } else {
                throw new Error(data.error || store.t('refreshFailed'));
            }
        }, this, 'refreshing', { errorMessage: 'Failed to refresh account' });
    },

    async toggleAccount(email, enabled) {
        const store = Alpine.store('global');
        const password = store.webuiPassword;

        // Optimistic update: immediately update UI
        const dataStore = Alpine.store('data');
        const account = dataStore.accounts.find(a => a.email === email);
        if (account) {
            account.enabled = enabled;
        }

        try {
            const { response, newPassword } = await window.utils.request(`/api/accounts/${encodeURIComponent(email)}/toggle`, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ enabled })
            }, password);
            if (newPassword) store.webuiPassword = newPassword;

            const data = await response.json();
            if (data.status === 'ok') {
                const status = enabled ? store.t('enabledStatus') : store.t('disabledStatus');
                store.showToast(store.t('accountToggled', { email: Redact.email(email), status }), 'success');
                // Refresh to confirm server state
                await dataStore.fetchData();
            } else {
                store.showToast(data.error || store.t('toggleFailed'), 'error');
                // Rollback optimistic update on error
                if (account) {
                    account.enabled = !enabled;
                }
                await dataStore.fetchData();
            }
        } catch (e) {
            store.showToast(store.t('toggleFailed') + ': ' + e.message, 'error');
            // Rollback optimistic update on error
            if (account) {
                account.enabled = !enabled;
            }
            await dataStore.fetchData();
        }
    },

    async fixAccount(email) {
        const store = Alpine.store('global');
        const dataStore = Alpine.store('data');
        // If the account has a verification URL (403 VALIDATION_REQUIRED), open it directly
        const account = (dataStore.accounts || []).find(a => a.email === email);
        if (account?.verifyUrl) {
            window.open(account.verifyUrl, '_blank');
            store.showToast(store.t('verifyThenRefresh') || 'After completing verification, click the ↻ Refresh button to re-enable this account', 'info', 10000);
            return;
        }
        // Otherwise fall back to OAuth re-auth
        store.showToast(store.t('reauthenticating', { email: Redact.email(email) }), 'info');
        const password = store.webuiPassword;
        try {
            const urlPath = `/api/auth/url?email=${encodeURIComponent(email)}`;
            const { response, newPassword } = await window.utils.request(urlPath, {}, password);
            if (newPassword) store.webuiPassword = newPassword;

            const data = await response.json();
            if (data.status === 'ok') {
                window.open(data.url, 'google_oauth', 'width=600,height=700,scrollbars=yes');
            } else {
                store.showToast(data.error || store.t('authUrlFailed'), 'error');
            }
        } catch (e) {
            store.showToast(store.t('authUrlFailed') + ': ' + e.message, 'error');
        }
    },

    confirmDeleteAccount(email) {
        this.deleteTarget = email;
        document.getElementById('delete_account_modal').showModal();
    },

    async executeDelete() {
        const email = this.deleteTarget;
        return await window.ErrorHandler.withLoading(async () => {
            const store = Alpine.store('global');

            const { response, newPassword } = await window.utils.request(
                `/api/accounts/${encodeURIComponent(email)}`,
                { method: 'DELETE' },
                store.webuiPassword
            );
            if (newPassword) store.webuiPassword = newPassword;

            const data = await response.json();
            if (data.status === 'ok') {
                store.showToast(store.t('deletedAccount', { email: Redact.email(email) }), 'success');
                Alpine.store('data').fetchData();
                document.getElementById('delete_account_modal').close();
                this.deleteTarget = '';
            } else {
                throw new Error(data.error || store.t('deleteFailed'));
            }
        }, this, 'deleting', { errorMessage: 'Failed to delete account' });
    },

    async reloadAccounts() {
        return await window.ErrorHandler.withLoading(async () => {
            const store = Alpine.store('global');

            const { response, newPassword } = await window.utils.request(
                '/api/accounts/reload',
                { method: 'POST' },
                store.webuiPassword
            );
            if (newPassword) store.webuiPassword = newPassword;

            const data = await response.json();
            if (data.status === 'ok') {
                store.showToast(store.t('accountsReloaded'), 'success');
                Alpine.store('data').fetchData();
            } else {
                throw new Error(data.error || store.t('reloadFailed'));
            }
        }, this, 'reloading', { errorMessage: 'Failed to reload accounts' });
    },

    openQuotaModal(account) {
        this.selectedAccountEmail = account.email || account.id;
        // Email-like "names" (backend sends email as name for google rows and
        // for CC rows without a display name) bypass Redact when rendered
        // verbatim; treat them as no name so the modal shows only the
        // redacted email.
        const nameLooksEmail = (account.name || '').includes('@');
        this.selectedAccountName = nameLooksEmail ? '' : (account.name || '');
        this.selectedAccountLimits = Object.fromEntries(
            Object.entries(account.limits || {}).filter(([, v]) => v != null)
        );
        this.selectedAccountId = account.id || account.email || '';
        this.selectedAccountProvider = account.provider || 'google';
        this.selectedAccountPools = (account.quota && account.quota.pools) || null;
        this.selectedAccountUsage = this.accountUsage(account);
        document.getElementById('quota_modal').showModal();
    },

    // ==========================================
    // Quota pools and Claude usage windows
    // ==========================================

    get qv() {
        return window.QuotaView;
    },

    accountPools(acc) {
        return window.QuotaView.sortedPools(acc && acc.quota && acc.quota.pools);
    },

    // The usage block when it has at least one window, else null.
    accountUsage(acc) {
        const u = acc && acc.usage;
        if (!u || typeof u !== 'object') return null;
        return (u.window5h || u.window7d) ? u : null;
    },

    // Shared pool-bar HTML for a raw quota.pools object (see QuotaView.renderPoolBars).
    renderPools(pools, compact = false) {
        const store = Alpine.store('global');
        return window.QuotaView.renderPoolBars(pools, {
            t: (k, p) => store.t(k, p),
            timeUntil: (ts) => window.utils.formatTimeUntil(ts),
            compact,
        });
    },

    poolSourceLabel(source) {
        const store = Alpine.store('global');
        return window.QuotaView.poolSourceLabel(source, (k) => store.t(k));
    },

    burnLabel(level) {
        const keys = { HIGH: 'ccBurnHigh', MODERATE: 'ccBurnModerate', NORMAL: 'ccBurnNormal' };
        return keys[level] ? Alpine.store('global').t(keys[level]) : '';
    },

    // Colour classes for an ok/warning/exceeds level.
    levelBg(level) {
        return level === 'exceeds' ? 'bg-red-500' : (level === 'warning' ? 'bg-yellow-500' : 'bg-emerald-500');
    },

    levelText(level) {
        return level === 'exceeds' ? 'text-red-400' : (level === 'warning' ? 'text-yellow-400' : 'text-gray-300');
    },

    // Level of the current utilization, on the same thresholds as the projection.
    nowLevel(win) {
        return window.QuotaView.projectedLevel({ projectedUtilization: win && win.utilization });
    },

    usageWindows(usage) {
        if (!usage) return [];
        const out = [];
        if (usage.window5h) out.push({ key: '5h', label: 'ccWindow5h', win: usage.window5h });
        if (usage.window7d) out.push({ key: '7d', label: 'ccWindow7d', win: usage.window7d });
        return out;
    },

    windowTimeLeft(win) {
        if (!win || !win.end) return '-';
        return window.utils.formatTimeUntil(win.end);
    },

    historyState(accountId) {
        return this.ccHistory[accountId] ||
            { open: false, report: 'daily', loading: false, error: '', data: {} };
    },

    historyRows(accountId) {
        const st = this.historyState(accountId);
        return window.QuotaView.reportRows(st.data[st.report], st.report);
    },

    historyTotals(accountId) {
        const st = this.historyState(accountId);
        const d = st.data[st.report];
        return (d && d.totals) || null;
    },

    toggleHistory(accountId) {
        const st = { ...this.historyState(accountId) };
        st.open = !st.open;
        this.ccHistory = { ...this.ccHistory, [accountId]: st };
        if (st.open) this.loadHistory(accountId, st.report);
    },

    setHistoryReport(accountId, report) {
        const st = { ...this.historyState(accountId), report };
        this.ccHistory = { ...this.ccHistory, [accountId]: st };
        this.loadHistory(accountId, report);
    },

    // Loads a report once per account and report kind. Placeholder accounts
    // get generated rows so the table can be seen without live data.
    async loadHistory(accountId, report, force = false) {
        const cur = this.historyState(accountId);
        if (!force && (cur.data[report] || cur.loading)) return;
        const patch = (fields) => {
            const st = this.historyState(accountId);
            this.ccHistory = { ...this.ccHistory, [accountId]: { ...st, ...fields } };
        };
        patch({ loading: true, error: '' });

        const dataStore = Alpine.store('data');
        if (dataStore.placeholderMode && String(accountId).startsWith('cc-placeholder-') &&
            typeof dataStore.placeholderUsageReport === 'function') {
            const st = this.historyState(accountId);
            patch({ loading: false, data: { ...st.data, [report]: dataStore.placeholderUsageReport(accountId, report) } });
            return;
        }

        const store = Alpine.store('global');
        try {
            const { response, newPassword } = await window.utils.request(
                window.QuotaView.usageURL(accountId, report), {}, store.webuiPassword);
            if (newPassword) store.webuiPassword = newPassword;
            if (!response.ok) {
                const err = await response.json().catch(() => ({}));
                throw new Error(err.error || `HTTP ${response.status}`);
            }
            const data = await response.json();
            const st = this.historyState(accountId);
            patch({ loading: false, data: { ...st.data, [report]: data || {} } });
        } catch (e) {
            patch({ loading: false, error: (store.t('ccHistoryLoadFailed')) + ': ' + (e.message || e) });
        }
    },

    // Threshold settings
    thresholdDialog: {
        email: '',
        quotaThreshold: null,  // null means use global
        modelQuotaThresholds: {},
        saving: false,
        addingModel: false,
        newModelId: '',
        newModelThreshold: 10
    },

    openThresholdModal(account) {
        this.thresholdDialog = {
            email: account.email,
            // Convert from fraction (0-1) to percentage (0-99) for display
            quotaThreshold: account.quotaThreshold !== undefined ? Math.round(account.quotaThreshold * 100) : null,
            modelQuotaThresholds: Object.fromEntries(
                Object.entries(account.modelQuotaThresholds || {}).map(([k, v]) => [k, Math.round(v * 100)])
            ),
            saving: false,
            addingModel: false,
            newModelId: '',
            newModelThreshold: 10
        };
        document.getElementById('threshold_modal').showModal();
    },

    async saveAccountThreshold() {
        const store = Alpine.store('global');
        this.thresholdDialog.saving = true;

        try {
            // Convert percentage back to fraction
            const quotaThreshold = this.thresholdDialog.quotaThreshold !== null && this.thresholdDialog.quotaThreshold !== ''
                ? parseFloat(this.thresholdDialog.quotaThreshold) / 100
                : null;

            // Convert model thresholds from percentage to fraction
            const modelQuotaThresholds = {};
            for (const [modelId, pct] of Object.entries(this.thresholdDialog.modelQuotaThresholds)) {
                modelQuotaThresholds[modelId] = parseFloat(pct) / 100;
            }

            const { response, newPassword } = await window.utils.request(
                `/api/accounts/${encodeURIComponent(this.thresholdDialog.email)}`,
                {
                    method: 'PATCH',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ quotaThreshold, modelQuotaThresholds })
                },
                store.webuiPassword
            );
            if (newPassword) store.webuiPassword = newPassword;

            const data = await response.json();
            if (data.status === 'ok') {
                store.showToast('Settings saved', 'success');
                Alpine.store('data').fetchData();
                document.getElementById('threshold_modal').close();
            } else {
                throw new Error(data.error || 'Failed to save settings');
            }
        } catch (e) {
            store.showToast('Failed to save settings: ' + e.message, 'error');
        } finally {
            this.thresholdDialog.saving = false;
        }
    },

    clearAccountThreshold() {
        this.thresholdDialog.quotaThreshold = null;
    },

    // Per-model threshold methods
    addModelThreshold() {
        this.thresholdDialog.addingModel = true;
        this.thresholdDialog.newModelId = '';
        this.thresholdDialog.newModelThreshold = 10;
    },

    updateModelThreshold(modelId, value) {
        const numValue = parseInt(value);
        if (!isNaN(numValue) && numValue >= 0 && numValue <= 99) {
            this.thresholdDialog.modelQuotaThresholds[modelId] = numValue;
        }
    },

    removeModelThreshold(modelId) {
        delete this.thresholdDialog.modelQuotaThresholds[modelId];
    },

    confirmAddModelThreshold() {
        const modelId = this.thresholdDialog.newModelId;
        const threshold = parseInt(this.thresholdDialog.newModelThreshold) || 10;

        if (modelId && threshold >= 0 && threshold <= 99) {
            this.thresholdDialog.modelQuotaThresholds[modelId] = threshold;
            this.thresholdDialog.addingModel = false;
            this.thresholdDialog.newModelId = '';
            this.thresholdDialog.newModelThreshold = 10;
        }
    },

    getAvailableModelsForThreshold() {
        // Get models from data store, exclude already configured ones
        const allModels = Alpine.store('data').models || [];
        const configured = Object.keys(this.thresholdDialog.modelQuotaThresholds);
        return allModels.filter(m => !configured.includes(m));
    },

    getEffectiveThreshold(account) {
        // Return display string for effective threshold
        if (account.quotaThreshold !== undefined) {
            return Math.round(account.quotaThreshold * 100) + '%';
        }
        // If no per-account threshold, show global value
        const globalThreshold = Alpine.store('data').globalQuotaThreshold;
        if (globalThreshold > 0) {
            return Math.round(globalThreshold * 100) + '% (global)';
        }
        return 'Global';
    },

    /**
     * Get main model quota for display
     * Prioritizes flagship models (Opus > Sonnet > Flash)
     * @param {Object} account - Account object with limits
     * @returns {Object} { percent: number|null, model: string }
     */
    getMainModelQuota(account) {
        const limits = account.limits || {};
        
        const getQuotaVal = (id) => {
             const l = limits[id];
             if (!l) return -1;
             if (l.remainingFraction != null) return l.remainingFraction;
             if (l.resetTime) return 0; // Rate limited, fraction unknown
             return -1; // Unknown
        };

        const validIds = Object.keys(limits).filter(id => getQuotaVal(id) >= 0);
        
        if (validIds.length === 0) return { percent: null, model: '-' };

        const DEAD_THRESHOLD = 0.01;
        
        const MODEL_TIERS = [
            { pattern: /\bopus\b/, aliveScore: 100, deadScore: 60 },
            { pattern: /\bsonnet\b/, aliveScore: 90, deadScore: 55 },
            // Gemini 3 Pro / Ultra
            { pattern: /\bgemini-3\b/, extraCheck: (l) => /\bpro\b/.test(l) || /\bultra\b/.test(l), aliveScore: 80, deadScore: 50 },
            { pattern: /\bpro\b/, aliveScore: 75, deadScore: 45 },
            // Mid/Low Tier
            { pattern: /\bhaiku\b/, aliveScore: 30, deadScore: 15 },
            { pattern: /\bflash\b/, aliveScore: 20, deadScore: 10 }
        ];

        const getPriority = (id) => {
            const lower = id.toLowerCase();
            const val = getQuotaVal(id);
            const isAlive = val > DEAD_THRESHOLD;
            
            for (const tier of MODEL_TIERS) {
                if (tier.pattern.test(lower)) {
                    if (tier.extraCheck && !tier.extraCheck(lower)) continue;
                    return isAlive ? tier.aliveScore : tier.deadScore;
                }
            }
            
            return isAlive ? 5 : 0;
        };

        // Sort by priority desc
        validIds.sort((a, b) => getPriority(b) - getPriority(a));

        const bestModel = validIds[0];
        const val = getQuotaVal(bestModel);
        
        return {
            percent: Math.round(val * 100),
            model: bestModel
        };
    },

    /**
     * Fetch strategy health data for the inspector panel
     */
    async fetchHealthData() {
        this.healthLoading = true;
        try {
            const store = Alpine.store('global');
            const { response, newPassword } = await window.utils.request(
                '/api/strategy/health',
                {},
                store.webuiPassword
            );
            if (newPassword) store.webuiPassword = newPassword;

            const data = await response.json();
            if (data.status === 'ok') {
                this.healthData = data;
            } else {
                this.healthData = {};
                if (response.status === 403) {
                    store.showToast(data.error || 'Developer mode is not enabled', 'warning');
                }
            }
        } catch (e) {
            console.error('Failed to fetch health data:', e);
        } finally {
            this.healthLoading = false;
        }
    },

    /**
     * Export accounts to JSON file
     */
    async exportAccounts() {
        const store = Alpine.store('global');
        try {
            const { response, newPassword } = await window.utils.request(
                '/api/accounts/export',
                {},
                store.webuiPassword
            );
            if (newPassword) store.webuiPassword = newPassword;

            const data = await response.json();
            // API returns plain array directly
            if (Array.isArray(data)) {
                const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
                const url = URL.createObjectURL(blob);
                const a = document.createElement('a');
                a.href = url;
                a.download = `antigravity-accounts-${new Date().toISOString().split('T')[0]}.json`;
                document.body.appendChild(a);
                a.click();
                document.body.removeChild(a);
                URL.revokeObjectURL(url);

                store.showToast(store.t('exportSuccess', { count: data.length }), 'success');
            } else if (data.error) {
                throw new Error(data.error);
            }
        } catch (e) {
            store.showToast(store.t('exportFailed') + ': ' + e.message, 'error');
        }
    },

    /**
     * Import accounts from JSON file
     * @param {Event} event - file input change event
     */
    async importAccounts(event) {
        const store = Alpine.store('global');
        const file = event.target.files?.[0];
        if (!file) return;

        try {
            const text = await file.text();
            const importData = JSON.parse(text);

            // Support both plain array and wrapped format
            const accounts = Array.isArray(importData) ? importData : (importData.accounts || []);
            if (!Array.isArray(accounts) || accounts.length === 0) {
                throw new Error('Invalid file format: expected accounts array');
            }

            const { response, newPassword } = await window.utils.request(
                '/api/accounts/import',
                {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(accounts)
                },
                store.webuiPassword
            );
            if (newPassword) store.webuiPassword = newPassword;

            const data = await response.json();
            if (data.status === 'ok') {
                const { added, updated, failed } = data.results;
                let msg = store.t('importSuccess') + ` ${added.length} added, ${updated.length} updated`;
                if (failed.length > 0) {
                    msg += `, ${failed.length} failed`;
                }
                store.showToast(msg, failed.length > 0 ? 'info' : 'success');
                Alpine.store('data').fetchData();
            } else {
                throw new Error(data.error || 'Import failed');
            }
        } catch (e) {
            store.showToast(store.t('importFailed') + ': ' + e.message, 'error');
        } finally {
            // Reset file input
            event.target.value = '';
        }
    },

    // ==========================================
    // Claude Code Accounts & Gateway Operations
    // ==========================================

    async loadCCAccounts() {
        const password = Alpine.store('global').webuiPassword;
        this.ccLoading = true;
        try {
            const { response, newPassword } = await window.utils.request('/api/claudecode/accounts', {}, password);
            if (newPassword) Alpine.store('global').webuiPassword = newPassword;
            if (!response.ok) return;
            const data = await response.json();
            this.ccAccounts = data.accounts || [];
        } catch (e) {
            console.error('Failed to load Claude Code accounts:', e);
        } finally {
            this.ccLoading = false;
        }
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
        } catch (e) {
            console.error('Failed to load Claude Code config:', e);
        }
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
                body: JSON.stringify(this.ccConfig)
            }, store.webuiPassword);
            if (newPassword) store.webuiPassword = newPassword;
            if (!response.ok) {
                const err = await response.json().catch(() => ({}));
                throw new Error(err.error || `HTTP ${response.status}`);
            }
            this.ccSuccess = 'Settings saved';
            setTimeout(() => { this.ccSuccess = ''; }, 3000);
            store.showToast('Claude Code configuration saved', 'success');
        } catch (e) {
            this.ccError = e.message || 'Save failed';
            store.showToast(this.ccError, 'error');
        } finally {
            this.ccSaving = false;
        }
    },

    async toggleCCAccount(acc) {
        const store = Alpine.store('global');
        acc.enabled = !acc.enabled;
        try {
            const { response, newPassword } = await window.utils.request('/api/claudecode/accounts', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ id: acc.id, enabled: acc.enabled })
            }, store.webuiPassword);
            if (newPassword) store.webuiPassword = newPassword;
            if (!response.ok) {
                acc.enabled = !acc.enabled;
                throw new Error('Failed to update account status');
            }
            store.showToast(`Account ${acc.enabled ? 'enabled' : 'disabled'}`, 'success');
            await this.loadCCAccounts();
        } catch (e) {
            store.showToast(e.message, 'error');
        }
    },

    async testCCAccount(acc) {
        const store = Alpine.store('global');
        this.ccTesting[acc.id] = true;
        this.ccTesting = { ...this.ccTesting };
        try {
            const { response, newPassword } = await window.utils.request('/api/claudecode/accounts/test', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ id: acc.id })
            }, store.webuiPassword);
            if (newPassword) store.webuiPassword = newPassword;
            const data = await response.json();
            if (data.valid) {
                store.showToast(`Account "${acc.name || acc.id}" is valid and ready!`, 'success');
            } else {
                store.showToast(`Account test failed: ${data.error || 'Invalid token'}`, 'error');
            }
            await this.loadCCAccounts();
        } catch (e) {
            store.showToast(`Test error: ${e.message}`, 'error');
        } finally {
            delete this.ccTesting[acc.id];
            this.ccTesting = { ...this.ccTesting };
        }
    },

    async deleteCCAccount(accountId) {
        const store = Alpine.store('global');
        if (!confirm('Are you sure you want to delete this Claude Code account?')) return;
        try {
            const { response, newPassword } = await window.utils.request(`/api/claudecode/accounts/${encodeURIComponent(accountId)}`, {
                method: 'DELETE'
            }, store.webuiPassword);
            if (newPassword) store.webuiPassword = newPassword;
            if (!response.ok) {
                const err = await response.json().catch(() => ({}));
                throw new Error(err.error || 'Failed to delete account');
            }
            store.showToast('Claude Code account deleted', 'success');
            await this.loadCCAccounts();
        } catch (e) {
            store.showToast(e.message, 'error');
        }
    },

    async addCCAccount() {
        const store = Alpine.store('global');
        if (!this.ccNewToken) {
            store.showToast('Please provide a token', 'error');
            return;
        }
        try {
            const { response, newPassword } = await window.utils.request('/api/claudecode/accounts', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({
                    token: this.ccNewToken,
                    name: this.ccNewName || 'Claude Code Account',
                    enabled: true
                })
            }, store.webuiPassword);
            if (newPassword) store.webuiPassword = newPassword;
            if (!response.ok) {
                const err = await response.json().catch(() => ({}));
                throw new Error(err.error || 'Failed to add account');
            }
            this.ccNewToken = '';
            this.ccNewName = '';
            store.showToast('Claude Code account added', 'success');
            await this.loadCCAccounts();
        } catch (e) {
            store.showToast(e.message, 'error');
        }
    },

    async autoImportCC() {
        const store = Alpine.store('global');
        this.ccImporting = true;
        try {
            const { response, newPassword } = await window.utils.request('/api/claudecode/import', {
                method: 'POST'
            }, store.webuiPassword);
            if (newPassword) store.webuiPassword = newPassword;
            const data = await response.json();
            if (!response.ok) throw new Error(data.error || `HTTP ${response.status}`);
            store.showToast(`Imported ${data.imported} Claude Code account(s)`, 'success');
            await this.loadCCAccounts();
        } catch (e) {
            store.showToast(e.message || 'Import failed', 'error');
        } finally {
            this.ccImporting = false;
        }
    }
});
