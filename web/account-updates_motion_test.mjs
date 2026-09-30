import assert from 'node:assert/strict';
import { createResetFeedback } from './account-updates.js';

// Exercise the real feedback controller across actual element replacement
// semantics without adding a browser dependency to make verify.
class Element {
  classes = new Set(); children = []; animations = []; styles = new Map();
  classList = { add: (...names) => names.forEach(n => this.classes.add(n)),
    remove: (...names) => names.forEach(n => this.classes.delete(n)), contains: n => this.classes.has(n) };
  style = { setProperty: (key, value) => this.styles.set(key, value) };
  appendChild(node) { node.parent = this; this.children.push(node); }
  remove() { this.parent.children = this.parent.children.filter(n => n !== this); }
  setAttribute() {}
  animate(frames, timing) {
    const animation = { frames, timing, currentTime: 0, cancel() { this.cancelled = true; } };
    this.animations.push(animation); return animation;
  }
}
function makeCard() {
  const card = new Element(), health = new Element(), fill = new Element(), hatch = new Element();
  const window = { dataset: { quotaWindow: 'Session' }, querySelector: selector => selector === '.hatch' ? hatch : fill };
  card.querySelector = selector => selector === '.account-health' ? health : health.children.find(n => n.className === 'reset-feedback');
  card.querySelectorAll = () => [window];
  return Object.assign(card, { health, fill, hatch });
}
globalThis.document = { createElement: () => new Element() };
let clock = 1000, card = makeCard();
const timers = [];
const controller = createResetFeedback({ getCard: () => card, reducedMotion: () => false, now: () => clock, schedule: (fn, delay) => timers.push({ fn, delay }) });
const before = { id: 'a', usage: { windows: [{ label: 'Session', used_percent: 100 }] } };
const pending = { ...before, last_reset: { id: 'reset-1', outcome: 'reset', pending: true } };
const refilled = { ...pending, last_reset: { ...pending.last_reset, pending: false }, usage: { windows: [{ label: 'Session', used_percent: 5 }] } };
assert.equal(controller.begin('a'), true);
assert.equal(controller.begin('a'), false);
controller.observe([before], [pending]);
assert.equal(card.classList.contains('reset-celebrate'), true);
clock += 500;
controller.observe([pending], [refilled]);
const firstAnimation = card.fill.animations[0];
assert.deepEqual(firstAnimation.frames, [{ width: '0%' }, { width: '95%' }]);
clock += 250;
card = makeCard(); // A health/countdown render replaced the node mid-refill.
controller.repaint([refilled]);
assert.equal(firstAnimation.cancelled, true);
assert.deepEqual(card.fill.animations[0].frames, firstAnimation.frames);
assert.equal(card.fill.animations[0].currentTime, 250, 'replacement must resume the original timeline');
assert.equal(card.styles.get('--reset-elapsed'), '-750ms', 'replacement must preserve the success sweep progress');
controller.repaint([refilled]);
assert.equal(card.fill.animations.length, 1, 'ordinary polling restarted the refill');
controller.end('a');
timers.find(t => t.delay === 5000).fn();
assert.equal(card.querySelector('.reset-feedback'), undefined);

card = makeCard();
const reduced = createResetFeedback({ getCard: () => card, reducedMotion: () => true, now: () => clock, schedule: () => {} });
reduced.observe([before], [refilled]);
assert.equal(card.fill.animations.length, 0, 'reduced motion must not animate widths');
card = makeCard();
const failure = createResetFeedback({ getCard: () => card });
failure.begin('a'); failure.end('a');
assert.equal(card.querySelector('.reset-feedback'), undefined);
assert.equal(card.classList.contains('reset-celebrate'), false, 'a failed request must never celebrate');
console.log('Reset motion survives card replacement, deduplicates, and respects reduced motion: OK');
