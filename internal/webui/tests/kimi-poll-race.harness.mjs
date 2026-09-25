// kimi-poll-race.harness.mjs
import fs from 'node:fs';
import vm from 'node:vm';

export function loadComponent(path, exportName) {
    const requests = [];               // { url, options, resolve, reject }
    const toasts = [];
    const closedDialogs = [];
    const store = { webuiPassword: 'pw', t: (k) => k,
        showToast: (msg, kind) => toasts.push({ msg, kind }) };
    const sandbox = {
        console,
        setTimeout: (fn) => { fn(); return 0; },   // instant 2 s sleep
        clearTimeout: () => {},
        Alpine: { store: (name) => (name === 'global' ? store : {}) },
        document: { getElementById: (id) => ({
            open: true,
            showModal() {},
            close() { closedDialogs.push(id); this.open = false; },
        }) },
    };
    sandbox.window = {
        Components: {},
        utils: { request: (url, options) =>
            new Promise((resolve, reject) =>
                requests.push({ url, options, resolve, reject })) },
    };
    sandbox.globalThis = sandbox;
    vm.createContext(sandbox);
    vm.runInContext(fs.readFileSync(path, 'utf8'), sandbox, { filename: path });
    return { component: sandbox.window.Components[exportName](),
             requests, toasts, closedDialogs, store };
}

export const okResponse = (data) => ({
    ok: true, status: 200,
    json: async () => data,
});
export const pendingTick = () => new Promise((r) => setImmediate(r));
