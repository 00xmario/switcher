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
  assert.match(html, /<code>studio\.local<\/code> · <code>100\.101\.2\.3 \(Tailscale\)<\/code>/);
  assert.match(html, /K7QF-M2XP/);
  assert.match(html, /Valid for 9:30/);
  assert.match(html, /MacBook Air<\/strong><span class="dim"> · last used 5 min ago/);
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
