/**
 * Add Account Modal Component
 * Registers itself to window.Components for Alpine.js to consume
 */
window.Components = window.Components || {};

// The Codex sign-in popup. Kept outside the component's reactive state, as
// app.js keeps its popups in local variables: Alpine would wrap the window in
// a proxy, and a cross-origin window rejects calls made through one.
let codexLoginPopup = null;

// A Codex login with no session. resetState() swaps in a fresh one, which is
// how requests still in flight for an older login know they are stale.
const idleCodexLogin = () => ({ sessionId: '', authUrl: '', mode: '', status: '', error: '', polling: false });

window.Components.addAccountModal = () => ({
    provider: 'google', // 'google' | 'claudecode' | 'kimi' | 'codex'

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

    // ChatGPT Codex Login State
    codexStarting: false,
    codexSubmitting: false,
    codexRedirectInput: '',
    codexOAuth: idleCodexLogin(),

    /**
     * Reset all state to initial values
     */
    resetState() {
        this._cancelKimiSession();
        this._cancelCodexSession();
        this.kimiStarting = false;
        this.kimiOAuth = { sessionId: '', userCode: '', verificationUri: '', status: '', error: '', polling: false };
        this.codexStarting = false;
        this.codexSubmitting = false;
        this.codexRedirectInput = '';
        this.codexOAuth = idleCodexLogin();
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
        if (this.provider === 'codex' && prov !== 'codex' && this.codexOAuth.status === 'pending') {
            this._cancelCodexSession();
            this.codexOAuth = idleCodexLogin();
            this.codexRedirectInput = '';
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
                    // A reset or newer login during the refresh owns the modal now.
                    if (this.kimiOAuth.sessionId === sessionId) {
                        document.getElementById('add_account_modal')?.close();
                    }
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
    },

    // --- ChatGPT Codex Login Methods ---
    // Connect opens OpenAI's sign-in in a popup and polls the session. After
    // sign-in the browser is sent to localhost:1455, where the proxy listens.
    // When the browser is not on the proxy's machine, or the proxy fell back to
    // manual mode, that page does not load and the user pastes its URL;
    // completeCodexLogin() then finishes the exchange.
    async startCodexLogin() {
        const store = Alpine.store('global');
        const state = this.codexOAuth; // resetState() swaps this object out
        this.codexStarting = true;
        try {
            const { response, newPassword } = await window.utils.request('/api/codex/auth/start', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ mode: 'loopback' })
            }, store.webuiPassword);
            if (newPassword) store.webuiPassword = newPassword;
            const data = await response.json().catch(() => ({}));
            if (this.codexOAuth !== state) {
                // Modal closed while starting: drop the new server session, stay idle.
                if (response.ok && data.session_id) this._postCodexCancel(data.session_id);
                return;
            }
            if (!response.ok || data.status !== 'ok' || !data.session_id) {
                store.showToast(data.error || `HTTP ${response.status}`, 'error');
                return;
            }
            Object.assign(state, {
                sessionId: data.session_id,
                authUrl: data.auth_url || '',
                mode: data.mode || 'loopback',
                status: 'pending',
                error: '',
                polling: true
            });
            this.codexRedirectInput = '';
            if (state.authUrl) {
                codexLoginPopup = window.open(state.authUrl, 'codex_oauth', 'width=600,height=700,scrollbars=yes');
            }
            this._pollCodexLogin();
        } catch (e) {
            store.showToast(e.message || store.t('failedToStartOAuth'), 'error');
        } finally {
            this.codexStarting = false;
        }
    },

    async _pollCodexLogin() {
        const store = Alpine.store('global');
        const state = this.codexOAuth;
        const sessionId = state.sessionId;
        // Live only while polling THIS session; a cancel, reset or newer login ends the loop.
        const live = () => this.codexOAuth === state && state.polling && state.sessionId === sessionId;
        while (live()) {
            await new Promise(resolve => setTimeout(resolve, 2000));
            if (!live()) return;
            try {
                const { response, newPassword } = await window.utils.request(
                    `/api/codex/auth/status?session_id=${encodeURIComponent(sessionId)}`,
                    {}, store.webuiPassword);
                if (newPassword) store.webuiPassword = newPassword;
                const data = await response.json().catch(() => ({}));
                const session = data.session || {};
                if (!live()) {
                    // Cancelled, reset or superseded while in flight. The server registers
                    // a login before reporting 'completed', so resync the store anyway,
                    // unless the pasted URL already finished this login.
                    if (response.ok && session.status === 'completed' && state.status !== 'completed') {
                        await this._refreshCodexStore();
                    }
                    return;
                }
                if (!response.ok) {
                    this._failCodexLogin(state, 'error', data.error || `HTTP ${response.status}`);
                    return;
                }
                if (session.status === 'completed') {
                    await this._finishCodexLogin(state, session.email);
                    return;
                }
                if (session.status === 'expired' || session.status === 'failed') {
                    const fallback = session.status === 'expired' ? 'codexOAuthExpired' : 'codexOAuthFailed';
                    this._failCodexLogin(state, session.status, session.error || store.t(fallback));
                    return;
                }
                if (session.status === 'cancelled') {
                    this._failCodexLogin(state, 'cancelled', '');
                    return;
                }
            } catch (e) {
                if (!live()) return; // cancelled/reset/superseded while in flight
                this._failCodexLogin(state, 'error', e.message || 'Login status check failed');
                return;
            }
        }
    },

    // Sends the URL the browser was redirected to (or its bare code); the
    // server checks its state against the session and exchanges the code.
    async completeCodexLogin() {
        const store = Alpine.store('global');
        const state = this.codexOAuth;
        const redirectUrl = this.codexRedirectInput.trim();
        if (!redirectUrl || state.status !== 'pending' || !state.sessionId || this.codexSubmitting) return;
        this.codexSubmitting = true;
        try {
            const { response, newPassword } = await window.utils.request('/api/codex/auth/complete', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ session_id: state.sessionId, redirect_url: redirectUrl })
            }, store.webuiPassword);
            if (newPassword) store.webuiPassword = newPassword;
            const data = await response.json().catch(() => ({}));
            const ok = response.ok && data.status === 'ok';
            if (this.codexOAuth !== state) {
                // Closed while the exchange ran; the server kept the account, so resync.
                if (ok) await this._refreshCodexStore();
                return;
            }
            if (!ok) {
                // The loopback callback may have finished this login first.
                if (state.status !== 'completed') store.showToast(data.error || `HTTP ${response.status}`, 'error');
                return;
            }
            await this._finishCodexLogin(state, data.account && data.account.email);
        } catch (e) {
            if (this.codexOAuth === state && state.status !== 'completed') {
                store.showToast(e.message || store.t('authFailed'), 'error');
            }
        } finally {
            if (this.codexOAuth === state) this.codexSubmitting = false;
        }
    },

    // The poll and the pasted URL can both see one login finish; the first
    // announces it and closes the modal, the second does nothing.
    async _finishCodexLogin(state, email) {
        if (state.status === 'completed') return;
        const store = Alpine.store('global');
        state.status = 'completed';
        state.error = '';
        state.polling = false;
        this._closeCodexPopup();
        store.showToast(store.t('codexAccountAdded') + (email ? ': ' + email : ''), 'success');
        await this._refreshCodexStore();
        // A reset or newer login during the refresh owns the modal now.
        if (this.codexOAuth === state) {
            document.getElementById('add_account_modal')?.close();
        }
    },

    _failCodexLogin(state, status, error) {
        state.status = status;
        state.error = error;
        state.polling = false;
        this._closeCodexPopup();
    },

    async _cancelCodexSession() {
        const { status, sessionId } = this.codexOAuth;
        this.codexOAuth.polling = false;
        this._closeCodexPopup();
        if (status === 'pending' && sessionId) await this._postCodexCancel(sessionId);
    },

    async _postCodexCancel(sessionId) {
        const store = Alpine.store('global');
        try {
            const { newPassword } = await window.utils.request('/api/codex/auth/cancel', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ session_id: sessionId })
            }, store.webuiPassword);
            if (newPassword) store.webuiPassword = newPassword;
        } catch (e) {
            // Best-effort; the session expires on its own.
        }
    },

    _closeCodexPopup() {
        if (codexLoginPopup && !codexLoginPopup.closed) codexLoginPopup.close();
        codexLoginPopup = null;
    },

    // A login adds an account and turns the gateway on; the Accounts tab and
    // the route diagram read both from the data store.
    async _refreshCodexStore() {
        const data = Alpine.store('data');
        await Promise.all([data.fetchCodexAccounts(true), data.performHealthCheck()]);
    }
});
