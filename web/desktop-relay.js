const base = '/api/desktop-relay';
const escape = value => String(value ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const taskKey = (scope, session) => JSON.stringify([scope, session]);
const count = value => Number.isSafeInteger(value) && value >= 0 ? value : 0;
const text = value => typeof value === 'string' ? value : '';
const validRevision = value => Number.isSafeInteger(value) && value >= 0;
const uuidID = value => typeof value === 'string' && /^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/.test(value) && value !== '00000000-0000-0000-0000-000000000000';
const projectName = value => text(value).replace(/[\\/]+$/, '').split(/[\\/]/).pop() || '';
const normalizedIdentity = value => value.normalize('NFC').replace(/\s+/g, ' ').trim();
function shortID(id, ids) {
  let short = id.slice(0, 8);
  while (short.length < id.length && ids.some(peer => peer !== id && peer.startsWith(short))) short = id.slice(0, short.length + 4);
  return short;
}
function sessionIdentities(sessions, scopes) {
  const sessionIDs = sessions.map(task => task.session_id), scopeIDs = sessions.map(task => task.scope_id);
  const identities = new Map(), groups = new Map();
  for (const task of sessions) {
    const session = shortID(task.session_id, sessionIDs);
    const title = task.title.trim() ? task.title : `Code session ${session}`;
    const project = projectName(task.project);
    const scope = shortID(task.scope_id, scopeIDs);
    const scopeLabel = scopes.find(scope => scope.id === task.scope_id)?.label || '';
    const identity = { title, project, session, scope, profile: scopeLabel.trim() ? scopeLabel : scope,
      scope_id: task.scope_id, session_id: task.session_id, conversation: !!task.conversation_id, sessionHint: '', profileHint: '' };
    identities.set(task.key, identity);
    // Group the rendered title and basename, since whitespace and full paths
    // must not make otherwise identical rows appear distinguishable.
    const key = JSON.stringify([normalizedIdentity(title), normalizedIdentity(project)]);
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(identity);
  }
  for (const group of groups.values()) {
    if (group.length < 2) continue;
    for (const identity of group) {
      if (group.some(peer => peer.session_id !== identity.session_id || peer.conversation !== identity.conversation)) identity.sessionHint = identity.session;
      const peers = group.filter(peer => peer.session_id === identity.session_id && peer.scope_id !== identity.scope_id);
      if (peers.length) identity.profileHint = peers.some(peer => normalizedIdentity(peer.profile) === normalizedIdentity(identity.profile))
        ? `${identity.profile} · ${identity.scope}` : identity.profile;
    }
  }
  return identities;
}
function activityTimestamp(value) {
  const timestamp = typeof value === 'number' ? value * 1000
    : typeof value === 'string' && /^\d+(?:\.\d+)?$/.test(value) ? Number(value) * 1000
      : typeof value === 'string' && /^\d{4}-\d{2}-\d{2}T/.test(value) ? Date.parse(value) : NaN;
  return Number.isFinite(timestamp) && timestamp > 0 && Number.isFinite(new Date(timestamp).getTime()) ? timestamp : null;
}
export function desktopRelayActivity(value, now = Date.now()) {
  const timestamp = activityTimestamp(value);
  if (timestamp === null || !Number.isFinite(now)) return null;
  const date = new Date(timestamp), age = Math.max(0, now - timestamp);
  const minutes = Math.floor(age / 60000);
  return { iso: date.toISOString(), title: date.toLocaleString(),
    label: minutes < 1 ? 'Now' : minutes < 60 ? `${minutes} min ago`
      : minutes < 1440 ? `${Math.floor(minutes / 60)} hr ago`
        : date.toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' }) };
}
function responseEvidence(value) {
  if (!value || !['caller', 'selected'].includes(value.route) || !Number.isInteger(value.status) ||
    value.status < 100 || value.status > 599 || activityTimestamp(value.at) === null) return null;
  return { route: value.route, account_id: text(value.account_id), model: safeMessage(value.model),
    status: value.status, at: new Date(activityTimestamp(value.at)).toISOString(), sequence: count(value.sequence) };
}
function responseIsLater(response, previous) {
  const sequence = count(response?.sequence), previousSequence = count(previous?.sequence);
  if (sequence > 0 && previousSequence > 0 && sequence !== previousSequence) return sequence > previousSequence;
  return (activityTimestamp(response?.at) || 0) > (activityTimestamp(previous?.at) || 0);
}
function commonMetadata(members, field, normalize = normalizedIdentity) {
  const values = members.map(member => text(member[field])).filter(value => value.trim());
  return values.length && new Set(values.map(normalize)).size === 1 ? values[0] : '';
}
function memberRevisions(members) {
  const revisions = Object.create(null);
  for (const member of members) {
    if (!uuidID(member.session_id) || !validRevision(member.revision) || Object.hasOwn(revisions, member.session_id)) return null;
    revisions[member.session_id] = member.revision;
  }
  return revisions;
}
function conversationViews(sessions, conversationBindings) {
  const groups = new Map(), bindings = new Map(), views = [];
  for (const binding of conversationBindings) {
    const key = taskKey(binding.scope_id, binding.conversation_id);
    bindings.set(key, bindings.has(key) ? { ...binding, revision: NaN } : binding);
  }
  for (const task of sessions) {
    if (!task.conversation_id) {
      views.push({ ...task, key: taskKey(task.scope_id, task.session_id), members: [task], selection: task.account_id ? 'bound' : 'default' });
      continue;
    }
    const key = taskKey(task.scope_id, task.conversation_id);
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(task);
  }
  for (const [key, members] of groups) {
    members.sort((a, b) => a.session_id < b.session_id ? -1 : a.session_id > b.session_id ? 1 : 0);
    const first = members[0], binding = bindings.get(key), activeBinding = !!binding?.account_id;
    const legacy = members.filter(member => member.account_id);
    const legacyAccounts = [...new Set(legacy.map(member => member.account_id))];
    const selection = activeBinding ? 'bound' : legacyAccounts.length > 1 ? 'mixed' : legacy.length ? 'partial' : 'default';
    const associationVerified = members.every(member => member.association_verified !== false);
    const latest = (field, timestamp) => members.reduce((result, member) =>
      timestamp(member[field]) > timestamp(result) ? member[field] : result, null);
    views.push({ ...first, key: key + ':conversation', session_id: first.conversation_id, members,
      title: commonMetadata(members, 'title'), project: commonMetadata(members, 'project', value => normalizedIdentity(projectName(value))),
      title_source: commonMetadata(members, 'title_source'), client_kind: associationVerified ? 'desktop' : commonMetadata(members, 'client_kind'), model: commonMetadata(members, 'model'),
      parent: '', parent_session_id: '', agent: '', agent_id: '',
      account_id: activeBinding ? binding.account_id : legacyAccounts.length === 1 ? legacyAccounts[0] : '',
      revision: binding ? binding.revision : 0, binding_present: activeBinding, binding_record_exists: !!binding, selection, legacy_count: legacy.length,
      association_verified: associationVerified,
      member_revisions: memberRevisions(members),
      requests: members.reduce((sum, member) => sum + count(member.requests), 0),
      in_flight: members.reduce((sum, member) => sum + count(member.in_flight), 0),
      last_seen: latest('last_seen', value => activityTimestamp(value) || 0),
      last_response: members.reduce((result, member) => responseIsLater(member.last_response, result) ? member.last_response : result, null),
    });
  }
  return views;
}
const setupConditions = ['not_configured', 'configured', 'changed', 'pending', 'unavailable'];
const safeMessage = value => typeof value === 'string' ? value
  .replace(/\b(https?:\/\/)[^/\s"'<>]*@/gi, '$1[redacted]@')
  .replace(/\b(Bearer|Basic)\s+[A-Za-z0-9._~+/-]+=*/gi, '$1 [redacted]')
  .replace(/\b(HTTPS_PROXY|NODE_EXTRA_CA_CERTS)\s*[:=]\s*(?:"[^"]*"|'[^']*'|[^\s,;}]+)/gi, '$1=[redacted]')
  .replace(/[\x00-\x1f\x7f]/g, ' ').trim().slice(0, 800) : '';
function setupStatus(value) {
  const condition = value == null ? 'not_configured' : setupConditions.includes(value.condition) ? value.condition : 'unavailable';
  return { condition, configured: condition === 'configured' && value?.configured !== false,
    restart_required: value?.restart_required === true,
    settings_path: typeof value?.settings_path === 'string' ? value.settings_path : '',
    backup_path: typeof value?.backup_path === 'string' ? value.backup_path : '',
    scope_id: typeof value?.scope_id === 'string' ? value.scope_id : '',
    message: safeMessage(value?.message) };
}

// The API also checks the socket and Host. The UI never offers these controls
// on a LAN dashboard, even when that dashboard has a valid browser session.
export function desktopRelayLoopback(hostname) {
  return ['localhost', '::1', '[::1]'].includes(hostname) ||
    /^127(?:\.(?:0|[1-9]\d{0,2})){3}$/.test(hostname) && hostname.split('.').every(part => Number(part) <= 255);
}

// All I/O belongs to the dashboard adapter. Constructing this controller does
// nothing. Settings explicitly opens it, and refreshing never mutates the relay.
export function createDesktopRelay({ api, getContext, getAccounts = () => [], copy, confirmStop = async () => false, confirmRestart = async () => false, paint = () => {}, now = () => Date.now() }) {
  let active = false, epoch = 0, generation = 0, snapshot = null, reading = null, attempted = false;
  let message = '', failed = false, view = null, busy = null, manualSetup = null, copying = false, profileLabel = '';
  let advancedOpen = false, detailsOpen = false, recentOpen = false;
  let focusRestore = null, renderedMarkup = null, focusRekey = null;
  const choices = new Map();
  const choiceReview = new Set();
  const diagnosticsOpen = new Set();
  const mounted = new WeakSet();
  const claudeAccounts = () => getAccounts().filter(account => account.provider === 'claude');
  const taskViews = () => snapshot?.views || [];
  function lookupTask(scope, session, conversationID) {
    const matches = taskViews().filter(task => task.scope_id === scope && (conversationID !== undefined
      ? conversationID ? task.conversation_id === conversationID : !task.conversation_id && task.session_id === session
      : task.session_id === session || task.conversation_id && task.members.some(member => member.session_id === session)));
    return matches.length === 1 ? matches[0] : null;
  }
  function chosenAccount(task) {
    if (choiceReview.has(task.key) || task.conversation_id && task.association_verified === false) return '';
    const key = task.key;
    const id = choices.has(key) ? choices.get(key) : task.account_id;
    return claudeAccounts().some(account => account.id === id) ? id : '';
  }
  function selectionChanged(task, chosen) {
    return task.conversation_id && !task.binding_present ? chosen !== '' || task.legacy_count > 0 : chosen !== task.account_id;
  }
  function canResetTask(task) {
    return !task.conversation_id || task.binding_record_exists || snapshot?.status.listening && task.association_verified !== false;
  }
  function reconcileViews(previous, next) {
    captureDetails();
    const nextChoices = new Map(), nextReviews = new Set(), nextDiagnostics = new Set(), rekeys = new Map();
    for (const task of next) {
      const sources = previous.filter(old => old.key === task.key || !old.conversation_id && task.conversation_id &&
        old.scope_id === task.scope_id && task.members.some(member => member.session_id === old.session_id));
      for (const old of sources) {
        if (old.key !== task.key) rekeys.set(old.key, { task, member: old.session_id });
        if (diagnosticsOpen.has(old.key)) nextDiagnostics.add(task.key);
      }
      const pending = sources.filter(old => choices.has(old.key) && !(busy?.task === old.key && busy.epoch !== epoch) &&
        (old.key === task.key || task.association_verified !== false)).map(old => choices.get(old.key));
      if (sources.some(old => choiceReview.has(old.key)) || new Set(pending).size > 1) nextReviews.add(task.key);
      else if (pending.length) nextChoices.set(task.key, pending[0]);
    }
    choices.clear();
    choiceReview.clear();
    diagnosticsOpen.clear();
    for (const [key, choice] of nextChoices) choices.set(key, choice);
    for (const key of nextReviews) choiceReview.add(key);
    for (const key of nextDiagnostics) diagnosticsOpen.add(key);
    const remapFocus = key => {
      for (const [from, target] of rekeys) {
        if (key === from) return target.task.key;
        if (key === from + ':session') return target.task.key + ':member:' + target.member;
        for (const suffix of ['use', 'diagnostics']) if (key === from + ':' + suffix) return target.task.key + ':' + suffix;
      }
      return key;
    };
    const focused = view?.contains(view.ownerDocument?.activeElement) ? view.ownerDocument.activeElement : null;
    const key = focused?.dataset.relayFocus;
    if (key && remapFocus(key) !== key) focusRekey = { from: key, to: remapFocus(key) };
    if (focusRestore) focusRestore = { ...focusRestore, key: remapFocus(focusRestore.key) };
  }
  const activeCounts = () => ({ inFlight: Math.max(count(snapshot?.status.in_flight),
    (snapshot?.sessions || []).reduce((sum, task) => sum + count(task.in_flight), 0)),
    activeTasks: (snapshot?.sessions || []).filter(task => count(task.in_flight) > 0).length });

  function access() {
    const context = getContext();
    return active && context.loopback && !context.locked && !context.loggingOut &&
      (['cookie', 'device'].includes(context.authKind) || !!context.controllerKey);
  }
  const canConfigure = () => snapshot && ['not_configured', 'configured', 'changed'].includes(snapshot.setup.condition);
  const ownedUnavailable = () => snapshot?.setup.condition === 'unavailable' &&
    !!snapshot.setup.scope_id.trim() && !!snapshot.setup.backup_path.trim();
  const canRestore = () => snapshot && (['configured', 'changed', 'pending'].includes(snapshot.setup.condition) || ownedUnavailable());
  const canRestart = () => snapshot && (snapshot.setup.configured && snapshot.status.listening ||
    snapshot.setup.condition === 'not_configured' && snapshot.setup.restart_required);
  function html() {
    const context = getContext();
    if (context.locked || context.loggingOut) return '';
    const status = snapshot?.status;
    const desktopSetup = snapshot?.setup;
    const { inFlight } = activeCounts();
    const sessions = [...taskViews()].sort((a, b) =>
      (activityTimestamp(b.last_seen) || 0) - (activityTimestamp(a.last_seen) || 0) ||
      (taskKey(a.scope_id, a.session_id) < taskKey(b.scope_id, b.session_id) ? -1 : taskKey(a.scope_id, a.session_id) > taskKey(b.scope_id, b.session_id) ? 1 : 0));
    const identities = sessionIdentities(sessions, snapshot?.scopes || []);
    const current = sessions.filter(task => count(task.in_flight) > 0);
    const activeTasks = current.length;
    const recent = sessions.filter(task => !count(task.in_flight));
    const observedRequests = sessions.reduce((sum, task) => sum + count(task.in_flight), 0);
    const relayReady = desktopSetup?.configured && status?.listening;
    const state = !status ? 'Not loaded' : status.condition === 'stopping' ? 'Stopping'
      : !status.enabled ? 'Disabled' : status.listening ? 'Ready' : 'Enabled, not listening';
    return `<div class="desktop-relay-header">
        <div><h2>Claude Desktop</h2><p class="desktop-relay-subtitle">Account switching</p></div>
        <div class="desktop-relay-status"><strong class="desktop-relay-setup-status">${!desktopSetup ? 'Setup not loaded' : relayReady ? 'Relay ready' : ({ not_configured: 'Not configured', configured: 'Configured', changed: 'Settings changed. Reconfigure needed.', pending: 'Setup pending', unavailable: 'Setup unavailable' })[desktopSetup.condition]}</strong>
          <button type="button" data-relay-action="refresh" data-relay-focus="refresh" aria-busy="${!!reading}" ${!access() || reading || busy ? 'disabled' : ''}>${reading ? 'Refreshing…' : 'Refresh status'}</button>
        </div>
      </div>
      <p class="settings-sub desktop-relay-caption">This changes the account used for conversation messages. Desktop sign-in and its usage display stay on the original account.</p>
      <p class="settings-sub">Conversations use their existing Claude sign-in. Choose an account only for a conversation you want to switch.</p>
      ${!context.loopback ? '<p class="settings-sub">Desktop relay controls are available only on a loopback dashboard. Open Switcher on this machine using localhost or a loopback address.</p>' : !context.authKind && !context.controllerKey ? '<p class="settings-sub">Waiting for the local dashboard control key. It is required even when password authentication is off.</p>' : ''}
      ${desktopSetup?.message ? `<p class="settings-sub">${escape(desktopSetup.message)}</p>` : ''}
      ${ownedUnavailable() ? `<p class="settings-sub">${!status.listening ? 'The relay is offline. ' : ''}You can still request removal of Desktop setup. Switcher checks the saved setup before restoring settings.</p>` : ''}
      ${!desktopSetup?.configured || !status?.listening || busy?.kind === 'config' ? `<div class="desktop-relay-controls"><button type="button" class="desktop-relay-primary" data-relay-action="configure" data-relay-focus="configure" aria-busy="${busy?.kind === 'config'}" ${!access() || busy || !canConfigure() ? 'disabled' : ''}>${busy?.kind === 'config' ? 'Configuring…' : 'Configure Claude Desktop'}</button></div>` : ''}
      <p class="settings-sub desktop-relay-feedback" role="${failed ? 'alert' : 'status'}" aria-live="polite">${escape(message)}</p>
      <div class="desktop-relay-conversations">
        ${snapshot ? `<p class="desktop-relay-totals">${sessions.length} remembered · ${activeTasks} receiving ${activeTasks === 1 ? 'a request' : 'requests'}</p>` : '<p class="settings-sub">Loading conversations…</p>'}
        ${inFlight > observedRequests ? `<p class="settings-sub">${inFlight} request${inFlight === 1 ? '' : 's'} in progress (some background requests are unidentified)</p>` : ''}
        ${current.length ? `<h3>Currently responding</h3><div class="desktop-relay-tasks">${current.map(task => taskHTML(task, identities.get(task.key))).join('')}</div>` : inFlight === 0 && snapshot ? '<p class="desktop-relay-idle">No requests in progress</p>' : ''}
        ${recent.length ? `<details class="desktop-relay-recent" data-relay-recent ${recentOpen ? 'open' : ''}>
          <summary data-relay-focus="recent">Recent conversations (${recent.length})</summary>
          <p class="settings-sub desktop-relay-recent-help">Includes idle and previously used sessions. This does not mean they are running.</p>
          <div class="desktop-relay-tasks">${recent.map(task => taskHTML(task, identities.get(task.key))).join('')}</div>
        </details>` : ''}
        ${snapshot && !sessions.length ? `<p class="settings-sub desktop-relay-empty">${desktopSetup?.configured ? 'Start a Code conversation, send a message, then refresh.' : 'Configure Claude Desktop to connect Code conversations.'}</p>` : ''}
      </div>
      ${sessions.length ? '<p class="settings-sub desktop-relay-switch-help">Existing streams finish with their previous account. New requests use the selected account after a switch completes.</p>' : ''}
      <details class="desktop-relay-details" data-relay-details ${detailsOpen ? 'open' : ''}>
        <summary data-relay-focus="setup-details">Connection setup</summary>
        <div class="desktop-relay-advanced-body">
          <p class="settings-sub">Switcher backs up settings and configures connection. Desktop sign-in stays as is.</p>
          <p class="settings-sub">Restart only after changing setup. Changing an account does not require restart.</p>
          ${!message && desktopSetup?.condition === 'not_configured' && desktopSetup.restart_required ? '<p class="settings-sub">Restored previous settings. Restart Desktop when work is finished.</p>' : ''}
          <div class="desktop-relay-controls">
            ${canRestart() ? `<button type="button" data-relay-action="restart-desktop" data-relay-focus="restart-desktop" aria-busy="${busy?.kind === 'restart'}" ${!access() || busy ? 'disabled' : ''}>${busy?.kind === 'restart' ? 'Requesting restart…' : 'Restart Claude Desktop'}</button>` : ''}
            <button type="button" data-relay-action="restore-setup" data-relay-focus="restore-setup" aria-busy="${busy?.kind === 'restore'}" ${!access() || busy || !canRestore() ? 'disabled' : ''}>${busy?.kind === 'restore' ? 'Removing setup…' : 'Remove Desktop setup'}</button>
          </div>
          ${desktopSetup?.settings_path ? `<p class="settings-sub">Settings: <code>${escape(desktopSetup.settings_path)}</code></p>` : ''}
          ${desktopSetup?.backup_path ? `<p class="settings-sub">Backup: <code>${escape(desktopSetup.backup_path)}</code></p>` : ''}
        </div>
      </details>
      <details class="desktop-relay-advanced" data-relay-advanced ${advancedOpen ? 'open' : ''}>
        <summary data-relay-focus="advanced">Advanced</summary>
        <div class="desktop-relay-advanced-body">
          <div class="desktop-relay-status"><strong>${state}</strong></div>
          ${status ? `<p class="settings-sub">Condition: ${escape(status.condition)}${status.listening ? ` · ${escape(status.address)}` : ''}</p>` : ''}
          ${sessions.length || inFlight ? '<p class="settings-sub">Traffic detected. Profiles may be shared by Desktop Code and standalone CLI sessions.</p>' : ''}
          <p class="settings-sub">Unidentified child requests keep caller auth until a separate session is observed and selected.</p>
          <p class="settings-sub desktop-relay-evidence">Upstream responses report message forwarding credentials. Subscription billing is not verified.</p>
          ${status?.condition === 'stopping' ? '<p class="settings-sub">Shutdown is still finishing. Refresh status, or use Start relay to recover once shutdown finishes.</p>' : ''}
          <div class="desktop-relay-controls">
            <button type="button" data-relay-action="start" data-relay-focus="start" aria-busy="${busy?.kind === 'start'}" ${!access() || busy || !status || status.listening ? 'disabled' : ''}>${busy?.kind === 'start' ? 'Starting…' : 'Start relay'}</button>
            <button type="button" data-relay-action="stop" data-relay-focus="stop" aria-busy="${busy?.kind === 'stop'}" ${!access() || busy || !status?.enabled ? 'disabled' : ''}>${busy?.kind === 'stop' ? 'Stopping…' : `Stop relay · ${inFlight} in flight`}</button>
          </div>
          <p class="settings-sub">Stop may be rejected while streams are in flight. Removing Desktop setup does not stop other profiles using the relay.</p>
          <form data-relay-profile-form>
            <label>Profile label<input name="relay-profile-label" value="${escape(profileLabel)}" required maxlength="120" autocomplete="off" data-relay-focus="profile-label" ${!access() || busy ? 'disabled' : ''}></label>
            <button type="submit" data-relay-focus="new-profile" ${!access() || busy || !status?.listening ? 'disabled' : ''}>${busy?.kind === 'create' ? 'Creating…' : 'New profile'}</button>
          </form>
          <p class="settings-sub">Manual profiles are for separate launches. Apply the copied environment only before launching a future Desktop Code task. Existing workers are not retrofitted. Standalone CLI peers launched with the same environment may inherit this profile; the task list shows the actual scope and session.</p>
          ${manualSetup && access() ? `<div class="desktop-relay-setup">
            <h3>One-time setup for ${escape(manualSetup.label)}</h3>
            <p class="settings-sub">This environment JSON includes this profile's admission credential and the public CA path. Copying, dismissing, or leaving Settings clears it here. It cannot be retrieved from status.</p>
            <label>HTTPS_PROXY and NODE_EXTRA_CA_CERTS<textarea readonly rows="6" spellcheck="false" autocomplete="off" data-relay-focus="setup-env">${escape(JSON.stringify(manualSetup.env, null, 2))}</textarea></label>
            <div class="desktop-relay-controls"><button type="button" data-relay-action="copy-setup" data-relay-focus="copy-setup" ${copying ? 'disabled' : ''}>${copying ? 'Copying…' : 'Copy environment JSON'}</button><button type="button" data-relay-action="dismiss-setup" data-relay-focus="dismiss-setup">Dismiss setup</button></div>
          </div>` : ''}
          <div class="desktop-relay-scopes">${(snapshot?.scopes || []).map(scope => `<div class="settings-row" data-relay-scope="${escape(scope.id)}">
            <div><strong>${escape(scope.label || 'Unlabeled profile')}</strong><code tabindex="0" title="${escape(scope.id)}">${escape(scope.id.length > 16 ? scope.id.slice(0, 12) + '…' : scope.id)}</code></div>
            <button type="button" data-relay-action="delete-scope" data-relay-focus="${escape(JSON.stringify([scope.id]) + ':revoke')}" aria-label="Revoke profile ${escape(scope.label || scope.id)}" ${!access() || busy ? 'disabled' : ''}>Revoke profile</button>
          </div>`).join('')}</div>
          <a class="desktop-relay-source" href="https://github.com/00xmario/switcher/blob/main/docs/desktop-reference-map.md" target="_blank" rel="noopener noreferrer">Source comparison</a>
        </div>
      </details>`;
  }
  function responseHTML(response) {
    if (!response) return '<p class="settings-sub desktop-relay-response">No upstream message response recorded yet</p>';
    const account = claudeAccounts().find(account => account.id === response.account_id);
    const activity = desktopRelayActivity(response.at, now());
    return `<p class="settings-sub desktop-relay-response ${response.status >= 400 ? 'desktop-relay-response-failed' : ''}">Last upstream response: ${response.route === 'caller' ? 'Claude caller credential' : `selected-account credential ${escape(account?.email || 'Stored account unavailable')}`}, HTTP ${response.status}${response.status >= 400 ? ' · request failed' : ''}${response.model ? ` · ${escape(response.model)}` : ''} · <time datetime="${activity.iso}" title="${escape(activity.title)}">${escape(activity.label)}</time></p>`;
  }
  function internalSessionsHTML(task) {
    return `<div class="desktop-relay-members">${task.members.map(member => `<div class="desktop-relay-member">
      <code tabindex="0" data-relay-focus="${escape(task.key + ':member:' + member.session_id)}">${escape(member.session_id)}</code>
      ${member.model ? `<p class="settings-sub">Model: ${escape(member.model)}</p>` : ''}
      <p class="settings-sub">${member.account_id ? `Internal session selection: ${escape(claudeAccounts().find(account => account.id === member.account_id)?.email || 'Stored account unavailable')}` : 'Internal session selection: Claude sign-in'}</p>
      ${member.parent_session_id || member.agent_id || member.parent || member.agent ? `<dl>${[
        ['parent_session_id', 'Parent session'], ['agent_id', 'Agent ID'], ['parent', 'Parent'], ['agent', 'Agent'],
      ].map(([field, label]) => member[field] ? `<div><dt>${label}</dt><dd>${escape(member[field])}</dd></div>` : '').join('')}</dl>` : ''}
      ${responseHTML(member.last_response)}
    </div>`).join('')}</div>`;
  }
  function taskHTML(task, display) {
    const scope = snapshot.scopes.find(scope => scope.id === task.scope_id);
    const accounts = claudeAccounts();
    const account = accounts.find(account => account.id === task.account_id);
    const key = task.key;
    const chosen = chosenAccount(task);
    const { title, project, sessionHint, profileHint } = display;
    const identity = `${title}, profile ${scope?.label || task.scope_id}, session ${task.session_id}`;
    const client = ['desktop', 'claude_desktop'].includes(task.client_kind) ? 'Claude Desktop'
      : ['cli', 'claude_code', 'claude_code_cli'].includes(task.client_kind) ? 'Claude Code CLI' : '';
    const worker = task.parent_session_id || task.parent || ['child', 'worker', 'subagent'].includes(task.client_kind);
    const activity = desktopRelayActivity(task.last_seen, now());
    const disabled = !access() || busy;
    const review = choiceReview.has(key);
    const associationUnavailable = !!task.conversation_id && task.association_verified === false;
    const groupValid = !task.conversation_id || uuidID(task.conversation_id) && validRevision(task.revision) && !!task.member_revisions;
    const applyDisabled = disabled || review || !groupValid || !selectionChanged(task, chosen) ||
      (chosen === '' ? !canResetTask(task) : !snapshot.status.listening || associationUnavailable);
    const switching = busy?.task === key;
    const reset = chosen === '' && (task.account_id || task.selection === 'mixed');
    const applyLabel = review ? 'Choose an account' : reset ? 'Use Claude sign-in' : task.selection === 'partial' ? 'Apply to whole conversation' : 'Switch conversation';
    return `<div class="desktop-relay-task" data-relay-scope="${escape(task.scope_id)}" data-relay-session="${escape(task.session_id)}"${task.conversation_id ? ` data-relay-conversation="${escape(task.conversation_id)}"` : ''}>
      <div class="desktop-relay-task-heading"><strong>${escape(title)}</strong><div class="desktop-relay-task-badges">${client ? `<span class="desktop-relay-badge">${client}</span>` : ''}${worker ? '<span class="desktop-relay-badge">Worker</span>' : ''}${review ? '<span class="desktop-relay-badge desktop-relay-partial">Review account choice</span>' : ''}${task.selection === 'partial' ? '<span class="desktop-relay-badge desktop-relay-partial">Partially applied</span>' : task.selection === 'mixed' ? '<span class="desktop-relay-badge desktop-relay-partial">Mixed selections</span>' : task.account_id ? '<span class="desktop-relay-badge desktop-relay-override">Override</span>' : ''}</div></div>
      <p class="desktop-relay-task-meta">${project ? `<span class="desktop-relay-project">${escape(project)}</span>` : ''}${sessionHint ? `<span class="desktop-relay-disambiguator">${task.conversation_id ? 'Conversation' : 'Session'} ${escape(sessionHint)}</span>` : ''}${profileHint ? `<span class="desktop-relay-disambiguator">Profile ${escape(profileHint)}</span>` : ''}<span>${activity ? `Last request <time datetime="${activity.iso}" title="${escape(activity.title)}">${escape(activity.label)}</time>` : 'Activity time unavailable'}</span>${count(task.in_flight) ? `<span class="desktop-relay-responding">${count(task.in_flight)} request${count(task.in_flight) === 1 ? '' : 's'} in progress</span>` : ''}</p>
      <div class="desktop-relay-selection">
        <label class="desktop-relay-account">Account for this conversation
        <select data-relay-action="choose" data-relay-focus="${escape(key)}" aria-label="Account for ${escape(identity)}" ${disabled ? 'disabled' : ''}>
          ${review ? '<option value="__relay_review__" selected disabled>Choose an account again</option>' : ''}
          <option value="" ${!review && chosen === '' ? 'selected' : ''}>Default: Claude sign-in</option>
          ${accounts.map(account => `<option value="${escape(account.id)}" ${chosen === account.id ? 'selected' : ''} ${associationUnavailable ? 'disabled' : ''}>${escape(account.email || account.id)}</option>`).join('')}
        </select>
        </label>
        <button type="button" data-relay-action="use" data-relay-focus="${escape(key + ':use')}" aria-label="${applyLabel} for ${escape(identity)}" aria-busy="${switching}" ${applyDisabled ? 'disabled' : ''}>${switching ? chosen === '' ? 'Restoring sign-in…' : 'Switching…' : applyLabel}</button>
      </div>
      <p class="settings-sub desktop-relay-selected">${task.selection === 'partial' ? `Previous internal session selection: ${escape(account?.email || 'Stored account unavailable')}` : task.selection === 'mixed' ? 'Internal sessions have different account selections' : task.account_id ? `${associationUnavailable ? 'Saved selection' : 'Selected'}: ${escape(account?.email || 'Stored account unavailable')}` : 'Default: Claude sign-in'}</p>
      ${review ? '<p class="settings-sub desktop-relay-selection-help">Internal sessions had different pending account choices. Choose an account again before applying to the whole conversation.</p>' : ''}
      ${associationUnavailable ? '<p class="settings-sub desktop-relay-association-warning">Saved selection; conversation identity unavailable. Restore Claude sign-in or refresh before choosing another account.</p>' : ''}
      ${task.selection === 'partial' ? `<p class="settings-sub desktop-relay-selection-help">${task.legacy_count === 1 ? 'Previous selection covered one internal session. Apply it to the whole conversation.' : 'Previous selections covered internal sessions only. Apply this account to the whole conversation.'}</p>` : task.selection === 'mixed' ? '<p class="settings-sub desktop-relay-selection-help">Choose one account for the whole conversation, or use Claude sign-in to reset all its internal sessions.</p>' : ''}
      ${responseHTML(task.last_response)}
      ${!groupValid ? '<p class="settings-sub">Conversation revision details are incomplete. Refresh status before applying an account.</p>' : ''}
      ${!canResetTask(task) ? `<p class="settings-sub">${!snapshot.status.listening ? 'Start relay in Advanced and refresh status before restoring Claude sign-in for this conversation.' : 'Refresh status to verify this conversation before restoring Claude sign-in.'}</p>` : !snapshot.status.listening ? '<p class="settings-sub">Claude sign-in can be restored while the relay is stopped. Configure Claude Desktop or use Start relay in Advanced before applying a stored account.</p>' : ''}
      <details class="desktop-relay-diagnostics" data-relay-diagnostic="${escape(key)}" ${diagnosticsOpen.has(key) ? 'open' : ''}>
        <summary data-relay-focus="${escape(key + ':diagnostics')}">${task.conversation_id ? `Internal request sessions (${task.members.length})` : 'Conversation details'}</summary>
        <dl><div><dt>Requests</dt><dd>${count(task.requests)}</dd></div>${task.model ? `<div><dt>Model</dt><dd>${escape(task.model)}</dd></div>` : ''}
          <div><dt>${task.conversation_id ? 'Conversation ID' : 'Session ID'}</dt><dd><code tabindex="0" data-relay-focus="${escape(key + ':session')}" title="${escape(task.session_id)}">${escape(task.session_id)}</code></dd></div>
          <div><dt>Profile</dt><dd>${escape(scope?.label || task.scope_id)} <code>${escape(task.scope_id)}</code></dd></div>
          ${task.parent_session_id || task.parent ? `<div><dt>Parent</dt><dd>${escape(task.parent_session_id || task.parent)}</dd></div>` : ''}${task.agent_id || task.agent ? `<div><dt>Agent</dt><dd>${escape(task.agent_id || task.agent)}</dd></div>` : ''}
          ${task.title_source ? `<div><dt>Title source</dt><dd>${escape(task.title_source)}</dd></div>` : ''}${task.client_kind ? `<div><dt>Client kind</dt><dd>${escape(task.client_kind)}</dd></div>` : ''}
        </dl>
        ${task.conversation_id ? internalSessionsHTML(task) : ''}
      </details>
    </div>`;
  }
  function render() {
    captureDetails();
    const doc = view?.ownerDocument;
    const focused = view?.contains(doc?.activeElement) ? doc.activeElement : null;
    const focusedKey = focused?.dataset.relayFocus;
    const focusKey = focusRekey && focusRekey.from === focusedKey ? focusRekey.to : focusedKey || focusRestore?.key;
    if (taskViews().some(task => !count(task.in_flight) &&
      ([task.key, ...['use', 'diagnostics', 'session'].map(suffix => task.key + ':' + suffix)].includes(focusKey) || focusKey?.startsWith(task.key + ':member:')))) recentOpen = true;
    if (focusKey === 'restart-desktop' || focusRestore && ['configure', 'restore-setup'].includes(focusKey) && !busy && !reading) detailsOpen = true;
    const markup = html();
    if (view && (renderedMarkup !== markup || focusRestore)) {
      if (focusRestore && !focused && doc?.activeElement && doc.activeElement !== doc.body) focusRestore = null;
      const focus = focusedKey ? { key: focusKey,
        start: focused.selectionStart, end: focused.selectionEnd } : focusRestore;
      view.innerHTML = markup;
      renderedMarkup = markup;
      if (focus) {
        const replacement = [...view.querySelectorAll('[data-relay-focus]')].find(node => node.dataset.relayFocus === focus.key);
        if (replacement && !replacement.disabled) {
          replacement.focus({ preventScroll: true });
          if (Number.isInteger(focus.start)) replacement.setSelectionRange?.(focus.start, focus.end);
          focusRestore = null;
        } else if (focus.key.endsWith(':use') && !busy && !reading) {
          const next = [...view.querySelectorAll('[data-relay-focus]')].find(node => node.dataset.relayFocus === focus.key.slice(0, -4) && !node.disabled);
          next?.focus({ preventScroll: true });
          focusRestore = null;
        } else if (['configure', 'restore-setup'].includes(focus.key) && !busy && !reading) {
          const key = canRestart() ? 'restart-desktop' : canConfigure() ? 'configure' : 'refresh';
          const next = [...view.querySelectorAll('[data-relay-focus]')].find(node => node.dataset.relayFocus === key && !node.disabled);
          next?.focus({ preventScroll: true });
          focusRestore = null;
        } else focusRestore = busy || reading || copying ? focus : null;
      }
    }
    focusRekey = null;
    paint(markup);
  }
  function captureDetails() {
    const advanced = view?.querySelector?.('[data-relay-advanced]');
    const details = view?.querySelector?.('[data-relay-details]');
    const recent = view?.querySelector?.('[data-relay-recent]');
    if (advanced) advancedOpen = advanced.open;
    if (details) detailsOpen = details.open;
    if (recent) recentOpen = recent.open;
    for (const detail of view?.querySelectorAll?.('[data-relay-diagnostic]') || []) {
      if (!detail.dataset?.relayDiagnostic || !taskViews().some(task => task.key === detail.dataset.relayDiagnostic)) continue;
      if (detail.open) diagnosticsOpen.add(detail.dataset.relayDiagnostic);
      else diagnosticsOpen.delete(detail.dataset.relayDiagnostic);
    }
  }
  function refresh() {
    return busy ? Promise.resolve(false) : load();
  }
  function update() {
    if (!active || getContext().locked || getContext().loggingOut) return;
    render();
    if (!attempted) return load();
  }
  async function load() {
    if (!access()) return false;
    if (reading) return reading.promise;
    if (!busy || busy.epoch !== epoch) { failed = false; message = ''; }
    const token = { epoch, generation, controller: new AbortController() };
    attempted = true;
    reading = token;
    render();
    token.promise = (async () => {
      try {
        const context = getContext();
        const body = await api(base, { method: 'GET', cache: 'no-store', signal: token.controller.signal,
          controllerKey: context.authKind ? undefined : context.controllerKey });
        if (!access() || token.epoch !== epoch || token.generation !== generation) return false;
        const previous = taskViews();
        const next = { status: {
          enabled: body.status.enabled === true, listening: body.status.listening === true,
          address: body.status.address, ca_path: body.status.ca_path,
          in_flight: count(body.status.in_flight),
          condition: body.status.condition, validation: body.status.validation,
        }, setup: setupStatus(body.setup), scopes: (body.scopes || []).map(scope => ({ id: String(scope.id), label: String(scope.label || '') })),
        sessions: (body.sessions || []).map(task => ({
          scope_id: String(task.scope_id), session_id: String(task.session_id), account_id: task.account_id || '',
          revision: task.revision, last_seen: task.last_seen, requests: task.requests, in_flight: task.in_flight,
          model: task.model, parent: task.parent, agent: task.agent, parent_session_id: task.parent_session_id, agent_id: task.agent_id,
          title: text(task.title), project: text(task.project), title_source: text(task.title_source), client_kind: text(task.client_kind),
          conversation_id: text(task.conversation_id).trim() ? text(task.conversation_id) : '', last_response: responseEvidence(task.last_response),
          association_verified: task.association_verified !== false,
        })), conversation_bindings: (body.conversation_bindings || []).map(binding => ({
          scope_id: text(binding.scope_id), conversation_id: text(binding.conversation_id),
          account_id: text(binding.account_id), revision: binding.revision,
        })) };
        next.views = conversationViews(next.sessions, next.conversation_bindings);
        reconcileViews(previous, next.views);
        snapshot = next;
        return true;
      } catch (error) {
        if (access() && token.epoch === epoch && token.generation === generation) {
          failed = true;
          message = `${busy && message ? message + ' ' : ''}${safeMessage(error.body?.error?.message || error.body?.error) || 'Could not load the Desktop relay. Refresh to try again.'}`;
        }
        return false;
      } finally {
        if (reading === token) reading = null;
        if (access() && token.epoch === epoch && token.generation === generation) render();
      }
    })();
    return token.promise;
  }
  async function mutate(path, method, body, operation) {
    if (!access() || busy) return false;
    const token = { ...operation, epoch, controller: new AbortController() };
    busy = token;
    generation++;
    reading?.controller.abort();
    reading = null;
    failed = false;
    message = '';
    render();
    try {
      if (operation.before && !await operation.before()) return false;
      if (!access() || token.epoch !== epoch) return false;
      const context = getContext();
      const result = await api(path, { method, cache: 'no-store', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body), signal: token.controller.signal, controllerKey: context.authKind ? undefined : context.controllerKey });
      if (!access() || token.epoch !== epoch) return false;
      if (operation.task) choices.delete(operation.task);
      if (operation.kind === 'create') {
        if (typeof result.id !== 'string' || typeof result.env?.HTTPS_PROXY !== 'string' || typeof result.env?.NODE_EXTRA_CA_CERTS !== 'string') throw new Error('Invalid setup response');
        manualSetup = { id: result.id, label: String(result.label || body.label),
          env: { HTTPS_PROXY: result.env.HTTPS_PROXY, NODE_EXTRA_CA_CERTS: result.env.NODE_EXTRA_CA_CERTS } };
        profileLabel = '';
      }
      if ((operation.kind === 'delete' && manualSetup?.id === operation.scope) || ['stop', 'config', 'restore'].includes(operation.kind)) manualSetup = null;
      message = operation.kind === 'config' ? 'Configured. Restart Claude Desktop when current work is finished.'
        : operation.kind === 'restore' ? 'Restored previous settings. Restart Desktop when work is finished.'
        : operation.kind === 'restart' ? 'Restart requested. Refresh status to check setup.'
        : operation.task ? 'Conversation selection saved. New requests use this selection. Send a message and refresh to check the upstream response.' : operation.kind === 'create'
        ? 'Profile created. Copy the one-time setup before leaving Settings.' : operation.kind === 'delete'
          ? 'Profile revoked. Its admission credential and task bindings are removed.' : operation.kind === 'stop'
            ? 'Relay stopped.' : 'Relay started. Create a profile for a future task.';
      await load();
      return true;
    } catch (error) {
      if (access() && token.epoch === epoch) {
        failed = true;
        const setupOperation = ['config', 'restore', 'restart'].includes(operation.kind);
        const ownedSetupConflict = error.status === 409 && error.body?.error_code === 'setup_owned' &&
          ['stop', 'delete'].includes(operation.kind);
        const managedDelete = operation.kind === 'delete' && error.status === 409 && operation.scope === snapshot?.setup.scope_id;
        const detail = setupOperation || ownedSetupConflict || managedDelete ? safeMessage(error.body?.error?.message || error.body?.error) : '';
        message = ownedSetupConflict || managedDelete ? `${detail ? detail + ' ' : ''}Remove Desktop setup before stopping relay or revoking its profile. The request was not retried.`
          : setupOperation ? `${detail || `Could not ${{ config: 'configure Claude Desktop', restore: 'remove Desktop setup', restart: 'request a Desktop restart' }[operation.kind]}.`}${error.status === 404 ? ' Update Switcher to a version with Desktop setup, then try again.' : ' Refresh status before trying again. The request was not retried.'}`
          : error.status === 409 ? operation.kind === 'stop'
          ? 'Relay is busy. Let active streams or shutdown finish, then refresh status. Stop was not retried.'
          : operation.kind === 'start' ? 'Relay is busy while shutdown finishes. Refresh status, then start again. Start was not retried.'
          : operation.task ? 'The conversation generation changed. Review its latest selection before trying again. The switch was not retried.'
            : 'The relay state changed. Review the refreshed list before trying again.'
          : `Could not ${operation.task ? 'change the conversation account' : ({ create: 'create the profile', delete: 'revoke the profile', stop: 'stop the relay' })[operation.kind] || 'start the relay'}. Refresh status before trying again.`;
        if (error.status === 409) {
          if (operation.task) choices.delete(operation.task);
          await load();
        }
      }
      return false;
    } finally {
      const released = busy === token;
      if (released) busy = null;
      if (access() && (token.epoch === epoch || released)) render();
    }
  }
  function useTask(scope, session, accountID, conversationID) {
    const task = lookupTask(scope, session, conversationID);
    if (typeof accountID !== 'string' || !task || !validRevision(task.revision) || task.conversation_id && (!uuidID(task.conversation_id) || !task.member_revisions) ||
      choiceReview.has(task.key) || accountID === '' && !canResetTask(task) ||
      accountID && (task.conversation_id && task.association_verified === false || !snapshot.status.listening || !claudeAccounts().some(account => account.id === accountID))) return Promise.resolve(false);
    const path = `${base}/scopes/${encodeURIComponent(scope)}/${task.conversation_id ? 'conversations' : 'sessions'}/${encodeURIComponent(task.session_id)}/account`;
    const body = { ...(accountID ? { account_id: accountID } : {}), revision: task.revision,
      ...(task.conversation_id ? { member_revisions: task.member_revisions } : {}) };
    return mutate(path, accountID ? 'POST' : 'DELETE', body, { task: task.key });
  }
  function chooseAccount(scope, session, accountID, conversationID) {
    const task = lookupTask(scope, session, conversationID);
    if (typeof accountID !== 'string' || !access() || busy || !task ||
      accountID && (task.conversation_id && task.association_verified === false || !claudeAccounts().some(account => account.id === accountID))) return false;
    choices.set(task.key, accountID);
    choiceReview.delete(task.key);
    render();
    return true;
  }
  function mount(root, focusKey) {
    captureDetails();
    if (root !== view) renderedMarkup = null;
    view = root;
    if (focusKey) focusRestore = { key: focusKey };
    render();
    if (!mounted.has(root)) {
      mounted.add(root);
      root.addEventListener('click', event => {
        if (root !== view || !access()) return;
        const button = event.target.closest('button[data-relay-action]');
        if (!button || !root.contains(button) || button.disabled) return;
        const row = button.closest('[data-relay-scope]');
        switch (button.dataset.relayAction) {
          case 'configure': return configure();
          case 'restore-setup': return restoreSetup();
          case 'restart-desktop': return restartDesktop();
          case 'refresh': return refresh();
          case 'start': return start();
          case 'stop': return stop();
          case 'copy-setup': return copySetup();
          case 'dismiss-setup': return dismissSetup();
          case 'delete-scope': return row && deleteScope(row.dataset.relayScope);
          case 'use': {
            const task = lookupTask(row?.dataset.relayScope, row?.dataset.relaySession, row?.dataset.relayConversation);
            if (!task) return;
            return useTask(task.scope_id, task.session_id, chosenAccount(task), task.conversation_id || '');
          }
        }
      });
      root.addEventListener('change', event => {
        if (root !== view) return;
        const select = event.target.closest('select[data-relay-action="choose"]');
        const row = select?.closest('.desktop-relay-task');
        if (row && root.contains(select)) return chooseAccount(row.dataset.relayScope, row.dataset.relaySession, select.value, row.dataset.relayConversation);
      });
      root.addEventListener('input', event => {
        if (root === view && access() && event.target.name === 'relay-profile-label') profileLabel = event.target.value;
      });
      root.addEventListener('submit', event => {
        if (!event.target.matches('[data-relay-profile-form]')) return;
        event.preventDefault();
        if (root === view) return createScope(event.target.elements?.['relay-profile-label']?.value ?? profileLabel);
      });
      root.addEventListener('toggle', event => {
        if (root !== view || !root.contains(event.target)) return;
        if (event.target.matches?.('[data-relay-advanced]')) advancedOpen = event.target.open;
        if (event.target.matches?.('[data-relay-details]')) detailsOpen = event.target.open;
        if (event.target.matches?.('[data-relay-recent]')) recentOpen = event.target.open;
        if (event.target.matches?.('[data-relay-diagnostic]')) {
          if (event.target.open) diagnosticsOpen.add(event.target.dataset.relayDiagnostic);
          else diagnosticsOpen.delete(event.target.dataset.relayDiagnostic);
        }
      }, true);
    }
    return setActive(true);
  }
  function configure() {
    if (!access() || busy || !canConfigure()) return Promise.resolve(false);
    manualSetup = null;
    return mutate(`${base}/configure`, 'POST', {}, { kind: 'config' });
  }
  function restoreSetup() {
    if (!access() || busy || !canRestore()) return Promise.resolve(false);
    manualSetup = null;
    return mutate(`${base}/restore`, 'POST', {}, { kind: 'restore' });
  }
  function restartDesktop() {
    if (!canRestart()) return Promise.resolve(false);
    return mutate(`${base}/restart-desktop`, 'POST', { confirmed: true }, { kind: 'restart',
      before: async () => await confirmRestart() === true });
  }
  function start() {
    if (!snapshot || snapshot.status.listening) return Promise.resolve(false);
    return mutate(`${base}/start`, 'POST', {}, { kind: 'start' });
  }
  function createScope(label) {
    if (typeof label !== 'string' || !access() || busy || !snapshot?.status.listening || !label.trim()) return Promise.resolve(false);
    manualSetup = null;
    return mutate(`${base}/scopes`, 'POST', { label: String(label).trim().slice(0, 120) }, { kind: 'create' });
  }
  function deleteScope(scope) {
    if (!snapshot?.scopes.some(item => item.id === scope)) return Promise.resolve(false);
    return mutate(`${base}/scopes/${encodeURIComponent(scope)}`, 'DELETE', {}, { kind: 'delete', scope });
  }
  function stop() {
    if (!snapshot?.status.enabled) return Promise.resolve(false);
    const counts = activeCounts();
    return mutate(`${base}/stop`, 'POST', {}, { kind: 'stop',
      before: counts.inFlight > 0 ? () => confirmStop(counts) : null });
  }
  function dismissSetup() {
    manualSetup = null;
    render();
  }
  async function copySetup() {
    if (!access() || !manualSetup || copying) return false;
    const captured = manualSetup, version = epoch;
    copying = true;
    render();
    try {
      await copy(JSON.stringify(captured.env, null, 2));
      if (!access() || epoch !== version || manualSetup !== captured) return false;
      manualSetup = null;
      failed = false;
      message = 'Environment JSON copied. The one-time setup has been cleared from this page.';
      return true;
    } catch {
      if (access() && epoch === version && manualSetup === captured) {
        failed = true;
        message = 'Could not copy. Select the environment JSON and copy it manually, then dismiss setup.';
      }
      return false;
    } finally {
      copying = false;
      if (active && epoch === version) render();
    }
  }
  function setActive(next) {
    const context = getContext();
    if (next && (context.locked || context.loggingOut)) return;
    if (active === next) return reading?.promise;
    active = next;
    epoch++;
    attempted = false;
    manualSetup = null;
    reading?.controller.abort();
    reading = null;
    if (!next) {
      busy?.controller.abort();
      focusRestore = null;
      render();
    }
    return next ? load() : undefined;
  }
  function clear() {
    active = false;
    epoch++;
    generation++;
    reading?.controller.abort();
    busy?.controller.abort();
    reading = snapshot = manualSetup = null;
    choices.clear();
    choiceReview.clear();
    diagnosticsOpen.clear();
    profileLabel = message = '';
    renderedMarkup = focusRestore = focusRekey = null;
    failed = false;
    attempted = false;
    advancedOpen = detailsOpen = recentOpen = false;
    if (view) view.replaceChildren();
    view = null;
  }
  return { html, mount, update, refresh, setActive, clear, chooseAccount, useTask, configure, restoreSetup, restartDesktop, start, stop, createScope, deleteScope, copySetup, dismissSetup };
}
