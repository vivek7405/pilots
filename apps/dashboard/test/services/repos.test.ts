/**
 * Connecting and disconnecting a repo.
 *
 * Two halves have to agree: the engine's own `repo` / `branch` / `autodeploy`
 * on the service, which the webhook handler reads to decide whether to build,
 * and the local `repo_connections` row, which the service page renders. A
 * connect that wrote only one of them would either build silently or show a
 * connection that does nothing.
 *
 * Counterfactuals: drop the `services.patch` call and "the engine was told"
 * fails; delete the row before patching on disconnect and the ordering
 * assertion fails; drop the repo-slug validator and `not a repo` is a 200.
 */

import assert from 'node:assert/strict';
import { generateKeyPairSync } from 'node:crypto';
import { after, afterEach, before, beforeEach, test } from 'node:test';
import { bootApp, signInAs, asUser } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';
import type { Service } from '@pilots/sdk';

let app: TestApp;
let cookie = '';
let orgId = '';
let db: typeof import('#db/connection.server.ts')['db'];
const realFetch = globalThis.fetch;

/**
 * A throwaway signing key, generated per run.
 *
 * Never a committed PEM: a private key in the tree is a private key in every
 * clone and every secret scanner's report, even when it signs nothing real.
 */
const TEST_PEM = generateKeyPairSync('rsa', {
  modulusLength: 2048,
  privateKeyEncoding: { type: 'pkcs8', format: 'pem' },
  publicKeyEncoding: { type: 'spki', format: 'pem' },
}).privateKey;

function service(id: string, org: string): Service {
  return {
    id,
    name: id,
    org_id: org,
    replicas: 1,
    knobs: {},
    autodeploy: false,
    created_at: 0,
  } as Service;
}

/** Stubs the App's installation listing. `null` means "not installed". */
function stubInstallations(installation: { id: number; login: string } | null): void {
  process.env.PILOT_GITHUB_APP_ID = '12345';
  process.env.PILOT_GITHUB_APP_KEY = TEST_PEM;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.startsWith('https://api.github.com/app/installations')) {
      const body = installation
        ? [{ id: installation.id, account: { login: installation.login }, suspended_at: null }]
        : [];
      return new Response(JSON.stringify(body), { headers: { 'content-type': 'application/json' } });
    }
    return new Response('{}', { status: 404 });
  }) as typeof globalThis.fetch;
}

before(async () => {
  app = await bootApp();
  cookie = await signInAs(app.handle, { id: 300, login: 'shipper' });
  ({ db } = await import('#db/connection.server.ts'));
  orgId = (await db.query.orgs.findMany()).find((o) => o.slug === 'shipper')!.id;
  app.fleet.data.services.push(service('svc-mine', orgId), service('svc-theirs', 'another-org'));
});

beforeEach(() => {
  app.fleet.calls.length = 0;
});

afterEach(async () => {
  globalThis.fetch = realFetch;
  delete process.env.PILOT_GITHUB_APP_ID;
  delete process.env.PILOT_GITHUB_APP_KEY;
  await db.delete((await import('#db/schema.server.ts')).repoConnections);
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

function connect(serviceId: string, body: unknown, method = 'PUT'): Request {
  return new Request(
    `http://localhost/api/repos/${serviceId}`,
    asUser(cookie, { method, body: JSON.stringify(body) }),
  );
}

test('a connect tells the engine AND writes the local row', async () => {
  stubInstallations({ id: 7, login: 'octo' });

  const res = await app.handle(connect('svc-mine', { repo: 'octo/app', branch: 'main' }));
  assert.equal(res.status, 200);

  const patch = app.fleet.calls.find((c) => c.method === 'services.patch');
  assert.ok(patch, 'the engine was told, because the webhook handler reads it');
  assert.deepEqual(patch.args, ['svc-mine', { repo: 'octo/app', branch: 'main', autodeploy: true }]);

  const row = await db.query.repoConnections.findFirst({ where: { serviceId: 'svc-mine' } });
  assert.ok(row);
  assert.equal(row.repo, 'octo/app');
  assert.equal(row.branch, 'main');
  assert.equal(row.autodeploy, true);
  assert.equal(row.installationId, 7, 'the installation the App actually has on that owner');
  assert.equal(row.orgId, orgId);
});

test('an owner with no installation connects anyway, with a null installation id', async () => {
  stubInstallations(null);

  const res = await app.handle(connect('svc-mine', { repo: 'nobody/app', branch: 'trunk' }));
  assert.equal(res.status, 200);
  assert.equal(((await res.json()) as { installation_id: number | null }).installation_id, null);

  const row = await db.query.repoConnections.findFirst({ where: { serviceId: 'svc-mine' } });
  assert.equal(row!.installationId, null, 'so the page can render the install link');
});

test('reconnecting the same service updates the row rather than adding one', async () => {
  stubInstallations({ id: 7, login: 'octo' });
  await app.handle(connect('svc-mine', { repo: 'octo/app', branch: 'main' }));
  await app.handle(connect('svc-mine', { repo: 'octo/app', branch: 'release', autodeploy: false }));

  const rows = await db.query.repoConnections.findMany();
  assert.equal(rows.length, 1);
  assert.equal(rows[0].branch, 'release');
  assert.equal(rows[0].autodeploy, false);
});

test('a disconnect empties the engine fields before dropping the row', async () => {
  stubInstallations({ id: 7, login: 'octo' });
  await app.handle(connect('svc-mine', { repo: 'octo/app', branch: 'main' }));

  app.fleet.calls.length = 0;
  const res = await app.handle(connect('svc-mine', {}, 'DELETE'));
  assert.equal(res.status, 200);

  const patch = app.fleet.calls.find((c) => c.method === 'services.patch');
  assert.deepEqual(
    patch!.args,
    ['svc-mine', { repo: '', branch: '', autodeploy: false }],
    'a service left autodeploying with no row here would build on every push silently',
  );
  assert.equal(await db.query.repoConnections.findFirst({ where: { serviceId: 'svc-mine' } }), undefined);
});

test('a malformed repo is a 422 and the engine is never told', async () => {
  const res = await app.handle(connect('svc-mine', { repo: 'not a repo', branch: 'main' }));
  assert.equal(res.status, 422);
  assert.match(JSON.stringify(await res.json()), /owner\/name/);
  assert.equal(app.fleet.calls.some((c) => c.method === 'services.patch'), false);
});

test("another org's service is a 404 on connect and on read", async () => {
  const put = await app.handle(connect('svc-theirs', { repo: 'octo/app', branch: 'main' }));
  assert.equal(put.status, 404);
  assert.equal(app.fleet.calls.some((c) => c.method === 'services.patch'), false);

  const get = await app.handle(new Request('http://localhost/api/repos/svc-theirs', asUser(cookie)));
  assert.equal(get.status, 404);
});
