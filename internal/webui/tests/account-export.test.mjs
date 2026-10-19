// Export must not fetch credentials until the user confirms in the dialog.
// Run: node internal/webui/tests/account-export.test.mjs
import assert from 'node:assert/strict';
import { loadComponent, okResponse, pendingTick } from './kimi-poll-race.harness.mjs';

const MODAL = 'export_accounts_modal';
const VIEWS = { views: ['accounts.html'] };

async function openDoesNotFetch() {
    const { component: c, requests, dialog } = loadComponent('account-manager.js', 'accountManager', VIEWS);
    c.exportAccounts();
    await pendingTick();
    assert.equal(requests.length, 0, 'opening the dialog must not request the export');
    assert.equal(dialog(MODAL).open, true, 'export dialog opens');
}

async function failedExportClosesAndReports() {
    const { component: c, requests, toasts, dialog, closedDialogs } =
        loadComponent('account-manager.js', 'accountManager', VIEWS);
    c.exportAccounts();
    const done = c.executeExport();
    assert.equal(c.exporting, true, 'busy while the request runs');
    assert.equal(requests.length, 1);
    assert.equal(requests[0].url, '/api/accounts/export');
    requests[0].resolve({ response: { ok: false, status: 500, json: async () => ({ error: 'boom' }) } });
    await done;
    assert.equal(c.exporting, false);
    assert.equal(dialog(MODAL).open, false);
    assert.ok(closedDialogs.includes(MODAL));
    assert.equal(toasts.length, 1);
    assert.equal(toasts[0].kind, 'error');
    assert.match(toasts[0].msg, /^exportFailed: boom$/);
}

async function confirmedExportDownloads() {
    const { component: c, requests, toasts, dialog, downloads } =
        loadComponent('account-manager.js', 'accountManager', VIEWS);
    c.exportAccounts();
    const done = c.executeExport();
    requests[0].resolve({ response: okResponse([{ email: 'a@x' }, { email: 'b@x' }]) });
    await done;
    assert.equal(requests.length, 1);
    assert.equal(downloads.length, 1, 'one file is downloaded');
    assert.match(downloads[0].download, /^antigravity-accounts-\d{4}-\d{2}-\d{2}\.json$/);
    assert.equal(toasts.length, 1);
    assert.equal(toasts[0].kind, 'success');
    assert.equal(c.exporting, false);
    assert.equal(dialog(MODAL).open, false, 'dialog closes after the download');
}

const cases = [
    ['export opens a confirmation and does not fetch', openDoesNotFetch],
    ['confirmed export that fails closes the dialog and reports', failedExportClosesAndReports],
    ['confirmed export downloads once and closes the dialog', confirmedExportDownloads],
];
for (const [name, fn] of cases) {
    await fn();
    console.log('ok', name);
}
console.log(`account-export: ${cases.length}/${cases.length} passed`);
