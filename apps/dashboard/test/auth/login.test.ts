/**
 * Sign in with GitHub, end to end through the real pipeline.
 *
 * The whole account model hangs off `callbacks.signIn`: it is what turns a
 * GitHub identity into a `users` row, a personal org and an `owner` membership.
 * Nothing else creates any of the three, so if this stops running the app signs
 * people in to nothing.
 *
 * Counterfactual: delete `callbacks.signIn` from `modules/auth/auth.server.ts`
 * and the "creates the user, org and membership" assertions fail with a users
 * count of 0.
 */

import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { getSetCookies } from '@webjsdev/server/testing';
import { bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';

let app: TestApp;
let db: typeof import('#db/connection.server.ts')['db'];

before(async () => {
  app = await bootApp();
  ({ db } = await import('#db/connection.server.ts'));
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

test('the signin route redirects to GitHub with a client id and a state cookie', async () => {
  const res = await app.handle(new Request('http://localhost/api/auth/signin/github'));
  assert.equal(res.status, 302);

  const location = new URL(res.headers.get('location')!);
  assert.equal(location.origin + location.pathname, 'https://github.com/login/oauth/authorize');
  assert.equal(location.searchParams.get('client_id'), 'Iv1.testclientid');
  assert.ok(location.searchParams.get('state'), 'a state parameter is present');
  assert.equal(
    location.searchParams.get('redirect_uri'),
    'http://localhost/api/auth/callback/github',
    'the callback URL is the one the GitHub App must be registered with',
  );

  const stateCookie = getSetCookies(res).find((c) => c.startsWith('webjs.auth.state='));
  assert.ok(stateCookie, 'the state is bound to a cookie, so the callback can verify it');
});

test('the callback creates the user, a personal org and an owner membership', async () => {
  const cookie = await signInAs(app.handle, {
    id: 5511,
    login: 'octocat',
    name: 'The Octocat',
    email: 'octo@example.com',
    avatar_url: 'https://avatars.example/octo.png',
  });
  assert.ok(cookie.startsWith('webjs.auth='));

  const user = await db.query.users.findFirst({ where: { githubId: '5511' } });
  assert.ok(user, 'the users row exists');
  assert.equal(user.login, 'octocat');
  assert.equal(user.email, 'octo@example.com');

  const orgs = await db.query.orgs.findMany();
  assert.equal(orgs.length, 1);
  assert.equal(orgs[0].slug, 'octocat', 'the personal org is slugged by GitHub login');
  assert.equal(orgs[0].personal, true);

  const members = await db.query.memberships.findMany();
  assert.equal(members.length, 1);
  assert.equal(members[0].role, 'owner');
  assert.equal(members[0].orgId, orgs[0].id);
});

test('a second sign-in for the same GitHub id creates nothing new', async () => {
  await signInAs(app.handle, { id: 5511, login: 'octocat', name: 'Renamed', email: 'octo@example.com' });

  assert.equal((await db.query.users.findMany()).length, 1, 'still one user');
  assert.equal((await db.query.orgs.findMany()).length, 1, 'still one org');
  assert.equal((await db.query.memberships.findMany()).length, 1, 'still one membership');

  const user = await db.query.users.findFirst({ where: { githubId: '5511' } });
  assert.equal(user!.name, 'Renamed', 'the profile is refreshed on every sign-in');
});

test('the session carries the GitHub login, not just the numeric id', async () => {
  const cookie = await signInAs(app.handle, { id: 8080, login: 'hubot', name: 'Hubot' });

  const res = await app.handle(
    new Request('http://localhost/api/auth/session', { headers: { cookie } }),
  );
  const session = (await res.json()) as { user?: { id?: string; login?: string } } | null;
  assert.equal(session?.user?.id, '8080');
  assert.equal(
    session?.user?.login,
    'hubot',
    'the jwt callback keeps `login` on every read, not only at sign-in',
  );
});
