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

const formatStart = source.indexOf('function resetTime(untilUnix, description = ');
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

const accountContext = vm.createContext({
  data: { active: { codex: 'active-id' } },
  pendingRechecks: new Map(),
  PLAN_NAMES: { pro: 'Pro 20x', prolite: 'Pro 5x' },
  ADD_METHOD: { codex: 'browser', opencode: 'key' },
  LOGOS: { codex: '<svg aria-hidden="true"></svg>' },
  statusOf: account => ({ cls: account.id === 'active-id' ? 'active' : '', label: 'Idle' }),
  healthText: () => 'Usage current',
  escapeHTML: hover.escapeHTML,
  windowHTML: () => '',
  fmtRemaining: seconds => seconds > Date.now() / 1000 ? '1h' : null,
});
vm.runInContext(source.slice(formatStart, formatEnd) + '\n' +
  source.slice(windowEnd, source.indexOf('\n// Update banner:', windowEnd)) +
  '\nthis.accountHTML = accountHTML; this.accountMenuHTML = accountMenuHTML;', accountContext);
const withoutCredit = accountContext.accountHTML({ id: 'active-id', provider: 'codex',
  email: 'active@example.com', plan: 'pro', health: { condition: 'usage_current' } });
const withCredit = accountContext.accountHTML({ id: 'banked-id', provider: 'codex',
  email: 'banked@example.com', plan: 'prolite', health: { condition: 'usage_current' },
  reset_credits: { count: 1 } });
assert.doesNotMatch(withoutCredit, /class="banked"|data-act="use-reset"/);
assert.match(withCredit, /class="meta">[\s\S]*Pro 5x[\s\S]*<span class="banked"[^>]*>⚡ 1 banked<\/span>[\s\S]*<\/div>\s*<div class="account-health/);
assert.match(withCredit, /data-account-menu[^>]*aria-haspopup="menu"[^>]*aria-expanded="false"/);
assert.doesNotMatch(withCredit.match(/<div class="actions">([\s\S]*?)<\/div>/)?.[1] || '', /class="banked"|data-act="use-reset"|data-act="relogin"|data-act="delete"/);
const menu = accountContext.accountMenuHTML({ id: 'banked-id', provider: 'codex',
  reset_credits: { count: 1 } });
assert.match(menu, /role="menuitem" data-act="use-reset" data-account-id="banked-id"/);
assert.match(menu, /role="menuitem" data-act="relogin"/);
assert.match(menu, /role="menuitem" data-act="delete"/);
assert.doesNotMatch(accountContext.accountMenuHTML({ id: 'active-id', provider: 'codex' }), /use-reset/);
assert.doesNotMatch(accountContext.accountMenuHTML({ id: 'key-id', provider: 'opencode' }), /relogin/);

const compactCard = accountContext.compactAccountHTML({
  id: 'banked-id', provider: 'codex', email: 'banked@example.com', plan: 'prolite',
  health: { condition: 'usage_current' }, reset_credits: { count: 2, expires_at: [reset, reset + 86400] },
  usage: { available: true, windows: [
    { label: 'Weekly', used_percent: 25, resets_at: reset },
    { label: 'Session', used_percent: 100, resets_at: null },
  ] },
});
assert.match(compactCard, /class="compact-card-head"/);
assert.match(compactCard, /Plan<\/span><strong>Pro 5x<\/strong>/);
assert.match(compactCard, /⚡ 2 banked/);
assert.equal((compactCard.match(/class="compact-window"/g) || []).length, 2);
assert.match(compactCard, /Weekly[\s\S]*75% <span>left<\/span>[\s\S]*<time datetime="[^"]+" title="Provider-reported reset:/);
assert.match(compactCard, /Session[\s\S]*0% <span>left<\/span>/);
assert.match(compactCard, /aria-valuenow="75"/);
assert.match(compactCard, /data-act="recheck"[\s\S]*Refresh quota[\s\S]*data-act="activate"/);
assert.match(compactCard, /data-account-menu/);
assert.equal((compactCard.match(/class="compact-credit"/g) || []).length, 2);
assert.match(compactCard, /Banked reset expiry[\s\S]*Reset 1[\s\S]*title="Banked reset expires:/);

let compactPage = false;
const providersEl = { hidden: false, innerHTML: '', querySelectorAll: () => [] };
const renderContext = vm.createContext({
  data: { active: {}, compact_accounts: true, accounts: [
    { id: 'a', provider: 'codex' }, { id: 'b', provider: 'codex' },
  ], order: ['codex'], hidden: [] },
  providersEl, activeAccountMenu: null,
  usagePage: { hidden: true }, settingsPage: { hidden: true },
  renderSettings: () => {},
  document: {
    activeElement: null,
    body: { classList: { toggle: (_, enabled) => { compactPage = enabled; } } },
    getElementById: id => id === 'providers' ? providersEl : { innerHTML: '' },
    querySelector: selector => selector === 'footer.footnote' ? { hidden: false } : null,
    querySelectorAll: () => [],
  },
  closeAccountMenu: () => {}, updateBannerHTML: () => '',
  escapeHTML: hover.escapeHTML,
  LOGOS: { codex: '<svg></svg>' }, PROVIDER_NAMES: { codex: 'Codex' },
  accountHTML: account => `expanded-${account.id}`,
  compactAccountHTML: account => `compact-${account.id}`,
});
const renderStart = source.indexOf('function render() {');
const renderEnd = source.indexOf('\n/* ---------- usage ---------- */', renderStart);
vm.runInContext(source.slice(renderStart, renderEnd) + '\nthis.render = render;', renderContext);
renderContext.render();
assert.equal(compactPage, true);
assert.match(providersEl.innerHTML, /class="account-list compact-grid"[\s\S]*compact-a[\s\S]*compact-b/);
renderContext.data.compact_accounts = false;
renderContext.render();
assert.equal(compactPage, false);
assert.match(providersEl.innerHTML, /class="account-list "[\s\S]*expanded-a[\s\S]*expanded-b/);
renderContext.data.compact_accounts = true;
renderContext.render();
const pageStart = source.indexOf('setPage = function (page) {');
const pageEnd = source.indexOf('\n// lanStateText', pageStart);
vm.runInContext(source.slice(pageStart, pageEnd), renderContext);
renderContext.setPage('usage');
assert.equal(compactPage, true, 'changing tabs must not change the app width while compact view is enabled');

const cliSetupStart = source.indexOf('function cliSetupRowHTML(client) {');
const cliSetupEnd = source.indexOf('\nasync function renderSettings()', cliSetupStart);
assert.ok(cliSetupStart >= 0 && cliSetupEnd > cliSetupStart);
const cliContext = vm.createContext({ escapeHTML: hover.escapeHTML, Number });
const escapeStart = source.indexOf('function escapeHTML(s) {');
const escapeEnd = source.indexOf('\n// fmtRemaining', escapeStart);
vm.runInContext(source.slice(escapeStart, escapeEnd) + '\n' + source.slice(cliSetupStart, cliSetupEnd) +
  '\nthis.cliSetupRowHTML = cliSetupRowHTML;', cliContext);
const codexEvidence = {
  id: 'codex', name: 'Codex CLI', capability: 'configure_user_file', stage: 'available',
  configuration: { condition: 'ready', expected_url: 'http://127.0.0.1:9123/codex/v1',
    ownership: 'tracked', restore_action: 'restore' },
  accounts: { evidence: 'known', count: 2 },
};
const configured = cliContext.cliSetupRowHTML(codexEvidence);
assert.match(configured, /Configured in inspected user file/);
assert.match(configured, /2 Switcher accounts stored/);
assert.match(configured, /Native login and request routing have not been tested/);
assert.match(configured, /data-cli-action="restore">Restore previous setup/);
assert.match(configured, /data-cli-action="test">Test Switcher route \(uses quota\)/);
assert.match(configured, /Switcher route not tested; native CLI request not tested/);
assert.doesNotMatch(configured, /Connected|data-cli-action="configure"/);
const missingConfig = cliContext.cliSetupRowHTML({ ...codexEvidence,
  configuration: { condition: 'missing', install_action: 'configure' } });
assert.match(missingConfig, /No Switcher selection in inspected Codex config/);
assert.match(missingConfig, /Configure Codex user settings/);
assert.match(missingConfig, /data-cli-action="configure">Configure Codex/);
assert.doesNotMatch(missingConfig, /backup/i);
assert.doesNotMatch(missingConfig, /No config at inspected path|Create a Codex user config/);
for (const condition of ['invalid', 'unreadable']) {
  assert.doesNotMatch(cliContext.cliSetupRowHTML({ ...codexEvidence,
    configuration: { condition } }), /data-cli-action="(configure|update|reselect|restore|remove-legacy)"/);
}
assert.match(cliContext.cliSetupRowHTML({ ...codexEvidence,
  configuration: { condition: 'not_selected', ownership: 'tracked', install_action: 'reselect',
    restore_action: 'restore' } }), /data-cli-action="reselect">Select Switcher again/);
assert.match(cliContext.cliSetupRowHTML({ ...codexEvidence,
  configuration: { condition: 'ready', ownership: 'legacy_candidate', restore_action: 'remove_legacy' } }),
  /data-cli-action="remove-legacy">Remove legacy entry/);
const informationalClient = {
  id: 'claude-code', name: 'Claude Code', capability: 'information_only',
  stage: 'protocol_validation_pending', configuration: { condition: 'not_checked' },
  accounts: { evidence: 'unknown', count: null },
};
const informational = cliContext.cliSetupRowHTML(informationalClient);
assert.match(informational, /Setup under validation/);
assert.match(informational, /Switcher account count unavailable/);
assert.doesNotMatch(informational, /data-cli-action|Connected/);
assert.doesNotMatch(cliContext.cliSetupRowHTML({ ...informationalClient, name: '<img src=x>' }), /<img src=x>/);
for (const [reason, expected] of Object.entries({
  oauth_coexistence_unverified: 'Native OAuth coexistence unverified',
  v2_account_model_transport_unverified: 'OpenCode v2 account, model, and transport unverified',
  entitlement_and_relay_unverified: 'Grok entitlement and WebSocket relay unverified',
  byok_is_different_billing: 'Copilot CLI BYOK is a separate billing path',
  subscription_route_unverified: 'Gemini CLI subscription route unverified',
  agy_protocol_unverified: 'Antigravity CLI protocol unverified',
})) {
  const html = cliContext.cliSetupRowHTML({ ...informationalClient, reason_code: reason,
    next_step: 'Keep native settings unchanged.' });
  assert.ok(html.includes(expected) && html.includes('Keep native settings unchanged.'));
  assert.doesNotMatch(html, /Connected|data-cli-action/);
}
const unknownReadiness = cliContext.cliSetupRowHTML({ ...informationalClient, reason_code: 'unknown_code',
  next_step: '<script>untrusted instruction</script>' });
assert.match(unknownReadiness, /Native setup is not verified/);
assert.doesNotMatch(unknownReadiness, /untrusted instruction|data-cli-action/);
for (const inherited of ['constructor', '__proto__', 'toString']) {
  const html = cliContext.cliSetupRowHTML({ ...informationalClient, reason_code: inherited,
    next_step: 'untrusted instruction' });
  assert.match(html, /Native setup is not verified/);
  assert.doesNotMatch(html, /untrusted instruction|data-cli-action/);
}
const testedRoute = cliContext.cliSetupRowHTML({ ...codexEvidence,
  verification: { condition: 'last_success', last_success: reset, model: 'gpt-5.5' } });
assert.match(testedRoute, /Last successful Switcher route test:/);
assert.match(testedRoute, /Native CLI request not tested/);
assert.doesNotMatch(testedRoute, /Connected|native CLI verified/i);
assert.match(cliContext.cliSetupRowHTML({ ...codexEvidence,
  verification: { condition: 'historical', last_success: reset, model: 'gpt-5.5' } }),
  /Historical Switcher route test:/);

console.log('State ordering and fast Recheck reconciliation: OK');
