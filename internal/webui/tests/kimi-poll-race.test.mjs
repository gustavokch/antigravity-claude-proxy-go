// Deterministic race-harness tests for the Kimi OAuth poll loops.
// Run: node internal/webui/tests/kimi-poll-race.test.mjs
import assert from 'node:assert/strict';
import { loadComponent, okResponse, pendingTick } from './kimi-poll-race.harness.mjs';

const MODELS = 'models.js';
const MODAL = 'add-account-modal.js';

// models.js: old in-flight 'completed' resolves after cancel → re-login.
async function t1_oldSessionCompleted_models() {
    const { component: c, requests, toasts, closedDialogs, dialog } =
        loadComponent(MODELS, 'models');

    c.fetchKimiConfig = async () => {};
    c.kimiOAuth = { polling: true, sessionId: 'old', status: 'pending', error: '' };

    const loop = c._pollKimiOAuth();
    await pendingTick();                        // request for 'old' now in flight
    assert.equal(requests.length, 1);
    assert.ok(requests[0].url.includes('session_id=old'));

    // User cancels, then immediately starts a NEW login before 'old' resolves.
    c.kimiOAuth.sessionId = 'new';
    c.kimiOAuth.status = 'pending';
    c.kimiOAuth.polling = true;
    dialog('kimi_oauth_modal').showModal();     // new login's dialog

    requests[0].resolve({ response: okResponse({ status: 'completed' }), newPassword: null });
    await loop;

    // New session must be untouched.
    assert.equal(c.kimiOAuth.polling, true, 'old response killed the new poll loop');
    assert.equal(c.kimiOAuth.status, 'pending', 'old response clobbered new status');
    assert.equal(toasts.length, 0, 'toasted success for a cancelled session');
    assert.equal(closedDialogs.length, 0, 'old response closed the reopened dialog');
}

// models.js: old in-flight rejection after re-login.
async function t1_oldSessionRejection_models() {
    const { component: c, requests } = loadComponent(MODELS, 'models');

    c.fetchKimiConfig = async () => {};
    c.kimiOAuth = { polling: true, sessionId: 'old', status: 'pending', error: '' };

    const loop = c._pollKimiOAuth();
    await pendingTick();
    assert.equal(requests.length, 1);

    c.kimiOAuth.sessionId = 'new';
    c.kimiOAuth.status = 'pending';
    c.kimiOAuth.polling = true;

    requests[0].reject(new Error('boom'));
    await loop;

    assert.equal(c.kimiOAuth.status, 'pending', 'old rejection clobbered new status');
    assert.equal(c.kimiOAuth.error, '', 'old rejection stamped error on new session');
}

// add-account-modal.js: resetState() replaces this.kimiOAuth with a fresh
// object; old in-flight 'completed' resolves after reset → re-login.
async function t2_oldSessionCompleted_modal() {
    const { component: c, requests, closedDialogs, dialog } =
        loadComponent(MODAL, 'addAccountModal');

    c._refreshKimiStore = async () => {};
    c.kimiOAuth = { polling: true, sessionId: 'old', status: 'pending', error: '' };

    const loop = c._pollKimiLogin();
    await pendingTick();                        // request for 'old' now in flight
    assert.equal(requests.length, 1);
    assert.ok(requests[0].url.includes('session_id=old'));

    // resetState() replaced the object; a fresh login is pending on it.
    c.kimiOAuth = { sessionId: 'new', userCode: '', verificationUri: '', status: 'pending', error: '', polling: true };
    dialog('add_account_modal').showModal();    // new login's dialog

    requests[0].resolve({ response: okResponse({ status: 'completed' }), newPassword: null });
    await loop;

    assert.equal(c.kimiOAuth.status, 'pending', 'old response clobbered new status');
    assert.equal(c.kimiOAuth.polling, true, 'old response killed the new poll loop');
    assert.ok(!closedDialogs.includes('add_account_modal'), 'old response closed the modal for the new login');
}

// add-account-modal.js: old in-flight rejection after reset → re-login.
async function t2_oldSessionRejection_modal() {
    const { component: c, requests } = loadComponent(MODAL, 'addAccountModal');

    c._refreshKimiStore = async () => {};
    c.kimiOAuth = { polling: true, sessionId: 'old', status: 'pending', error: '' };

    const loop = c._pollKimiLogin();
    await pendingTick();
    assert.equal(requests.length, 1);

    c.kimiOAuth = { sessionId: 'new', userCode: '', verificationUri: '', status: 'pending', error: '', polling: true };

    requests[0].reject(new Error('boom'));
    await loop;

    assert.equal(c.kimiOAuth.status, 'pending', 'old rejection clobbered new status');
    assert.equal(c.kimiOAuth.error, '', 'old rejection stamped error on new session');
}

// models.js: a new login started during the cancel round-trip must not be
// stamped 'cancelled' when the stale cancel response arrives.
async function t3_staleCancelClobber() {
    const { component: c, requests } = loadComponent(MODELS, 'models');

    c.kimiOAuth = { polling: true, sessionId: 'old', status: 'pending', error: '' };
    const cancel = c.cancelKimiOAuthLogin();
    await pendingTick();                        // cancel POST in flight
    assert.equal(requests.length, 1);
    assert.ok(requests[0].url.includes('/api/kimi/auth/cancel'));

    // New login starts during the cancel round-trip.
    c.kimiOAuth.sessionId = 'new';
    c.kimiOAuth.status = 'pending';
    c.kimiOAuth.polling = true;

    requests[0].resolve({ response: okResponse({ status: 'ok' }), newPassword: null });
    await cancel;

    assert.equal(c.kimiOAuth.status, 'pending', 'stale cancel clobbered new login');
}

// models.js: pending with empty sessionId (start returned no session_id)
// must still cancel to 'cancelled' without firing the cancel POST.
async function t4_emptySessionCancel() {
    const { component: c, requests } = loadComponent(MODELS, 'models');

    c.kimiOAuth = { polling: true, sessionId: '', status: 'pending', error: '' };
    await c.cancelKimiOAuthLogin();
    assert.equal(c.kimiOAuth.status, 'cancelled', 'stuck pending with empty sessionId');
    assert.equal(requests.length, 0, 'cancel POST must not fire without sessionId');
}

// Rotated webui password must be stored even when the poll was cancelled
// in flight (guard returns before any status handling).
async function t5_passwordRotation(path, exportName, pollMethod, stub) {
    const { component: c, requests, store } = loadComponent(path, exportName);

    stub(c);
    c.kimiOAuth = { polling: true, sessionId: 's1', status: 'pending', error: '' };

    const loop = c[pollMethod]();
    await pendingTick();                        // status request in flight
    assert.equal(requests.length, 1);

    // Cancel while in flight, then resolve with a rotated password.
    c.kimiOAuth.polling = false;
    requests[0].resolve({ response: okResponse({ status: 'pending' }), newPassword: 'rotated' });
    await loop;

    assert.equal(store.webuiPassword, 'rotated', `${path}: rotated password dropped after in-flight cancel`);
}

// models.js: a stale cancel resolving after a new login reopened the dialog
// must not close it (closing fires @close → cancels the NEW session).
async function t6_staleCancelKeepsNewDialog() {
    const { component: c, requests, dialog } = loadComponent(MODELS, 'models');
    const dlg = dialog('kimi_oauth_modal');
    dlg.onclose = () => c.cancelKimiOAuthLogin();       // mirrors @close in settings.html

    c.kimiOAuth = { polling: true, sessionId: 'old', status: 'pending', error: '' };
    const cancel = c.cancelKimiOAuthLogin();            // Esc: dialog already closed
    await pendingTick();                                // cancel POST for 'old' in flight
    assert.equal(requests.length, 1);

    // New login starts and reopens the dialog during the cancel round-trip.
    c.kimiOAuth.sessionId = 'new';
    c.kimiOAuth.status = 'pending';
    c.kimiOAuth.polling = true;
    dlg.showModal();

    requests[0].resolve({ response: okResponse({ status: 'ok' }), newPassword: null });
    await cancel;

    assert.equal(dlg.open, true, 'stale cancel closed the new login dialog');
    assert.equal(c.kimiOAuth.polling, true, 'stale cancel stopped the new poll');
    assert.equal(requests.length, 1, 'stale cancel fired a cancel POST for the new session');
}

// models.js: a loop sleeping through cancel → re-login must exit, not adopt
// the new session (otherwise two loops poll it concurrently).
async function t7_orphanLoopExits_models() {
    const { component: c, requests, flushTimers } =
        loadComponent(MODELS, 'models', { manualTimers: true });
    c.fetchKimiConfig = async () => {};

    c.kimiOAuth = { polling: true, sessionId: 'old', status: 'pending', error: '' };
    c._pollKimiOAuth();                                 // loop A sleeping
    await pendingTick();

    c.kimiOAuth.polling = false;                        // cancel while A sleeps
    c.kimiOAuth.sessionId = 'new';                      // immediate re-login
    c.kimiOAuth.status = 'pending';
    c.kimiOAuth.polling = true;
    c._pollKimiOAuth();                                 // loop B sleeping
    await pendingTick();

    flushTimers();                                      // both sleeps elapse
    await pendingTick();

    assert.deepEqual(requests.map((r) => r.url),
        ['/api/kimi/auth/status?session_id=new'],
        'orphaned loop adopted the new session');
}

// add-account-modal.js: resetState() → startKimiLogin() inside the 2 s sleep.
async function t8_orphanLoopExits_modal() {
    const { component: c, requests, flushTimers } =
        loadComponent(MODAL, 'addAccountModal', { manualTimers: true });
    c._refreshKimiStore = async () => {};

    c.kimiOAuth = { polling: true, sessionId: 'old', status: 'pending', error: '' };
    c._pollKimiLogin();                                 // loop A sleeping
    await pendingTick();

    // resetState() replaced the object; a fresh login is pending on it.
    c.kimiOAuth = { sessionId: 'new', userCode: '', verificationUri: '', status: 'pending', error: '', polling: true };
    c._pollKimiLogin();                                 // loop B sleeping
    await pendingTick();

    flushTimers();
    await pendingTick();

    assert.deepEqual(requests.map((r) => r.url),
        ['/api/kimi/auth/status?session_id=new'],
        'orphaned loop adopted the new session');
}

const cases = [
    ['t1 old completed after re-login (models.js)', t1_oldSessionCompleted_models],
    ['t1 old rejection after re-login (models.js)', t1_oldSessionRejection_models],
    ['t2 old completed after re-login (add-account-modal.js)', t2_oldSessionCompleted_modal],
    ['t2 old rejection after re-login (add-account-modal.js)', t2_oldSessionRejection_modal],
    ['t3 stale cancel does not clobber new login', t3_staleCancelClobber],
    ['t4 empty sessionId still cancels to cancelled', t4_emptySessionCancel],
    ['t5 rotated password applied (models.js)', () =>
        t5_passwordRotation(MODELS, 'models', '_pollKimiOAuth', (c) => { c.fetchKimiConfig = async () => {}; })],
    ['t5 rotated password applied (add-account-modal.js)', () =>
        t5_passwordRotation(MODAL, 'addAccountModal', '_pollKimiLogin', (c) => { c._refreshKimiStore = async () => {}; })],
    ['t6 stale cancel keeps new login dialog open (models.js)', t6_staleCancelKeepsNewDialog],
    ['t7 orphaned sleeping loop exits on re-login (models.js)', t7_orphanLoopExits_models],
    ['t8 orphaned sleeping loop exits on re-login (add-account-modal.js)', t8_orphanLoopExits_modal],
];

for (const [name, fn] of cases) {
    await fn();
    console.log('ok', name);
}
console.log(`kimi-poll-race: ${cases.length}/${cases.length} passed`);
