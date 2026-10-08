// Switcher on your phone. Served only through the Tailscale add-on to the
// owner's devices, and only to phones approved on the Mac. The page can read
// accounts and usage, refresh them, switch accounts and spend banked resets.
import { switcherMark, playMark } from './brand.js';

const PROVIDERS = { codex: 'Codex', claude: 'Claude', grok: 'Grok', opencode: 'OpenCode', antigravity: 'Antigravity', gemini: 'Gemini', copilot: 'Copilot' };
const PLANS = {
  claude_max_5x: 'Max 5x', claude_max_20x: 'Max 20x', claude_max: 'Max', claude_pro: 'Pro', max: 'Max', pro: 'Pro 20x',
  prolite: 'Pro 5x', plus: 'Plus', free: 'Free', copilot_pro: 'Pro', copilot_pro_plus: 'Pro+', copilot_business: 'Business',
  copilot_enterprise: 'Enterprise', copilot_free: 'Free',
};
const OUTCOMES = {
  reset: 'Banked reset used', already_redeemed: 'That reset was already spent',
  nothing_to_reset: 'Nothing to reset right now; the reset was kept', no_credit: 'No banked reset available',
};

export const esc = value => String(value ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]);

export function duration(seconds) {
  if (!(seconds > 0)) return '';
  const m = Math.floor(seconds / 60);
  if (m < 60) return `${Math.max(m, 1)}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ${m % 60}m`;
  return `${Math.floor(h / 24)}d ${h % 24}h`;
}

// windowHTML draws one usage window as what is left.
export function windowHTML(w, now = Date.now() / 1000) {
  const used = Math.min(100, Math.max(0, Number(w.used_percent) || 0));
  const left = Math.round(100 - used);
  const resets = w.resets_at > now ? `resets in ${duration(w.resets_at - now)}` : '';
  const level = left <= 10 ? 'is-low' : left <= 30 ? 'is-mid' : '';
  return `<div class="window ${level}">
    <div class="window-line"><span>${esc(w.label)}</span><strong>${left}% left</strong></div>
    <div class="bar" role="img" aria-label="${esc(w.label)}: ${left}% left"><i style="width:${left}%"></i></div>
    ${resets ? `<div class="window-reset">${resets}</div>` : ''}
  </div>`;
}

// accountHTML draws one account with what can be done from the phone.
export function accountHTML(a, now = Date.now() / 1000) {
  const windows = a.usage?.windows || [];
  const credits = a.reset_credits?.count || 0;
  const out = a.exhausted_until > now;
  const plan = PLANS[a.plan] || '';
  const actions = [];
  if (!a.active) {
    actions.push(`<button type="button" class="button" data-use="${esc(a.id)}">Use</button>`);
  }
  if (credits > 0 && a.reset_credits?.next_id) {
    actions.push(`<button type="button" class="button ${out ? 'primary' : ''}" data-reset="${esc(a.id)}">Use banked reset <span class="count">${credits}</span></button>`);
  }
  return `<article class="account ${a.active ? 'is-active' : ''} ${out ? 'is-out' : ''}" data-account="${esc(a.id)}">
    <div class="account-head">
      <span class="dot" aria-hidden="true"></span>
      <span class="email">${esc(a.email || a.id)}</span>
      ${plan ? `<span class="plan">${esc(plan)}</span>` : ''}
      ${a.active ? '<span class="pill">In use</span>' : ''}
    </div>
    ${out ? `<p class="out">Out of usage · back in ${duration(a.exhausted_until - now)}</p>` : ''}
    ${windows.length ? `<div class="windows">${windows.map(w => windowHTML(w, now)).join('')}</div>` : '<p class="muted">No usage reported yet</p>'}
    ${a.native_switch_available && !a.active ? '<p class="muted small">Use also switches Claude Code on your Mac.</p>' : ''}
    ${actions.length ? `<div class="actions">${actions.join('')}</div>` : ''}
  </article>`;
}

// stateHTML groups accounts by provider in the dashboard's order.
export function stateHTML(state, now = Date.now() / 1000) {
  const hidden = new Set(state.hidden || []);
  const order = [...(state.order || [])];
  for (const a of state.accounts || []) if (!order.includes(a.provider)) order.push(a.provider);
  const groups = order.filter(p => !hidden.has(p)).map(p => {
    const accounts = (state.accounts || []).filter(a => a.provider === p)
      .sort((x, y) => Number(y.active) - Number(x.active));
    if (!accounts.length) return '';
    return `<section class="provider"><h2>${esc(PROVIDERS[p] || p)}</h2>${accounts.map(a => accountHTML(a, now)).join('')}</section>`;
  }).join('');
  return groups || '<div class="empty"><p>No accounts yet. Add them in Switcher on your Mac.</p></div>';
}

export function footerHTML(device) {
  return `<footer class="foot">
    <span>${device ? `This phone: ${esc(device)}` : 'This phone'} · only your Tailscale devices reach this page</span>
    <button type="button" class="link" id="sign-out">Sign this phone out</button>
  </footer>`;
}

export function pairHTML(pairing, now = Date.now() / 1000) {
  if (!pairing) {
    return `<section class="center">
      <div class="hero-mark">${switcherMark({ tile: true, size: 72, id: 'pair-mark' })}</div>
      <h1>Use Switcher on this phone</h1>
      <p>Your Mac approves each phone once. This phone will show a code; you type it into Switcher on your Mac.</p>
      <button type="button" class="button primary wide" id="pair-start">Pair this phone</button>
    </section>`;
  }
  const code = String(pairing.code || '');
  const left = Math.max(0, Math.round(pairing.expires_at - now));
  return `<section class="center">
    <div class="hero-mark">${switcherMark({ tile: true, size: 72, id: 'pair-mark' })}</div>
    <h1>Approve this phone</h1>
    <p>On your Mac, open Switcher, go to <strong>Settings → Phone</strong> and enter this code.</p>
    <div class="code" aria-label="Code ${esc(code.split('').join(' '))}">${esc(code.slice(0, 3))}<span></span>${esc(code.slice(3))}</div>
    <p class="muted small">Expires in ${Math.floor(left / 60)}:${String(left % 60).padStart(2, '0')}. Or in a terminal on the Mac:<br><code>switcher phone approve ${esc(code)}</code></p>
    <div class="waiting"><i></i><i></i><i></i><span>Waiting for your Mac</span></div>
  </section>`;
}

export function messageHTML(title, text) {
  return `<section class="center">
    <div class="hero-mark">${switcherMark({ tile: true, size: 72, id: 'msg-mark' })}</div>
    <h1>${esc(title)}</h1><p>${esc(text)}</p>
    <button type="button" class="button wide" id="retry">Try again</button>
  </section>`;
}

/* ---------- the page ---------- */

const app = { csrf: '', state: null, pairing: null, timer: 0, mac: '', device: '', busy: false };

class HttpError extends Error {
  constructor(status, body) { super(body?.error || `HTTP ${status}`); this.status = status; this.body = body; }
}

async function api(path, { method = 'GET', body } = {}) {
  const headers = {};
  if (method !== 'GET') {
    headers['X-Switcher-CSRF'] = app.csrf;
    headers['Content-Type'] = 'application/json';
  }
  const response = await fetch(path, { method, headers, body: body ? JSON.stringify(body) : undefined, credentials: 'same-origin', cache: 'no-store' });
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new HttpError(response.status, data);
  return data;
}

const $ = id => document.getElementById(id);

function show(html) {
  $('view').innerHTML = html;
}

function toast(text) {
  const el = $('toast');
  el.textContent = text;
  el.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { el.hidden = true; }, 3200);
}

function confirmSheet(title, text, action) {
  return new Promise(resolve => {
    $('sheet-title').textContent = title;
    $('sheet-text').textContent = text;
    $('sheet-confirm').textContent = action;
    $('sheet').hidden = false;
    const done = value => {
      $('sheet').hidden = true;
      $('sheet-confirm').onclick = $('sheet-cancel').onclick = $('sheet').onclick = null;
      resolve(value);
    };
    $('sheet-confirm').onclick = () => done(true);
    $('sheet-cancel').onclick = () => done(false);
    $('sheet').onclick = event => { if (event.target === $('sheet')) done(false); };
  });
}

function schedule(fn, ms) {
  clearTimeout(app.timer);
  app.timer = setTimeout(fn, ms);
}

function handleError(error) {
  if (error.status === 401) {
    app.state = null;
    $('refresh').hidden = true;
    show(pairHTML(null));
    return;
  }
  if (error.status === 503 && error.body?.enabled === false) {
    $('refresh').hidden = true;
    show(messageHTML('Phone access is off', 'Turn it on in Switcher on your Mac, under Settings → Phone.'));
    return;
  }
  if (app.state) {
    toast(error.message || 'Switcher did not answer');
    return;
  }
  show(messageHTML('Switcher did not answer', error.message || 'Check that your Mac is awake and Tailscale is connected.'));
}

async function loadState() {
  try {
    app.state = await api('/api/state');
    $('refresh').hidden = false;
    if (!app.busy) show(stateHTML(app.state) + footerHTML(app.device));
    schedule(loadState, document.visibilityState === 'visible' ? 15000 : 60000);
  } catch (error) {
    handleError(error);
  }
}

async function poll() {
  try {
    const answer = await api('/api/pair');
    if (answer.status === 'approved') {
      app.csrf = answer.csrf;
      app.pairing = null;
      playMark($('top-mark'));
      toast('This phone is approved');
      await loadState();
      return;
    }
    if (answer.status === 'expired') {
      app.pairing = null;
      show(pairHTML(null));
      toast('The code expired; ask for a new one');
      return;
    }
    app.pairing = answer;
    show(pairHTML(answer));
    schedule(poll, 2000);
  } catch (error) {
    handleError(error);
  }
}

async function startPairing() {
  try {
    app.pairing = await api('/api/pair', { method: 'POST' });
    show(pairHTML(app.pairing));
    schedule(poll, 2000);
  } catch (error) {
    if (error.status === 429) toast(error.message);
    else handleError(error);
  }
}

async function boot() {
  $('top-mark').innerHTML = switcherMark({ tile: true, size: 30, id: 'top-mark-svg' });
  try {
    const session = await api('/api/session');
    app.mac = session.mac || '';
    app.device = session.device || '';
    $('top-mac').textContent = app.mac ? `on ${app.mac}` : '';
    if (session.paired) {
      app.csrf = session.csrf;
      await loadState();
    } else {
      show(pairHTML(null));
    }
  } catch (error) {
    handleError(error);
  }
}

async function act(button, run) {
  if (app.busy) return;
  app.busy = true;
  button.disabled = true;
  try {
    await run();
  } catch (error) {
    if (error.status === 401 || error.status === 503) handleError(error);
    else toast(error.message || 'That did not work');
  } finally {
    app.busy = false;
    await loadState();
  }
}

function account(id) {
  return app.state?.accounts?.find(a => a.id === id);
}

if (typeof document !== 'undefined') {
  document.addEventListener('click', event => {
    const target = event.target.closest('button');
    if (!target) return;
    if (target.id === 'pair-start') startPairing();
    else if (target.id === 'retry') boot();
    else if (target.id === 'sign-out') {
      confirmSheet('Sign this phone out?', 'To use Switcher here again, you approve this phone on your Mac once more.', 'Sign out').then(async ok => {
        if (!ok) return;
        try {
          await api('/api/logout', { method: 'POST' });
        } catch { /* signed out either way below */ }
        clearTimeout(app.timer);
        app.state = null;
        $('refresh').hidden = true;
        show(pairHTML(null));
      });
    }
    else if (target.id === 'refresh') {
      act(target, async () => {
        await api('/api/usage/refresh', { method: 'POST' });
        toast('Refreshing usage');
      }).finally(() => { target.disabled = false; });
    } else if (target.dataset.use) {
      const a = account(target.dataset.use);
      act(target, async () => {
        const answer = await api(`/api/accounts/${encodeURIComponent(target.dataset.use)}/use`, { method: 'POST' });
        playMark($('top-mark'));
        toast(answer.claude_code_switched ? `Claude and Claude Code now use ${a?.email || 'that account'}`
          : `${PROVIDERS[a?.provider] || 'Switcher'} now uses ${a?.email || 'that account'}`);
      });
    } else if (target.dataset.reset) {
      const a = account(target.dataset.reset);
      const credits = a?.reset_credits?.count || 0;
      confirmSheet('Use a banked reset?', `${a?.email || 'This account'} gets its usage back right away. You have ${credits} banked reset${credits === 1 ? '' : 's'}; a used reset cannot be given back.`, 'Use reset')
        .then(ok => {
          if (!ok) return;
          act(target, async () => {
            const answer = await api(`/api/accounts/${encodeURIComponent(a.id)}/reset`, { method: 'POST', body: { credit_id: a.reset_credits.next_id } });
            if (answer.outcome === 'reset') playMark($('top-mark'));
            toast(OUTCOMES[answer.outcome] || `The provider answered ${answer.outcome}`);
          });
        });
    }
  });
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible' && app.state) loadState();
  });
  boot();
}
