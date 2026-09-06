/**
 * The overview: the screen a signed-in visitor now lands on.
 *
 * It used to be a redirect to the machines list, so this file starts by
 * proving there is no redirect left, then that each of the four things the
 * page exists to show is actually on it. The seeding is deliberate: an org
 * with no rows satisfies every assertion below vacuously.
 *
 * Counterfactual: group the services by name instead of by `app` and the app
 * card test fails, because the two services that resolve each other at
 * `<name>.internal` would land in different cards.
 */

import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { asUser, bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';
import type { Host, Machine, Release, Service, Volume } from '@pilots/sdk';

let app: TestApp;
let cookie = '';
let org = '';

const NOW = Date.now();

before(async () => {
  app = await bootApp();
  cookie = await signInAs(app.handle, { id: 7300, login: 'overviewer' });

  const { db } = await import('#db/connection.server.ts');
  org = (await db.query.orgs.findMany()).find((o) => o.slug === 'overviewer')!.id;

  app.fleet.data.hosts.push(
    ...([
      { id: 'h-amd-1', alive: true, cpu_free: 6, mem_free_mib: 20480, cpu_vendor: 'AuthenticAMD' },
      { id: 'h-amd-2', alive: false, cpu_free: 0, mem_free_mib: 0, cpu_vendor: 'AuthenticAMD' },
    ] as unknown as Host[]),
  );
  // Two services in ONE app group, and one outside it.
  app.fleet.data.services.push(
    ...([
      { id: 'svc-web', name: 'web', org_id: org, app: 'gallery', replicas: 1, url: 'https://web.example', release_id: 'rel-w' },
      { id: 'svc-api', name: 'api', org_id: org, app: 'gallery', replicas: 1, url: 'https://api.example', release_id: 'rel-a' },
      { id: 'svc-docs', name: 'docs', org_id: org, replicas: 1, url: 'https://docs.example', release_id: 'rel-d' },
    ] as unknown as Service[]),
  );
  for (const [id, release] of [
    ['svc-web', 'rel-w'],
    ['svc-api', 'rel-a'],
    ['svc-docs', 'rel-d'],
  ] as const) {
    app.fleet.data.releases[id] = [
      { id: release, healthy: true, created_at: NOW - 3_600_000 },
    ] as unknown as Release[];
  }
  app.fleet.data.machines.push(
    // One replica, and three sandboxes: one running, one that resumes warm,
    // one whose vendor has no live host and so will cold-boot.
    ...([
      { id: 'm-rep', name: 'web-1', state: 'running', org_id: org, host_id: 'h-amd-1', service_id: 'svc-web', release_id: 'rel-w', vcpus: 2, mem_mib: 2048 },
      { id: 'm-run', name: 'box-run', state: 'running', org_id: org, host_id: 'h-amd-1', vcpus: 1, mem_mib: 512 },
      { id: 'm-warm', name: 'box-warm', state: 'suspended', org_id: org, host_id: 'h-amd-1', vcpus: 1, mem_mib: 512 },
      { id: 'm-cold', name: 'box-cold', state: 'suspended', org_id: org, host_id: 'h-gone', vcpus: 1, mem_mib: 512 },
    ] as unknown as Machine[]),
  );
  app.fleet.data.volumes.push({ id: 'vol-1', name: 'data', org_id: org, size_gib: 25 } as unknown as Volume);
  app.fleet.data.quotas = {
    org_id: org,
    max_machines: 10,
    max_vcpus: 16,
    max_mem_mib: 8192,
    max_volume_gib: 100,
    max_builds: 4,
    updated_at: 0,
  };
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

async function overview(): Promise<string> {
  const res = await app.handle(new Request('http://localhost/', asUser(cookie)));
  assert.equal(res.status, 200, 'signed in, / is the overview and not a redirect');
  return res.text();
}

test('services are grouped by the app they resolve each other within', async () => {
  const body = await overview();

  // One card per app group. The two gallery services share a table; docs,
  // which declares no app, gets its own.
  assert.match(body, /Services in gallery/, 'the app group names itself in the table caption');
  assert.match(body, /Services in no app group/);
  assert.match(body, /services here reach each other at &lt;name&gt;.internal/);

  const gallery = body.slice(body.indexOf('Services in gallery'));
  const upToDocs = gallery.slice(0, gallery.indexOf('Services in no app group'));
  assert.ok(upToDocs.includes('>web<') && upToDocs.includes('>api<'), 'both gallery services are in one card');
  assert.ok(!upToDocs.includes('>docs<'), 'and docs is not');
});

test('a service says how many instances are up, not just how many exist', async () => {
  const body = await overview();
  assert.match(body, /1\/1 instances online/);
});

test('the sandbox chips count by resume tier, and the replica is not among them', async () => {
  const body = await overview();
  const list = body.slice(body.indexOf('<machine-list'));

  // Three sandboxes: the service replica belongs to the Services section.
  // Each chip carries its own count, so the distribution is legible before
  // any filter is applied, and each is named in the user's words.
  assert.match(list, /All 3/);
  assert.match(list, /Online 1/);
  assert.match(list, /Sleeping \(resumes warm\) 1/);
  assert.match(list, /Sleeping \(starts fresh\) 1/);
});

test('the four quota bars carry the org ceiling and the current use', async () => {
  const body = await overview();
  const bars = [...body.matchAll(/<progress[^>]*>/g)].map((m) => m[0]);
  assert.equal(bars.length, 4, `expected four bars, found ${bars.length}`);

  // Every bar names itself: a <progress> supplies the role and the value, and
  // the accessible name is the author's job.
  for (const bar of bars) assert.match(bar, /aria-label="/);
  assert.ok(bars.some((b) => /aria-label="Machines: 4 of 10"/.test(b)), `machines bar: ${bars.join(' | ')}`);
  assert.ok(bars.some((b) => /aria-label="vCPUs: 5 of 16"/.test(b)), `vcpu bar: ${bars.join(' | ')}`);
  assert.ok(bars.some((b) => /aria-label="Memory: 3584 of 8192 MiB"/.test(b)), `memory bar: ${bars.join(' | ')}`);
  assert.ok(bars.some((b) => /aria-label="Volumes: 25 of 100 GiB"/.test(b)), `volume bar: ${bars.join(' | ')}`);
});

test('the fleet strip is on the overview, where the hosts matter', async () => {
  const body = await overview();
  assert.match(body, /<hosts-strip/);
});

test('an org with nothing in it is told what to do, not just that it is empty', async () => {
  // A second user gets their own personal org, and every row above belongs to
  // the first one, so this is a genuinely empty org rather than a mocked one.
  const other = await signInAs(app.handle, { id: 7301, login: 'newcomer' });
  const res = await app.handle(new Request('http://localhost/', asUser(other)));
  const body = await res.text();

  assert.equal(res.status, 200);
  assert.match(body, /No services yet/);
  assert.match(body, /pilot deploy/, 'the empty state carries the command that ends it');
  assert.match(body, /How to deploy/, 'and a link to the page that explains it');
});
