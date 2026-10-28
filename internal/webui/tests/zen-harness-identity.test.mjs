// A Zen harness identity value typed into the panel stays pinned across
// upgrades, so clearing the field must unpin it: the save posts "" (the server
// deletes the key) and the form then shows the default the server reports.
// Fields left as loaded are not echoed, which would pin the current default.
// Run: node internal/webui/tests/zen-harness-identity.test.mjs
import assert from 'node:assert/strict';
import { loadComponent, okResponse, pendingTick } from './kimi-poll-race.harness.mjs';

const zenConfig = (harness) => ({ status: 'ok', config: {
    enabled: true,
    baseUrl: 'https://opencode.ai/zen',
    hasApiKey: true,
    keySource: 'config',
    allowlist: [],
    harness,
} });

async function clearingAPinnedFieldUnpinsIt() {
    const { component: c, requests } = loadComponent('models.js', 'models', { data: { performHealthCheck() {} } });

    const load = c.fetchZenConfig();
    requests.shift().resolve({ response: okResponse(zenConfig(
        { enabled: true, tls: true, version: '9.9.9', client: 'cli', project: 'acme' })), newPassword: null });
    await load;

    c.zenConfig.harness.version = '';
    c.zenConfig.harness.project = '  ';
    const save = c.saveZenConfig();
    await pendingTick();
    const post = requests.shift();
    assert.equal(post.options.method, 'POST');
    assert.deepEqual(JSON.parse(post.options.body).harness,
        { enabled: true, tls: true, version: '', project: '' },
        'cleared fields must post "", untouched ones nothing');
    post.resolve({ response: okResponse(zenConfig(
        { enabled: true, tls: true, version: '2.0.20', client: 'cli', project: 'global' })), newPassword: null });
    await save;

    assert.equal(c.zenConfig.harness.version, '2.0.20', 'the form must show the default the version follows again');
    assert.equal(c.zenConfig.harness.project, 'global', 'the form must show the default the project follows again');

    const again = c.saveZenConfig();
    await pendingTick();
    const second = requests.shift();
    assert.deepEqual(JSON.parse(second.options.body).harness, { enabled: true, tls: true },
        'a later save must not pin the defaults the form now shows');
    second.resolve({ response: okResponse(zenConfig(
        { enabled: true, tls: true, version: '2.0.20', client: 'cli', project: 'global' })), newPassword: null });
    await again;
}

const cases = [
    ['clearing a pinned identity field unpins it', clearingAPinnedFieldUnpinsIt],
];
for (const [name, fn] of cases) {
    await fn();
    console.log('ok', name);
}
console.log(`zen-harness-identity: ${cases.length}/${cases.length} passed`);
