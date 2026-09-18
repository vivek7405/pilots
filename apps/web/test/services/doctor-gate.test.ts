/**
 * A deploy that fails its health gate reaches the page with its details, so
 * the doctor card can name the instance and quote its last answer. Before
 * this the action flattened every error to a string and the card could only
 * say that something failed.
 */
import assert from 'node:assert/strict';
import { before, test } from 'node:test';
import { submitForm } from '@webjsdev/server/testing';
import { HealthGateError } from '@pilots/sdk';
import { bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';
import type { Machine, Release, Service } from '@pilots/sdk';

let app: TestApp;
let cookie: string;
let org = '';

before(async () => {
  app = await bootApp();
  cookie = await signInAs(app.handle, { id: 7820, login: 'doctor' });
  const { db } = await import('#db/connection.server.ts');
  const { builds } = await import('#db/schema.server.ts');
  org = (await db.query.orgs.findMany()).find((o) => o.slug === 'doctor')!.id;
  app.fleet.data.services.push({ id: 'svc-g', name: 'api', org_id: org, app: 'shop', replicas: 1, release_id: 'rel-2' } as unknown as Service);
  app.fleet.data.releases['svc-g'] = [
    { id: 'rel-2', healthy: false, created_at: Math.floor(Date.now() / 1000) - 600 },
    { id: 'rel-1', healthy: true, created_at: Math.floor(Date.now() / 1000) - 7200 },
  ] as unknown as Release[];
  app.fleet.data.machines.push({
    id: 'm-g', name: 'api-9', state: 'error', org_id: org, service_id: 'svc-g', release_id: 'rel-2',
  } as unknown as Machine);
  await db
    .insert(builds)
    .values({ orgId: org, serviceId: 'svc-g', jobId: 'bld-g', repo: 'acme/api', ref: 'main', startedBy: 1 })
    .run();
});

test('a health-gate refusal is a 422 whose card names the instance and quotes its last answer', async () => {
  app.fleet.data.deployError = new HealthGateError('deploy of api failed its health check', {
    service: 'svc-g',
    replica: 'm-g',
    release: 'rel-2',
    grace_sec: 30,
    last: { status: 503, body: 'database not ready' },
  }, { next: 'read the instance log' });

  const res = await submitForm(
    app.handle,
    '/services/svc-g?tab=deployments',
    { service: 'svc-g', release: 'rel-1', back: '/services/svc-g?tab=deployments' },
    { cookies: cookie, match: 'Deploy' },
  );
  assert.equal(res.status, 422);
  const body = await res.text();
  assert.match(body, /did not answer its health check within 30 s/, 'the symptom names the grace window');
  assert.match(body, /href="\/machines\/m-g"[^>]*>api-9</, 'the instance is named and linked');
  assert.match(body, /503 database not ready/, 'the last answer is quoted');
  assert.match(body, /href="\/api\/builds\/bld-g\/logs"[^>]*>Open the build log/, 'the build log is one click away');
  app.fleet.data.deployError = null;
});

test('any other deploy failure is still a plain refusal, not a diagnosis', async () => {
  app.fleet.data.deployError = new Error('the image does not exist');
  const res = await submitForm(
    app.handle,
    '/services/svc-g?tab=deployments',
    { service: 'svc-g', release: 'rel-1', back: '/services/svc-g?tab=deployments' },
    { cookies: cookie, match: 'Deploy' },
  );
  assert.equal(res.status, 502);
  assert.match(await res.text(), /Deploy refused: the image does not exist/);
  app.fleet.data.deployError = null;
});
