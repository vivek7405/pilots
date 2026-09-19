// The brand page prints the palette's hex values as text, which makes them a
// second copy of what `public/site.input.css` declares. This holds the two
// together: change a token in the stylesheet and this fails until the brand
// page says the same thing. The mark files carry the ink colours too.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { PALETTE } from '../../site/lib/design/palette.ts';

const ROOT = join(import.meta.dirname, '..', '..');
const CSS = readFileSync(join(ROOT, 'public', 'site.input.css'), 'utf8');

function declared(token: string) {
  const m = new RegExp(`--${token}:\\s*light-dark\\((#[0-9a-f]{6}),\\s*(#[0-9a-f]{6})\\)`, 'i').exec(CSS);
  assert.ok(m, `--${token} is not a light-dark() pair in site.input.css`);
  return { light: m[1].toLowerCase(), dark: m[2].toLowerCase() };
}

test('the brand page palette matches the stylesheet', () => {
  for (const s of PALETTE) {
    assert.deepEqual({ light: s.light, dark: s.dark }, declared(s.token), `--${s.token} drifted`);
  }
});

test('the downloadable marks are drawn in the palette ink', () => {
  const ink = declared('ink');
  const file = (name: string) => readFileSync(join(ROOT, 'public', 'brand', name), 'utf8');
  assert.ok(file('pilots-mark-ink.svg').includes(`fill="${ink.light}"`), 'the mark for light backgrounds');
  assert.ok(file('pilots-mark-paper.svg').includes(`fill="${ink.dark}"`), 'the mark for dark backgrounds');
});
