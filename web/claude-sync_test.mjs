import assert from 'node:assert/strict';
import { createClaudeSync } from './claude-sync.js';

let finish, calls = 0, paints = 0;
const timers = [];
const controller = createClaudeSync({
  request: () => { calls++; return new Promise(resolve => { finish = resolve; }); },
  paint: () => paints++, schedule: fn => timers.push(fn),
});
assert.match(controller.html(), /Sync Claude Code sessions between accounts/);
assert.doesNotMatch(controller.html(), /disabled/);
const pending = controller.run();
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
finish({ok:false, json:async()=>({error:'Sync failed',details:'<untrusted error>',result:{backup:'/safe/backup'}})});
await failed;
assert.match(controller.html(), /<details/);
assert.match(controller.html(), /&lt;untrusted error&gt;/);
assert.match(controller.html(), /Backup: \/safe\/backup/);
assert.doesNotMatch(controller.html(), /disabled/);
assert.ok(paints >= 5);
console.log('Claude sync interaction states, duplicate guard, and error details: OK');
