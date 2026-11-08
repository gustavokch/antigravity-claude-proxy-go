// Tests for the shell's route diagram (route-mimic.js): account aspects must
// agree with the Accounts view, and signals open the matching view.
// Run: node internal/webui/tests/route-mimic.test.mjs
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

function loadMimic(data) {
    const sandbox = { console, window: { Components: {} }, document: { addEventListener() {} } };
    sandbox.globalThis = sandbox;
    vm.createContext(sandbox);
    for (const file of ['account-manager.js', 'gateway-order.js', 'route-mimic.js']) {
        const url = new URL(`../public/js/components/${file}`, import.meta.url);
        vm.runInContext(fs.readFileSync(url, 'utf8'), sandbox, { filename: url.pathname });
    }
    const c = sandbox.window.Components.routeMimic();
    c.$store = { data, global: { t: (k) => k, activeTab: 'dashboard', settingsTab: 'ui', accountsTab: 'google' } };
    return c;
}

const local = (v) => JSON.parse(JSON.stringify(v));
const KNOWN = ['kimi', 'zen', 'claudecode', 'codex', 'openrouter', 'custom', 'cloudcode'];

function gatewayStates() {
    const base = { accounts: [], gateways: {
        order: [], known: KNOWN,
        kimi: { enabled: true, hasApiKey: false, oauth: true },
        zen: { enabled: true, hasApiKey: false },
        openrouter: { enabled: true, hasApiKey: true },
        claudecode: { enabled: false },
        codex: { enabled: false },
        customCount: 2,
    } };
    const cases = [
        ['kimi: an OAuth login is a credential', {}, 'kimi', 'clear', 'routeReady'],
        ['kimi: no key and no login', { gateways: { kimi: { enabled: true, hasApiKey: false, oauth: false } } }, 'kimi', 'danger', 'routeNoKeyOrLogin'],
        ['zen: no key', {}, 'zen', 'danger', 'routeNoKey'],
        ['zen: disabled', { gateways: { zen: { enabled: false } } }, 'zen', 'off', 'routeDisabled'],
        ['openrouter: balance unreadable', { openrouterCredits: { status: 'error' } }, 'openrouter', 'caution', 'routeCreditsError'],
        ['openrouter: under $1', { openrouterCredits: { credits: { balance: 0.5 } } }, 'openrouter', 'danger', 'routeBalanceLow'],
        ['openrouter: under $5', { openrouterCredits: { credits: { balance: 4 } } }, 'openrouter', 'caution', 'routeBalanceLow'],
        ['openrouter: healthy balance', { openrouterCredits: { credits: { balance: 20 } } }, 'openrouter', 'clear', 'routeReady'],
        ['claude code: disabled', {}, 'claudecode', 'off', 'routeDisabled'],
        ['codex: disabled', {}, 'codex', 'off', 'routeDisabled'],
        ['codex: enabled, no account', { gateways: { codex: { enabled: true } } }, 'codex', 'danger', 'routeNoEnabledAccounts'],
        ['custom: endpoints', {}, 'custom', 'clear', 'routeEndpoints'],
        ['custom: none', { gateways: { customCount: 0 } }, 'custom', 'off', 'routeNoEndpoints'],
    ];
    for (const [name, patch, id, aspect, reason] of cases) {
        const data = { ...base, ...patch, gateways: { ...base.gateways, ...(patch.gateways || {}) } };
        const st = loadMimic(data).gatewayState(id);
        assert.equal(st.aspect, aspect, name);
        assert.equal(st.reason, reason, name);
    }
}

// Same result as config.GatewayOrderConfig.Effective("") for the global order.
function effectiveOrderMatchesTheServer() {
    const order = (o) => local(loadMimic({ accounts: [], gateways: { order: o, known: KNOWN } }).effectiveOrder());
    assert.deepEqual(order([' OpenRouter', 'kimi']), ['openrouter', 'kimi', 'zen', 'claudecode', 'codex', 'custom', 'cloudcode']);
    assert.deepEqual(order(['openrouter', 'openrouter', 'bogus']), ['openrouter', 'kimi', 'zen', 'claudecode', 'codex', 'custom', 'cloudcode']);
    assert.deepEqual(order([]), KNOWN);
}

const pools = (...fractions) => ({
    pools: Object.fromEntries(fractions.map((f, i) => [`gemini-${i}`, { remainingFraction: f }])),
});

function exhaustedAccountIsNotReady() {
    const c = loadMimic({
        accounts: [
            { email: 'a@x', provider: 'google', enabled: true, status: 'ok', quota: pools(0, 0) },
        ],
    });
    const cc = c.gatewayState('cloudcode');
    assert.equal(cc.lamps[0].aspect, 'danger', 'every pool at 0% is blocked');
    assert.equal(cc.aspect, 'danger');
    assert.equal(cc.reason, 'routeNoAccountReady');
}

function partlyExhaustedAccountIsCaution() {
    const c = loadMimic({
        accounts: [
            { email: 'a@x', provider: 'google', enabled: true, status: 'ok', quota: pools(0, 0.6) },
            { email: 'b@x', provider: 'google', enabled: true, status: 'ok', quota: pools(0.4, 0.6) },
        ],
    });
    const cc = c.gatewayState('cloudcode');
    assert.deepEqual(cc.lamps.map((l) => l.aspect), ['caution', 'clear']);
    assert.equal(cc.aspect, 'caution', 'both serve; one is partly exhausted');
}

function partlyExhaustedAccountStillServes() {
    const c = loadMimic({ accounts: [
        { email: 'a@x', provider: 'google', enabled: true, status: 'ok', quota: pools(0, 0.8) },
    ] });
    const cc = c.gatewayState('cloudcode');
    assert.equal(cc.lamps[0].aspect, 'caution');
    assert.equal(cc.aspect, 'caution', 'Cloud Code still serves the model with quota left');
    assert.equal(cc.reason, 'routeAccountsReady');
}

function exhaustedClaudeCodeAccountIsNotReady() {
    const c = loadMimic({
        accounts: [{ id: 'c1', email: 'c@x', provider: 'claudecode', enabled: true, status: 'rate_limited', quota: pools(0, 0) }],
        gateways: { claudecode: { enabled: true } },
    });
    const st = c.gatewayState('claudecode');
    assert.equal(st.lamps[0].aspect, 'danger');
    assert.equal(st.reason, 'routeNoAccountReady');
}

// Codex lamps come from the pool snapshots ($store.data.codexAccounts), read
// with the Accounts view's rule: a used-up window blocks, a lost grant needs
// a sign-in, a healthy account with quota left is ready.
function codexLampsFollowTheSnapshots() {
    const win = (usedPercent, windowMinutes) => ({ usedPercent, windowMinutes, resetAt: '0001-01-01T00:00:00Z' });
    const c = loadMimic({
        accounts: [{ email: 'g@x', provider: 'google', enabled: true, status: 'ok' }],
        codexAccounts: [
            { id: 'a', email: 'a@x', enabled: true, status: 'healthy', quota: { primary: win(20, 300), secondary: win(40, 10080) } },
            { id: 'b', email: 'b@x', enabled: true, status: 'healthy', quota: { primary: win(100, 300), secondary: win(100, 10080) } },
            { id: 'c', email: 'c@x', enabled: true, status: 'needs_login' },
            { id: 'd', email: 'd@x', enabled: false, status: 'disabled' },
        ],
        gateways: { codex: { enabled: true } },
    });
    const st = c.gatewayState('codex');
    assert.deepEqual(st.lamps.map((l) => [l.key, l.aspect]),
        [['a', 'clear'], ['b', 'danger'], ['c', 'danger'], ['d', 'off']]);
    assert.equal(st.aspect, 'caution', 'one of three enabled accounts serves');
    assert.equal(st.reason, 'routeAccountsReady');
}

function signalsOpenTheMatchingAccountsTab() {
    const c = loadMimic({ accounts: [] });
    c.open('claudecode');
    assert.equal(c.$store.global.activeTab, 'accounts');
    assert.equal(c.$store.global.accountsTab, 'claudecode');
    c.open('codex');
    assert.equal(c.$store.global.accountsTab, 'codex');
    c.open('cloudcode');
    assert.equal(c.$store.global.accountsTab, 'google');
    c.open('zen');
    assert.equal(c.$store.global.activeTab, 'settings');
    assert.equal(c.$store.global.settingsTab, 'models');
}

const cases = [
    ['an account with every pool exhausted is not ready', exhaustedAccountIsNotReady],
    ['an account with one pool exhausted is caution', partlyExhaustedAccountIsCaution],
    ['a partly exhausted account still serves', partlyExhaustedAccountStillServes],
    ['an exhausted Claude Code account is not ready', exhaustedClaudeCodeAccountIsNotReady],
    ['Codex lamps follow the pool snapshots', codexLampsFollowTheSnapshots],
    ['gateway signals open the matching accounts tab', signalsOpenTheMatchingAccountsTab],
    ['gateway aspects and reasons', gatewayStates],
    ['effective order matches the server', effectiveOrderMatchesTheServer],
];
for (const [name, fn] of cases) {
    await fn();
    console.log('ok', name);
}
console.log(`route-mimic: ${cases.length}/${cases.length} passed`);
