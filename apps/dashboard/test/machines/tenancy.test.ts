/**
 * Tenancy at the dashboard's edge.
 *
 * This app holds ONE admin key, which by construction sees every machine on
 * the fleet. So hostd's own tenancy check cannot help here: the only thing
 * standing between org A's session and org B's machine is `assertOwned` in
 * every route. These tests are that guarantee.
 *
 * Counterfactual: drop `assertOwned` from `app/api/machines/[id]/*` and every
 * 404 assertion below becomes a 200.
 */

import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { bootApp, signInAs, asUser } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';
import type { Machine } from '@pilots/sdk';

let app: TestApp;
let cookieA = '';
let cookieB = '';
let orgA = '';
let orgB = '';

function machine(id: string, orgId: string): Machine {
  return {
    id,
    name: id,
    state: 'running',
    org_id: orgId,
    url: `https://${id}.pilotrun.app`,
    host_id: 'host-1',
    created_at: 0,
  } as Machine;
}

before(async () => {
  app = await bootApp();
  cookieA = await signInAs(app.handle, { id: 100, login: 'alice' });
  cookieB = await signInAs(app.handle, { id: 200, login: 'bob' });

  const { db } = await import('#db/connection.server.ts');
  const orgs = await db.query.orgs.findMany();
  orgA = orgs.find((o) => o.slug === 'alice')!.id;
  orgB = orgs.find((o) => o.slug === 'bob')!.id;

  app.fleet.data.machines.push(machine('m-alice', orgA), machine('m-bob', orgB));
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

test('the list is narrowed to the caller org, by asking the fleet for that org', async () => {
  app.fleet.calls.length = 0;
  const res = await app.handle(new Request('http://localhost/api/machines', asUser(cookieA)));
  assert.equal(res.status, 200);

  const body = (await res.json()) as { machines: Machine[] };
  assert.deepEqual(body.machines.map((m) => m.id), ['m-alice']);

  const listCall = app.fleet.calls.find((c) => c.method === 'http.json');
  assert.ok(listCall, 'the route asked the fleet rather than filtering a full list itself');
  assert.deepEqual(listCall.args[2], { org: orgA }, 'the org filter went to the engine');
});

test("a foreign machine is a 404 on GET, not a 403", async () => {
  const res = await app.handle(new Request('http://localhost/api/machines/m-bob', asUser(cookieA)));
  assert.equal(res.status, 404, 'a 403 would confirm the id exists to a stranger');
  assert.match(await res.text(), /not found/);
});

test('a foreign machine is a 404 on suspend, and the fleet is never asked to act', async () => {
  app.fleet.calls.length = 0;
  const res = await app.handle(
    new Request('http://localhost/api/machines/m-bob/suspend', asUser(cookieA, { method: 'POST' })),
  );
  assert.equal(res.status, 404);
  assert.equal(
    app.fleet.calls.some((c) => c.method === 'machines.suspend'),
    false,
    'ownership is checked BEFORE the mutating call, not after',
  );
});

test('a foreign machine is a 404 on DELETE, and the machine survives', async () => {
  const res = await app.handle(
    new Request('http://localhost/api/machines/m-bob', asUser(cookieA, { method: 'DELETE' })),
  );
  assert.equal(res.status, 404);
  assert.ok(app.fleet.data.machines.some((m) => m.id === 'm-bob'), "bob's machine is still there");
});

test('the owner can suspend their own machine', async () => {
  const res = await app.handle(
    new Request('http://localhost/api/machines/m-bob/suspend', asUser(cookieB, { method: 'POST' })),
  );
  assert.equal(res.status, 200);
  assert.equal(app.fleet.data.machines.find((m) => m.id === 'm-bob')!.state, 'suspended');
});

test('signed out is a 401 on every machine route', async () => {
  for (const [path, init] of [
    ['http://localhost/api/machines', {}],
    ['http://localhost/api/machines/m-alice', {}],
    ['http://localhost/api/machines/m-alice/suspend', { method: 'POST' }],
  ] as const) {
    const res = await app.handle(new Request(path, init));
    assert.equal(res.status, 401, `${path} refuses an anonymous caller`);
  }
});
