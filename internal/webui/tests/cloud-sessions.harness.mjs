// cloud-sessions.harness.mjs
import fs from 'node:fs';
import vm from 'node:vm';

const COMPONENT = new URL('../public/js/components/cloud-sessions.js', import.meta.url);

// Loads the Cloud tab component into a sandbox with a fake Alpine store, a
// manual interval and timeout clock, a mutable document.hidden and a request
// queue that the test answers by hand.
export function loadCloudSessions() {
    const requests = [];               // { url, options, resolve, reject }
    const toasts = [];
    const intervals = new Map();       // id -> tick function
    const timeouts = [];
    const watchers = new Map();        // $watch expression -> callback
    const revoked = [];
    const links = [];
    let nextTimer = 1;
    const store = {
        webuiPassword: 'pw', activeTab: 'dashboard', settingsTab: 'ui',
        t: (key) => key,
        showToast: (msg, kind) => toasts.push({ msg, kind }),
    };
    const document = {
        hidden: false,
        body: { appendChild: (el) => { el.appended = true; } },
        createElement: () => {
            const link = {
                clicked: false, appended: false, removed: false,
                click() { this.clicked = true; },
                remove() { this.removed = true; },
            };
            links.push(link);
            return link;
        },
    };
    const sandbox = {
        console: { ...console, error() {} },   // the component logs failures it handles
        setInterval: (fn) => { const id = nextTimer++; intervals.set(id, fn); return id; },
        clearInterval: (id) => { intervals.delete(id); },
        setTimeout: (fn) => { timeouts.push(fn); return 0; },
        Alpine: { store: (name) => (name === 'global' ? store : {}) },
        document,
        URL: { createObjectURL: () => 'blob:ca', revokeObjectURL: (url) => revoked.push(url) },
        navigator: { clipboard: { writeText: async () => {} } },
    };
    sandbox.window = {
        Components: {},
        utils: { request: (url, options) =>
            new Promise((resolve, reject) => requests.push({ url, options, resolve, reject })) },
    };
    sandbox.globalThis = sandbox;
    vm.createContext(sandbox);
    vm.runInContext(fs.readFileSync(COMPONENT, 'utf8'), sandbox, { filename: COMPONENT.pathname });
    const component = sandbox.window.Components.cloudSessions();
    component.$watch = (expression, callback) => watchers.set(expression, callback);
    return { component, store, document, requests, toasts, intervals, timeouts, watchers, revoked, links };
}

export const okResponse = (data) => ({ ok: true, status: 200, json: async () => data });
export const failResponse = (status, data = {}) => ({ ok: false, status, json: async () => data });
export const pendingTick = () => new Promise((resolve) => setImmediate(resolve));
