/**
 * The build log: the NDJSON route and the Deployments tab that follows it.
 *
 * Ownership is this app's own `builds` row, so a job this org did not start is
 * a 404 rather than a 403: a job id must leak nothing about existing.
 */
import assert from 'node:assert/strict';
import { before, test } from 'node:test';
import { asUser, bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';
import type { Service } from '@pilots/sdk';

let app: TestApp;
let cookie: string;
let org = '';

before(async () => {
  app = await bootApp();
  cookie = await signInAs(app.handle, { id: 7810, login: 'builder' });
  const { db } = await import('#db/connection.server.ts');
  const { builds } = await import('#db/schema.server.ts');
  org = (await db.query.orgs.findMany()).find((o) => o.slug === 'builder')!.id;
  app.fleet.data.services.push({ id: 'svc-b', name: 'web', org_id: org, app: 'shop', replicas: 1 } as unknown as Service);
  await db
    .insert(builds)
    .values({ orgId: org, serviceId: 'svc-b', jobId: 'bld-mine', repo: 'acme/shop', ref: 'main', startedBy: 1 })
    .run();
  await db
    .insert(builds)
    .values({ orgId: 'someone-else', serviceId: 'svc-x', jobId: 'bld-theirs', repo: 'x/y', ref: 'main', startedBy: 2 })
    .run();
});

test('a job the org did not start is a 404, not a 403', async () => {
  const res = await app.handle(new Request('http://localhost/api/builds/bld-theirs/logs', asUser(cookie)));
  assert.equal(res.status, 404);
  const none = await app.handle(new Request('http://localhost/api/builds/bld-nope/logs', asUser(cookie)));
  assert.equal(none.status, 404);
});

test('an owned job streams its lines as NDJSON, following when asked', async () => {
  app.fleet.calls.length = 0;
  app.fleet.data.buildLines.push({ step: 'fetch', line: 'cloning', ts: 1 }, { result: 'rootfs-123', ts: 2 });
  const res = await app.handle(new Request('http://localhost/api/builds/bld-mine/logs?follow=1', asUser(cookie)));
  assert.equal(res.status, 200);
  assert.match(res.headers.get('content-type') ?? '', /x-ndjson/);
  const lines = (await res.text()).trim().split('\n').map((l) => JSON.parse(l));
  assert.deepEqual(lines, [
    { step: 'fetch', line: 'cloning', ts: 1 },
    { result: 'rootfs-123', ts: 2 },
  ]);
  assert.deepEqual(app.fleet.calls.find((c) => c.method === 'builds.logs')!.args, ['bld-mine', { follow: true }]);
  assert.ok(app.fleet.calls.some((c) => c.method === 'as' && c.args[0] === org), 'read as the visitor org');
});

test('the Deployments tab follows a build it owns and lists the rest', async () => {
  const following = await app.handle(
    new Request('http://localhost/services/svc-b?tab=deployments&build=bld-mine', asUser(cookie)),
  );
  const body = await following.text();
  assert.match(body, /Building/);
  assert.match(body, /acme\/shop@main/);
  assert.match(body, /<build-log[^>]*build-id="bld-mine"[^>]*service-id="svc-b"[^>]*autodeploy/, 'the element follows and deploys');
  assert.match(body, /the Deploy form below takes it/, 'the scripting-off path is stated');

  const listed = await app.handle(new Request('http://localhost/services/svc-b?tab=deployments', asUser(cookie)));
  const list = await listed.text();
  assert.match(list, />Builds</);
  assert.match(list, /href="\/api\/builds\/bld-mine\/logs"/, 'the raw log link');
  assert.ok(!list.includes('bld-theirs'), 'another org\'s build never appears');
  assert.ok(!list.includes('<build-log'), 'nothing is followed unless asked');
});

test('a build someone else owns is not followed even when named', async () => {
  const res = await app.handle(
    new Request('http://localhost/services/svc-b?tab=deployments&build=bld-theirs', asUser(cookie)),
  );
  const body = await res.text();
  assert.ok(!body.includes('<build-log'), 'a foreign job id is ignored, not followed');
});
