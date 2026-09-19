// The social card is a picture of the site, made by a script nobody runs on a
// deploy, so nothing else notices when it falls behind. It did once: it carried
// a logo the site had retired and a tagline the footer had dropped. This holds
// the card's words to the footer and the home page, its mark to the header's,
// and the file to what the platforms accept.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

const ROOT = join(import.meta.dirname, '..', '..');
const read = (...p: string[]) => readFileSync(join(ROOT, ...p), 'utf8');
const flat = (s: string) => s.replace(/\s+/g, ' ');

// Read out of the script's source rather than imported: it is a plain .mjs
// with no types, and importing it would also pull in its font files.
const SCRIPT = read('scripts', 'build-og.mjs');
const constant = (name: string) => {
  const m = new RegExp(`export const ${name} =\\s*'([^']+)'`).exec(SCRIPT);
  assert.ok(m, `build-og.mjs exports ${name} as a plain string`);
  return m[1];
};
const HEADLINE = constant('HEADLINE');
const BLURB = constant('BLURB');

test('the card says what the footer and the home page say', () => {
  assert.ok(flat(read('site', 'lib', 'ui', 'site-footer.ts')).includes(BLURB), 'the blurb is the footer blurb, word for word');
  assert.ok(flat(read('app', '(site)', 'page.ts')).includes(HEADLINE), 'the headline is the home page heading');
});

test('the card draws the mark the header does', () => {
  const d = /<path d="([^"]+)"/.exec(read('site', 'lib', 'design', 'logo-candidates.ts'))?.[1];
  assert.ok(d, 'the mark has a path');
  assert.ok(SCRIPT.includes(`d="${d}"`), 'build-og.mjs carries the same path');
});

test('the file is what the platforms accept', () => {
  const png = readFileSync(join(ROOT, 'public', 'og.png'));
  assert.equal(png.toString('latin1', 1, 4), 'PNG');
  assert.equal(`${png.readUInt32BE(16)}x${png.readUInt32BE(20)}`, '1200x630');
  // WhatsApp drops a preview image that is too heavy; 300 KiB is the safe side
  // of every figure published for it.
  assert.ok(png.length < 300 * 1024, `og.png is ${Math.round(png.length / 1024)} KiB`);
});

test('the image address carries a version', () => {
  assert.match(read('app', '(site)', 'layout.ts'), /og\.png\?v=\$\{OG_VERSION\}/);
});
