/**
 * Every product path is under /dashboard.
 *
 * The marketing site owns `/` in this app, so a product link written the old
 * way (`/machines/m-1`) does not 404 loudly in review: it lands on the
 * marketing 404 in production, for one link, on one page. This reads the
 * source instead of rendering it, so a link behind a tab nobody's test opens
 * is still caught.
 *
 * `/api`, `/login` and `/oauth` are deliberately not product paths: they are
 * addresses the CLI and GitHub hold, and they stay at the root.
 *
 * Counterfactual: write `href="/keys"` anywhere under app/, components/,
 * modules/ or lib/ and this fails naming the file and the line.
 */
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { DASHBOARD, SECTIONS } from '#lib/paths.ts';
import { breadcrumb } from '#lib/utils/breadcrumb.ts';

const ROOT = join(import.meta.dirname, '..', '..');
const BARE = new RegExp(`["'\`(=]/(${SECTIONS.join('|')})(?=[/"'\`?#$ ]|$)`);

function sources(dir: string, out: string[] = []): string[] {
  for (const name of readdirSync(dir)) {
    const full = join(dir, name);
    if (statSync(full).isDirectory()) sources(full, out);
    else if (name.endsWith('.ts')) out.push(full);
  }
  return out;
}

function bareProductPaths(text: string): number[] {
  const lines: number[] = [];
  text.split('\n').forEach((line, i) => {
    // The fleet API has its own /v1/machines, /v1/services and so on.
    if (line.includes('/v1/') || line.includes('/api/')) return;
    if (BARE.test(line)) lines.push(i + 1);
  });
  return lines;
}

test('no product path is written without the /dashboard prefix', () => {
  const offenders: string[] = [];
  // The product half only: the marketing site has a /sandboxes page of its own.
  for (const dir of ['app/(product)', 'app/api', 'components', 'modules', 'lib']) {
    for (const file of sources(join(ROOT, dir))) {
      for (const line of bareProductPaths(readFileSync(file, 'utf8'))) {
        offenders.push(`${relative(ROOT, file)}:${line}`);
      }
    }
  }
  assert.deepEqual(offenders, [], 'these lines link to a product page at the root, which is the marketing site');
});

test('the check fires on a bare product path and not on a prefixed or API one', () => {
  assert.deepEqual(bareProductPaths('<a href="/keys">'), [1]);
  assert.deepEqual(bareProductPaths("redirect: `/machines/${id}`"), [1]);
  assert.deepEqual(bareProductPaths('<a href="/dashboard/keys">'), []);
  assert.deepEqual(bareProductPaths("fetch('/api/machines/x')"), []);
  assert.deepEqual(bareProductPaths("client.get('/v1/services')"), []);
  // A word that merely starts like a section is not one.
  assert.deepEqual(bareProductPaths('<a href="/organic">'), []);
});

test('the product pages live where the prefix says', () => {
  const dir = join(ROOT, 'app', '(product)', DASHBOARD.slice(1));
  assert.ok(statSync(join(dir, 'page.ts')).isFile(), 'the product home');
  for (const section of SECTIONS) {
    assert.ok(statSync(join(dir, '(app)', section)).isDirectory(), `${section} is under the prefix, behind the gate`);
  }
  // Route groups are not in the URL: (product)/login is still /login.
  for (const stays of ['api', join('(product)', 'login'), join('(product)', 'oauth')]) {
    assert.ok(statSync(join(ROOT, 'app', stays)).isDirectory(), `${stays} keeps its root address: something outside the app holds it`);
  }
});

test('the prefix is where the product lives, never a crumb of its own', () => {
  assert.deepEqual(breadcrumb('/dashboard'), [{ label: 'pilots' }]);
  const keys = breadcrumb('/dashboard/keys');
  assert.equal(keys[0].href, DASHBOARD, 'the root crumb goes to the product home, not the marketing page');
  assert.equal(keys.some((c) => c.label.toLowerCase() === 'dashboard'), false);
  assert.equal(keys.at(-1)!.label, 'Tokens');
});
