/**
 * `POST /api/cli/exchange`, the one route `pilot login` depends on.
 *
 * The security property under test is which GitHub endpoint verifies the
 * token. `GET /user` would accept a token issued to ANY OAuth app, so a token
 * leaked from an unrelated application could mint pilots keys for its owner.
 * The check-a-token endpoint answers 404 for a token that is not this App's.
 *
 * Counterfactual: point the route at `GET /user` and the "a token GitHub does
 * not recognise as ours is a 401" assertion fails, because the stub answers
 * that path with a user.
 */

import assert from 'node:assert/strict';
import { after, afterEach, before, test } from 'node:test';
import { bootApp } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';

let app: TestApp;
let db: typeof import('#db/connection.server.ts')['db'];
const realFetch = globalThis.fetch;

/**
 * Stubs GitHub. `checkToken` decides what the check-a-token endpoint answers;
 * `GET /user` always answers with a user, so a route that used it instead of
 * the check endpoint would pass where it should not.
 */
function stubGithub(checkToken: () => Response): void {
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.includes('/applications/') && url.endsWith('/token')) return checkToken();
    if (url === 'https://api.github.com/user') {
      return new Response(JSON.stringify({ id: 999, login: 'someone-elses-app-user' }), {
        headers: { 'content-type': 'application/json' },
      });
    }
    return new Response('{}', { status: 404 });
  }) as typeof globalThis.fetch;
}

function goodCheck(login = 'clidev', id = 4242) {
  return () =>
    new Response(JSON.stringify({ user: { id, login, name: 'CLI Dev', avatar_url: 'https://a/b.png' } }), {
      headers: { 'content-type': 'application/json' },
    });
}

function exchange(token: unknown, ip = '198.51.100.7'): Request {
  return new Request('http://localhost/api/cli/exchange', {
    method: 'POST',
    headers: { 'content-type': 'application/json', 'x-forwarded-for': ip },
    body: JSON.stringify(token === undefined ? {} : { github_access_token: token }),
  });
}

before(async () => {
  app = await bootApp();
  ({ db } = await import('#db/connection.server.ts'));
});

afterEach(() => {
  globalThis.fetch = realFetch;
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

test('a token issued to this app mints a deploy key and creates the account', async () => {
  stubGithub(goodCheck());
  app.fleet.calls.length = 0;

  const res = await app.handle(exchange('gho_valid'));
  assert.equal(res.status, 200);

  const body = (await res.json()) as { api_key: string; org_id: string; scopes: string[] };
  assert.match(body.api_key, /^pilot_/);
  assert.deepEqual(body.scopes, ['deploy']);

  const org = await db.query.orgs.findFirst({ where: { slug: 'clidev' } });
  assert.ok(org, 'the personal org was created');
  assert.equal(body.org_id, org.id);

  const user = await db.query.users.findFirst({ where: { githubId: '4242' } });
  assert.ok(user);
  assert.equal((await db.query.memberships.findMany()).length, 1);

  const call = app.fleet.calls.find((c) => c.method === 'apiKeys.create');
  assert.deepEqual(call!.args[0], { org_id: org.id, scopes: ['deploy'] });

  const row = await db.query.apiKeys.findFirst({ where: { orgId: org.id } });
  assert.equal(row!.createdBy, null, 'a CLI login has no session to attribute the key to');
  assert.match(row!.name, /^cli clidev \d{4}-\d{2}-\d{2}$/);
});

test('the same user logging in again reuses the org and mints a NEW key', async () => {
  stubGithub(goodCheck());
  const res = await app.handle(exchange('gho_valid_again'));
  assert.equal(res.status, 200);
  const body = (await res.json()) as { api_key: string; org_id: string };

  const orgs = (await db.query.orgs.findMany()).filter((o) => o.slug === 'clidev');
  assert.equal(orgs.length, 1, 'still one org');
  assert.equal(body.org_id, orgs[0].id);

  const keys = (await db.query.apiKeys.findMany()).filter((k) => k.orgId === orgs[0].id);
  assert.equal(keys.length, 2, 'each login mints its own key rather than resurrecting one');
});

test('a token GitHub does not recognise as ours is a 401, not a key', async () => {
  stubGithub(() => new Response('{"message":"Not Found"}', { status: 404 }));
  app.fleet.calls.length = 0;

  const res = await app.handle(exchange('gho_someone_elses_app', '198.51.100.8'));
  assert.equal(res.status, 401);
  assert.equal(((await res.json()) as { error: string }).error, 'token not issued to this app');
  assert.equal(
    app.fleet.calls.some((c) => c.method === 'apiKeys.create'),
    false,
    'nothing was minted for a token belonging to another application',
  );
});

test('a GitHub failure that is not a 404 is a 502, never a key', async () => {
  stubGithub(() => new Response('boom', { status: 500 }));
  const res = await app.handle(exchange('gho_x', '198.51.100.9'));
  assert.equal(res.status, 502);
});

test('a missing token is a 422 and GitHub is never called', async () => {
  let called = false;
  stubGithub(() => {
    called = true;
    return new Response('{}', { status: 200 });
  });
  const res = await app.handle(exchange(undefined, '198.51.100.10'));
  assert.equal(res.status, 422);
  assert.equal(called, false);
});

test('the exchange is limited to ten a minute per IP', async () => {
  stubGithub(goodCheck('ratelimited', 5150));
  const ip = '198.51.100.44';

  const statuses: number[] = [];
  for (let i = 0; i < 11; i += 1) {
    statuses.push((await app.handle(exchange('gho_valid', ip))).status);
  }

  assert.equal(statuses.filter((s) => s === 200).length, 10, 'ten get through');
  assert.equal(statuses.at(-1), 429, 'the eleventh is refused');

  const other = await app.handle(exchange('gho_valid', '198.51.100.45'));
  assert.notEqual(other.status, 429, 'a different forwarded IP has its own bucket');
});
