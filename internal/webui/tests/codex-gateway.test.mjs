// Tests for the ChatGPT Codex pieces of the WebUI: the add-account login
// (popup, status poll, pasted redirect URL) in add-account-modal.js, the
// Accounts tab actions in account-manager.js, and the allowlist editor in
// models.js.
// Run: node internal/webui/tests/codex-gateway.test.mjs
import assert from 'node:assert/strict';
import { loadComponent, okResponse, pendingTick } from './kimi-poll-race.harness.mjs';

const MODAL = 'add-account-modal.js';
const ACCOUNTS = 'account-manager.js';
const MODELS = 'models.js';

const local = (v) => JSON.parse(JSON.stringify(v));
const settle = async () => { for (let i = 0; i < 4; i++) await pendingTick(); };
const errorResponse = (status, error) =>
    ({ ok: false, status, json: async () => ({ status: 'error', error }) });
const reply = (req, response) => req.resolve({ response, newPassword: null });

const START = { status: 'ok', session_id: 's1', auth_url: 'https://auth.openai.com/oauth/authorize?x=1', mode: 'loopback' };
const REDIRECT = 'http://localhost:1455/auth/callback?code=c1&state=abc';

// A modal with a started login: popup open, status loop sleeping.
async function startedLogin() {
    const h = loadComponent(MODAL, 'addAccountModal', { manualTimers: true });
    let refreshed = 0;
    h.component._refreshCodexStore = async () => { refreshed++; };
    h.dialog('add_account_modal').showModal();
    const start = h.component.startCodexLogin();
    await pendingTick();
    reply(h.requests[0], okResponse(START));
    await start;
    return { ...h, c: h.component, refreshed: () => refreshed };
}

async function connectOpensThePopupAndThePollFinishes() {
    const { c, requests, popups, toasts, closedDialogs, flushTimers, refreshed } = await startedLogin();
    assert.equal(requests[0].url, '/api/codex/auth/start');
    assert.equal(requests[0].options.body, '{"mode":"loopback"}');
    assert.equal(popups.length, 1);
    assert.equal(popups[0].url, START.auth_url);
    assert.deepEqual(local(c.codexOAuth), { sessionId: 's1', authUrl: START.auth_url, mode: 'loopback', status: 'pending', error: '', polling: true });

    flushTimers();
    await pendingTick();
    assert.equal(requests[1].url, '/api/codex/auth/status?session_id=s1');
    reply(requests[1], okResponse({ status: 'ok', session: { id: 's1', status: 'completed', email: 'a@x.com' } }));
    await settle();

    assert.equal(c.codexOAuth.status, 'completed');
    assert.equal(refreshed(), 1);
    assert.deepEqual(toasts, [{ msg: 'codexAccountAdded: a@x.com', kind: 'success' }]);
    assert.equal(popups[0].closed, true, 'popup left open after the login finished');
    assert.ok(closedDialogs.includes('add_account_modal'));
}

// The browser cannot reach localhost:1455 on the proxy, so the user pastes
// the URL it landed on. The poll then sees the same login finish and must
// not announce or resync it a second time.
async function pastedUrlFinishesOnce() {
    const { c, requests, toasts, closedDialogs, flushTimers, refreshed } = await startedLogin();
    flushTimers();
    await pendingTick();
    const poll = requests[1];

    c.codexRedirectInput = `  ${REDIRECT}  `;
    const complete = c.completeCodexLogin();
    await pendingTick();
    assert.equal(requests[2].url, '/api/codex/auth/complete');
    assert.equal(requests[2].options.method, 'POST');
    assert.deepEqual(JSON.parse(requests[2].options.body), { session_id: 's1', redirect_url: REDIRECT });
    reply(requests[2], okResponse({ status: 'ok', account: { id: 'codex-a@x.com', email: 'a@x.com', planType: 'plus' } }));
    await complete;

    assert.equal(c.codexOAuth.status, 'completed');
    assert.ok(closedDialogs.includes('add_account_modal'));

    reply(poll, okResponse({ status: 'ok', session: { id: 's1', status: 'completed', email: 'a@x.com' } }));
    await settle();
    assert.equal(toasts.length, 1, 'one login announced twice');
    assert.equal(refreshed(), 1, 'one login resynced twice');
}

async function rejectedPasteKeepsTheLoginPending() {
    const { c, requests, toasts } = await startedLogin();
    c.codexRedirectInput = REDIRECT;
    const complete = c.completeCodexLogin();
    await pendingTick();
    reply(requests[1], errorResponse(400, 'state mismatch'));
    await complete;

    assert.deepEqual(toasts, [{ msg: 'state mismatch', kind: 'error' }]);
    assert.equal(c.codexOAuth.status, 'pending', 'a bad paste ended the login');
    assert.equal(c.codexOAuth.polling, true);
    assert.equal(c.codexSubmitting, false);
}

async function closingTheModalCancelsTheSession() {
    const { c, requests, popups, flushTimers } = await startedLogin();
    c.resetState();
    await pendingTick();
    assert.deepEqual(requests.slice(1).map((r) => [r.url, r.options.body]),
        [['/api/codex/auth/cancel', '{"session_id":"s1"}']]);
    assert.equal(popups[0].closed, true);
    assert.equal(c.codexOAuth.status, '');

    flushTimers();
    await pendingTick();
    assert.equal(requests.length, 2, 'the closed login kept polling');
}

async function closingDuringStartDoesNotReviveTheLogin() {
    const { component: c, requests, popups, flushTimers } =
        loadComponent(MODAL, 'addAccountModal', { manualTimers: true });
    const start = c.startCodexLogin();
    await pendingTick();
    c.resetState();
    reply(requests[0], okResponse(START));
    await start;
    flushTimers();
    await pendingTick();

    assert.equal(c.codexOAuth.status, '', 'closed modal revived a pending login');
    assert.equal(popups.length, 0, 'popup opened for a closed modal');
    assert.deepEqual(requests.slice(1).map((r) => [r.url, r.options.body]),
        [['/api/codex/auth/cancel', '{"session_id":"s1"}']], 'orphaned server session not cancelled');
}

// The modal closes while the pasted URL is being exchanged. The server keeps
// the account, so the store resyncs, without a toast for a closed dialog.
async function closingDuringPasteResyncsQuietly() {
    const { c, requests, toasts, refreshed } = await startedLogin();
    c.codexRedirectInput = REDIRECT;
    const complete = c.completeCodexLogin();
    await pendingTick();
    c.resetState();
    reply(requests[1], okResponse({ status: 'ok', account: { id: 'codex-a', email: 'a@x.com' } }));
    await complete;

    assert.equal(refreshed(), 1, 'server-committed login not reflected in the store');
    assert.equal(toasts.length, 0, 'toasted success for a closed login');
}

async function expiredSessionEndsTheLogin() {
    const { c, requests, popups, flushTimers } = await startedLogin();
    flushTimers();
    await pendingTick();
    reply(requests[1], okResponse({ status: 'ok', session: { id: 's1', status: 'expired', error: '' } }));
    await settle();

    assert.deepEqual(local(c.codexOAuth), { sessionId: 's1', authUrl: START.auth_url, mode: 'loopback', status: 'expired', error: 'codexOAuthExpired', polling: false });
    assert.equal(popups[0].closed, true);
}

function accountsHarness() {
    let reloads = 0;
    const data = {
        codexAccounts: [{ id: 'codex-a@x.com', email: 'a@x.com', enabled: true, priority: 1, status: 'healthy' }],
        fetchCodexAccounts: async () => { reloads++; },
    };
    const h = loadComponent(ACCOUNTS, 'accountManager', { data });
    return { ...h, c: h.component, row: h.component.codexAccounts[0], reloads: () => reloads };
}

// Test uses the path form /accounts/{id}/test; the id is not sent in a body.
async function testHitsTheAccountPath() {
    const { c, row, requests, toasts, reloads } = accountsHarness();
    let run = c.testCodexAccount(row);
    await pendingTick();
    assert.equal(requests[0].url, '/api/codex/accounts/codex-a%40x.com/test');
    assert.equal(requests[0].options.method, 'POST');
    assert.equal(requests[0].options.body, undefined);
    assert.equal(c.codexBusy[row.id], 'test');
    reply(requests[0], okResponse({ status: 'ok', models: 9 }));
    await run;
    assert.deepEqual(toasts.pop(), { msg: 'codexTestOk', kind: 'success' });
    assert.equal(c.codexBusy[row.id], undefined);

    run = c.testCodexAccount(row);
    await pendingTick();
    reply(requests[1], errorResponse(401, 'token revoked'));
    await run;
    assert.deepEqual(toasts.pop(), { msg: 'codexTestFailed', kind: 'error' });
    assert.equal(reloads(), 2, 'the list did not reload after a test');
}

async function accountUpdatesSendOnlyTheChange() {
    const { c, row, requests, toasts, reloads } = accountsHarness();
    for (const bad of ['', '-1', '1.5', 'x']) {
        await c.setCodexPriority(row, bad);
    }
    assert.equal(requests.length, 0, 'an invalid priority reached the server');
    assert.equal(toasts.filter((t) => t.msg === 'codexPriorityInvalid').length, 4);
    assert.equal(reloads(), 4, 'a rejected priority did not put the stored one back');

    let run = c.setCodexPriority(row, ' 3 ');
    await pendingTick();
    assert.equal(requests[0].url, '/api/codex/accounts');
    assert.deepEqual(JSON.parse(requests[0].options.body), { id: 'codex-a@x.com', priority: 3 });
    reply(requests[0], okResponse({ status: 'ok' }));
    await run;

    run = c.toggleCodexAccount(row);
    await pendingTick();
    assert.deepEqual(JSON.parse(requests[1].options.body), { id: 'codex-a@x.com', enabled: false });
    reply(requests[1], okResponse({ status: 'ok' }));
    await run;
}

function codexModels() {
    const h = loadComponent(MODELS, 'models', {
        data: { performHealthCheck() {}, fetchCodexAccounts() {} },
    });
    return { ...h, c: h.component };
}

async function effortsStayAscendingAndKnown() {
    const { c, toasts } = codexModels();
    const m = c.codexEntry({ id: 'gpt-6-sol', efforts: ['low', 'medium'], defaultEffort: 'medium' });
    c.setCodexEfforts(m, 'High, low, LOW, turbo');
    assert.deepEqual(local(m.efforts), ['low', 'high']);
    assert.equal(m.defaultEffort, '', 'a default effort the list lost was kept');
    assert.deepEqual(toasts, [{ msg: 'codexEffortsUnknown', kind: 'error' }]);

    m.defaultEffort = 'high';
    c.setCodexEfforts(m, 'xhigh, high');
    assert.deepEqual(local(m.efforts), ['high', 'xhigh']);
    assert.equal(m.defaultEffort, 'high');
}

// Load folds the two alias forms into the edited comma list and keeps fields
// the editor does not know; save sends the contract body without accounts.
async function loadAndSaveFollowTheContract() {
    const { c, requests } = codexModels();
    const load = c.loadCodexConfig();
    await pendingTick();
    assert.equal(requests[0].url, '/api/codex/config');
    reply(requests[0], okResponse({ status: 'ok', config: {
        enabled: true, baseUrl: '', accounts: [{ id: 'codex-a', hasToken: true }],
        allowlist: [{ id: 'gpt-6.1-sol', alias: 'sol', aliases: ['Sol', 'g61'], efforts: ['low', 'ultra'], future: 1, enabled: true }],
        identity: { originator: 'codex-tui' },
    } }));
    await load;
    const entry = c.codexConfig.allowlist[0];
    assert.equal(entry.alias, 'sol, g61');
    assert.deepEqual(local(entry.aliases), ['sol', 'g61']);
    assert.equal(entry.future, 1, 'an unknown field was dropped');
    assert.equal(c.codexConfig.baseUrl, 'https://chatgpt.com/backend-api/codex');

    c.codexConfig.identity.originator = '  ';
    const save = c.saveCodexConfig();
    await pendingTick();
    assert.equal(requests[1].url, '/api/codex/config');
    const body = JSON.parse(requests[1].options.body);
    assert.deepEqual(Object.keys(body).sort(), ['allowlist', 'baseUrl', 'enabled', 'identity']);
    assert.deepEqual(body.identity, { userAgent: '', originator: '' }, 'a cleared override was not sent blank');
    assert.equal(body.allowlist[0].id, 'gpt-6.1-sol');
    reply(requests[1], okResponse({ status: 'ok' }));
    await save;
    assert.equal(c.codexError, '');
}

async function importDefaultsKeepsEdits() {
    const { c, requests, toasts } = codexModels();
    c.codexConfig.allowlist = [c.codexEntry({ id: 'gpt-5.5', displayName: 'Mine', enabled: false })];
    c.importCodexDefaults();
    assert.equal(c.codexConfig.allowlist.length, 9);
    assert.equal(c.codexConfig.allowlist[0].displayName, 'Mine');
    assert.equal(c.codexConfig.allowlist[0].enabled, false);
    assert.equal(requests.length, 1, 'import did not save');

    c.importCodexDefaults();
    assert.equal(requests.length, 1, 'a no-op import saved');
    assert.equal(toasts.pop().msg, 'codexDefaultsPresent');
}

async function discoveryImportsOnlyNewPicks() {
    const { c, requests } = codexModels();
    c.codexConfig.allowlist = [c.codexEntry({ id: 'gpt-6-sol', enabled: true })];
    const fetch = c.fetchCodexModels();
    await pendingTick();
    assert.equal(requests[0].url, '/api/codex/models/fetch');
    assert.equal(requests[0].options.method, 'POST');
    reply(requests[0], okResponse({ status: 'ok', models: [
        { id: 'gpt-6-sol', displayName: 'GPT-6-Sol', contextLength: 272000, efforts: ['low'], defaultEffort: 'low' },
        { id: 'gpt-6-luna', displayName: 'GPT-6-Luna', contextLength: 272000, efforts: ['low', 'max'], defaultEffort: 'medium' },
    ] }));
    await fetch;
    assert.deepEqual(c.codexDiscovered.map((m) => [m.id, m.listed, m.selected]),
        [['gpt-6-sol', true, false], ['gpt-6-luna', false, true]]);

    c.importCodexDiscovered();
    assert.deepEqual(c.codexConfig.allowlist.map((m) => m.id), ['gpt-6-sol', 'gpt-6-luna']);
    assert.deepEqual(local(c.codexConfig.allowlist[1].efforts), ['low', 'max']);

    const failed = c.fetchCodexModels();
    await pendingTick();
    reply(requests[2], errorResponse(503, 'no enabled Codex account'));
    await failed;
    assert.equal(c.codexDiscoverError, 'no enabled Codex account');
    assert.equal(c.codexDiscovered.length, 0);
}

const cases = [
    ['connect opens the popup and the poll finishes the login', connectOpensThePopupAndThePollFinishes],
    ['a pasted redirect URL finishes the login once', pastedUrlFinishesOnce],
    ['a rejected paste keeps the login pending', rejectedPasteKeepsTheLoginPending],
    ['closing the modal cancels the session and the popup', closingTheModalCancelsTheSession],
    ['closing during start does not revive the login', closingDuringStartDoesNotReviveTheLogin],
    ['closing during the paste exchange resyncs without a toast', closingDuringPasteResyncsQuietly],
    ['an expired session ends the login', expiredSessionEndsTheLogin],
    ['Test posts to the account path', testHitsTheAccountPath],
    ['account updates validate priority and send only the change', accountUpdatesSendOnlyTheChange],
    ['efforts stay ascending and known', effortsStayAscendingAndKnown],
    ['config load and save follow the contract', loadAndSaveFollowTheContract],
    ['import defaults keeps edited entries', importDefaultsKeepsEdits],
    ['discovery imports only new picks', discoveryImportsOnlyNewPicks],
];

for (const [name, fn] of cases) {
    await fn();
    console.log('ok', name);
}
console.log(`codex-gateway: ${cases.length}/${cases.length} passed`);
