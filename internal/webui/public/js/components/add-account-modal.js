/**
 * Add Account Modal Component
 * Registers itself to window.Components for Alpine.js to consume
 */
window.Components = window.Components || {};

window.Components.addAccountModal = () => ({
    provider: 'google', // 'google' | 'claudecode' | 'kimi'

    // Google OAuth State
    manualMode: false,
    authUrl: '',
    authState: '',
    callbackInput: '',
    submitting: false,

    // Claude Code OAuth State
    claudeCodeManualMode: false,
    claudeCodeAuthUrl: '',
    claudeCodeSessionId: '',
    claudeCodeCallbackInput: '',
    claudeCodeSubmitting: false,

    // Kimi Code Device-Flow Login State
    kimiStarting: false,
    kimiOAuth: { sessionId: '', userCode: '', verificationUri: '', status: '', error: '', polling: false },

    /**
     * Reset all state to initial values
     */
    resetState() {
        this._cancelKimiSession();
        this.kimiStarting = false;
        this.kimiOAuth = { sessionId: '', userCode: '', verificationUri: '', status: '', error: '', polling: false };
        this.provider = 'google';
        this.manualMode = false;
        this.authUrl = '';
        this.authState = '';
        this.callbackInput = '';
        this.submitting = false;

        this.claudeCodeManualMode = false;
        this.claudeCodeAuthUrl = '';
        this.claudeCodeSessionId = '';
        this.claudeCodeCallbackInput = '';
        this.claudeCodeSubmitting = false;

        // Close any open details elements
        const details = document.querySelectorAll('#add_account_modal details[open]');
        details.forEach(d => d.removeAttribute('open'));
    },

    setProvider(prov) {
        if (this.provider === 'kimi' && prov !== 'kimi' && this.kimiOAuth.status === 'pending') {
            this._cancelKimiSession();
            this.kimiOAuth = { sessionId: '', userCode: '', verificationUri: '', status: '', error: '', polling: false };
        }
        this.provider = prov;
    },

    // --- Google OAuth Methods ---
    async copyLink() {
        if (!this.authUrl) return;
        await navigator.clipboard.writeText(this.authUrl);
        Alpine.store('global').showToast(Alpine.store('global').t('linkCopied'), 'success');
    },

    async initManualAuth(event) {
        if (event.target.open && !this.authUrl) {
            try {
                const password = Alpine.store('global').webuiPassword;
                const {
                    response,
                    newPassword
                } = await window.utils.request('/api/auth/url', {}, password);
                if (newPassword) Alpine.store('global').webuiPassword = newPassword;
                const data = await response.json();
                if (data.status === 'ok') {
                    this.authUrl = data.url;
                    this.authState = data.state;
                }
            } catch (e) {
                Alpine.store('global').showToast(e.message, 'error');
            }
        }
    },

    async completeManualAuth() {
        if (!this.callbackInput || !this.authState) return;
        this.submitting = true;
        try {
            const store = Alpine.store('global');
            const {
                response,
                newPassword
            } = await window.utils.request('/api/auth/complete', {
                method: 'POST',
                headers: {
                    'Content-Type': 'application/json'
                },
                body: JSON.stringify({
                    callbackInput: this.callbackInput,
                    state: this.authState
                })
            }, store.webuiPassword);
            if (newPassword) store.webuiPassword = newPassword;
            const data = await response.json();
            if (data.status === 'ok') {
                store.showToast(store.t('accountAddedSuccess'), 'success');
                Alpine.store('data').fetchData();
                document.getElementById('add_account_modal').close();
                this.resetState();
            } else {
                store.showToast(data.error || store.t('authFailed'), 'error');
            }
        } catch (e) {
            Alpine.store('global').showToast(e.message, 'error');
        } finally {
            this.submitting = false;
        }
    },

    // --- Claude Code OAuth Methods ---
    async copyClaudeCodeLink() {
        if (!this.claudeCodeAuthUrl) return;
        await navigator.clipboard.writeText(this.claudeCodeAuthUrl);
        Alpine.store('global').showToast(Alpine.store('global').t('linkCopied'), 'success');
    },

    async initClaudeCodeManualAuth(event) {
        if (event.target.open && !this.claudeCodeAuthUrl) {
            try {
                const password = Alpine.store('global').webuiPassword;
                const {
                    response,
                    newPassword
                } = await window.utils.request('/api/claudecode/auth/start', {
                    method: 'POST',
                    headers: {
                        'Content-Type': 'application/json'
                    },
                    body: JSON.stringify({ mode: 'manual' })
                }, password);
                if (newPassword) Alpine.store('global').webuiPassword = newPassword;
                const data = await response.json();
                if (data.status === 'ok') {
                    this.claudeCodeAuthUrl = data.manual_auth_url || data.auth_url;
                    this.claudeCodeSessionId = data.session_id;
                }
            } catch (e) {
                Alpine.store('global').showToast(e.message, 'error');
            }
        }
    },

    async completeClaudeCodeManualAuth() {
        if (!this.claudeCodeCallbackInput || !this.claudeCodeSessionId) return;
        this.claudeCodeSubmitting = true;
        try {
            const store = Alpine.store('global');
            const {
                response,
                newPassword
            } = await window.utils.request('/api/claudecode/auth/complete', {
                method: 'POST',
                headers: {
                    'Content-Type': 'application/json'
                },
                body: JSON.stringify({
                    session_id: this.claudeCodeSessionId,
                    code: this.claudeCodeCallbackInput
                })
            }, store.webuiPassword);
            if (newPassword) store.webuiPassword = newPassword;
            const data = await response.json();
            if (data.status === 'ok') {
                store.showToast(
                    (store.t('claudeCodeAccountAddedSuccess') || 'Claude Code account added successfully') + (data.account?.email ? ': ' + data.account.email : ''),
                    'success'
                );
                Alpine.store('data').fetchData();
                document.getElementById('add_account_modal').close();
                this.resetState();
            } else {
                store.showToast(data.error || 'Authentication failed', 'error');
            }
        } catch (e) {
            Alpine.store('global').showToast(e.message, 'error');
        } finally {
            this.claudeCodeSubmitting = false;
        }
    },

    // --- Kimi Code Device-Flow Login Methods ---
    async startKimiLogin() {
        const store = Alpine.store('global');
        const state = this.kimiOAuth; // resetState() swaps this object out
        this.kimiStarting = true;
        try {
            const { response, newPassword } = await window.utils.request('/api/kimi/auth/start', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: '{}'
            }, store.webuiPassword);
            if (newPassword) store.webuiPassword = newPassword;
            const data = await response.json().catch(() => ({}));
            if (this.kimiOAuth !== state) {
                // Modal closed while starting: drop the new server session, stay idle.
                if (response.ok && data.session_id) this._postKimiCancel(data.session_id);
                return;
            }
            if (!response.ok || data.status !== 'ok') {
                store.showToast(data.error || `HTTP ${response.status}`, 'error');
                return;
            }
            this.kimiOAuth.sessionId = data.session_id || '';
            this.kimiOAuth.userCode = data.user_code || '';
            this.kimiOAuth.verificationUri = data.verification_uri_complete || data.verification_uri || '';
            this.kimiOAuth.status = 'pending';
            this.kimiOAuth.error = '';
            this.kimiOAuth.polling = true;
            this._pollKimiLogin();
        } catch (e) {
            store.showToast(e.message || 'Failed to start Kimi Code login', 'error');
        } finally {
            this.kimiStarting = false;
        }
    },

    async _pollKimiLogin() {
        const store = Alpine.store('global');
        const sessionId = this.kimiOAuth.sessionId;
        // Live only while polling THIS session; a cancel or newer login ends the loop.
        const live = () => this.kimiOAuth.polling && this.kimiOAuth.sessionId === sessionId;
        while (live()) {
            await new Promise(resolve => setTimeout(resolve, 2000));
            if (!live()) return;
            try {
                const { response, newPassword } = await window.utils.request(
                    `/api/kimi/auth/status?session_id=${encodeURIComponent(sessionId)}`,
                    {}, store.webuiPassword);
                if (newPassword) store.webuiPassword = newPassword;
                const data = await response.json().catch(() => ({}));
                if (!live()) {
                    // Cancelled/reset/superseded while in flight. The server persists a
                    // login before answering 'completed', so resync the store anyway.
                    if (response.ok && data.status === 'completed') await this._refreshKimiStore();
                    return;
                }
                if (!response.ok) {
                    this.kimiOAuth.status = 'error';
                    this.kimiOAuth.error = data.error || `HTTP ${response.status}`;
                    this.kimiOAuth.polling = false;
                    return;
                }
                if (data.status === 'completed') {
                    this.kimiOAuth.status = 'completed';
                    this.kimiOAuth.polling = false;
                    const email = data.account && (data.account.email || data.account.nickname || data.account.user_id);
                    store.showToast(
                        (store.t('kimiOAuthSuccess') || 'Kimi Code login complete') + (email ? ': ' + email : ''),
                        'success');
                    await this._refreshKimiStore();
                    document.getElementById('add_account_modal')?.close();
                    return;
                }
                if (['expired', 'denied', 'cancelled', 'error'].includes(data.status)) {
                    this.kimiOAuth.status = data.status;
                    this.kimiOAuth.error = data.error || '';
                    this.kimiOAuth.polling = false;
                    return;
                }
            } catch (e) {
                if (!live()) return; // cancelled/reset/superseded while in flight
                this.kimiOAuth.status = 'error';
                this.kimiOAuth.error = e.message || 'Login status check failed';
                this.kimiOAuth.polling = false;
                return;
            }
        }
    },

    async _cancelKimiSession() {
        const { status, sessionId } = this.kimiOAuth;
        this.kimiOAuth.polling = false;
        if (status === 'pending' && sessionId) await this._postKimiCancel(sessionId);
    },

    async _postKimiCancel(sessionId) {
        const store = Alpine.store('global');
        try {
            const { newPassword } = await window.utils.request('/api/kimi/auth/cancel', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ session_id: sessionId })
            }, store.webuiPassword);
            if (newPassword) store.webuiPassword = newPassword;
        } catch (e) {
            // Best-effort; the session expires on its own.
        }
    },

    async _refreshKimiStore() {
        const store = Alpine.store('global');
        try {
            const { response, newPassword } = await window.utils.request('/api/kimi/config', {}, store.webuiPassword);
            if (newPassword) store.webuiPassword = newPassword;
            if (!response.ok) return;
            const data = await response.json();
            if (data.config) Alpine.store('data').kimi = data.config;
        } catch (e) {
            // Non-fatal: the settings page refetches the config on load.
        }
    }
});
