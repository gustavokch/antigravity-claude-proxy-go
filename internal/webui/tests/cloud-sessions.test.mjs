// Tests for the Settings > Cloud tab component (cloud-sessions.js).
// Run: node internal/webui/tests/cloud-sessions.test.mjs
import assert from 'node:assert/strict';
import { loadCloudSessions, okResponse, failResponse, pendingTick } from './cloud-sessions.harness.mjs';

const STATUS = { enabled: true, listen: '127.0.0.1:8092', stats: {} };
const ANSWERS = {
    '/api/config': { config: { mitm: { enabled: true } } },
    '/api/mitm/status': STATUS,
    '/api/sessions/cloud': { enabled: true, sessions: [] },
};
const ALL = ['/api/config', '/api/mitm/status', '/api/sessions/cloud'];
const POLL = ['/api/mitm/status', '/api/sessions/cloud'];

const urls = (requests) => requests.map((r) => r.url).sort();

// Answers every queued request with its canned payload.
function answerAll(requests) {
    for (const r of requests.splice(0)) {
        assert.ok(r.url in ANSWERS, `unexpected request ${r.url}`);
        r.resolve({ response: okResponse(ANSWERS[r.url]), newPassword: null });
    }
}

function openCloudTab(c) {
    c.store.activeTab = 'settings';
    c.store.settingsTab = 'cloudsessions';
    c.watchers.get('$store.global.settingsTab')();
}

const tick = (c) => [...c.intervals.values()][0]();

// x-load-view mounts every view at page load: nothing may be fetched, and no
// timer started, until the user opens Settings > Cloud.
async function t1_initOnAnotherTabDoesNothing() {
    const c = loadCloudSessions();
    c.component.init();
    assert.equal(c.requests.length, 0, 'init fetched before the tab was opened');
    assert.equal(c.intervals.size, 0, 'init started polling before the tab was opened');
}

async function t2_initOnTheCloudTabActivates() {
    const c = loadCloudSessions();
    c.store.activeTab = 'settings';
    c.store.settingsTab = 'cloudsessions';
    c.component.init();
    assert.deepEqual(urls(c.requests), ALL);
    assert.equal(c.intervals.size, 1);
}

async function t3_openingTheTabFetchesOnceAndPolls() {
    const c = loadCloudSessions();
    c.component.init();
    openCloudTab(c);
    assert.deepEqual(urls(c.requests), ALL);
    assert.equal(c.intervals.size, 1);
    answerAll(c.requests);
    await pendingTick();
    assert.equal(c.component.configuredEnabled, true);
    assert.equal(c.component.status.enabled, true);
    assert.equal(c.component.loading, false);
}

async function t4_tickSkipsConfig() {
    const c = loadCloudSessions();
    c.component.init();
    openCloudTab(c);
    answerAll(c.requests);
    await pendingTick();
    tick(c);
    assert.deepEqual(urls(c.requests), POLL);
}

async function t5_hiddenPageIsNotPolled() {
    const c = loadCloudSessions();
    c.component.init();
    openCloudTab(c);
    answerAll(c.requests);
    await pendingTick();
    c.document.hidden = true;
    tick(c);
    assert.equal(c.requests.length, 0, 'polled while the page was hidden');
    c.document.hidden = false;
    tick(c);
    assert.deepEqual(urls(c.requests), POLL);
}

async function t6_leavingTheTabStopsPolling() {
    const c = loadCloudSessions();
    c.component.init();
    openCloudTab(c);
    assert.equal(c.intervals.size, 1);

    c.store.activeTab = 'dashboard';          // another main tab; settingsTab is unchanged
    c.watchers.get('$store.global.activeTab')();
    assert.equal(c.intervals.size, 0, 'kept polling after leaving Settings');

    openCloudTab(c);
    assert.equal(c.intervals.size, 1);
    c.store.settingsTab = 'ui';               // another Settings tab
    c.watchers.get('$store.global.settingsTab')();
    assert.equal(c.intervals.size, 0, 'kept polling after leaving the Cloud tab');
}

async function t7_refreshIsSingleFlight() {
    const c = loadCloudSessions();
    c.component.init();
    openCloudTab(c);                          // three requests now in flight
    tick(c);
    assert.equal(c.requests.length, ALL.length, 'a tick overlapped an in-flight refresh');

    for (const r of c.requests.splice(0)) r.reject(new Error('boom'));
    await pendingTick();
    assert.equal(c.component.error, 'boom');
    tick(c);
    assert.deepEqual(urls(c.requests), POLL, 'a failed refresh left the component stuck in flight');
}

async function t8_unauthorizedStopsPolling() {
    const c = loadCloudSessions();
    c.component.init();
    openCloudTab(c);
    for (const r of c.requests.splice(0)) {
        const response = r.url === '/api/mitm/status' ? failResponse(401) : okResponse(ANSWERS[r.url]);
        r.resolve({ response, newPassword: null });
    }
    await pendingTick();
    assert.equal(c.intervals.size, 0, 'polling continued after the password prompt was dismissed');
    assert.equal(c.component.error, 'HTTP 401');

    c.component.activate();                   // the Refresh button retries and resumes polling
    assert.equal(c.intervals.size, 1);
}

async function t9_failedToggleRevertsTheCheckbox() {
    const c = loadCloudSessions();
    const input = { checked: true };
    const saving = c.component.setEnabled(input);
    assert.equal(c.requests.length, 1);
    assert.equal(c.requests[0].url, '/api/config');
    assert.equal(c.requests[0].options.method, 'POST');
    assert.equal(c.requests[0].options.body, JSON.stringify({ mitm: { enabled: true } }));
    c.requests[0].resolve({
        response: failResponse(400, { error: 'mitm listen "" must be host:port' }),
        newPassword: null,
    });
    await saving;
    assert.equal(input.checked, false, 'the checkbox kept a state the server rejected');
    assert.equal(c.component.configuredEnabled, false);
    assert.equal(c.component.saving, false);
    assert.deepEqual(c.toasts.map((t) => t.kind), ['error']);
}

async function t10_savedToggleKeepsTheCheckbox() {
    const c = loadCloudSessions();
    const input = { checked: true };
    const saving = c.component.setEnabled(input);
    c.requests[0].resolve({ response: okResponse({ status: 'ok' }), newPassword: null });
    await saving;
    assert.equal(input.checked, true);
    assert.equal(c.component.configuredEnabled, true);
    assert.deepEqual(c.toasts.map((t) => t.kind), ['success']);
}

async function t11_downloadRevokesAfterTheClick() {
    const c = loadCloudSessions();
    const done = c.component.downloadCA();
    assert.equal(c.requests[0].url, '/api/mitm/ca.pem');
    c.requests[0].resolve({ response: { ok: true, status: 200, blob: async () => 'PEM' }, newPassword: null });
    await done;
    assert.equal(c.links.length, 1);
    const [link] = c.links;
    assert.ok(link.appended && link.clicked && link.removed, 'the anchor was not attached, clicked and removed');
    assert.equal(link.download, 'antigravity-proxy-mitm-ca.pem');
    assert.deepEqual(c.revoked, [], 'the object URL was revoked before the browser started the download');
    c.timeouts.splice(0).forEach((fn) => fn());
    assert.deepEqual(c.revoked, ['blob:ca']);
}

async function t12_envSnippetBypassesTheProxyForTheGateway() {
    const c = loadCloudSessions();
    assert.ok(c.component.envSnippet().includes('NO_PROXY=127.0.0.1,localhost'));
    assert.ok(c.component.envSnippet().includes('HTTPS_PROXY=http://127.0.0.1:8092'));
    c.component.status = { enabled: true, listen: '127.0.0.1:9000' };
    assert.ok(c.component.envSnippet().includes('HTTPS_PROXY=http://127.0.0.1:9000'));
}

const cases = [
    ['t1 init on another tab fetches nothing and starts no timer', t1_initOnAnotherTabDoesNothing],
    ['t2 init on the Cloud tab activates', t2_initOnTheCloudTabActivates],
    ['t3 opening the tab fetches once and polls', t3_openingTheTabFetchesOnceAndPolls],
    ['t4 poll ticks skip /api/config', t4_tickSkipsConfig],
    ['t5 hidden page is not polled', t5_hiddenPageIsNotPolled],
    ['t6 leaving the tab stops polling', t6_leavingTheTabStopsPolling],
    ['t7 refresh is single-flight', t7_refreshIsSingleFlight],
    ['t8 a 401 stops polling', t8_unauthorizedStopsPolling],
    ['t9 failed toggle reverts the checkbox', t9_failedToggleRevertsTheCheckbox],
    ['t10 saved toggle keeps the checkbox', t10_savedToggleKeepsTheCheckbox],
    ['t11 CA download revokes after the click', t11_downloadRevokesAfterTheClick],
    ['t12 env snippet bypasses the proxy for the gateway', t12_envSnippetBypassesTheProxyForTheGateway],
];

for (const [name, fn] of cases) {
    await fn();
    console.log('ok', name);
}
console.log(`cloud-sessions: ${cases.length}/${cases.length} passed`);
