/**
 * Releases and rollback.
 *
 * The rule under test is which release gets a rollback button. A release that
 * never passed its health gate was never serving traffic, so offering it as a
 * rollback target would be a deploy of something known broken, dressed up as a
 * recovery. Only the newest healthy release BEFORE the current one qualifies.
 *
 * Counterfactual: ignore `healthy` when picking the target and the unhealthy
 * release gets a rollback button, which the assertion below catches.
 */

import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { submitForm } from '@webjsdev/server/testing';
import { bootApp, signInAs, asUser } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';
import type { Release, Service } from '@pilots/sdk';

let app: TestApp;
let cookie = '';
let orgId = '';

before(async () => {
  app = await bootApp();
  cookie = await signInAs(app.handle, { id: 610, login: 'releaser' });
  const { db } = await import('#db/connection.server.ts');
  orgId = (await db.query.orgs.findMany()).find((o) => o.slug === 'releaser')!.id;

  app.fleet.data.services.push({
    id: 'svc-1',
    name: 'api',
    org_id: orgId,
    replicas: 2,
    knobs: {},
    autodeploy: false,
    release_id: 'rel-3',
    created_at: 0,
  } as Service);

  // Newest first, as the engine returns them.
  app.fleet.data.releases['svc-1'] = [
    { id: 'rel-3', service_id: 'svc-1', healthy: true, created_at: 3 },
    { id: 'rel-2', service_id: 'svc-1', healthy: false, created_at: 2 },
    { id: 'rel-1', service_id: 'svc-1', healthy: true, created_at: 1 },
  ] as Release[];
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

test('the releases route returns the engine rows for an owned service', async () => {
  const res = await app.handle(new Request('http://localhost/api/services/svc-1/releases', asUser(cookie)));
  assert.equal(res.status, 200);
  const body = (await res.json()) as { releases: Release[] };
  assert.deepEqual(body.releases.map((r) => r.id), ['rel-3', 'rel-2', 'rel-1']);
});

test('only the previous HEALTHY release is offered as a rollback target', async () => {
  const res = await app.handle(new Request('http://localhost/services/svc-1', asUser(cookie)));
  assert.equal(res.status, 200);
  const body = await res.text();

  const buttons = body.match(/Roll back to this/g) ?? [];
  assert.equal(buttons.length, 1, 'exactly one release carries the button');

  // The row for rel-1 (healthy, older) holds it; rel-2 (never healthy) does not.
  const rowOf = (id: string) => {
    const start = body.indexOf(id);
    return body.slice(start, body.indexOf('</tr>', start));
  };
  assert.match(rowOf('rel-1'), /Roll back to this/, 'the newest healthy release before the current one');
  assert.doesNotMatch(rowOf('rel-2'), /Roll back to this/, 'a release that never passed its gate is not a target');
  assert.doesNotMatch(rowOf('rel-3'), /Roll back to this/, 'and neither is the release already running');
});

test('submitting the rollback form calls the engine and returns to the service', async () => {
  // The page carries the service id as a hidden field, which is what a browser
  // would post; `submitForm` sends only what it is handed, so it is passed here
  // and asserted on the rendered form separately.
  const page = await app.handle(new Request('http://localhost/services/svc-1', asUser(cookie)));
  assert.match(await page.text(), /name="service" value="svc-1"/);

  app.fleet.calls.length = 0;
  const res = await submitForm(app.handle, '/services/svc-1', { service: 'svc-1' }, {
    cookies: cookie,
    match: 'Roll back to this',
  });

  assert.equal(res.status, 303, 'a successful action redirects rather than re-rendering');
  assert.equal(res.headers.get('location'), '/services/svc-1');
  assert.deepEqual(
    app.fleet.calls.find((c) => c.method === 'services.rollback')!.args,
    ['svc-1'],
    'no target is sent: only the engine knows which release passed its gate',
  );
});

test("another org's service is a 404 on the releases route", async () => {
  app.fleet.data.services.push({
    id: 'svc-theirs',
    name: 'theirs',
    org_id: 'another-org',
    replicas: 1,
    knobs: {},
    autodeploy: false,
    created_at: 0,
  } as Service);

  const res = await app.handle(new Request('http://localhost/api/services/svc-theirs/releases', asUser(cookie)));
  assert.equal(res.status, 404);
});
