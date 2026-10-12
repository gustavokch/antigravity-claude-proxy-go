// Tests for importCCDefaults limits in models.js.
// Run: node internal/webui/tests/import-cc-defaults.test.mjs
import assert from 'node:assert/strict';
import { loadComponent } from './kimi-poll-race.harness.mjs';

function importedDefaultsHaveClaude55Limits() {
    const { component: c, requests } = loadComponent('models.js', 'models');
    c.importCCDefaults();

    // Verify in-memory allowlist state:
    const byID = new Map(c.ccConfig.allowlist.map((m) => [m.id, m]));
    const sonnet = byID.get('claude-sonnet-5-5');
    const opus = byID.get('claude-opus-5-5');

    assert.ok(sonnet, 'claude-sonnet-5-5 must be in imported allowlist');
    assert.equal(sonnet.contextLength, 1000000, 'sonnet contextLength');
    assert.equal(sonnet.maxOutputTokens, 128000, 'sonnet maxOutputTokens');

    assert.ok(opus, 'claude-opus-5-5 must be in imported allowlist');
    assert.equal(opus.contextLength, 1000000, 'opus contextLength');
    assert.equal(opus.maxOutputTokens, 128000, 'opus maxOutputTokens');

    // Verify saveCCConfig posted the limits over the wire:
    assert.equal(requests.length, 1, 'saveCCConfig must issue 1 POST');
    const body = JSON.parse(requests[0].options.body);
    const wireByID = new Map(body.allowlist.map((m) => [m.id, m]));
    assert.equal(wireByID.get('claude-sonnet-5-5').contextLength, 1000000);
    assert.equal(wireByID.get('claude-sonnet-5-5').maxOutputTokens, 128000);
    assert.equal(wireByID.get('claude-opus-5-5').contextLength, 1000000);
    assert.equal(wireByID.get('claude-opus-5-5').maxOutputTokens, 128000);
}

const cases = [
    ['imported defaults have Claude 5.5 limits', importedDefaultsHaveClaude55Limits],
];

for (const [name, fn] of cases) {
    await fn();
    console.log('ok', name);
}
console.log(`import-cc-defaults: ${cases.length}/${cases.length} passed`);
