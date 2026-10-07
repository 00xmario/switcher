import assert from 'node:assert/strict';
import test from 'node:test';
import { createDesktopRelay, desktopRelayEntries, desktopRelayLoopback, desktopRelayActivity } from './desktop-relay.js';

const scope = 'aaaaaaaa-0000-4000-8000-000000000000';
const conv = 'cccccccc-0000-4000-8000-000000000000';
const at = seconds => new Date(Date.UTC(2026, 9, 4, 12, 0, seconds)).toISOString();
const session = (id, extra = {}) => ({ scope_id: scope, session_id: id, revision: 2, last_seen: at(0), in_flight: 0, ...extra });

function body() {
  return {
    status: { enabled: true, listening: true, in_flight: 1, address: '127.0.0.1:8789' },
    setup: { condition: 'configured', configured: true, scope_id: scope, settings_path: '/x/settings.json' },
    scopes: [{ id: scope, label: 'Claude Desktop' }],
    sessions: [
      session('11111111-0000-4000-8000-000000000001', { conversation_id: conv, last_seen: at(10), title: '', in_flight: 1 }),
      session('11111111-0000-4000-8000-000000000002', { conversation_id: conv, last_seen: at(5), title: 'Fix the relay', project: '/Users/me/switcher' }),
      session('22222222-0000-4000-8000-000000000003', { last_seen: at(20), title: 'Loose session', account_id: 'claude-b' }),
    ],
    conversation_bindings: [{ scope_id: scope, conversation_id: conv, account_id: 'claude-a', revision: 3 }],
  };
}

test('one entry per Desktop conversation, plus unlinked sessions', () => {
  const entries = desktopRelayEntries(body());
  assert.equal(entries.length, 2);
  const [conversation, loose] = entries;
  assert.equal(conversation.conversation, conv, 'live conversations sort first');
  assert.equal(conversation.title, 'Fix the relay');
  assert.equal(conversation.project, 'switcher');
  assert.equal(conversation.inFlight, 1);
  assert.equal(conversation.account, 'claude-a');
  assert.equal(conversation.sessionOnly, false);
  assert.equal(loose.session, '22222222-0000-4000-8000-000000000003');
  assert.equal(loose.account, 'claude-b');
  assert.equal(loose.revision, 2);
});

test('an exact selection without a conversation selection is shown as session-only', () => {
  const data = body();
  data.conversation_bindings = [];
  data.sessions[0].account_id = 'claude-a';
  const [conversation] = desktopRelayEntries(data);
  assert.equal(conversation.account, 'claude-a');
  assert.equal(conversation.sessionOnly, true);
});

test('loopback and activity helpers', () => {
  assert.ok(desktopRelayLoopback('localhost') && desktopRelayLoopback('127.0.0.1') && desktopRelayLoopback('[::1]'));
  assert.ok(!desktopRelayLoopback('192.168.1.2') && !desktopRelayLoopback('127.0.0.256'));
  assert.equal(desktopRelayActivity(at(0), Date.parse(at(30))).label, 'just now');
  assert.equal(desktopRelayActivity(at(0), Date.parse(at(0)) + 5 * 60000).label, '5 min ago');
  assert.equal(desktopRelayActivity('nonsense'), null);
});

function fixture({ loopback = true, failPost = false } = {}) {
  const calls = [];
  const relay = createDesktopRelay({
    api: async (path, options = {}) => {
      calls.push({ path, method: options.method || 'GET', body: options.body ? JSON.parse(options.body) : null, key: options.controllerKey });
      if (options.method && options.method !== 'GET' && failPost) {
        const error = new Error('nope');
        error.body = { error: 'desktop relay selection changed; reload and try again' };
        throw error;
      }
      return options.method && options.method !== 'GET' ? {} : body();
    },
    getContext: () => ({ loopback, locked: false, loggingOut: false, authKind: '', controllerKey: 'key' }),
    getAccounts: () => [{ id: 'claude-a', provider: 'claude', email: 'work@example.test' }, { id: 'claude-b', provider: 'claude', email: 'home@example.test' }, { id: 'codex-a', provider: 'codex', email: 'x@example.test' }],
    now: () => Date.parse(at(40)),
  });
  return { relay, calls };
}

test('the list shows each conversation with a one-click account picker', async () => {
  const { relay, calls } = fixture();
  await relay.setActive(true);
  assert.deepEqual(calls.map(c => [c.path, c.method, c.key]), [['/api/desktop-relay', 'GET', 'key']]);
  const html = relay.settingsHTML();
  assert.match(html, /Fix the relay/);
  assert.match(html, /1 replying/);
  assert.match(html, /aria-checked="true" data-dr-choose="c:[^"]+" data-dr-account="claude-a"/);
  assert.doesNotMatch(html, /codex-a/, 'only Claude accounts can be picked');
  assert.match(html, /Desktop login/);
  assert.match(html, /data-dr-account="" title="Default: Claude Desktop’s own login, through Switcher" aria-label="Desktop login" class="dr-default"[^>]*>\s*<svg class="switcher-mark"/,
    'the default option is the Switcher mark, named for screen readers');
  assert.equal(relay.settingsHTML(), relay.settingsHTML(), 'an unchanged view renders identical markup, so polling leaves the DOM alone');
});

test('picking an account switches the whole conversation; Desktop login clears it', async () => {
  const { relay, calls } = fixture();
  await relay.setActive(true);
  const [conversation, loose] = relay.entries();
  assert.equal(await relay.choose(conversation.key, 'claude-b'), true);
  assert.deepEqual(calls[1], { path: `/api/desktop-relay/scopes/${scope}/conversations/${conv}/account`, method: 'POST', body: { account_id: 'claude-b' }, key: 'key' });
  await relay.choose(relay.entries()[0].key, '');
  assert.equal(calls.at(-2).method, 'DELETE');
  await relay.choose(loose.key, 'claude-a');
  assert.deepEqual(calls.at(-2).body, { account_id: 'claude-a', revision: 2 }, 'a lone session uses its revision');
  assert.equal(await relay.choose(conversation.key, 'codex-a'), false, 'non-Claude accounts are refused');
});

test('a failed switch is shown on that row', async () => {
  const { relay } = fixture({ failPost: true });
  await relay.setActive(true);
  const [conversation] = relay.entries();
  assert.equal(await relay.choose(conversation.key, 'claude-b'), false);
  assert.match(relay.settingsHTML(), /class="dr-error" role="alert">desktop relay selection changed/);
});

test('a LAN dashboard never loads or offers controls', async () => {
  const { relay, calls } = fixture({ loopback: false });
  await relay.setActive(true);
  assert.equal(calls.length, 0);
  assert.match(relay.settingsHTML(), /Open Switcher on this Mac/);
});

test('settings offer connect, restart behind confirmation, and disconnect', async () => {
  let confirmed = false;
  const calls = [];
  const data = body();
  data.setup = { condition: 'not_configured', configured: false };
  const relay = createDesktopRelay({
    api: async (path, options = {}) => { calls.push([path, options.method || 'GET']); return options.method ? {} : data; },
    getContext: () => ({ loopback: true, authKind: 'cookie' }),
    confirmRestart: async () => confirmed,
  });
  await relay.setActive(true);
  assert.match(relay.settingsHTML(), /data-dr-action="configure"/);
  assert.match(relay.settingsHTML(), /Not connected/);
  await relay.configure();
  assert.deepEqual(calls[1], ['/api/desktop-relay/configure', 'POST']);
  data.setup = { condition: 'configured', configured: true };
  await relay.refresh();
  assert.match(relay.settingsHTML(), /Connected/);
  const before = calls.length;
  await relay.restart();
  assert.equal(calls.slice(before).some(([path]) => path.endsWith('/restart-desktop')), false, 'restart needs confirmation');
  confirmed = true;
  await relay.restart();
  assert.ok(calls.some(([path, method]) => path.endsWith('/restart-desktop') && method === 'POST'));
  await relay.restore();
  assert.ok(calls.some(([path]) => path.endsWith('/restore')));
});
