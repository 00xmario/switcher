import assert from 'node:assert/strict';
import { createClaudeSync } from './claude-sync.js';

let finish, calls = 0, paints = 0;
const timers = [];
const controller = createClaudeSync({
  confirm: async () => true,
  request: () => { calls++; return new Promise(resolve => { finish = resolve; }); },
  paint: () => paints++, schedule: fn => timers.push(fn),
});
assert.match(controller.html(), /Sync Claude Code sessions between accounts/);
assert.doesNotMatch(controller.html(), /disabled/);
const pending = controller.run();
await Promise.resolve();
assert.match(controller.html(), /aria-busy="true" disabled/);
await controller.run();
assert.equal(calls, 1, 'double-click must not start a second sync');
finish({ok:true, json:async()=>({detail:'2 new sessions added (account: 2)'})});
await pending;
assert.match(controller.html(), /success/);
assert.match(controller.html(), /2 new sessions added/);
timers.shift()();
assert.match(controller.html(), /button idle/);

const failed = controller.run();
await Promise.resolve();
finish({ok:false, json:async()=>({error:'Sync failed',details:'<untrusted error>',result:{backup:'/safe/backup'}})});
await failed;
assert.match(controller.html(), /<details/);
assert.match(controller.html(), /&lt;untrusted error&gt;/);
assert.match(controller.html(), /Backup: \/safe\/backup/);
assert.doesNotMatch(controller.html(), /disabled/);
assert.ok(paints >= 5);

let decide, dialogs = 0, syncs = 0;
const guarded = createClaudeSync({
  confirm: () => { dialogs++; return new Promise(resolve => { decide = resolve; }); },
  request: async () => { syncs++; return { ok: true, json: async () => ({ detail: 'Synced' }) }; },
  paint: () => {}, schedule: () => {},
});
const cancelled = guarded.run();
await guarded.run();
assert.equal(dialogs, 1, 'double-click must not open two dialogs');
assert.equal(syncs, 0, 'nothing may run while confirmation is open');
assert.doesNotMatch(guarded.html(), /aria-busy="true"/);
decide(false);
await cancelled;
assert.equal(syncs, 0, 'Cancel must not send a sync request');
assert.match(guarded.html(), /button idle/);
const approved = guarded.run();
assert.equal(dialogs, 2, 'a new click must require a new confirmation');
decide(true);
await approved;
assert.equal(syncs, 1, 'confirmation sends exactly one request');
console.log('Claude sync confirmation, cancellation, interaction states, and error details: OK');
