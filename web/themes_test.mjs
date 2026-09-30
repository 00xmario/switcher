import assert from 'node:assert/strict';
import { THEME_PRESETS, THEME_PROVIDERS, THEME_STORAGE_KEY, normalizeTheme, normalizeLibrary, themeTokens, contrast, createAppearance } from './themes.js';

for (const theme of THEME_PRESETS) {
  for (const mode of ['light', 'dark']) {
    const { tokens: t } = themeTokens(theme, mode);
    for (const surface of ['--bg', '--surface', '--surface-2']) {
      assert.ok(contrast(t['--text'], t[surface]) >= 4.5, `${theme.name}/${mode}: text on ${surface}`);
      assert.ok(contrast(t['--dim'], t[surface]) >= 4.5, `${theme.name}/${mode}: secondary text on ${surface}`);
    }
    assert.ok(contrast(t['--accent'], t['--accent-soft']) >= 4.5, `${theme.name}/${mode}: selected badge`);
    assert.ok(contrast(t['--on-accent'], t['--accent']) >= 4.5, `${theme.name}/${mode}: filled button`);
    for (const id of THEME_PROVIDERS) assert.ok(contrast(t[`--bar-${id}`], t['--surface-2']) >= 3, `${theme.name}/${mode}: ${id} bar`);
  }
}

const injected = normalizeTheme({ id: 'custom-test', name: '<img src=x onerror=alert(1)>', corners: '999px', buttons: 'anything',
  light: { page: 'url(https://example.invalid/)', accent: 'red; background:url(x)', bars: { codex: '#123abc' } } });
assert.equal(injected.corners, 'soft');
assert.equal(injected.buttons, 'outline');
assert.equal(injected.light.page, THEME_PRESETS[0].light.page);
assert.equal(injected.light.bars.codex, '#123abc');
assert.equal(Object.keys(injected.dark.bars).length, 7);
assert.doesNotMatch(JSON.stringify(themeTokens(injected, 'light')), /url\(|background:url/);
for (const color of ['#000000', '#ffffff', '#777777', '#737373', '#00ff00', '#ff0000']) {
  const custom = normalizeTheme({ light: { page: color, accent: color, surface: '#ffffff' } });
  const { tokens: t } = themeTokens(custom, 'light');
  assert.ok(contrast(t['--text'], t['--bg']) >= 4.5, `custom ${color}: text`);
  assert.ok(contrast(t['--on-accent'], t['--accent']) >= 4.5, `custom ${color}: action text`);
  assert.ok(contrast(t['--accent'], t['--accent-soft']) >= 4.5, `custom ${color}: badge`);
}

const library = normalizeLibrary({ selected: 'custom-a', custom: [{ id: 'custom-a', name: 'One' }, { id: 'custom-a', name: 'Duplicate' }, { id: 'graphite', name: 'Hijack' }, { id: 'custom-"bad', name: 'Bad' }] });
assert.equal(library.custom.length, 1);
assert.equal(library.selected, 'custom-a');
assert.equal(normalizeLibrary({ selected: '__proto__' }).selected, 'graphite');
assert.equal(normalizeLibrary({ custom: Array.from({ length: 20 }, (_, i) => ({ id: `custom-${i}` })) }).custom.length, 8);

const values = new Map([['switcher-theme', 'system'], [THEME_STORAGE_KEY, JSON.stringify({ selected: 'tide' })]]);
const storage = { getItem: key => values.get(key), setItem: (key, value) => values.set(key, value) };
function root() {
  const properties = new Map();
  return { dataset: {}, style: { setProperty: (k, v) => properties.set(k, v), getPropertyValue: k => properties.get(k) || '' } };
}
const element = root();
let mediaChange;
const media = { matches: true, addEventListener: (_, callback) => { mediaChange = callback; } };
const appearance = createAppearance({ root: element, storage, media });
assert.equal(element.dataset.appearance, 'tide');
assert.equal(element.dataset.effectiveTheme, 'dark');
media.matches = false; mediaChange();
assert.equal(element.dataset.effectiveTheme, 'light');
appearance.setMode('dark');
assert.equal(values.get('switcher-theme'), 'dark');
assert.equal(element.dataset.effectiveTheme, 'dark');
media.matches = false; mediaChange();
assert.equal(element.dataset.effectiveTheme, 'dark', 'explicit mode must ignore OS changes');
const copy = appearance.current; copy.dark.accent = '#000000';
assert.notEqual(appearance.current.dark.accent, '#000000', 'draft accessor mutated a preset');
assert.doesNotThrow(() => createAppearance({ root: root(), media, storage: { getItem: () => { throw new Error('blocked'); }, setItem: () => { throw new Error('blocked'); } } }));
console.log('Theme palettes, contrast, validation, persistence, and system-mode handling: OK');
