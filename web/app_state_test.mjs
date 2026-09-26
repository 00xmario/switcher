import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

// Exercise the actual async state handler without a browser or extra deps.
const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
const start = source.indexOf('async function refreshState() {');
const end = source.indexOf('\nfunction statusOf(', start);
const settleStart = source.indexOf('function settleRechecks(accounts) {');
const settleEnd = source.indexOf('\nfunction healthText(', settleStart);
const healthStart = source.indexOf('function healthText(health) {', settleEnd);
const healthEnd = source.indexOf('\n// PROVIDER_BAR_COLORS', healthStart);
assert.ok(start >= 0 && end > start && settleStart >= 0 && settleEnd > settleStart &&
  healthStart >= 0 && healthEnd > healthStart,
  'state functions must remain findable');

const responses = [];
const pendingRechecks = new Map();
const accountStatusAnnouncer = { textContent: '' };
let renders = 0;
const context = vm.createContext({
  data: null,
  stateEpoch: 0,
  stateRequestId: 0,
  lastAppliedStateRequestId: 0,
  pendingRechecks,
  accountStatusAnnouncer,
  PROVIDER_NAMES: { fake: 'Fake' },
  api: () => new Promise(resolve => responses.push(resolve)),
  render: () => { renders++; },
  renderAddProviderMenu: () => {},
  toast: message => { throw new Error(message); },
});
vm.runInContext(source.slice(settleStart, settleEnd) + '\n' + source.slice(healthStart, healthEnd) +
  '\n' + source.slice(start, end) +
  '\nthis.refreshState = refreshState;', context);

const older = context.refreshState();
const newer = context.refreshState();
responses[1]({ value: 'new', accounts: [] });
await newer;
responses[0]({ value: 'old', accounts: [] });
await older;
assert.equal(context.data.value, 'new', 'late older GET must not replace newer state');
assert.equal(renders, 1);

pendingRechecks.set('a', { accepted: true });
const accountA = { id: 'a', provider: 'fake', email: 'a@example.com', health: { condition: 'usage_current' } };
context.data = { value: 'same', accounts: [accountA] };
const before = renders;
const same = context.refreshState();
responses[2]({ value: 'same', accounts: [accountA] });
await same;
assert.equal(pendingRechecks.has('a'), false, 'fast unchanged recheck must settle');
assert.equal(renders, before + 1, 'settled recheck must repaint even when JSON is unchanged');
assert.match(accountStatusAnnouncer.textContent, /Fake a@example.com: Usage current/);

const preRecheck = context.refreshState();
context.stateEpoch++;
pendingRechecks.set('a', { accepted: true });
const postRecheck = context.refreshState();
responses[3]({ value: 'old', accounts: [accountA] });
await preRecheck;
assert.equal(pendingRechecks.has('a'), true, 'pre-recheck response must not settle the new request');
responses[4]({ value: 'checking', accounts: [{ ...accountA, health: { condition: 'checking' } }] });
await postRecheck;
assert.equal(context.data.value, 'checking');
assert.equal(pendingRechecks.has('a'), true);

pendingRechecks.set('b', { accepted: true });
const both = context.refreshState();
responses[5]({ accounts: [accountA, {
  id: 'b', provider: 'fake', email: 'b@example.com', health: { condition: 'usage_unavailable' },
}] });
await both;
assert.match(accountStatusAnnouncer.textContent, /a@example.com.*b@example.com/,
  'simultaneous checks must announce both account outcomes');

pendingRechecks.set('removed', { accepted: true });
const afterDelete = context.refreshState();
responses[6]({ accounts: [accountA] });
await afterDelete;
assert.equal(pendingRechecks.has('removed'), false,
  'removing an account must discard its in-flight Recheck state');

const formatStart = source.indexOf('function resetTime(untilUnix) {');
const formatEnd = source.indexOf('\nfunction toast(', formatStart);
const windowStart = source.indexOf('function windowHTML(win, providerID) {');
const windowEnd = source.indexOf('\nfunction accountHTML(', windowStart);
assert.ok(formatStart >= 0 && formatEnd > formatStart && windowStart >= 0 && windowEnd > windowStart);
const hover = vm.createContext({
  PROVIDER_NAMES: { fake: 'Fake' }, PROVIDER_BAR_COLORS: { fake: '#000' },
  fmtRemaining: seconds => seconds > Date.now() / 1000 ? '1h' : null,
  escapeHTML: s => String(s).replace(/&/g, '&amp;').replace(/"/g, '&quot;'),
  Date, Intl, Number, Math,
});
vm.runInContext(source.slice(formatStart, formatEnd) + '\n' + source.slice(windowStart, windowEnd) +
  '\nthis.windowHTML = windowHTML;', hover);
const reset = Math.floor(Date.now() / 1000) + 3600;
const markup = hover.windowHTML({ label: 'Session', used_percent: 20, resets_at: reset }, 'fake');
assert.match(markup, /<time class="reset-badge" datetime="\d{4}-\d\d-\d\dT[^\"]+Z" title="Provider-reported reset:/);
assert.match(markup, /<div class="recover" title="Provider-reported reset:/);
for (const missing of [null, 0, reset - 7200]) {
  const html = hover.windowHTML({ label: 'Session', used_percent: 20, resets_at: missing }, 'fake');
  assert.doesNotMatch(html, /Provider-reported reset:|<time class="reset-badge"/);
}

console.log('State ordering and fast Recheck reconciliation: OK');
