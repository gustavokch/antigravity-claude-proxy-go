// A Zen save must start from the persisted config. When the page opens
// directly on Settings → Models, init() must load the Zen config; otherwise
// the first save posts the form defaults (hasApiKey:false, empty allowlist)
// and the server drops the stored API key.
// Run: node internal/webui/tests/zen-config-load.test.mjs
import assert from 'node:assert/strict';
import { loadComponent, okResponse, pendingTick } from './kimi-poll-race.harness.mjs';

async function importAfterDirectLoadKeepsKeyAndAllowlist() {
    const { component: c, requests } = loadComponent('models.js', 'models');
    c.$store = { global: { activeTab: 'settings', settingsTab: 'models' } };
    c.$watch = () => {};
    c.$nextTick = () => {};
    c.loadCCConfig = () => {};

    c.init();
    await pendingTick();
    const zenGet = requests.find((r) => r.url === '/api/zen/config' && !r.options?.method);
    assert.ok(zenGet, 'init() on the models tab did not load the Zen config');
    zenGet.resolve({ response: okResponse({ status: 'ok', config: {
        enabled: true,
        baseUrl: 'https://opencode.ai/zen',
        hasApiKey: true,
        keySource: 'config',
        allowlist: [{ id: 'glm-5.3-flash-free', alias: 'glm', enabled: true }],
        harness: { enabled: true, tls: true },
        fallbackProxy: 'socks5h://192.0.2.10:1080',
    } }), newPassword: null });
    await pendingTick();

    c.zenDiscovered = [{ id: 'beam-free', displayName: 'beam-free', usable: true, selected: true }];
    c.importZenDiscovered();
    await pendingTick();

    const save = requests.find((r) => r.url === '/api/zen/config' && r.options?.method === 'POST');
    assert.ok(save, 'import did not save');
    const body = JSON.parse(save.options.body);
    assert.equal(body.hasApiKey, true, 'save would drop the persisted API key');
    assert.deepEqual(body.allowlist.map((m) => [m.id, m.alias]),
        [['glm-5.3-flash-free', 'glm'], ['beam-free', '']],
        'save replaced the persisted allowlist');
    // The form sends the loaded proxy back; an empty value would clear it.
    assert.equal(body.fallbackProxy, 'socks5h://192.0.2.10:1080', 'save would drop the fallback proxy');
}

// The section never loaded (the GET failed): a save would post the form
// defaults and the server would delete the key, the allowlist and the proxy.
async function saveBeforeLoadPostsNothing() {
    const { component: c, requests, toasts } = loadComponent('models.js', 'models');
    c.$store = { global: { activeTab: 'settings', settingsTab: 'models' } };
    c.$watch = () => {};
    c.$nextTick = () => {};
    c.loadCCConfig = () => {};

    c.init();
    await pendingTick();
    const zenGet = requests.find((r) => r.url === '/api/zen/config' && !r.options?.method);
    zenGet.resolve({ response: { ok: false, status: 500, json: async () => ({}) }, newPassword: null });
    await pendingTick();

    c.zenDiscovered = [{ id: 'beam-free', displayName: 'beam-free', usable: true, selected: true }];
    c.importZenDiscovered();
    await pendingTick();

    const save = requests.find((r) => r.url === '/api/zen/config' && r.options?.method === 'POST');
    assert.equal(save, undefined, 'a save before the Zen config loaded would delete the stored key');
    assert.ok(toasts.some((t) => t.kind === 'error'), 'the refused save must tell the user');
}

const cases = [
    ['import after a direct models-tab load keeps the key and allowlist', importAfterDirectLoadKeepsKeyAndAllowlist],
    ['a save before the Zen config loaded posts nothing', saveBeforeLoadPostsNothing],
];
for (const [name, fn] of cases) {
    await fn();
    console.log('ok', name);
}
console.log(`zen-config-load: ${cases.length}/${cases.length} passed`);
