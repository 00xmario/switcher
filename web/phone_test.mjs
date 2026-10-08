import test from 'node:test';
import assert from 'node:assert/strict';
import { accountHTML, stateHTML, pairHTML, windowHTML, duration, esc } from './phone.js';

const now = 1_800_000_000;

test('a window shows what is left and when it resets', () => {
  const html = windowHTML({ label: 'Weekly', used_percent: 92, resets_at: now + 2 * 86400 + 3600 }, now);
  assert.match(html, /8% left/);
  assert.match(html, /resets in 2d 1h/);
  assert.match(html, /is-low/);
  assert.equal(duration(30), '1m');
});

test('an account offers a banked reset and switching only where the phone may', () => {
  const codex = { id: 'codex-a', provider: 'codex', email: 'a@example.com', plan: 'prolite', active: true,
    exhausted_until: now + 3600, usage: { windows: [{ label: 'Weekly', used_percent: 100 }] },
    reset_credits: { count: 2, next_id: 'credit_1' } };
  const html = accountHTML(codex, now);
  assert.match(html, /data-reset="codex-a"/);
  assert.match(html, /class="button primary" data-reset/);
  assert.match(html, /Out of usage · back in 1h 0m/);
  assert.doesNotMatch(html, /data-use=/);
  const claude = { id: 'claude-b', provider: 'claude', email: 'b@example.com', active: false, native_switch_available: true };
  assert.doesNotMatch(accountHTML(claude, now), /data-use=/);
  assert.match(accountHTML({ ...claude, native_switch_available: false }, now), /data-use="claude-b"/);
});

test('account text is escaped', () => {
  const html = accountHTML({ id: 'x', provider: 'codex', email: '<img src=x onerror=alert(1)>' }, now);
  assert.doesNotMatch(html, /<img/);
  assert.equal(esc(`"'&`), '&quot;&#39;&amp;');
});

test('providers follow the dashboard order and hidden ones stay hidden', () => {
  const html = stateHTML({ order: ['claude', 'codex'], hidden: ['claude'], accounts: [
    { id: 'c', provider: 'claude', email: 'c@example.com' },
    { id: 'x', provider: 'codex', email: 'x@example.com' },
  ] }, now);
  assert.match(html, /Codex/);
  assert.doesNotMatch(html, /c@example.com/);
});

test('pairing shows the code in two groups with its expiry', () => {
  const html = pairHTML({ code: '482913', expires_at: now + 125 }, now);
  assert.match(html, /482<span><\/span>913/);
  assert.match(html, /Expires in 2:05/);
  assert.match(html, /switcher phone approve 482913/);
  assert.match(pairHTML(null), /id="pair-start"/);
});
