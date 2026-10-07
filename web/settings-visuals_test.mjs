import assert from 'node:assert/strict';
import test from 'node:test';
import { accountLayout, LAYOUTS, layoutChooserHTML, ladderHTML, securityHeroHTML, navHTML, SECTIONS } from './settings-visuals.js';
import { switcherMark } from './brand.js';

test('the account layout maps both stored flags to one choice and back', () => {
  assert.equal(accountLayout({}), 'expanded');
  assert.equal(accountLayout({ compact_accounts: true }), 'compact');
  assert.equal(accountLayout({ compact_accounts: true, merge_accounts: true }), 'merged', 'merge wins, as on the Accounts tab');
  for (const [id, flags] of Object.entries(LAYOUTS)) assert.equal(accountLayout(flags), id);
  assert.match(layoutChooserHTML('compact'), /aria-checked="true" data-account-layout="compact"/);
});

test('the run-out ladder shows banked resets as on or off', () => {
  assert.match(ladderHTML(false), /is-reset is-off" data-ladder-reset[\s\S]*?>Off</);
  assert.match(ladderHTML(true), /<em class="ladder-state">On</);
});

test('security says who can reach this Switcher, and only with the full status', () => {
  assert.doesNotMatch(securityHeroHTML({ lan_active: false, auth_enabled: false }), /is-warn/);
  assert.match(securityHeroHTML({ lan_active: false }), /This Mac only/);
  assert.match(securityHeroHTML({ lan_active: false }, true), /This Mac and paired Macs/);
  assert.match(securityHeroHTML({ lan_active: true, auth_enabled: false }), /is-warn/);
  assert.match(securityHeroHTML({ lan_active: false, auth_enabled: true, sessions: 1 }), /1 signed-in browser</);
  assert.equal(securityHeroHTML({ auth_enabled: true, password_set: true }), '', 'a browser on another device gets no summary');
});

test('navigation marks one section and every mark has its own ids', () => {
  const nav = navHTML('desktop');
  assert.equal((nav.match(/aria-current="page"/g) || []).length, 1);
  assert.match(nav, /data-section="desktop" aria-current="page"/);
  assert.equal(SECTIONS.length, new Set(SECTIONS.map(s => s.id)).size);
  const a = switcherMark(), b = switcherMark();
  assert.notEqual(a.match(/id="([^"]+)-cut"/)[1], b.match(/id="([^"]+)-cut"/)[1]);
});
