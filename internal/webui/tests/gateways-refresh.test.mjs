// The route diagram reads $store.data.gateways, built from GET /api/config.
// It must follow the server payload shape, and a Settings save must refresh it.
// Run: node internal/webui/tests/gateways-refresh.test.mjs
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const local = (v) => JSON.parse(JSON.stringify(v));

function loadStores(serverConfig, codexAccounts = []) {
    const listeners = {};
    const stores = {};
    const urls = [];
    const sandbox = {
        console, setTimeout, clearTimeout, setInterval: () => 0, clearInterval() {},
        document: { addEventListener: (e, f) => (listeners[e] = listeners[e] || []).push(f), hidden: false },
        window: { Components: {} },
        Alpine: { store: (n, v) => (v ? (stores[n] = v) : stores[n]), data() {} },
    };
    sandbox.window.utils = {
        request: async (url, opts = {}) => {
            urls.push(url);
            if (url === '/api/codex/accounts') {
                return { response: { ok: true, json: async () => ({ status: 'ok', accounts: codexAccounts }) } };
            }
            if (opts.method === 'POST') Object.assign(serverConfig, JSON.parse(opts.body));
            return { response: { ok: true, json: async () => ({ status: 'ok', config: serverConfig }) } };
        },
    };
    sandbox.globalThis = sandbox;
    vm.createContext(sandbox);
    for (const f of ['data-store.js', 'components/gateway-order.js']) {
        const url = new URL(`../public/js/${f}`, import.meta.url);
        vm.runInContext(fs.readFileSync(url, 'utf8'), sandbox, { filename: url.pathname });
    }
    (listeners['alpine:init'] || []).forEach((f) => f());
    stores.global = { webuiPassword: '', showToast() {}, t: (k) => k };
    return { data: stores.data, sandbox, urls };
}

// Shape of config.GetPublicConfig (internal/config/config.go) as the
// diagram needs it, including Zen's env-only key (applyZenKeySource).
async function gatewaysFollowThePublicConfig() {
    const cfg = {
        gatewayOrder: { order: ['zen', 'kimi'] },
        knownProviders: ['kimi', 'zen', 'claudecode', 'codex', 'openrouter', 'custom', 'cloudcode'],
        kimi: { enabled: true, oauth: { hasToken: true, maskedToken: 'tok…abcd' } },
        zen: { enabled: true, keySource: 'env', hasApiKey: true },
        openrouter: { enabled: false },
        claudecode: { enabled: true, accounts: [] },
        codex: { enabled: true, accounts: [{ id: 'codex-a', hasToken: true }] },
        customEndpoints: { 'my-model': { url: 'http://x', hasApiKey: true } },
    };
    const { data } = loadStores(cfg);
    await data.performHealthCheck();
    assert.deepEqual(local(data.gateways), {
        order: ['zen', 'kimi'],
        known: cfg.knownProviders,
        kimi: { enabled: true, hasApiKey: false, oauth: true },
        zen: { enabled: true, hasApiKey: true },
        openrouter: { enabled: false, hasApiKey: false },
        claudecode: { enabled: true },
        codex: { enabled: true },
        customCount: 1,
    });
}

async function savingTheOrderRefreshesTheDiagram() {
    const cfg = { gatewayOrder: { order: ['kimi', 'zen'] }, knownProviders: ['kimi', 'zen', 'cloudcode'] };
    const { data, sandbox } = loadStores(cfg);
    await data.performHealthCheck();
    const go = sandbox.window.Components.gatewayOrder();
    go.knownProviders = cfg.knownProviders;
    go.order = ['zen', 'kimi'];
    await go.saveConfig();
    await new Promise((r) => setImmediate(r));
    assert.deepEqual(local(data.gateways.order), ['zen', 'kimi', 'cloudcode']);
}

// Codex accounts are not in /account-limits; the store fetches their pool
// snapshots for the diagram, except while the gateway is known to be off.
// The Accounts tab forces the fetch, since it lists accounts either way.
async function codexAccountsFollowTheGateway() {
    const cfg = { knownProviders: ['codex', 'cloudcode'], codex: { enabled: false } };
    const { data, urls } = loadStores(cfg, [{ id: 'codex-a', status: 'healthy' }]);
    await data.fetchCodexAccounts();
    assert.deepEqual(local(data.codexAccounts), [{ id: 'codex-a', status: 'healthy' }],
        'skipped before the gateway state was known');

    data.codexAccounts = [];
    await data.performHealthCheck();
    await data.fetchCodexAccounts();
    assert.equal(data.codexAccounts.length, 0, 'fetched while the gateway is off');
    await data.fetchCodexAccounts(true);
    assert.equal(data.codexAccounts.length, 1, 'a forced fetch was skipped');

    cfg.codex.enabled = true;
    data.codexAccounts = [];
    await data.performHealthCheck();
    await data.fetchCodexAccounts();
    assert.equal(data.codexAccounts.length, 1);
    assert.equal(urls.filter((u) => u === '/api/codex/accounts').length, 3);
}

const cases = [
    ['gateways follow the public config payload', gatewaysFollowThePublicConfig],
    ['saving the gateway order refreshes the diagram', savingTheOrderRefreshesTheDiagram],
    ['Codex accounts load unless the gateway is off', codexAccountsFollowTheGateway],
];
for (const [name, fn] of cases) {
    await fn();
    console.log('ok', name);
}
console.log(`gateways-refresh: ${cases.length}/${cases.length} passed`);
