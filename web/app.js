const providersEl = document.getElementById('providers');
const themeButtons = document.querySelectorAll('[data-theme-choice]');

let data = { accounts: [], order: [], hidden: [] };

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
  // Official OpenAI circular mark; black disc + white knot reads on both
  // themes.
  codex: `<svg viewBox="0 0 512 512" aria-hidden="true"><path d="M256 0c141.4 0 256 114.6 256 256S397.4 512 256 512 0 397.4 0 256 114.6 0 256 0"/><path style="fill:#fff" d="M212.8 209.5v-35.7c0-2.4.7-4.1 3.1-5.5l66.2-38.4c8.9-5.1 20.3-7.6 31.2-7.6 41.9 0 68.3 32.3 68.3 66.9 0 2.7 0 6.5-.7 9.6l-69.3-40.5c-3.4-2.1-7.2-2.4-11.3 0zm153.1 126.7v-79.3c0-4.5-1.7-7.6-5.8-10l-87.9-51.1 30.9-17.8c1.7-1 4.5-1 6.2 0l66.6 38.4c18.9 11 31.9 35 31.9 58-.1 27.1-16.5 52.1-41.9 61.8m-171.3-68.4L164 249.7c-2.4-1.4-3.1-3.1-3.1-5.5v-76.5c0-37.4 28.5-65.6 67.3-65.6 15.1 0 29.5 5.1 41.2 14.4l-69 40.2c-4.1 2.4-5.8 5.5-5.8 10zm59.7 34.4-41.5-23.3v-49.4l41.5-23.3 41.2 23.3v49.4zm25.7 104c-15.1 0-29.5-5.1-41.2-14.4l69-40.2c4.1-2.4 5.8-5.5 5.8-10V240.4l30.9 18.2c2.4 1.4 3.1 3.1 3.1 5.5v76.5c.1 37.4-28.8 65.6-67.6 65.6m-81-75.9-66.6-38.4c-18.9-11-31.9-35-31.9-58 0-27.5 16.8-52.2 42.2-61.8v79.6c0 4.5 1.7 7.6 5.8 10l87.5 50.8-30.9 17.8c-1.6 1-4.4 1-6.1 0m-4.1 55.6c-39.5 0-68.3-29.5-68.3-66.2 0-3.4.3-6.9.7-10l69 39.8c4.1 2.4 7.6 2.4 11.7 0l87.5-50.8v35.7c0 2.4-.7 4.1-3.1 5.5l-66.2 38.4c-9 5.2-20.3 7.6-31.3 7.6m85.1 38.8c41.2 0 75.5-29.5 83.1-68.7 38.4-9.6 63.2-45.3 63.2-81.7 0-24-10.3-47-29.2-63.8 1.7-7.2 3.1-14.8 3.1-22 0-48.4-39.5-84.8-84.8-84.8-9.3 0-18.5 1.7-27.5 4.8-15.4-15.4-36.4-25.1-59.7-25.1-41.2 0-75.5 29.5-83.1 68.7-38.4 9.6-63.2 45.3-63.2 81.7 0 24 10.3 47 29.2 63.8-1.7 7.2-3.1 14.8-3.1 22 0 48.4 39.5 84.8 84.8 84.8 9.3 0 18.5-1.7 27.5-4.8 15.5 15.5 36.4 25.1 59.7 25.1"/></svg>`,
  // Official Anthropic starburst in its brand clay color; reads on both
  // themes.
  claude: `<svg viewBox="0 0 248 248" aria-hidden="true"><path d="M52.4285 162.873L98.7844 136.879L99.5485 134.602L98.7844 133.334H96.4921L88.7237 132.862L62.2346 132.153L39.3113 131.207L17.0249 130.026L11.4214 128.844L6.2 121.873L6.7094 118.447L11.4214 115.257L18.171 115.847L33.0711 116.911L55.485 118.447L71.6586 119.392L95.728 121.873H99.5485L100.058 120.337L98.7844 119.392L97.7656 118.447L74.5877 102.732L49.4995 86.1905L36.3823 76.62L29.3779 71.7757L25.8121 67.2858L24.2839 57.3608L30.6515 50.2716L39.3113 50.8623L41.4763 51.4531L50.2636 58.1879L68.9842 72.7209L93.4357 90.6804L97.0015 93.6343L98.4374 92.6652L98.6571 91.9801L97.0015 89.2625L83.757 65.2772L69.621 40.8192L63.2534 30.6579L61.5978 24.632C60.9565 22.1032 60.579 20.0111 60.579 17.4246L67.8381 7.49965L71.9133 6.19995L81.7193 7.49965L85.7946 11.0443L91.9074 24.9865L101.714 46.8451L116.996 76.62L121.453 85.4816L123.873 93.6343L124.764 96.1155H126.292V94.6976L127.566 77.9197L129.858 57.3608L132.15 30.8942L132.915 23.4505L136.608 14.4708L143.994 9.62643L149.725 12.344L154.437 19.0788L153.8 23.4505L150.998 41.6463L145.522 70.1215L141.957 89.2625H143.994L146.414 86.7813L156.093 74.0206L172.266 53.698L179.398 45.6635L187.803 36.802L193.152 32.5484H203.34L210.726 43.6549L207.415 55.1159L196.972 68.3492L188.312 79.5739L175.896 96.2095L168.191 109.585L168.882 110.689L170.738 110.53L198.755 104.504L213.91 101.787L231.994 98.7149L240.144 102.496L241.036 106.395L237.852 114.311L218.495 119.037L195.826 123.645L162.07 131.592L161.696 131.893L162.137 132.547L177.36 133.925L183.855 134.279H199.774L229.447 136.524L237.215 141.605L241.8 147.867L241.036 152.711L229.065 158.737L213.019 154.956L175.45 145.977L162.587 142.787H160.805V143.85L171.502 154.366L191.242 172.089L215.82 195.011L217.094 200.682L213.91 205.172L210.599 204.699L188.949 188.394L180.544 181.069L161.696 165.118H160.422V166.772L164.752 173.152L187.803 207.771L188.949 218.405L187.294 221.832L181.308 223.959L174.813 222.777L161.187 203.754L147.305 182.486L136.098 163.345L134.745 164.2L128.075 235.42L125.019 239.082L117.887 241.8L111.902 237.31L108.718 229.984L111.902 215.452L115.722 196.547L118.779 181.541L121.58 162.873L123.291 156.636L123.14 156.219L121.773 156.449L107.699 175.752L86.304 204.699L69.3663 222.777L65.291 224.431L58.2867 220.768L58.9235 214.27L62.8713 208.48L86.304 178.705L100.44 160.155L109.551 149.507L109.462 147.967L108.959 147.924L46.6977 188.512L35.6182 189.93L30.7788 185.44L31.4156 178.115L33.7079 175.752L52.4285 162.873Z" fill="#D97757"/></svg>`,
  // Official xAI mark, currentColor so it is black on light and white on
  // dark (same geometry as the supplied light and dark files).
  grok: `<svg viewBox="0.36 0.5 33.33 32" fill="currentColor" aria-hidden="true"><path d="M13.2371 21.0407L24.3186 12.8506C24.8619 12.4491 25.6384 12.6057 25.8973 13.2294C27.2597 16.5185 26.651 20.4712 23.9403 23.1851C21.2297 25.8989 17.4581 26.4941 14.0108 25.1386L10.2449 26.8843C15.6463 30.5806 22.2053 29.6665 26.304 25.5601C29.5551 22.3051 30.562 17.8683 29.6205 13.8673L29.629 13.8758C28.2637 7.99809 29.9647 5.64871 33.449 0.844576C33.5314 0.730667 33.6139 0.616757 33.6964 0.5L29.1113 5.09055V5.07631L13.2343 21.0436"/><path d="M10.9503 23.0313C7.07343 19.3235 7.74185 13.5853 11.0498 10.2763C13.4959 7.82722 17.5036 6.82767 21.0021 8.2971L24.7595 6.55998C24.0826 6.07017 23.215 5.54334 22.2195 5.17313C17.7198 3.31926 12.3326 4.24192 8.67479 7.90126C5.15635 11.4239 4.0499 16.8403 5.94992 21.4622C7.36924 24.9165 5.04257 27.3598 2.69884 29.826C1.86829 30.7002 1.0349 31.5745 0.36364 32.5L10.9474 23.0341"/></svg>`,
  // OpenCode mark; the dark frame is inverted in dark theme so it stays
  // visible.
  opencode: `<svg viewBox="0 0 240 300" aria-hidden="true"><path d="M180 240H60V120H180V240Z" fill="#CFCECD"/><path d="M180 60H60V240H180V60ZM240 300H0V0H240V300Z" fill="#211E1E"/></svg>`,
};

const PROVIDER_NAMES = { codex: 'Codex', claude: 'Claude', grok: 'Grok', opencode: 'OpenCode' };

// ChatGPT plan tiers as OpenAI markets them.
const PLAN_NAMES = {
  pro: 'Pro 20x',
  prolite: 'Pro 5x',
  plus: 'Plus',
  free: 'Free',
};

// How each provider adds an account: browser popup, device code, or key.
const ADD_METHOD = { codex: 'browser', claude: 'browser', grok: 'device', opencode: 'key' };

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
      renderAddProviderMenu();
    }
  } catch (err) {
    toast('Could not reach Switcher: ' + err.message);
  }
}

function statusOf(account) {
  if (account.exhausted_until * 1000 > Date.now()) {
    return { cls: 'exhausted', label: 'Out of usage' };
  }
  if (account.id === data.active?.[account.provider]) return { cls: 'active', label: 'Active' };
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
  const isActive = account.id === data.active?.[account.provider];
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
          <div class="email" tabindex="0">${escapeHTML(account.email)}</div>
          <div class="meta">
            <span class="dot ${status.cls}"></span>${status.label}
            ${plan ? ` · ${escapeHTML(plan)}` : ''}
          </div>
        </div>
        <div class="actions">
          ${account.reset_credits ? `<span class="reset-badge banked" title="Banked usage-limit resets available">⚡ ${account.reset_credits.count} banked</span>` : ''}
          <button class="use" data-act="activate" ${isActive ? 'disabled' : ''}>
            ${isActive ? 'Active' : 'Use this account'}
          </button>
          ${account.reset_credits ? `<button data-act="use-reset" title="Spend one banked reset: clears this account's out-of-usage state">Use reset</button>` : ''}
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
  const order = (data.order || Object.keys(PROVIDER_NAMES))
    .filter(id => !(data.hidden || []).includes(id));
  let html = '';
  order.forEach((providerID) => {
    const accounts = byProvider.get(providerID) || [];
    html += `
      <section class="provider" data-provider="${providerID}" id="provider-${providerID}">
        <div class="provider-head" draggable="true">
          <span class="drag-grip" title="Drag to reorder">
            <svg viewBox="0 0 12 18" aria-hidden="true"><circle cx="4" cy="3" r="1.5"/><circle cx="10" cy="3" r="1.5"/><circle cx="4" cy="9" r="1.5"/><circle cx="10" cy="9" r="1.5"/><circle cx="4" cy="15" r="1.5"/><circle cx="10" cy="15" r="1.5"/></svg>
          </span>
          <span class="logo logo-${providerID}">${LOGOS[providerID] || ''}</span>
          <h2>${escapeHTML(PROVIDER_NAMES[providerID] || providerID)}</h2>
          <span class="count">${accounts.length}</span>
          <button class="add-provider" data-add="${providerID}">Add account</button>
          <span class="provider-tools">
            <button data-menu="${providerID}" title="Provider options">⋯</button>
          </span>
        </div>
        ${accounts.length ? accounts.map(accountHTML).join('') : `<div class="unknown">No accounts yet.</div>`}
      </section>`;
  });
  providersEl.innerHTML = html;
}

/* ---------- usage ---------- */

let usageTimer = null;
async function refreshAllUsage() {
  for (const account of data.accounts) {
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

/* ---------- provider menus ---------- */

// openProviderMenu shows a small dropdown with per-provider actions.
function openProviderMenu(anchor, providerID) {
  document.querySelector('.prov-menu')?.remove();
  const name = PROVIDER_NAMES[providerID] || providerID;
  const menu = document.createElement('div');
  menu.className = 'prov-menu';
  menu.innerHTML = `
    <button data-remove-provider="${escapeHTML(providerID)}">
      <span class="menu-logo">${LOGOS[providerID] || ''}</span>Remove ${escapeHTML(name)}
    </button>`;
  const rect = anchor.getBoundingClientRect();
  menu.style.top = `${rect.bottom + 6}px`;
  menu.style.left = `${Math.max(8, Math.min(rect.left, window.innerWidth - 240))}px`;
  document.body.appendChild(menu);

  const close = (e) => {
    if (menu.contains(e.target) || e.target === anchor) return;
    menu.remove();
    document.removeEventListener('click', close);
  };
  setTimeout(() => document.addEventListener('click', close), 0);

  menu.addEventListener('click', async (e) => {
    const btn = e.target.closest('[data-remove-provider]');
    if (!btn) return;
    menu.remove();
    document.removeEventListener('click', close);
    const yes = await confirmDialog({
      title: `Remove ${name}?`,
      message: 'Its accounts stay on disk and keep serving traffic. "Add provider" brings it back.',
      confirmLabel: 'Remove',
      danger: true,
      logoHTML: LOGOS[providerID] || '',
    });
    if (!yes) return;
    await api(`/api/providers/${providerID}/hide`, { method: 'POST' });
    await refreshState();
  });
}

// "Add provider" button in the header: lists removed providers.
function renderAddProviderMenu() {
  const host = document.getElementById('add-provider-slot');
  if (!host) return;
  if (typeof data.hidden !== 'object' || !Array.isArray(data.hidden)) data.hidden = [];
  host.innerHTML = `
    <button id="add-provider">Add provider
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 9l6 6 6-6"/></svg>
    </button>
    <div class="hidden-list">
      ${data.hidden.length
        ? data.hidden.map(id => `
            <button data-show-provider="${escapeHTML(id)}">
              <span class="menu-logo">${LOGOS[id] || ''}</span>${escapeHTML(PROVIDER_NAMES[id] || id)}
            </button>`).join('')
        : `<button disabled>All providers are on the page</button>`}
    </div>`;
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
  const menuBtn = event.target.closest('button[data-menu]');
  if (menuBtn) {
    openProviderMenu(menuBtn, menuBtn.dataset.menu);
    return;
  }
  const button = event.target.closest('button[data-act]');
  if (!button || button.disabled) return;
  const id = button.closest('.account').dataset.id;
  try {
    if (button.dataset.act === 'activate') {
      await api(`/api/accounts/${id}/activate`, { method: 'POST' });
      await refreshState();
    } else if (button.dataset.act === 'use-reset') {
      button.disabled = true;
      const res = await api(`/api/accounts/${id}/use-reset`, { method: 'POST' });
      const outcomes = {
        reset: 'Banked reset used; this account is back in rotation',
        already_redeemed: 'That reset was already spent',
        nothing_to_reset: 'Nothing to reset right now',
        no_credit: 'No banked reset available',
      };
      toast(outcomes[res.outcome] || 'Done');
      await refreshState();
    } else if (button.dataset.act === 'delete') {
      const mail = button.closest('.account').querySelector('.email').textContent;
      const yes = await confirmDialog({
        title: 'Remove account?',
        message: `${mail} will be removed. You can always add it back.`,
        confirmLabel: 'Remove',
        danger: true,
      });
      if (!yes) return;
      await api(`/api/accounts/${id}`, { method: 'DELETE' });
      await refreshState();
    }
  } catch (err) {
    toast(err.message);
  }
});

/* ---------- drag-and-drop provider ordering ---------- */

// The section header is the drag handle: grab it anywhere and slide the
// section up or down; the order is persisted when the drag ends.
providersEl.addEventListener('dragstart', (event) => {
  const head = event.target.closest('.provider-head');
  if (!head) { event.preventDefault(); return; }
  const section = head.closest('.provider');
  event.dataTransfer.effectAllowed = 'move';
  event.dataTransfer.setData('text/plain', section.dataset.provider);
  section.classList.add('dragging');
});

providersEl.addEventListener('dragend', async () => {
  const dragged = providersEl.querySelector('.provider.dragging');
  if (!dragged) return;
  dragged.classList.remove('dragging');
  const order = [...providersEl.querySelectorAll('.provider')]
    .map((el) => el.dataset.provider);
  await api('/api/providers/order', {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ order }),
  });
  await refreshState();
});

providersEl.addEventListener('dragover', (event) => {
  event.preventDefault();
  event.dataTransfer.dropEffect = 'move';
  const dragged = providersEl.querySelector('.provider.dragging');
  if (!dragged) return;
  const target = event.target.closest('.provider');
  if (!target || target === dragged) return;
  const box = target.getBoundingClientRect();
  const before = event.clientY < box.top + box.height / 2;
  target.parentNode.insertBefore(dragged, before ? target : target.nextSibling);
});

providersEl.addEventListener('click', async (event) => {
  const add = event.target.closest('button[data-add]');
  if (!add || add.disabled) return;
  add.disabled = true;
  const providerID = add.dataset.add;
  try {
    if (ADD_METHOD[providerID] === 'key') {
      const key = await promptKey(PROVIDER_NAMES[providerID]);
      if (!key) { add.disabled = false; return; }
      const res = await api('/api/accounts', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ provider: providerID, key }),
      });
      toast(`Account added: ${res.account.email}`);
      await refreshState();
      await refreshAllUsage();
    } else {
      const login = await api('/api/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ provider: providerID }),
      });
      if (login.kind === 'device') {
        showDeviceModal(login.verification_url, login.user_code);
      } else {
        const w = 520, h = 720;
        const left = Math.max(0, Math.round((screen.width - w) / 2));
        const top = Math.max(0, Math.round((screen.height - h) / 2));
        const popup = window.open(login.url, 'switcher-login', `width=${w},height=${h},left=${left},top=${top}`);
        if (!popup) {
          // Popup blocked: navigate this tab; the callback page says how to get back.
          location.href = login.url;
          return;
        }
      }
      pollLogin(login.state); // detached: the button stays usable for another attempt
    }
  } catch (err) {
    toast(err.message);
  } finally {
    add.disabled = false;
  }
});

// Header: add back a removed provider, with its logo.
document.addEventListener('click', (event) => {
  const trigger = event.target.closest('#add-provider');
  if (trigger) {
    document.querySelector('#add-provider-slot .hidden-list')?.classList.toggle('open');
    return;
  }
  const show = event.target.closest('[data-show-provider]');
  if (show) {
    api(`/api/providers/${show.dataset.showProvider}/show`, { method: 'POST' })
      .then(async () => {
        await refreshState();
        document.getElementById(`provider-${show.dataset.showProvider}`)
          ?.scrollIntoView({ behavior: 'smooth', block: 'start' });
      });
    return;
  }
  document.querySelector('#add-provider-slot .hidden-list')?.classList.remove('open');
});

// Login polling runs detached so closing the popup never locks the button:
// the user can always click "Add account" again immediately.
async function pollLogin(state) {
  const deadline = Date.now() + 5 * 60 * 1000;
  while (Date.now() < deadline) {
    try {
      const res = await api(`/api/login/${state}`);
      if (res.status === 'done') {
        document.querySelector('.device-overlay')?.remove();
        toast(`Account added: ${res.account.email}`);
        await refreshState();
        await refreshAllUsage();
        return;
      }
      if (res.status === 'failed') {
        document.querySelector('.device-overlay')?.remove();
        toast('Login failed: ' + (res.error || 'did not complete'));
        return;
      }
      if (res.status === 'finished') return;
    } catch {
      // Transient network hiccup while polling: keep trying.
    }
    await new Promise(resolve => setTimeout(resolve, 1000));
  }
  document.querySelector('.device-overlay')?.remove();
  toast('Login timed out');
}

// showDeviceModal presents the grok device code and a link to the
// verification page; keep it on screen until the login resolves.
function showDeviceModal(verifyURL, userCode) {
  const overlay = document.createElement('div');
  overlay.className = 'device-overlay';
  overlay.innerHTML = `
    <div class="device-modal" role="dialog" aria-modal="true" aria-label="Sign in to Grok">
      <button class="modal-close" title="Close" aria-label="Close">✕</button>
      <h3>Sign in to Grok</h3>
      <p>Open <a href="${escapeHTML(verifyURL)}" target="_blank" rel="noopener">the verification page</a> and enter this code:</p>
      <div class="device-code">${escapeHTML(userCode)}</div>
      <div class="device-copy"><button type="button">Copy code</button></div>
      <p class="device-wait">Waiting for you to finish in the browser...</p>
    </div>`;
  document.body.appendChild(overlay);
  overlay.querySelector('.device-copy button').addEventListener('click', () => {
    navigator.clipboard?.writeText(userCode).then(() => toast('Code copied'), () => {});
  });
  // Escaping the dialog never aborts the login: the background poll keeps
  // running and the account appears when x.ai confirms it.
  const close = () => {
    overlay.remove();
    document.removeEventListener('keydown', onKey);
  };
  const onKey = (e) => { if (e.key === 'Escape') close(); };
  overlay.querySelector('.modal-close').addEventListener('click', close);
  overlay.addEventListener('click', e => { if (e.target === overlay) close(); });
  document.addEventListener('keydown', onKey);
}

// confirmDialog is the in-app replacement for window.confirm: themed,
// keyboard-friendly, and destructive actions get their own styling.
function confirmDialog({ title, message, confirmLabel = 'Confirm', cancelLabel = 'Cancel', danger = false, logoHTML = '' }) {
  return new Promise(resolve => {
    const overlay = document.createElement('div');
    overlay.className = 'device-overlay confirm-overlay';
    overlay.innerHTML = `
      <div class="device-modal confirm-modal" role="alertdialog" aria-modal="true" aria-label="${escapeHTML(title)}">
        ${logoHTML ? `<span class="confirm-logo">${logoHTML}</span>` : ''}
        <h3>${escapeHTML(title)}</h3>
        ${message ? `<p class="confirm-message">${escapeHTML(message)}</p>` : ''}
        <div class="device-copy confirm-actions">
          <button type="button" data-cancel>${escapeHTML(cancelLabel)}</button>
          <button type="button" class="${danger ? 'danger' : 'primary'}" data-ok>${escapeHTML(confirmLabel)}</button>
        </div>
      </div>`;
    document.body.appendChild(overlay);
    const ok = overlay.querySelector('[data-ok]');
    const cancel = overlay.querySelector('[data-cancel]');
    ok.focus();
    const close = (value) => { overlay.remove(); document.removeEventListener('keydown', onKey); resolve(value); };
    const onKey = (e) => {
      if (e.key === 'Escape') close(false);
      if (e.key === 'Enter') close(true);
    };
    ok.addEventListener('click', () => close(true));
    cancel.addEventListener('click', () => close(false));
    overlay.addEventListener('click', e => { if (e.target === overlay) close(false); });
    document.addEventListener('keydown', onKey);
  });
}

// promptKey asks for an API key inline (a real dialog beats prompt()).
function promptKey(name) {
  return new Promise(resolve => {
    const overlay = document.createElement('div');
    overlay.className = 'device-overlay';
    overlay.innerHTML = `
      <div class="device-modal">
        <h3>Add an ${escapeHTML(name)} account</h3>
        <p>Paste the API key from your ${escapeHTML(name)} account.</p>
        <input type="password" spellcheck="false" autocomplete="off" placeholder="sk-...">
        <div class="device-copy">
          <button type="button" data-cancel>Cancel</button>
          <button type="button" class="primary" data-ok>Add account</button>
        </div>
      </div>`;
    document.body.appendChild(overlay);
    const input = overlay.querySelector('input');
    input.focus();
    const close = (value) => { overlay.remove(); resolve(value); };
    overlay.querySelector('[data-ok]').addEventListener('click', () => close(input.value.trim()));
    overlay.querySelector('[data-cancel]').addEventListener('click', () => close(null));
    input.addEventListener('keydown', e => {
      if (e.key === 'Enter') close(input.value.trim());
      if (e.key === 'Escape') close(null);
    });
    overlay.addEventListener('click', e => { if (e.target === overlay) close(null); });
  });
}

// The hub dialog hands the user everything T3 Code's "Add a CLIProxyAPI
// hub" dialog asks for: the hub URL and the management key.
document.addEventListener('click', (event) => {
  if (!event.target.closest('#hub-button')) return;
  document.querySelector('.hub-modal')?.remove();
  const overlay = document.createElement('div');
  overlay.className = 'device-overlay';
  overlay.innerHTML = `
    <div class="device-modal" role="dialog" aria-modal="true" aria-label="Use Switcher as a hub">
      <button class="modal-close" title="Close" aria-label="Close">✕</button>
      <h3>Use Switcher as a hub</h3>
      <p>In T3 Code: <em>Settings → Providers → Add a CLIProxyAPI hub</em>. Enter these values; the key stays on this machine.</p>
      <label class="field-label">Hub URL</label>
      <div class="copy-row"><input readonly value="${escapeHTML(data.hub_url || 'http://127.0.0.1:8787')}"><button class="copy-btn" data-copy="${escapeHTML(data.hub_url || '')}">Copy</button></div>
      <label class="field-label">Management key</label>
      <div class="copy-row"><input readonly type="password" value="${escapeHTML(data.hub_management_key || '')}"><button class="copy-btn" data-copy="${escapeHTML(data.hub_management_key || '')}">Copy</button></div>
      <p class="device-wait">T3 Code shows the quota of every account this hub pools. Codex and Claude accounts are supported.</p>
    </div>`;
  document.body.appendChild(overlay);
  const close = () => { overlay.remove(); document.removeEventListener('keydown', onKey); };
  const onKey = (e) => { if (e.key === 'Escape') close(); };
  overlay.querySelector('.modal-close').addEventListener('click', close);
  overlay.addEventListener('click', e => { if (e.target === overlay) close(); });
  document.addEventListener('keydown', onKey);
  overlay.querySelectorAll('.copy-btn').forEach(b => b.addEventListener('click', () => {
    navigator.clipboard?.writeText(b.dataset.copy).then(() => toast('Copied'), () => {});
  }));
});

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
