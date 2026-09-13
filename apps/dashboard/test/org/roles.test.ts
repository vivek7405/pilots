/**
 * What an admin may do, and the four things it may not.
 *
 * The admin role only earns its place if it is genuinely narrower than owner.
 * Each test here is one of the boundaries, driven through the REAL RPC
 * endpoint so a refusal is a returned envelope with a status rather than a
 * generic 500.
 *
 * Counterfactuals: widen `canAdministerOrg` to include `admin` and the four
 * refusals below become successes; widen `validateScopes` and an admin mints a
 * key that administers every team on the fleet; narrow `canManageMembers` to
 * the owner and the two admin successes fail.
 */

import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { invokeActionForTest } from '@webjsdev/server/testing';
import { APP_DIR, bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';

let app: TestApp;
let handler: { handle: TestApp['handle']; appDir: string };
let adminCookie = '';
let ownerCookie = '';
let ownerId = 0;
let adminId = 0;
let hangerOnId = 0;
let orgId = '';
let db: (typeof import('#db/connection.server.ts'))['db'];
const realFetch = globalThis.fetch;

const INVITE = 'modules/orgs/actions/invite-member.server.ts';
const REMOVE = 'modules/orgs/actions/remove-member.server.ts';
const RENAME = 'modules/orgs/actions/rename-org.server.ts';
const TRANSFER = 'modules/orgs/actions/transfer-ownership.server.ts';
const DELETE = 'modules/orgs/actions/delete-org.server.ts';
const PLAN = 'modules/billing/actions/activate-plan.server.ts';
const KEY = 'modules/keys/actions/create-key.server.ts';

interface Envelope {
  success?: boolean;
  error?: string;
  status?: number;
  fieldErrors?: Record<string, string>;
  data?: { key?: string };
}

function form(fields: Record<string, string> | [string, string][]): FormData {
  const fd = new FormData();
  for (const [k, v] of Array.isArray(fields) ? fields : Object.entries(fields)) fd.append(k, v);
  return fd;
}

function stubGithubUser(user: { id: number; login: string }): void {
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.startsWith('https://api.github.com/users/')) {
      return new Response(JSON.stringify(user), { headers: { 'content-type': 'application/json' } });
    }
    return new Response('{}', { status: 404 });
  }) as typeof globalThis.fetch;
}

before(async () => {
  app = await bootApp();
  handler = { handle: app.handle, appDir: APP_DIR };

  ownerCookie = await signInAs(app.handle, { id: 930, login: 'principal' });
  adminCookie = await signInAs(app.handle, { id: 931, login: 'deputy' });
  await signInAs(app.handle, { id: 932, login: 'hangeron' });

  ({ db } = await import('#db/connection.server.ts'));
  const schema = await import('#db/schema.server.ts');
  const users = await db.query.users.findMany();
  ownerId = users.find((u) => u.login === 'principal')!.id;
  adminId = users.find((u) => u.login === 'deputy')!.id;
  hangerOnId = users.find((u) => u.login === 'hangeron')!.id;

  const [org] = await db
    .insert(schema.orgs)
    .values({ slug: 'shared', name: 'Shared', personal: false, ownerId })
    .returning();
  orgId = org.id;
  await db.insert(schema.memberships).values({ userId: ownerId, orgId, role: 'owner' });
  await db.insert(schema.memberships).values({ userId: adminId, orgId, role: 'admin' });
  await db.insert(schema.memberships).values({ userId: hangerOnId, orgId, role: 'member' });

  adminCookie = `${adminCookie}; pilots_org=${orgId}`;
  ownerCookie = `${ownerCookie}; pilots_org=${orgId}`;
});

after(() => {
  globalThis.fetch = realFetch;
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

test('an admin can add a member', async () => {
  stubGithubUser({ id: 9100, login: 'newcomer' });
  const result = (await invokeActionForTest(handler, INVITE, 'inviteMember', [form({ login: 'newcomer' })], {
    extraCookies: adminCookie,
  })) as Envelope;
  globalThis.fetch = realFetch;

  assert.equal(result.success, true);
  const added = (await db.query.users.findMany()).find((u) => u.githubId === '9100')!;
  const row = (await db.query.memberships.findMany()).find((m) => m.orgId === orgId && m.userId === added.id);
  assert.equal(row?.role, 'member');
});

test('an admin cannot add another admin: only the owner grants that', async () => {
  stubGithubUser({ id: 9101, login: 'aspirant' });
  const result = (await invokeActionForTest(
    handler,
    INVITE,
    'inviteMember',
    [form({ login: 'aspirant', role: 'admin' })],
    { extraCookies: adminCookie },
  )) as Envelope;
  globalThis.fetch = realFetch;

  assert.equal(result.success, false);
  assert.equal(result.status, 403);
  assert.equal((await db.query.users.findMany()).some((u) => u.githubId === '9101'), false);
});

test('an admin can remove a member but never the owner', async () => {
  const ok = (await invokeActionForTest(handler, REMOVE, 'removeMember', [form({ user: String(hangerOnId) })], {
    extraCookies: adminCookie,
  })) as Envelope;
  assert.equal(ok.success, true);

  const refused = (await invokeActionForTest(handler, REMOVE, 'removeMember', [form({ user: String(ownerId) })], {
    extraCookies: adminCookie,
  })) as Envelope;
  assert.equal(refused.success, false);
  assert.equal(refused.status, 403);
  assert.ok(
    (await db.query.memberships.findMany()).some((m) => m.orgId === orgId && m.userId === ownerId),
    'otherwise the admin role is the owner role with one extra step',
  );
});

test('an admin mints a deploy token but not an admin-scoped one', async () => {
  const ok = (await invokeActionForTest(
    handler,
    KEY,
    'createKey',
    [form([['name', 'ci'], ['scopes', 'deploy']])],
    { extraCookies: adminCookie },
  )) as Envelope;
  assert.equal(ok.success, true);
  assert.ok(ok.data?.key);

  const refused = (await invokeActionForTest(
    handler,
    KEY,
    'createKey',
    [form([['name', 'root'], ['scopes', 'admin']])],
    { extraCookies: adminCookie },
  )) as Envelope;
  assert.equal(refused.success, false);
  assert.equal(refused.status, 403);
  assert.match(refused.error!, /Only an org owner/);
  assert.equal(
    app.fleet.data.apiKeyRows.filter((r) => r.scopes.includes('admin')).length,
    0,
    'an admin key reaches every team on the fleet, not just this one',
  );
});

test('an admin cannot rename, hand over, delete or change the plan', async () => {
  const refusals: [string, string, string, FormData][] = [
    ['rename', RENAME, 'renameOrg', form({ name: 'Renamed' })],
    ['transfer', TRANSFER, 'transferOwnership', form({ user: String(adminId) })],
    ['delete', DELETE, 'deleteOrg', form({ confirm: 'Shared' })],
    ['plan', PLAN, 'activatePlan', form({ plan: 'pro' })],
  ];

  for (const [what, file, fn, body] of refusals) {
    const result = (await invokeActionForTest(handler, file, fn, [body], { extraCookies: adminCookie })) as Envelope;
    assert.equal(result.success, false, `${what} is refused`);
    assert.equal(result.status, 403, `${what} answers 403, not a generic 500`);
  }

  const org = (await db.query.orgs.findMany()).find((o) => o.id === orgId)!;
  assert.equal(org.name, 'Shared');
  assert.equal(org.ownerId, ownerId);
  assert.equal(org.billingAccountId, null);
  assert.equal(
    app.fleet.calls.some((c) => c.method === 'quotas.put'),
    false,
    'and no quota was written on the way to the refusal',
  );
});

test('the owner can do all four', async () => {
  const rename = (await invokeActionForTest(handler, RENAME, 'renameOrg', [form({ name: 'Shared Again' })], {
    extraCookies: ownerCookie,
  })) as Envelope;
  assert.equal(rename.success, true);

  const plan = (await invokeActionForTest(handler, PLAN, 'activatePlan', [form({ plan: 'pro' })], {
    extraCookies: ownerCookie,
  })) as Envelope;
  assert.equal(plan.success, true);

  const transfer = (await invokeActionForTest(handler, TRANSFER, 'transferOwnership', [form({ user: String(adminId) })], {
    extraCookies: ownerCookie,
  })) as Envelope;
  assert.equal(transfer.success, true);
});
