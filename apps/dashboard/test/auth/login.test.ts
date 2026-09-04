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

test('a login GitHub freed and a new account claimed still signs in', async () => {
  // octocat (id 5511) exists from the tests above. GitHub lets a different
  // account take the login `octocat` after a rename or a deletion; the org
  // slug is unique, so this sign-in used to throw on the insert and the new
  // account could never get in.
  const cookie = await signInAs(app.handle, { id: 5512, login: 'octocat', name: 'The New Octocat' });
  assert.ok(cookie.startsWith('webjs.auth='), 'the sign-in completes');

  const users = await db.query.users.findMany();
  assert.deepEqual(
    users.filter((u) => u.login === 'octocat').map((u) => u.githubId).sort(),
    ['5511', '5512'],
    'two accounts, told apart by GitHub id, may share a login',
  );

  const slugs = (await db.query.orgs.findMany()).map((o) => o.slug).sort();
  assert.deepEqual(slugs, ['octocat', 'octocat-2'], 'the second personal org gets a suffixed slug');

  const first = users.find((u) => u.githubId === '5511')!;
  const second = users.find((u) => u.githubId === '5512')!;
  const personal = (await db.query.orgs.findMany()).filter((o) => o.personal);
  assert.equal(personal.find((o) => o.ownerId === first.id)!.slug, 'octocat', 'the original keeps their org');
  assert.equal(personal.find((o) => o.ownerId === second.id)!.slug, 'octocat-2');
});

test('a rename keeps the org and its slug, and a changed case is one login', async () => {
  await signInAs(app.handle, { id: 5511, login: 'octocat-dev' });
  const orgs = await db.query.orgs.findMany();
  assert.equal(orgs.length, 2, 'a rename creates no org');
  assert.ok(orgs.some((o) => o.slug === 'octocat'), 'and the personal org keeps the slug it was minted with');

  await signInAs(app.handle, { id: 5513, login: 'OctoCat' });
  const third = (await db.query.users.findFirst({ where: { githubId: '5513' } }))!;
  const after = await db.query.orgs.findMany();
  assert.equal(after.find((o) => o.ownerId === third.id)!.slug, 'octocat-3', 'case is folded before the slug is chosen');
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
