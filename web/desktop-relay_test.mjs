import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import { createDesktopRelay, desktopRelayLoopback, desktopRelayActivity } from './desktop-relay.js';

const disabled = () => ({
  status: { enabled: false, listening: false, address: '127.0.0.1:9443', ca_path: '/fixture/relay-ca.pem', condition: 'disabled', validation: 'fixture_tested' },
  scopes: [], sessions: [],
});
const accounts = [
  { id: 'a', provider: 'claude', email: 'a@example.test' },
  { id: 'b', provider: 'claude', email: 'b@example.test' },
  { id: 'codex', provider: 'codex', email: 'codex@example.test' },
];
const ready = () => ({ ...disabled(),
  status: { ...disabled().status, enabled: true, listening: true, condition: 'ready' },
  scopes: [{ id: 'scope/a', label: 'Future task' }, { id: 'peer-scope', label: 'Peer' }],
  sessions: [
    { scope_id: 'scope/a', session_id: 'session/a', account_id: 'a', revision: 7, last_seen: 1790942400, requests: 4, in_flight: 1, model: 'same-model' },
    { scope_id: 'peer-scope', session_id: 'session/a', account_id: 'a', revision: 2, last_seen: 1790942400, requests: 3, in_flight: 0, model: 'same-model' },
  ],
});
function fixture(handler = async () => disabled(), dependencies = {}) {
  const calls = [], copies = [], paints = [];
  const context = { loopback: true, authKind: '', controllerKey: 'fixture-control-key', locked: false, loggingOut: false };
  const relay = createDesktopRelay({
    api: (path, options = {}) => { calls.push({ path, options }); return handler(path, options); },
    getContext: () => context, getAccounts: () => accounts,
    copy: async text => copies.push(text), paint: html => paints.push(html), now: () => Date.parse('2026-10-03T12:00:00Z'), ...dependencies,
  });
  return { relay, calls, copies, paints, context };
}
const deferred = () => {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
};

test('opening disabled relay reads status on demand without starting or creating a scope', async () => {
  const { relay, calls } = fixture();
  assert.deepEqual(calls, []);
  await relay.setActive(true);
  assert.deepEqual(calls.map(c => c.path), ['/api/desktop-relay']);
  assert.equal(calls[0].options.method, 'GET');
  assert.equal(calls[0].options.controllerKey, 'fixture-control-key');
  assert.match(relay.html(), /<h2>Claude Desktop<\/h2>/);
  assert.match(relay.html(), /Account switching/);
  assert.match(relay.html(), /Disabled/);
  assert.doesNotMatch(relay.html(), /Fixture-tested/);
  assert.match(relay.html(), /Subscription billing is not verified/);
  assert.doesNotMatch(relay.html(), /fixture-control-key|codex@example.test/);
  await relay.setActive(true);
  assert.equal(calls.length, 1, 'repainting Settings must not fetch again');
});

test('task rendering escapes all visible identities and ignores admission secrets from status', async () => {
  const body = ready();
  body.scopes[0].label = '<img src=x onerror=alert(1)>';
  body.sessions[0].session_id = 'session/"<script>x</script>';
  body.sessions[0].model = '<svg onload=alert(1)>';
  body.scopes[0].proxy_url = 'scope-admission-secret';
  body.status.env = { HTTPS_PROXY: 'status-admission-secret' };
  body.env = { HTTPS_PROXY: 'top-level-admission-secret' };
  const { relay } = fixture(async () => body);
  accounts.push({ id: 'c"<', provider: 'claude', email: '"<img src=x>@example.test' });
  try {
    await relay.setActive(true);
    const html = relay.html();
    assert.match(html, /Ready/);
    assert.match(html, /&lt;img src=x onerror=alert\(1\)&gt;/);
    assert.match(html, /title="session\/&quot;&lt;script&gt;x&lt;\/script&gt;"/);
    assert.match(html, /&lt;svg onload=alert\(1\)&gt;/);
    assert.match(html, /value="c&quot;&lt;"/);
    assert.match(html, /&quot;&lt;img src=x&gt;@example.test/);
    assert.match(html, /1 request in progress/);
    assert.match(html, /Existing streams finish with their previous account/);
    assert.match(html, /Unidentified child requests keep caller auth/);
    assert.doesNotMatch(html, /<img|<script|<svg|admission-secret|codex@example.test|prompt/);
    assert.match(html, /https:\/\/github.com\/00xmario\/switcher\/blob\/main\/docs\/desktop-reference-map.md/);
  } finally { accounts.pop(); }
});

test('task A to B uses scoped revision CAS and leaves its same-session same-model peer unchanged', async () => {
  const state = ready(), completion = deferred();
  const { relay, calls } = fixture(async (path, options) => {
    if (options.method === 'GET') return state;
    assert.equal(path, '/api/desktop-relay/scopes/scope%2Fa/sessions/session%2Fa/account');
    assert.deepEqual(JSON.parse(options.body), { account_id: 'b', revision: 7 });
    await completion.promise;
    state.sessions[0] = { ...state.sessions[0], account_id: 'b', revision: 8 };
    return {};
  });
  await relay.setActive(true);
  const switchTask = relay.useTask('scope/a', 'session/a', 'b');
  const duplicate = relay.useTask('scope/a', 'session/a', 'b');
  assert.equal(calls.filter(c => c.options.method === 'POST').length, 1);
  assert.match(relay.html(), /data-relay-action="use"[^>]*disabled[^>]*>Switching/);
  completion.resolve();
  await Promise.all([switchTask, duplicate]);
  assert.match(relay.html(), /Selected: b@example.test/);
  assert.match(relay.html(), /Selected: a@example.test/);
  assert.equal(state.sessions[1].revision, 2);
  assert.equal(state.sessions[1].in_flight, 0);
  assert.equal(state.sessions[0].in_flight, 1, 'existing streams are still reported on the task');
  assert.deepEqual(calls.map(c => c.options.method), ['GET', 'POST', 'GET']);
  assert.ok(calls.every(c => c.path.startsWith('/api/desktop-relay')));
  await relay.useTask('scope/a', 'session/a', 'codex');
  await relay.useTask('unknown', 'session/a', 'b');
  assert.equal(calls.length, 3, 'only stored Claude accounts and observed scope/session pairs can be selected');
});

test('CAS conflict refreshes the generation, explains it, and never replays the switch', async () => {
  const state = ready();
  const { relay, calls } = fixture(async (_path, options) => {
    if (options.method === 'GET') return state;
    state.sessions[0] = { ...state.sessions[0], account_id: '', revision: 9 };
    throw Object.assign(new Error('raw-server-secret'), { status: 409 });
  });
  await relay.setActive(true);
  assert.equal(await relay.useTask('scope/a', 'session/a', 'b'), false);
  assert.deepEqual(calls.map(c => c.options.method), ['GET', 'POST', 'GET']);
  assert.match(relay.html(), /generation changed/i);
  assert.match(relay.html(), /Default: Claude sign-in/);
  assert.doesNotMatch(relay.html(), /raw-server-secret/);
  assert.ok(calls.every(c => !c.path.includes('/activate')));
});

test('restoring caller auth sends DELETE with the latest revision and reports generic failures', async () => {
  const state = ready();
  let fail = false;
  const { relay, calls } = fixture(async (path, options) => {
    if (options.method === 'GET') return state;
    assert.equal(path, '/api/desktop-relay/scopes/scope%2Fa/sessions/session%2Fa/account');
    assert.equal(options.method, 'DELETE');
    assert.deepEqual(JSON.parse(options.body), { revision: 7 });
    if (fail) throw Object.assign(new Error('<script>raw-server-secret</script>'), { status: 500 });
    state.sessions[0] = { ...state.sessions[0], account_id: '', revision: 8 };
    return {};
  });
  await relay.setActive(true);
  assert.equal(await relay.useTask('scope/a', 'session/a', ''), true);
  assert.match(relay.html(), /Default: Claude sign-in/);
  assert.equal(state.sessions[1].account_id, 'a');
  state.sessions[0].revision = 7;
  await relay.refresh();
  fail = true;
  assert.equal(await relay.useTask('scope/a', 'session/a', ''), false);
  assert.match(relay.html(), /Could not change the conversation account/);
  assert.doesNotMatch(relay.html(), /raw-server-secret|<script>/);
  assert.equal(calls.filter(c => c.options.method === 'DELETE').length, 2, 'failed changes are not retried');
});

test('starting is explicit and duplicate lifecycle controls share a pending operation', async () => {
  const state = disabled(), completion = deferred();
  const { relay, calls } = fixture(async (path, options) => {
    if (options.method === 'GET') return state;
    assert.equal(path, '/api/desktop-relay/start');
    assert.deepEqual(JSON.parse(options.body), {});
    await completion.promise;
    state.status.enabled = state.status.listening = true;
    state.status.condition = 'ready';
    return {};
  });
  await relay.setActive(true);
  const starting = relay.start();
  await relay.start();
  await relay.refresh();
  assert.deepEqual(calls.map(c => c.options.method), ['GET', 'POST']);
  assert.match(relay.html(), /data-relay-action="start"[^>]*disabled/);
  completion.resolve();
  await starting;
  assert.match(relay.html(), /Ready/);
});

test('new scope reveals setup only for that request and copying clears its ephemeral credentials', async () => {
  const state = ready();
  const { relay, calls, copies } = fixture(async (path, options) => {
    if (options.method === 'GET') return state;
    assert.equal(path, '/api/desktop-relay/scopes');
    assert.deepEqual(JSON.parse(options.body), { label: 'Future Desktop task' });
    state.scopes.push({ id: 'new-profile', label: 'Future Desktop task' });
    return { id: 'new-profile', label: 'Future Desktop task', proxy_url: 'http://scope:one-time-secret@127.0.0.1:9443',
      ca_path: '/fixture/public-ca.pem', env: { HTTPS_PROXY: 'http://scope:one-time-secret@127.0.0.1:9443', NODE_EXTRA_CA_CERTS: '/fixture/public-ca.pem', IGNORE_SECRET: 'other-secret' } };
  });
  await relay.setActive(true);
  assert.doesNotMatch(relay.html(), /one-time-secret|data-relay-action="copy-setup"/);
  assert.equal(await relay.createScope('Future Desktop task'), true);
  assert.match(relay.html(), /one-time-secret/);
  assert.match(relay.html(), /NODE_EXTRA_CA_CERTS/);
  assert.match(relay.html(), /before launching a future/);
  assert.match(relay.html(), /Existing workers are not retrofitted/);
  assert.match(relay.html(), /Standalone CLI peers.*inherit/s);
  assert.doesNotMatch(relay.html(), /other-secret|IGNORE_SECRET/);
  await relay.refresh();
  await relay.copySetup();
  assert.deepEqual(JSON.parse(copies[0]), { HTTPS_PROXY: 'http://scope:one-time-secret@127.0.0.1:9443', NODE_EXTRA_CA_CERTS: '/fixture/public-ca.pem' });
  assert.doesNotMatch(relay.html(), /one-time-secret|data-relay-action="copy-setup"/);
  await relay.copySetup();
  assert.equal(copies.length, 1);
  assert.equal(calls.filter(c => c.options.method === 'POST').length, 1);
});

test('deleting a scope uses its encoded route and removes only that scope and its tasks', async () => {
  const state = ready();
  const { relay, calls } = fixture(async (path, options) => {
    if (options.method === 'GET') return state;
    assert.equal(path, '/api/desktop-relay/scopes/scope%2Fa');
    assert.equal(options.method, 'DELETE');
    state.scopes = state.scopes.filter(s => s.id !== 'scope/a');
    state.sessions = state.sessions.filter(s => s.scope_id !== 'scope/a');
    return {};
  });
  await relay.setActive(true);
  assert.equal(await relay.deleteScope('scope/a'), true);
  assert.doesNotMatch(relay.html(), /data-relay-scope="scope\/a"/);
  assert.match(relay.html(), /data-relay-scope="peer-scope"/);
  assert.match(relay.html(), /Profile revoked/);
  await relay.deleteScope('not-observed');
  assert.equal(calls.length, 3);
});

test('Stop shows active counts, deduplicates confirmation, and explains a backend Busy rejection', async () => {
  const approval = deferred(), state = ready(), confirmations = [];
  const { relay, calls } = fixture(async (_path, options) => {
    if (options.method === 'GET') return state;
    throw Object.assign(new Error('raw backend busy'), { status: 409 });
  }, { confirmStop: counts => { confirmations.push(counts); return approval.promise; } });
  await relay.setActive(true);
  assert.match(relay.html(), /Stop relay · 1 in flight/);
  assert.match(relay.html(), /2 remembered · 1 receiving a request/);
  const stopping = relay.stop();
  await relay.stop();
  assert.deepEqual(confirmations, [{ inFlight: 1, activeTasks: 1 }]);
  assert.equal(calls.length, 1);
  approval.resolve(true);
  assert.equal(await stopping, false);
  assert.deepEqual(calls.map(c => c.path), ['/api/desktop-relay', '/api/desktop-relay/stop', '/api/desktop-relay']);
  assert.match(relay.html(), /Relay is busy.*streams.*finish/i);
  assert.doesNotMatch(relay.html(), /raw backend busy|generation changed/);
});

test('Stop with no active streams needs no confirmation or automatic restart', async () => {
  const state = ready();
  state.sessions.forEach(s => { s.in_flight = 0; });
  const { relay, calls } = fixture(async (path, options) => {
    if (options.method === 'GET') return state;
    assert.equal(path, '/api/desktop-relay/stop');
    assert.deepEqual(JSON.parse(options.body), {});
    state.status = disabled().status;
    return {};
  }, { confirmStop: () => assert.fail('idle relay must not add an approval flow') });
  await relay.setActive(true);
  assert.equal(await relay.stop(), true);
  assert.match(relay.html(), /Disabled/);
  assert.ok(calls.every(c => !c.path.endsWith('/start')));
});

test('leaving Settings aborts reads and a late prior-page response cannot replace the new page state', async () => {
  const replies = [deferred(), deferred()];
  let index = 0;
  const { relay, calls } = fixture(() => replies[index++].promise);
  const oldPage = relay.setActive(true);
  relay.setActive(false);
  assert.equal(calls[0].options.signal.aborted, true);
  const newPage = relay.setActive(true);
  assert.equal(calls.length, 2);
  replies[1].resolve(ready());
  await newPage;
  replies[0].resolve(disabled());
  await oldPage;
  assert.match(relay.html(), /Ready/);
});

test('logout clears setup and tasks, and late mutations cannot repaint or request a refresh', async () => {
  const state = ready(), completion = deferred();
  const { relay, context, calls, paints, copies } = fixture(async (_path, options) => {
    if (options.method === 'GET') return state;
    return completion.promise;
  });
  await relay.setActive(true);
  const creating = relay.createScope('future');
  context.locked = context.loggingOut = true;
  relay.clear();
  const paintCount = paints.length;
  completion.resolve({ id: 'new', label: 'future', env: { HTTPS_PROXY: 'late-admission-secret', NODE_EXTRA_CA_CERTS: '/fixture/ca.pem' } });
  await creating;
  await relay.refresh();
  await relay.start();
  await relay.useTask('scope/a', 'session/a', 'b');
  await relay.copySetup();
  await relay.setActive(true);
  assert.equal(calls.length, 2);
  assert.equal(calls[1].options.signal.aborted, true);
  assert.equal(paints.length, paintCount);
  assert.deepEqual(copies, []);
  assert.doesNotMatch(relay.html(), /late-admission-secret|a@example.test|Future task|session\/a/);
});

class MockPage {
  constructor() { this.listeners = new Map(); this.innerHTML = ''; this.ownerDocument = { activeElement: null }; }
  addEventListener(type, listener) { this.listeners.set(type, listener); }
  contains(node) { return node?.page === this; }
  querySelectorAll() { return []; }
  replaceChildren() { this.innerHTML = ''; }
  fire(type, target) { return this.listeners.get(type)?.({ target, preventDefault() {} }); }
  control(action, scope = '', session = '') {
    const row = { dataset: { relayScope: scope, relaySession: session } };
    const control = { page: this, disabled: false, dataset: { relayAction: action },
      closest: selector => selector === '.desktop-relay-task' || selector === '[data-relay-scope]' ? row : control };
    return control;
  }
}

test('headless Settings replacement retains dropdown choice and the shared busy guard', async () => {
  const state = ready(), completion = deferred();
  const { relay, calls } = fixture(async (_path, options) => options.method === 'GET' ? state : completion.promise);
  const first = new MockPage(), replacement = new MockPage();
  await relay.mount(first);
  const select = first.control('choose', 'scope/a', 'session/a');
  select.value = 'b';
  await first.fire('change', select);
  assert.equal(calls.length, 1, 'changing the dropdown alone must not mutate');
  await relay.mount(replacement);
  assert.match(replacement.innerHTML, /value="b" selected/);
  const switching = replacement.fire('click', replacement.control('use', 'scope/a', 'session/a'));
  const third = new MockPage();
  await relay.mount(third);
  assert.match(third.innerHTML, /data-relay-action="use"[^>]*disabled[^>]*>Switching/);
  await third.fire('click', third.control('use', 'scope/a', 'session/a'));
  await first.fire('click', first.control('use', 'scope/a', 'session/a'));
  assert.equal(calls.length, 2, 'replaced controls must not duplicate the mutation');
  assert.deepEqual(JSON.parse(calls[1].options.body), { account_id: 'b', revision: 7 });
  completion.resolve({});
  await switching;
  assert.equal(calls.length, 3);
});

test('the dashboard adapter places control proof in a header and delegates CSRF hydration', async () => {
  const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
  const from = source.indexOf('let authState = '), to = source.indexOf('\nfunction escapeHTML(', from);
  const requests = [], storage = new Map();
  const context = vm.createContext({
    AbortController, setTimeout: () => 1, clearTimeout() {},
    localStorage: { getItem: key => storage.get(key), setItem: (key, value) => storage.set(key, value), removeItem: key => storage.delete(key) },
    fetch: async (path, options) => {
      requests.push({ path, options });
      const body = path === '/api/auth/status' ? { auth_enabled: true, csrf: 'fixture-csrf' }
        : path.endsWith('/start') ? {} : disabled();
      return { ok: true, status: 200, json: async () => body };
    },
  });
  vm.runInContext(source.slice(from, to) + '\nthis.api = api;', context);
  const { relay } = fixture(undefined, { api: context.api });
  await relay.setActive(true);
  await relay.start();
  const start = requests.find(r => r.path.endsWith('/start'));
  assert.equal(start.options.headers['X-Switcher-Desktop-Control'], 'fixture-control-key');
  assert.equal(start.options.headers['X-Switcher-CSRF'], 'fixture-csrf');
  assert.equal(start.options.headers['Content-Type'], 'application/json');
  assert.equal(start.options.controllerKey, undefined, 'proof option must be consumed by the adapter');
  assert.equal(start.options.credentials, 'same-origin');
  assert.ok(requests.every(r => !r.path.includes('fixture-control-key')));
  assert.deepEqual([...storage.keys()], ['switcher-csrf']);
});

test('a late Settings hydration cannot mount controls after navigation or a dashboard epoch change', async () => {
  const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
  const from = source.indexOf('async function renderSettings() {');
  const to = source.indexOf("\nif (location.hash === '#settings')", from);
  for (const invalidate of [context => { context.settingsPage.hidden = true; }, context => { context.stateEpoch++; }]) {
    const status = deferred();
    const context = vm.createContext({
      authState: { locked: false }, settingsPage: { hidden: false, innerHTML: '' },
      settingsRenderGeneration: 0, stateEpoch: 3, api: () => status.promise,
      escapeHTML: value => String(value), lanStateText: () => '',
    });
    vm.runInContext(source.slice(from, to) + '\nthis.renderSettings = renderSettings;', context);
    const opening = context.renderSettings();
    invalidate(context);
    status.resolve({});
    await opening;
    assert.equal(context.settingsPage.innerHTML, '');
  }
});

test('late dashboard key hydration permits one demand load, with all controls still loopback-only', async () => {
  for (const hostname of ['localhost', '127.0.0.1', '127.0.0.2', '[::1]', '::1']) assert.equal(desktopRelayLoopback(hostname), true);
  for (const hostname of ['192.168.1.2', 'relay.example', 'localhost.example', '127.0.0.256', '::ffff:127.0.0.1', '']) assert.equal(desktopRelayLoopback(hostname), false);
  const { relay, context, calls } = fixture();
  context.controllerKey = '';
  await relay.setActive(true);
  assert.equal(calls.length, 0, 'auth-off is not management authority');
  assert.match(relay.html(), /Waiting for.*control key/i);
  context.controllerKey = 'hydrated-key';
  await relay.update();
  await relay.update();
  assert.equal(calls.length, 1, 'ordinary dashboard repaints do not poll the relay');
  assert.equal(calls[0].options.controllerKey, 'hydrated-key');
  context.loopback = false;
  await relay.refresh();
  await relay.start();
  assert.equal(calls.length, 1);
  assert.match(relay.html(), /only on a loopback dashboard/i);
  assert.match(relay.html(), /data-relay-action="start"[^>]*disabled/);
});

test('relay refresh and Settings replacement restore keyboard focus by a scoped control identity', async () => {
  const { relay } = fixture(async () => ready());
  const page = new MockPage();
  await relay.mount(page);
  const oldSelect = page.control('choose', 'scope/a', 'session/a');
  oldSelect.dataset.relayFocus = '["scope/a","session/a"]';
  page.ownerDocument.activeElement = oldSelect;
  const replacementSelect = { dataset: { relayFocus: '["scope/a","session/a"]' },
    focus: () => { page.ownerDocument.activeElement = replacementSelect; } };
  page.querySelectorAll = () => [replacementSelect];
  await relay.refresh();
  assert.equal(page.ownerDocument.activeElement, replacementSelect);
  const replacementPage = new MockPage();
  const afterSettingsPaint = { ...replacementSelect, focus: () => { replacementPage.ownerDocument.activeElement = afterSettingsPaint; } };
  replacementPage.querySelectorAll = () => [afterSettingsPaint];
  await relay.mount(replacementPage, '["scope/a","session/a"]');
  assert.equal(replacementPage.ownerDocument.activeElement, afterSettingsPaint);
});

test('an obsolete task completion releases the guard on the replacement page without applying its response', async () => {
  const completion = deferred();
  const { relay, calls } = fixture(async (_path, options) => options.method === 'GET' ? ready() : completion.promise);
  await relay.setActive(true);
  const switching = relay.useTask('scope/a', 'session/a', 'b');
  relay.setActive(false);
  const replacement = new MockPage();
  await relay.mount(replacement);
  completion.resolve({ status: ready().status, sessions: [{ scope_id: 'scope/a', session_id: 'session/a', account_id: 'b' }] });
  await switching;
  assert.doesNotMatch(replacement.innerHTML, />Switching|Conversation selection saved|Selected: b@example.test/);
  assert.match(replacement.innerHTML, /data-relay-action="use"[^>]*>Switch conversation/);
  assert.equal(calls.length, 3, 'no old-page completion refresh is sent');
});

test('a CAS refresh failure still explains the changed generation and permits manual refresh', async () => {
  let reads = 0;
  const { relay, calls } = fixture(async (_path, options) => {
    if (options.method !== 'GET') throw Object.assign(new Error('private error'), { status: 409 });
    if (++reads === 2) throw new Error('private read error');
    return ready();
  });
  await relay.setActive(true);
  await relay.useTask('scope/a', 'session/a', 'b');
  assert.match(relay.html(), /generation changed/);
  assert.match(relay.html(), /Could not load/);
  assert.doesNotMatch(relay.html(), /private error|private read error/);
  await relay.refresh();
  assert.doesNotMatch(relay.html(), /generation changed|Could not load/);
  assert.equal(calls.filter(c => c.options.method === 'POST').length, 1);
});

test('one-time setup is cleared on dismiss, navigation, revocation, and logout, including clipboard failure', async () => {
  for (const cleanup of ['dismiss', 'leave', 'revoke', 'logout']) {
    const state = ready();
    const { relay, context } = fixture(async (_path, options) => {
      if (options.method === 'GET') return state;
      if (options.method === 'DELETE') { state.scopes = state.scopes.filter(s => s.id !== 'new'); return {}; }
      state.scopes.push({ id: 'new', label: 'future' });
      return { id: 'new', label: 'future', env: { HTTPS_PROXY: 'one-time-secret', NODE_EXTRA_CA_CERTS: '/fixture/public-ca.pem' } };
    }, { copy: async () => { throw new Error('private clipboard details'); } });
    await relay.setActive(true);
    await relay.createScope('future');
    assert.equal(await relay.copySetup(), false);
    assert.match(relay.html(), /copy it manually/);
    assert.match(relay.html(), /one-time-secret/);
    assert.doesNotMatch(relay.html(), /private clipboard details/);
    if (cleanup === 'dismiss') relay.dismissSetup();
    if (cleanup === 'leave') { relay.setActive(false); await relay.setActive(true); }
    if (cleanup === 'revoke') await relay.deleteScope('new');
    if (cleanup === 'logout') { context.locked = true; relay.clear(); }
    assert.doesNotMatch(relay.html(), /one-time-secret|data-relay-action="copy-setup"/);
  }
});

test('cookie and device authorities delegate authentication without attaching the local key', async () => {
  for (const authKind of ['cookie', 'device']) {
    const { relay, calls, context } = fixture();
    context.authKind = authKind;
    context.controllerKey = '';
    await relay.setActive(true);
    await relay.start();
    assert.deepEqual(calls.map(c => c.options.method), ['GET', 'POST', 'GET']);
    assert.ok(calls.every(c => c.options.controllerKey === undefined));
    assert.ok(calls.every(c => c.options.headers?.['X-Switcher-CSRF'] === undefined), 'the request adapter owns CSRF');
  }
});

test('GETs are deduplicated and pre-switch status cannot roll back an acknowledged selection', async () => {
  const stale = deferred();
  let reads = 0;
  const { relay, calls } = fixture(async (_path, options) => {
    if (options.method !== 'GET') return {};
    if (++reads === 2) return stale.promise;
    const state = ready();
    if (reads > 2) state.sessions[0] = { ...state.sessions[0], account_id: 'b', revision: 8 };
    return state;
  });
  await relay.setActive(true);
  const oldRefresh = relay.refresh(), duplicate = relay.refresh();
  assert.equal(calls.length, 2);
  await relay.useTask('scope/a', 'session/a', 'b');
  assert.equal(calls[1].options.signal.aborted, true);
  stale.resolve(disabled());
  await Promise.all([oldRefresh, duplicate]);
  assert.match(relay.html(), /Ready/);
  assert.match(relay.html(), /Selected: b@example.test/);
  assert.equal(calls.length, 4);
});

test('an unavailable stored binding can be explicitly restored to the visible caller-auth choice', async () => {
  const { relay, calls } = fixture(async (_path, options) => options.method === 'GET' ? ready() : {},
    { getAccounts: () => [accounts[1]] });
  const page = new MockPage();
  await relay.mount(page);
  assert.match(page.innerHTML, /Selected: Stored account unavailable/);
  assert.match(page.innerHTML, /option value="" selected>Default: Claude sign-in/);
  await page.fire('click', page.control('use', 'scope/a', 'session/a'));
  assert.equal(calls[1].options.method, 'DELETE');
  assert.deepEqual(JSON.parse(calls[1].options.body), { revision: 7 });
  const before = calls.length;
  await relay.useTask('scope/a', 'session/a', undefined);
  assert.equal(calls.length, before, 'an omitted action argument must never silently unbind');
});

test('a disabled relay restores a persisted task to caller auth without Start and blocks stored-account binding', async () => {
  const state = ready();
  state.status = disabled().status;
  state.sessions.forEach(task => { task.in_flight = 0; });
  const { relay, calls } = fixture(async (path, options) => {
    if (options.method === 'GET') return state;
    assert.equal(path, '/api/desktop-relay/scopes/scope%2Fa/sessions/session%2Fa/account');
    assert.equal(options.method, 'DELETE');
    assert.deepEqual(JSON.parse(options.body), { revision: 7 });
    state.sessions[0] = { ...state.sessions[0], account_id: '', revision: 8 };
    return {};
  });
  const page = new MockPage();
  await relay.mount(page);
  assert.match(page.innerHTML, /Disabled/);
  assert.doesNotMatch(rowHTML(page.innerHTML, 'scope/a', 'session/a').match(/<select[^>]*data-relay-action="choose"[^>]*>/)?.[0] || '', /\bdisabled\b/,
    'the dropdown must allow a caller-auth choice while the relay is stopped');
  assert.match(actionTag(rowHTML(page.innerHTML, 'scope/a', 'session/a'), 'use'), /\bdisabled\b/,
    'applying the current stored account still requires a listener');
  const select = page.control('choose', 'scope/a', 'session/a');
  select.value = '';
  await page.fire('change', select);
  assert.match(page.innerHTML, /option value="" selected>Default: Claude sign-in/);
  assert.doesNotMatch(actionTag(rowHTML(page.innerHTML, 'scope/a', 'session/a'), 'use'), /\bdisabled\b/,
    'choosing caller auth must immediately enable the restore action');
  await page.fire('click', page.control('use', 'scope/a', 'session/a'));
  assert.deepEqual(calls.map(call => [call.options.method, call.path]), [
    ['GET', '/api/desktop-relay'],
    ['DELETE', '/api/desktop-relay/scopes/scope%2Fa/sessions/session%2Fa/account'],
    ['GET', '/api/desktop-relay'],
  ]);
  assert.equal(state.sessions[1].account_id, 'a');
  assert.equal(state.sessions[1].revision, 2);
  assert.match(page.innerHTML, /Default: Claude sign-in/);
  select.value = 'b';
  await page.fire('change', select);
  assert.match(actionTag(rowHTML(page.innerHTML, 'scope/a', 'session/a'), 'use'), /\bdisabled\b/);
  assert.equal(await relay.useTask('scope/a', 'session/a', 'b'), false);
  await page.fire('click', page.control('use', 'scope/a', 'session/a'));
  assert.equal(calls.length, 3, 'stored-account attempts must not send POST or start the relay');
});

test('unfinished shutdown is shown as Stopping and explicit Start can recover after a Busy response', async () => {
  const state = ready(), completion = deferred();
  state.sessions.forEach(task => { task.in_flight = 0; });
  let startAttempts = 0;
  const { relay, calls } = fixture(async (path, options) => {
    if (options.method === 'GET') return state;
    if (path.endsWith('/stop')) {
      state.status = { ...disabled().status, condition: 'stopping' };
      throw Object.assign(new Error('private shutdown timeout'), { status: 409 });
    }
    assert.equal(path, '/api/desktop-relay/start');
    assert.deepEqual(JSON.parse(options.body), {});
    if (++startAttempts === 1) throw Object.assign(new Error('private runtime busy'), { status: 409 });
    await completion.promise;
    state.status = ready().status;
    return {};
  });
  await relay.setActive(true);
  assert.equal(await relay.stop(), false);
  assert.match(relay.html(), /class="desktop-relay-status"><strong>Stopping<\/strong>/);
  assert.match(relay.html(), /Condition: stopping/);
  assert.doesNotMatch(relay.html().match(/<button[^>]*data-relay-action="start"[^>]*>/)?.[0] || '', /\bdisabled\b/,
    'Start must remain available to reclaim a finished runtime');
  assert.match(relay.html(), /role="alert"[^>]*>Relay is busy/);
  assert.equal(startAttempts, 0, 'Stop must not automatically restart the relay');
  assert.equal(await relay.start(), false);
  assert.match(relay.html(), /role="alert"[^>]*>Relay is busy/);
  assert.match(relay.html(), /Start was not retried/);
  assert.doesNotMatch(relay.html(), /private shutdown timeout|private runtime busy|generation changed|relay state changed/);
  const recovering = relay.start();
  await relay.start();
  assert.match(relay.html(), /data-relay-action="start"[^>]*aria-busy="true"[^>]*disabled[^>]*>Starting/);
  assert.equal(startAttempts, 2);
  completion.resolve();
  assert.equal(await recovering, true);
  assert.match(relay.html(), /class="desktop-relay-status"><strong>Ready<\/strong>/);
  assert.equal(calls.filter(call => call.path.endsWith('/start')).length, 2, 'only explicit Start attempts were sent');
});

const setupRecord = (condition = 'configured', extra = {}) => ({
  condition, configured: condition === 'configured', restart_required: condition === 'configured',
  settings_path: '/fixture/desktop/settings.json', backup_path: '/fixture/desktop/settings.backup.json',
  scope_id: condition === 'not_configured' ? '' : 'scope/a', ...extra,
});
const configured = () => ({ ...ready(), setup: setupRecord() });
const actionTag = (html, action) => html.match(new RegExp(`<button[^>]*data-relay-action="${action}"[^>]*>`))?.[0] || '';
const rowHTML = (html, scope, session) => html.split('<div class="desktop-relay-task"').find(row => row.startsWith(` data-relay-scope="${scope}" data-relay-session="${session}"`)) || '';
const visibleRowHTML = (html, scope, session) => rowHTML(html, scope, session).split('<details class="desktop-relay-diagnostics"')[0];
const mutations = calls => calls.filter(call => call.options.method !== 'GET');
const settle = async () => { for (let i = 0; i < 8; i++) await Promise.resolve(); };

test('first-time setup needs only Configure, with manual profiles and lifecycle controls inside closed Advanced', async () => {
  const state = { ...disabled(), setup: setupRecord('not_configured') };
  const { relay, calls } = fixture(async () => state);
  assert.match(actionTag(relay.html(), 'configure'), /disabled/);
  await relay.setActive(true);
  const html = relay.html(), main = html.slice(0, html.indexOf('<details class="desktop-relay-advanced"'));
  assert.match(main, />Configure Claude Desktop<\/button>/);
  assert.doesNotMatch(actionTag(main, 'configure'), /disabled/);
  assert.match(actionTag(main, 'restore-setup'), /disabled/);
  assert.doesNotMatch(main, /Profile label|New profile|Start relay|Stop relay|HTTPS_PROXY|NODE_EXTRA_CA_CERTS|environment JSON|Setup is manual/);
  assert.match(main, /Switcher backs up settings and configures connection\. Desktop sign-in stays as is\./);
  assert.equal(actionTag(main, 'restart-desktop'), '');
  assert.doesNotMatch(html.match(/<details[^>]*data-relay-advanced[^>]*>/)[0], /\bopen\b/);
  assert.match(html, /data-relay-profile-form/);
  assert.deepEqual(calls.map(call => call.options.method), ['GET']);
});

test('one Configure click sends only an empty POST, reconciles GET, and never starts, creates profiles, or restarts from the browser', async () => {
  const state = { ...ready(), status: disabled().status, setup: setupRecord('not_configured') };
  const peers = structuredClone({ scopes: state.scopes, sessions: state.sessions });
  const { relay, calls, copies } = fixture(async (path, options) => {
    if (options.method === 'GET') return state;
    assert.equal(path, '/api/desktop-relay/configure');
    assert.deepEqual(JSON.parse(options.body), {});
    state.setup = setupRecord();
    state.status = ready().status;
    return { status: 'configured', setup: { ...state.setup, message: 'mutation message must not replace GET' },
      env: { HTTPS_PROXY: 'mutation-admission-secret', NODE_EXTRA_CA_CERTS: 'mutation-private-path' } };
  }, { confirmRestart: () => assert.fail('Configure must not ask to restart') });
  const page = new MockPage();
  await relay.mount(page);
  assert.equal(await page.fire('click', page.control('configure')), true);
  assert.deepEqual(calls.map(call => [call.options.method, call.path]), [
    ['GET', '/api/desktop-relay'], ['POST', '/api/desktop-relay/configure'], ['GET', '/api/desktop-relay'],
  ]);
  assert.match(page.innerHTML, /Configured\. Restart Claude Desktop when current work is finished\./);
  assert.doesNotMatch(actionTag(page.innerHTML, 'restart-desktop'), /disabled/);
  assert.deepEqual({ scopes: state.scopes, sessions: state.sessions }, peers);
  assert.deepEqual(copies, []);
  assert.doesNotMatch(page.innerHTML, /mutation-admission-secret|mutation-private-path|mutation message/);
});

test('Configure shares the mutation guard, paints progress, and leaves retries to an explicit click', async () => {
  const state = { ...ready(), setup: setupRecord('not_configured') }, completion = deferred();
  const { relay, calls, paints } = fixture(async (_path, options) => options.method === 'GET' ? state : completion.promise);
  await relay.setActive(true);
  const configuring = relay.configure();
  assert.match(actionTag(relay.html(), 'configure'), /aria-busy="true"[^>]*disabled/);
  assert.match(paints.at(-1), />Configuring…<\/button>/);
  await relay.configure();
  await relay.restoreSetup();
  await relay.restartDesktop();
  await relay.createScope('duplicate');
  await relay.useTask('scope/a', 'session/a', 'b');
  await relay.refresh();
  assert.equal(mutations(calls).length, 1);
  completion.reject(Object.assign(new Error('raw-private-error'), { status: 503, body: { error: 'Desktop settings are unavailable.' } }));
  assert.equal(await configuring, false);
  assert.match(relay.html(), /role="alert"[^>]*>Desktop settings are unavailable\./);
  assert.match(relay.html(), /request was not retried/);
  assert.doesNotMatch(relay.html(), /raw-private-error/);
  assert.doesNotMatch(actionTag(relay.html(), 'configure'), /disabled/);
  await relay.update();
  assert.equal(mutations(calls).length, 1, 'dashboard paints must not replay a failed Configure');
  assert.equal(await relay.configure(), false);
  assert.equal(mutations(calls).length, 2, 'only an explicit retry sends another POST');
});

test('setup status uses only whitelisted metadata and escapes settings paths, backups, and messages', async () => {
  const state = configured();
  state.setup = setupRecord('changed', {
    settings_path: '/fixture/"<img src=x>.json', backup_path: '/fixture/<script>backup</script>',
    message: '<svg onload=x> Review http://scope:setup-admission-secret@127.0.0.1:9443 before configuring.',
    env: { HTTPS_PROXY: 'get-env-secret', NODE_EXTRA_CA_CERTS: 'get-ca-secret' },
    proxy_url: 'get-proxy-secret', credential: 'get-credential-secret',
  });
  const { relay, calls } = fixture(async () => state);
  await relay.setActive(true);
  const html = relay.html();
  assert.match(html, /Settings changed\. Reconfigure needed\./);
  assert.match(html, /&quot;&lt;img src=x&gt;\.json/);
  assert.match(html, /&lt;script&gt;backup&lt;\/script&gt;/);
  assert.match(html, /&lt;svg onload=x&gt; Review http:\/\/\[redacted\]@127\.0\.0\.1/);
  assert.doesNotMatch(html, /<img|<script|<svg|setup-admission-secret|get-env-secret|get-ca-secret|get-proxy-secret|get-credential-secret/);
  assert.doesNotMatch(html.match(/<details[^>]*data-relay-details[^>]*>/)[0], /\bopen\b/);
  assert.equal(mutations(calls).length, 0);
});

test('pending, unavailable, and unknown setup states block Configure and restart without mutating', async () => {
  for (const condition of ['pending', 'unavailable', 'future_condition']) {
    const state = { ...ready(), setup: setupRecord(condition, { configured: true, restart_required: true }) };
    const { relay, calls } = fixture(async () => state, { confirmRestart: () => assert.fail('setup is not ready') });
    await relay.setActive(true);
    assert.match(actionTag(relay.html(), 'configure'), /disabled/);
    assert.equal(actionTag(relay.html(), 'restart-desktop'), '');
    assert.equal(await relay.configure(), false);
    assert.equal(await relay.restartDesktop(), false);
    assert.equal(mutations(calls).length, 0);
    assert.match(relay.html(), condition === 'pending' ? /Setup pending/ : /Setup unavailable/);
  }
});

test('legacy GET without setup defaults to Not configured and an explicit Configure 404 explains the version requirement', async () => {
  const { relay, calls } = fixture(async (_path, options) => {
    if (options.method === 'GET') return disabled();
    throw Object.assign(new Error('raw legacy error'), { status: 404, body: { error: 'Desktop setup endpoint unavailable.' } });
  });
  await relay.setActive(true);
  assert.match(relay.html(), /Not configured/);
  assert.equal(mutations(calls).length, 0);
  assert.equal(await relay.configure(), false);
  assert.match(relay.html(), /Desktop setup endpoint unavailable/);
  assert.match(relay.html(), /Update Switcher to a version with Desktop setup/);
  assert.doesNotMatch(relay.html(), /raw legacy error/);
  assert.equal(mutations(calls).length, 1);
  assert.match(relay.html(), /data-relay-profile-form/, 'legacy manual profiles remain available under Advanced');
});

test('restart is gated by configured listening setup, or GET evidence of a required restart after restoration', async () => {
  for (const [setup, listening, allowed] of [
    [setupRecord(), true, true], [setupRecord(), false, false],
    [setupRecord('configured', { configured: false }), true, false],
    [setupRecord('changed', { configured: true }), true, false],
    [setupRecord('not_configured'), true, false],
    [setupRecord('not_configured', { restart_required: true }), false, true],
  ]) {
    const state = { ...ready(), setup, status: { ...ready().status, listening } };
    let confirmations = 0;
    const { relay, calls } = fixture(async () => state, { confirmRestart: async () => { confirmations++; return false; } });
    await relay.setActive(true);
    assert.equal(Boolean(actionTag(relay.html(), 'restart-desktop')), allowed);
    assert.equal(await relay.restartDesktop(), false);
    assert.equal(confirmations, Number(allowed));
    assert.equal(mutations(calls).length, 0);
  }
});

test('restart Cancel and the default confirmation dependency send no POST and deduplicate the dialog', async () => {
  const approval = deferred();
  let confirmations = 0;
  const { relay, calls } = fixture(async () => configured(), { confirmRestart: () => { confirmations++; return approval.promise; } });
  const page = new MockPage();
  await relay.mount(page);
  const restarting = page.fire('click', page.control('restart-desktop'));
  await relay.restartDesktop();
  assert.equal(confirmations, 1);
  assert.match(actionTag(page.innerHTML, 'restart-desktop'), /aria-busy="true"[^>]*disabled/);
  assert.equal(mutations(calls).length, 0);
  approval.resolve(false);
  assert.equal(await restarting, false);
  assert.doesNotMatch(actionTag(page.innerHTML, 'restart-desktop'), /disabled/);
  assert.deepEqual(calls.map(call => call.options.method), ['GET']);
  const guarded = fixture(async () => configured());
  await guarded.relay.setActive(true);
  await guarded.relay.restartDesktop();
  assert.equal(mutations(guarded.calls).length, 0);
});

test('confirmed restart sends confirmed true once, treats restart_requested as acceptance, and reconciles GET', async () => {
  const completion = deferred(), state = configured();
  const { relay, calls, paints } = fixture(async (path, options) => {
    if (options.method === 'GET') return state;
    assert.equal(path, '/api/desktop-relay/restart-desktop');
    assert.deepEqual(JSON.parse(options.body), { confirmed: true });
    return completion.promise;
  }, { confirmRestart: async () => true });
  await relay.setActive(true);
  const restarting = relay.restartDesktop();
  await settle();
  await relay.restartDesktop();
  await relay.configure();
  await relay.restoreSetup();
  assert.equal(mutations(calls).length, 1);
  assert.match(paints.at(-1), />Requesting restart…<\/button>/);
  completion.resolve({ status: 'restart_requested', setup: setupRecord('not_configured'), desktop_running: true });
  assert.equal(await restarting, true);
  assert.deepEqual(calls.map(call => call.options.method), ['GET', 'POST', 'GET']);
  assert.match(relay.html(), /Restart requested\. Refresh status to check setup\./);
  assert.match(relay.html(), /desktop-relay-setup-status">Relay ready/);
  assert.doesNotMatch(relay.html(), /Desktop restarted|Desktop is running|Restart completed/);
});

test('Remove setup sends one empty POST, keeps peer tasks and pending account choices, and leaves restart explicit', async () => {
  const state = configured(), completion = deferred();
  const peers = structuredClone({ scopes: state.scopes, sessions: state.sessions });
  const { relay, calls, paints } = fixture(async (path, options) => {
    if (options.method === 'GET') return state;
    assert.equal(path, '/api/desktop-relay/restore');
    assert.deepEqual(JSON.parse(options.body), {});
    await completion.promise;
    state.setup = setupRecord('not_configured', { restart_required: true });
    return { status: 'restored', env: { HTTPS_PROXY: 'restore-private-env' } };
  }, { confirmRestart: () => assert.fail('removal must not ask to restart') });
  const page = new MockPage();
  await relay.mount(page);
  relay.chooseAccount('peer-scope', 'session/a', 'b');
  const restoring = page.fire('click', page.control('restore-setup'));
  await relay.restoreSetup();
  assert.match(actionTag(page.innerHTML, 'restore-setup'), /aria-busy="true"[^>]*disabled/);
  assert.match(paints.at(-1), />Removing setup…<\/button>/);
  assert.equal(mutations(calls).length, 1);
  completion.resolve();
  assert.equal(await restoring, true);
  assert.deepEqual(calls.map(call => [call.options.method, call.path]), [
    ['GET', '/api/desktop-relay'], ['POST', '/api/desktop-relay/restore'], ['GET', '/api/desktop-relay'],
  ]);
  assert.match(page.innerHTML, /Restored previous settings\. Restart Desktop when work is finished\./);
  assert.match(actionTag(page.innerHTML, 'restore-setup'), /disabled/);
  assert.doesNotMatch(actionTag(page.innerHTML, 'restart-desktop'), /disabled/);
  assert.match(page.innerHTML, /value="b" selected/);
  assert.deepEqual({ scopes: state.scopes, sessions: state.sessions }, peers);
  assert.doesNotMatch(page.innerHTML, /restore-private-env/);
  assert.equal(await relay.restoreSetup(), false);
  assert.equal(mutations(calls).length, 1, 'no automatic Stop, activation, or replay after restoration');
});

test('owned-settings conflicts and managed profile revocation errors retain useful sanitized HTTP messages without replay', async () => {
  for (const operation of ['restore', 'delete']) {
    const state = configured();
    state.setup = setupRecord('changed');
    const { relay, calls } = fixture(async (_path, options) => {
      if (options.method === 'GET') return state;
      throw Object.assign(new Error('raw-private-stack'), { status: 409,
        body: { error: { message: 'owned settings changed <script>x</script>; HTTPS_PROXY="http://scope:private-value@127.0.0.1:9443"; Bearer private-token' } } });
    });
    await relay.setActive(true);
    assert.equal(await (operation === 'restore' ? relay.restoreSetup() : relay.deleteScope('scope/a')), false);
    assert.match(relay.html(), /owned settings changed &lt;script&gt;x&lt;\/script&gt;/);
    assert.doesNotMatch(relay.html(), /<script>|raw-private-stack|private-value|private-token/);
    assert.match(relay.html(), operation === 'delete' ? /Remove Desktop setup before stopping relay or revoking its profile/ : /request was not retried/);
    assert.deepEqual(calls.map(call => call.options.method), ['GET', operation === 'delete' ? 'DELETE' : 'POST', 'GET']);
    assert.match(relay.html(), /data-relay-scope="peer-scope"/);
  }
});

test('Configure fences a pre-mutation GET so stale setup cannot hide the acknowledged restart action', async () => {
  const stale = deferred();
  let reads = 0;
  const { relay, calls } = fixture(async (_path, options) => {
    if (options.method !== 'GET') return {};
    if (++reads === 2) return stale.promise;
    return reads > 2 ? configured() : { ...disabled(), setup: setupRecord('not_configured') };
  });
  await relay.setActive(true);
  const refreshing = relay.refresh();
  await relay.configure();
  assert.equal(calls[1].options.signal.aborted, true);
  stale.resolve({ ...disabled(), setup: setupRecord('not_configured') });
  await refreshing;
  assert.match(relay.html(), /desktop-relay-setup-status">Relay ready/);
  assert.doesNotMatch(actionTag(relay.html(), 'restart-desktop'), /disabled/);
  assert.deepEqual(calls.map(call => call.options.method), ['GET', 'GET', 'POST', 'GET']);
});

test('setup mutations obey local authority and cookie/device proof rules', async () => {
  for (const method of ['configure', 'restoreSetup', 'restartDesktop']) {
    for (const fence of ['loopback', 'controllerKey', 'locked', 'loggingOut']) {
      const { relay, calls, context } = fixture(async () => configured(), { confirmRestart: () => assert.fail('no authority') });
      await relay.setActive(true);
      context[fence] = fence === 'controllerKey' ? '' : fence === 'loopback' ? false : true;
      assert.equal(await relay[method](), false);
      assert.equal(mutations(calls).length, 0);
    }
    for (const authKind of ['cookie', 'device']) {
      const { relay, calls, context } = fixture(async (_path, options) => options.method === 'GET' ? configured() : {}, { confirmRestart: async () => true });
      context.authKind = authKind;
      context.controllerKey = '';
      await relay.setActive(true);
      await relay[method]();
      assert.equal(mutations(calls).length, 1);
      assert.ok(calls.every(call => call.options.controllerKey === undefined));
    }
  }
});

test('pending restart confirmation cannot POST after logout or a Settings page fence', async () => {
  for (const fence of ['logout', 'navigation']) {
    const approval = deferred();
    const { relay, calls, context } = fixture(async () => configured(), { confirmRestart: () => approval.promise });
    await relay.setActive(true);
    const restarting = relay.restartDesktop();
    if (fence === 'logout') { context.locked = context.loggingOut = true; relay.clear(); }
    else { relay.setActive(false); await relay.setActive(true); }
    approval.resolve(true);
    assert.equal(await restarting, false);
    assert.equal(mutations(calls).length, 0);
    assert.doesNotMatch(relay.html(), /Restart requested|Requesting restart/);
  }
});

test('late setup mutation successes and failures cannot repopulate a logged-out or replacement Settings page', async () => {
  for (const method of ['configure', 'restoreSetup', 'restartDesktop']) {
    for (const fence of ['logout', 'navigation']) {
      for (const fail of [false, true]) {
        const completion = deferred();
        let reads = 0;
        const { relay, calls, paints, context } = fixture(async (_path, options) => {
          if (options.method !== 'GET') return completion.promise;
          return ++reads === 1 ? configured() : { ...disabled(), setup: setupRecord('not_configured') };
        }, { confirmRestart: async () => true });
        await relay.setActive(true);
        const operation = relay[method]();
        await settle();
        assert.equal(mutations(calls).length, 1);
        if (fence === 'logout') { context.locked = context.loggingOut = true; relay.clear(); }
        else { relay.setActive(false); await relay.setActive(true); }
        const paintCount = paints.length, callCount = calls.length;
        assert.equal(mutations(calls)[0].options.signal.aborted, true);
        if (fail) completion.reject(Object.assign(new Error('private late error'), { status: 409, body: { error: 'late error must stay hidden' } }));
        else completion.resolve({ status: 'restart_requested', setup: setupRecord(), env: { HTTPS_PROXY: 'late-private-env' } });
        assert.equal(await operation, false);
        assert.equal(calls.length, callCount, 'a stale mutation must not request reconciliation');
        if (fence === 'logout') assert.equal(paints.length, paintCount);
        else assert.doesNotMatch(actionTag(relay.html(), 'configure'), /disabled/);
        assert.doesNotMatch(relay.html(), /Configured\. Restart|Restored previous|Restart requested|late-private-env|late error must stay hidden|private late error/);
      }
    }
  }
});

test('the dashboard restart hook warns about Desktop windows and Code tasks with Cancel as initial focus', async () => {
  const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
  const from = source.indexOf('settingsPage.desktopRelay = createDesktopRelay({');
  const to = source.indexOf('\nconst settingsTab = ', from);
  const dialogs = [];
  const context = vm.createContext({
    settingsPage: {}, api() {}, createDesktopRelay: dependencies => dependencies,
    confirmDialog: options => { dialogs.push(options); return Promise.resolve(false); },
  });
  vm.runInContext(source.slice(from, to), context);
  assert.equal(await context.settingsPage.desktopRelay.confirmRestart(), false);
  assert.equal(dialogs[0].title, 'Restart Claude Desktop?');
  assert.match(dialogs[0].message, /closes Claude Desktop windows/);
  assert.match(dialogs[0].message, /interrupts active Desktop Code tasks/);
  assert.match(dialogs[0].message, /Finish your current work/);
  assert.equal(dialogs[0].confirmLabel, 'Restart Claude Desktop');
  assert.equal(dialogs[0].initialFocus, 'cancel');
});

class FocusPage extends MockPage {
  constructor() {
    super();
    this.ownerDocument.body = {};
    this.ownerDocument.activeElement = this.ownerDocument.body;
    this.innerHTML = '';
  }
  get innerHTML() { return this.markup || ''; }
  set innerHTML(markup) {
    if (this.ownerDocument) this.ownerDocument.activeElement = this.ownerDocument.body;
    for (const node of this.nodes || []) node.page = null;
    this.markup = markup;
    this.nodes = [...markup.matchAll(/<(?:button|summary|select|code)[^>]*data-relay-focus="([^"]+)"[^>]*>/g)].map(([tag, encoded]) => {
      const key = encoded.replaceAll('&quot;', '"');
      return { page: this, disabled: /\bdisabled\b/.test(tag), dataset: { relayFocus: key },
        focus: () => { this.ownerDocument.activeElement = this.nodes.find(node => node.dataset.relayFocus === key); } };
    });
    this.details = new Map([...markup.matchAll(/<details[^>]*data-relay-(advanced|details|recent)[^>]*>/g)].map(([tag, kind]) => [kind, {
      page: this, open: /\bopen\b/.test(tag), matches: selector => selector === `[data-relay-${kind}]`,
    }]));
    this.diagnostics = new Map([...markup.matchAll(/<details[^>]*data-relay-diagnostic="([^"]+)"[^>]*>/g)].map(([tag, encoded]) => {
      const key = encoded.replaceAll('&quot;', '"');
      return [key, { page: this, open: /\bopen\b/.test(tag), dataset: { relayDiagnostic: key },
        matches: selector => selector === '[data-relay-diagnostic]' }];
    }));
  }
  querySelectorAll(selector) { return selector === '[data-relay-diagnostic]' ? [...this.diagnostics.values()] : this.nodes; }
  querySelector(selector) { return this.details.get(selector.match(/^\[data-relay-(advanced|details|recent)\]$/)?.[1]); }
}

test('Configure and Remove progress restore keyboard focus to the explicit restart action when their button becomes unavailable', async () => {
  for (const method of ['configure', 'restoreSetup']) {
    const completion = deferred(), state = configured();
    if (method === 'configure') { state.setup = setupRecord('not_configured'); state.status = disabled().status; }
    const { relay } = fixture(async (_path, options) => {
      if (options.method === 'GET') return state;
      await completion.promise;
      state.status = ready().status;
      state.setup = method === 'configure' ? setupRecord() : setupRecord('not_configured', { restart_required: true });
      return {};
    });
    const page = new FocusPage();
    await relay.mount(page);
    const key = method === 'configure' ? 'configure' : 'restore-setup';
    page.nodes.find(node => node.dataset.relayFocus === key).focus();
    const operation = relay[method]();
    assert.equal(page.ownerDocument.activeElement, page.ownerDocument.body, 'disabled operation controls cannot keep focus');
    completion.resolve();
    await operation;
    assert.equal(page.ownerDocument.activeElement.dataset.relayFocus, 'restart-desktop');
  }
});

test('Advanced and Setup details stay open through refresh and Settings replacement while tasks remain outside Advanced', async () => {
  const { relay, calls } = fixture(async () => configured());
  const page = new FocusPage();
  await relay.mount(page);
  for (const kind of ['advanced', 'details']) {
    const details = page.details.get(kind);
    details.open = true;
    page.fire('toggle', details);
  }
  await relay.refresh();
  const replacement = new FocusPage();
  await relay.mount(replacement);
  for (const kind of ['advanced', 'details']) assert.equal(replacement.details.get(kind).open, true);
  const main = replacement.innerHTML.slice(0, replacement.innerHTML.indexOf('<details class="desktop-relay-advanced"'));
  assert.match(main, /data-relay-session="session\/a"/);
  assert.equal(mutations(calls).length, 0);
});

test('an owned unavailable setup permits explicit removal without a listener and keeps Configure and restart blocked', async () => {
  const state = configured(), completion = deferred();
  state.setup = setupRecord('unavailable', { configured: true, restart_required: true, message: 'Relay port is occupied.' });
  state.status = { ...ready().status, enabled: false, listening: false, condition: 'unavailable' };
  const peers = structuredClone({ scopes: state.scopes, sessions: state.sessions });
  const { relay, calls } = fixture(async (path, options) => {
    if (options.method === 'GET') return state;
    assert.equal(path, '/api/desktop-relay/restore');
    assert.deepEqual(JSON.parse(options.body), {});
    await completion.promise;
    state.setup = setupRecord('not_configured', { restart_required: true });
    return { status: 'restored' };
  }, { confirmRestart: () => assert.fail('unavailable setup must not offer restart') });
  const page = new MockPage();
  await relay.mount(page);
  assert.match(page.innerHTML, /Relay port is occupied\./);
  assert.match(page.innerHTML, /The relay is offline\. You can still request removal of Desktop setup\./);
  assert.match(actionTag(page.innerHTML, 'configure'), /disabled/);
  assert.equal(actionTag(page.innerHTML, 'restart-desktop'), '');
  assert.doesNotMatch(actionTag(page.innerHTML, 'restore-setup'), /disabled/);
  await relay.configure();
  await relay.restartDesktop();
  assert.equal(mutations(calls).length, 0);
  const restoring = page.fire('click', page.control('restore-setup'));
  await relay.restoreSetup();
  assert.match(actionTag(page.innerHTML, 'restore-setup'), /aria-busy="true"[^>]*disabled/);
  completion.resolve();
  assert.equal(await restoring, true);
  assert.deepEqual(calls.map(call => [call.options.method, call.path]), [
    ['GET', '/api/desktop-relay'], ['POST', '/api/desktop-relay/restore'], ['GET', '/api/desktop-relay'],
  ]);
  assert.deepEqual({ scopes: state.scopes, sessions: state.sessions }, peers);
  assert.match(page.innerHTML, /Restored previous settings\./);
});

test('unavailable setup removal needs both an owned scope ID and backup path', async () => {
  for (const extra of [
    { scope_id: '' }, { backup_path: '' }, { scope_id: '   ' }, { backup_path: '   ' },
    { scope_id: null }, { backup_path: 12 },
  ]) {
    const state = { ...disabled(), setup: setupRecord('unavailable', extra) };
    const { relay, calls } = fixture(async () => state);
    await relay.setActive(true);
    assert.match(actionTag(relay.html(), 'restore-setup'), /disabled/);
    assert.equal(await relay.restoreSetup(), false);
    assert.equal(mutations(calls).length, 0);
    assert.doesNotMatch(relay.html(), /You can still request removal/);
  }
});

test('offline removal leaves unsafe-store validation to the backend and surfaces its rejection without replay', async () => {
  const state = { ...disabled(), setup: setupRecord('unavailable') };
  const { relay, calls } = fixture(async (_path, options) => {
    if (options.method === 'GET') return state;
    throw Object.assign(new Error('private storage details'), {
      status: 503, body: { error_code: 'unavailable', error: 'Desktop setup storage is unavailable.' },
    });
  });
  await relay.setActive(true);
  assert.equal(await relay.restoreSetup(), false);
  assert.match(relay.html(), /role="alert"[^>]*>Desktop setup storage is unavailable\./);
  assert.match(relay.html(), /request was not retried/);
  assert.doesNotMatch(relay.html(), /private storage details|Restored previous settings/);
  assert.doesNotMatch(actionTag(relay.html(), 'restore-setup'), /disabled/);
  assert.deepEqual(calls.map(call => [call.options.method, call.path]), [
    ['GET', '/api/desktop-relay'], ['POST', '/api/desktop-relay/restore'],
  ]);
});

test('HTTP 409 setup_owned takes priority for idle Stop and profile revocation without automatic removal or retry', async () => {
  for (const operation of ['stop', 'delete']) {
    const state = configured();
    state.sessions.forEach(task => { task.in_flight = 0; });
    state.status.in_flight = 0;
    if (operation === 'delete') state.setup = setupRecord('not_configured');
    const peers = structuredClone({ scopes: state.scopes, sessions: state.sessions });
    const { relay, calls } = fixture(async (_path, options) => {
      if (options.method === 'GET') return state;
      throw Object.assign(new Error('private owned setup error'), {
        status: 409, body: { error_code: 'setup_owned', error: 'Desktop setup owns this relay <script>x</script>.' },
      });
    }, { confirmStop: () => assert.fail('an idle relay needs no Stop confirmation') });
    await relay.setActive(true);
    assert.equal(await (operation === 'stop' ? relay.stop() : relay.deleteScope('peer-scope')), false);
    assert.match(relay.html(), /Desktop setup owns this relay &lt;script&gt;x&lt;\/script&gt;/);
    assert.match(relay.html(), /Remove Desktop setup before stopping relay or revoking its profile\./);
    assert.match(relay.html(), /request was not retried/);
    assert.doesNotMatch(relay.html(), /Let active streams|shutdown finish|generation changed|private owned setup error|<script>/);
    assert.deepEqual(calls.map(call => [call.options.method, call.path]), [
      ['GET', '/api/desktop-relay'],
      [operation === 'stop' ? 'POST' : 'DELETE', `/api/desktop-relay/${operation === 'stop' ? 'stop' : 'scopes/peer-scope'}`],
      ['GET', '/api/desktop-relay'],
    ]);
    assert.deepEqual({ scopes: state.scopes, sessions: state.sessions }, peers);
  }
});

test('owned-setup guidance depends on the exact HTTP status and error code, with other Stop errors retaining their guidance', async () => {
  for (const [status, error_code] of [[409, 'busy'], [409, 'setup_owned_other'], [503, 'setup_owned']]) {
    const state = configured();
    state.sessions.forEach(task => { task.in_flight = 0; });
    const { relay, calls } = fixture(async (_path, options) => {
      if (options.method === 'GET') return state;
      throw Object.assign(new Error('setup_owned'), { status, body: { error_code, error: 'setup_owned' } });
    });
    await relay.setActive(true);
    assert.equal(await relay.stop(), false);
    assert.doesNotMatch(relay.html(), /Remove Desktop setup before stopping/);
    assert.match(relay.html(), status === 409 ? /Relay is busy\. Let active streams/ : /Could not stop the relay/);
    assert.equal(mutations(calls).length, 1);
  }
});

test('global in-flight requests require one cancellable Stop confirmation even without observed session records', async () => {
  const state = ready(), approval = deferred(), confirmations = [];
  state.status.in_flight = 3;
  state.sessions = [];
  const { relay, calls } = fixture(async () => state, { confirmStop: counts => { confirmations.push(counts); return approval.promise; } });
  const page = new MockPage();
  await relay.mount(page);
  assert.match(page.innerHTML, /0 remembered · 0 receiving requests/);
  assert.match(page.innerHTML, /3 requests in progress \(some background requests are unidentified\)/);
  assert.match(page.innerHTML, /Stop relay · 3 in flight/);
  const stopping = page.fire('click', page.control('stop'));
  await relay.stop();
  await relay.refresh();
  assert.deepEqual(confirmations, [{ inFlight: 3, activeTasks: 0 }]);
  assert.match(actionTag(page.innerHTML, 'stop'), /aria-busy="true"[^>]*disabled/);
  assert.equal(mutations(calls).length, 0);
  approval.resolve(false);
  assert.equal(await stopping, false);
  assert.equal(calls.length, 1);
  assert.doesNotMatch(actionTag(page.innerHTML, 'stop'), /disabled/);
});

test('Stop counts the maximum of runtime and observed requests, with a safe legacy fallback and no zero-count prompt', async () => {
  for (const [global, observed, expected] of [
    [5, [1, 2], 5], [1, [1, 2], 3], [0, [1, 2], 3], [undefined, [1, 2], 3],
    [-1, [1], 1], ['8', [1], 1], [Number.MAX_SAFE_INTEGER + 1, [1], 1],
    ['<script>count-secret</script>', [], 0], [0, [], 0], [undefined, [], 0],
  ]) {
    const state = ready(), confirmations = [];
    if (global !== undefined) state.status.in_flight = global;
    state.status.env = { HTTPS_PROXY: 'status-count-env-secret' };
    state.sessions = observed.map((in_flight, index) => ({ ...state.sessions[0], session_id: `count/${index}`, in_flight }));
    const { relay, calls } = fixture(async (path, options) => {
      if (options.method === 'GET') return state;
      assert.equal(path, '/api/desktop-relay/stop');
      assert.deepEqual(JSON.parse(options.body), {});
      state.status = disabled().status;
      return {};
    }, { confirmStop: async counts => { confirmations.push(counts); return false; } });
    await relay.setActive(true);
    assert.match(relay.html(), new RegExp(`Stop relay · ${expected} in flight`));
    assert.doesNotMatch(relay.html(), /count-secret|status-count-env-secret/);
    assert.equal(await relay.stop(), expected === 0);
    assert.deepEqual(confirmations, expected > 0 ? [{ inFlight: expected, activeTasks: observed.length }] : []);
    assert.equal(mutations(calls).length, expected > 0 ? 0 : 1);
  }
});

test('revoking a scope removes its observed records while global admitted requests still trigger Stop confirmation', async () => {
  const state = ready(), confirmations = [];
  state.status.in_flight = 1;
  state.scopes = [state.scopes[0]];
  state.sessions = [state.sessions[0]];
  const { relay, calls } = fixture(async (path, options) => {
    if (options.method === 'GET') return state;
    assert.equal(path, '/api/desktop-relay/scopes/scope%2Fa');
    assert.equal(options.method, 'DELETE');
    state.scopes = [];
    state.sessions = [];
    return {};
  }, { confirmStop: async counts => { confirmations.push(counts); return false; } });
  await relay.setActive(true);
  assert.equal(await relay.deleteScope('scope/a'), true);
  assert.match(relay.html(), /0 remembered · 0 receiving requests/);
  assert.match(relay.html(), /1 request in progress \(some background requests are unidentified\)/);
  assert.equal(await relay.stop(), false);
  assert.deepEqual(confirmations, [{ inFlight: 1, activeTasks: 0 }]);
  assert.deepEqual(mutations(calls).map(call => [call.options.method, call.path]), [['DELETE', '/api/desktop-relay/scopes/scope%2Fa']]);
});

test('the Stop dialog reports global requests separately from observed tasks and keeps Cancel as initial focus', async () => {
  const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
  const from = source.indexOf('settingsPage.desktopRelay = createDesktopRelay({');
  const to = source.indexOf('\nconst settingsTab = ', from);
  const dialogs = [];
  const context = vm.createContext({
    settingsPage: {}, api() {}, createDesktopRelay: dependencies => dependencies,
    confirmDialog: options => { dialogs.push(options); return Promise.resolve(false); },
  });
  vm.runInContext(source.slice(from, to), context);
  assert.equal(await context.settingsPage.desktopRelay.confirmStop({ inFlight: 3, activeTasks: 0 }), false);
  assert.equal(dialogs[0].title, 'Stop the Desktop task relay?');
  assert.match(dialogs[0].message, /The relay has 3 requests in flight\./);
  assert.match(dialogs[0].message, /0 observed tasks are active\./);
  assert.doesNotMatch(dialogs[0].message, /0 active tasks have 3/);
  assert.equal(dialogs[0].initialFocus, 'cancel');
});

test('conversation titles and project basenames come from GET metadata, with escaped text and evidence-based client badges', async () => {
  const state = configured();
  state.scopes.forEach(scope => { scope.label = 'Claude Desktop'; });
  state.sessions[0] = { ...state.sessions[0], title: 'Fix <img src=x onerror=alert(1)> & "checkout"',
    project: '/fixture/private/folder/store<&>', title_source: 'desktop_metadata', client_kind: 'desktop' };
  state.sessions[1] = { ...state.sessions[1], title: 'Review payment flow', project: 'C:\\fixture\\payments\\',
    title_source: 'cli_session_index', client_kind: 'cli' };
  const { relay, calls } = fixture(async () => state);
  await relay.setActive(true);
  const html = relay.html();
  assert.match(html, /<strong>Fix &lt;img src=x onerror=alert\(1\)&gt; &amp; &quot;checkout&quot;<\/strong>/);
  assert.match(html, /<strong>Review payment flow<\/strong>/);
  assert.match(html, /desktop-relay-project">store&lt;&amp;&gt;<\/span>/);
  assert.match(html, /desktop-relay-project">payments<\/span>/);
  assert.match(html, /desktop-relay-badge">Claude Desktop<\/span>/);
  assert.match(html, /desktop-relay-badge">Claude Code CLI<\/span>/);
  assert.match(html, /<dt>Title source<\/dt><dd>desktop_metadata/);
  assert.doesNotMatch(html, /<img|href="[^\"]*payments|private\/folder/);
  assert.doesNotMatch(html.slice(0, html.indexOf('<details class="desktop-relay-advanced"')), /<strong>Claude Desktop<\/strong>/);
  assert.match(html, /Conversations use their existing Claude sign-in\. Choose an account only for a conversation you want to switch\./);
  assert.match(html, /desktop-relay-setup-status">Relay ready/);
  assert.doesNotMatch(html.slice(0, html.indexOf('data-relay-details')), /settings\.json|settings\.backup\.json/);
  assert.deepEqual(calls.map(call => call.options.method), ['GET']);
});

test('untitled sessions use distinct short IDs, never profile labels, prompts, guessed workers, or ordinal titles', async () => {
  const state = ready();
  state.scopes.forEach(scope => { scope.label = 'Claude Desktop'; });
  state.sessions = ['12345678-aaaa-1111', '12345678-bbbb-2222', '87654321-cccc-3333'].map((session_id, index) => ({
    ...state.sessions[0], session_id, title: index === 2 ? '   ' : undefined, in_flight: 0,
    agent_id: 'agent-without-parent', prompt: 'secret prompt title', user_agent: 'Claude Desktop',
  }));
  const { relay } = fixture(async () => state);
  await relay.setActive(true);
  const html = relay.html();
  assert.match(html, /<strong>Code session 12345678-aaa<\/strong>/);
  assert.match(html, /<strong>Code session 12345678-bbb<\/strong>/);
  assert.match(html, /<strong>Code session 87654321<\/strong>/);
  assert.doesNotMatch(html.slice(0, html.indexOf('<details class="desktop-relay-advanced"')), /secret prompt|<strong>Claude Desktop|desktop-relay-badge">(?:Worker|Claude Desktop)|Conversation [123]|Task [123]/);
});

test('Worker labels require explicit child or parent metadata and preserve an actual child title', async () => {
  const state = ready();
  state.sessions[0] = { ...state.sessions[0], title: 'Review schema', parent_session_id: 'parent-fixture' };
  state.sessions[1] = { ...state.sessions[1], client_kind: 'subagent' };
  const { relay } = fixture(async () => state);
  await relay.setActive(true);
  const html = relay.html();
  assert.equal([...html.matchAll(/desktop-relay-badge">Worker<\/span>/g)].length, 2);
  assert.match(html, /<strong>Review schema<\/strong>/);
  assert.match(html, /<dt>Parent<\/dt><dd>parent-fixture/);
  assert.doesNotMatch(html, /Subagent [123]|Worker [123]/);
});

test('all idle sessions are inside a closed recent disclosure and activity order uses ISO, Unix, and stable scoped ties', async () => {
  const state = configured();
  state.sessions = [
    { ...state.sessions[0], scope_id: 'z-scope', session_id: 'z-session', title: 'Unix recent', last_seen: 1791028680, in_flight: 0 },
    { ...state.sessions[0], scope_id: 'a-scope', session_id: 'a-session', title: 'ISO recent', last_seen: '2026-10-03T11:58:00Z', in_flight: 0 },
    { ...state.sessions[0], session_id: 'old-session', title: 'Older conversation', last_seen: '2026-10-01T12:00:00Z', in_flight: 0 },
    { ...state.sessions[0], session_id: 'invalid-session', title: 'Missing activity', last_seen: 'not-a-date', in_flight: 0 },
  ];
  const { relay } = fixture(async () => state);
  const page = new FocusPage();
  await relay.mount(page);
  assert.equal(page.details.get('recent').open, false);
  assert.match(page.innerHTML, /No requests in progress/);
  assert.match(page.innerHTML, /Recent conversations \(4\)/);
  assert.match(page.innerHTML, /Includes idle and previously used sessions\. This does not mean they are running\./);
  assert.doesNotMatch(page.innerHTML, /Currently responding|0 active tasks|Archived|Closed conversation|NaN|Invalid Date/);
  assert.ok(page.innerHTML.indexOf('<strong>ISO recent') < page.innerHTML.indexOf('<strong>Unix recent'));
  assert.ok(page.innerHTML.indexOf('<strong>Unix recent') < page.innerHTML.indexOf('<strong>Older conversation'));
  assert.ok(page.innerHTML.indexOf('<strong>Older conversation') < page.innerHTML.indexOf('<strong>Missing activity'));
  assert.match(rowHTML(page.innerHTML, 'a-scope', 'a-session'), /Last request <time datetime="2026-10-03T11:58:00.000Z" title="[^"]+">2 min ago/);
  assert.match(rowHTML(page.innerHTML, 'scope/a', 'invalid-session'), /Activity time unavailable/);
  page.details.get('recent').open = true;
  page.fire('toggle', page.details.get('recent'));
  await relay.refresh();
  assert.equal(page.details.get('recent').open, true);
  const replacement = new FocusPage();
  await relay.mount(replacement);
  assert.equal(replacement.details.get('recent').open, true);
  replacement.details.get('recent').open = false;
  replacement.fire('toggle', replacement.details.get('recent'));
  await relay.refresh();
  assert.equal(replacement.details.get('recent').open, false);
});

test('request activity handles ISO and Unix seconds, missing and invalid times, and clamps future relative age', () => {
  const now = Date.parse('2026-10-03T12:00:00Z');
  for (const value of [undefined, null, '', 'not-a-date', 0, -1, Infinity, NaN, {}, '0001-01-01T00:00:00Z']) {
    assert.equal(desktopRelayActivity(value, now), null);
  }
  const iso = desktopRelayActivity('2026-10-03T11:58:00Z', now);
  assert.deepEqual(desktopRelayActivity(1791028680, now), iso);
  assert.deepEqual(desktopRelayActivity('1791028680', now), iso);
  assert.equal(iso.label, '2 min ago');
  assert.equal(iso.iso, '2026-10-03T11:58:00.000Z');
  assert.ok(iso.title);
  assert.equal(desktopRelayActivity('2026-10-03T11:59:45Z', now).label, 'Now');
  assert.equal(desktopRelayActivity('2026-10-03T10:00:00Z', now).label, '2 hr ago');
  assert.equal(desktopRelayActivity('2026-10-04T12:00:00Z', now).label, 'Now');
  assert.match(desktopRelayActivity('2026-10-01T12:00:00Z', now).label, /2026/);
});

test('only in-flight rows are primary and unidentified runtime requests never create synthetic workers', async () => {
  const state = configured();
  state.status.in_flight = 4;
  state.sessions[0].title = 'Receiving conversation';
  state.sessions[1].title = 'Remembered conversation';
  const { relay } = fixture(async () => state);
  await relay.setActive(true);
  const html = relay.html(), beforeRecent = html.slice(0, html.indexOf('<details class="desktop-relay-recent"'));
  assert.match(beforeRecent, /Currently responding/);
  assert.match(beforeRecent, /<strong>Receiving conversation/);
  assert.doesNotMatch(beforeRecent, /Remembered conversation|No requests in progress/);
  assert.match(beforeRecent, /2 remembered · 1 receiving a request/);
  assert.match(beforeRecent, /4 requests in progress \(some background requests are unidentified\)/);
  assert.equal([...html.matchAll(/class="desktop-relay-task"/g)].length, 2);
  assert.doesNotMatch(html, /desktop-relay-badge">Worker/);
});

test('apply requires a changed choice while an unavailable binding still offers an explicit default reset', async () => {
  const state = ready();
  state.sessions[0].account_id = '';
  const { relay, calls } = fixture(async () => state);
  await relay.setActive(true);
  let html = rowHTML(relay.html(), 'scope/a', 'session/a');
  assert.match(actionTag(html, 'use'), /disabled/);
  relay.chooseAccount('scope/a', 'session/a', 'b');
  html = rowHTML(relay.html(), 'scope/a', 'session/a');
  assert.doesNotMatch(actionTag(html, 'use'), /disabled/);
  assert.equal(calls.length, 1);
  relay.chooseAccount('scope/a', 'session/a', '');
  assert.match(actionTag(rowHTML(relay.html(), 'scope/a', 'session/a'), 'use'), /disabled/);
  state.sessions[0].account_id = 'missing-account';
  await relay.refresh();
  html = rowHTML(relay.html(), 'scope/a', 'session/a');
  assert.doesNotMatch(actionTag(html, 'use'), /disabled/);
  assert.match(html, />Use Claude sign-in<\/button>/);
  assert.match(html, /Selected: Stored account unavailable/);
  assert.equal(await relay.useTask('scope/a', 'session/a', undefined), false);
  assert.equal(mutations(calls).length, 0);
});

test('metadata title updates preserve scoped choice and focus without crossing same-session peers', async () => {
  const state = ready();
  state.sessions.forEach((task, index) => { task.title = index ? 'Peer title' : 'Initial title'; task.in_flight = 1; });
  const { relay, calls } = fixture(async () => state);
  const page = new FocusPage();
  await relay.mount(page);
  const key = '["scope/a","session/a"]';
  relay.chooseAccount('scope/a', 'session/a', 'b');
  page.nodes.find(node => node.dataset.relayFocus === key).focus();
  state.sessions[0].title = 'Updated from Desktop metadata';
  state.sessions[0].project = '/fixture/new-project';
  state.sessions[0].title_source = 'desktop_metadata';
  await relay.refresh();
  assert.equal(page.ownerDocument.activeElement.dataset.relayFocus, key);
  assert.match(rowHTML(page.innerHTML, 'scope/a', 'session/a'), /<strong>Updated from Desktop metadata/);
  assert.match(rowHTML(page.innerHTML, 'scope/a', 'session/a'), /value="b" selected/);
  assert.match(rowHTML(page.innerHTML, 'peer-scope', 'session/a'), /value="a" selected/);
  assert.match(rowHTML(page.innerHTML, 'peer-scope', 'session/a'), /<strong>Peer title/);
  assert.equal(mutations(calls).length, 0);
});

test('a focused conversation moving to idle during a switch expands recent and restores focus after the pending guard', async () => {
  const state = ready(), completion = deferred();
  const { relay, calls } = fixture(async (_path, options) => {
    if (options.method === 'GET') return state;
    await completion.promise;
    state.sessions[0] = { ...state.sessions[0], account_id: 'b', revision: 8, in_flight: 0, title: 'Now idle' };
    return {};
  });
  const page = new FocusPage();
  await relay.mount(page);
  const key = '["scope/a","session/a"]';
  relay.chooseAccount('scope/a', 'session/a', 'b');
  page.nodes.find(node => node.dataset.relayFocus === key + ':use').focus();
  const switching = relay.useTask('scope/a', 'session/a', 'b');
  assert.equal(page.ownerDocument.activeElement, page.ownerDocument.body);
  assert.equal(relay.chooseAccount('peer-scope', 'session/a', 'b'), false);
  await relay.useTask('scope/a', 'session/a', 'b');
  completion.resolve();
  assert.equal(await switching, true);
  assert.equal(page.details.get('recent').open, true);
  assert.equal(page.ownerDocument.activeElement.dataset.relayFocus, key);
  assert.match(rowHTML(page.innerHTML, 'scope/a', 'session/a'), /Selected: b@example.test/);
  assert.match(actionTag(rowHTML(page.innerHTML, 'scope/a', 'session/a'), 'use'), /disabled/);
  assert.deepEqual(mutations(calls).map(call => JSON.parse(call.options.body)), [{ account_id: 'b', revision: 7 }]);
});

test('relative activity updates with ordinary dashboard paints without new relay requests or timers', async () => {
  const state = ready();
  state.sessions[0].last_seen = '2026-10-03T12:00:00Z';
  let clock = Date.parse('2026-10-03T12:00:00Z');
  const { relay, calls } = fixture(async () => state, { now: () => clock });
  await relay.setActive(true);
  assert.match(rowHTML(relay.html(), 'scope/a', 'session/a'), />Now<\/time>/);
  clock += 3 * 60000;
  await relay.update();
  assert.match(rowHTML(relay.html(), 'scope/a', 'session/a'), />3 min ago<\/time>/);
  assert.equal(calls.length, 1);
});

test('configured empty state invites a message and setup guidance never demands a restart after account changes', async () => {
  const state = configured();
  state.sessions = [];
  const { relay } = fixture(async () => state);
  await relay.setActive(true);
  assert.match(relay.html(), /Start a Code conversation, send a message, then refresh\./);
  assert.match(relay.html(), /desktop-relay-setup-status">Relay ready/);
  assert.match(relay.html(), /Restart only after changing setup\. Changing an account does not require restart\./);
  assert.doesNotMatch(relay.html(), /Configured\. Restart Claude Desktop|active tasks|Archived/);
});

const collisionState = () => {
  const state = configured();
  state.scopes = [
    { id: '12345678-aaaa-scope', label: 'Claude Desktop' },
    { id: '12345678-bbbb-scope', label: 'Claude Desktop' },
  ];
  state.sessions = state.scopes.map(scope => ({ ...state.sessions[0], scope_id: scope.id,
    session_id: '11111111-1111-4111-8111-111111111111', title: 'Review checkout',
    project: '/fixture/projects/store', account_id: 'a', in_flight: 0, last_seen: '2026-10-03T11:58:00Z' }));
  return state;
};

test('identical titles, projects, accounts and times expose distinct profiles with collision-safe scope prefixes', async () => {
  const state = collisionState();
  const { relay, calls } = fixture(async () => state);
  await relay.setActive(true);
  const html = relay.html();
  const first = visibleRowHTML(html, state.scopes[0].id, state.sessions[0].session_id);
  const second = visibleRowHTML(html, state.scopes[1].id, state.sessions[1].session_id);
  assert.match(first, /desktop-relay-disambiguator">Profile Claude Desktop · 12345678-aaa<\/span>/);
  assert.match(second, /desktop-relay-disambiguator">Profile Claude Desktop · 12345678-bbb<\/span>/);
  for (const visible of [first, second]) {
    assert.match(visible, /<strong>Review checkout<\/strong>/);
    assert.match(visible, /desktop-relay-project">store<\/span>/);
    assert.match(visible, /Account for this conversation/);
    assert.match(visible, /Selected: a@example.test/);
    assert.doesNotMatch(visible, /desktop-relay-disambiguator">Session|<details/);
  }
  assert.deepEqual(calls.map(call => call.options.method), ['GET']);
});

test('unique profile labels distinguish a shared session without extra scope IDs and escape untrusted labels', async () => {
  const state = collisionState();
  state.scopes[0].label = 'Workspace <A> & "one"';
  state.scopes[1].label = 'Workspace B';
  const { relay } = fixture(async () => state);
  await relay.setActive(true);
  const first = visibleRowHTML(relay.html(), state.scopes[0].id, state.sessions[0].session_id);
  const second = visibleRowHTML(relay.html(), state.scopes[1].id, state.sessions[1].session_id);
  assert.match(first, /desktop-relay-disambiguator">Profile Workspace &lt;A&gt; &amp; &quot;one&quot;<\/span>/);
  assert.match(second, /desktop-relay-disambiguator">Profile Workspace B<\/span>/);
  assert.doesNotMatch(first + second, /<A>|Profile [^<]* · 12345678|href=/);
});

test('rendered whitespace and project basenames define collisions, with session prefixes extending past shared eight characters', async () => {
  const state = collisionState();
  state.sessions[0].title = 'Review  checkout';
  state.sessions[1].title = ' Review\ncheckout ';
  state.sessions[1].project = 'C:\\fixture\\other-folder\\store\\';
  state.sessions[0].session_id = '12345678-abcd-1111';
  state.sessions[1].session_id = '12345678-abcd-2222';
  const { relay } = fixture(async () => state);
  await relay.setActive(true);
  const first = visibleRowHTML(relay.html(), state.scopes[0].id, state.sessions[0].session_id);
  const second = visibleRowHTML(relay.html(), state.scopes[1].id, state.sessions[1].session_id);
  assert.match(first, /desktop-relay-disambiguator">Session 12345678-abcd-11<\/span>/);
  assert.match(second, /desktop-relay-disambiguator">Session 12345678-abcd-22<\/span>/);
  assert.doesNotMatch(first + second, /desktop-relay-disambiguator">Profile/);
});

test('untitled shared UUIDs visibly distinguish profiles while unique fallback titles need no extra identifiers', async () => {
  const state = collisionState();
  state.sessions.forEach(task => { task.title = ''; });
  const { relay } = fixture(async () => state);
  await relay.setActive(true);
  let first = visibleRowHTML(relay.html(), state.scopes[0].id, state.sessions[0].session_id);
  let second = visibleRowHTML(relay.html(), state.scopes[1].id, state.sessions[1].session_id);
  assert.match(first, /<strong>Code session 11111111<\/strong>/);
  assert.match(second, /<strong>Code session 11111111<\/strong>/);
  assert.match(first, /Profile Claude Desktop · 12345678-aaa/);
  assert.match(second, /Profile Claude Desktop · 12345678-bbb/);
  state.sessions[1].session_id = '22222222-2222-4222-8222-222222222222';
  await relay.refresh();
  first = visibleRowHTML(relay.html(), state.scopes[0].id, state.sessions[0].session_id);
  second = visibleRowHTML(relay.html(), state.scopes[1].id, state.sessions[1].session_id);
  assert.match(first, /<strong>Code session 11111111<\/strong>/);
  assert.match(second, /<strong>Code session 22222222<\/strong>/);
  assert.doesNotMatch(first + second, /desktop-relay-disambiguator/);
});

test('mixed collision groups distinguish duplicate scoped UUIDs and a third session with only the required identifiers', async () => {
  const state = collisionState();
  state.sessions.push({ ...state.sessions[0], session_id: '22222222-2222-4222-8222-222222222222' });
  const { relay } = fixture(async () => state);
  await relay.setActive(true);
  for (const task of state.sessions.slice(0, 2)) {
    const visible = visibleRowHTML(relay.html(), task.scope_id, task.session_id);
    assert.match(visible, /desktop-relay-disambiguator">Session 11111111<\/span>/);
    assert.match(visible, /desktop-relay-disambiguator">Profile Claude Desktop · 12345678-/);
  }
  const third = visibleRowHTML(relay.html(), state.sessions[2].scope_id, state.sessions[2].session_id);
  assert.match(third, /desktop-relay-disambiguator">Session 22222222<\/span>/);
  assert.doesNotMatch(third, /desktop-relay-disambiguator">Profile/);
});

test('collision hints appear and disappear after metadata updates while scoped choices and keyboard focus stay intact', async () => {
  const state = collisionState();
  state.sessions[1].title = 'Review deployment';
  const { relay, calls } = fixture(async () => state);
  const page = new FocusPage();
  await relay.mount(page);
  page.details.get('recent').open = true;
  page.fire('toggle', page.details.get('recent'));
  const selected = state.sessions[1], peer = state.sessions[0];
  const key = JSON.stringify([selected.scope_id, selected.session_id]);
  relay.chooseAccount(selected.scope_id, selected.session_id, 'b');
  page.nodes.find(node => node.dataset.relayFocus === key).focus();
  assert.doesNotMatch(visibleRowHTML(page.innerHTML, selected.scope_id, selected.session_id), /desktop-relay-disambiguator/);
  selected.title = peer.title;
  selected.title_source = 'desktop';
  await relay.refresh();
  assert.equal(page.ownerDocument.activeElement.dataset.relayFocus, key);
  assert.match(visibleRowHTML(page.innerHTML, selected.scope_id, selected.session_id), /Profile Claude Desktop · 12345678-bbb/);
  assert.match(rowHTML(page.innerHTML, selected.scope_id, selected.session_id), /value="b" selected/);
  assert.match(rowHTML(page.innerHTML, peer.scope_id, peer.session_id), /value="a" selected/);
  selected.project = '/fixture/projects/deployment';
  await relay.refresh();
  assert.equal(page.ownerDocument.activeElement.dataset.relayFocus, key);
  assert.doesNotMatch(visibleRowHTML(page.innerHTML, selected.scope_id, selected.session_id), /desktop-relay-disambiguator/);
  assert.doesNotMatch(visibleRowHTML(page.innerHTML, peer.scope_id, peer.session_id), /desktop-relay-disambiguator/);
  assert.match(rowHTML(page.innerHTML, selected.scope_id, selected.session_id), /value="b" selected/);
  assert.equal(mutations(calls).length, 0);
});

test('month-old idle CLI records report relay readiness without implying a current Desktop connection', async () => {
  for (const listening of [true, false]) {
    const state = configured();
    state.status.listening = listening;
    state.status.in_flight = 0;
    state.sessions = [{ ...state.sessions[0], title: 'Stored CLI conversation', title_source: 'cli', client_kind: 'cli',
      in_flight: 0, last_seen: '2026-09-03T12:00:00Z' }];
    const { relay } = fixture(async () => state);
    await relay.setActive(true);
    const html = relay.html();
    assert.match(html, listening ? /desktop-relay-setup-status">Relay ready<\/strong>/ : /desktop-relay-setup-status">Configured<\/strong>/);
    assert.match(html, /1 remembered · 0 receiving requests/);
    assert.match(html, /No requests in progress/);
    assert.match(html, /desktop-relay-badge">Claude Code CLI<\/span>/);
    assert.doesNotMatch(html, /Connected|Currently responding|desktop-relay-badge">Claude Desktop<\/span>/);
  }
});

const conversationID = '92ef0000-0000-4000-8000-000000000001';
const opusID = '11111111-1111-4111-8111-111111111111';
const haikuID = '22222222-2222-4222-8222-222222222222';
const futureID = '33333333-3333-4333-8333-333333333333';
const conversationPath = `/api/desktop-relay/scopes/scope%2Fa/conversations/${conversationID}/account`;
const conversationState = () => ({ ...configured(), conversation_bindings: [], sessions: [
  { scope_id: 'scope/a', conversation_id: conversationID, session_id: opusID, title: 'Salenza', project: '/fixture/projects/salenza',
    title_source: 'desktop', client_kind: 'desktop', association_verified: true, account_id: 'b', revision: 7, requests: 4, in_flight: 1,
    last_seen: '2026-10-03T11:58:00Z', model: 'claude-opus-fixture',
    last_response: { route: 'selected', account_id: 'b', model: 'claude-opus-fixture', status: 200, at: '2026-10-03T11:58:00Z', sequence: 4 } },
  { scope_id: 'scope/a', conversation_id: conversationID, session_id: haikuID, title: 'Salenza', project: '/fixture/projects/salenza',
    title_source: 'desktop', client_kind: 'desktop', association_verified: true, account_id: '', revision: 2, requests: 3, in_flight: 0,
    last_seen: '2026-10-03T11:59:00Z', model: 'claude-haiku-fixture',
    last_response: { route: 'caller', model: 'claude-haiku-fixture', status: 200, at: '2026-10-03T11:59:00Z', sequence: 5 } },
] });
const expectedMembers = { [opusID]: 7, [haikuID]: 2 };

test('verified Desktop aliases render one conversation with aggregated activity and honest partial selection plus both upstream responses', async () => {
  const state = conversationState();
  state.sessions[1].in_flight = 2;
  const { relay, calls } = fixture(async () => state);
  await relay.setActive(true);
  const html = relay.html(), row = rowHTML(html, 'scope/a', conversationID);
  const visible = visibleRowHTML(html, 'scope/a', conversationID);
  assert.equal([...html.matchAll(/class="desktop-relay-task"/g)].length, 1);
  assert.match(row, new RegExp(`data-relay-conversation="${conversationID}"`));
  assert.match(visible, /<strong>Salenza<\/strong>/);
  assert.match(html, /1 remembered · 1 receiving a request/);
  assert.match(visible, /3 requests in progress/);
  assert.match(visible, /datetime="2026-10-03T11:59:00.000Z"[^>]*>1 min ago/);
  assert.match(visible, /Partially applied/);
  assert.match(visible, /value="b" selected/);
  assert.doesNotMatch(actionTag(visible, 'use'), /disabled/);
  assert.match(visible, />Apply to whole conversation<\/button>/);
  assert.match(visible, /Previous selection covered one internal session\. Apply it to the whole conversation\./);
  assert.doesNotMatch(visible, /Selected: b@example.test|>Override<\/span>/);
  assert.match(visible, /Last upstream response: Claude caller credential, HTTP 200 · claude-haiku-fixture/);
  assert.match(row, /Internal request sessions \(2\)/);
  assert.match(row, /<dt>Requests<\/dt><dd>7<\/dd>/);
  assert.match(row, /Last upstream response: selected-account credential b@example.test, HTTP 200 · claude-opus-fixture/);
  assert.match(row, new RegExp(`<code[^>]*>${opusID}<\\/code>`));
  assert.match(row, new RegExp(`<code[^>]*>${haikuID}<\\/code>`));
  assert.match(html, /This changes the account used for conversation messages\. Desktop sign-in and its usage display stay on the original account\./);
  assert.deepEqual(calls.map(call => call.options.method), ['GET']);
});

test('explicit whole-conversation Apply sends binding revision zero and every member revision, preserving other scopes and future verified aliases', async () => {
  const state = conversationState();
  const peer = { ...state.sessions[0], scope_id: 'peer-scope', account_id: 'a', revision: 11, in_flight: 0 };
  const unknown = { ...state.sessions[1], session_id: futureID, conversation_id: '', client_kind: 'cli', account_id: '', revision: 3 };
  state.sessions.push(peer, unknown);
  const originalPeer = structuredClone(peer), originalUnknown = structuredClone(unknown);
  const { relay, calls } = fixture(async (path, options) => {
    if (options.method === 'GET') return state;
    assert.equal(path, conversationPath);
    assert.equal(options.method, 'POST');
    assert.deepEqual(JSON.parse(options.body), { account_id: 'b', revision: 0, member_revisions: expectedMembers });
    assert.equal(options.controllerKey, 'fixture-control-key');
    assert.equal(options.headers['Content-Type'], 'application/json');
    state.conversation_bindings.push({ scope_id: 'scope/a', conversation_id: conversationID, account_id: 'b', revision: 1 });
    for (const member of state.sessions.filter(task => task.scope_id === 'scope/a' && task.conversation_id === conversationID)) {
      member.account_id = 'b'; member.revision++;
    }
    return {};
  });
  const page = new MockPage();
  await relay.mount(page);
  const before = mutations(calls).length;
  relay.chooseAccount('scope/a', opusID, 'b');
  assert.equal(mutations(calls).length, before);
  assert.equal(await page.fire('click', page.control('use', 'scope/a', conversationID)), true);
  let visible = visibleRowHTML(page.innerHTML, 'scope/a', conversationID);
  assert.match(visible, /Selected: b@example.test/);
  assert.match(visible, />Override<\/span>/);
  assert.doesNotMatch(visible, /Partially applied/);
  assert.match(actionTag(visible, 'use'), /disabled/);
  assert.deepEqual(peer, originalPeer);
  assert.deepEqual(unknown, originalUnknown);
  state.sessions.push({ ...state.sessions[1], session_id: '44444444-4444-4444-8444-444444444444', account_id: 'b', revision: 0,
    last_response: null, model: 'future-verified-model' });
  await relay.refresh();
  visible = visibleRowHTML(page.innerHTML, 'scope/a', conversationID);
  assert.match(visible, /Selected: b@example.test/);
  assert.match(rowHTML(page.innerHTML, 'scope/a', conversationID), /Internal request sessions \(3\)/);
  assert.match(visibleRowHTML(page.innerHTML, 'scope/a', futureID), /Default: Claude sign-in/);
  assert.equal(mutations(calls).length, 1, 'new alias inheritance comes from GET, never a second frontend mutation');
  assert.ok(calls.every(call => call.path.startsWith('/api/desktop-relay')));
});

test('persisted group binding takes precedence over legacy member selections and uses its own current revision', async () => {
  const state = conversationState();
  state.conversation_bindings = [{ scope_id: 'scope/a', conversation_id: conversationID, account_id: 'a', revision: 9 }];
  const { relay, calls } = fixture(async (path, options) => {
    if (options.method === 'GET') return state;
    assert.equal(path, conversationPath);
    assert.deepEqual(JSON.parse(options.body), { account_id: 'b', revision: 9, member_revisions: expectedMembers });
    state.conversation_bindings[0] = { ...state.conversation_bindings[0], account_id: 'b', revision: 10 };
    return {};
  });
  await relay.setActive(true);
  let visible = visibleRowHTML(relay.html(), 'scope/a', conversationID);
  assert.match(visible, /Selected: a@example.test/);
  assert.match(visible, /value="a" selected/);
  assert.doesNotMatch(visible, /Partially applied|Mixed selections/);
  assert.match(actionTag(visible, 'use'), /disabled/);
  assert.equal(relay.chooseAccount('scope/a', haikuID, 'b'), true);
  assert.equal(await relay.useTask('scope/a', haikuID, 'b'), true);
  visible = visibleRowHTML(relay.html(), 'scope/a', conversationID);
  assert.match(visible, /Selected: b@example.test/);
  assert.match(visible, /Last upstream response: Claude caller credential/);
  assert.equal(mutations(calls).length, 1);
});

test('unowned mixed legacy selections require Start before an initial verified whole-conversation reset', async () => {
  const state = conversationState();
  state.sessions[1].account_id = 'a';
  state.status = disabled().status;
  state.sessions.forEach(task => { task.in_flight = 0; });
  const { relay, calls } = fixture(async (path, options) => {
    if (options.method === 'GET') return state;
    assert.equal(path, conversationPath);
    assert.equal(options.method, 'DELETE');
    assert.deepEqual(JSON.parse(options.body), { revision: 0, member_revisions: expectedMembers });
    state.sessions.forEach(task => { task.account_id = ''; task.revision++; });
    state.conversation_bindings.push({ scope_id: 'scope/a', conversation_id: conversationID, account_id: '', revision: 1 });
    return {};
  });
  const page = new MockPage();
  await relay.mount(page);
  let visible = visibleRowHTML(page.innerHTML, 'scope/a', conversationID);
  assert.match(visible, /Mixed selections/);
  assert.match(visible, /option value="" selected>Default: Claude sign-in/);
  assert.match(visible, /Choose one account for the whole conversation, or use Claude sign-in to reset all its internal sessions\./);
  assert.match(visible, />Use Claude sign-in<\/button>/);
  assert.match(actionTag(visible, 'use'), /disabled/);
  assert.match(visible, /Start relay in Advanced and refresh status before restoring Claude sign-in/);
  assert.doesNotMatch(visible, /Claude sign-in can be restored while the relay is stopped/);
  assert.equal(await relay.useTask('scope/a', conversationID, ''), false);
  assert.equal(await page.fire('click', page.control('use', 'scope/a', conversationID)), false);
  assert.equal(mutations(calls).length, 0);
  state.status = ready().status;
  await relay.refresh();
  visible = visibleRowHTML(page.innerHTML, 'scope/a', conversationID);
  assert.doesNotMatch(actionTag(visible, 'use'), /disabled/);
  assert.equal(await page.fire('click', page.control('use', 'scope/a', conversationID)), true);
  visible = visibleRowHTML(page.innerHTML, 'scope/a', conversationID);
  assert.match(visible, /Default: Claude sign-in/);
  assert.doesNotMatch(visible, /Mixed selections|Partially applied|>Override/);
  assert.match(actionTag(visible, 'use'), /disabled/);
  assert.deepEqual(mutations(calls).map(call => call.path), [conversationPath]);
});

test('an unavailable stored group binding can reset with its binding and member revisions, while omitted or non-Claude choices never mutate', async () => {
  const state = conversationState();
  state.conversation_bindings = [{ scope_id: 'scope/a', conversation_id: conversationID, account_id: 'missing-account', revision: 5 }];
  const { relay, calls } = fixture(async (_path, options) => options.method === 'GET' ? state : {});
  await relay.setActive(true);
  const visible = visibleRowHTML(relay.html(), 'scope/a', conversationID);
  assert.match(visible, /Selected: Stored account unavailable/);
  assert.match(visible, />Use Claude sign-in<\/button>/);
  assert.doesNotMatch(actionTag(visible, 'use'), /disabled/);
  assert.equal(await relay.useTask('scope/a', conversationID, undefined), false);
  assert.equal(await relay.useTask('scope/a', conversationID, 'codex'), false);
  assert.equal(await relay.useTask('scope/a', conversationID, ''), true);
  assert.deepEqual(JSON.parse(mutations(calls)[0].options.body), { revision: 5, member_revisions: expectedMembers });
  assert.equal(mutations(calls)[0].options.method, 'DELETE');
});

test('equal title, model and project never authorize grouping, and cached bindings cannot associate unmapped sessions', async () => {
  const state = conversationState();
  state.sessions[1].conversation_id = '';
  state.sessions[1].model = state.sessions[0].model;
  state.sessions[1].request = { conversation_id: conversationID };
  state.sessions[1].metadata = { conversation_id: conversationID };
  state.conversation_bindings = [{ scope_id: 'scope/a', conversation_id: conversationID, account_id: 'b', revision: 3 }];
  state.sessions.push({ ...state.sessions[0], session_id: futureID, conversation_id: '92ef0000-0000-4000-8000-000000000002', account_id: '' });
  const { relay, calls } = fixture(async (_path, options) => options.method === 'GET' ? state : {});
  await relay.setActive(true);
  const html = relay.html();
  assert.equal([...html.matchAll(/class="desktop-relay-task"/g)].length, 3);
  assert.match(visibleRowHTML(html, 'scope/a', haikuID), /Default: Claude sign-in/);
  assert.match(rowHTML(html, 'scope/a', conversationID), /Internal request sessions \(1\)/);
  assert.match(visibleRowHTML(html, 'scope/a', conversationID), /Conversation 92ef0000-0000-40/);
  assert.equal(await relay.useTask('scope/a', haikuID, 'b'), true);
  assert.equal(mutations(calls)[0].path, `/api/desktop-relay/scopes/scope%2Fa/sessions/${haikuID}/account`);
  assert.deepEqual(JSON.parse(mutations(calls)[0].options.body), { account_id: 'b', revision: 2 });
});

test('invalid group or member revisions and noncanonical member IDs block unsafe CAS bodies without object-prototype effects', async () => {
  for (const invalid of [
    state => { state.sessions[1].revision = undefined; }, state => { state.sessions[1].revision = -1; },
    state => { state.sessions[1].revision = '2'; }, state => { state.sessions[1].revision = Number.MAX_SAFE_INTEGER + 1; },
    state => { state.sessions[1].session_id = '__proto__'; }, state => { state.sessions[1].session_id = 'constructor'; },
    state => { state.sessions[1].session_id = 'AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA'; },
    state => { state.sessions[1].session_id = '00000000-0000-0000-0000-000000000000'; },
    state => { state.sessions.push({ ...state.sessions[1] }); },
    state => { state.conversation_bindings = [{ scope_id: 'scope/a', conversation_id: conversationID, account_id: 'b', revision: NaN }]; },
    state => { state.sessions.forEach(task => { task.conversation_id = '__proto__'; }); },
  ]) {
    const state = conversationState();
    invalid(state);
    const id = state.sessions[0].conversation_id;
    const { relay, calls } = fixture(async () => state);
    await relay.setActive(true);
    assert.match(actionTag(visibleRowHTML(relay.html(), 'scope/a', id), 'use'), /disabled/);
    assert.match(relay.html(), /Conversation revision details are incomplete/);
    assert.equal(await relay.useTask('scope/a', id, 'a'), false);
    assert.equal(mutations(calls).length, 0);
    assert.equal(Object.prototype.polluted, undefined);
  }
});

test('last upstream response describes the captured credential rather than the current selection, including failed and escaped responses', async () => {
  const state = conversationState();
  state.conversation_bindings = [{ scope_id: 'scope/a', conversation_id: conversationID, account_id: 'a', revision: 1 }];
  state.sessions[1].last_response = { route: 'selected', account_id: 'b', model: '<svg onload=x> Bearer private-token', status: 401,
    at: '2026-10-03T11:59:00Z', access_token: 'credential-secret', headers: { Authorization: 'Bearer hidden-header' }, sequence: 6 };
  const { relay } = fixture(async () => state);
  await relay.setActive(true);
  const visible = visibleRowHTML(relay.html(), 'scope/a', conversationID);
  assert.match(visible, /Selected: a@example.test/);
  assert.match(visible, /Last upstream response: selected-account credential b@example.test, HTTP 401 · request failed/);
  assert.match(visible, /desktop-relay-response-failed/);
  assert.match(visible, /&lt;svg onload=x&gt; Bearer \[redacted\]/);
  assert.doesNotMatch(relay.html(), /<svg|private-token|credential-secret|hidden-header|billing verified|Fixture-tested/);
  state.sessions[1].last_response = { route: 'caller', account_id: 'a', model: 'claude-haiku-fixture', status: 200, at: '2026-10-03T11:59:00Z' };
  await relay.refresh();
  const callerLine = visibleRowHTML(relay.html(), 'scope/a', conversationID).match(/<p[^>]*desktop-relay-response[^>]*>[\s\S]*?<\/p>/)?.[0];
  assert.match(callerLine, /Claude caller credential, HTTP 200/);
  assert.doesNotMatch(callerLine, /a@example.test|b@example.test|request failed/);
});

test('request counts, count_tokens responses and local blocks cannot manufacture upstream Messages evidence', async () => {
  const state = conversationState();
  for (const member of state.sessions) {
    member.last_response = null;
    member.requests = 400;
    member.last_count_tokens_response = { route: 'selected', account_id: 'b', model: 'token-count-model', status: 200, at: '2026-10-03T11:59:00Z' };
    member.local_response = { route: 'selected', account_id: 'b', model: 'locally-blocked-model', status: 401, at: '2026-10-03T11:59:00Z' };
  }
  const { relay } = fixture(async () => state);
  await relay.setActive(true);
  assert.match(visibleRowHTML(relay.html(), 'scope/a', conversationID), /No upstream message response recorded yet/);
  assert.doesNotMatch(relay.html(), /Last upstream response:|token-count-model|locally-blocked-model/);
  for (const invalid of [
    { route: 'blocked', status: 401, at: '2026-10-03T11:59:00Z' },
    { route: 'caller', status: 0, at: '2026-10-03T11:59:00Z' },
    { route: 'selected', status: '200', at: '2026-10-03T11:59:00Z' },
    { route: 'selected', status: 200, at: 'invalid' },
  ]) {
    state.sessions[1].last_response = invalid;
    await relay.refresh();
    assert.match(visibleRowHTML(relay.html(), 'scope/a', conversationID), /No upstream message response recorded yet/);
  }
});

test('upstream sequence breaks timestamp ties across members instead of showing a stale response', async () => {
  const state = conversationState();
  state.sessions[0].last_response.at = state.sessions[1].last_response.at;
  state.sessions[0].last_response.sequence = 9;
  const { relay } = fixture(async () => state);
  await relay.setActive(true);
  assert.match(visibleRowHTML(relay.html(), 'scope/a', conversationID), /selected-account credential b@example.test, HTTP 200 · claude-opus-fixture/);
});

test('group keys preserve pending choice, focus and history expansion as metadata changes and a verified alias arrives', async () => {
  const state = conversationState();
  state.sessions.forEach(task => { task.in_flight = 0; });
  const { relay, calls } = fixture(async () => state);
  const page = new FocusPage();
  await relay.mount(page);
  page.details.get('recent').open = true;
  page.fire('toggle', page.details.get('recent'));
  const key = JSON.stringify(['scope/a', conversationID]) + ':conversation';
  relay.chooseAccount('scope/a', opusID, 'a');
  page.nodes.find(node => node.dataset.relayFocus === key).focus();
  for (const task of state.sessions) { task.title = 'Updated Salenza title'; task.project = '/fixture/projects/new-project'; }
  state.sessions.unshift({ ...state.sessions[1], session_id: futureID, revision: 0 });
  await relay.refresh();
  assert.equal(page.ownerDocument.activeElement.dataset.relayFocus, key);
  assert.equal(page.details.get('recent').open, true);
  assert.match(visibleRowHTML(page.innerHTML, 'scope/a', conversationID), /<strong>Updated Salenza title/);
  assert.match(visibleRowHTML(page.innerHTML, 'scope/a', conversationID), /value="a" selected/);
  assert.match(rowHTML(page.innerHTML, 'scope/a', conversationID), /Internal request sessions \(3\)/);
  assert.equal(mutations(calls).length, 0);
});

test('group CAS conflicts refresh both binding and members without replaying or treating selection as new response evidence', async () => {
  const state = conversationState();
  const { relay, calls } = fixture(async (_path, options) => {
    if (options.method === 'GET') return state;
    state.conversation_bindings = [{ scope_id: 'scope/a', conversation_id: conversationID, account_id: 'a', revision: 3 }];
    state.sessions.push({ ...state.sessions[1], session_id: futureID, account_id: 'a', revision: 0 });
    throw Object.assign(new Error('private conflict details'), { status: 409 });
  });
  await relay.setActive(true);
  relay.chooseAccount('scope/a', conversationID, 'b');
  assert.equal(await relay.useTask('scope/a', conversationID, 'b'), false);
  assert.deepEqual(calls.map(call => call.options.method), ['GET', 'POST', 'GET']);
  assert.match(relay.html(), /generation changed/);
  assert.match(visibleRowHTML(relay.html(), 'scope/a', conversationID), /Selected: a@example.test/);
  assert.match(visibleRowHTML(relay.html(), 'scope/a', conversationID), /Last upstream response: Claude caller credential/);
  assert.doesNotMatch(relay.html(), /private conflict details/);
});

test('logout fences a pending group mutation, aborts it and clears recorded response evidence', async () => {
  const state = conversationState(), completion = deferred();
  const { relay, calls, context, paints } = fixture(async (_path, options) => options.method === 'GET' ? state : completion.promise);
  await relay.setActive(true);
  const applying = relay.useTask('scope/a', conversationID, 'b');
  context.locked = context.loggingOut = true;
  relay.clear();
  const paintCount = paints.length;
  completion.resolve({ conversation_bindings: [{ scope_id: 'scope/a', conversation_id: conversationID, account_id: 'b', revision: 1 }] });
  assert.equal(await applying, false);
  assert.equal(mutations(calls)[0].options.signal.aborted, true);
  assert.equal(calls.length, 2);
  assert.equal(paints.length, paintCount);
  assert.equal(relay.html(), '');
});

test('empty-account group records retain CAS revision but expose subsequent partial, mixed and default member selections', async () => {
  for (const [memberAccounts, selection] of [[['b', ''], 'partial'], [['a', 'b'], 'mixed'], [['', ''], 'default']]) {
    const state = conversationState();
    state.conversation_bindings = [{ scope_id: 'scope/a', conversation_id: conversationID, account_id: '', revision: 5 }];
    state.sessions.forEach((task, index) => { task.account_id = memberAccounts[index]; });
    const { relay, calls } = fixture(async (path, options) => {
      if (options.method === 'GET') return state;
      assert.equal(path, conversationPath);
      assert.deepEqual(JSON.parse(options.body), { ...(selection === 'default' ? { account_id: 'b' } : {}), revision: 5, member_revisions: expectedMembers });
      assert.equal(options.method, selection === 'default' ? 'POST' : 'DELETE');
      state.conversation_bindings[0] = { ...state.conversation_bindings[0], account_id: selection === 'default' ? 'b' : '', revision: 6 };
      state.sessions.forEach(task => { task.account_id = state.conversation_bindings[0].account_id; task.revision++; });
      return {};
    });
    await relay.setActive(true);
    let visible = visibleRowHTML(relay.html(), 'scope/a', conversationID);
    if (selection === 'partial') {
      assert.match(visible, /Partially applied/);
      assert.match(visible, /value="b" selected/);
      assert.doesNotMatch(actionTag(visible, 'use'), /disabled/);
      assert.equal(relay.chooseAccount('scope/a', conversationID, ''), true);
    } else if (selection === 'mixed') {
      assert.match(visible, /Mixed selections/);
      assert.match(visible, />Use Claude sign-in<\/button>/);
      assert.doesNotMatch(actionTag(visible, 'use'), /disabled/);
    } else {
      assert.match(visible, /Default: Claude sign-in/);
      assert.match(actionTag(visible, 'use'), /disabled/);
      assert.equal(relay.chooseAccount('scope/a', conversationID, 'b'), true);
    }
    assert.doesNotMatch(visible, />Override<\/span>/);
    assert.equal(await relay.useTask('scope/a', conversationID, selection === 'default' ? 'b' : ''), true);
    visible = visibleRowHTML(relay.html(), 'scope/a', conversationID);
    assert.match(visible, selection === 'default' ? /Selected: b@example.test/ : /Default: Claude sign-in/);
    assert.doesNotMatch(visible, /Partially applied|Mixed selections/);
    assert.match(actionTag(visible, 'use'), /disabled/);
    assert.equal(mutations(calls).length, 1);
  }
});

test('an exact-session override after a group reset remains visible and resets using the retained group revision', async () => {
  const state = conversationState();
  state.conversation_bindings = [{ scope_id: 'scope/a', conversation_id: conversationID, account_id: '', revision: 6 }];
  state.sessions[0].revision = 12;
  state.sessions[1].revision = 4;
  state.sessions.forEach(task => { task.in_flight = 0; });
  state.status = disabled().status;
  const { relay, calls } = fixture(async (_path, options) => {
    if (options.method === 'GET') return state;
    assert.equal(options.method, 'DELETE');
    assert.deepEqual(JSON.parse(options.body), { revision: 6, member_revisions: { [opusID]: 12, [haikuID]: 4 } });
    state.conversation_bindings[0].revision++;
    state.sessions.forEach(task => { task.account_id = ''; task.revision++; });
    return {};
  });
  await relay.setActive(true);
  assert.match(visibleRowHTML(relay.html(), 'scope/a', conversationID), /Partially applied/);
  relay.chooseAccount('scope/a', conversationID, '');
  assert.doesNotMatch(actionTag(visibleRowHTML(relay.html(), 'scope/a', conversationID), 'use'), /disabled/);
  assert.equal(await relay.useTask('scope/a', conversationID, ''), true);
  assert.equal(mutations(calls).length, 1);
});

test('global request-start sequences outrank inverted response completion times and retain the correct model', async () => {
  const state = conversationState();
  state.sessions[0].last_response = { route: 'caller', model: 'older-request-later-finish', status: 200, at: '2026-10-03T12:02:00Z', sequence: 10 };
  state.sessions[1].last_response = { route: 'selected', account_id: 'b', model: 'newer-request-earlier-finish', status: 200, at: '2026-10-03T12:01:00Z', sequence: 11 };
  const { relay } = fixture(async () => state);
  await relay.setActive(true);
  let visible = visibleRowHTML(relay.html(), 'scope/a', conversationID);
  assert.match(visible, /selected-account credential b@example.test, HTTP 200 · newer-request-earlier-finish/);
  assert.doesNotMatch(visible, /older-request-later-finish/);
  state.sessions[0].last_response.sequence = Number.MAX_SAFE_INTEGER - 1;
  state.sessions[1].last_response.sequence = Number.MAX_SAFE_INTEGER;
  await relay.refresh();
  visible = visibleRowHTML(relay.html(), 'scope/a', conversationID);
  assert.match(visible, /newer-request-earlier-finish/);
});

test('unknown, unsafe or equal sequence values fall back to response time rather than inventing request order', async () => {
  const state = conversationState();
  state.sessions[0].last_response = { route: 'caller', model: 'later-response', status: 200, at: '2026-10-03T12:02:00Z', sequence: 10 };
  state.sessions[1].last_response = { route: 'selected', account_id: 'b', model: 'earlier-response', status: 200, at: '2026-10-03T12:01:00Z', sequence: 11 };
  const { relay } = fixture(async () => state);
  await relay.setActive(true);
  for (const sequence of [undefined, 0, -1, NaN, Infinity, 1.5, '12', Number.MAX_SAFE_INTEGER + 1, 11]) {
    state.sessions[0].last_response.sequence = sequence;
    await relay.refresh();
    const visible = visibleRowHTML(relay.html(), 'scope/a', conversationID);
    assert.match(visible, /Claude caller credential, HTTP 200 · later-response/);
    assert.doesNotMatch(visible, /earlier-response/);
  }
});

const flatConversationState = () => {
  const state = conversationState();
  state.sessions.forEach(task => { task.conversation_id = ''; task.association_verified = false; task.account_id = ''; task.in_flight = 0; });
  return state;
};
const verifyAliases = state => state.sessions.filter(task => task.scope_id === 'scope/a').forEach(task => {
  task.conversation_id = conversationID; task.association_verified = true;
});

test('flat-to-group migration preserves the exact scoped member choice, focus and disclosures through first verified metadata', async () => {
  const state = flatConversationState();
  const peer = { ...state.sessions[0], scope_id: 'peer-scope' };
  state.sessions.push(peer);
  const { relay, calls } = fixture(async () => state);
  const page = new FocusPage();
  await relay.mount(page);
  page.details.get('recent').open = true;
  page.fire('toggle', page.details.get('recent'));
  const oldKey = JSON.stringify(['scope/a', opusID]), groupKey = JSON.stringify(['scope/a', conversationID]) + ':conversation';
  page.diagnostics.get(oldKey).open = true;
  page.fire('toggle', page.diagnostics.get(oldKey));
  relay.chooseAccount('peer-scope', opusID, 'a');
  relay.chooseAccount('scope/a', opusID, 'b');
  page.nodes.find(node => node.dataset.relayFocus === oldKey).focus();
  verifyAliases(state);
  state.sessions.filter(task => task.scope_id === 'scope/a').forEach(task => { task.title = 'Native title after lookup'; });
  await relay.refresh();
  assert.equal(page.ownerDocument.activeElement.dataset.relayFocus, groupKey);
  assert.equal(page.details.get('recent').open, true);
  assert.equal(page.diagnostics.get(groupKey).open, true);
  assert.match(visibleRowHTML(page.innerHTML, 'scope/a', conversationID), /value="b" selected/);
  assert.match(visibleRowHTML(page.innerHTML, 'peer-scope', opusID), /value="a" selected/);
  assert.doesNotMatch(page.innerHTML, /Review account choice/);
  assert.equal(mutations(calls).length, 0);
  const replacement = new FocusPage();
  await relay.mount(replacement, groupKey);
  assert.equal(replacement.ownerDocument.activeElement.dataset.relayFocus, groupKey);
  assert.equal(replacement.diagnostics.get(groupKey).open, true);
});

test('a focused flat UUID moves to that exact internal-member UUID instead of the aggregate conversation ID', async () => {
  const state = flatConversationState();
  const { relay } = fixture(async () => state);
  const page = new FocusPage();
  await relay.mount(page);
  const oldKey = JSON.stringify(['scope/a', haikuID]), groupKey = JSON.stringify(['scope/a', conversationID]) + ':conversation';
  page.details.get('recent').open = true;
  page.diagnostics.get(oldKey).open = true;
  page.nodes.find(node => node.dataset.relayFocus === oldKey + ':session').focus();
  verifyAliases(state);
  await relay.refresh();
  assert.equal(page.ownerDocument.activeElement.dataset.relayFocus, groupKey + ':member:' + haikuID);
  assert.equal(page.diagnostics.get(groupKey).open, true);
  assert.equal(page.details.get('recent').open, true);
});

test('different pending alias choices require a new explicit group choice, including an original-sign-in pending choice', async () => {
  for (const pending of [['a', 'b'], ['', 'b']]) {
    const state = flatConversationState();
    const { relay, calls } = fixture(async (_path, options) => options.method === 'GET' ? state : {});
    await relay.setActive(true);
    relay.chooseAccount('scope/a', opusID, pending[0]);
    relay.chooseAccount('scope/a', haikuID, pending[1]);
    verifyAliases(state);
    await relay.refresh();
    let visible = visibleRowHTML(relay.html(), 'scope/a', conversationID);
    assert.match(visible, /Review account choice/);
    assert.match(visible, /Choose an account again before applying to the whole conversation/);
    assert.match(visible, /option value="__relay_review__" selected disabled/);
    assert.match(actionTag(visible, 'use'), /disabled/);
    assert.equal(await relay.useTask('scope/a', conversationID, 'b'), false);
    assert.equal(await relay.useTask('scope/a', conversationID, ''), false);
    await relay.refresh();
    assert.match(visibleRowHTML(relay.html(), 'scope/a', conversationID), /Review account choice/);
    assert.equal(relay.chooseAccount('scope/a', conversationID, 'b'), true);
    visible = visibleRowHTML(relay.html(), 'scope/a', conversationID);
    assert.doesNotMatch(visible, /Review account choice/);
    assert.doesNotMatch(actionTag(visible, 'use'), /disabled/);
    assert.equal(await relay.useTask('scope/a', conversationID, 'b'), true);
    assert.deepEqual(JSON.parse(mutations(calls)[0].options.body), { account_id: 'b', revision: 0, member_revisions: expectedMembers });
    assert.equal(mutations(calls).length, 1);
  }
});

test('matching pending alias choices merge once and stale flat choices cannot resurrect when mapping disappears', async () => {
  const state = flatConversationState();
  const { relay, calls } = fixture(async () => state);
  await relay.setActive(true);
  relay.chooseAccount('scope/a', opusID, 'b');
  relay.chooseAccount('scope/a', haikuID, 'b');
  verifyAliases(state);
  await relay.refresh();
  assert.match(visibleRowHTML(relay.html(), 'scope/a', conversationID), /value="b" selected/);
  assert.doesNotMatch(relay.html(), /Review account choice/);
  state.sessions.forEach(task => { task.conversation_id = ''; task.association_verified = false; });
  await relay.refresh();
  for (const id of [opusID, haikuID]) assert.match(visibleRowHTML(relay.html(), 'scope/a', id), /option value="" selected/);
  verifyAliases(state);
  await relay.refresh();
  assert.match(visibleRowHTML(relay.html(), 'scope/a', conversationID), /option value="" selected/);
  assert.match(actionTag(visibleRowHTML(relay.html(), 'scope/a', conversationID), 'use'), /disabled/);
  assert.equal(mutations(calls).length, 0);
});

test('same-title unknown aliases stay separate and do not join another member pending choice or review state', async () => {
  const state = flatConversationState();
  const { relay, calls } = fixture(async () => state);
  await relay.setActive(true);
  relay.chooseAccount('scope/a', opusID, 'b');
  relay.chooseAccount('scope/a', haikuID, 'a');
  state.sessions[0].conversation_id = conversationID;
  state.sessions[0].association_verified = true;
  await relay.refresh();
  assert.equal([...relay.html().matchAll(/class="desktop-relay-task"/g)].length, 2);
  assert.match(visibleRowHTML(relay.html(), 'scope/a', conversationID), /value="b" selected/);
  assert.match(visibleRowHTML(relay.html(), 'scope/a', haikuID), /value="a" selected/);
  assert.doesNotMatch(relay.html(), /Review account choice/);
  assert.equal(mutations(calls).length, 0);
});

test('an obsolete pending exact-session mutation cannot become a group mutation or reapply its old draft after navigation', async () => {
  const state = flatConversationState(), completion = deferred();
  const { relay, calls } = fixture(async (_path, options) => options.method === 'GET' ? state : completion.promise);
  await relay.setActive(true);
  relay.chooseAccount('scope/a', opusID, 'a');
  const applying = relay.useTask('scope/a', opusID, 'a');
  assert.equal(mutations(calls)[0].path, `/api/desktop-relay/scopes/scope%2Fa/sessions/${opusID}/account`);
  relay.setActive(false);
  verifyAliases(state);
  state.conversation_bindings = [{ scope_id: 'scope/a', conversation_id: conversationID, account_id: 'b', revision: 1 }];
  state.sessions.forEach(task => { task.account_id = 'b'; task.revision++; });
  await relay.setActive(true);
  assert.equal(await relay.useTask('scope/a', conversationID, 'a'), false, 'the old pending guard still owns the operation');
  completion.reject(Object.assign(new Error('stale exact-session rejection'), { status: 409 }));
  assert.equal(await applying, false);
  assert.match(visibleRowHTML(relay.html(), 'scope/a', conversationID), /value="b" selected/);
  assert.doesNotMatch(relay.html(), /stale exact-session rejection|generation changed/);
  assert.deepEqual(calls.map(call => call.options.method), ['GET', 'POST', 'GET']);
  assert.equal(mutations(calls)[0].options.signal.aborted, true);
});

test('explicitly unverified saved groups block every stored-account choice but permit revision-guarded reset while stopped', async () => {
  for (const recordAccount of ['b', '']) {
    const state = conversationState();
    state.status = disabled().status;
    state.sessions.forEach(task => { task.in_flight = 0; task.title = ''; task.client_kind = ''; task.title_source = ''; });
    state.sessions[1].association_verified = false;
    state.conversation_bindings = [{ scope_id: 'scope/a', conversation_id: conversationID, account_id: recordAccount, revision: 5 }];
    const { relay, calls } = fixture(async (path, options) => {
      if (options.method === 'GET') return state;
      assert.equal(path, conversationPath);
      assert.equal(options.method, 'DELETE');
      assert.deepEqual(JSON.parse(options.body), { revision: 5, member_revisions: expectedMembers });
      state.conversation_bindings[0] = { ...state.conversation_bindings[0], account_id: '', revision: 6 };
      state.sessions.forEach(task => { task.account_id = ''; task.revision++; });
      return {};
    });
    await relay.setActive(true);
    let visible = visibleRowHTML(relay.html(), 'scope/a', conversationID);
    assert.match(visible, /Saved selection; conversation identity unavailable\. Restore Claude sign-in or refresh before choosing another account\./);
    assert.match(visible, /<strong>Code session 92ef0000<\/strong>/);
    assert.match(visible, /option value="" selected/);
    assert.match(visible, /value="b"[^>]*disabled/);
    assert.match(visible, />Use Claude sign-in<\/button>/);
    assert.doesNotMatch(actionTag(visible, 'use'), /disabled/);
    assert.equal(relay.chooseAccount('scope/a', conversationID, 'a'), false);
    assert.equal(relay.chooseAccount('scope/a', opusID, 'b'), false);
    assert.equal(await relay.useTask('scope/a', conversationID, 'b'), false);
    assert.equal(mutations(calls).length, 0);
    assert.equal(relay.chooseAccount('scope/a', conversationID, ''), true);
    assert.equal(await relay.useTask('scope/a', conversationID, ''), true);
    visible = visibleRowHTML(relay.html(), 'scope/a', conversationID);
    assert.match(visible, /Default: Claude sign-in/);
    assert.match(actionTag(visible, 'use'), /disabled/);
    assert.deepEqual(mutations(calls).map(call => call.path), [conversationPath]);
  }
});

test('a false member flag blocks new binding while listening, and verification recovery restores stored-account options', async () => {
  const state = conversationState();
  state.conversation_bindings = [{ scope_id: 'scope/a', conversation_id: conversationID, account_id: 'b', revision: 4 }];
  state.sessions[1].association_verified = false;
  const { relay, calls } = fixture(async (_path, options) => options.method === 'GET' ? state : {});
  await relay.setActive(true);
  assert.equal(relay.chooseAccount('scope/a', conversationID, 'a'), false);
  assert.equal(await relay.useTask('scope/a', conversationID, 'a'), false);
  assert.match(visibleRowHTML(relay.html(), 'scope/a', conversationID), /Saved selection: b@example.test/);
  state.sessions[1].association_verified = true;
  await relay.refresh();
  const visible = visibleRowHTML(relay.html(), 'scope/a', conversationID);
  assert.doesNotMatch(visible, /conversation identity unavailable/);
  assert.match(visible, /value="b" selected/);
  assert.equal(relay.chooseAccount('scope/a', conversationID, 'a'), true);
  assert.equal(await relay.useTask('scope/a', conversationID, 'a'), true);
  assert.equal(mutations(calls).length, 1);
});

test('unverified cached membership cannot migrate a flat draft, and an unassociated CLI keeps its exact-session controls', async () => {
  const state = flatConversationState();
  const { relay, calls } = fixture(async (_path, options) => options.method === 'GET' ? state : {});
  await relay.setActive(true);
  relay.chooseAccount('scope/a', opusID, 'b');
  state.sessions[0].conversation_id = conversationID;
  await relay.refresh();
  assert.equal(relay.chooseAccount('scope/a', conversationID, 'a'), false);
  state.sessions[0].association_verified = true;
  await relay.refresh();
  assert.match(visibleRowHTML(relay.html(), 'scope/a', conversationID), /option value="" selected/);
  assert.equal(relay.chooseAccount('scope/a', haikuID, 'a'), true);
  assert.equal(await relay.useTask('scope/a', haikuID, 'a'), true);
  assert.equal(mutations(calls)[0].path, `/api/desktop-relay/scopes/scope%2Fa/sessions/${haikuID}/account`);
});

test('group member diagnostics retain each parent and agent field with escaped raw values', async () => {
  const state = conversationState();
  state.sessions[0].parent_session_id = 'parent-"<script>root</script>';
  state.sessions[0].agent_id = 'agent-"<img src=x>';
  state.sessions[1].parent = 'legacy-<svg onload=x>';
  state.sessions[1].agent = 'worker-&"<script>child</script>';
  const { relay } = fixture(async () => state);
  await relay.setActive(true);
  const visible = visibleRowHTML(relay.html(), 'scope/a', conversationID);
  const row = rowHTML(relay.html(), 'scope/a', conversationID);
  assert.doesNotMatch(visible, />Worker<\/span>/);
  assert.match(row, /<dt>Parent session<\/dt><dd>parent-&quot;&lt;script&gt;root&lt;\/script&gt;<\/dd>/);
  assert.match(row, /<dt>Agent ID<\/dt><dd>agent-&quot;&lt;img src=x&gt;<\/dd>/);
  assert.match(row, /<dt>Parent<\/dt><dd>legacy-&lt;svg onload=x&gt;<\/dd>/);
  assert.match(row, /<dt>Agent<\/dt><dd>worker-&amp;&quot;&lt;script&gt;child&lt;\/script&gt;<\/dd>/);
  assert.doesNotMatch(row, /<script>|<img|<svg|href="parent/);
});

test('unowned partial group reset is disabled and direct DELETE is blocked unless the relay is listening and every association is verified', async () => {
  for (const [listening, verified] of [[false, true], [false, false], [true, false]]) {
    const state = conversationState();
    state.status.listening = listening;
    state.sessions.forEach(task => { task.in_flight = 0; });
    state.sessions[1].association_verified = verified;
    const { relay, calls } = fixture(async () => state);
    await relay.setActive(true);
    assert.equal(relay.chooseAccount('scope/a', conversationID, ''), true);
    const visible = visibleRowHTML(relay.html(), 'scope/a', conversationID);
    assert.match(visible, /Partially applied/);
    assert.match(actionTag(visible, 'use'), /disabled/);
    assert.match(visible, listening ? /Refresh status to verify this conversation before restoring Claude sign-in/ : /Start relay in Advanced and refresh status before restoring Claude sign-in/);
    assert.doesNotMatch(visible, /Claude sign-in can be restored while the relay is stopped/);
    assert.equal(await relay.useTask('scope/a', conversationID, ''), false);
    assert.equal(await relay.useTask('scope/a', opusID, ''), false);
    assert.equal(mutations(calls).length, 0);
  }
});

test('saved empty-account group ownership is scoped and permits stopped reset only for the matching record', async () => {
  const state = conversationState();
  state.status = disabled().status;
  state.sessions.forEach(task => { task.in_flight = 0; });
  state.conversation_bindings = [{ scope_id: 'peer-scope', conversation_id: conversationID, account_id: '', revision: 9 }];
  const { relay, calls } = fixture(async (path, options) => {
    if (options.method === 'GET') return state;
    assert.equal(path, conversationPath);
    assert.equal(options.method, 'DELETE');
    assert.deepEqual(JSON.parse(options.body), { revision: 4, member_revisions: expectedMembers });
    return {};
  });
  await relay.setActive(true);
  relay.chooseAccount('scope/a', conversationID, '');
  assert.match(actionTag(visibleRowHTML(relay.html(), 'scope/a', conversationID), 'use'), /disabled/);
  assert.equal(await relay.useTask('scope/a', conversationID, ''), false);
  assert.equal(mutations(calls).length, 0);
  state.conversation_bindings.push({ scope_id: 'scope/a', conversation_id: conversationID, account_id: '', revision: 4 });
  state.sessions[1].association_verified = false;
  await relay.refresh();
  assert.doesNotMatch(actionTag(visibleRowHTML(relay.html(), 'scope/a', conversationID), 'use'), /disabled/);
  assert.equal(await relay.useTask('scope/a', conversationID, ''), true);
  assert.equal(mutations(calls).length, 1);
});
