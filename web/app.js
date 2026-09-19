const providersEl = document.getElementById('providers');
const addBtn = document.getElementById('add-account');
const themeButtons = document.querySelectorAll('[data-theme-choice]');

let data = { active: '', accounts: [] };

/* ---------- theme ---------- */

function applyTheme(theme) {
  document.documentElement.dataset.theme = theme;
  localStorage.setItem('switcher-theme', theme);
  themeButtons.forEach(b => b.classList.toggle('selected', b.dataset.themeChoice === theme));
}

themeButtons.forEach(btn =>
  btn.addEventListener('click', () => applyTheme(btn.dataset.themeChoice)));

// Keep "system" alive when the OS preference flips.
matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
  if (document.documentElement.dataset.theme === 'system') render();
});

applyTheme(localStorage.getItem('switcher-theme') || 'system');

/* ---------- provider marks ---------- */

// Official OpenAI logomark, inlined so it follows the theme color.
const OPENAI_PATH = 'm297.06 130.97c7.26-21.79 4.76-45.66-6.85-65.48-17.46-30.4-52.56-46.04-86.84-38.68-15.25-17.18-37.16-26.95-60.13-26.81-35.04-.08-66.13 22.48-76.91 55.82-22.51 4.61-41.94 18.7-53.31 38.67-17.59 30.32-13.58 68.54 9.92 94.54-7.26 21.79-4.76 45.66 6.85 65.48 17.46 30.4 52.56 46.04 86.84 38.68 15.24 17.18 37.16 26.95 60.13 26.8 35.06.09 66.16-22.49 76.94-55.86 22.51-4.61 41.94-18.7 53.31-38.67 17.57-30.32 13.55-68.51-9.94-94.51zm-120.28 168.11c-14.03.02-27.62-4.89-38.39-13.88.49-.26 1.34-.73 1.89-1.07l63.72-36.8c3.26-1.85 5.26-5.32 5.24-9.07v-89.83l26.93 15.55c.29.14.48.42.52.74v74.39c-.04 33.08-26.83 59.9-59.91 59.97zm-128.84-55.03c-7.03-12.14-9.56-26.37-7.15-40.18.47.28 1.3.79 1.89 1.13l63.72 36.8c3.23 1.89 7.23 1.89 10.47 0l77.79-44.92v31.1c.02.32-.13.63-.38.83l-64.41 37.19c-28.69 16.52-65.33 6.7-81.92-21.95zm-16.77-139.09c7-12.16 18.05-21.46 31.21-26.29 0 .55-.03 1.52-.03 2.2v73.61c-.02 3.74 1.98 7.21 5.23 9.06l77.79 44.91-26.93 15.55c-.27.18-.61.21-.91.08l-64.42-37.22c-28.63-16.58-38.45-53.21-21.95-81.89zm221.26 51.49-77.79-44.92 26.93-15.54c.27-.18.61-.21.91-.08l64.42 37.19c28.68 16.57 38.51 53.26 21.94 81.94-7.01 12.14-18.05 21.44-31.2 26.28v-75.81c.03-3.74-1.96-7.2-5.2-9.06zm26.8-40.34c-.47-.29-1.3-.79-1.89-1.13l-63.72-36.8c-3.23-1.89-7.23-1.89-10.47 0l-77.79 44.92v-31.1c-.02-.32.13-.63.38-.83l64.41-37.16c28.69-16.55 65.37-6.7 81.91 22 6.99 12.12 9.52 26.31 7.15 40.1zm-168.51 55.43-26.94-15.55c-.29-.14-.48-.42-.52-.74v-74.39c.02-33.12 26.89-59.96 60.01-59.94 14.01 0 27.57 4.92 38.34 13.88-.49.26-1.33.73-1.89 1.07l-63.72 36.8c-3.26 1.85-5.26 5.31-5.24 9.06l-.04 89.79zm14.63-31.54 34.65-20.01 34.65 20v40.01l-34.65 20-34.65-20z';

const LOGOS = {
  codex: `<svg viewBox="0 0 320 320" fill="currentColor" aria-hidden="true"><path d="${OPENAI_PATH}"/></svg>`,
  grok: `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" aria-hidden="true"><path d="M5 4l10 16"/><path d="M19 4c-2 3.5-5.5 3.5-7 6.5S9.5 17 5 20"/><circle cx="16.5" cy="6.5" r="2.6" fill="currentColor" stroke="none"/></svg>`,
  claude: `<svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M6.5 19.2L11.9 4h2.2l5.4 15.2h-2.8l-1.3-3.9h-5l-1.3 3.2H6.5zm3.9-5.6h3.4l-1.7-4.8-1.9 4.8z"/></svg>`,
};

const PROVIDER_NAMES = { codex: 'Codex', grok: 'Grok', claude: 'Claude' };

// ChatGPT plan tiers as OpenAI markets them.
const PLAN_NAMES = {
  pro: 'Pro 20x',
  prolite: 'Pro 5x',
  plus: 'Plus',
  free: 'Free',
};

/* ---------- helpers ---------- */

async function api(path, options) {
  const res = await fetch(path, options);
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(errorMessage(body, res.statusText));
  return body;
}

function errorMessage(body, fallback) {
  if (!body || body.error === undefined) return fallback || 'Request failed';
  if (typeof body.error === 'string') return body.error;
  return body.error.message || fallback || 'Request failed';
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, c => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[c]));
}

// fmtRemaining renders a countdown the way T3 does: "6d 23h", "3h 44m".
function fmtRemaining(untilUnix) {
  let ms = untilUnix * 1000 - Date.now();
  if (ms <= 0) return null; // window already rolled; upstream will refresh soon
  const m = Math.floor(ms / 60000);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ${m % 60}m`;
  const d = Math.floor(h / 24);
  return `${d}d ${h % 24}h`;
}

function toast(message) {
  const el = document.createElement('div');
  el.className = 'toast';
  el.textContent = message;
  document.body.appendChild(el);
  setTimeout(() => el.remove(), 4000);
}

/* ---------- rendering ---------- */

async function refreshState() {
  try {
    const next = await api('/api/state');
    if (JSON.stringify(next) !== JSON.stringify(data)) {
      data = next;
      render();
    }
  } catch (err) {
    toast('Could not reach Switcher: ' + err.message);
  }
}

function statusOf(account) {
  if (account.exhausted_until * 1000 > Date.now()) {
    return { cls: 'exhausted', label: 'Out of usage' };
  }
  if (account.id === data.active) return { cls: 'active', label: 'Active' };
  return { cls: '', label: 'Idle' };
}

function windowHTML(win, providerID) {
  const left = Math.max(0, Math.min(100, 100 - win.used_percent));
  const used = 100 - left;
  const name = PROVIDER_NAMES[providerID] || providerID;
  const remaining = fmtRemaining(win.resets_at);
  const resetText = remaining ? `↻ ${remaining}` : '↻ soon';
  const recovery = left < 100
    ? `<div class="recover">↻ +${used}% in ${remaining || 'a moment'}</div>`
    : '';
  const bar = left === 0
    // Fully hatched track, label sitting on it: nothing to fill yet.
    ? `<div class="bar"><span class="bar-label">${escapeHTML(`${name} 0%`)}</span></div>`
    : `<div class="bar"><div class="fill" style="width:${left}%">${left >= 14 ? `<span>${escapeHTML(`${name} ${left}%`)}</span>` : ''}</div></div>`;
  return `
    <div class="window">
      <div class="win-left">
        <div class="win-label">${escapeHTML(win.label)}</div>
        <div class="win-pct">${left}%<span>left</span></div>
        ${recovery}
      </div>
      <div class="win-bar">
        ${bar}
        <span class="reset-badge">${resetText}</span>
      </div>
    </div>`;
}

function accountHTML(account) {
  const isActive = account.id === data.active;
  const status = statusOf(account);
  const plan = PLAN_NAMES[account.plan] || account.plan || '';
  let windows;
  if (account.usage && account.usage.windows && account.usage.windows.length) {
    windows = `<div class="windows">${account.usage.windows.map(w => windowHTML(w, account.provider)).join('')}</div>`;
  } else if (account.usage) {
    windows = `<div class="unknown">Usage unavailable right now.</div>`;
  } else {
    windows = '';
  }
  return `
    <div class="account ${isActive ? 'active' : ''}" data-id="${escapeHTML(account.id)}">
      <div class="account-head">
        <div class="who">
          <div class="email" tabindex="0" title="">${escapeHTML(account.email)}</div>
          <div class="meta">
            <span class="dot ${status.cls}"></span>${status.label}
            ${plan ? ` · ${escapeHTML(plan)}` : ''}
          </div>
        </div>
        <div class="actions">
          <button class="use" data-act="activate" ${isActive ? 'disabled' : ''}>
            ${isActive ? 'Active' : 'Use this account'}
          </button>
          <button data-act="delete" title="Remove account">Remove</button>
        </div>
      </div>
      ${windows}
    </div>`;
}

function render() {
  const byProvider = new Map();
  for (const a of data.accounts) {
    if (!byProvider.has(a.provider)) byProvider.set(a.provider, []);
    byProvider.get(a.provider).push(a);
  }
  if (!byProvider.size) {
    providersEl.innerHTML = `
      <div class="empty">
        No accounts yet.<br>Add your first Codex account to start switching.
      </div>`;
    return;
  }
  let html = '';
  for (const [providerID, accounts] of byProvider) {
    html += `
      <section class="provider">
        <div class="provider-head">
          <span class="logo">${LOGOS[providerID] || ''}</span>
          <h2>${escapeHTML(PROVIDER_NAMES[providerID] || providerID)}</h2>
          <span class="count">${accounts.length}</span>
        </div>
        ${accounts.map(accountHTML).join('')}
      </section>`;
  }
  providersEl.innerHTML = html;
}

/* ---------- usage ---------- */

let usageTimer = null;
async function refreshAllUsage() {
  for (const account of data.accounts.filter(a => a.provider === 'codex')) {
    try { await api(`/api/accounts/${account.id}/refresh`, { method: 'POST' }); } catch { /* keep old data */ }
  }
  await refreshState();
}

function scheduleUsage() {
  clearTimeout(usageTimer);
  usageTimer = setTimeout(async () => {
    await refreshAllUsage();
    scheduleUsage();
  }, 60000);
}

/* ---------- actions ---------- */

providersEl.addEventListener('click', async (event) => {
  // Emails are blurred for shoulder-surfing privacy; clicking one toggles
  // a sticky reveal.
  const email = event.target.closest('.email');
  if (email) {
    email.classList.toggle('revealed');
    return;
  }
  const button = event.target.closest('button[data-act]');
  if (!button || button.disabled) return;
  const id = button.closest('.account').dataset.id;
  try {
    if (button.dataset.act === 'activate') {
      await api(`/api/accounts/${id}/activate`, { method: 'POST' });
      await refreshState();
    } else if (button.dataset.act === 'delete') {
      const mail = button.closest('.account').querySelector('.email').textContent;
      if (!confirm(`Remove ${mail}? You can always add it back.`)) return;
      await api(`/api/accounts/${id}`, { method: 'DELETE' });
      await refreshState();
    }
  } catch (err) {
    toast(err.message);
  }
});

addBtn.addEventListener('click', async () => {
  addBtn.disabled = true;
  try {
    const { url, state } = await api('/api/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ provider: 'codex' }),
    });
    const popup = window.open(url, 'switcher-login', 'width=520,height=720');
    if (!popup) {
      // Popup blocked: navigate this tab; the callback page says how to get back.
      location.href = url;
      return;
    }
    pollLogin(state); // detached: the button stays usable for another attempt
  } catch (err) {
    toast(err.message);
  } finally {
    addBtn.disabled = false;
  }
});

// Login polling runs detached so closing the popup never locks the button:
// the user can always click "Add Codex account" again immediately.
async function pollLogin(state) {
  const deadline = Date.now() + 5 * 60 * 1000;
  while (Date.now() < deadline) {
    try {
      const res = await api(`/api/login/${state}`);
      if (res.status === 'done') {
        toast(`Account added: ${res.account.email}`);
        await refreshState();
        await refreshAllUsage();
        return;
      }
      if (res.status === 'failed') {
        toast('Login failed: ' + (res.error || 'did not complete'));
        return;
      }
      if (res.status === 'finished') return;
    } catch {
      // Transient network hiccup while polling: keep trying.
    }
    await new Promise(resolve => setTimeout(resolve, 1000));
  }
  toast('Login timed out');
}

/* ---------- boot ---------- */

// Prefer the generated PNG logo when it exists (scripts/generate-logo.py);
// the inline vector badge remains the fallback.
fetch('/logo.png', { method: 'HEAD' }).then(res => {
  if (!res.ok) return;
  const mark = document.querySelector('.brand-mark');
  if (mark) mark.innerHTML = `<img src="logo.png" alt="">`;
});

refreshState().then(refreshAllUsage);
scheduleUsage();
setInterval(refreshState, 4000);
