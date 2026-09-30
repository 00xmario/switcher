// Appearance is local to this browser, like the existing light/dark choice.
// Only validated colors and enumerated styles become CSS variables.
export const THEME_STORAGE_KEY = 'switcher-appearance-v1';
export const THEME_PROVIDERS = ['codex', 'claude', 'grok', 'opencode', 'antigravity', 'gemini', 'copilot'];
const providerNames = { codex: 'Codex', claude: 'Claude', grok: 'Grok', opencode: 'OpenCode', antigravity: 'Antigravity', gemini: 'Gemini', copilot: 'Copilot' };
const bars = colors => Object.fromEntries(THEME_PROVIDERS.map((id, i) => [id, colors[i]]));
export const THEME_PRESETS = [
  { id: 'graphite', name: 'Graphite', description: 'Cool charcoal, quiet mint', corners: 'soft', buttons: 'outline',
    light: { page: '#f5f6f8', surface: '#ffffff', accent: '#267653', bars: bars(['#368361', '#b76b4c', '#626c7c', '#478887', '#567c63', '#5279b8', '#6c7999']) },
    dark: { page: '#101318', surface: '#1a1f26', accent: '#7bd1a6', bars: bars(['#73bd99', '#df9e80', '#a8b3c6', '#74bcb9', '#9ac096', '#89b0ed', '#a2b3dc']) } },
  { id: 'tide', name: 'Tide', description: 'Deep blue, clean edges', corners: 'round', buttons: 'filled',
    light: { page: '#f0f5fb', surface: '#ffffff', accent: '#285d9c', bars: bars(['#477dbd', '#b2795d', '#677e9d', '#358a98', '#43896f', '#607fc3', '#587baf']) },
    dark: { page: '#0c1623', surface: '#142336', accent: '#83b9ef', bars: bars(['#82b4ee', '#e0b093', '#a4b9d5', '#6fc2d1', '#80c8ac', '#a3b3f0', '#83b1ce']) } },
  { id: 'ember', name: 'Ember', description: 'Warm stone, copper details', corners: 'square', buttons: 'outline',
    light: { page: '#f7f3ee', surface: '#fffdf9', accent: '#9c512d', bars: bars(['#a26c48', '#b3663f', '#887262', '#6d8674', '#778855', '#687d9e', '#94736e']) },
    dark: { page: '#181411', surface: '#251e19', accent: '#e8ad7d', bars: bars(['#d2a67b', '#e4a078', '#bdab97', '#a4b8a0', '#bdc092', '#a6b6d0', '#c2a09a']) } },
];

const isColor = value => typeof value === 'string' && /^#[\da-f]{6}$/i.test(value);
const rgb = value => [1, 3, 5].map(i => parseInt(value.slice(i, i + 2), 16));
export function mix(a, b, amount) {
  const right = rgb(b);
  return '#' + rgb(a).map((v, i) => Math.round(v + (right[i] - v) * amount).toString(16).padStart(2, '0')).join('');
}
function luminance(color) {
  const values = rgb(color).map(v => { v /= 255; return v <= .04045 ? v / 12.92 : ((v + .055) / 1.055) ** 2.4; });
  return values[0] * .2126 + values[1] * .7152 + values[2] * .0722;
}
export function contrast(a, b) {
  const x = luminance(a), y = luminance(b);
  return (Math.max(x, y) + .05) / (Math.min(x, y) + .05);
}
function ink(background) {
  const soft = ['#1b2028', '#f3f5f7'].sort((a, b) => contrast(b, background) - contrast(a, background));
  if (contrast(soft[0], background) >= 4.5) return soft[0];
  return contrast('#000000', background) > contrast('#ffffff', background) ? '#000000' : '#ffffff';
}
function readable(color, backgrounds, minimum = 4.5) {
  const target = ink(backgrounds[0]);
  for (let i = 0; i <= 40; i++) {
    const candidate = mix(color, target, i / 40);
    if (backgrounds.every(bg => contrast(candidate, bg) >= minimum)) return candidate;
  }
  return target;
}
export function surfaceFor(page) {
  const target = ink(page);
  const proposed = mix(page, '#ffffff', luminance(target) > .5 ? .045 : .72);
  return contrast(target, proposed) >= 4.5 ? proposed : page;
}
function palette(raw, fallback) {
  const color = (value, alternative) => isColor(value) ? value.toLowerCase() : alternative;
  const page = color(raw?.page, fallback.page);
  const proposed = color(raw?.surface, fallback.surface);
  return {
    page, surface: contrast(ink(page), proposed) >= 4.5 ? proposed : surfaceFor(page),
    accent: color(raw?.accent, fallback.accent),
    bars: Object.fromEntries(THEME_PROVIDERS.map(id => [id, color(raw?.bars?.[id], fallback.bars[id])])),
  };
}
export function normalizeTheme(raw, fallback = THEME_PRESETS[0]) {
  return {
    id: typeof raw?.id === 'string' ? raw.id.slice(0, 64) : fallback.id,
    name: typeof raw?.name === 'string' && raw.name.trim() ? raw.name.trim().slice(0, 32) : 'My theme',
    corners: ['soft', 'round', 'square'].includes(raw?.corners) ? raw.corners : fallback.corners,
    buttons: ['outline', 'filled'].includes(raw?.buttons) ? raw.buttons : fallback.buttons,
    light: palette(raw?.light, fallback.light), dark: palette(raw?.dark, fallback.dark),
  };
}
export function normalizeLibrary(raw) {
  const ids = new Set();
  const custom = (Array.isArray(raw?.custom) ? raw.custom : []).filter(t => {
    if (!/^custom-[a-z0-9-]{1,48}$/.test(t?.id || '') || ids.has(t.id)) return false;
    ids.add(t.id); return true;
  }).slice(0, 8).map(t => normalizeTheme(t));
  const themes = [...THEME_PRESETS, ...custom];
  return { version: 1, selected: themes.some(t => t.id === raw?.selected) ? raw.selected : 'graphite', custom };
}

export function themeTokens(theme, mode) {
  const p = theme[mode === 'dark' ? 'dark' : 'light'];
  const text = ink(p.page);
  const raised = mix(p.surface, text, .035);
  const surface2 = contrast(text, raised) >= 4.5 ? raised : p.surface;
  const backgrounds = [p.page, p.surface, surface2];
  const accent = readable(p.accent, backgrounds, 5);
  const tint = mix(p.surface, accent, .075);
  const danger = readable('#d86556', backgrounds);
  const radius = { square: 6, soft: 14, round: 20 }[theme.corners];
  const tokens = {
    '--bg': p.page, '--surface': p.surface, '--surface-2': surface2, '--muted': surface2,
    '--text': text, '--dim': readable(mix(p.surface, text, .62), backgrounds),
    '--faint': readable(mix(p.surface, text, .50), backgrounds),
    '--border': mix(p.surface, text, .13), '--border-strong': mix(p.surface, text, .26),
    '--accent': accent, '--on-accent': ink(accent),
    '--accent-soft': contrast(accent, tint) >= 4.5 ? tint : p.surface,
    '--selection-border': mix(p.surface, accent, .40), '--selection-ring': accent + '0a',
    '--warn': readable('#c48b36', backgrounds), '--danger': danger, '--on-danger': ink(danger),
    '--chart-1': accent, '--chart-fill': accent + '14', '--shadow': `0 2px 8px ${p.page}40`,
    '--card-radius': radius + 'px', '--button-radius': ({ square: 4, soft: 8, round: 12 }[theme.corners]) + 'px',
  };
  for (const id of THEME_PROVIDERS) tokens[`--bar-${id}`] = readable(p.bars[id], [surface2], 3);
  return { tokens, dark: luminance(text) > .5 };
}

const escape = text => String(text).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const clone = theme => normalizeTheme(theme, theme);

export function createAppearance({ root, storage, media, onModeChange = () => {} }) {
  const get = key => { try { return storage.getItem(key); } catch { return null; } };
  function load() { try { return normalizeLibrary(JSON.parse(get(THEME_STORAGE_KEY) || '{}')); } catch { return normalizeLibrary({}); } }
  let library = load(), mode = get('switcher-theme') || 'system', draft = null, editing = false, container = null;
  if (!['light', 'dark', 'system'].includes(mode)) mode = 'system';
  const themes = () => [...THEME_PRESETS, ...library.custom];
  const selected = () => themes().find(t => t.id === library.selected) || THEME_PRESETS[0];
  const effectiveMode = () => mode === 'system' ? media.matches ? 'dark' : 'light' : mode;
  function apply() {
    const theme = draft || selected();
    const { tokens, dark } = themeTokens(theme, effectiveMode());
    for (const [key, value] of Object.entries(tokens)) if (root.style.getPropertyValue(key) !== value) root.style.setProperty(key, value);
    root.style.colorScheme = dark ? 'dark' : 'light';
    root.dataset.theme = mode;
    root.dataset.effectiveTheme = dark ? 'dark' : 'light';
    root.dataset.appearance = theme.id;
    root.dataset.buttonStyle = theme.buttons;
  }
  function feedback(message, error = false) {
    const node = container?.querySelector('.appearance-feedback');
    if (node) { node.textContent = message; node.classList.toggle('error', error); }
  }
  function persist(next) {
    try { storage.setItem(THEME_STORAGE_KEY, JSON.stringify(next)); library = next; return true; }
    catch { feedback('Could not save this theme. Browser storage is unavailable.', true); return false; }
  }
  function tile(theme) {
    const { tokens } = themeTokens(theme, effectiveMode());
    const colors = ['codex', 'claude', 'opencode'].map(id => tokens[`--bar-${id}`]);
    return `<button type="button" class="theme-tile" data-theme-preset="${escape(theme.id)}" aria-pressed="${theme.id === library.selected}" aria-label="${escape(theme.name)} theme">
      <span class="theme-swatch" style="--preview-bg:${tokens['--bg']};--preview-surface:${tokens['--surface']};--preview-text:${tokens['--text']};--preview-accent:${tokens['--accent']};--preview-radius:${tokens['--card-radius']}">
        <span class="theme-mini-card"><span class="theme-mini-heading">Account <span>Active</span></span>${colors.map((color, i) => `<i style="--preview-bar:${color};--preview-width:${76 - i * 15}%"></i>`).join('')}</span>
      </span><span class="theme-tile-label">${escape(theme.name)}${theme.id === library.selected ? '<span aria-hidden="true">✓</span>' : ''}</span>
      <span class="theme-tile-description">${escape(theme.description || 'Your custom palette')}</span>
    </button>`;
  }
  function render() {
    if (!container?.isConnected) return;
    const focused = container.contains(document.activeElement) ? document.activeElement : null;
    const focusedPreset = focused?.dataset.themePreset;
    const focusedField = focused?.name;
    const selection = focusedField === 'name' ? [focused.selectionStart, focused.selectionEnd] : null;
    const scroll = container.querySelector('.theme-grid')?.scrollTop || 0;
    const current = selected(), p = (draft || current)[effectiveMode()];
    container.innerHTML = `<div class="appearance-heading"><div><h2>Appearance</h2><p class="settings-sub">Choose a look. Light, dark, and system mode work with every theme.</p></div></div>
      <div class="theme-grid" role="group" aria-label="Color themes">${themes().map(tile).join('')}</div>
      ${draft ? `<form class="theme-editor" data-theme-editor>
        <div class="theme-editor-heading"><strong>${editing ? 'Edit theme' : 'Make it yours'}</strong><span>Editing the ${effectiveMode()} palette · live preview</span></div>
        <div class="theme-fields">
          <label class="theme-name-field">Theme name<input name="name" value="${escape(draft.name)}" maxlength="32" required autocomplete="off"></label>
          <label>Background<input type="color" name="page" value="${p.page}"></label>
          <label>Accent<input type="color" name="accent" value="${p.accent}"></label>
          <label>Corners<select name="corners">${['square', 'soft', 'round'].map(v => `<option value="${v}" ${draft.corners === v ? 'selected' : ''}>${{square:'Crisp',soft:'Soft',round:'Rounded'}[v]}</option>`).join('')}</select></label>
          <label>Action buttons<select name="buttons"><option value="outline" ${draft.buttons === 'outline' ? 'selected' : ''}>Outlined</option><option value="filled" ${draft.buttons === 'filled' ? 'selected' : ''}>Filled</option></select></label>
        </div>
        <div class="theme-live-preview" aria-hidden="true"><span class="settings-sub">Preview</span><span class="account-badge">✓ Active</span><span class="theme-button-preview">Action button</span><span class="theme-bar-preview"><i></i></span></div>
        <details class="theme-bar-editor"><summary>Usage bar colors</summary><p class="settings-sub">Shared by compact cards, expanded cards, and usage charts.</p>
          <div class="theme-bar-colors">${THEME_PROVIDERS.map(id => `<label>${providerNames[id]}<input type="color" name="bar-${id}" value="${p.bars[id]}"></label>`).join('')}</div>
          <button type="button" data-theme-bars-accent>Use accent for every bar</button>
        </details>
        <p class="settings-sub">Switch light/dark above to edit that palette. Text and contrast adapt automatically.</p>
        <div class="appearance-actions"><button type="button" data-theme-cancel>Cancel</button><button class="theme-save" type="submit">Save theme</button></div>
      </form>` : `<div class="appearance-actions">
        <button type="button" data-theme-create ${library.custom.length >= 8 ? 'disabled' : ''}>Create a theme</button>
        ${current.id.startsWith('custom-') ? '<button type="button" data-theme-edit>Edit</button><button type="button" data-theme-delete>Delete theme</button>' : ''}
        <span class="settings-sub">Saved in this browser</span>
      </div>`}
      <p class="appearance-feedback" role="status" aria-live="polite"></p>`;
    container.querySelector('.theme-grid').scrollTop = scroll;
    if (focusedPreset) container.querySelector(`[data-theme-preset="${focusedPreset}"]`)?.focus({ preventScroll: true });
    else if (draft && /^[a-z-]+$/.test(focusedField || '')) {
      const field = container.querySelector(`[name="${focusedField}"]`);
      field?.focus({ preventScroll: true });
      if (selection && field) field.setSelectionRange(...selection);
    }
    else if (focused && !draft) container.querySelector(`[data-theme-preset="${library.selected}"]`)?.focus({ preventScroll: true });
    const grid = container.querySelector('.theme-grid');
    const activeTile = grid.contains(document.activeElement) ? document.activeElement : null;
    if (activeTile) {
      if (activeTile.offsetTop < grid.scrollTop) grid.scrollTop = activeTile.offsetTop;
      else if (activeTile.offsetTop + activeTile.offsetHeight > grid.scrollTop + grid.clientHeight) grid.scrollTop = activeTile.offsetTop + activeTile.offsetHeight - grid.clientHeight;
    }
  }
  function startEditing(copy) {
    if (copy && library.custom.length >= 8) return;
    draft = clone(selected()); editing = !copy;
    if (copy) { draft.id = `custom-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 9)}`; draft.name = `${selected().name} remix`.slice(0, 32); }
    apply(); render();
    container.querySelector('[name="name"]')?.focus();
  }
  function mount(node) {
    container = node; render();
    node.addEventListener('click', event => {
      const target = event.target.closest('button'); if (!target || target.disabled) return;
      if (target.dataset.themePreset) {
        if (!themes().some(t => t.id === target.dataset.themePreset)) return;
        if (draft) { feedback('Save or cancel your preview before switching themes.'); return; }
        if (persist({ ...library, selected: target.dataset.themePreset })) { apply(); render(); feedback(`${selected().name} applied`); }
      } else if (target.hasAttribute('data-theme-create')) startEditing(true);
      else if (target.hasAttribute('data-theme-edit')) startEditing(false);
      else if (target.hasAttribute('data-theme-cancel')) { draft = null; apply(); render(); }
      else if (target.hasAttribute('data-theme-delete')) {
        const next = { ...library, selected: 'graphite', custom: library.custom.filter(t => t.id !== library.selected) };
        if (persist(next)) { apply(); render(); feedback('Custom theme deleted. Graphite applied.'); }
      } else if (target.hasAttribute('data-theme-bars-accent') && draft) {
        const palette = draft[effectiveMode()];
        for (const id of THEME_PROVIDERS) { palette.bars[id] = palette.accent; node.querySelector(`[name="bar-${id}"]`).value = palette.accent; }
        apply();
      }
    });
    node.addEventListener('input', event => {
      if (!draft) return;
      const { name, value } = event.target, p = draft[effectiveMode()];
      if (typeof name !== 'string') return;
      if (name === 'name') { draft.name = value; return; }
      else if (name === 'page' && isColor(value)) { p.page = value; p.surface = surfaceFor(value); }
      else if (name === 'accent' && isColor(value)) p.accent = value;
      else if (name.startsWith('bar-') && THEME_PROVIDERS.includes(name.slice(4)) && isColor(value)) p.bars[name.slice(4)] = value;
      else if (name === 'corners' && ['square', 'soft', 'round'].includes(value)) draft.corners = value;
      else if (name === 'buttons' && ['outline', 'filled'].includes(value)) draft.buttons = value;
      apply();
    });
    node.addEventListener('submit', event => {
      if (!event.target.matches('[data-theme-editor]')) return;
      event.preventDefault(); if (!draft) return;
      if (!draft.name.trim()) { feedback('Give your theme a name.', true); return; }
      const theme = normalizeTheme(draft);
      if (themes().some(t => t.id !== theme.id && t.name.toLowerCase() === theme.name.toLowerCase())) { feedback('A theme already uses that name.', true); return; }
      if (!library.custom.some(t => t.id === theme.id) && library.custom.length >= 8) { feedback('You can save up to eight custom themes. Delete one first.', true); return; }
      const custom = library.custom.filter(t => t.id !== theme.id); custom.push(theme);
      if (persist({ version: 1, selected: theme.id, custom })) { draft = null; apply(); render(); feedback('Theme saved'); }
    });
  }
  function setMode(value, remember = true) {
    mode = ['light', 'dark', 'system'].includes(value) ? value : 'system';
    if (remember) { try { storage.setItem('switcher-theme', mode); } catch { /* mode still works for this session */ } }
    apply(); if (container?.isConnected) render();
    onModeChange(mode);
  }
  media.addEventListener('change', () => { if (mode === 'system') { apply(); render(); } });
  globalThis.addEventListener?.('storage', event => {
    if (event.key === THEME_STORAGE_KEY || event.key === null) { library = load(); apply(); render(); }
    if (event.key === 'switcher-theme' || event.key === null) setMode(get('switcher-theme') || 'system', false);
  });
  apply();
  return { mount, setMode, get mode() { return mode; }, get current() { return clone(draft || selected()); } };
}
