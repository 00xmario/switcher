// Settings section for using Switcher from a phone over Tailscale. Until a
// phone is approved it is a guided setup whose steps check themselves off as
// Switcher sees them happen; after that it shows the phone address, a QR code
// for adding another phone, and the approved phones.
import { switcherMark } from './brand.js';
import { qrSVG } from './qr.js';

const escape = value => String(value ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

export const PHONE_ICON = '<svg viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="6.5" y="2.5" width="11" height="19" rx="2.6"/><path d="M10.5 18.5h3"/></svg>';
const CHECK = '<svg viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round"><path d="M5 12.5l4.5 4.5L19 7.5"/></svg>';
const LOCK = '<svg viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><rect x="5" y="10.5" width="14" height="10" rx="2.2"/><path d="M8.5 10.5V8a3.5 3.5 0 0 1 7 0v2.5"/></svg>';
const ARROW = '<svg viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M7 17L17 7M9 7h8v8"/></svg>';
const DNS_SETTINGS = 'https://login.tailscale.com/admin/dns';
const TAILSCALE_APP = 'https://tailscale.com/download';

function ago(value, now) {
  const ms = Date.parse(value || '');
  if (!ms || ms < 0) return 'never';
  const minutes = Math.floor(Math.max(0, now - ms) / 60000);
  return minutes < 1 ? 'just now' : minutes < 60 ? `${minutes} min ago` : minutes < 1440 ? `${Math.floor(minutes / 60)} hr ago` : new Date(ms).toLocaleDateString();
}

const spinner = text => `<span class="phone-wait"><i class="phone-spin" aria-hidden="true"></i>${text}</span>`;

// mockHTML is the phone in the hero: Switcher's phone page in miniature.
function mockHTML() {
  return `<div class="phone-mock" aria-hidden="true">
    <div class="pm-island"></div>
    <div class="pm-screen">
      <div class="pm-top">${switcherMark({ tile: true, size: 18, id: 'pm-mark' })}<b>Switcher</b></div>
      <div class="pm-card is-active"><div class="pm-line"><i class="pm-email"></i><span class="pm-pill">In use</span></div>
        <div class="pm-bar"><i style="--w:68%"></i></div><div class="pm-bar"><i style="--w:34%;--d:.3s"></i></div></div>
      <div class="pm-card"><div class="pm-line"><i class="pm-email is-short"></i></div><div class="pm-bar"><i style="--w:88%;--d:.6s"></i></div></div>
      <div class="pm-reset">Use banked reset <b>2</b></div>
    </div>
  </div>`;
}

// steps lists what phone access needs, in order, each checked live.
export function steps(status) {
  const t = status.tailnet || {};
  const signedIn = !!(t.enabled && t.running && t.state === 'Running');
  const https = signedIn && !!t.https;
  const serving = https && !!status.enabled && !!status.ready;
  const scanned = (status.waiting || []).length > 0 || (status.devices || []).length > 0;
  const approved = (status.devices || []).length > 0;
  let tailscale;
  if (!t.enabled) tailscale = `<button type="button" class="primary" data-phone-action="tailnet-on">Turn on Tailscale</button><span class="phone-hint">Adds this Mac to your tailnet with Switcher’s add-on${t.installed ? '' : ' (19 MB)'}. Nothing else changes on this Mac.</span>`;
  else if (t.downloading) tailscale = spinner('Downloading the add-on…');
  else if (t.error) tailscale = `<span class="phone-error">${escape(t.error)}</span>`;
  else if (!t.running) tailscale = spinner('Starting…');
  else if (t.state === 'NeedsLogin' && t.auth_url) tailscale = `<a class="button primary" href="${escape(t.auth_url)}" target="_blank" rel="noopener noreferrer">Sign in with Tailscale ${ARROW}</a><span class="phone-hint">Use the account you will sign in with on your phone.</span>`;
  else tailscale = spinner('Connecting to Tailscale…');
  return [
    { key: 'tailscale', done: signedIn, title: 'Tailscale on this Mac', doneText: t.tailnet ? `On ${escape(t.tailnet)}` : 'Connected', action: tailscale },
    { key: 'https', done: https, title: 'A secure address', doneText: 'HTTPS certificates are on',
      action: `<a class="button primary" href="${DNS_SETTINGS}" target="_blank" rel="noopener noreferrer">Open Tailscale DNS settings ${ARROW}</a>
        <span class="phone-hint">Turn on <b>MagicDNS</b> and <b>HTTPS Certificates</b>. This page notices by itself.</span>${spinner('Waiting for Tailscale…')}` },
    { key: 'access', done: serving, title: 'Phone access', doneText: 'On',
      action: status.enabled ? spinner(escape(status.problem || 'Starting the secure address…')) : '<button type="button" class="primary" data-phone-action="access-on">Turn on phone access</button>' },
    { key: 'scan', done: scanned, title: 'Open it on your phone', doneText: 'Your phone found it',
      action: status.url ? `<div class="phone-scan">${qrFrame(status.url)}<div class="phone-scan-text">
        <strong>Point your iPhone camera here</strong>
        <span class="phone-hint">Your phone needs the <a href="${TAILSCALE_APP}" target="_blank" rel="noopener noreferrer">Tailscale app</a>, signed in as you. Then tap <b>Pair this phone</b>.</span>
        ${copyChip(status.url)}</div></div>` : '' },
    { key: 'approve', done: approved, title: 'Enter the code', doneText: 'Approved',
      action: codeEntry(status) },
  ];
}

function qrFrame(url) {
  let code = '';
  try { code = qrSVG(url, { label: `QR code for ${url}` }); } catch { return ''; }
  return `<div class="qr-frame">${code}<span class="qr-scan" aria-hidden="true"></span><span class="qr-corners" aria-hidden="true"><i></i><i></i><i></i><i></i></span></div>`;
}

function copyChip(text) {
  return `<button type="button" class="set-chip" data-phone-copy="${escape(text)}" title="Click to copy"><code>${escape(text)}</code></button>`;
}

// codeEntry is six boxes over one real input, so typing, pasting and
// autofill all work; the boxes only show what the input holds.
function codeEntry(status, { busy = '' } = {}) {
  const waiting = (status.waiting || []).filter(w => !w.approved);
  const deny = w => `<button type="button" class="quiet" data-phone-action="deny" data-phone-id="${escape(w.id)}" ${busy ? 'disabled' : ''}>Deny</button>`;
  const who = waiting.map(w => `<span class="phone-who"><i class="phone-ping"></i>${escape(w.name || 'A phone')} is waiting ${deny(w)}</span>`).join('');
  return `<form class="code-entry" data-phone-form>
      <label class="code-boxes">
        <input name="code" inputmode="numeric" autocomplete="one-time-code" maxlength="7" pattern="[0-9 ]*" aria-label="Code shown on your phone" ${busy ? 'disabled' : ''}>
        <span class="code-cells" aria-hidden="true"><i></i><i></i><i></i><b></b><i></i><i></i><i></i></span>
      </label>
      <button type="submit" class="primary" ${busy ? 'disabled' : ''}>${busy === 'approve' ? 'Approving…' : 'Approve'}</button>
    </form>${who}`;
}

export function phoneHTML(status, { busy = '', error = '', now = Date.now(), celebrate = false } = {}) {
  const enabled = !!status.enabled;
  const ready = !!status.ready;
  const devices = status.devices || [];
  const waiting = (status.waiting || []).filter(w => !w.approved);
  const list = steps(status);
  const setup = !devices.length;
  const done = list.filter(s => s.done).length;
  const current = list.findIndex(s => !s.done);
  const pill = !enabled ? '<span class="dr-status warn">Off</span>' : ready ? '<span class="dr-status ok">Ready</span>' : '<span class="dr-status warn">Setting up</span>';

  const hero = `<div class="settings-card set-hero phone-hero ${celebrate ? 'is-celebrating' : ''}">
    <div class="set-hero-status">${pill}</div>
    <div class="phone-hero-art">${mockHTML()}
      <span class="phone-float is-a">${LOCK}Your tailnet only</span>
      <span class="phone-float is-b">${CHECK}Approved on this Mac</span>
      ${celebrate ? `<span class="phone-burst" aria-hidden="true">${CHECK}</span>` : ''}
    </div>
    <div class="phone-hero-text">
      <h3>Switcher in your pocket</h3>
      <p>See usage, switch accounts and use banked resets from your phone, wherever you are. Only your own Tailscale devices reach it, and only phones you approve here get in.</p>
      ${setup ? `<div class="phone-progress" role="progressbar" aria-valuemin="0" aria-valuemax="${list.length}" aria-valuenow="${done}" aria-label="Setup progress"><i style="--p:${done / list.length}"></i></div>
        <span class="phone-progress-text">${done === list.length ? 'All set' : `Step ${current + 1} of ${list.length}`}</span>`
        : `<div class="phone-ready-line">${ready ? `${devices.length} phone${devices.length === 1 ? '' : 's'} approved` : 'Phone access is off'}</div>`}
    </div>
  </div>`;

  const stepper = setup ? `<div class="settings-card phone-setup">
    <ol class="phone-steps">${list.map((s, i) => {
      const state = s.done ? 'is-done' : i === current ? 'is-current' : 'is-todo';
      return `<li class="${state}" style="--i:${i}">
        <span class="phone-step-n">${s.done ? CHECK : i + 1}</span>
        <div class="phone-step-body"><strong>${s.title}</strong>${s.done ? `<span class="phone-step-done">${s.doneText}</span>` : ''}
          ${i === current ? `<div class="phone-step-action">${s.key === 'approve' ? codeEntry(status, { busy }) : s.action}</div>` : ''}</div>
      </li>`;
    }).join('')}</ol>
    ${error ? `<p class="remote-error" role="alert">${escape(error)}</p>` : ''}
  </div>` : '';

  const add = !setup && enabled ? `<div class="settings-card phone-add ${waiting.length ? 'has-waiting' : ''}">
    ${ready && status.url ? `<div class="phone-scan">${qrFrame(status.url)}<div class="phone-scan-text">
      <strong>${waiting.length ? 'Enter the code it shows' : 'Add a phone'}</strong>
      <span class="phone-hint">${waiting.length ? 'The phone shows a six-digit code after you tap Pair this phone.' : 'Scan with the phone’s camera, tap Pair this phone, then enter the code it shows.'}</span>
      ${copyChip(status.url)}
      ${codeEntry(status, { busy })}
    </div></div>` : `<p class="phone-hint">${escape(status.problem || 'Starting the secure address…')}</p>`}
    ${error ? `<p class="remote-error" role="alert">${escape(error)}</p>` : ''}
  </div>` : '';

  const phones = devices.length ? `<div class="settings-card">
    <div class="set-card-head"><h3>Approved phones</h3></div>
    <ul class="phone-list">${devices.map(d => `<li class="phone-device">
      <span class="phone-device-icon">${PHONE_ICON}</span>
      <div><strong>${escape(d.name || 'Phone')}</strong><span>${d.os ? `${escape(d.os)} · ` : ''}last used ${ago(d.last_seen, now)}</span></div>
      <button type="button" class="quiet danger" data-phone-action="revoke" data-phone-id="${escape(d.id)}" ${busy ? 'disabled' : ''}>Revoke</button>
    </li>`).join('')}</ul>
    <p class="set-foot">A phone stays signed in for 30 days after its last use, at most 90 days, and only on the Tailscale device it was approved on.</p>
  </div>` : '';

  const access = `<div class="settings-card">
    <div class="settings-row set-row-icon">
      <span class="set-icon">${PHONE_ICON}</span>
      <div><strong>Phone access</strong><span class="dim">${enabled ? (ready ? 'On' : 'On, not reachable yet') : devices.length ? `Off; ${devices.length} approved phone${devices.length === 1 ? ' gets' : 's get'} back in when you turn it on` : 'Off'}</span></div>
      <label class="switch-wrap"><input type="checkbox" data-phone-action="toggle" aria-label="Allow phone access over Tailscale" ${enabled ? 'checked' : ''} ${busy ? 'disabled' : ''}><span class="switch-visual"></span></label>
    </div>
    <p class="set-foot">${LOCK}Like every HTTPS certificate, the one Tailscale issues for this address is listed in public certificate logs. That shows its name and your tailnet’s, not a way in. Turning access off keeps approved phones; revoke a phone you lost.</p>
  </div>`;

  return `${hero}${stepper}${add}${phones}${access}`;
}

export function createPhoneSettings({ api, confirm = async () => true, toast = () => {}, openSection = () => {}, now = () => Date.now() }) {
  let view = null, status = null, busy = '', error = '', timer = null, shown = '', celebrate = false, approvedBefore = -1;
  const post = (path, body) => api(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body || {}) });

  async function load() {
    try {
      status = await api('/api/phone');
      const count = (status.devices || []).length;
      if (approvedBefore >= 0 && count > approvedBefore) {
        celebrate = true;
        setTimeout(() => { celebrate = false; render(); }, 2600);
      }
      approvedBefore = count;
    } catch (e) {
      error = e.message || 'Could not load phone access.';
    }
    render();
  }

  function html() {
    if (!status) return `<div class="settings-card"><p class="settings-sub">${escape(error || 'Loading…')}</p></div>`;
    return phoneHTML(status, { busy, error, now: now(), celebrate });
  }

  // cells mirrors the code input in the six boxes.
  function cells() {
    view?.querySelectorAll('[data-phone-form]').forEach(form => {
      const input = form.elements.code;
      const digits = input.value.replace(/\D/g, '');
      const boxes = form.querySelectorAll('.code-cells i');
      boxes.forEach((box, i) => {
        box.textContent = digits[i] || '';
        box.classList.toggle('is-filled', i < digits.length);
        box.classList.toggle('is-next', i === digits.length && document.activeElement === input);
      });
    });
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
    cells();
  }

  async function act(kind, work) {
    if (busy) return false;
    busy = kind;
    error = '';
    render();
    try {
      const result = await work();
      if (result && typeof result === 'object' && 'enabled' in result && 'devices' in result) status = result;
      return true;
    } catch (e) {
      error = e.message || 'That did not work.';
      return false;
    } finally {
      busy = '';
      await load();
    }
  }

  async function approve(form) {
    const input = form.elements.code;
    const code = input.value.replace(/\D/g, '');
    if (code.length !== 6) { error = 'Enter the six digits shown on your phone.'; shown = ''; render(); return; }
    let name = '';
    const ok = await act('approve', async () => { name = (await post('/api/phone/approve', { code }))?.approved?.name || ''; });
    const field = view?.querySelector('[data-phone-form] input');
    if (ok) {
      if (field) field.value = '';
      toast(`Approved ${name || 'the phone'}`);
    } else {
      form = view?.querySelector('[data-phone-form]');
      form?.classList.remove('is-wrong');
      void form?.offsetWidth;
      form?.classList.add('is-wrong');
      if (field) { field.value = ''; field.focus(); }
    }
    cells();
  }

  function bind(root) {
    if (root.dataset.phoneBound) return;
    root.dataset.phoneBound = '1';
    root.addEventListener('change', event => {
      if (!event.target.matches('[data-phone-action="toggle"]')) return;
      const on = event.target.checked;
      act('toggle', () => post('/api/phone/access', { on }));
    });
    root.addEventListener('input', event => {
      if (!event.target.matches('[data-phone-form] input')) return;
      const digits = event.target.value.replace(/\D/g, '').slice(0, 6);
      event.target.value = digits;
      cells();
      if (digits.length === 6) approve(event.target.form);
    });
    root.addEventListener('focusin', cells);
    root.addEventListener('focusout', () => setTimeout(cells));
    root.addEventListener('click', async event => {
      const copy = event.target.closest('[data-phone-copy]');
      if (copy) {
        navigator.clipboard?.writeText(copy.dataset.phoneCopy).then(() => {
          copy.classList.add('is-copied');
          setTimeout(() => copy.classList.remove('is-copied'), 1200);
        }, () => toast('Could not copy'));
        return;
      }
      const button = event.target.closest('button[data-phone-action]');
      if (!button || button.disabled) return;
      const id = button.dataset.phoneId;
      switch (button.dataset.phoneAction) {
        case 'open-sharing': return openSection('sharing');
        case 'tailnet-on': return act('tailnet', () => post('/api/remote/tailnet', { enabled: true }));
        case 'access-on': return act('toggle', () => post('/api/phone/access', { on: true }));
        case 'deny': return act('deny', () => post('/api/phone/deny', { id }));
        case 'revoke':
          if (!await confirm({ title: 'Revoke this phone?', message: 'It is signed out right away and has to be approved again.', confirmLabel: 'Revoke', danger: true })) return;
          return act('revoke', () => api(`/api/phone/devices/${encodeURIComponent(id)}`, { method: 'DELETE' }));
      }
    });
    root.addEventListener('submit', event => {
      if (!event.target.matches('[data-phone-form]')) return;
      event.preventDefault();
      approve(event.target);
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
    // Setup steps check themselves off, so poll while the section is open.
    timer = setInterval(() => {
      if (!view?.isConnected) { clearInterval(timer); timer = null; return; }
      if (view.closest('[hidden]') || document.hidden) return;
      load();
    }, 2500);
  }

  return { mount, html, load };
}
