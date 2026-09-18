/**
 * The /logs page: one source per RUNNING machine of the org, named by its
 * service or "Sandbox", plus a scripting-off raw link each, and an empty state
 * when nothing is up.
 */
import assert from 'node:assert/strict';
import { before, test } from 'node:test';
import { asUser, bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';
import type { Machine, Service } from '@pilots/sdk';

let app: TestApp;
let cookie: string;
let org = '';

before(async () => {
  app = await bootApp();
  cookie = await signInAs(app.handle, { id: 7601, login: 'logs-pilot' });
  const { db } = await import('#db/connection.server.ts');
  org = (await db.query.orgs.findMany()).find((o) => o.slug === 'logs-pilot')!.id;
  app.fleet.data.services.push({ id: 'svc-web', name: 'web', org_id: org, app: 'shop', replicas: 1 } as unknown as Service);
  app.fleet.data.machines.push(
    ...([
      { id: 'm-web', name: 'web-1', state: 'running', org_id: org, service_id: 'svc-web' },
      { id: 'm-box', name: 'calm-box-ab12', state: 'running', org_id: org },
      { id: 'm-off', name: 'off-1', state: 'suspended', org_id: org, service_id: 'svc-web' },
      { id: 'm-other', name: 'other-1', state: 'running', org_id: 'someone-else' },
    ] as unknown as Machine[]),
  );
});

async function logs(ck = cookie): Promise<string> {
  const res = await app.handle(new Request('http://localhost/logs', asUser(ck)));
  assert.equal(res.status, 200);
  return res.text();
}

test('a source per running machine of the org, and none suspended or foreign', async () => {
  const body = await logs();
  assert.match(body, /<log-stream/, 'the stream element renders');
  // Scope the source check to the raw-link list, which is exactly the sources
  // the stream opens. The whole body also carries the command palette's search
  // index, which indexes every machine and is not what this asserts.
  // The layout has its own <noscript> (the Account link), so find the one
  // that carries the raw log links rather than the first on the page.
  const start = body.indexOf('<noscript>', body.indexOf('/api/machines/') - 200);
  const noscript = body.slice(start, body.indexOf('</noscript>', start));
  assert.match(noscript, /m-web/, 'the running replica is a source');
  assert.match(noscript, /m-box/, 'the running sandbox is a source');
  assert.ok(!noscript.includes('m-off'), 'a suspended machine has no live guest to tail');
  assert.ok(!noscript.includes('m-other'), 'another org never leaks in');
});

test('a running instance is named by its service, a loose one is a Sandbox', async () => {
  const body = await logs();
  assert.match(body, /web/, 'the service name labels its instance');
  assert.match(body, /Sandbox/, 'a machine with no service is a Sandbox');
});

test('scripting off gets a raw link per source', async () => {
  const body = await logs();
  assert.match(body, /<noscript>/);
  assert.match(body, /href="\/api\/machines\/m-web\/logs"/);
  assert.match(body, /href="\/api\/machines\/m-box\/logs"/);
});

test('an org with nothing running is told so, not shown an empty table', async () => {
  const empty = await signInAs(app.handle, { id: 7602, login: 'quiet-pilot' });
  const body = await logs(empty);
  assert.match(body, /Nothing is running/);
  assert.ok(!body.includes('<log-stream'), 'no stream when there is nothing to stream');
});
