// kimi-poll-race.harness.mjs
import fs from 'node:fs';
import vm from 'node:vm';

const COMPONENTS = new URL('../public/js/components/', import.meta.url);

export function loadComponent(file, exportName, { manualTimers = false } = {}) {
    const requests = [];               // { url, options, resolve, reject }
    const toasts = [];
    const closedDialogs = [];
    const timers = [];
    const dialogs = new Map();
    const store = { webuiPassword: 'pw', t: (k) => k,
        showToast: (msg, kind) => toasts.push({ msg, kind }) };
    const dialog = (id) => {
        if (!dialogs.has(id)) {
            dialogs.set(id, {
                open: false,
                onclose: null,         // test hook mirroring the template's @close
                showModal() { this.open = true; },
                close() {
                    if (!this.open) return;   // native: no close event when already closed
                    this.open = false;
                    closedDialogs.push(id);
                    if (this.onclose) this.onclose();
                },
            });
        }
        return dialogs.get(id);
    };
    const sandbox = {
        console,
        setTimeout: (fn) => {
            if (manualTimers) timers.push(fn); else fn();
            return 0;
        },
        clearTimeout: () => {},
        Alpine: { store: (name) => (name === 'global' ? store : {}) },
        document: { getElementById: dialog, querySelectorAll: () => [] },
    };
    sandbox.window = {
        Components: {},
        utils: { request: (url, options) =>
            new Promise((resolve, reject) =>
                requests.push({ url, options, resolve, reject })) },
    };
    sandbox.globalThis = sandbox;
    vm.createContext(sandbox);
    const url = new URL(file, COMPONENTS);
    vm.runInContext(fs.readFileSync(url, 'utf8'), sandbox, { filename: url.pathname });
    const flushTimers = () => timers.splice(0).forEach((fn) => fn());
    return { component: sandbox.window.Components[exportName](),
             requests, toasts, closedDialogs, store, dialog, flushTimers };
}

export const okResponse = (data) => ({
    ok: true, status: 200,
    json: async () => data,
});
export const pendingTick = () => new Promise((r) => setImmediate(r));
