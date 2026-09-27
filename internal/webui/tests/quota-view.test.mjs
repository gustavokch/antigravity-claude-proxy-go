// Tests for the pure quota-pool and Claude usage-window helpers in
// account-manager.js (window.QuotaView).
// Run: node internal/webui/tests/quota-view.test.mjs
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import { loadComponent, okResponse, pendingTick } from './kimi-poll-race.harness.mjs';

function loadQuotaView() {
    const sandbox = { console, window: { Components: {} } };
    sandbox.globalThis = sandbox;
    vm.createContext(sandbox);
    const url = new URL('../public/js/components/account-manager.js', import.meta.url);
    vm.runInContext(fs.readFileSync(url, 'utf8'), sandbox, { filename: url.pathname });
    return sandbox.window.QuotaView;
}

const qv = loadQuotaView();
// Values from the vm realm, copied so deepEqual compares structure only.
const local = (v) => JSON.parse(JSON.stringify(v));

function poolOrder() {
    const google = {
        '3p-weekly': { remainingFraction: 0.9, resetTime: '2026-09-30T00:00:00Z' },
        'gemini-weekly': { remainingFraction: 0.5, resetTime: '2026-09-30T00:00:00Z' },
        '3p-5h': { remainingFraction: 0.4, resetTime: '2026-09-27T12:00:00Z' },
        'gemini-5h': { remainingFraction: 0.8, resetTime: '2026-09-27T12:00:00Z', source: 'headers' },
    };
    assert.deepEqual(local(qv.sortedPools(google).map((p) => p.id)),
        ['gemini-5h', 'gemini-weekly', '3p-5h', '3p-weekly']);

    const claude = {
        'claude-weekly': { remainingFraction: 0.7 },
        'claude-5h': { remainingFraction: 0.2 },
    };
    assert.deepEqual(local(qv.sortedPools(claude).map((p) => p.id)), ['claude-5h', 'claude-weekly']);

    // Unknown families sort after the known ones; unknown windows after weekly.
    const mixed = {
        'zeta-weekly': {}, 'claude-7d': {}, 'alpha-5h': {}, 'claude-daily': {}, 'claude-5h': {},
    };
    assert.deepEqual(local(qv.sortedPools(mixed).map((p) => p.id)),
        ['claude-5h', 'claude-7d', 'claude-daily', 'alpha-5h', 'zeta-weekly']);
}

function poolTolerance() {
    assert.deepEqual(local(qv.sortedPools(undefined)), []);
    assert.deepEqual(local(qv.sortedPools(null)), []);
    assert.deepEqual(local(qv.sortedPools({ 'gemini-5h': null })), []);
    const [p] = qv.sortedPools({ 'gemini-5h': { remainingFraction: null } });
    assert.equal(p.percent, null);
    assert.equal(p.resetTime, null);
    assert.equal(p.source, null);
    assert.deepEqual({ ...qv.splitPoolId('3p-weekly') }, { family: '3p', window: 'weekly' });
    assert.deepEqual({ ...qv.splitPoolId('solo') }, { family: 'solo', window: '' });
}

function poolPercent() {
    assert.equal(qv.poolPercent({ remainingFraction: 0.456 }), 46);
    assert.equal(qv.poolPercent({ remainingFraction: 0 }), 0);
    assert.equal(qv.poolPercent({ remainingFraction: 1.3 }), 100);
    assert.equal(qv.poolPercent({ remainingFraction: -0.2 }), 0);
    assert.equal(qv.poolPercent({ remainingFraction: null }), null);
    assert.equal(qv.poolPercent({ remainingFraction: '0.5' }), null);
    assert.equal(qv.poolPercent({}), null);
    assert.equal(qv.poolPercent(null), null);

    assert.equal(qv.remainingBarClass(null), 'bg-gray-600');
    assert.equal(qv.remainingBarClass(80), 'bg-emerald-500');
    assert.equal(qv.remainingBarClass(50), 'bg-yellow-500');
    assert.equal(qv.remainingBarClass(20), 'bg-red-500');
}

function burnLevels() {
    // The usage history loads lazily on open, once per report kind, and records a
// failure instead of throwing.
async function historyLoading() {
    const { component: c, requests } = loadComponent('account-manager.js', 'accountManager');
    assert.equal(requests.length, 0, 'nothing is fetched before the history is opened');

    c.toggleHistory('acct-1');
    await pendingTick();
    assert.equal(requests.length, 1);
    assert.equal(requests[0].url, '/api/claudecode/usage?report=daily&account=acct-1');
    assert.equal(requests[0].options.headers, undefined);
    requests[0].resolve({ response: okResponse({ daily: [{ date: '2026-09-27', totalCost: 1 }], totals: { totalCost: 1 } }), newPassword: null });
    await pendingTick();
    await pendingTick();
    assert.equal(c.historyRows('acct-1').length, 1);
    assert.equal(c.historyTotals('acct-1').totalCost, 1);

    // Closing and reopening reuses the loaded report.
    c.toggleHistory('acct-1');
    c.toggleHistory('acct-1');
    await pendingTick();
    assert.equal(requests.length, 1, 'reopening refetched a loaded report');

    c.setHistoryReport('acct-1', 'weekly');
    await pendingTick();
    assert.equal(requests.length, 2);
    assert.equal(requests[1].url, '/api/claudecode/usage?report=weekly&account=acct-1');
    requests[1].resolve({ response: { ok: false, status: 404, json: async () => ({ error: 'not found' }) }, newPassword: null });
    await pendingTick();
    await pendingTick();
    const st = c.historyState('acct-1');
    assert.equal(st.loading, false);
    assert.ok(st.error.includes('not found'), `error not recorded: ${st.error}`);
    assert.equal(c.historyRows('acct-1').length, 0);
}

const cases = [
        [1001, 'HIGH'], [1000, 'MODERATE'], [501, 'MODERATE'], [500, 'NORMAL'], [0, 'NORMAL'],
    ];
    for (const [tpm, want] of cases) {
        assert.equal(qv.burnLevel({ tokensPerMinuteForIndicator: tpm }), want, `tpm ${tpm}`);
    }
    assert.equal(qv.burnLevel(null), null);
    assert.equal(qv.burnLevel({}), null);
    // The raw rate never drives the indicator.
    assert.equal(qv.burnLevel({ tokensPerMinute: 99999, tokensPerMinuteForIndicator: 10 }), 'NORMAL');
}

function projectedLevels() {
    assert.equal(qv.projectedLevel({ projectedUtilization: 1.01 }), 'exceeds');
    assert.equal(qv.projectedLevel({ projectedUtilization: 1.0 }), 'warning');
    assert.equal(qv.projectedLevel({ projectedUtilization: 0.81 }), 'warning');
    assert.equal(qv.projectedLevel({ projectedUtilization: 0.8 }), 'ok');
    // Without a projection the server's status is used, if it is a known one.
    assert.equal(qv.projectedLevel({ projectedUtilization: null, status: 'exceeds' }), 'exceeds');
    assert.equal(qv.projectedLevel({ status: 'bogus' }), null);
    assert.equal(qv.projectedLevel(null), null);

    assert.equal(qv.usedPercent(1.32), 132);
    assert.equal(qv.usedPercent(null), null);
    assert.equal(qv.barPosition(1.32), 100);
    assert.equal(qv.barPosition(0.42), 42);
    assert.equal(qv.barPosition(undefined), 0);
}

function formatting() {
    assert.equal(qv.formatUSD(12.345), '$12.35');
    assert.equal(qv.formatUSD(null), '-');
    assert.equal(qv.formatTokens(3810000), '3.81M');
    assert.equal(qv.formatTokens(1500), '1.5K');
    assert.equal(qv.formatTokens(42), '42');
    assert.equal(qv.formatTokens(undefined), '-');
}

function reports() {
    const daily = { daily: [{ date: '2026-09-26' }, { date: '2026-09-27' }], totals: {} };
    assert.equal(qv.reportRows(daily, 'daily').length, 2);
    assert.deepEqual(local(qv.reportRows(daily, 'weekly')), []);
    assert.deepEqual(local(qv.reportRows(null, 'daily')), []);
    assert.equal(qv.reportPeriod({ week: '2026-09-21' }), '2026-09-21');
    assert.equal(qv.reportPeriod({ date: '2026-09-27' }), '2026-09-27');
    assert.equal(qv.usageURL('acct 1&x', 'weekly'),
        '/api/claudecode/usage?report=weekly&account=acct%201%26x');
    assert.equal(qv.usageURL('', 'daily'), '/api/claudecode/usage?report=daily');
}

function renderer() {
    const t = (k, p) => (p && p.time ? `${k}:${p.time}` : k);
    const html = qv.renderPoolBars({
        'gemini-weekly': { remainingFraction: 0.5, resetTime: 'R2' },
        'gemini-5h': { remainingFraction: 0.25, resetTime: 'R1', source: 'headers' },
        '<b>-5h': { remainingFraction: null, source: '"x"' },
    }, { t, timeUntil: (ts) => `in-${ts}` });
    const ids = [...html.matchAll(/data-pool-id="([^"]*)"/g)].map((m) => m[1]);
    assert.deepEqual(ids, ['gemini-5h', 'gemini-weekly', '&lt;b&gt;-5h']);
    assert.ok(html.includes('25%'));
    assert.ok(html.includes('N/A'));
    assert.ok(html.includes('resetsIn:in-R1'), 'modal layout shows the reset countdown');
    assert.ok(html.includes('poolSourceHeaders'), 'source badge is translated');
    assert.ok(!html.includes('<b>'), 'pool ids are escaped');
    assert.ok(!html.includes('"x"'), 'sources are escaped');

    const compact = qv.renderPoolBars({ 'claude-5h': { remainingFraction: 0.6, resetTime: 'R' } },
        { t, timeUntil: (ts) => `in-${ts}`, compact: true });
    assert.ok(compact.includes('in-R'));
    assert.ok(compact.includes('Claude · poolWindow5h'));
    assert.equal(qv.renderPoolBars(undefined, { t }), '');
    assert.equal(qv.renderPoolBars({}, { t }), '');
}

// The usage history loads lazily on open, once per report kind, and records a
// failure instead of throwing.
async function historyLoading() {
    const { component: c, requests } = loadComponent('account-manager.js', 'accountManager');
    assert.equal(requests.length, 0, 'nothing is fetched before the history is opened');

    c.toggleHistory('acct-1');
    await pendingTick();
    assert.equal(requests.length, 1);
    assert.equal(requests[0].url, '/api/claudecode/usage?report=daily&account=acct-1');
    assert.equal(requests[0].options.headers, undefined);
    requests[0].resolve({ response: okResponse({ daily: [{ date: '2026-09-27', totalCost: 1 }], totals: { totalCost: 1 } }), newPassword: null });
    await pendingTick();
    await pendingTick();
    assert.equal(c.historyRows('acct-1').length, 1);
    assert.equal(c.historyTotals('acct-1').totalCost, 1);

    // Closing and reopening reuses the loaded report.
    c.toggleHistory('acct-1');
    c.toggleHistory('acct-1');
    await pendingTick();
    assert.equal(requests.length, 1, 'reopening refetched a loaded report');

    c.setHistoryReport('acct-1', 'weekly');
    await pendingTick();
    assert.equal(requests.length, 2);
    assert.equal(requests[1].url, '/api/claudecode/usage?report=weekly&account=acct-1');
    requests[1].resolve({ response: { ok: false, status: 404, json: async () => ({ error: 'not found' }) }, newPassword: null });
    await pendingTick();
    await pendingTick();
    const st = c.historyState('acct-1');
    assert.equal(st.loading, false);
    assert.ok(st.error.includes('not found'), `error not recorded: ${st.error}`);
    assert.equal(c.historyRows('acct-1').length, 0);
}

const cases = [
    ['pools order 5h before weekly, grouped by family', poolOrder],
    ['pools tolerate missing and null fields', poolTolerance],
    ['pool percent and bar colour', poolPercent],
    ['burn level thresholds', burnLevels],
    ['projected marker levels', projectedLevels],
    ['number formatting', formatting],
    ['usage report rows and URL', reports],
    ['shared pool-bar renderer', renderer],
    ['usage history loads lazily per report', historyLoading],
];

for (const [name, fn] of cases) {
    await fn();
    console.log('ok', name);
}
console.log(`quota-view: ${cases.length}/${cases.length} passed`);
