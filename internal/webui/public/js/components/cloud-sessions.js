/**
 * Cloud Sessions Component
 * Read-only view of Claude Code cloud sessions seen by the observe-only
 * forward proxy (internal/mitm), plus its CA download and setup snippet.
 * The proxy binds loopback only; enabling it applies on restart.
 */
window.Components = window.Components || {};

window.Components.cloudSessions = () => ({
    loading: false,
    saving: false,
    error: '',
    configuredEnabled: false,
    status: { enabled: false },
    sessions: [],
    copied: false,
    _timer: null,

    init() {
        this.refresh();
        if (Alpine.store('global').settingsTab === 'cloudsessions') {
            this.startPolling();
        }
        if (this.$watch) {
            this.$watch('$store.global.settingsTab', (tab) => {
                if (tab === 'cloudsessions') {
                    this.refresh();
                    this.startPolling();
                } else {
                    this.stopPolling();
                }
            });
        }
    },

    startPolling() {
        this.stopPolling();
        this._timer = setInterval(() => this.refresh(), 5000);
    },

    stopPolling() {
        if (this._timer) {
            clearInterval(this._timer);
            this._timer = null;
        }
    },

    async getJSON(url) {
        const store = Alpine.store('global');
        const { response, newPassword } = await window.utils.request(url, {}, store?.webuiPassword);
        if (newPassword && store) store.webuiPassword = newPassword;
        if (!response.ok) throw new Error(`HTTP ${response.status}`);
        return response.json();
    },

    async refresh() {
        this.loading = true;
        this.error = '';
        try {
            const [config, status, list] = await Promise.all([
                this.getJSON('/api/config'),
                this.getJSON('/api/mitm/status'),
                this.getJSON('/api/sessions/cloud'),
            ]);
            this.configuredEnabled = config?.config?.mitm?.enabled === true;
            this.status = status || { enabled: false };
            this.sessions = Array.isArray(list?.sessions) ? list.sessions : [];
        } catch (err) {
            this.error = err.message;
            console.error('Failed to load cloud sessions:', err);
        } finally {
            this.loading = false;
        }
    },

    // Persist mitm.enabled. The listener starts or stops on the next restart.
    async setEnabled(value) {
        this.saving = true;
        this.error = '';
        try {
            const store = Alpine.store('global');
            const { response, newPassword } = await window.utils.request('/api/config', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ mitm: { enabled: value } }),
            }, store?.webuiPassword);
            if (newPassword && store) store.webuiPassword = newPassword;
            if (!response.ok) {
                const data = await response.json().catch(() => ({}));
                throw new Error(data.error || `HTTP ${response.status}`);
            }
            this.configuredEnabled = value;
            store.showToast(store.t('cloudSessionsSaved'), 'success');
        } catch (err) {
            this.error = err.message;
            Alpine.store('global').showToast(err.message, 'error');
        } finally {
            this.saving = false;
        }
    },

    envSnippet() {
        const listen = this.status.listen || '127.0.0.1:8092';
        return `HTTPS_PROXY=http://${listen} NODE_EXTRA_CA_CERTS=/path/to/antigravity-proxy-mitm-ca.pem`;
    },

    async copySnippet() {
        try {
            await navigator.clipboard.writeText(this.envSnippet());
            this.copied = true;
            setTimeout(() => { this.copied = false; }, 2000);
        } catch (err) {
            console.error('Clipboard unavailable:', err);
        }
    },

    // Fetched with the password header, so it works when a WebUI password is set.
    async downloadCA() {
        try {
            const store = Alpine.store('global');
            const { response, newPassword } = await window.utils.request('/api/mitm/ca.pem', {}, store?.webuiPassword);
            if (newPassword && store) store.webuiPassword = newPassword;
            if (!response.ok) throw new Error(`HTTP ${response.status}`);
            const blob = await response.blob();
            const link = document.createElement('a');
            link.href = URL.createObjectURL(blob);
            link.download = 'antigravity-proxy-mitm-ca.pem';
            link.click();
            URL.revokeObjectURL(link.href);
        } catch (err) {
            this.error = err.message;
        }
    },

    formatTime(iso) {
        if (!iso) return '';
        const date = new Date(iso);
        return isNaN(date.getTime()) ? '' : date.toLocaleString();
    },
});
