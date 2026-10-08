import assert from 'node:assert/strict';
import test from 'node:test';
import { createRemote } from './remote.js';

const now = Date.parse('2026-10-07T12:00:00Z');
const host = (extra = {}) => ({ enabled: false, listening: false, name: 'Studio Mac', port: 8788, lan: ['studio.local'], tailscale: ['100.101.2.3'], devices: [], ...extra });

async function render(status, found) {
  const calls = [];
  const remote = createRemote({ api: async (path, options = {}) => { calls.push([path, options.method || 'GET']); return path === '/api/remote/discover' ? { hosts: found || [] } : status; }, now: () => now });
  await remote.load();
  return { html: remote.html(), calls };
}

test('an idle Mac can share itself or use another Switcher', async () => {
  const { html } = await render({ host: host(), client: { connected: false } });
  assert.match(html, /Share this Switcher/);
  assert.match(html, /Use another Switcher/);
  assert.match(html, /data-remote-form/);
});

test('a sharing Mac shows its addresses, pairing code and paired Macs', async () => {
  const { html } = await render({ host: host({ enabled: true, listening: true, pairing: { code: 'K7QF-M2XP', expires_at: '2026-10-07T12:09:30Z' },
    devices: [{ id: 'd1', name: 'MacBook Air', created_at: '2026-10-06T10:00:00Z', last_seen: '2026-10-07T11:55:00Z' }] }), client: { connected: false } });
  assert.match(html, /data-remote-copy="studio\.local"[^>]*>[\s\S]*?<code>studio\.local<\/code>/);
  assert.match(html, /title="Tailscale: click to copy">[\s\S]*?<code>100\.101\.2\.3<\/code>/);
  assert.match(html, /K7QF-M2XP/);
  assert.match(html, /Valid for 9:30/);
  assert.match(html, /MacBook Air<\/strong><span class="dim">last used 5 min ago/);
  assert.match(html, /data-remote-action="revoke" data-remote-id="d1"/);
  assert.doesNotMatch(html, /Use another Switcher/, 'a sharing Mac cannot also use another one');
});

test('a connected Mac shows its host and how to disconnect', async () => {
  const { html } = await render({ host: host(), client: { connected: true, host_name: 'Studio Mac', address: 'studio.local', reachable: false, error: 'the Switcher host is unreachable' } });
  assert.match(html, /Using Studio Mac/);
  assert.match(html, /Unreachable/);
  assert.match(html, /data-remote-action="disconnect"/);
  assert.doesNotMatch(html, /Share this Switcher/);
});

test('away from home: off, signing in, and running on Tailscale', async () => {
  const base = { host: host(), client: { connected: false } };
  let { html } = await render({ ...base, tailnet: { installed: false, enabled: false, running: false, peers: [] } });
  assert.match(html, /Away from home/);
  assert.match(html, /downloads a 19 MB add-on/);
  assert.doesNotMatch(html, /Remove add-on/);
  ({ html } = await render({ ...base, tailnet: { installed: true, enabled: true, running: true, state: 'NeedsLogin', auth_url: 'https://login.tailscale.com/a/abc', peers: [] } }));
  assert.match(html, /href="https:\/\/login\.tailscale\.com\/a\/abc"[^>]*>Sign in with Tailscale/);
  ({ html } = await render({ ...base, tailnet: { installed: true, enabled: true, running: true, state: 'Running', dns_name: 'switcher-studio.tail1234.ts.net', peers: [] } }));
  assert.match(html, /On Tailscale as <code>switcher-studio\.tail1234\.ts\.net<\/code>/);
  assert.match(html, /Remove add-on and sign out/);
});

test('an action error stays visible after the status reloads', async () => {
  const status = { host: host(), client: { connected: false } };
  const remote = createRemote({ api: async () => status, now: () => now });
  await remote.act('connect', async () => { throw new Error('pairing failed: check the code shown on the other Mac'); });
  assert.match(remote.html(), /role="alert">pairing failed: check the code/);
  await remote.load();
  assert.match(remote.html(), /pairing failed/, 'a status poll does not hide it');
  await remote.act('scan', async () => {});
  assert.doesNotMatch(remote.html(), /pairing failed/, 'the next action clears it');
});
