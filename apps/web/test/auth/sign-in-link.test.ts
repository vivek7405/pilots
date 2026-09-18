/**
 * The sign-in link opts out of the client router.
 *
 * `/api/auth/signin/github` is same-origin, so webjs's client router intercepts
 * a click on it, fetches the path itself and follows the `302` off origin to
 * GitHub. It cannot render GitHub's HTML as a webjs page, so it replaces the
 * page with `This page could not be loaded.` and the visitor never reaches the
 * authorize screen. `data-no-router` on the anchor is what stops that.
 *
 * No server test can see the failure itself: the route answers a correct `302`
 * either way, and it always did. The discriminating assertion available without
 * a browser is the rendered attribute, so that is what this asserts, on every
 * href the component can produce.
 *
 * Counterfactual: drop `data-no-router` from the `<a>` in
 * `modules/auth/sign-in-link.ts` and both anchor assertions fail with
 * "the sign-in anchor opts out of the client router".
 */

import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { bootApp } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';

let app: TestApp;

before(async () => {
  app = await bootApp();
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

/** The opening tag of the anchor pointing at the sign-in route, as rendered. */
async function signInAnchor(path: string): Promise<string> {
  const res = await app.handle(new Request(`http://localhost${path}`));
  assert.equal(res.status, 200, `${path} rendered (status ${res.status})`);
  const html = await res.text();
  const tag = html.match(/<a\b[^>]*href="\/api\/auth\/signin\/github[^"]*"[^>]*>/);
  assert.ok(tag, `${path} renders an anchor to the sign-in route`);
  return tag[0];
}

test('the home page sign-in anchor carries data-no-router', async () => {
  const tag = await signInAnchor('/');
  assert.match(tag, /\bdata-no-router\b/, 'the sign-in anchor opts out of the client router');
});

test('the login page keeps data-no-router on the ?next= href', async () => {
  const tag = await signInAnchor('/login?next=%2Fmachines');
  assert.match(tag, /href="\/api\/auth\/signin\/github\?next=%2Fmachines"/, 'the next target rides the href');
  assert.match(tag, /\bdata-no-router\b/, 'the sign-in anchor opts out of the client router');
});

/**
 * Sign-out is the sibling case, and it needs no opt-out. The router intercepts
 * form submissions too, but its interception only breaks a navigation that
 * leaves the origin: `signOut` answers a `302` to a same-origin path, which the
 * router's fetch follows and renders as an ordinary page. This asserts the
 * property the reasoning rests on, so a change that starts sending sign-out off
 * origin fails here instead of in a browser.
 */
test('sign-out redirects same origin, which is why it needs no opt-out', async () => {
  const res = await app.handle(new Request('http://localhost/api/auth/signout', { method: 'POST' }));
  assert.equal(res.status, 302);
  const location = res.headers.get('location')!;
  assert.equal(new URL(location, 'http://localhost').origin, 'http://localhost', 'sign-out stays on origin');
});
