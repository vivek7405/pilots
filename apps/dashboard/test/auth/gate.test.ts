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

const PAGES = ['/machines', '/services', '/volumes', '/domains', '/usage', '/keys', '/org'] as const;

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

test('the home page sends a signed-in visitor to the machines list', async () => {
  const anon = await app.handle(new Request('http://localhost/'));
  assert.equal(anon.status, 200, 'signed out, it offers the sign-in link');
  assert.match(await anon.text(), /Sign in with GitHub/);

  const signedIn = await app.handle(new Request('http://localhost/', asUser(cookie)));
  assert.equal(signedIn.status, 302);
  assert.equal(signedIn.headers.get('location'), '/machines');
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
