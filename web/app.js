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

/* ---------- provider marks (simplified official logos) ---------- */

const LOGOS = {
  codex: `<svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><g>
    <g id="petal"><path d="M12 3.4c1.7-1 3.9-.4 4.9 1.3l.2.4h.5c2 0 3.6 1.6 3.6 3.6 0 .6-.1 1.1-.4 1.6l-.2.4.2.4c.4.5.6 1.2.6 1.9 0 1.7-1.2 3.2-2.9 3.5l-.4.1-.2.4c-1 1.7-3.2 2.3-4.9 2.4v-2c1.3 0 2.5-.2 3.1-1.2l.3-.6-.5-.3a4.6 4.6 0 0 1-2-3.6v-1h3.3c.5 0 .9-.4.9-.9 0-1.3-1-2.3-2.3-2.3l-.7.1-.3-.6a2.6 2.6 0 0 0-3.5-1V3.4z"/><use href="#petal" transform="rotate(60 12 12)"/><use href="#petal" transform="rotate(120 12 12)"/><use href="#petal" transform="rotate(180 12 12)"/><use href="#petal" transform="rotate(240 12 12)"/><use href="#petal" transform="rotate(300 12 12)"/></g></g></svg>`,
  grok: `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" aria-hidden="true"><path d="M5 4l10 16M19 4c-2 3.5-5.5 3.5-7 6.5S9.5 17 5 20"/><circle cx="16.5" cy="6.5" r="2.6" fill="currentColor" stroke="none"/></svg>`,
  claude: `<svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M12.6 4.8l4.8 14.4h-2.9l-1-3.2H7.9l-1 3.2H4L8.6 4.8c.2-.5.7-.8 1.2-.8h1.3c.5 0 1 .3 1.2.8l-.2-.8zM9 13.4h5.4L11.8 6.7 9 13.4zM20 4.8h1.6l.6 1.8-1.4 1z" opacity="0"/><path d="M6.5 19.2L11.9 4h2.2l5.4 15.2h-2.8l-1.3-3.9h-5l-1.3 3.2H6.5zm3.9-5.6h3.4l-1.7-4.8-1.9 4.8z"/></svg>`,
};

const PROVIDER_NAMES = { codex: 'Codex', grok: 'Grok', claude: 'Claude' };

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
  if (ms < 0) ms = 0;
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
  const remaining = fmtRemaining(win.resets_at);
  return `
    <div class="window">
      <div class="win-left">
        <div class="win-label">${escapeHTML(win.label)}</div>
        <div class="win-pct">${left}%<span>left</span></div>
        ${left < 100 ? `<div class="recover">↻ +${left}% in ${remaining}</div>` : ''}
      </div>
      <div class="win-bar">
        <div class="bar">
          <div class="fill" style="width:${left}%">${escapeHTML(PROVIDER_NAMES[providerID] || providerID)} ${left}%</div>
        </div>
        <span class="reset-badge">↻ ${remaining}</span>
      </div>
    </div>`;
}

function accountHTML(account) {
  const isActive = account.id === data.active;
  const status = statusOf(account);
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
          <div class="email">${escapeHTML(account.email)}</div>
          <div class="meta">
            <span class="dot ${status.cls}"></span>${status.label}
            ${account.plan ? ` · ${escapeHTML(account.plan)}` : ''}
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
  const button = event.target.closest('button[data-act]');
  if (!button || button.disabled) return;
  const id = button.closest('.account').dataset.id;
  try {
    if (button.dataset.act === 'activate') {
      await api(`/api/accounts/${id}/activate`, { method: 'POST' });
      await refreshState();
    } else if (button.dataset.act === 'delete') {
      const email = button.closest('.account').querySelector('.email').textContent;
      if (!confirm(`Remove ${email}? You can always add it back.`)) return;
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

refreshState().then(refreshAllUsage);
scheduleUsage();
setInterval(refreshState, 4000);
