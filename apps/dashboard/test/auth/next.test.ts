/**
 * A deep link survives signing in.
 *
 * The gate sends a signed-out visitor to `/login?next=<path>`. That parameter
 * has to reach the sign-in link, ride the round trip through GitHub, and decide
 * where the callback lands, or the URL promises behaviour that is not there
 * and the visitor ends up on the default page. The framework's callback always
 * lands on `/`, so the app carries the target in a cookie and applies it on
 * the way back.
 *
 * The target is attacker-controlled at every step (a query string, then a
 * cookie), so each step applies the same same-origin rule the org switcher
 * uses, and a bad value is dropped rather than repaired.
 *
 * Counterfactuals: drop the `next` argument from `signInLink()` and the link
 * assertion fails; drop the cookie override in the auth route and the callback
 * lands on `/`; drop `localPath` from either side and `//evil.com` goes through.
 */

import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { getSetCookies } from '@webjsdev/server/testing';
import { bootApp, driveOAuth } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';

let app: TestApp;
let nextId = 3000;

/** A fresh GitHub identity per sign-in, so each test creates its own account. */
function profile() {
  nextId += 1;
  return { id: nextId, login: `deep-${nextId}` };
}

before(async () => {
  app = await bootApp();
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

test('the gate carries the path, and the login page hands it to the sign-in link', async () => {
  const bounced = await app.handle(new Request('http://localhost/services/abc?tab=releases'));
  assert.equal(bounced.status, 302);
  const location = bounced.headers.get('location')!;
  assert.equal(location, '/login?next=%2Fservices%2Fabc%3Ftab%3Dreleases');

  const login = await app.handle(new Request(`http://localhost${location}`));
  assert.equal(login.status, 200);
  assert.match(
    await login.text(),
    /href="\/api\/auth\/signin\/github\?next=%2Fservices%2Fabc%3Ftab%3Dreleases"/,
    'the link names the target',
  );
});

test('the login page drops a target that would leave the origin', async () => {
  for (const next of ['//evil.com', '/\\evil.com', 'https://evil.com', 'evil.com']) {
    const res = await app.handle(new Request(`http://localhost/login?next=${encodeURIComponent(next)}`));
    const body = await res.text();
    assert.match(body, /href="\/api\/auth\/signin\/github"/, `next=${next} renders the bare link`);
    assert.equal(body.includes('evil.com'), false);
  }
});

test('the sign-in start sets the target cookie for a same-origin path only', async () => {
  const ok = await app.handle(new Request('http://localhost/api/auth/signin/github?next=%2Fservices%2Fabc'));
  assert.equal(ok.status, 302);
  const cookie = getSetCookies(ok).find((c) => c.startsWith('pilots_next='));
  assert.ok(cookie, 'the target rides a cookie across the round trip');
  assert.match(cookie, /Path=\/api\/auth/, 'scoped to the auth routes');
  assert.match(cookie, /HttpOnly/);
  assert.match(cookie, /SameSite=Lax/, 'Lax, because the callback is a top-level navigation from GitHub');

  for (const next of ['//evil.com', '/\\evil.com', 'https://evil.com']) {
    const bad = await app.handle(new Request(`http://localhost/api/auth/signin/github?next=${encodeURIComponent(next)}`));
    assert.equal(bad.status, 302, 'the sign-in itself still proceeds');
    assert.equal(getSetCookies(bad).some((c) => c.startsWith('pilots_next=')), false, `next=${next} sets no cookie`);
  }
});

test('the callback lands on the target and clears the cookie', async () => {
  const { callback } = await driveOAuth(app.handle, profile(), { signin: '/api/auth/signin/github?next=%2Fservices%2Fabc' });
  assert.equal(callback.status, 302);
  assert.equal(callback.headers.get('location'), '/services/abc');
  const cookies = getSetCookies(callback);
  assert.ok(cookies.some((c) => c.startsWith('webjs.auth=')), 'and the visitor is signed in');
  const cleared = cookies.find((c) => c.startsWith('pilots_next='));
  assert.ok(cleared, 'the target cookie is cleared');
  assert.match(cleared, /Max-Age=0/);
});

test('without a target the callback lands on the default', async () => {
  const { callback } = await driveOAuth(app.handle, profile());
  assert.equal(callback.headers.get('location'), '/');
  assert.equal(getSetCookies(callback).some((c) => c.startsWith('pilots_next=')), false, 'nothing to clear');
});

test('a forged target cookie cannot redirect off the origin', async () => {
  for (const next of ['//evil.com', '/\\evil.com', 'https://evil.com']) {
    const { callback } = await driveOAuth(app.handle, profile(), { extraCookies: `pilots_next=${encodeURIComponent(next)}` });
    assert.equal(callback.headers.get('location'), '/', `a cookie of ${next} is dropped`);
    assert.ok(getSetCookies(callback).some((c) => c.startsWith('pilots_next=') && c.includes('Max-Age=0')), 'and cleared');
  }
});
