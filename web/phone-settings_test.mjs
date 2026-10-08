import test from 'node:test';
import assert from 'node:assert/strict';
import { phoneHTML, steps } from './phone-settings.js';

const signedIn = { enabled: true, running: true, state: 'Running', https: true, phone: true };

test('off shows the switch and no approval form', () => {
  const html = phoneHTML({ enabled: false, tailnet: {} });
  assert.match(html, /data-phone-action="toggle"/);
  assert.doesNotMatch(html, /checked/);
  assert.doesNotMatch(html, /data-phone-form/);
  assert.match(html, /dr-status warn">Off/);
});

test('setup points at the first missing piece', () => {
  const noAddon = steps({ tailnet: {} });
  assert.equal(noAddon[0].done, false);
  assert.match(noAddon[0].action, /open-sharing/);
  const noHTTPS = steps({ tailnet: { ...signedIn, https: false } });
  assert.equal(noHTTPS[0].done, true);
  assert.match(noHTTPS[1].action, /login\.tailscale\.com\/admin\/dns/);
  const html = phoneHTML({ enabled: true, ready: false, problem: 'Turn on MagicDNS', tailnet: { ...signedIn, https: false } });
  assert.match(html, /Needs setup/);
  assert.match(html, /phone-steps/);
  assert.match(html, /Turn on MagicDNS/);
});

test('ready shows the address, waiting phones and approved phones', () => {
  const html = phoneHTML({ enabled: true, ready: true, url: 'https://switcher-mac.tail1.ts.net', tailnet: signedIn,
    waiting: [{ id: 'w1', name: 'marios-iphone', os: 'iOS' }],
    devices: [{ id: 'p1', name: 'old <phone>', os: 'iOS', last_seen: new Date(Date.now() - 120000).toISOString() }] });
  assert.match(html, /value="https:\/\/switcher-mac\.tail1\.ts\.net"/);
  assert.match(html, /dr-status ok">Ready/);
  assert.match(html, /data-phone-action="deny" data-phone-id="w1"/);
  assert.match(html, /data-phone-action="revoke" data-phone-id="p1"/);
  assert.match(html, /old &lt;phone&gt;/);
  assert.match(html, /last used 2 min ago/);
  assert.doesNotMatch(html, /phone-steps/);
});
