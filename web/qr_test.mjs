import test from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { qrMatrix, qrSVG } from './qr.js';

// The expected symbols below were checked module for module against the
// qrcode package (with the mask this encoder picked) and decoded with jsQR.
// The encoder as a whole matched that package for versions 1 to 10 and every
// mask when it was written.
const digest = m => createHash('sha256').update(m.map(r => r.map(c => (c ? 1 : 0)).join('')).join('\n')).digest('hex');

test('a phone address encodes to the verified symbol', () => {
  const m = qrMatrix('https://switcher-studio-mac.tail1234.ts.net');
  assert.equal(m.length, 33);
  assert.equal(digest(m), '5f372ca158016c765732ea5b0ec8b0647a50be264b45bc14064af6a358fa0c3d');
  assert.equal(digest(qrMatrix('A')), 'bc9009ae87ca68f1d256b69eb26362086d73d9f0b95f1e9f3dccae752626c3b6');
});

test('longer addresses use versions with version information, verified too', () => {
  const v9 = qrMatrix('https://example.com/' + 'x'.repeat(140));
  assert.equal(v9.length, 53);
  assert.equal(digest(v9), 'ba6981df16c0e3f95f9e26d4af355cc16b594560a029a4479385f92027769746');
  const v8 = qrMatrix('https://switcher-a-very-long-mac-name-for-testing.tail1234.ts.net/?q=' + 'y'.repeat(60));
  assert.equal(v8.length, 49);
  assert.equal(digest(v8), '06d388fa53ecd6090e7916e3bc85eb5bbf599780ed932a00bebfc0060ba75c98');
});

test('finder patterns sit in three corners', () => {
  const m = qrMatrix('A');
  const finder = (x0, y0) => [0, 1, 2, 3, 4, 5, 6].every(i => m[y0][x0 + i] && m[y0 + 6][x0 + i] && m[y0 + i][x0] && m[y0 + i][x0 + 6]);
  assert.ok(finder(0, 0) && finder(14, 0) && finder(0, 14));
});

test('the SVG has a quiet zone and refuses text that does not fit', () => {
  assert.match(qrSVG('A', { label: 'Open on your phone' }), /viewBox="0 0 29 29"[^>]*aria-label="Open on your phone"/);
  assert.match(qrSVG('A', { label: 'x" onload="alert(1)' }), /aria-label="x&quot; onload=&quot;alert\(1\)"/);
  assert.throws(() => qrMatrix('x'.repeat(300)), /too long/);
});
