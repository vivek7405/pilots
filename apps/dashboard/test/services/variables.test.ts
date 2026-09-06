/**
 * The Variables and Metrics tabs, and the save that sits behind Variables.
 *
 * The one property that matters most here is negative: a secret's VALUE never
 * comes back. It goes to hostd in the patch and is then gone from this app;
 * the table lists names, the result carries no value, and nothing is logged.
 */
import assert from 'node:assert/strict';
import { before, test } from 'node:test';
import { submitForm } from '@webjsdev/server/testing';
import { asUser, bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';
import type { Machine, Service } from '@pilots/sdk';

let app: TestApp;
let cookie: string;
let org = '';
const NOW_SEC = Math.floor(Date.now() / 1000);

before(async () => {
  app = await bootApp();
  cookie = await signInAs(app.handle, { id: 7700, login: 'variabler' });
  const { db } = await import('#db/connection.server.ts');
  org = (await db.query.orgs.findMany()).find((o) => o.slug === 'variabler')!.id;
  app.fleet.data.services.push({ id: 'svc-var', name: 'api', org_id: org, app: 'shop', replicas: 2 } as unknown as Service);
  app.fleet.data.machines.push(
    ...([
      { id: 'm-a', name: 'api-1', state: 'running', org_id: org, service_id: 'svc-var', vcpus: 2, mem_mib: 2048, last_start: 'restore', last_start_at: NOW_SEC - 300 },
      { id: 'm-b', name: 'api-2', state: 'suspended', org_id: org, service_id: 'svc-var', vcpus: 1, mem_mib: 512, last_start: 'boot', last_start_at: NOW_SEC - 3600 },
    ] as unknown as Machine[]),
  );
});

async function tab(name: string): Promise<string> {
  const res = await app.handle(new Request(`http://localhost/services/svc-var?tab=${name}`, asUser(cookie)));
  assert.equal(res.status, 200);
  return res.text();
}

test('the Variables tab starts empty, explains itself, and lists what pilots provides', async () => {
  const body = await tab('variables');
  assert.match(body, /No variables set from here/);
  assert.match(body, /applied but not listed/, 'the footnote says what the API will not return');
  assert.match(body, /3 things pilots provides to every service/);
  assert.match(body, /<details/, 'the provided list is a collapsible');
  assert.match(body, /PORT/);
  assert.equal((body.match(/name="secret_name"/g) ?? []).length, 4, 'four secret rows render on the server');
  assert.match(body, /name="confirm"/, 'the replace-the-set confirm is a real checkbox');
});

test('the Metrics tab shows each instance and says what is not recorded', async () => {
  const body = await tab('metrics');
  assert.match(body, /api-1/);
  assert.match(body, /api-2/);
  assert.match(body, /2 vCPU/);
  assert.match(body, /2\.0 GB/);
  assert.match(body, /Resumed/, 'how an instance last came up, in a word');
  assert.equal((body.match(/Not recorded yet/g) ?? []).length, 2, 'CPU and memory over time both say so');
  assert.ok(!/Last 15 min|Pause/.test(body), 'no toolbar for data that does not exist');
});

test('saving without the confirm is refused and touches nothing', async () => {
  app.fleet.calls.length = 0;
  const res = await submitForm(
    app.handle,
    '/services/svc-var?tab=variables',
    { service: 'svc-var', back: '/services/svc-var?tab=variables', env: 'A=1' },
    { cookies: cookie, match: 'Save' },
  );
  assert.equal(res.status, 422);
  assert.match(await res.text(), /Tick the box/);
  assert.ok(!app.fleet.calls.some((c) => c.method === 'services.patch'), 'no patch without the confirm');
});

test('an empty save and a malformed line are refused with the line named', async () => {
  app.fleet.calls.length = 0;
  const empty = await submitForm(
    app.handle,
    '/services/svc-var?tab=variables',
    { service: 'svc-var', back: '/services/svc-var?tab=variables', env: '', confirm: 'on' },
    { cookies: cookie, match: 'Save' },
  );
  assert.equal(empty.status, 422);
  assert.match(await empty.text(), /Nothing to save/);

  const bad = await submitForm(
    app.handle,
    '/services/svc-var?tab=variables',
    { service: 'svc-var', back: '/services/svc-var?tab=variables', env: 'A=1\nNOEQUALS', confirm: 'on' },
    { cookies: cookie, match: 'Save' },
  );
  assert.equal(bad.status, 422);
  assert.match(await bad.text(), /Line 2 has no "="/);
  assert.ok(!app.fleet.calls.some((c) => c.method === 'services.patch'));
});

test('a valid save patches hostd with both kinds, stores names only, and returns to the tab', async () => {
  app.fleet.calls.length = 0;
  const res = await submitForm(
    app.handle,
    '/services/svc-var?tab=variables',
    {
      service: 'svc-var',
      back: '/services/svc-var?tab=variables',
      env: 'NODE_ENV=production\n# a comment\nLOG_LEVEL=info',
      // The form helper cannot repeat a field, so one secret row is sent; the
      // action pairs names and values by position either way.
      secret_name: 'DB_URL',
      secret_value: 'postgres://secret-value',
      confirm: 'on',
    },
    { cookies: cookie, match: 'Save' },
  );
  assert.equal(res.status, 303);
  assert.equal(res.headers.get('location'), '/services/svc-var?tab=variables&ok=variables-saved');

  const patch = app.fleet.calls.find((c) => c.method === 'services.patch');
  assert.ok(patch, 'hostd was patched');
  assert.deepEqual(patch!.args, [
    'svc-var',
    { env: { NODE_ENV: 'production', LOG_LEVEL: 'info' }, secret_env: { DB_URL: 'postgres://secret-value' } },
  ]);

  // Names only reached this app's database.
  const { db } = await import('#db/connection.server.ts');
  const rows = await db.query.serviceVariables.findMany();
  const mine = rows.filter((r) => r.serviceId === 'svc-var');
  assert.deepEqual(
    mine.map((r) => [r.name, r.secret]).sort(),
    [
      ['DB_URL', true],
      ['LOG_LEVEL', false],
      ['NODE_ENV', false],
    ],
  );
  assert.ok(!JSON.stringify(rows).includes('secret-value'), 'the value is nowhere in the database');

  // And the tab lists them, masked.
  const body = await tab('variables');
  assert.match(body, /DB_URL/);
  assert.match(body, /•••••••/);
  assert.ok(!body.includes('secret-value'), 'the value is nowhere on the page');
});

test('saving one kind leaves the other kind alone on hostd', async () => {
  app.fleet.calls.length = 0;
  const res = await submitForm(
    app.handle,
    '/services/svc-var?tab=variables',
    { service: 'svc-var', back: '/services/svc-var?tab=variables', env: 'ONLY=plain', confirm: 'on' },
    { cookies: cookie, match: 'Save' },
  );
  assert.equal(res.status, 303);
  const patch = app.fleet.calls.find((c) => c.method === 'services.patch')!;
  assert.deepEqual(patch.args, ['svc-var', { env: { ONLY: 'plain' } }], 'no secret_env key at all, so hostd keeps its secrets');
});
