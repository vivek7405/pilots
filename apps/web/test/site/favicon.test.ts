// The favicon is a second drawing of the mark, in a file the header never
// reads, so nothing else would notice it drifting. This holds it to the mark
// in logo-candidates.ts and to the palette, and checks the baked rasters are
// the sizes the layouts declare. Regenerate with scripts/generate-favicon.sh.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

const ROOT = join(import.meta.dirname, '..', '..');
const pub = (name: string) => readFileSync(join(ROOT, 'public', name));
const SVG = pub('favicon.svg').toString('utf8');

test('the favicon draws the same mark the header does', () => {
  const mark = readFileSync(join(ROOT, 'site', 'lib', 'design', 'logo-candidates.ts'), 'utf8');
  const d = /<path d="([^"]+)"/.exec(mark)?.[1];
  assert.ok(d, 'the mark has a path');
  assert.ok(SVG.includes(`d="${d}"`), 'favicon.svg carries the same path');
  for (const t of ['skewX(-11)', 'translate(5.4 0)', 'x="3" y="17.6" width="26" height="2"']) {
    assert.ok(mark.includes(t) && SVG.includes(t), `both drawings carry ${t}`);
  }
});

test('the favicon is drawn in the palette', () => {
  const css = readFileSync(join(ROOT, 'public', 'site.input.css'), 'utf8');
  const dark = (token: string) => {
    const m = new RegExp(`--${token}:\\s*light-dark\\(#[0-9a-f]{6},\\s*(#[0-9a-f]{6})\\)`, 'i').exec(css);
    assert.ok(m, `--${token} is a light-dark() pair`);
    return m[1].toLowerCase();
  };
  assert.ok(SVG.includes(`fill="${dark('paper')}"`), 'the tile is the deep ink');
  assert.ok(SVG.includes(`fill="${dark('ink')}"`), 'the mark is the light ink');
  assert.ok(SVG.includes(`stroke="${dark('rule-strong')}"`), 'the border is the strong rule');
});

test('the baked rasters are the sizes the layouts declare', () => {
  const pngSize = (name: string) => {
    const b = pub(name);
    assert.equal(b.toString('latin1', 1, 4), 'PNG', `${name} is a PNG`);
    return `${b.readUInt32BE(16)}x${b.readUInt32BE(20)}`;
  };
  assert.equal(pngSize('favicon-192.png'), '192x192');
  assert.equal(pngSize('favicon.png'), '512x512');
  assert.equal(pngSize('apple-touch-icon.png'), '180x180');

  const ico = pub('favicon.ico');
  assert.deepEqual([ico.readUInt16LE(0), ico.readUInt16LE(2)], [0, 1], 'favicon.ico is an icon file');
  const sizes = Array.from({ length: ico.readUInt16LE(4) }, (_, i) => ico[6 + i * 16]);
  assert.deepEqual(sizes, [16, 32, 48]);
});
