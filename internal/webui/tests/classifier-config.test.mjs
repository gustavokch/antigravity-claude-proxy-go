// Tests for the classifier settings component (classifier-config.js).
// Run: node internal/webui/tests/classifier-config.test.mjs
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

function loadClassifierConfig() {
    const sandbox = { console, window: { Components: {} }, document: { addEventListener() {} } };
    sandbox.globalThis = sandbox;
    vm.createContext(sandbox);
    const url = new URL('../public/js/components/classifier-config.js', import.meta.url);
    vm.runInContext(fs.readFileSync(url, 'utf8'), sandbox, { filename: url.pathname });
    return sandbox.window.Components.classifierConfig();
}

// The backend form has no timeout field, so a backend added in it must not
// carry a timeout the operator never chose: an explicit 20000 overrides the
// shorter default a laya or jev backend gets from the server, and a stalled
// call would hold the permission prompt for 20 seconds.
function addedBackendLeavesTheTimeoutUnset() {
    const component = loadClassifierConfig();
    component.addBackend();
    const [backend] = Object.values(component.config.backends);
    assert.equal(backend.timeoutMs, 0);
}

const cases = [
    ['a backend added in the form leaves the timeout unset', addedBackendLeavesTheTimeoutUnset],
];

for (const [name, fn] of cases) {
    await fn();
    console.log('ok', name);
}
console.log(`classifier-config: ${cases.length}/${cases.length} passed`);
