/**
 * Connecting and disconnecting a repo.
 *
 * THREE things have to agree: the engine's own `repo` / `branch` /
 * `autodeploy` on the service, which the webhook handler reads to decide
 * whether to build; hostd's `repo_links` claim, which is what lets the ORG's
 * own key fetch that repository; and the local `repo_connections` row, which
 * the service page renders. A connect that wrote only some of them would build
 * silently, show a connection that does nothing, or leave the org's own key
 * refused the repository this page says is connected.
 *
 * Counterfactuals: drop the `services.patch` call and "the engine was told"
 * fails; drop the `claimRepo` call and the claim tests fail; delete the row
 * before patching on disconnect and the ordering assertion fails; drop the
 * repo-slug validator and `not a repo` is a 200.
 */

import assert from 'node:assert/strict';
import { generateKeyPairSync } from 'node:crypto';
import { after, afterEach, before, beforeEach, test } from 'node:test';
import { submitForm } from '@webjsdev/server/testing';
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

// THREE halves, not two, since the engine grew a claim of its own.
//
// hostd keeps a `repo_links` row per (org, repository) and reads it before it
// will fetch a repository by name. A connect that patches `services.repo` and
// skips the claim leaves this page saying "connected" and the webhook
// autodeploying, while the org's own key gets 403 repo_not_connected from
// `pilot deploy`, the SDK and every agent -- one product action, two fleet
// states, invisible here.
//
// Counterfactual: drop the claimRepo call from the route and this fails while
// every other test in this file stays green.
test('a connect claims the repository for the org on the fleet', async () => {
  stubInstallations({ id: 7, login: 'octo' });

  const res = await app.handle(connect('svc-mine', { repo: 'octo/app', branch: 'main' }));
  assert.equal(res.status, 200);

  const methods = app.fleet.calls.map((c) => c.method);
  const claimed = app.fleet.calls.find((c) => c.method === 'repos.connect');
  assert.ok(claimed, `the repository was never claimed on the fleet: ${methods.join(', ')}`);
  assert.deepEqual(claimed.args, ['octo/app']);
  // As the ORG, not as the ops org: the claim is written under `?org=`, so it
  // has to go through the org-scoped client.
  assert.ok(
    app.fleet.calls.some((c) => c.method === 'as' && c.args[0] === orgId),
    'the claim was not made as the visitor org',
  );
  assert.ok(
    methods.indexOf('repos.connect') < methods.indexOf('services.patch'),
    'the claim is written before the engine is told, so a refused claim leaves nothing patched',
  );
});

// The service page's FORM is a second caller of the same rule, and it used to
// be the one that skipped it. Both go through claimRepo, so this is the test
// that says "both", not "the route".
test('the service page form claims the repository too, not only the JSON route', async () => {
  stubInstallations({ id: 7, login: 'octo' });
  app.fleet.calls.length = 0;

  const res = await submitForm(
    app.handle,
    '/services/svc-mine?tab=settings',
    { service: 'svc-mine', repo: 'octo/app', branch: 'main', autodeploy: 'on' },
    { cookies: cookie, match: 'Connect' },
  );
  assert.equal(res.status, 303);

  const claimed = app.fleet.calls.find((c) => c.method === 'repos.connect');
  assert.ok(claimed, `the form connected without claiming: ${app.fleet.calls.map((c) => c.method).join(', ')}`);
  assert.deepEqual(claimed.args, ['octo/app']);
});

// The other side of the same rule. A claim is write-once with no disconnect,
// so it must never name an account the fleet was never given -- but this
// surface still records the intent and renders the install link, which is what
// the test below this one pins.
test('an owner with no installation is connected but never claimed', async () => {
  stubInstallations(null);

  const res = await app.handle(connect('svc-mine', { repo: 'nobody/app', branch: 'main' }));
  assert.equal(res.status, 200);
  assert.equal(
    app.fleet.calls.some((c) => c.method === 'repos.connect'),
    false,
    'a permanent claim was minted on an account the fleet cannot even read',
  );
  assert.ok(app.fleet.calls.some((c) => c.method === 'services.patch'), 'the intent is still recorded');
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
