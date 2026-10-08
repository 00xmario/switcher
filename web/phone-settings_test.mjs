import test from 'node:test';
import assert from 'node:assert/strict';
import { phoneHTML, steps } from './phone-settings.js';

const running = { enabled: true, running: true, state: 'Running', tailnet: 'you.github' };
const url = 'https://switcher-mac.tail1.ts.net';
const current = html => (html.match(/<li class="is-current"[\s\S]*?<strong>([^<]+)<\/strong>/) || [])[1];

test('setup starts by turning on Tailscale, and nothing else can be done yet', () => {
  const html = phoneHTML({ enabled: false, tailnet: {} });
  assert.equal(current(html), 'Tailscale on this Mac');
  assert.match(html, /data-phone-action="tailnet-on"/);
  assert.match(html, /Step 1 of 5/);
  assert.doesNotMatch(html, /data-phone-form/);
  assert.match(html, /dr-status warn">Off/);
});

test('each step checks itself off as Switcher sees it happen', () => {
  assert.match(phoneHTML({ tailnet: { enabled: true, running: true, state: 'NeedsLogin', auth_url: 'https://login.tailscale.com/a/x' } }),
    /href="https:\/\/login\.tailscale\.com\/a\/x"[^>]*>Sign in with Tailscale/);
  const https = phoneHTML({ tailnet: running });
  assert.equal(current(https), 'A secure address');
  assert.match(https, /href="https:\/\/login\.tailscale\.com\/admin\/dns"/);
  assert.match(https, /<li class="is-done"[\s\S]*?On you\.github/);
  const access = phoneHTML({ tailnet: { ...running, https: true } });
  assert.equal(current(access), 'Phone access');
  assert.match(access, /data-phone-action="access-on"/);
  const scan = phoneHTML({ enabled: true, ready: true, url, tailnet: { ...running, https: true, phone: true } });
  assert.equal(current(scan), 'Open it on your phone');
  assert.match(scan, /<svg class="qr"[^>]*aria-label="QR code for https:\/\/switcher-mac\.tail1\.ts\.net"/);
  assert.match(scan, /data-phone-copy="https:\/\/switcher-mac\.tail1\.ts\.net"/);
  const code = phoneHTML({ enabled: true, ready: true, url, tailnet: { ...running, https: true, phone: true }, waiting: [{ id: 'w1', name: 'marios-iphone' }] });
  assert.equal(current(code), 'Enter the code');
  assert.match(code, /data-phone-form/);
  assert.match(code, /marios-iphone is waiting/);
  assert.equal(steps({ tailnet: {} }).filter(s => s.done).length, 0);
});

test('once a phone is approved it shows the phones and a way to add another', () => {
  const html = phoneHTML({ enabled: true, ready: true, url, tailnet: { ...running, https: true, phone: true },
    waiting: [{ id: 'w1', name: 'ipad' }],
    devices: [{ id: 'p1', name: 'old <phone>', os: 'iOS', last_seen: new Date(Date.now() - 120000).toISOString() }] });
  assert.doesNotMatch(html, /phone-steps/);
  assert.match(html, /dr-status ok">Ready/);
  assert.match(html, /1 phone approved/);
  assert.match(html, /<svg class="qr"/);
  assert.match(html, /data-phone-form/);
  assert.match(html, /data-phone-action="deny" data-phone-id="w1"/);
  assert.match(html, /data-phone-action="revoke" data-phone-id="p1"/);
  assert.match(html, /old &lt;phone&gt;/);
  assert.match(html, /last used 2 min ago/);
});

test('turned off, approved phones are said to come back', () => {
  const html = phoneHTML({ enabled: false, tailnet: running, devices: [{ id: 'p1', name: 'phone' }] });
  assert.match(html, /1 approved phone gets back in when you turn it on/);
  assert.match(html, /Phone access is off/);
});
