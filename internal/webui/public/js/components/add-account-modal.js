/**
 * Add Account Modal Component
 * Registers itself to window.Components for Alpine.js to consume
 */
window.Components = window.Components || {};

window.Components.addAccountModal = () => ({
    provider: 'google', // 'google' | 'claudecode'

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

    /**
     * Reset all state to initial values
     */
    resetState() {
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
    }
});
