// Settings section for using Switcher from a phone over Tailscale: turn it
// on, see what is still missing, approve a phone by the code it shows, and
// revoke phones.
import { switcherMark } from './brand.js';

const escape = value => String(value ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

export const PHONE_ICON = '<svg viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="6.5" y="2.5" width="11" height="19" rx="2.6"/><path d="M10.5 18.5h3"/></svg>';
const MAC_ICON = '<svg viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="4" y="5" width="16" height="10.5" rx="1.8"/><path d="M2 18.5h20"/></svg>';
const CHECK = '<svg viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="M5 12.5l4.5 4.5L19 7.5"/></svg>';
const DNS_SETTINGS = 'https://login.tailscale.com/admin/dns';

function ago(value, now) {
  const ms = Date.parse(value || '');
  if (!ms || ms < 0) return 'never';
  const minutes = Math.floor(Math.max(0, now - ms) / 60000);
  return minutes < 1 ? 'just now' : minutes < 60 ? `${minutes} min ago` : minutes < 1440 ? `${Math.floor(minutes / 60)} hr ago` : new Date(ms).toLocaleDateString();
}

// steps lists what phone access needs, in the order the user sets it up.
export function steps(status) {
  const t = status.tailnet || {};
  const signedIn = !!(t.enabled && t.running && t.state === 'Running');
  return [
    { done: signedIn, title: 'Tailscale add-on on and signed in', sub: 'Under Share between Macs, turn on Away from home.', action: signedIn ? '' : '<button type="button" class="quiet" data-phone-action="open-sharing">Open Share between Macs</button>' },
    { done: signedIn && !!t.https, title: 'MagicDNS and HTTPS certificates on', sub: 'One switch each in your tailnet’s DNS settings. They give this Mac a real certificate.', action: signedIn && !t.https ? `<a class="quiet-link" href="${DNS_SETTINGS}" target="_blank" rel="noopener noreferrer">Open DNS settings</a>` : '' },
    { done: false, neutral: true, title: 'Tailscale on your phone', sub: 'Install the Tailscale app and sign in with the same account.' },
    { done: (status.devices || []).length > 0, title: 'Approve your phone', sub: 'Open the address on your phone, then enter the code it shows here.' },
  ];
}

export function phoneHTML(status, { busy = '', error = '', now = Date.now() } = {}) {
  const enabled = !!status.enabled;
  const ready = !!status.ready;
  const devices = status.devices || [];
  const waiting = status.waiting || [];
  const state = ready ? 'is-on' : 'is-off';
  const pill = !enabled ? '<span class="dr-status warn">Off</span>' : ready ? '<span class="dr-status ok">Ready</span>' : '<span class="dr-status warn">Needs setup</span>';
  const node = (icon, name, sub, keeps) => `<div class="flow-node ${keeps ? 'is-host' : ''}"><span class="flow-icon">${icon}</span><strong>${name}</strong><small>${sub}</small></div>`;
  const phones = devices.length ? `${devices.length} phone${devices.length === 1 ? '' : 's'}` : 'Your phone';
  const hero = `<div class="settings-card remote-hero">
    <div class="flow ${state}" aria-hidden="true">${node(PHONE_ICON, phones, ready ? 'anywhere' : 'not connected')}<div class="flow-wire"><span></span>${ready ? '<em class="flow-tag">Tailscale · HTTPS</em>' : ''}</div>${node(MAC_ICON, 'This Mac', 'keeps the accounts', true)}</div>
    <div class="dr-settings-head">${pill}</div>
    <p class="settings-sub dr-lead">See usage, switch accounts and use banked resets from your phone, wherever you are. Only your own Tailscale devices reach it, and only phones you approve here get in.</p>
  </div>`;
  const access = `<div class="settings-card remote-section">
    <div class="settings-row">
      <div><strong>Phone access</strong><span class="dim"> · ${enabled ? (ready ? 'on' : 'on, not reachable yet') : devices.length ? `off; ${devices.length} approved phone${devices.length === 1 ? ' gets' : 's get'} back in when you turn it on` : 'off'}</span></div>
      <label class="switch-wrap"><input type="checkbox" data-phone-action="toggle" aria-label="Allow phone access over Tailscale" ${enabled ? 'checked' : ''} ${busy ? 'disabled' : ''}><span class="switch-visual"></span></label>
    </div>
    ${ready && status.url ? `<label class="field-label">Open on your phone</label>
      <div class="copy-row"><input readonly aria-label="Phone address" value="${escape(status.url)}"><button type="button" class="copy-btn" data-phone-copy="${escape(status.url)}">Copy</button></div>` : ''}
    ${enabled && status.problem ? `<p class="phone-problem">${escape(status.problem)}</p>` : ''}
    <p class="settings-sub">Like every HTTPS certificate, the one Tailscale issues for this address is listed in public certificate logs. That shows the name above and your tailnet’s name, not a way in. Turning access off keeps approved phones; revoke a phone you lost.</p>
  </div>`;
  const setup = enabled && !ready ? `<div class="settings-card">
    <div class="set-card-head"><h3>Set up</h3><p class="settings-sub">Once per Mac. Switcher checks the first two for you.</p></div>
    <ol class="phone-steps">${steps(status).map((s, i) => `<li class="${s.done ? 'is-done' : s.neutral ? 'is-neutral' : ''}">
      <span class="phone-step-n">${s.done ? CHECK : i + 1}</span>
      <div><strong>${s.title}</strong><span>${s.sub}</span>${s.action ? `<div class="phone-step-action">${s.action}</div>` : ''}</div>
    </li>`).join('')}</ol>
  </div>` : '';
  const approve = enabled ? `<div class="settings-card">
    <div class="set-card-head"><h3>Approve a phone</h3><p class="settings-sub">${ready ? `Open <strong>${escape(status.url)}</strong> on your phone and tap Pair. Then enter the code it shows.` : 'When the address works, open it on your phone and tap Pair. Then enter the code it shows.'}</p></div>
    ${waiting.map(w => `<div class="phone-device is-waiting">
      <span class="phone-device-icon">${PHONE_ICON}</span>
      <div><strong>${escape(w.name || 'A phone')}</strong><span>${w.approved ? 'Approved, opening on the phone' : `Waiting for its code${w.os ? ` · ${escape(w.os)}` : ''}`}</span></div>
      ${w.approved ? '' : `<button type="button" class="quiet" data-phone-action="deny" data-phone-id="${escape(w.id)}" ${busy ? 'disabled' : ''}>Deny</button>`}
    </div>`).join('')}
    <form class="phone-code" data-phone-form>
      <input name="code" inputmode="numeric" autocomplete="one-time-code" maxlength="7" placeholder="123 456" aria-label="Code shown on your phone" ${busy ? 'disabled' : ''}>
      <button type="submit" class="primary" ${busy ? 'disabled' : ''}>${busy === 'approve' ? 'Approving…' : 'Approve'}</button>
    </form>
    ${error ? `<p class="remote-error" role="alert">${escape(error)}</p>` : ''}
  </div>` : (error ? `<p class="remote-error" role="alert">${escape(error)}</p>` : '');
  const list = devices.length ? `<div class="settings-card">
    <div class="set-card-head"><h3>Approved phones</h3><p class="settings-sub">A phone stays signed in for 30 days after its last use, at most 90 days, and only on the Tailscale device it was approved on.</p></div>
    ${devices.map(d => `<div class="phone-device">
      <span class="phone-device-icon">${PHONE_ICON}</span>
      <div><strong>${escape(d.name || 'Phone')}</strong><span>${d.os ? `${escape(d.os)} · ` : ''}last used ${ago(d.last_seen, now)}</span></div>
      <button type="button" class="quiet danger" data-phone-action="revoke" data-phone-id="${escape(d.id)}" ${busy ? 'disabled' : ''}>Revoke</button>
    </div>`).join('')}
  </div>` : '';
  const tiles = `<div class="sec-hero phone-guards">
    <div class="sec-tile is-ok"><span class="sec-icon">${switcherMark({ size: 18, id: 'phone-guard-mark' })}</span><strong>Only your tailnet</strong><span>Not on the internet or your Wi‑Fi</span></div>
    <div class="sec-tile is-ok"><span class="sec-icon">${PHONE_ICON}</span><strong>Only approved phones</strong><span>Your devices, then a code on this Mac</span></div>
    <div class="sec-tile is-ok"><span class="sec-icon">${CHECK}</span><strong>Nothing to change</strong><span>No settings, logins or keys on the phone</span></div>
  </div>`;
  return `${hero}${tiles}${access}${setup}${approve}${list}`;
}

export function createPhoneSettings({ api, confirm = async () => true, toast = () => {}, openSection = () => {}, now = () => Date.now() }) {
  let view = null, status = null, busy = '', error = '', timer = null, shown = '';
  const post = (path, body) => api(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body || {}) });

  async function load() {
    try {
      status = await api('/api/phone');
    } catch (e) {
      error = e.message || 'Could not load phone access.';
    }
    render();
  }

  function html() {
    if (!status) return `<div class="settings-card"><p class="settings-sub">${escape(error || 'Loading…')}</p></div>`;
    return phoneHTML(status, { busy, error, now: now() });
  }

  function render() {
    if (!view) return;
    const markup = html();
    if (markup === shown) return;
    shown = markup;
    const input = view.querySelector('[data-phone-form] input');
    const value = input?.value || '';
    const focused = input && document.activeElement === input;
    view.innerHTML = markup;
    const next = view.querySelector('[data-phone-form] input');
    if (next && value) next.value = value;
    if (next && focused) next.focus({ preventScroll: true });
  }

  async function act(kind, work) {
    if (busy) return false;
    busy = kind;
    error = '';
    render();
    try {
      const result = await work();
      if (result && typeof result === 'object' && 'enabled' in result) status = result;
      return true;
    } catch (e) {
      error = e.message || 'That did not work.';
      return false;
    } finally {
      busy = '';
      await load();
    }
  }

  function bind(root) {
    if (root.dataset.phoneBound) return;
    root.dataset.phoneBound = '1';
    root.addEventListener('change', event => {
      if (!event.target.matches('[data-phone-action="toggle"]')) return;
      const on = event.target.checked;
      act('toggle', () => post('/api/phone/access', { on }));
    });
    root.addEventListener('click', async event => {
      const copy = event.target.closest('[data-phone-copy]');
      if (copy) {
        navigator.clipboard?.writeText(copy.dataset.phoneCopy).then(() => toast('Copied'), () => toast('Could not copy'));
        return;
      }
      const button = event.target.closest('button[data-phone-action]');
      if (!button || button.disabled) return;
      const id = button.dataset.phoneId;
      switch (button.dataset.phoneAction) {
        case 'open-sharing': return openSection('sharing');
        case 'deny': return act('deny', () => post('/api/phone/deny', { id }));
        case 'revoke':
          if (!await confirm({ title: 'Revoke this phone?', message: 'It is signed out right away and has to be approved again.', confirmLabel: 'Revoke', danger: true })) return;
          return act('revoke', () => api(`/api/phone/devices/${encodeURIComponent(id)}`, { method: 'DELETE' }));
      }
    });
    root.addEventListener('submit', async event => {
      if (!event.target.matches('[data-phone-form]')) return;
      event.preventDefault();
      const input = event.target.elements.code;
      const code = input.value.replace(/\D/g, '');
      if (code.length !== 6) { error = 'Enter the six digits shown on your phone.'; shown = ''; render(); return; }
      let name = '';
      const ok = await act('approve', async () => { name = (await post('/api/phone/approve', { code }))?.approved?.name || ''; });
      if (ok) {
        const field = view?.querySelector('[data-phone-form] input');
        if (field) field.value = '';
        toast(`Approved ${name || 'the phone'}`);
      }
    });
  }

  function mount(root) {
    view = root;
    shown = '';
    error = '';
    bind(root);
    render();
    load();
    clearInterval(timer);
    timer = setInterval(() => {
      if (!view?.isConnected) { clearInterval(timer); timer = null; return; }
      if (view.closest('[hidden]')) return;
      if (status?.enabled) load();
    }, 3000);
  }

  return { mount, html, load };
}
