/**
 * Org membership.
 *
 * The actions are driven through `invokeActionForTest`, which posts them to the
 * REAL RPC endpoint. That matters here: an auth failure inside a `'use server'`
 * action must be a RETURNED envelope, never a throw, because a throw is
 * sanitized to a generic 500 in production and the caller loses the reason.
 * Driving the endpoint is what makes the difference visible.
 *
 * Counterfactuals: drop the owner check in `removeMember` and the member's 403
 * becomes a success; drop the self-removal guard and an org can be left with no
 * owner; throw instead of returning and the status becomes 500.
 */

import assert from 'node:assert/strict';
import { after, afterEach, before, test } from 'node:test';
import { invokeActionForTest } from '@webjsdev/server/testing';
import { bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';

let app: TestApp;
let handler: Awaited<ReturnType<typeof import('@webjsdev/server')['createRequestHandler']>>;
let ownerCookie = '';
let memberCookie = '';
let ownerOrg = '';
let ownerId = 0;
let db: typeof import('#db/connection.server.ts')['db'];
const realFetch = globalThis.fetch;

const INVITE = 'modules/orgs/actions/invite-member.server.ts';
const REMOVE = 'modules/orgs/actions/remove-member.server.ts';

interface Envelope {
  success?: boolean;
  error?: string;
  status?: number;
  fieldErrors?: Record<string, string>;
}

function stubGithubUser(user: { id: number; login: string } | null): void {
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.startsWith('https://api.github.com/users/')) {
      return user
        ? new Response(JSON.stringify(user), { headers: { 'content-type': 'application/json' } })
        : new Response('{"message":"Not Found"}', { status: 404 });
    }
    return new Response('{}', { status: 404 });
  }) as typeof globalThis.fetch;
}

function form(fields: Record<string, string>): FormData {
  const fd = new FormData();
  for (const [k, v] of Object.entries(fields)) fd.set(k, v);
  return fd;
}

before(async () => {
  app = await bootApp();
  const { createRequestHandler } = await import('@webjsdev/server');
  const { APP_DIR } = await import('../helpers/app.ts');
  handler = await createRequestHandler({ appDir: APP_DIR, dev: true });

  ownerCookie = await signInAs(app.handle, { id: 810, login: 'boss' });
  memberCookie = await signInAs(app.handle, { id: 811, login: 'crew' });

  ({ db } = await import('#db/connection.server.ts'));
  const schema = await import('#db/schema.server.ts');
  ownerOrg = (await db.query.orgs.findMany()).find((o) => o.slug === 'boss')!.id;
  ownerId = (await db.query.users.findMany()).find((u) => u.login === 'boss')!.id;

  const crew = (await db.query.users.findMany()).find((u) => u.login === 'crew')!;
  await db.insert(schema.memberships).values({ userId: crew.id, orgId: ownerOrg, role: 'member' });
  memberCookie = `${memberCookie}; pilots_org=${ownerOrg}`;
});

afterEach(() => {
  globalThis.fetch = realFetch;
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

test('an owner adds a member by GitHub login, resolved to a real id', async () => {
  stubGithubUser({ id: 9001, login: 'octocat' });

  const result = (await invokeActionForTest(handler, INVITE, 'inviteMember', [form({ login: 'octocat' })], { extraCookies: ownerCookie })) as Envelope;
  assert.equal(result.success, true);

  const user = await db.query.users.findFirst({ where: { githubId: '9001' } });
  assert.ok(user, 'the invite is keyed on the same github id their first sign-in will carry');

  const members = (await db.query.memberships.findMany()).filter((m) => m.orgId === ownerOrg);
  assert.equal(members.length, 3);
  assert.equal(members.find((m) => m.userId === user.id)!.role, 'member');
});

test('a GitHub login that does not exist is a field error, not a member', async () => {
  stubGithubUser(null);
  const result = (await invokeActionForTest(handler, INVITE, 'inviteMember', [form({ login: 'ghost' })], { extraCookies: ownerCookie })) as Envelope;
  assert.equal(result.success, false);
  assert.match(result.fieldErrors!.login, /No such GitHub user/);
});

test('a member cannot remove anyone, and the refusal is a 403 rather than a 500', async () => {
  const result = (await invokeActionForTest(handler, REMOVE, 'removeMember', [form({ user: String(ownerId) })], { extraCookies: memberCookie })) as Envelope;

  assert.equal(result.success, false);
  assert.equal(result.status, 403, 'a thrown forbidden() would surface as a generic 500 in production');
  assert.match(result.error!, /Only an owner/);

  const members = (await db.query.memberships.findMany()).filter((m) => m.orgId === ownerOrg);
  assert.ok(members.some((m) => m.userId === ownerId), 'the owner is still there');
});

test('an owner cannot remove themselves', async () => {
  const result = (await invokeActionForTest(handler, REMOVE, 'removeMember', [form({ user: String(ownerId) })], { extraCookies: ownerCookie })) as Envelope;

  assert.equal(result.success, false);
  assert.match(result.error!, /cannot remove themselves/);

  const members = (await db.query.memberships.findMany()).filter((m) => m.orgId === ownerOrg);
  assert.ok(
    members.some((m) => m.userId === ownerId && m.role === 'owner'),
    'an org with no owner has nobody who can invite one back',
  );
});

test('an owner can remove someone else', async () => {
  const crew = (await db.query.users.findMany()).find((u) => u.login === 'crew')!;
  const result = (await invokeActionForTest(handler, REMOVE, 'removeMember', [form({ user: String(crew.id) })], { extraCookies: ownerCookie })) as Envelope;

  assert.equal(result.success, true);
  const members = (await db.query.memberships.findMany()).filter((m) => m.orgId === ownerOrg);
  assert.equal(members.some((m) => m.userId === crew.id), false);
});
