/**
 * Route Mimic
 * The shell's signal-box diagram: client → proxy → each gateway in effective
 * precedence order. Every node carries an aspect (clear / caution / danger /
 * off) derived only from real store data, and a readout naming the reason.
 *
 * Inputs: $store.data.gateways (from /api/config), $store.data accounts
 * (real rows, never placeholder rows), $store.data.openrouterCredits, and
 * $store.data.connectionStatus.
 */
window.Components = window.Components || {};

window.Components.routeMimic = () => ({
    MAX_LAMPS: 8,
    readout: null, // { title, lines[], x, y }

    t(key, params) {
        return this.$store.global.t(key, params);
    },

    realAccounts() {
        const data = this.$store.data;
        return data._realAccounts || data.accounts || [];
    },

    effectiveOrder() {
        const g = this.$store.data.gateways;
        if (!g) return [];
        const known = g.known || [];
        const listed = (g.order || []).filter((id) => known.includes(id));
        const rest = known.filter((id) => !listed.includes(id));
        return [...listed, ...rest];
    },

    label(id) {
        if (id === 'cloudcode') return 'Cloud Code';
        return this.t('gateway' + id.charAt(0).toUpperCase() + id.slice(1));
    },

    accountAspect(acc) {
        if (!acc.enabled || acc.status === 'disabled') return 'off';
        if (acc.status === 'ok') return 'clear';
        if (acc.status === 'invalid' || acc.isInvalid) return 'danger';
        return 'caution'; // rate_limited, cooldown
    },

    accountGroup(provider) {
        const rows = this.realAccounts().filter((a) => (a.provider || 'google') === provider);
        const lamps = rows.map((a) => ({
            key: a.id || a.email,
            aspect: this.accountAspect(a),
            title: a.email || a.name || a.id,
        }));
        const enabled = lamps.filter((l) => l.aspect !== 'off');
        const ready = enabled.filter((l) => l.aspect === 'clear');
        let aspect = 'clear';
        let reason;
        if (enabled.length === 0) {
            aspect = 'danger';
            reason = this.t('routeNoEnabledAccounts');
        } else if (ready.length === 0) {
            aspect = 'danger';
            reason = this.t('routeNoAccountReady');
        } else {
            if (ready.length < enabled.length) aspect = 'caution';
            reason = this.t('routeAccountsReady', { ready: ready.length, total: enabled.length });
        }
        return { aspect, reason, lamps };
    },

    keyedGateway(cfg, needsLogin) {
        if (!cfg || !cfg.enabled) return { aspect: 'off', reason: this.t('routeDisabled'), lamps: [] };
        const hasCred = cfg.hasApiKey || (needsLogin && cfg.oauth);
        if (!hasCred) {
            const reason = needsLogin ? this.t('routeNoKeyOrLogin') : this.t('routeNoKey');
            return { aspect: 'danger', reason, lamps: [{ key: 'k', aspect: 'danger' }] };
        }
        return { aspect: 'clear', reason: this.t('routeReady'), lamps: [{ key: 'k', aspect: 'clear' }] };
    },

    gatewayState(id) {
        const g = this.$store.data.gateways || {};
        switch (id) {
            case 'kimi':
                return this.keyedGateway(g.kimi, true);
            case 'zen':
                return this.keyedGateway(g.zen, false);
            case 'openrouter': {
                const base = this.keyedGateway(g.openrouter, false);
                if (base.aspect !== 'clear') return base;
                const credits = this.$store.data.openrouterCredits;
                if (credits && credits.status === 'error') {
                    return { aspect: 'caution', reason: this.t('routeCreditsError'), lamps: [{ key: 'k', aspect: 'caution' }] };
                }
                if (credits && credits.credits) {
                    const bal = Number(credits.credits.balance);
                    const text = this.t('routeBalanceLow', { balance: '$' + bal.toFixed(2) });
                    if (bal < 1) return { aspect: 'danger', reason: text, lamps: [{ key: 'k', aspect: 'danger' }] };
                    if (bal < 5) return { aspect: 'caution', reason: text, lamps: [{ key: 'k', aspect: 'caution' }] };
                }
                return base;
            }
            case 'claudecode':
                if (!g.claudecode || !g.claudecode.enabled) {
                    return { aspect: 'off', reason: this.t('routeDisabled'), lamps: [] };
                }
                return this.accountGroup('claudecode');
            case 'custom': {
                const n = g.customCount || 0;
                if (n === 0) return { aspect: 'off', reason: this.t('routeNoEndpoints'), lamps: [] };
                return { aspect: 'clear', reason: this.t('routeEndpoints', { n }), lamps: [{ key: 'k', aspect: 'clear' }], count: n };
            }
            case 'cloudcode':
                return this.accountGroup('google');
            default:
                return { aspect: 'off', reason: '', lamps: [] };
        }
    },

    nodes() {
        const order = this.effectiveOrder();
        return order.map((id, i) => {
            const st = this.gatewayState(id);
            const lamps = st.lamps.slice(0, this.MAX_LAMPS);
            return {
                id,
                no: i + 1,
                label: this.label(id),
                aspect: st.aspect,
                reason: st.reason,
                lamps,
                more: Math.max(0, st.lamps.length - lamps.length),
                count: st.count || 0,
                total: order.length,
            };
        });
    },

    proxyAspect() {
        const s = this.$store.data.connectionStatus;
        if (s === 'connected') return 'clear';
        if (s === 'connecting') return 'caution';
        return 'danger';
    },

    proxyReason() {
        const s = this.$store.data.connectionStatus;
        if (s === 'connected') return this.t('routeProxyConnected');
        if (s === 'connecting') return this.t('routeProxyConnecting');
        return this.t('routeProxyDisconnected');
    },

    // Where a signal leads when pulled.
    open(id) {
        const global = this.$store.global;
        if (id === 'cloudcode' || id === 'claudecode') {
            global.activeTab = 'accounts';
            return;
        }
        global.settingsTab = 'models';
        global.activeTab = 'settings';
    },

    show(evt, title, lines) {
        const r = evt.currentTarget.getBoundingClientRect();
        const x = Math.min(Math.max(8, r.left), window.innerWidth - 328);
        this.readout = { title, lines: lines.filter(Boolean), x, y: r.bottom + 8 };
    },

    hide() {
        this.readout = null;
    },

    nodeAria(node) {
        return [node.label, this.t('routePrecedence', { n: node.no, total: node.total }), node.reason].join('. ');
    },
});
