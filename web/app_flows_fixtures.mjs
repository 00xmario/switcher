import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

// Imported by app_state_test.mjs so the existing verify target runs these fixtures.
const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
function section(start, end) {
  const from = source.indexOf(start), to = source.indexOf(end, from);
  assert.ok(from >= 0 && to > from, `${start} must remain findable`);
  return source.slice(from, to);
}
const deferred = () => {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
};

test('ordinary Usage navigation loads usage through the final page handler', () => {
  let loads = 0;
  const context = vm.createContext({
    data: { compact_accounts: true },
    authState: { locked: false },
    document: {
      querySelectorAll: () => [], getElementById: () => ({}), querySelector: () => ({}),
      body: { classList: { toggle() {} } },
    },
    usagePage: {}, settingsPage: {}, renderSettings() {}, loadUsage: () => loads++,
  });
  vm.runInContext(section('setPage = function (page) {', '\n// lanStateText'), context);
  context.setPage('usage');
  assert.equal(context.usagePage.hidden, false);
  assert.equal(loads, 1);
  context.setPage('accounts');
  assert.equal(loads, 1, 'other pages must not load usage');
});

function usageFixture() {
  const requests = [], rendered = [], timers = [];
  const request = path => {
    const reply = deferred(); requests.push({ path, ...reply }); return reply.promise;
  };
  const context = vm.createContext({
    usageState: { days: 30, loading: false, summary: null, generation: 0, promise: null, timer: null },
    api: request,
    fetch: path => request(path).then(body => ({ ok: true, json: async () => body })),
    renderUsage: () => rendered.push(context.usageState.summary.fixtureDays),
    usageHeadline: {}, usageSub: {}, usageProviders: {}, usageTotals: {}, usageChartBox: {}, usageBreakdownTable: {},
    usagePage: { hidden: false }, document: { hidden: false }, authState: { locked: false },
    toast() {}, clearTimeout() {}, setTimeout: callback => { timers.push(callback); return timers.length; },
  });
  vm.runInContext(section('async function loadUsage() {', '\n// sigRound') + '\nthis.loadUsage = loadUsage;', context);
  return { context, requests, rendered, timers };
}

test('usage queues only the latest period and fences the old response', async () => {
  const { context, requests, rendered } = usageFixture();
  const first = context.loadUsage();
  context.usageState.summary = { fixtureDays: 30 };
  context.usageState.summaryDays = 30;
  context.usageState.days = 1;
  const today = context.loadUsage();
  assert.equal(context.usageState.summary, null, 'period changes must immediately hide the old period, even while queued');
  context.usageState.days = 7;
  const week = context.loadUsage();
  assert.equal(requests.length, 1, 'period changes must not create parallel requests');
  requests[0].resolve({ summary: { fixtureDays: 30 } });
  for (let i = 0; i < 8; i++) await Promise.resolve();
  assert.deepEqual(requests.map(r => r.path), ['/api/tokens?days=30', '/api/tokens?days=7']);
  assert.deepEqual(rendered, [], '30-day data must not appear under the selected 7-day period');
  requests[1].resolve({ summary: { fixtureDays: 7 } });
  await Promise.all([first, today, week]);
  assert.deepEqual(rendered, [7]);
  assert.equal(context.usageState.loading, false);
});

test('obsolete scanning retries cannot fetch an old usage period', async () => {
  const { context, requests, timers } = usageFixture();
  const scan = context.loadUsage();
  requests[0].resolve({ scanning: true });
  await scan;
  const retry = timers[0];
  context.usageState.days = 1;
  const today = context.loadUsage();
  requests[1].resolve({ summary: { fixtureDays: 1 } });
  await today;
  retry();
  assert.equal(requests.length, 2);
  assert.equal(context.usageState.summary.fixtureDays, 1);
});

function authFixture(fetchReply, storage = new Map()) {
  const requests = [], redirects = [], timers = new Map(), timerDelays = new Map();
  let timerID = 0;
  const context = vm.createContext({
    fetch: (path, options) => { requests.push({ path, options }); return fetchReply(path, options); },
    localStorage: {
      getItem: key => storage.get(key) || null, setItem: (key, value) => storage.set(key, value),
      removeItem: key => storage.delete(key),
    },
    AbortController, Error, stateTimer: null, usageTimer: null,
    clearInterval() {}, clearTimeout: id => { timers.delete(id); timerDelays.delete(id); },
    setTimeout: (callback, delay) => { timers.set(++timerID, callback); timerDelays.set(timerID, delay); return timerID; },
    location: { replace: path => redirects.push(path) },
  });
  vm.runInContext(section('let authState = ', '\nfunction escapeHTML(') + '\nthis.api = api;', context);
  return { context, requests, redirects, timers, timerDelays, storage };
}
const reply = (body, status = 200) => ({ ok: status >= 200 && status < 300, status,
  statusText: status === 403 ? 'Forbidden' : '', json: async () => body });

test('authenticated startup hydrates CSRF before starting state polling', async () => {
  const status = deferred();
  const { context, requests, storage } = authFixture(() => status.promise);
  const polls = [];
  context.pollState = () => polls.push(storage.get('switcher-csrf'));
  context.toast = message => assert.fail(message);
  vm.runInContext(section('ensureAuthReady().then(() => pollState()).catch(error => {', '\nwindow.addEventListener'), context);
  assert.deepEqual(requests.map(r => r.path), ['/api/auth/status']);
  assert.deepEqual(polls, []);
  status.resolve(reply({ auth_enabled: true, csrf: 'startup-csrf' }));
  await context.ensureAuthReady();
  await Promise.resolve();
  assert.deepEqual(polls, ['startup-csrf']);
});

test('mutations await shared authenticated status hydration after storage was cleared', async () => {
  const status = deferred();
  const { context, requests, storage } = authFixture(path => path === '/api/auth/status'
    ? status.promise : Promise.resolve(reply({ status: 'ok' })));
  const first = context.api('/api/accounts/fixture/recheck', { method: 'POST' });
  const second = context.api('/api/settings', { method: 'PATCH' });
  assert.deepEqual(requests.map(r => r.path), ['/api/auth/status']);
  status.resolve(reply({ auth_enabled: true, csrf: 'fixture-csrf' }));
  await Promise.all([first, second]);
  assert.equal(storage.get('switcher-csrf'), 'fixture-csrf');
  for (const request of requests.slice(1)) assert.equal(request.options.headers['X-Switcher-CSRF'], 'fixture-csrf');
});

test('request timeout covers a stalled response body and reports a useful error', async () => {
  const body = deferred();
  const { context, timers } = authFixture((_path, options) => {
    options.signal?.addEventListener('abort', () => body.reject(new Error('aborted')));
    return Promise.resolve({ ...reply({}), json: () => body.promise });
  });
  const request = context.api('/api/state', { timeoutMs: 5 });
  await Promise.resolve();
  assert.equal(timers.size, 1, 'the deadline must remain active until the JSON body is read');
  for (const callback of [...timers.values()]) callback();
  await assert.rejects(request, /timed out/i);
  assert.equal(timers.size, 0);
});

test('bounded mutation deadlines allow a provider reset to complete after twenty seconds', async () => {
  const body = deferred();
  const { context, timers, timerDelays } = authFixture((path, options) => {
    if (path === '/api/auth/status') return Promise.resolve(reply({ auth_enabled: true, csrf: 'fixture-csrf' }));
    options.signal.addEventListener('abort', () => body.reject(new Error('aborted')));
    return Promise.resolve({ ...reply({}), json: () => body.promise });
  });
  const reset = context.api('/api/accounts/fixture/use-reset', { method: 'POST' });
  for (let i = 0; i < 12; i++) await Promise.resolve();
  for (const [id, callback] of timers) if (timerDelays.get(id) <= 20000) callback();
  body.resolve({ outcome: 'reset' });
  assert.equal((await reset).outcome, 'reset');
  assert.equal(timers.size, 0);
});

test('expired sessions redirect for raw native activation and expose structured HTTP failures', async () => {
  const { context, redirects } = authFixture(() => Promise.resolve(reply({ auth_required: true }, 401)));
  await assert.rejects(context.fetchWithCSRF('/api/accounts/fixture/activate', { method: 'POST' }), /Authentication required/);
  assert.deepEqual(redirects, ['/login']);
  const failed = authFixture(() => Promise.resolve(reply({ error: { message: 'fixture rejection' }, details: 'fixture details' }, 403)));
  await assert.rejects(failed.context.api('/api/state'), error => error.message === 'fixture rejection' && error.status === 403 && error.body.details === 'fixture details');
});

test('activation is deduplicated across controls and stays pending across a repaint', async () => {
  const requests = [], paints = [];
  const pendingActivations = new Set();
  const context = vm.createContext({
    pendingActivations, stateEpoch: 0, authState: { locked: false },
    fetchWithCSRF: () => { const result = deferred(); requests.push(result); return result.promise; },
    render: () => paints.push(pendingActivations.has('claude-fixture')),
    refreshState: async () => {}, toast() {}, confirmDialog: async () => true,
  });
  vm.runInContext(section('async function runAccountAction(button) {', '\n/* ---------- drag-and-drop') + '\nthis.runAccountAction = runAccountAction;', context);
  const button = () => ({ disabled: false, dataset: { act: 'activate', accountId: 'claude-fixture' }, getAttribute: () => null });
  const first = context.runAccountAction(button());
  const duplicate = context.runAccountAction(button());
  assert.equal(requests.length, 1, 'the card and menu must share the pending activation');
  assert.deepEqual(paints, [true]);
  requests[0].resolve(reply({ native: { message: 'fixture switch completed' } }));
  await Promise.all([first, duplicate]);
  assert.equal(pendingActivations.size, 0);
  assert.deepEqual(paints, [true, false]);
  const retry = context.runAccountAction(button());
  requests[1].reject(new Error('fixture timeout'));
  await retry;
  assert.equal(pendingActivations.size, 0, 'a failure must release the shared guard');
});

function updateFixture(install) {
  const messages = [], requests = [];
  let clock = 0;
  const context = vm.createContext({
    updateRunning: false, data: { version: '1.0' }, stateEpoch: 0, authState: { locked: false },
    Date: { now: () => clock },
    api: (path, options) => {
      requests.push({ path, options });
      return path === '/api/update' ? install() : Promise.resolve({ version: '2.0', accounts: [] });
    },
    fetch: async () => ({ json: async () => ({ version: '1.0' }) }),
    setTimeout: callback => { clock += 1500; callback(); },
    toast: message => messages.push(message), refreshState: async () => {}, render() {}, renderAddProviderMenu() {},
  });
  vm.runInContext(section('async function runUpdateFlow(button) {', '\n/* ---------- boot') + '\nthis.runUpdateFlow = runUpdateFlow;', context);
  const classes = new Set();
  const button = { disabled: false, textContent: 'Install & restart', closest: () => ({
    classList: { add: name => classes.add(name), remove: name => classes.delete(name) },
  }) };
  return { context, messages, requests, button, classes };
}

test('a definitive update failure is surfaced before version polling', async () => {
  const error = Object.assign(new Error('fixture download failed'), { status: 500 });
  const { context, messages, requests, button, classes } = updateFixture(() => Promise.reject(error));
  await context.runUpdateFlow(button);
  assert.deepEqual(messages, ['Update failed: fixture download failed']);
  assert.deepEqual(requests.map(r => r.path), ['/api/update']);
  assert.equal(button.disabled, false);
  assert.equal(classes.has('updating'), false);
});

test('an update disconnect is reconciled by authenticated version polling', async () => {
  const { context, messages, requests, button } = updateFixture(() => Promise.reject(new Error('network disconnected')));
  await context.runUpdateFlow(button);
  assert.deepEqual(messages, ['Updated to Switcher v2.0']);
  assert.deepEqual(requests.map(r => r.path), ['/api/update', '/api/state']);
  assert.equal(context.stateEpoch, 1, 'old state requests must not undo the observed upgrade');
  assert.equal(button.disabled, false);
});

class LogoutNode {
  constructor(id = '', html = '') { this.id = id; this.innerHTML = html; this.textContent = ''; this.children = []; this.listeners = new Map(); this.attributes = new Map(); this.disabled = false; }
  classList = { remove() {}, add() {}, toggle() {} };
  replaceChildren(...children) { this.innerHTML = ''; this.textContent = ''; this.children = children; }
  setAttribute(name, value) { this.attributes.set(name, value); }
  addEventListener(event, callback) { this.listeners.set(event, callback); }
  fire(event = 'click') { return this.listeners.get(event)?.({ preventDefault() {} }); }
  focus() {}
  querySelector(selector) {
    const matches = node => selector.startsWith('#') ? node.id === selector.slice(1) : node.attributes.has(selector.slice(1, -1));
    for (const child of this.children) { if (matches(child)) return child; const found = child.querySelector(selector); if (found) return found; }
    const attribute = selector.startsWith('#') ? `id="${selector.slice(1)}"` : selector.slice(1, -1);
    if (!this.innerHTML.includes(attribute)) return null;
    const node = new LogoutNode(selector.startsWith('#') ? selector.slice(1) : '');
    if (!selector.startsWith('#')) node.setAttribute(attribute, '');
    this.children.push(node); return node;
  }
  html() { return this.innerHTML + this.textContent + this.children.map(node => node.html()).join(''); }
}

function logoutFixture(logoutResponse, stateResponse = () => Promise.resolve(reply({ accounts: [] }))) {
  const cookie = { value: 'fixture-httpOnly-cookie' }, logoutCookies = [];
  const fixture = authFixture((path, options) => {
    if (path === '/api/auth/logout') { logoutCookies.push(cookie.value); return logoutResponse(options, cookie); }
    if (path === '/api/auth/status') return Promise.resolve(reply({ auth_enabled: true, csrf: 'logout-csrf' }));
    if (path === '/api/state') return stateResponse(options);
    assert.fail(`unexpected protected request: ${path}`);
  });
  const { context, timers } = fixture;
  const body = new LogoutNode(), app = new LogoutNode('app');
  const providers = new LogoutNode('providers', 'private@example.test');
  const settings = new LogoutNode('settings-page', 'private-hub-key');
  const usage = new LogoutNode('usage-page', 'private-usage-total');
  const button = new LogoutNode('logout-here');
  settings.children.push(button); app.children.push(providers, settings, usage); body.children.push(app);
  context.document = { body, hidden: false, createElement: () => new LogoutNode(),
    getElementById: id => body.id === id ? body : body.querySelector(`#${id}`), querySelectorAll: () => [] };
  Object.assign(context, {
    data: { accounts: [{ id: 'private-account', email: 'private@example.test' }], hub_management_key: 'private-hub-key' },
    providersEl: providers, settingsPage: settings, usagePage: usage,
    usageState: { summary: { private: 'private-usage-total' }, generation: 0, timer: 903 },
    stateTimer: 901, usageTimer: 902, stateEpoch: 0, pollRequest: 0, stateRequestId: 0, lastAppliedStateRequestId: 0, lastRenderMinute: 0,
    pendingRechecks: new Map(), pendingActivations: new Set(), closeAccountMenu() {},
    settleRechecks: () => false, resetFeedback: { observe() {} },
    render: () => assert.fail('private dashboard repainted'), renderAddProviderMenu: () => assert.fail('private controls repainted'),
    toast: () => assert.fail('logout must use its dedicated view'),
  });
  for (const name of ['usageHeadline', 'usageSub', 'usageProviders', 'usageTotals', 'usageChartBox', 'usageChartTitle', 'usageBreakdownTable', 'accountStatusAnnouncer']) {
    context[name] = new LogoutNode('', 'private-usage-total');
  }
  for (const id of [901, 902, 903]) timers.set(id, () => assert.fail('protected polling resumed'));
  context.hydrateAuthStatus({ auth_enabled: true, csrf: 'logout-csrf' });
  vm.runInContext(section('async function refreshState() {', '\nfunction statusOf(') +
    section('async function pollState() {', '\ndocument.addEventListener') +
    section("  settingsPage.querySelector('#logout-here')", "\n  settingsPage.querySelector('#change-password-btn')"), context);
  return { ...fixture, body, button, cookie, logoutCookies };
}

test('logout persistence failure clears private UI and retains an explicit logout-only retry', async () => {
  const firstResponse = deferred(), oldState = deferred();
  let attempts = 0;
  const fixture = logoutFixture((_options, cookie) => {
    if (++attempts === 1) return firstResponse.promise;
    cookie.value = '';
    return Promise.resolve(reply({ status: 'ok' }));
  }, () => oldState.promise);
  const { context, button, body, storage, requests, timers, redirects, cookie } = fixture;
  const oldPoll = context.pollState();
  const logout = button.fire();
  assert.doesNotMatch(body.html(), /private@example|private-hub-key|private-usage-total/,
    'private content must disappear synchronously when logout starts');
  for (const id of [901, 902, 903]) assert.equal(timers.has(id), false);
  assert.equal(context.data.accounts.length, 0);
  assert.equal(context.data.hub_management_key, undefined);
  assert.equal(context.usageState.summary, null);
  oldState.resolve(reply({ auth_required: true }, 401));
  await oldPoll;
  assert.equal(storage.get('switcher-csrf'), 'logout-csrf', 'late 401 must not destroy retry proof');
  assert.deepEqual(redirects, []);
  firstResponse.resolve(reply({ error: 'fixture durable storage failure' }, 500));
  await logout;
  assert.equal(cookie.value, 'fixture-httpOnly-cookie');
  assert.equal(storage.get('switcher-csrf'), 'logout-csrf');
  const retry = body.querySelector('[data-logout-retry]');
  assert.ok(retry && !retry.disabled, 'failure must expose an explicit retry button');
  assert.match(body.html(), /Retry logout/);
  assert.doesNotMatch(body.html(), /private@example|private-hub-key|private-usage-total/);
  const before = requests.length;
  for (const [path, method] of [['/api/state', 'GET'], ['/api/auth/status', 'GET'], ['/api/settings', 'PATCH'], ['/api/auth/sessions', 'DELETE'], ['/api/auth/logout', 'GET']]) {
    await assert.rejects(context.api(path, { method }), /Authentication required|Logout/i);
  }
  await context.pollState();
  assert.equal(requests.length, before, 'the retry view must not fetch or mutate protected routes');
  storage.delete('switcher-csrf'); // Another tab may clear shared storage after revocation.
  await retry.fire();
  const posts = requests.filter(request => request.path === '/api/auth/logout');
  assert.equal(posts.length, 2);
  for (const post of posts) {
    assert.equal(post.options.method, 'POST');
    assert.equal(post.options.credentials, 'same-origin');
    assert.equal(post.options.headers['X-Switcher-CSRF'], 'logout-csrf');
  }
  assert.deepEqual(fixture.logoutCookies, ['fixture-httpOnly-cookie', 'fixture-httpOnly-cookie']);
  assert.equal(storage.has('switcher-csrf'), false);
  assert.deepEqual(redirects, ['/login']);
});

test('late successful state cannot restore private content during failed logout', async () => {
  const oldState = deferred();
  const fixture = logoutFixture(() => Promise.resolve(reply({ error: 'storage failure' }, 500)), () => oldState.promise);
  const poll = fixture.context.pollState();
  await fixture.button.fire();
  oldState.resolve(reply({ accounts: [{ id: 'late-private-account', email: 'late-private@example.test' }], hub_management_key: 'late-private-key' }));
  await poll;
  assert.doesNotMatch(fixture.body.html(), /late-private|private@example|private-hub-key/);
  assert.equal(fixture.context.data.accounts.length, 0);
  assert.equal(fixture.storage.get('switcher-csrf'), 'logout-csrf');
  assert.deepEqual(fixture.redirects, []);
});

test('logout timeout and repeated clicks never automatically replay its POST', async () => {
  const fixture = logoutFixture(options => new Promise((_resolve, reject) => {
    options.signal.addEventListener('abort', () => reject(new Error('aborted')));
  }));
  const pending = fixture.button.fire();
  const retry = fixture.body.querySelector('[data-logout-retry]');
  assert.ok(retry?.disabled);
  await retry.fire();
  assert.equal(fixture.requests.filter(request => request.path === '/api/auth/logout').length, 1);
  for (const callback of [...fixture.timers.values()]) callback();
  await pending;
  assert.equal(fixture.requests.filter(request => request.path === '/api/auth/logout').length, 1);
  assert.equal(retry.disabled, false);
  assert.equal(fixture.storage.get('switcher-csrf'), 'logout-csrf');
  assert.deepEqual(fixture.redirects, []);
  assert.doesNotMatch(fixture.body.html(), /private@example|private-hub-key|private-usage-total/);
});

test('an expired logout-only proof stays on the cleared retry view without reauthentication', async () => {
  let attempt = 0;
  const fixture = logoutFixture(() => Promise.resolve(++attempt === 1
    ? reply({ error: 'storage failure' }, 500) : reply({ auth_required: true }, 401)));
  await fixture.button.fire();
  await fixture.body.querySelector('[data-logout-retry]').fire();
  assert.match(fixture.body.html(), /expired or been rejected/);
  assert.doesNotMatch(fixture.body.html(), /private@example|private-hub-key|private-usage-total/);
  assert.equal(fixture.storage.get('switcher-csrf'), 'logout-csrf');
  assert.deepEqual(fixture.redirects, []);
  assert.deepEqual(fixture.requests.map(request => request.path), ['/api/auth/logout', '/api/auth/logout']);
});

test('late auth status cannot clear the captured logout proof', async () => {
  const fixture = logoutFixture(() => Promise.resolve(reply({ error: 'storage failure' }, 500)));
  const originalFetch = fixture.context.fetch;
  fixture.context.fetch = (path, options) => path === '/api/auth/status'
    ? Promise.resolve(reply({ auth_enabled: false })) : originalFetch(path, options);
  const status = fixture.context.api('/api/auth/status');
  const logout = fixture.button.fire();
  await assert.rejects(status, /Authentication required/);
  await logout;
  assert.equal(fixture.storage.get('switcher-csrf'), 'logout-csrf');
  assert.deepEqual(fixture.redirects, []);
  assert.doesNotMatch(fixture.body.html(), /private@example|private-hub-key|private-usage-total/);
});

test('an already queued settings reload cannot leave the logout retry view', async () => {
  const fixture = logoutFixture(() => Promise.resolve(reply({ error: 'storage failure' }, 500)));
  const bind = new LogoutNode('bind-lan'); bind.checked = true;
  fixture.context.settingsPage.children.push(bind);
  const originalFetch = fixture.context.fetch;
  fixture.context.fetch = (path, options) => path === '/api/settings'
    ? Promise.resolve(reply({ status: 'ok' })) : originalFetch(path, options);
  fixture.context.toast = () => {};
  let reloads = 0;
  fixture.context.location.reload = () => reloads++;
  vm.runInContext(section("  const bindLan = settingsPage.querySelector('#bind-lan');", "\n  settingsPage.querySelector('#set-password-form')"), fixture.context);
  const saving = bind.fire('change');
  for (let i = 0; i < 30; i++) await Promise.resolve();
  const reloadTimer = [...fixture.timerDelays].find(([, delay]) => delay === 600)?.[0];
  assert.ok(reloadTimer, 'fixture must reach the already scheduled reload');
  const resume = fixture.timers.get(reloadTimer);
  await fixture.button.fire();
  resume();
  await saving;
  assert.equal(reloads, 0);
  assert.equal(fixture.storage.get('switcher-csrf'), 'logout-csrf');
  assert.deepEqual(fixture.redirects, []);
});

test('a late logout-all completion cannot consume logout-only retry proof', async () => {
  const fixture = logoutFixture(() => Promise.resolve(reply({ error: 'storage failure' }, 500)));
  const all = new LogoutNode('logout-all'); fixture.context.settingsPage.children.push(all);
  const oldResult = deferred(), originalRequest = fixture.context.fetchWithCSRF;
  fixture.context.fetchWithCSRF = (path, options) => path === '/api/auth/sessions'
    ? oldResult.promise : originalRequest(path, options);
  vm.runInContext(section("  settingsPage.querySelector('#logout-all')", "\n  settingsPage.querySelector('#logout-here')"), fixture.context);
  const oldLogout = all.fire();
  await fixture.button.fire();
  oldResult.resolve(reply({ status: 'ok' }));
  await oldLogout;
  assert.equal(fixture.storage.get('switcher-csrf'), 'logout-csrf');
  assert.deepEqual(fixture.redirects, []);
});
