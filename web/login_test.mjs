import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const loginScript = readFileSync(new URL('./login.js', import.meta.url), 'utf8');

async function submitLogin(reply) {
  const elements = {
    'login-form': { addEventListener: (_event, handler) => { elements.handler = handler; } },
    'login-password': { value: 'test-password' },
    'login-submit': { disabled: false, textContent: 'Unlock Switcher' },
    'login-error': { textContent: '' },
  };
  const requests = [];
  const stored = [];
  const redirects = [];
  vm.runInNewContext(loginScript, {
    document: { getElementById: id => elements[id] },
    fetch: async (path, options) => { requests.push({ path, options }); return reply; },
    localStorage: { setItem: (...args) => stored.push(args) },
    location: { replace: path => redirects.push(path) },
  });
  await elements.handler({ preventDefault() {} });
  return { elements, requests, stored, redirects };
}

const success = await submitLogin({ ok: true, json: async () => ({ csrf: 'csrf-for-session' }) });
assert.equal(success.requests[0].path, '/api/auth/login');
assert.equal(success.requests[0].options.credentials, 'same-origin');
assert.equal(JSON.parse(success.requests[0].options.body).password, 'test-password');
assert.equal(success.stored[0][0], 'switcher-csrf');
assert.equal(success.stored[0][1], 'csrf-for-session');
assert.deepEqual(success.redirects, ['/']);
assert.equal(success.elements['login-password'].value, '');

const failure = await submitLogin({ ok: false, json: async () => ({ error: 'Incorrect password' }) });
assert.equal(failure.elements['login-error'].textContent, 'Incorrect password');
assert.deepEqual(failure.redirects, []);
assert.deepEqual(failure.stored, []);
assert.equal(failure.elements['login-submit'].disabled, false);

// Expired sessions must navigate away from app.js, not inject a DOM overlay.
const appScript = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
const start = appScript.indexOf('function authLocked() {');
const end = appScript.indexOf('\n// fetchWithCSRF', start);
assert.ok(start >= 0 && end > start);
const removed = [];
const navigated = [];
const auth = vm.createContext({
  authState: { locked: false }, stateTimer: 123, usageTimer: 456,
  clearInterval: () => {}, clearTimeout: () => {},
  localStorage: { removeItem: key => removed.push(key) },
  location: { replace: path => navigated.push(path) },
});
vm.runInContext(appScript.slice(start, end) + '\nthis.authLocked = authLocked;', auth);
auth.authLocked();
assert.deepEqual(navigated, ['/login']);
assert.deepEqual(removed, ['switcher-csrf']);

console.log('Standalone login and expired-session navigation: OK');
