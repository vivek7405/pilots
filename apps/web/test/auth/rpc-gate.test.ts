/**
 * The action RPC boundary refuses a caller with no session, and a caller with
 * one gets only their own org.
 *
 * Every export of a `'use server'` file is reachable over HTTP at
 * `/__webjs/action/<hash>/<fn>`, guarded by the CSRF origin check and nothing
 * else. So a query that took the org id as an argument was an unauthenticated
 * endpoint handing out any tenant's data, and `listOrgs(userId)` was the way
 * to harvest the org ids to ask for. These tests drive the REAL endpoint, the
 * same way the browser stub does, with and without a session, and with a
 * foreign id in the argument position to show it no longer matters.
 *
 * Counterfactuals: put `orgId` back in `listKeys`'s signature and "a signed-in
 * caller cannot name another org" fails with bob's key in alice's list; drop
 * the `requireOrg()` call from any query and its 401 assertion fails.
 */

import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { invokeActionForTest, rawActionRequest } from '@webjsdev/server/testing';
import { APP_DIR, bootApp, signInAs, asUser } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';
import type { Machine } from '@pilots/sdk';

let app: TestApp;
let rpc: { handle: TestApp['handle']; appDir: string };
let alice = '';
let bob = '';
let bobOrg = '';
let bobUserId = 0;

const SINCE = new Date('2026-09-01T00:00:00Z');
const UNTIL = new Date('2026-09-30T00:00:00Z');

/** Every query in the app, with the argument a signed-in page would pass. */
const QUERIES: [file: string, fn: string, args: () => unknown[]][] = [
  ['modules/orgs/queries/list-orgs.server.ts', 'listOrgs', () => []],
  ['modules/orgs/queries/list-members.server.ts', 'listMembers', () => []],
  ['modules/keys/queries/list-keys.server.ts', 'listKeys', () => []],
  ['modules/usage/queries/usage-for-org.server.ts', 'usageForOrg', () => [{ since: SINCE, until: UNTIL }]],
  ['modules/machines/queries/list-machines.server.ts', 'listMachines', () => []],
  ['modules/machines/queries/get-machine.server.ts', 'getMachine', () => [{ id: 'm-bob' }]],
  ['modules/services/queries/list-services.server.ts', 'listServices', () => []],
  ['modules/services/queries/get-service.server.ts', 'getService', () => [{ id: 'svc-bob' }]],
  ['modules/volumes/queries/list-volumes.server.ts', 'listVolumes', () => []],
  ['modules/domains/queries/list-domains.server.ts', 'listDomains', () => []],
];

interface Envelope {
  success?: boolean;
  error?: string;
  status?: number;
}

before(async () => {
  app = await bootApp();
  rpc = { handle: app.handle, appDir: APP_DIR };
  alice = await signInAs(app.handle, { id: 100, login: 'alice' });
  bob = await signInAs(app.handle, { id: 200, login: 'bob' });

  const { db } = await import('#db/connection.server.ts');
  const schema = await import('#db/schema.server.ts');
  bobOrg = (await db.query.orgs.findMany()).find((o) => o.slug === 'bob')!.id;
  bobUserId = (await db.query.users.findMany()).find((u) => u.login === 'bob')!.id;

  // Bob has a key, a usage row, a machine and a service; alice has nothing.
  const minted = await app.handle(
    new Request('http://localhost/api/keys', asUser(bob, { method: 'POST', body: JSON.stringify({ name: 'bob-ci', scopes: ['deploy'] }) })),
  );
  assert.equal(minted.status, 200);
  await db.insert(schema.usageSamples).values({
    orgId: bobOrg,
    hostId: 'h1',
    windowStart: new Date('2026-09-02T00:00:00Z'),
    windowEnd: new Date('2026-09-02T01:00:00Z'),
    machineSeconds: 3600,
    vcpuSeconds: 3600,
    mibSeconds: 1,
    volumeGibSeconds: 0,
  });
  app.fleet.data.machines.push({ id: 'm-bob', name: 'm-bob', state: 'running', org_id: bobOrg, url: 'https://m-bob.pilotrun.app', host_id: 'h1', created_at: 0 } as Machine);
  app.fleet.data.services.push({ id: 'svc-bob', name: 'svc-bob', org_id: bobOrg, replicas: 1, url: 'https://svc-bob.pilotrun.app' } as never);
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

test('every query refuses an anonymous POST to its RPC endpoint', async () => {
  for (const [file, fn, args] of QUERIES) {
    const res = await rawActionRequest(rpc, file, fn, args());
    // A returned envelope is a 200 RPC payload with the status inside it; the
    // client stub resolves with it rather than throwing. What matters is that
    // the body is the refusal and nothing else.
    assert.equal(res.status, 200, `${fn} returned an envelope rather than throwing`);
    const body = await res.text();
    assert.match(body, /Sign in to continue/, `${fn} refused the anonymous caller`);
    assert.equal(body.includes(bobOrg), false, `${fn} leaks no org id`);
    assert.equal(body.includes('bob-ci'), false, `${fn} leaks no key name`);
    assert.equal(body.includes('m-bob.pilotrun.app'), false, `${fn} leaks no machine URL`);
    assert.equal(body.includes('svc-bob'), false, `${fn} leaks no service`);
  }
});

test('an anonymous caller cannot walk user ids through listOrgs', async () => {
  // The old signature was `listOrgs(userId)`; an integer in that position now
  // buys nothing, with or without a session.
  const anon = (await invokeActionForTest(rpc, 'modules/orgs/queries/list-orgs.server.ts', 'listOrgs', [bobUserId], { throwOnError: false })) as Envelope;
  assert.equal(anon.success, false);
  assert.equal(anon.status, 401);
  assert.equal(JSON.stringify(anon).includes(bobOrg), false);

  const asAlice = (await invokeActionForTest(rpc, 'modules/orgs/queries/list-orgs.server.ts', 'listOrgs', [bobUserId], { extraCookies: alice })) as { slug: string }[];
  assert.deepEqual(asAlice.map((o) => o.slug), ['alice'], 'alice sees her own orgs, whatever integer she sent');
});

test('a signed-in caller cannot name another org in the argument position', async () => {
  const keys = (await invokeActionForTest(rpc, 'modules/keys/queries/list-keys.server.ts', 'listKeys', [bobOrg], { extraCookies: alice })) as { name: string }[];
  assert.deepEqual(keys, [], "alice's list is empty; bob's org id in the argument is ignored");

  const usage = (await invokeActionForTest(rpc, 'modules/usage/queries/usage-for-org.server.ts', 'usageForOrg', [{ orgId: bobOrg, since: SINCE, until: UNTIL }], { extraCookies: alice })) as unknown[];
  assert.deepEqual(usage, [], "alice gets no usage rows; bob's org id in the input is ignored");

  const machine = await invokeActionForTest(rpc, 'modules/machines/queries/get-machine.server.ts', 'getMachine', [{ orgId: bobOrg, id: 'm-bob' }], { extraCookies: alice });
  assert.equal(machine, null, "bob's machine is not alice's, whatever org id she sent");

  const service = await invokeActionForTest(rpc, 'modules/services/queries/get-service.server.ts', 'getService', [{ orgId: bobOrg, id: 'svc-bob' }], { extraCookies: alice });
  assert.equal(service, null, "bob's service is not alice's either");
});

test('the same endpoints answer the session owner with their own data', async () => {
  const keys = (await invokeActionForTest(rpc, 'modules/keys/queries/list-keys.server.ts', 'listKeys', [], { extraCookies: bob })) as { name: string }[];
  assert.deepEqual(keys.map((k) => k.name), ['bob-ci']);

  const usage = (await invokeActionForTest(rpc, 'modules/usage/queries/usage-for-org.server.ts', 'usageForOrg', [{ since: SINCE, until: UNTIL }], { extraCookies: bob })) as { machineSeconds: number }[];
  assert.equal(usage.length, 1);
  assert.equal(usage[0].machineSeconds, 3600);

  const orgs = (await invokeActionForTest(rpc, 'modules/orgs/queries/list-orgs.server.ts', 'listOrgs', [], { extraCookies: bob })) as { id: string }[];
  assert.deepEqual(orgs.map((o) => o.id), [bobOrg]);

  const found = (await invokeActionForTest(rpc, 'modules/machines/queries/get-machine.server.ts', 'getMachine', [{ id: 'm-bob' }], { extraCookies: bob })) as { machine: Machine };
  assert.equal(found.machine.id, 'm-bob');
});

test('the refusal is a returned envelope, not a sanitized throw', async () => {
  const res = await rawActionRequest(rpc, 'modules/keys/queries/list-keys.server.ts', 'listKeys', []);
  assert.equal(res.ok, true, 'a thrown unauthorized() would be a non-2xx generic error');
  const body = (await invokeActionForTest(rpc, 'modules/keys/queries/list-keys.server.ts', 'listKeys', [], { throwOnError: false })) as Envelope;
  assert.equal(body.success, false);
  assert.equal(body.status, 401, 'the status the client acts on rides inside the envelope');
  assert.match(body.error ?? '', /Sign in/);
});
