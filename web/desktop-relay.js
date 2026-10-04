// Claude Desktop account switching in Settings: connection controls and an
// opt-in account per conversation, fed by GET /api/desktop-relay.
const base = '/api/desktop-relay';
const escape = value => String(value ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const text = value => typeof value === 'string' ? value : '';
const count = value => Number.isSafeInteger(value) && value > 0 ? value : 0;
const projectName = value => text(value).replace(/[\\/]+$/, '').split(/[\\/]/).pop() || '';
const timestamp = value => {
  const ms = typeof value === 'string' ? Date.parse(value) : NaN;
  return Number.isFinite(ms) && ms > 0 ? ms : 0;
};
const VISIBLE = 6;
const POLL_MS = 4000;

export function desktopRelayActivity(value, now = Date.now()) {
  const ms = timestamp(value);
  if (!ms || !Number.isFinite(now)) return null;
  const minutes = Math.floor(Math.max(0, now - ms) / 60000);
  const date = new Date(ms);
  return { iso: date.toISOString(), title: date.toLocaleString(),
    label: minutes < 1 ? 'just now' : minutes < 60 ? `${minutes} min ago`
      : minutes < 1440 ? `${Math.floor(minutes / 60)} hr ago`
        : date.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }) };
}

// The API also checks the socket and Host. The UI never offers these controls
// on a LAN dashboard, even when that dashboard has a valid browser session.
export function desktopRelayLoopback(hostname) {
  return ['localhost', '::1', '[::1]'].includes(hostname) ||
    /^127(?:\.(?:0|[1-9]\d{0,2})){3}$/.test(hostname) && hostname.split('.').every(part => Number(part) <= 255);
}

// One entry per Desktop conversation; request sessions not linked to a
// conversation are listed on their own.
export function desktopRelayEntries(body) {
  const sessions = (body?.sessions || []).filter(s => typeof s?.session_id === 'string' && typeof s?.scope_id === 'string');
  const byConversation = new Map();
  for (const s of sessions) {
    if (!text(s.conversation_id)) continue;
    const key = s.scope_id + '/' + s.conversation_id;
    if (!byConversation.has(key)) byConversation.set(key, []);
    byConversation.get(key).push(s);
  }
  const bindings = new Map((body?.conversation_bindings || []).map(b => [b.scope_id + '/' + b.conversation_id, b]));
  const entry = (members, extra) => {
    members = [...members].sort((a, b) => timestamp(b.last_seen) - timestamp(a.last_seen));
    const latest = members[0];
    const titled = members.find(m => text(m.title).trim());
    const answered = members.filter(m => m.last_response).sort((a, b) => timestamp(b.last_response.at) - timestamp(a.last_response.at))[0];
    return { scope: latest.scope_id, title: text(titled?.title).trim(), project: projectName(members.find(m => text(m.project))?.project),
      lastSeen: latest.last_seen, inFlight: members.reduce((sum, m) => sum + count(m.in_flight), 0),
      lastResponse: answered?.last_response || null, ...extra(latest) };
  };
  const entries = [];
  for (const [key, members] of byConversation) {
    const bound = text(bindings.get(key)?.account_id);
    entries.push(entry(members, latest => ({ key: 'c:' + key, conversation: latest.conversation_id, session: '',
      account: bound || text(latest.account_id), sessionOnly: !bound && !!text(latest.account_id) })));
  }
  for (const s of sessions) {
    if (text(s.conversation_id)) continue;
    entries.push(entry([s], latest => ({ key: 's:' + latest.scope_id + '/' + latest.session_id, conversation: '', session: latest.session_id,
      revision: latest.revision, account: text(latest.account_id), sessionOnly: false })));
  }
  return entries.sort((a, b) => b.inFlight - a.inFlight || timestamp(b.lastSeen) - timestamp(a.lastSeen));
}

// All I/O belongs to the dashboard adapter. Constructing this controller does
// nothing until it is activated.
export function createDesktopRelay({ api, getContext, getAccounts = () => [], copy = async () => {},
  confirmStop = async () => false, confirmRestart = async () => false, now = () => Date.now() }) {
  let active = false, snapshot = null, entries = [], loadedAt = 0, reading = null, epoch = 0;
  let busy = null, message = '', failed = false, showAll = false, profile = null;
  const rowErrors = new Map();
  const views = { settings: null };
  const rendered = { settings: null };
  const claudeAccounts = () => getAccounts().filter(account => account.provider === 'claude');
  const access = () => {
    const context = getContext();
    return active && context.loopback && !context.locked && !context.loggingOut &&
      (['cookie', 'device'].includes(context.authKind) || !!context.controllerKey);
  };
  const request = (path, options = {}) => {
    const context = getContext();
    return api(path, { cache: 'no-store', ...options, controllerKey: context.authKind ? undefined : context.controllerKey });
  };
  const errorText = (error, fallback) => text(error?.body?.error?.message) || text(error?.body?.error) || fallback;
  const connected = () => !!snapshot?.setup?.configured && !!snapshot?.status?.listening;

  async function load() {
    if (!access()) return false;
    if (reading) return reading;
    const version = epoch;
    reading = (async () => {
      try {
        const body = await request(base);
        if (version !== epoch || !access()) return false;
        snapshot = { status: body.status || {}, setup: body.setup || {}, scopes: body.scopes || [] };
        entries = desktopRelayEntries(body);
        loadedAt = now();
        if (!busy && failed && !message.startsWith('Could not')) { failed = false; message = ''; }
        return true;
      } catch (error) {
        if (version === epoch && access()) { failed = true; message = errorText(error, 'Could not load Claude Desktop status.'); }
        return false;
      } finally {
        reading = null;
        if (version === epoch) render();
      }
    })();
    return reading;
  }

  async function run(kind, path, method, body, done, before) {
    if (!access() || busy) return false;
    busy = { kind };
    failed = false;
    message = '';
    render();
    try {
      if (before && !await before()) return false;
      const result = await request(path, { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
      message = typeof done === 'function' ? done(result) : done;
      return true;
    } catch (error) {
      failed = true;
      message = errorText(error, 'That did not work. Refresh and try again.');
      return false;
    } finally {
      busy = null;
      await load();
      render();
    }
  }

  async function choose(key, accountID) {
    const item = entries.find(e => e.key === key);
    if (!item || !access() || busy || typeof accountID !== 'string') return false;
    if (accountID && !claudeAccounts().some(a => a.id === accountID)) return false;
    if (accountID === item.account && !item.sessionOnly) return true;
    const path = item.conversation
      ? `${base}/scopes/${encodeURIComponent(item.scope)}/conversations/${encodeURIComponent(item.conversation)}/account`
      : `${base}/scopes/${encodeURIComponent(item.scope)}/sessions/${encodeURIComponent(item.session)}/account`;
    const body = item.conversation ? (accountID ? { account_id: accountID } : {}) : { ...(accountID ? { account_id: accountID } : {}), revision: item.revision };
    busy = { kind: 'choose', key, account: accountID };
    rowErrors.delete(key);
    render();
    try {
      await request(path, { method: accountID ? 'POST' : 'DELETE', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
      item.account = accountID;
      item.sessionOnly = false;
      return true;
    } catch (error) {
      rowErrors.set(key, errorText(error, 'Could not switch this conversation.'));
      return false;
    } finally {
      busy = null;
      await load();
      render();
    }
  }

  const accountLabel = account => text(account?.email).split('@')[0] || account?.id || '';
  function pickerHTML(item) {
    const accounts = claudeAccounts();
    const pending = busy?.kind === 'choose' && busy.key === item.key;
    const selected = pending ? busy.account : accounts.some(a => a.id === item.account) ? item.account : '';
    const disabled = !access() || !!busy;
    const options = [{ id: '', label: 'Desktop login', title: 'The account Claude Desktop is signed in to' },
      ...accounts.map(a => ({ id: a.id, label: accountLabel(a), title: a.email }))];
    if (options.length > 4) {
      return `<select class="dr-select" data-dr-choose="${escape(item.key)}" aria-label="Account for ${escape(item.title || 'this conversation')}" ${disabled ? 'disabled' : ''}>
        ${options.map(o => `<option value="${escape(o.id)}" ${o.id === selected ? 'selected' : ''}>${escape(o.id ? o.title : o.label)}</option>`).join('')}</select>`;
    }
    return `<div class="dr-segmented" role="radiogroup" aria-label="Account for ${escape(item.title || 'this conversation')}">
      ${options.map(o => `<button type="button" role="radio" aria-checked="${o.id === selected}" data-dr-choose="${escape(item.key)}" data-dr-account="${escape(o.id)}" title="${escape(o.title)}" ${disabled ? 'disabled' : ''}>
        ${pending && o.id === selected ? '<span class="dr-spinner" aria-hidden="true"></span>' : ''}${escape(o.label)}</button>`).join('')}
    </div>`;
  }

  function rowHTML(item) {
    const activity = desktopRelayActivity(item.lastSeen, now());
    const response = item.lastResponse;
    const answeredBy = response?.route === 'selected'
      ? accountLabel(claudeAccounts().find(a => a.id === response.account_id)) || 'selected account'
      : response ? 'Desktop login' : '';
    const meta = [
      item.project ? `<span>${escape(item.project)}</span>` : '',
      item.inFlight ? `<span class="dr-live">${item.inFlight} replying</span>`
        : activity ? `<time datetime="${activity.iso}" title="${escape(activity.title)}">${escape(activity.label)}</time>` : '',
      answeredBy ? `<span title="Credential used for the last reply${response.status >= 400 ? `, HTTP ${response.status}` : ''}">last reply · ${escape(answeredBy)}${response.status >= 400 ? ` <b class="dr-failed">${response.status}</b>` : ''}</span>` : '',
    ].filter(Boolean).join('<span class="dr-sep" aria-hidden="true">·</span>');
    const error = rowErrors.get(item.key);
    return `<li class="dr-row ${item.inFlight ? 'is-live' : ''}" data-dr-key="${escape(item.key)}">
      <span class="dr-dot" aria-hidden="true"></span>
      <div class="dr-main">
        <p class="dr-title">${item.title ? escape(item.title) : `<span class="dr-untitled">Untitled ${item.conversation ? 'conversation' : 'session'}</span>`}</p>
        <p class="dr-meta">${meta}</p>
        ${item.sessionOnly ? '<p class="dr-note">Only the current session is switched. Pick an account to switch the whole conversation.</p>' : ''}
        ${error ? `<p class="dr-error" role="alert">${escape(error)}</p>` : ''}
      </div>
      ${pickerHTML(item)}
    </li>`;
  }

  // Per-conversation routing is opt-in: every conversation uses Desktop's own
  // login until an account is picked for it here.
  function conversationsHTML() {
    if (!connected() && !entries.length) return '';
    const shown = showAll ? entries : entries.slice(0, VISIBLE);
    return `<div class="dr-conversations">
      <h3>Per-conversation accounts</h3>
      <p class="settings-sub">Off unless you pick one: conversations use Desktop's own login. A pick applies to that conversation's new messages; running replies finish where they started.</p>
      ${entries.length ? `<ul class="dr-list">${shown.map(rowHTML).join('')}</ul>` : '<p class="settings-sub">Conversations appear here after you send a message in Claude Desktop.</p>'}
      ${entries.length > VISIBLE ? `<button type="button" class="quiet" data-dr-action="toggle-all">${showAll ? 'Show fewer' : `Show all ${entries.length}`}</button>` : ''}
    </div>`;
  }

  function settingsHTML() {
    const context = getContext();
    if (context.locked || context.loggingOut) return '';
    if (!context.loopback) return `<h2>Claude Desktop</h2><p class="settings-sub">Open Switcher on this Mac (localhost) to manage Claude Desktop.</p>`;
    const status = snapshot?.status || {}, setup = snapshot?.setup || {};
    const doing = kind => busy?.kind === kind;
    const disabled = !access() || !!busy;
    const configured = !!setup.configured;
    const state = !snapshot ? 'Loading…' : connected() ? 'Connected' : configured ? 'Relay stopped'
      : setup.condition === 'changed' ? 'Settings changed' : setup.condition === 'pending' ? 'Setup pending' : 'Not connected';
    const scopes = snapshot?.scopes || [];
    return `<div class="dr-settings-head">
        <div><h2>Claude Desktop</h2><p class="settings-sub">Optional: pick a Switcher account for individual Desktop conversations. Desktop stays signed in as it is.</p></div>
        <span class="dr-status ${connected() ? 'ok' : 'warn'}">${state}</span>
      </div>
      ${message ? `<p class="dr-message ${failed ? 'error' : ''}" role="${failed ? 'alert' : 'status'}">${escape(message)}</p>` : ''}
      <div class="dr-actions">
        ${!configured || setup.condition === 'changed' ? `<button type="button" class="primary" data-dr-action="configure" ${disabled ? 'disabled' : ''}>${doing('configure') ? 'Connecting…' : 'Connect Claude Desktop'}</button>` : ''}
        ${configured || setup.restart_required ? `<button type="button" data-dr-action="restart" ${disabled ? 'disabled' : ''}>${doing('restart') ? 'Restarting…' : 'Restart Claude Desktop'}</button>` : ''}
        ${configured || ['changed', 'pending'].includes(setup.condition) ? `<button type="button" class="quiet" data-dr-action="restore" ${disabled ? 'disabled' : ''}>${doing('restore') ? 'Disconnecting…' : 'Disconnect'}</button>` : ''}
      </div>
      ${configured && !status.listening ? '<p class="settings-sub">Start the relay below to resume switching.</p>' : ''}
      ${conversationsHTML()}
      <p class="settings-sub">Connecting adds a local proxy to <code>${escape(setup.settings_path || '~/.claude/settings.json')}</code> and keeps a backup. Restart Desktop once after connecting or disconnecting; switching accounts never needs a restart.</p>
      <details class="dr-advanced">
        <summary>Advanced</summary>
        <div class="dr-advanced-body">
          <div class="settings-row"><div><strong>Relay</strong><span class="dim"> · ${status.listening ? escape(status.address || 'listening') : 'stopped'}${count(status.in_flight) ? ` · ${count(status.in_flight)} in flight` : ''}</span></div>
            ${status.listening
              ? `<button type="button" data-dr-action="stop" ${disabled || configured ? 'disabled' : ''} ${configured ? 'title="Disconnect Claude Desktop first"' : ''}>${doing('stop') ? 'Stopping…' : 'Stop'}</button>`
              : `<button type="button" data-dr-action="start" ${disabled ? 'disabled' : ''}>${doing('start') ? 'Starting…' : 'Start'}</button>`}
          </div>
          ${setup.backup_path ? `<p class="settings-sub">Backup: <code>${escape(setup.backup_path)}</code></p>` : ''}
          <p class="settings-sub">Manual profiles give another app its own proxy login. Copy the environment once; it is not shown again.</p>
          ${profile ? `<div class="dr-profile-env"><textarea readonly rows="4" spellcheck="false">${escape(JSON.stringify(profile.env, null, 2))}</textarea>
            <div class="dr-actions"><button type="button" data-dr-action="copy-profile">Copy environment</button><button type="button" class="quiet" data-dr-action="dismiss-profile">Done</button></div></div>` : ''}
          <form class="dr-profile-form" data-dr-profile-form>
            <input name="label" placeholder="Profile name" maxlength="120" autocomplete="off" required ${disabled || !status.listening ? 'disabled' : ''}>
            <button type="submit" ${disabled || !status.listening ? 'disabled' : ''}>${doing('create') ? 'Creating…' : 'New profile'}</button>
          </form>
          ${scopes.length ? `<ul class="dr-scopes">${scopes.map(scope => `<li><span>${escape(scope.label || 'Unnamed profile')}</span>
            <button type="button" class="quiet danger" data-dr-action="revoke" data-dr-scope="${escape(scope.id)}" ${disabled || scope.id === setup.scope_id && configured ? 'disabled' : ''}>Revoke</button></li>`).join('')}</ul>` : ''}
        </div>
      </details>`;
  }

  function paint(name, html) {
    const view = views[name];
    if (!view || rendered[name] === html) return;
    const focused = view.contains(view.ownerDocument.activeElement) ? view.ownerDocument.activeElement : null;
    const focusKey = focused && (focused.dataset.drAction || (focused.dataset.drChoose && focused.dataset.drChoose + '|' + (focused.dataset.drAccount ?? '')));
    const open = view.querySelector('details.dr-advanced')?.open;
    view.innerHTML = html;
    rendered[name] = html;
    if (open) view.querySelector('details.dr-advanced')?.setAttribute('open', '');
    if (focusKey) {
      const next = [...view.querySelectorAll('[data-dr-action], [data-dr-choose]')].find(node =>
        (node.dataset.drAction || (node.dataset.drChoose && node.dataset.drChoose + '|' + (node.dataset.drAccount ?? ''))) === focusKey);
      next?.focus({ preventScroll: true });
    }
  }
  function render() {
    paint('settings', settingsHTML());
  }

  function bind(root) {
    if (root.dataset.drBound) return;
    root.dataset.drBound = '1';
    root.addEventListener('click', event => {
      const target = event.target.closest('[data-dr-action], button[data-dr-choose]');
      if (!target || target.disabled || !root.contains(target)) return;
      if (target.dataset.drChoose) return choose(target.dataset.drChoose, target.dataset.drAccount || '');
      switch (target.dataset.drAction) {
        case 'toggle-all': showAll = !showAll; return render();
        case 'configure': return configure();
        case 'restore': return restore();
        case 'restart': return restart();
        case 'start': return start();
        case 'stop': return stop();
        case 'revoke': return revoke(target.dataset.drScope);
        case 'copy-profile': return copyProfile();
        case 'dismiss-profile': profile = null; return render();
      }
    });
    root.addEventListener('change', event => {
      const select = event.target.closest('select[data-dr-choose]');
      if (select && root.contains(select)) choose(select.dataset.drChoose, select.value);
    });
    root.addEventListener('submit', event => {
      if (!event.target.matches('[data-dr-profile-form]')) return;
      event.preventDefault();
      createProfile(event.target.elements.label?.value || '');
    });
  }
  function mountSettings(root) {
    if (views.settings !== root) { views.settings = root; rendered.settings = null; bind(root); }
    render();
    return update(true);
  }

  function configure() {
    return run('configure', `${base}/configure`, 'POST', {}, 'Connected. Restart Claude Desktop when your current work is done.');
  }
  function restore() {
    return run('restore', `${base}/restore`, 'POST', {}, 'Disconnected. Restart Claude Desktop when your current work is done.');
  }
  function restart() {
    return run('restart', `${base}/restart-desktop`, 'POST', { confirmed: true }, 'Claude Desktop is restarting.', async () => await confirmRestart() === true);
  }
  function start() {
    return run('start', `${base}/start`, 'POST', {}, 'Relay started.');
  }
  function stop() {
    const inFlight = count(snapshot?.status?.in_flight);
    return run('stop', `${base}/stop`, 'POST', {}, 'Relay stopped.', inFlight ? () => confirmStop({ inFlight }) : null);
  }
  function revoke(scope) {
    if (!snapshot?.scopes?.some(s => s.id === scope)) return Promise.resolve(false);
    return run('revoke', `${base}/scopes/${encodeURIComponent(scope)}`, 'DELETE', {}, 'Profile revoked.');
  }
  function createProfile(label) {
    if (!label.trim()) return Promise.resolve(false);
    profile = null;
    return run('create', `${base}/scopes`, 'POST', { label: label.trim().slice(0, 120) }, result => {
      profile = { env: { HTTPS_PROXY: text(result?.env?.HTTPS_PROXY), NODE_EXTRA_CA_CERTS: text(result?.env?.NODE_EXTRA_CA_CERTS) } };
      return 'Profile created. Copy its environment now.';
    });
  }
  async function copyProfile() {
    if (!profile) return false;
    try {
      await copy(JSON.stringify(profile.env, null, 2));
      profile = null;
      failed = false;
      message = 'Environment copied.';
    } catch {
      failed = true;
      message = 'Could not copy. Select the text and copy it manually.';
    }
    render();
    return !failed;
  }

  // Called on every dashboard state poll; reloads at most every few seconds.
  function update(force = false) {
    if (!access()) return render();
    if (force || !loadedAt || now() - loadedAt >= POLL_MS - 250) return load();
    render();
  }
  function setActive(next) {
    if (active === next) return;
    active = next;
    epoch++;
    if (next) return load();
    render();
  }
  function clear() {
    active = false;
    epoch++;
    snapshot = null;
    entries = [];
    loadedAt = 0;
    busy = null;
    message = '';
    failed = false;
    profile = null;
    rowErrors.clear();
    views.settings?.replaceChildren();
    rendered.settings = null;
  }
  return { mountSettings, update, setActive, clear, refresh: load, choose, configure, restore, restart, start, stop, revoke, createProfile,
    entries: () => entries, settingsHTML };
}
