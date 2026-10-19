/**
 * Antigravity Console - Main Entry
 *
 * This file orchestrates Alpine.js initialization.
 * Components are loaded via separate script files that register themselves
 * to window.Components before this script runs.
 */

document.addEventListener('alpine:init', () => {
    // Register Components (loaded from separate files via window.Components)
    Alpine.data('dashboard', window.Components.dashboard);
    Alpine.data('models', window.Components.models);
    Alpine.data('accountManager', window.Components.accountManager);
    Alpine.data('claudeConfig', window.Components.claudeConfig);
    Alpine.data('logsViewer', window.Components.logsViewer);
    Alpine.data('addAccountModal', window.Components.addAccountModal);
    Alpine.data('routeMimic', window.Components.routeMimic);

    // View Loader Directive
    Alpine.directive('load-view', (el, { expression }, { evaluate }) => {
        if (!window.viewCache) window.viewCache = new Map();

        // Evaluate the expression to get the actual view name (removes quotes)
        const viewName = evaluate(expression);

        if (window.viewCache.has(viewName)) {
            el.innerHTML = window.viewCache.get(viewName);
            Alpine.initTree(el);
            return;
        }

        fetch(`views/${viewName}.html?t=${Date.now()}`)
            .then(response => {
                if (!response.ok) throw new Error(`HTTP ${response.status}`);
                return response.text();
            })
            .then(html => {
                // Update cache (optional, or remove if we want always-fresh)
                // keeping cache for session performance, but initial load will now bypass browser cache
                window.viewCache.set(viewName, html);
                el.innerHTML = html;
                Alpine.initTree(el);
            })
            .catch(err => {
                console.error('Failed to load view:', viewName, err);
                el.innerHTML = `<div class="view"><div class="notice notice-danger" role="alert">
                    <span class="lamp lamp-danger" aria-hidden="true"></span>
                    <div><div class="notice-title">Error loading view: ${viewName}</div>
                    <p>${err.message}. Reload the page; if it persists, check the proxy log.</p></div>
                </div></div>`;
            });
    });

    // Main App Controller
    Alpine.data('app', () => ({
        get connectionStatus() {
            return Alpine.store('data')?.connectionStatus || 'connecting';
        },
        get loading() {
            return Alpine.store('data')?.loading || false;
        },

        views: ['dashboard', 'models', 'accounts', 'logs', 'settings'],

        init() {
            // Lever keys: 1-5 pull the view levers, unless the operator is
            // typing or a modal holds focus: an open <dialog> or a
            // class-opened .modal-open one, or a visible aria-modal confirm
            // such as the dashboard's clear-stats prompt.
            window.addEventListener('keydown', (e) => {
                if (e.metaKey || e.ctrlKey || e.altKey) return;
                const el = e.target;
                if (el && (el.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName))) return;
                if (document.querySelector('dialog[open], .modal-open')) return;
                if ([...document.querySelectorAll('[aria-modal="true"]')].some((m) => m.checkVisibility())) return;
                const i = Number(e.key) - 1;
                if (Number.isInteger(i) && i >= 0 && i < this.views.length) {
                    Alpine.store('global').activeTab = this.views[i];
                }
            });

            this.applyChartDefaults();
            document.addEventListener('sb-theme-changed', () => this.applyChartDefaults());

            // Start Data Polling
            this.startAutoRefresh();
            document.addEventListener('refresh-interval-changed', () => this.startAutoRefresh());

            // Initial Fetch
            Alpine.store('data').fetchData();
        },

        // Chart.js reads colors once per chart; dashboard charts rebuild on
        // sb-theme-changed and pick these defaults up.
        applyChartDefaults() {
            if (typeof Chart === 'undefined') return;
            Chart.defaults.color = window.utils.getThemeColor('--sb-chart-tick');
            Chart.defaults.borderColor = window.utils.getThemeColor('--sb-chart-grid');
            Chart.defaults.font.family = '"Barlow", system-ui, sans-serif';
        },

        refreshTimer: null,

        fetchData() {
            Alpine.store('data').fetchData();
        },

        startAutoRefresh() {
            if (this.refreshTimer) clearInterval(this.refreshTimer);
            const interval = parseInt(Alpine.store('settings')?.refreshInterval || 60);
            if (interval > 0) {
                this.refreshTimer = setInterval(() => Alpine.store('data').fetchData(), interval * 1000);
            }
        },

        t(key) {
            return Alpine.store('global')?.t(key) || key;
        },

        async addAccountWeb(reAuthEmail = null) {
            const password = Alpine.store('global').webuiPassword;
            try {
                const urlPath = reAuthEmail
                    ? `/api/auth/url?email=${encodeURIComponent(reAuthEmail)}`
                    : '/api/auth/url';

                const { response, newPassword } = await window.utils.request(urlPath, {}, password);
                if (newPassword) Alpine.store('global').webuiPassword = newPassword;

                const data = await response.json();

                if (data.status === 'ok') {
                    // Show info toast that OAuth is in progress
                    Alpine.store('global').showToast(Alpine.store('global').t('oauthInProgress'), 'info');

                    // Open OAuth window
                    const oauthWindow = window.open(data.url, 'google_oauth', 'width=600,height=700,scrollbars=yes');

                    // Poll for account changes instead of relying on postMessage
                    // (since OAuth callback is now on port 51121, not this server)
                    const initialAccountCount = Alpine.store('data').accounts.length;
                    let pollCount = 0;
                    const maxPolls = 60; // 2 minutes (2 second intervals)
                    let cancelled = false;

                    // Show progress modal
                    Alpine.store('global').oauthProgress = {
                        active: true,
                        current: 0,
                        max: maxPolls,
                        cancel: () => {
                            cancelled = true;
                            clearInterval(pollInterval);
                            Alpine.store('global').oauthProgress.active = false;
                            Alpine.store('global').showToast(Alpine.store('global').t('oauthCancelled'), 'info');
                            if (oauthWindow && !oauthWindow.closed) {
                                oauthWindow.close();
                            }
                        }
                    };

                    const pollInterval = setInterval(async () => {
                        if (cancelled) {
                            clearInterval(pollInterval);
                            return;
                        }

                        pollCount++;
                        Alpine.store('global').oauthProgress.current = pollCount;

                        // Check if OAuth window was closed manually
                        if (oauthWindow && oauthWindow.closed && !cancelled) {
                            clearInterval(pollInterval);
                            Alpine.store('global').oauthProgress.active = false;
                            Alpine.store('global').showToast(Alpine.store('global').t('oauthWindowClosed'), 'warning');
                            return;
                        }

                        // Refresh account list
                        await Alpine.store('data').fetchData();

                        // Check if new account was added
                        const currentAccountCount = Alpine.store('data').accounts.length;
                        if (currentAccountCount > initialAccountCount) {
                            clearInterval(pollInterval);
                            Alpine.store('global').oauthProgress.active = false;

                            const actionKey = reAuthEmail ? 'accountReauthSuccess' : 'accountAddedSuccess';
                            Alpine.store('global').showToast(
                                Alpine.store('global').t(actionKey),
                                'success'
                            );
                            document.getElementById('add_account_modal')?.close();

                            if (oauthWindow && !oauthWindow.closed) {
                                oauthWindow.close();
                            }
                        }

                        // Stop polling after max attempts
                        if (pollCount >= maxPolls) {
                            clearInterval(pollInterval);
                            Alpine.store('global').oauthProgress.active = false;
                            Alpine.store('global').showToast(
                                Alpine.store('global').t('oauthTimeout'),
                                'warning'
                            );
                        }
                    }, 2000); // Poll every 2 seconds
                } else {
                    Alpine.store('global').showToast(data.error || Alpine.store('global').t('failedToGetAuthUrl'), 'error');
                }
            } catch (e) {
                Alpine.store('global').showToast(Alpine.store('global').t('failedToStartOAuth') + ': ' + e.message, 'error');
            }
        },

        async addClaudeCodeAccountWeb(mode = 'loopback') {
            try {
                const res = await fetch('/api/claudecode/auth/start', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ mode: mode })
                });
                const data = await res.json();

                if (data.status === 'ok' && data.auth_url) {
                    const sessionId = data.session_id;

                    if (mode === 'manual') {
                        return data;
                    }

                    const width = 600, height = 700;
                    const left = (window.innerWidth - width) / 2;
                    const top = (window.innerHeight - height) / 2;
                    const oauthWindow = window.open(
                        data.auth_url,
                        'claudecode_oauth',
                        `width=${width},height=${height},top=${top},left=${left},scrollbars=yes`
                    );

                    Alpine.store('global').oauthProgress = {
                        active: true,
                        email: 'Claude.ai Account',
                        mode: 'add',
                        source: 'claudecode',
                        sessionId: sessionId,
                        step: 1,
                        totalSteps: 2,
                        status: Alpine.store('global').t('oauthInProgress')
                    };

                    let pollCount = 0;
                    const maxPolls = 150;
                    const pollInterval = setInterval(async () => {
                        pollCount++;
                        if (!Alpine.store('global').oauthProgress.active) {
                            clearInterval(pollInterval);
                            try {
                                await fetch('/api/claudecode/auth/cancel', {
                                    method: 'POST',
                                    headers: { 'Content-Type': 'application/json' },
                                    body: JSON.stringify({ session_id: sessionId })
                                });
                            } catch (err) {}
                            return;
                        }

                        try {
                            const statusRes = await fetch(`/api/claudecode/auth/status?session_id=${sessionId}`);
                            const statusData = await statusRes.json();

                            if (statusData.status === 'completed') {
                                clearInterval(pollInterval);
                                Alpine.store('global').oauthProgress.active = false;
                                await Alpine.store('data').fetchData();
                                Alpine.store('global').showToast(
                                    (Alpine.store('global').t('claudeCodeAccountAddedSuccess') || 'Claude Code account added successfully') + (statusData.account?.email ? ': ' + statusData.account.email : ''),
                                    'success'
                                );
                                document.getElementById('add_account_modal')?.close();
                                if (oauthWindow && !oauthWindow.closed) {
                                    oauthWindow.close();
                                }
                            } else if (statusData.status === 'failed' || statusData.status === 'expired') {
                                clearInterval(pollInterval);
                                Alpine.store('global').oauthProgress.active = false;
                                Alpine.store('global').showToast(statusData.error || 'Claude Code OAuth failed', 'error');
                            }
                        } catch (err) {
                            console.error('Claude Code OAuth status poll error:', err);
                        }

                        if (pollCount >= maxPolls) {
                            clearInterval(pollInterval);
                            Alpine.store('global').oauthProgress.active = false;
                            Alpine.store('global').showToast(
                                Alpine.store('global').t('oauthTimeout'),
                                'warning'
                            );
                        }
                    }, 2000);
                } else {
                    Alpine.store('global').showToast(data.error || Alpine.store('global').t('failedToGetAuthUrl'), 'error');
                }
            } catch (e) {
                Alpine.store('global').showToast(Alpine.store('global').t('failedToStartOAuth') + ': ' + e.message, 'error');
            }
        }
    }));
});