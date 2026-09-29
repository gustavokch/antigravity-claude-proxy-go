/**
 * Cloud Sessions Component
 * Read-only view of Claude Code cloud sessions seen by the observe-only
 * forward proxy (internal/mitm), plus its CA download and setup snippet.
 * The proxy binds loopback only; enabling it applies on restart.
 *
 * x-load-view mounts every view at page load, so nothing is fetched until the
 * user opens Settings > Cloud, and polling runs only while that tab is showing.
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
    _inflight: false,

    init() {
        const sync = () => (this.onCloudTab() ? this.activate() : this.stopPolling());
        if (this.$watch) {
            this.$watch('$store.global.settingsTab', sync);
            this.$watch('$store.global.activeTab', sync);
        }
        if (this.onCloudTab()) this.activate();
    },

    // Settings > Cloud is the tab the user is looking at.
    onCloudTab() {
        const store = Alpine.store('global');
        return store.activeTab === 'settings' && store.settingsTab === 'cloudsessions';
    },

    // Load everything and (re)start polling. The Refresh button does the same,
    // so it also resumes polling after a dismissed password prompt stopped it.
    activate() {
        this.startPolling();
        this.refresh({ withConfig: true });
    },

    startPolling() {
        this.stopPolling();
        this._timer = setInterval(() => {
            if (!document.hidden) this.refresh();
        }, 5000);
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
        // utils.request already prompted for the password, so a 401 here means
        // the prompt was dismissed: stop polling instead of prompting every 5 s.
        if (response.status === 401) this.stopPolling();
        if (!response.ok) throw new Error(`HTTP ${response.status}`);
        return response.json();
    },

    // Poll ticks skip /api/config: the enabled flag only changes through setEnabled.
    async refresh({ withConfig = false } = {}) {
        if (this._inflight) return;
        this._inflight = true;
        this.loading = true;
        this.error = '';
        try {
            const calls = [this.getJSON('/api/mitm/status'), this.getJSON('/api/sessions/cloud')];
            if (withConfig) calls.push(this.getJSON('/api/config'));
            const [status, list, config] = await Promise.all(calls);
            if (config) this.configuredEnabled = config?.config?.mitm?.enabled === true;
            this.status = status || { enabled: false };
            this.sessions = Array.isArray(list?.sessions) ? list.sessions : [];
        } catch (err) {
            this.error = err.message;
            console.error('Failed to load cloud sessions:', err);
        } finally {
            this.loading = false;
            this._inflight = false;
        }
    },

    // Persist mitm.enabled. The listener starts or stops on the next restart.
    async setEnabled(input) {
        const value = input.checked;
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
            // Nothing was saved and configuredEnabled did not change, so Alpine
            // will not re-render the box: put it back to what the server has.
            input.checked = !value;
            this.error = err.message;
            Alpine.store('global').showToast(err.message, 'error');
        } finally {
            this.saving = false;
        }
    },

    // NO_PROXY keeps a plain-http ANTHROPIC_BASE_URL (the gateway) off the
    // CONNECT-only proxy, which answers 405 to anything else.
    envSnippet() {
        const listen = this.status.listen || '127.0.0.1:8092';
        return `HTTPS_PROXY=http://${listen} NO_PROXY=127.0.0.1,localhost NODE_EXTRA_CA_CERTS=/path/to/antigravity-proxy-mitm-ca.pem`;
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
            const url = URL.createObjectURL(blob);
            const link = document.createElement('a');
            link.href = url;
            link.download = 'antigravity-proxy-mitm-ca.pem';
            document.body.appendChild(link);
            link.click();
            link.remove();
            // Revoking right after click() can cancel the download in Firefox and Safari.
            setTimeout(() => URL.revokeObjectURL(url), 1000);
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
