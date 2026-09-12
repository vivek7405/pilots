/**
 * Builders on the team page, and the reset behind them.
 *
 * Both routes are reached through the SDK's transport rather than an SDK
 * method, because the SDK has no builders service; the fake answers them by
 * path, so what these tests pin is the exact contract the dashboard was
 * written against: `GET /v1/builders` answering an OBJECT with a `builders`
 * key, and `POST /v1/builders/{host}/reset`.
 *
 * Counterfactuals: read the list as a bare array and the section renders
 * empty; drop the `?org=` and a team sees another team's builders; widen the
 * reset past owner-and-admin and any member can make the next build slow for
 * everyone.
 */

import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { invokeActionForTest } from '@webjsdev/server/testing';
import { APP_DIR, asUser, bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';
import type { Machine } from '@pilots/sdk';

let app: TestApp;
let handler: { handle: TestApp['handle']; appDir: string };
let ownerCookie = '';
let memberCookie = '';
let orgId = '';

const RESET = 'modules/builders/actions/reset-builder.server.ts';

interface Envelope {
  success?: boolean;
  error?: string;
  status?: number;
  redirect?: string;
}

function form(fields: Record<string, string>): FormData {
  const fd = new FormData();
  for (const [k, v] of Object.entries(fields)) fd.set(k, v);
  return fd;
}

before(async () => {
  app = await bootApp();
  handler = { handle: app.handle, appDir: APP_DIR };
  const owner = await signInAs(app.handle, { id: 950, login: 'builderboss' });
  const member = await signInAs(app.handle, { id: 951, login: 'builderhand' });

  const { db } = await import('#db/connection.server.ts');
  const schema = await import('#db/schema.server.ts');
  const users = await db.query.users.findMany();
  const ownerId = users.find((u) => u.login === 'builderboss')!.id;
  const memberId = users.find((u) => u.login === 'builderhand')!.id;

  const [org] = await db
    .insert(schema.orgs)
    .values({ slug: 'buildco', name: 'Buildco', personal: false, ownerId })
    .returning();
  orgId = org.id;
  await db.insert(schema.memberships).values({ userId: ownerId, orgId, role: 'owner' });
  await db.insert(schema.memberships).values({ userId: memberId, orgId, role: 'member' });

  ownerCookie = `${owner}; pilots_org=${orgId}`;
  memberCookie = `${member}; pilots_org=${orgId}`;

  // A builder is an ordinary machine row: `GET /v1/builders` answers the same
  // objects `GET /v1/machines` does, so the fixture lives in `machines`.
  app.fleet.data.machines.push({
    id: 'm-builder-1',
    name: `builder-${orgId}-host-a`,
    host_id: 'host-a',
    state: 'suspended',
    vcpus: 4,
    mem_mib: 8192,
    // `last_activity`, in SECONDS, which is what hostd stamps. There is no
    // `last_used_at` anywhere in the Machine struct.
    last_activity: Math.floor(Date.now() / 1000) - 600,
    org_id: orgId,
  } as unknown as Machine);
  // Another team's builder, to prove the list is narrowed by the fleet rather
  // than filtered on the page.
  app.fleet.data.machines.push({
    id: 'm-builder-2',
    name: 'builder-someone-else-host-b',
    host_id: 'host-b',
    state: 'running',
    vcpus: 8,
    mem_mib: 16384,
    org_id: 'some-other-team',
  } as unknown as Machine);
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

test('the team page lists this team’s builders and nobody else’s', async () => {
  const res = await app.handle(new Request('http://localhost/org', asUser(ownerCookie)));
  assert.equal(res.status, 200);
  const body = await res.text();

  assert.match(body, /Builders/);
  assert.match(body, /host-a/, 'the builder that belongs to this team is listed');
  assert.doesNotMatch(body, /host-b/, 'and one that does not is never rendered');
  assert.match(body, /Where it runs/, 'the column is named in the words the rest of the app uses');
  assert.match(body, /4 vCPU, 8 GiB/, 'its size, so a slow build has a visible reason');

  const call = app.fleet.calls.find((c) => c.method === 'http.json' && c.args[1] === '/v1/builders');
  assert.ok(call, 'read through the SDK transport, not a hand-written fetch');
  assert.deepEqual(call.args[2], { org: orgId }, 'this app holds one admin key, so every list is narrowed');
});

test('an owner resets a builder, and the fleet is asked for that host', async () => {
  const result = (await invokeActionForTest(handler, RESET, 'resetBuilder', [form({ host: 'host-a' })], {
    extraCookies: ownerCookie,
  })) as Envelope;

  assert.equal(result.success, true);
  assert.equal(result.redirect, '/org?ok=builder-reset');
  assert.deepEqual(app.fleet.data.builderResets.at(-1), { host: 'host-a', org: orgId });
  assert.equal(
    app.fleet.data.machines.some((m) => m.id === 'm-builder-1'),
    false,
    'the reset destroys the builder as well as bumping the cache epoch',
  );
});

test('the rows the live list runs on are asked for WITH builders, and nothing else is', async () => {
  // `GET /v1/machines` omits builders unless asked, so the feed that backs
  // `<machine-list>` has to ask: its `builders` chip is a COUNT, and a count
  // of rows that never arrive is always zero.
  app.fleet.calls.length = 0;
  const res = await app.handle(new Request('http://localhost/api/machines', asUser(ownerCookie)));
  assert.equal(res.status, 200);
  const asked = app.fleet.calls.find((c) => c.args[1] === '/v1/machines')!;
  assert.deepEqual(asked.args[2], { org: orgId, include: 'builders' });

  // Every OTHER caller stays on the default, and one of them matters: the
  // check that refuses to delete a team while it still owns something must not
  // count a builder, because a builder is not a thing any person can remove
  // and counting one would make such a team permanently undeletable.
  const { listMachines } = await import('#modules/fleet/client.server.ts');
  app.fleet.calls.length = 0;
  const plainRows = await listMachines(orgId);
  const plain = app.fleet.calls.find((c) => c.args[1] === '/v1/machines')!;
  assert.equal((plain.args[2] as { include?: string }).include, undefined);
  assert.equal(
    plainRows.some((m) => (m.name ?? '').startsWith('builder-')),
    false,
    'so a team whose only remaining row is a builder still counts as empty',
  );
});

test('a member cannot reset a builder', async () => {
  const before = app.fleet.data.builderResets.length;
  const result = (await invokeActionForTest(handler, RESET, 'resetBuilder', [form({ host: 'host-a' })], {
    extraCookies: memberCookie,
  })) as Envelope;

  assert.equal(result.success, false);
  assert.equal(result.status, 403, 'a returned envelope, not a sanitized 500');
  assert.equal(app.fleet.data.builderResets.length, before, 'and the fleet was never asked');
});

test('a host id that is not one refuses before it reaches a URL', async () => {
  const before = app.fleet.data.builderResets.length;
  for (const host of ['../../v1/machines', '', 'host a', 'host/../..']) {
    const result = (await invokeActionForTest(handler, RESET, 'resetBuilder', [form({ host })], {
      extraCookies: ownerCookie,
    })) as Envelope;
    assert.equal(result.success, false, `${JSON.stringify(host)} is refused`);
    assert.equal(result.status, 404);
  }
  assert.equal(app.fleet.data.builderResets.length, before);
});

test('a fleet with no builders route leaves the section empty rather than losing the page', async () => {
  const saved = app.fleet.http;
  (app.fleet as unknown as { http: unknown }).http = {
    json: async () => {
      throw new Error('404 not found');
    },
  };
  try {
    const res = await app.handle(new Request('http://localhost/org', asUser(ownerCookie)));
    assert.equal(res.status, 200, 'the whole team page must not depend on a route the fleet may not serve');
    assert.match(await res.text(), /Nothing has been built for this team yet/);
  } finally {
    (app.fleet as unknown as { http: unknown }).http = saved;
  }
});
