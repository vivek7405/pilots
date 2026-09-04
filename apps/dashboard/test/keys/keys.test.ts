/**
 * Minting and revoking keys.
 *
 * The assertion that matters most is the one about the plaintext. A key is
 * shown once, by the response that minted it, and appears in nothing
 * afterwards -- not the list, not the row, not a log line. So the test asserts
 * on the FULL body text rather than on a field, because a leak would arrive as
 * a field nobody thought to check.
 *
 * Counterfactuals: return the rows unfiltered from `GET` and the "no plaintext
 * and no hash in the list" assertion fails; delete the row on revoke and the
 * "the row survives with revoked_at" assertion fails; drop the role check and
 * the "a member cannot mint admin" assertion fails.
 */

import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { eq } from 'drizzle-orm';
import { bootApp, signInAs, asUser } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';

let app: TestApp;
let ownerCookie = '';
let memberCookie = '';
let ownerOrg = '';
let db: typeof import('#db/connection.server.ts')['db'];

before(async () => {
  app = await bootApp();
  ownerCookie = await signInAs(app.handle, { id: 700, login: 'owner' });
  memberCookie = await signInAs(app.handle, { id: 701, login: 'member' });

  ({ db } = await import('#db/connection.server.ts'));
  const schema = await import('#db/schema.server.ts');
  const all = await db.query.orgs.findMany();
  ownerOrg = all.find((o) => o.slug === 'owner')!.id;

  // Put the second user in the first user's org as a plain member, and make
  // that their acting org, so the role branch is exercised on a real row.
  const memberUser = (await db.query.users.findMany()).find((u) => u.login === 'member')!;
  await db.insert(schema.memberships).values({ userId: memberUser.id, orgId: ownerOrg, role: 'member' });
  memberCookie = `${memberCookie}; pilots_org=${ownerOrg}`;
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

test('minting is refused to an anonymous caller', async () => {
  const res = await app.handle(
    new Request('http://localhost/api/keys', {
      method: 'POST',
      body: JSON.stringify({ name: 'ci', scopes: ['deploy'] }),
    }),
  );
  assert.equal(res.status, 401);
  assert.equal(app.fleet.calls.some((c) => c.method === 'apiKeys.create'), false);
});

test('a mint returns the plaintext once and asks the fleet for the caller org', async () => {
  app.fleet.calls.length = 0;
  const res = await app.handle(
    new Request(
      'http://localhost/api/keys',
      asUser(ownerCookie, { method: 'POST', body: JSON.stringify({ name: 'ci', scopes: ['deploy'] }) }),
    ),
  );
  assert.equal(res.status, 200);

  const body = (await res.json()) as { key: string; prefix: string; scopes: string[] };
  assert.match(body.key, /^pilot_/);
  assert.equal(body.prefix, body.key.slice(0, 'pilot_'.length + 8));

  const call = app.fleet.calls.find((c) => c.method === 'apiKeys.create');
  assert.deepEqual(call!.args[0], { org_id: ownerOrg, scopes: ['deploy'] });

  const row = await db.query.apiKeys.findFirst({ where: { name: 'ci' } });
  assert.ok(row, 'a metadata row was written');
  assert.equal(row.hash.startsWith('sha256:'), true, 'the row holds the hash the fleet returned');
  assert.equal(
    JSON.stringify(row).includes(body.key),
    false,
    'the plaintext is nowhere in the stored row',
  );
});

test('the list carries neither the plaintext nor the hash', async () => {
  const minted = await app.handle(
    new Request(
      'http://localhost/api/keys',
      asUser(ownerCookie, { method: 'POST', body: JSON.stringify({ name: 'second', scopes: ['machines'] }) }),
    ),
  );
  const { key } = (await minted.json()) as { key: string };

  const listed = await app.handle(new Request('http://localhost/api/keys', asUser(ownerCookie)));
  const text = await listed.text();

  assert.equal(text.includes(key), false, 'the plaintext appears in no later response');
  assert.equal(text.includes('sha256:'), false, 'the hash is not a value a list needs to carry');
  assert.match(text, /"prefix":"pilot_/, 'the prefix is there so a human can recognise the key');
});

test('a revoke tombstones the row and tells the fleet the hash', async () => {
  const minted = await app.handle(
    new Request(
      'http://localhost/api/keys',
      asUser(ownerCookie, { method: 'POST', body: JSON.stringify({ name: 'doomed', scopes: ['deploy'] }) }),
    ),
  );
  assert.equal(minted.status, 200);
  const row = (await db.query.apiKeys.findFirst({ where: { name: 'doomed' } }))!;

  app.fleet.calls.length = 0;
  const res = await app.handle(
    new Request('http://localhost/api/keys', asUser(ownerCookie, { method: 'DELETE', body: JSON.stringify({ id: row.id }) })),
  );
  assert.equal(res.status, 200);
  assert.deepEqual(app.fleet.calls.find((c) => c.method === 'apiKeys.revoke')!.args, [row.hash]);

  const after_ = await db.select().from((await import('#db/schema.server.ts')).apiKeys)
    .where(eq((await import('#db/schema.server.ts')).apiKeys.id, row.id))
    .get();
  assert.ok(after_, 'the row still exists: a revoked key is a tombstone, never a delete');
  assert.ok(after_.revokedAt instanceof Date, 'and it is marked revoked');
});

test("a key from another org cannot be revoked, and the fleet is never told", async () => {
  const row = (await db.query.apiKeys.findFirst({ where: { name: 'ci' } }))!;
  await db.update((await import('#db/schema.server.ts')).apiKeys)
    .set({ orgId: 'some-other-org' })
    .where(eq((await import('#db/schema.server.ts')).apiKeys.id, row.id));

  app.fleet.calls.length = 0;
  const res = await app.handle(
    new Request('http://localhost/api/keys', asUser(ownerCookie, { method: 'DELETE', body: JSON.stringify({ id: row.id }) })),
  );
  assert.equal(res.status, 404);
  assert.equal(app.fleet.calls.some((c) => c.method === 'apiKeys.revoke'), false);
});

test('a member may mint a deploy key but not an admin key', async () => {
  const ok = await app.handle(
    new Request(
      'http://localhost/api/keys',
      asUser(memberCookie, { method: 'POST', body: JSON.stringify({ name: 'member-deploy', scopes: ['deploy'] }) }),
    ),
  );
  assert.equal(ok.status, 200, 'a member can mint within their own scope');

  app.fleet.calls.length = 0;
  const refused = await app.handle(
    new Request(
      'http://localhost/api/keys',
      asUser(memberCookie, { method: 'POST', body: JSON.stringify({ name: 'takeover', scopes: ['admin'] }) }),
    ),
  );
  assert.equal(refused.status, 403, 'an admin key can create keys for any org on the fleet');
  assert.equal(app.fleet.calls.some((c) => c.method === 'apiKeys.create'), false);
});

test('a bad shape is a 422 with a field error, and nothing is minted', async () => {
  for (const body of [{ scopes: ['deploy'] }, { name: 'x', scopes: [] }, { name: 'x', scopes: ['root'] }]) {
    app.fleet.calls.length = 0;
    const res = await app.handle(
      new Request('http://localhost/api/keys', asUser(ownerCookie, { method: 'POST', body: JSON.stringify(body) })),
    );
    assert.equal(res.status, 422, `refused ${JSON.stringify(body)}`);
    assert.ok(((await res.json()) as { fieldErrors: object }).fieldErrors);
    assert.equal(app.fleet.calls.some((c) => c.method === 'apiKeys.create'), false);
  }
});
