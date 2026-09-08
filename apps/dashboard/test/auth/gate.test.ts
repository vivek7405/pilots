/**
 * The gate on every signed-in page, and what each page renders once past it.
 *
 * The gate is a per-segment middleware rather than a check at the top of each
 * page, so this asserts it holds for every page under it, including ones added
 * later: the loop below is the whole route list.
 *
 * Counterfactual: delete `app/(app)/middleware.ts` and every page below
 * answers 200 to an anonymous request instead of 302.
 */

import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { bootApp, signInAs, asUser } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';

let app: TestApp;
let cookie = '';

const PAGES = ['/sandboxes', '/storage', '/domains', '/usage', '/keys', '/org'] as const;

/** The three that moved. Each answers 308 to its new address, signed in or not. */
const MOVED: [string, string][] = [
  ['/machines', '/sandboxes'],
  ['/services', '/'],
  ['/volumes', '/storage'],
];

before(async () => {
  app = await bootApp();
  cookie = await signInAs(app.handle, { id: 4001, login: 'pilot' });
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

test('every signed-in page redirects to login when signed out', async () => {
  for (const path of PAGES) {
    const res = await app.handle(new Request(`http://localhost${path}`));
    assert.equal(res.status, 302, `${path} redirects`);
    assert.match(
      res.headers.get('location') ?? '',
      /^\/login\?next=/,
      `${path} carries where they were going, so the sign-in lands somewhere useful`,
    );
  }
});

test('every signed-in page renders once signed in', async () => {
  for (const path of PAGES) {
    const res = await app.handle(new Request(`http://localhost${path}`, asUser(cookie)));
    assert.equal(res.status, 200, `${path} renders`);
    const body = await res.text();
    assert.match(body, /<\/html>/, `${path} produced a document`);
    assert.match(body, /pilots/, `${path} carries the nav`);
  }
});

test('the home page is the overview once signed in, and the sign-in offer before', async () => {
  const anon = await app.handle(new Request('http://localhost/'));
  assert.equal(anon.status, 200, 'signed out, it offers the sign-in link');
  assert.match(await anon.text(), /Sign in with GitHub/);

  // It used to redirect to /machines, which made the product's first screen a
  // table of rows with no state on them. Now it is the list of apps.
  const signedIn = await app.handle(new Request('http://localhost/', asUser(cookie)));
  assert.equal(signedIn.status, 200, 'no redirect: this page is the app list');
  const body = await signedIn.text();
  assert.ok(body.includes('>Apps<'), 'the app list is the home page');
  // Limits and capacity moved to Usage: the apps page is apps and nothing else.
  assert.ok(!body.includes('>Limits<'), 'no limits on the apps page');
  assert.ok(!body.includes('>Capacity<'), 'no capacity on the apps page');
});

test('the login page shows a failed sign-in rather than swallowing it', async () => {
  const res = await app.handle(new Request('http://localhost/login?error=AccessDenied'));
  assert.equal(res.status, 200);
  assert.match(await res.text(), /GitHub declined that sign-in/);
});

test('the keys page never renders a stored key value', async () => {
  const minted = await app.handle(
    new Request(
      'http://localhost/api/keys',
      asUser(cookie, { method: 'POST', body: JSON.stringify({ name: 'page-check', scopes: ['deploy'] }) }),
    ),
  );
  const { key } = (await minted.json()) as { key: string };

  const page = await app.handle(new Request('http://localhost/keys', asUser(cookie)));
  const body = await page.text();
  assert.match(body, /page-check/, 'the key is listed');
  assert.equal(body.includes(key), false, 'but its plaintext is not on the page');
  assert.equal(body.includes('sha256:'), false, 'and neither is its hash');
});

// A URL segment is an address. `/machines` was in the nav, in the command
// palette and in anyone's bookmarks, so it moves with a permanent redirect
// rather than a 404, while `/machines/<id>` keeps its path entirely.
test('the renamed list routes redirect permanently to their new address', async () => {
  for (const [from, to] of MOVED) {
    const res = await app.handle(new Request(`http://localhost${from}`, asUser(cookie)));
    assert.equal(res.status, 308, `${from} redirects permanently`);
    assert.equal(res.headers.get('location'), to, `${from} points at ${to}`);
  }
});
