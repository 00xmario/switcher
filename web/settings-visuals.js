// Settings navigation and the small illustrations that explain each section.
// Pure markup: app.js wires the controls.
import { switcherMark } from './brand.js';

const icon = body => `<svg viewBox="0 0 24 24" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">${body}</svg>`;

export const ICONS = {
  sliders: icon('<path d="M4 7h10M18 7h2M4 17h4M12 17h8"/><circle cx="16" cy="7" r="2.2"/><circle cx="10" cy="17" r="2.2"/>'),
  desktop: icon('<rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4"/><path d="M12 7.2v5.6M9.6 8.6l4.8 2.8M14.4 8.6l-4.8 2.8"/>'),
  phone: icon('<rect x="6.5" y="2.5" width="11" height="19" rx="2.6"/><path d="M10.5 18.5h3"/>'),
  macs: icon('<rect x="2.5" y="5" width="9" height="7" rx="1.4"/><path d="M1.5 15h11"/><rect x="12.5" y="9" width="9" height="7" rx="1.4"/><path d="M11.5 19h11"/>'),
  terminal: icon('<rect x="3" y="4" width="18" height="16" rx="2.5"/><path d="M7 10l3 2.5L7 15M12.5 15H17"/>'),
  lock: icon('<rect x="5" y="10.5" width="14" height="10" rx="2.2"/><path d="M8.5 10.5V8a3.5 3.5 0 0 1 7 0v2.5"/><path d="M12 14.5v2.5"/>'),
  shield: icon('<path d="M12 3l7 3v5.5c0 4.3-2.9 7.7-7 9.5-4.1-1.8-7-5.2-7-9.5V6z"/><path d="M9 12l2.2 2.2L15.5 10"/>'),
  globe: icon('<circle cx="12" cy="12" r="8.5"/><path d="M3.5 12h17M12 3.5c2.4 2.6 3.5 5.5 3.5 8.5s-1.1 5.9-3.5 8.5c-2.4-2.6-3.5-5.5-3.5-8.5s1.1-5.9 3.5-8.5z"/>'),
  eye: icon('<path d="M2.5 12S6 5.5 12 5.5 21.5 12 21.5 12 18 18.5 12 18.5 2.5 12 2.5 12z"/><circle cx="12" cy="12" r="2.8"/><path d="M4 20L20 4"/>'),
  key: icon('<circle cx="8" cy="15" r="4"/><path d="M11 12l8.5-8.5M16 7l2.5 2.5M14 9l2 2"/>'),
  empty: icon('<circle cx="12" cy="12" r="8.5"/><path d="M8 12h8"/>'),
  bolt: icon('<path d="M13 2.5L5 13.5h6.5L10.5 21.5 19 10h-6.5z"/>'),
  pass: icon('<path d="M4 12h12M12 6l6 6-6 6"/>'),
  check: icon('<path d="M5 12.5l4.5 4.5L19 7.5"/>'),
};

export const SECTIONS = [
  { id: 'general', title: 'General', sub: 'Appearance, how accounts are laid out, and the menu bar.', tile: 'slate', icon: 'sliders' },
  { id: 'switching', title: 'Switching', sub: 'Claude Code’s login, and what happens when an account runs out.', tile: 'brand', icon: 'mark' },
  { id: 'desktop', title: 'Claude Desktop', sub: 'Pick an account per Desktop conversation.', tile: 'clay', icon: 'desktop' },
  { id: 'sharing', title: 'Share between Macs', sub: 'Use one Mac’s accounts on your other Macs, at home or anywhere.', tile: 'blue', icon: 'macs' },
  { id: 'phone', title: 'Phone', sub: 'Usage, switching and banked resets on your phone, over Tailscale.', tile: 'teal', icon: 'phone' },
  { id: 'tools', title: 'Command line & tools', sub: 'The switcher command, CLI setup and the T3 Code hub.', tile: 'graphite', icon: 'terminal' },
  { id: 'security', title: 'Privacy & security', sub: 'Who can reach this Switcher, and with what.', tile: 'indigo', icon: 'lock' },
  { id: 'about', title: 'About', sub: 'Version, updates and links.', tile: 'app', icon: 'app' },
];

export function sectionTile(section, size = 'small') {
  if (section.icon === 'app') return `<span class="set-tile tile-app is-${size}">${switcherMark({ tile: true, size: size === 'large' ? 44 : 26 })}</span>`;
  const glyph = section.icon === 'mark' ? switcherMark({ size: size === 'large' ? 28 : 17 }) : ICONS[section.icon];
  return `<span class="set-tile tile-${section.tile} is-${size}">${glyph}</span>`;
}

export function navHTML(current) {
  return `<nav class="set-nav" aria-label="Settings sections">
    ${SECTIONS.map(s => `<button type="button" class="set-nav-item" data-section="${s.id}" ${s.id === current ? 'aria-current="page"' : ''}>
      ${sectionTile(s)}<span>${s.title}</span></button>`).join('')}
  </nav>`;
}

export function sectionHeadHTML(section, extra = '') {
  return `<header class="set-head">${sectionTile(section, 'large')}
    <div><h2 id="set-title-${section.id}">${section.title}</h2><p>${section.sub}</p></div>${extra}</header>`;
}

// accountLayout maps the two stored flags to the three layouts.
export function accountLayout(state) {
  if (state?.merge_accounts) return 'merged';
  return state?.compact_accounts ? 'compact' : 'expanded';
}

export const LAYOUTS = {
  expanded: { compact_accounts: false, merge_accounts: false },
  compact: { compact_accounts: true, merge_accounts: false },
  merged: { compact_accounts: false, merge_accounts: true },
};

export function layoutChooserHTML(layout) {
  const bars = n => Array.from({ length: n }, (_, i) => `<i style="--w:${[78, 52, 64][i % 3]}%"></i>`).join('');
  const options = [
    { id: 'expanded', title: 'Expanded', sub: 'One account per card, every window in full', art: `<div class="lay-card">${bars(3)}</div>` },
    { id: 'compact', title: 'Side by side', sub: 'Compact cards next to each other', art: `<div class="lay-pair"><div class="lay-card">${bars(2)}</div><div class="lay-card">${bars(2)}</div></div>` },
    { id: 'merged', title: 'Merged', sub: 'One card per window, every account in it', art: `<div class="lay-card lay-merged">${[0, 1, 2].map(i => `<b><i style="--w:${[70, 45, 58][i]}%"></i><i style="--w:${[30, 62, 20][i]}%"></i></b>`).join('')}</div>` },
  ];
  return `<div class="lay-options" role="radiogroup" aria-label="Account layout">
    ${options.map(o => `<button type="button" class="lay-option" role="radio" aria-checked="${o.id === layout}" data-account-layout="${o.id}">
      <span class="lay-art" aria-hidden="true">${o.art}</span>
      <span class="lay-text"><strong>${o.title}</strong><span>${o.sub}</span></span>
    </button>`).join('')}
  </div>`;
}

// menuPreviewHTML sketches the menu bar dropdown with or without usage bars.
export function menuPreviewHTML(bars) {
  const row = (label, left) => `<div class="mp-row"><span class="mp-label">${label}</span><span class="mp-bar"><i style="--w:${left}%"></i></span><span class="mp-left">${left}% left</span></div>`;
  return `<div class="menu-preview ${bars ? '' : 'no-bars'}" aria-hidden="true">
    <div class="mp-strip"><span class="mp-icon">${switcherMark({ mono: true, size: 13 })}</span><span>9:41</span></div>
    <div class="mp-menu">
      <div class="mp-account"><span class="mp-email"></span><b>Max 20x</b></div>
      ${row('Session', 66)}${row('Weekly', 39)}
    </div>
  </div>`;
}

// ladderHTML shows what happens, in order, when the active account runs out.
export function ladderHTML(autoReset) {
  const steps = [
    { icon: ICONS.empty, title: 'An account runs out', sub: 'The provider reports it is out of usage.' },
    { icon: switcherMark({ size: 19 }), title: 'Switcher moves on', sub: 'Another paid account takes the request.', key: 'move' },
    { icon: ICONS.bolt, title: 'A banked reset', sub: 'When no paid account is left, before a Free one.', key: 'reset', off: !autoReset },
    { icon: ICONS.pass, title: 'Free, then the limit', sub: 'A Free account if you have one, then the provider’s own message.' },
  ];
  return `<ol class="ladder">${steps.map((s, i) => `<li class="ladder-step ${s.key ? `is-${s.key}` : ''} ${s.off ? 'is-off' : ''}" ${s.key === 'reset' ? 'data-ladder-reset' : ''}>
    <span class="ladder-icon">${s.icon}</span><span class="ladder-n">${i + 1}</span>
    <strong>${s.title}</strong><span>${s.sub}</span>
    ${s.key === 'reset' ? `<em class="ladder-state">${autoReset ? 'On' : 'Off'}</em>` : ''}
  </li>`).join('')}</ol>`;
}

// securityHeroHTML summarizes who can reach this Switcher. Browsers on
// another device get a reduced status and no summary.
export function securityHeroHTML(status, sharing = false) {
  if (!('lan_active' in status)) return '';
  const lan = !!status.lan_active;
  const password = !!status.auth_enabled;
  const reach = lan ? { icon: ICONS.globe, title: 'Your network', sub: 'The dashboard is on the LAN, over TLS' }
    : sharing ? { icon: ICONS.shield, title: 'This Mac and paired Macs', sub: 'Sharing accepts only Macs you paired' }
      : { icon: ICONS.shield, title: 'This Mac only', sub: 'The dashboard listens on localhost' };
  const tiles = [
    { ...reach, ok: !lan || password },
    { icon: ICONS.lock, title: password ? 'Password on' : 'No password', sub: password ? `${status.sessions || 0} signed-in browser${status.sessions === 1 ? '' : 's'}` : 'Fine while only this Mac reaches the dashboard', ok: password || !lan },
    { icon: ICONS.eye, title: 'Prompts stay private', sub: 'Switcher never inspects prompts', ok: true },
  ];
  return `<div class="sec-hero">${tiles.map(t => `<div class="sec-tile ${t.ok ? 'is-ok' : 'is-warn'}"><span class="sec-icon">${t.icon}</span><strong>${t.title}</strong><span>${t.sub}</span></div>`).join('')}</div>`;
}
