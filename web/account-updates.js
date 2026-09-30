// Keep stable account/header nodes across state polls. Health timestamps
// that don't change rendered markup must not close menus or interrupt motion.
const rendered = new WeakMap();
function remember(node) { rendered.set(node, node.outerHTML); return node; }
function place(parent, node, index) {
  if (parent.children[index] !== node) parent.insertBefore(node, parent.children[index] || null);
}
export function patchProviderList(container, html) {
  const focus = document.activeElement;
  const template = document.createElement('template');
  template.innerHTML = html;
  const wanted = [...template.content.children];
  const existing = new Map([...container.children].map(node => [node.dataset.provider, node]));
  wanted.forEach((section, index) => {
    const old = existing.get(section.dataset.provider);
    existing.delete(section.dataset.provider);
    if (!old) {
      for (const node of section.querySelectorAll('.account, .provider-head')) remember(node);
      place(container, section, index);
      return;
    }
    old.className = section.className;
    const head = section.querySelector('.provider-head');
    const oldHead = old.querySelector('.provider-head');
    if (rendered.get(oldHead) !== head.outerHTML) {
      const sync = oldHead.querySelector('.claude-sync-control');
      remember(head);
      if (sync) head.querySelector('.claude-sync-control')?.replaceWith(sync);
      oldHead.replaceWith(head);
    }
    const list = old.querySelector('.account-list');
    const nextList = section.querySelector('.account-list');
    list.className = nextList.className;
    const cards = [...nextList.querySelectorAll('.account')];
    if (!cards.length) {
      if (list.innerHTML !== nextList.innerHTML) list.innerHTML = nextList.innerHTML;
    } else {
      const current = new Map([...list.querySelectorAll('.account')].map(node => [node.dataset.id, node]));
      for (const child of [...list.children]) if (!child.matches('.account')) child.remove();
      cards.forEach((card, position) => {
        const previous = current.get(card.dataset.id);
        current.delete(card.dataset.id);
        let node = previous;
        if (!previous || rendered.get(previous) !== card.outerHTML) {
          remember(card);
          if (previous?.querySelector('.email.revealed')) card.querySelector('.email')?.classList.add('revealed');
          if (previous) previous.replaceWith(card);
          node = card;
        }
        place(list, node, position);
      });
      for (const node of current.values()) node.remove();
    }
    place(container, old, index);
  });
  for (const node of existing.values()) node.remove();
  if (focus?.isConnected && container.contains(focus) && document.activeElement !== focus) focus.focus({ preventScroll: true });
}

export function quotaLevels(account) {
  return new Map((account?.usage?.windows || []).map(w => [w.label, Math.max(0, Math.min(100, 100 - w.used_percent))]));
}

// Only a new, confirmed redemption creates a celebration. Initial page
// loads and already-redeemed/failure responses must not replay old events.
export function newlyRedeemed(previous, next) {
  const old = new Map(previous.map(a => [a.id, a]));
  return next.filter(a => old.has(a.id) && a.last_reset?.outcome === 'reset' &&
    a.last_reset.id !== old.get(a.id).last_reset?.id);
}

export function mergeAccountMutation(current, incoming) {
  if (current && current.quota_epoch === incoming.quota_epoch &&
      (current.quota_revision || 0) > (incoming.quota_revision || 0)) return current;
  return incoming;
}

export function createResetFeedback({ getCard, reducedMotion = () => matchMedia('(prefers-reduced-motion: reduce)').matches, schedule = setTimeout, now = Date.now }) {
  const sending = new Set(), effects = new Map();
  function clear(id) {
    const card = getCard(id);
    card?.classList.remove('reset-sending', 'reset-celebrate');
    card?.querySelector('.reset-feedback')?.remove();
    card?.querySelector('.account-health')?.classList.remove('reset-status-host');
  }
  function notice(card, message) {
    let node = card.querySelector('.reset-feedback');
    if (!node) {
      node = document.createElement('span'); node.className = 'reset-feedback'; node.setAttribute('role', 'status');
      const host = card.querySelector('.account-health') || card;
      host.classList.add('reset-status-host'); host.appendChild(node);
    }
    node.textContent = message;
  }
  function begin(id) {
    if (sending.has(id)) return false;
    sending.add(id);
    const card = getCard(id);
    if (card) { card.classList.add('reset-sending'); notice(card, 'Using banked reset…'); }
    return true;
  }
  function end(id) {
    sending.delete(id);
    getCard(id)?.classList.remove('reset-sending');
    if (!effects.has(id)) clear(id);
  }
  function observe(previous, next) {
    const old = new Map(previous.map(a => [a.id, a]));
    for (const account of newlyRedeemed(previous, next)) {
      const effect = { id: account.last_reset.id, until: now() + 45000, started: now(), levels: quotaLevels(old.get(account.id)), fills: new Map() };
      effects.set(account.id, effect);
      schedule(() => { if (effects.get(account.id) === effect) { effects.delete(account.id); clear(account.id); } }, 45000);
    }
    for (const account of next) {
      const card = getCard(account.id), effect = effects.get(account.id);
      if (!card) continue;
      if (sending.has(account.id) && !effect) { card.classList.add('reset-sending'); notice(card, 'Using banked reset…'); }
      if (!effect || now() > effect.until) continue;
      notice(card, account.last_reset?.pending ? '✓ Banked reset used · refreshing quota…'
        : account.last_reset?.usage_confirmed === false ? '✓ Reset used · quota not updated' : '✓ Banked reset used');
      const elapsed = now() - effect.started;
      if (elapsed < 1600 && !card.classList.contains('reset-celebrate')) {
        card.style.setProperty('--reset-elapsed', `${-elapsed}ms`);
        card.classList.add('reset-celebrate');
        schedule(() => card.classList.remove('reset-celebrate'), 1600 - elapsed);
      }
      for (const [label, value] of quotaLevels(account)) {
        const before = effect.levels.get(label);
        const window = [...card.querySelectorAll('[data-quota-window]')].find(node => node.dataset.quotaWindow === label);
        if (!window) continue;
        let fill = effect.fills.get(label);
        if (fill && now() - fill.started >= 1100) { effect.fills.delete(label); fill = null; }
        if (!fill && before !== undefined && value > before) {
          fill = { from: before, to: value, started: now() };
          effect.fills.set(label, fill);
        }
        effect.levels.set(label, value);
        if (!fill) continue;
        const node = window.querySelector('.fill, .compact-fill');
        if (fill.node === node && fill.to === value) continue;
        fill.to = value;
        fill.animation?.cancel(); fill.hatchAnimation?.cancel();
        fill.node = node;
        if (!reducedMotion()) {
          fill.animation = node?.animate(
            [{ width: `${fill.from}%` }, { width: `${value}%` }], { duration: 1100, easing: 'cubic-bezier(.16,1,.3,1)' });
          fill.hatchAnimation = window.querySelector('.hatch')?.animate(
            [{ width: `${100-fill.from}%` }, { width: `${100-value}%` }], { duration: 1100, easing: 'cubic-bezier(.16,1,.3,1)' });
          // A health/countdown update may replace the card mid-animation.
          // Continue at the original timeline position instead of dropping
          // or restarting the refill.
          if (fill.animation) fill.animation.currentTime = now() - fill.started;
          if (fill.hatchAnimation) fill.hatchAnimation.currentTime = now() - fill.started;
        }
      }
      if (!account.last_reset?.pending && !effect.finishing) {
        effect.finishing = true;
        schedule(() => { if (effects.get(account.id) === effect) { effects.delete(account.id); clear(account.id); } }, 5000);
      }
    }
    const present = new Set(next.map(a => a.id));
    for (const id of effects.keys()) if (!present.has(id)) { effects.delete(id); sending.delete(id); }
  }
  return { begin, end, observe, repaint: accounts => observe(accounts, accounts) };
}
