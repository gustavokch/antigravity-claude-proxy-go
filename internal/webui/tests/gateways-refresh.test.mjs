// The route diagram reads $store.data.gateways, built from GET /api/config.
// It must follow the server payload shape, and a Settings save must refresh it.
// Run: node internal/webui/tests/gateways-refresh.test.mjs
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const local = (v) => JSON.parse(JSON.stringify(v));

function loadStores(serverConfig) {
    const listeners = {};
    const stores = {};
    const sandbox = {
        console, setTimeout, clearTimeout, setInterval: () => 0, clearInterval() {},
        document: { addEventListener: (e, f) => (listeners[e] = listeners[e] || []).push(f), hidden: false },
        window: { Components: {} },
        Alpine: { store: (n, v) => (v ? (stores[n] = v) : stores[n]), data() {} },
    };
    sandbox.window.utils = {
        request: async (_url, opts = {}) => {
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
    return { data: stores.data, sandbox };
}

// Shape of config.GetPublicConfig (internal/config/config.go) as the
// diagram needs it, including Zen's env-only key (applyZenKeySource).
async function gatewaysFollowThePublicConfig() {
    const cfg = {
        gatewayOrder: { order: ['zen', 'kimi'] },
        knownProviders: ['kimi', 'zen', 'claudecode', 'openrouter', 'custom', 'cloudcode'],
        kimi: { enabled: true, oauth: { hasToken: true, maskedToken: 'tok…abcd' } },
        zen: { enabled: true, keySource: 'env', hasApiKey: true },
        openrouter: { enabled: false },
        claudecode: { enabled: true, accounts: [] },
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

const cases = [
    ['gateways follow the public config payload', gatewaysFollowThePublicConfig],
    ['saving the gateway order refreshes the diagram', savingTheOrderRefreshesTheDiagram],
];
for (const [name, fn] of cases) {
    await fn();
    console.log('ok', name);
}
console.log(`gateways-refresh: ${cases.length}/${cases.length} passed`);
