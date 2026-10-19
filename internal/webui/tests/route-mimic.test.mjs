// Tests for the shell's route diagram (route-mimic.js): account aspects must
// agree with the Accounts view, and signals open the matching view.
// Run: node internal/webui/tests/route-mimic.test.mjs
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

function loadMimic(data) {
    const sandbox = { console, window: { Components: {} } };
    sandbox.globalThis = sandbox;
    vm.createContext(sandbox);
    for (const file of ['account-manager.js', 'route-mimic.js']) {
        const url = new URL(`../public/js/components/${file}`, import.meta.url);
        vm.runInContext(fs.readFileSync(url, 'utf8'), sandbox, { filename: url.pathname });
    }
    const c = sandbox.window.Components.routeMimic();
    c.$store = { data, global: { t: (k) => k, activeTab: 'dashboard', settingsTab: 'ui', accountsTab: 'google' } };
    return c;
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

function signalsOpenTheMatchingAccountsTab() {
    const c = loadMimic({ accounts: [] });
    c.open('claudecode');
    assert.equal(c.$store.global.activeTab, 'accounts');
    assert.equal(c.$store.global.accountsTab, 'claudecode');
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
    ['gateway signals open the matching accounts tab', signalsOpenTheMatchingAccountsTab],
];
for (const [name, fn] of cases) {
    await fn();
    console.log('ok', name);
}
console.log(`route-mimic: ${cases.length}/${cases.length} passed`);
