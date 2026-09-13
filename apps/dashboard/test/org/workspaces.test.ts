/**
 * Creating, renaming, handing over, leaving and deleting a team.
 *
 * The create is driven through `submitForm`, the way a browser with scripting
 * off drives it, because the action answers with a `Response`: creating a team
 * switches to it and a switch is a cookie, which has to ride a header. The
 * rest go through `invokeActionForTest`, which posts them to the REAL RPC
 * endpoint -- that is what makes the difference between a RETURNED refusal and
 * a thrown one visible, since a throw inside an action is sanitized to a
 * generic 500 in production and the caller loses the reason.
 *
 * Counterfactuals, each of which turns one assertion below red:
 *
 *   - drop the last-owner check in `leaveOrg` and a team is left with nobody
 *     who can rename, hand over, delete or change its plan
 *   - drop the personal check in `leaveOrg` or `deleteOrg` and an account can
 *     delete its own home and see nothing afterwards
 *   - skip the demotion half of `transferOwnership` and the team has two owners
 *   - answer the fleet counts with anything but a refusal and `deleteOrg`
 *     orphans everything the team was running
 *   - throw instead of returning and every `status` assertion becomes 500
 */

import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { and, eq } from 'drizzle-orm';
import { getSetCookies, invokeActionForTest, submitForm } from '@webjsdev/server/testing';
import { APP_DIR, bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';
import type { Machine } from '@pilots/sdk';

let app: TestApp;
/**
 * ONE handler for both the page renders and the action calls.
 *
 * Booting a second `createRequestHandler` in the same process re-seeds the
 * action registry, and the pages the FIRST handler renders then carry form
 * actions the runtime no longer recognises -- `<form action=${createOrg}>`
 * throws "is not a server action" at SSR and the page is a 500. Every test
 * here drives one app, which is also what production has.
 */
let handler: { handle: TestApp['handle']; appDir: string };
let ownerCookie = '';
let mateCookie = '';
let ownerId = 0;
let mateId = 0;
let db: (typeof import('#db/connection.server.ts'))['db'];
let schema: typeof import('#db/schema.server.ts');

const RENAME = 'modules/orgs/actions/rename-org.server.ts';
const LEAVE = 'modules/orgs/actions/leave-org.server.ts';
const TRANSFER = 'modules/orgs/actions/transfer-ownership.server.ts';
const DELETE = 'modules/orgs/actions/delete-org.server.ts';

interface Envelope {
  success?: boolean;
  error?: string;
  status?: number;
  redirect?: string;
  fieldErrors?: Record<string, string>;
}

function form(fields: Record<string, string>): FormData {
  const fd = new FormData();
  for (const [k, v] of Object.entries(fields)) fd.set(k, v);
  return fd;
}

/** A cookie pair that acts as one specific team, whatever the session picked. */
function acting(cookie: string, orgId: string): string {
  return `${cookie}; pilots_org=${orgId}`;
}

/** Make a team owned by `ownerId`, with `mateId` in it at `role`. */
async function makeTeam(name: string, slug: string, role = 'member'): Promise<string> {
  const [org] = await db
    .insert(schema.orgs)
    .values({ slug, name, personal: false, ownerId })
    .returning();
  await db.insert(schema.memberships).values({ userId: ownerId, orgId: org.id, role: 'owner' });
  await db.insert(schema.memberships).values({ userId: mateId, orgId: org.id, role });
  return org.id;
}

before(async () => {
  app = await bootApp();
  handler = { handle: app.handle, appDir: APP_DIR };

  ownerCookie = await signInAs(app.handle, { id: 920, login: 'chief' });
  mateCookie = await signInAs(app.handle, { id: 921, login: 'mate' });

  ({ db } = await import('#db/connection.server.ts'));
  schema = await import('#db/schema.server.ts');
  ownerId = (await db.query.users.findMany()).find((u) => u.login === 'chief')!.id;
  mateId = (await db.query.users.findMany()).find((u) => u.login === 'mate')!.id;
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

test('creating a team makes the creator its owner and switches to it', async () => {
  const res = await submitForm(app.handle, '/org/new', { name: 'Acme Rockets' }, { cookies: ownerCookie, match: /name="name"/ });

  assert.equal(res.status, 303);
  assert.equal(res.headers.get('location'), '/org?ok=created');

  const org = (await db.query.orgs.findMany()).find((o) => o.name === 'Acme Rockets');
  assert.ok(org, 'the team exists');
  assert.equal(org.personal, false, 'a team someone created is never a personal one');
  assert.equal(org.slug, 'acme-rockets', 'the address is derived from the name');
  assert.equal(org.billingAccountId, null, 'no billing account until a plan is activated');

  const membership = (await db.query.memberships.findMany()).find(
    (m) => m.orgId === org.id && m.userId === ownerId,
  );
  assert.equal(membership?.role, 'owner');

  assert.ok(
    getSetCookies(res).some((c) => c.startsWith(`pilots_org=${encodeURIComponent(org.id)}`)),
    'and the visitor is acting as the team they just made',
  );
});

test('a second team with the same name gets its own address rather than failing', async () => {
  const res = await submitForm(app.handle, '/org/new', { name: 'Acme Rockets' }, { cookies: ownerCookie, match: /name="name"/ });
  assert.equal(res.status, 303);
  const slugs = (await db.query.orgs.findMany()).filter((o) => o.name === 'Acme Rockets').map((o) => o.slug);
  assert.deepEqual(slugs.sort(), ['acme-rockets', 'acme-rockets-2']);
});

test('a name with no letters or digits is a field error, not a team called nothing', async () => {
  const before = (await db.query.orgs.findMany()).length;
  const res = await submitForm(app.handle, '/org/new', { name: '///' }, { cookies: ownerCookie, match: /name="name"/ });
  assert.equal(res.headers.get('location'), null, 'no redirect, so the form re-renders with its error');
  assert.equal((await db.query.orgs.findMany()).length, before, 'and nothing was written');
});

test('an owner renames a team and the address moves with it', async () => {
  const orgId = await makeTeam('Old Name', 'old-name');
  const result = (await invokeActionForTest(handler, RENAME, 'renameOrg', [form({ name: 'New Name' })], {
    extraCookies: acting(ownerCookie, orgId),
  })) as Envelope;

  assert.equal(result.success, true);
  const org = (await db.query.orgs.findMany()).find((o) => o.id === orgId)!;
  assert.equal(org.name, 'New Name');
  assert.equal(org.slug, 'new-name', 'the switcher shows the slug, so a stale one shows the old name');
});

test('a member cannot rename the team, and the refusal is a 403 rather than a 500', async () => {
  const orgId = await makeTeam('Stays Put', 'stays-put');
  const result = (await invokeActionForTest(handler, RENAME, 'renameOrg', [form({ name: 'Hijacked' })], {
    extraCookies: acting(mateCookie, orgId),
  })) as Envelope;

  assert.equal(result.success, false);
  assert.equal(result.status, 403, 'a thrown forbidden() would surface as a generic 500 in production');
  assert.equal((await db.query.orgs.findMany()).find((o) => o.id === orgId)!.name, 'Stays Put');
});

test('a member leaves a team, and the team keeps its owner', async () => {
  const orgId = await makeTeam('Leavable', 'leavable');
  const result = (await invokeActionForTest(handler, LEAVE, 'leaveOrg', [], {
    extraCookies: acting(mateCookie, orgId),
  })) as Envelope;

  assert.equal(result.success, true);
  const rows = (await db.query.memberships.findMany()).filter((m) => m.orgId === orgId);
  assert.deepEqual(rows.map((r) => r.userId), [ownerId]);
});

test('the last owner cannot leave', async () => {
  const orgId = await makeTeam('Only Owner', 'only-owner');
  await db
    .delete(schema.memberships)
    .where(and(eq(schema.memberships.orgId, orgId), eq(schema.memberships.userId, mateId)));

  const result = (await invokeActionForTest(handler, LEAVE, 'leaveOrg', [], {
    extraCookies: acting(ownerCookie, orgId),
  })) as Envelope;

  assert.equal(result.success, false);
  assert.equal(result.status, 422);
  assert.match(result.error!, /Hand the team/);
  const rows = (await db.query.memberships.findMany()).filter((m) => m.orgId === orgId);
  assert.equal(rows.length, 1, 'a team with no owner has nobody who can change or remove it');
});

test('a personal team can never be left', async () => {
  const personal = (await db.query.orgs.findMany()).find((o) => o.personal && o.ownerId === ownerId)!;
  const result = (await invokeActionForTest(handler, LEAVE, 'leaveOrg', [], {
    extraCookies: acting(ownerCookie, personal.id),
  })) as Envelope;

  assert.equal(result.success, false);
  assert.match(result.error!, /personal team cannot be left/);
  assert.ok(
    (await db.query.memberships.findMany()).some((m) => m.orgId === personal.id && m.userId === ownerId),
    'an account with no team can see nothing and has no path back',
  );
});

test('handing a team over promotes one and demotes the other in the same write', async () => {
  const orgId = await makeTeam('Handover', 'handover');
  const result = (await invokeActionForTest(handler, TRANSFER, 'transferOwnership', [form({ user: String(mateId) })], {
    extraCookies: acting(ownerCookie, orgId),
  })) as Envelope;

  assert.equal(result.success, true);
  const rows = (await db.query.memberships.findMany()).filter((m) => m.orgId === orgId);
  assert.equal(rows.find((r) => r.userId === mateId)!.role, 'owner');
  assert.equal(rows.find((r) => r.userId === ownerId)!.role, 'admin', 'the previous owner keeps running the team');
  assert.equal(rows.filter((r) => r.role === 'owner').length, 1, 'exactly one owner, never two');
  assert.equal(
    (await db.query.orgs.findMany()).find((o) => o.id === orgId)!.ownerId,
    mateId,
    'and the org row agrees about who owns it',
  );
});

test('a personal team cannot be handed over', async () => {
  const personal = (await db.query.orgs.findMany()).find((o) => o.personal && o.ownerId === ownerId)!;
  await db.insert(schema.memberships).values({ userId: mateId, orgId: personal.id, role: 'member' });

  const result = (await invokeActionForTest(handler, TRANSFER, 'transferOwnership', [form({ user: String(mateId) })], {
    extraCookies: acting(ownerCookie, personal.id),
  })) as Envelope;

  assert.equal(result.success, false);
  assert.equal(result.status, 422);
  assert.equal(
    (await db.query.orgs.findMany()).find((o) => o.id === personal.id)!.ownerId,
    ownerId,
    'the personal team is found by its owner id on every sign-in',
  );
});

test('deleting is refused while the team still runs something, and says what', async () => {
  const orgId = await makeTeam('Busy Team', 'busy-team');
  app.fleet.data.machines.push({ id: 'm1', name: 'api', state: 'running', org_id: orgId } as unknown as Machine);

  const result = (await invokeActionForTest(handler, DELETE, 'deleteOrg', [form({ confirm: 'Busy Team' })], {
    extraCookies: acting(ownerCookie, orgId),
  })) as Envelope;

  assert.equal(result.success, false);
  assert.equal(result.status, 409, 'a returned envelope, so the page can say what is left');
  assert.match(result.error!, /still has/);
  assert.ok(
    (await db.query.orgs.findMany()).some((o) => o.id === orgId),
    'deleting here would not delete what the fleet is running; it would only orphan it',
  );

  app.fleet.data.machines.length = 0;
});

test('the name has to be typed before a team is deleted', async () => {
  const orgId = await makeTeam('Type Me', 'type-me');
  const result = (await invokeActionForTest(handler, DELETE, 'deleteOrg', [form({ confirm: 'type me' })], {
    extraCookies: acting(ownerCookie, orgId),
  })) as Envelope;

  assert.equal(result.success, false);
  assert.ok(result.fieldErrors?.confirm);
  assert.ok((await db.query.orgs.findMany()).some((o) => o.id === orgId));
});

test('an empty team is deleted, its memberships go, and its tokens are revoked', async () => {
  const orgId = await makeTeam('Empty Team', 'empty-team');
  await db.insert(schema.apiKeys).values({
    orgId,
    name: 'ci',
    prefix: 'pilot_abcdef12',
    hash: 'sha256:to-revoke',
    scopes: ['deploy'],
    createdBy: ownerId,
  });

  const result = (await invokeActionForTest(handler, DELETE, 'deleteOrg', [form({ confirm: 'Empty Team' })], {
    extraCookies: acting(ownerCookie, orgId),
  })) as Envelope;

  assert.equal(result.success, true);
  assert.equal(result.redirect, '/org?ok=deleted');
  assert.equal((await db.query.orgs.findMany()).some((o) => o.id === orgId), false);
  assert.equal((await db.query.memberships.findMany()).some((m) => m.orgId === orgId), false);

  const key = (await db.query.apiKeys.findMany()).find((k) => k.hash === 'sha256:to-revoke')!;
  assert.ok(key.revokedAt, 'the row stays as the record the key existed, revoked');
  assert.ok(
    app.fleet.calls.some((c) => c.method === 'apiKeys.revoke' && c.args[0] === 'sha256:to-revoke'),
    'a live token for a team nobody can see is a credential with no owner',
  );
});

test('a member cannot delete the team', async () => {
  const orgId = await makeTeam('Guarded', 'guarded');
  const result = (await invokeActionForTest(handler, DELETE, 'deleteOrg', [form({ confirm: 'Guarded' })], {
    extraCookies: acting(mateCookie, orgId),
  })) as Envelope;

  assert.equal(result.success, false);
  assert.equal(result.status, 403);
  assert.ok((await db.query.orgs.findMany()).some((o) => o.id === orgId));
});

test('a personal team cannot be deleted', async () => {
  const personal = (await db.query.orgs.findMany()).find((o) => o.personal && o.ownerId === mateId)!;
  const result = (await invokeActionForTest(handler, DELETE, 'deleteOrg', [form({ confirm: personal.name })], {
    extraCookies: acting(mateCookie, personal.id),
  })) as Envelope;

  assert.equal(result.success, false);
  assert.equal(result.status, 422);
  assert.ok((await db.query.orgs.findMany()).some((o) => o.id === personal.id));
});
