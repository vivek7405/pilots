/**
 * The app list at `/`: the first step of the journey.
 *
 * One card per app with its service count as a status line, a thumbnail
 * drawn from the same layout the canvas uses, and a whole-card link to the
 * canvas; a service outside any app listed below under its own name; a sort
 * that works as a plain GET; and an empty org told what to do.
 *
 * Seeded on purpose: an org with no rows satisfies every assertion below
 * vacuously except the last.
 *
 * Counterfactuals: group by name instead of `app` and the card test fails,
 * because web and api land on two cards; ignore `?sort=` and the sort test
 * fails on the order; count only running instances as online and the
 * sleeping service turns `2/2` into `1/2`.
 */

import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { asUser, bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';
import type { Host, Machine, Release, Service, Volume } from '@pilots/sdk';

let app: TestApp;
let cookie = '';
let org = '';

const NOW_SEC = Math.floor(Date.now() / 1000);

before(async () => {
  app = await bootApp();
  cookie = await signInAs(app.handle, { id: 7300, login: 'lister' });

  const { db } = await import('#db/connection.server.ts');
  org = (await db.query.orgs.findMany()).find((o) => o.slug === 'lister')!.id;

  app.fleet.data.hosts.push({ id: 'h-1', alive: true, cpu_free: 6, mem_free_mib: 20480 } as unknown as Host);
  // Two apps and one loose service. `gallery` deployed an hour ago and was
  // created first; `shop` deployed a day ago and was created last.
  app.fleet.data.services.push(
    ...([
      { id: 'svc-web', name: 'web', org_id: org, app: 'gallery', replicas: 1, url: 'https://web.example', release_id: 'rel-w', depends_on: ['api'], created_at: 100 },
      { id: 'svc-api', name: 'api', org_id: org, app: 'gallery', replicas: 1, url: 'https://api.example', release_id: 'rel-a', created_at: 200 },
      { id: 'svc-store', name: 'store', org_id: org, app: 'shop', replicas: 1, url: 'https://store.example', release_id: 'rel-s', created_at: 300 },
      { id: 'svc-docs', name: 'docs', org_id: org, replicas: 1, url: 'https://docs.example', release_id: 'rel-d', created_at: 400 },
    ] as unknown as Service[]),
  );
  app.fleet.data.releases['svc-web'] = [{ id: 'rel-w', healthy: true, created_at: NOW_SEC - 3600 }] as unknown as Release[];
  app.fleet.data.releases['svc-api'] = [{ id: 'rel-a', healthy: true, created_at: NOW_SEC - 7200 }] as unknown as Release[];
  app.fleet.data.releases['svc-store'] = [{ id: 'rel-s', healthy: true, created_at: NOW_SEC - 86_400 }] as unknown as Release[];
  app.fleet.data.releases['svc-docs'] = [{ id: 'rel-d', healthy: true, created_at: NOW_SEC - 60 }] as unknown as Release[];
  app.fleet.data.machines.push(
    ...([
      // web is awake, api is asleep: both answer a request, so both are online.
      { id: 'm-web', name: 'web-1', state: 'running', org_id: org, host_id: 'h-1', service_id: 'svc-web', release_id: 'rel-w', vcpus: 2, mem_mib: 2048 },
      { id: 'm-api', name: 'api-1', state: 'suspended', org_id: org, host_id: 'h-1', service_id: 'svc-api', release_id: 'rel-a', vcpus: 1, mem_mib: 1024 },
      { id: 'm-store', name: 'store-1', state: 'running', org_id: org, host_id: 'h-1', service_id: 'svc-store', release_id: 'rel-s', vcpus: 1, mem_mib: 512 },
      { id: 'm-docs', name: 'docs-1', state: 'running', org_id: org, host_id: 'h-1', service_id: 'svc-docs', release_id: 'rel-d', vcpus: 1, mem_mib: 512 },
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

async function list(query = ''): Promise<string> {
  const res = await app.handle(new Request(`http://localhost/${query}`, asUser(cookie)));
  assert.equal(res.status, 200, 'signed in, / is the app list');
  return res.text();
}

/** The app names in the order their cards appear. */
function cardOrder(body: string): string[] {
  return [...body.matchAll(/data-href="\/apps\/([^"]+)"/g)].map((m) => decodeURIComponent(m[1]!));
}

test('one card per app, linking to its canvas, with a status line and a thumbnail', async () => {
  const body = await list();
  assert.deepEqual(new Set(cardOrder(body)), new Set(['gallery', 'shop']), 'two apps, two cards');

  const gallery = body.slice(body.indexOf('data-href="/apps/gallery"'), body.indexOf('data-href="/apps/shop"'));
  assert.match(gallery, /2\/2 services online/, 'a sleeping service still answers, so it counts as online');
  assert.match(gallery, /<svg[^>]*role="img"[^>]*aria-label="2 services, 1 connection"/, 'the thumbnail is the canvas');
  assert.match(gallery, /deployed\s*<relative-time/, 'the last deploy across the app');
  assert.ok(!gallery.includes('>docs<'), 'the loose service is not on an app card');
});

test('a service with no app is listed under its own name below the grid', async () => {
  const body = await list();
  const start = body.indexOf('Not in an app');
  assert.ok(start > 0, 'the section exists');
  const section = body.slice(start);
  assert.match(section, /Services that belong to no app/);
  assert.match(section, /href="\/services\/svc-docs"/);
  assert.match(section, /data-href="\/services\/svc-docs"/, 'the row navigates');
});

test('the sort is a GET parameter, applied on the server', async () => {
  // Recent activity is the default: gallery deployed an hour ago, shop a day ago.
  assert.deepEqual(cardOrder(await list()), ['gallery', 'shop']);
  // Newest created first: shop's oldest service was created after gallery's.
  assert.deepEqual(cardOrder(await list('?sort=created')), ['shop', 'gallery']);
  assert.deepEqual(cardOrder(await list('?sort=name')), ['gallery', 'shop']);

  const body = await list('?sort=name');
  assert.match(body, /<option value="name" selected(="")?>/, 'the select shows the sort in force');
  assert.match(body, /<form method="get" action="\/"/, 'and it is a plain form');
  assert.match(body, /<list-filter[^>]*\bfor="apps"[^>]*placeholder="Search apps"/);
});

test('the list view is the same apps as rows, one link each', async () => {
  const body = await list('?view=list');
  const table = body.slice(body.indexOf('<table'), body.indexOf('</table>'));
  assert.match(table, /Your apps/);
  assert.match(table, /data-href="\/apps\/gallery"/);
  assert.match(table, /data-href="\/apps\/shop"/);
  assert.ok(!table.includes('<svg'), 'no thumbnails in the list view');
  assert.match(body, /href="\/\?sort=activity&amp;view=grid"[^>]*aria-current="false"/);
  assert.match(body, /href="\/\?sort=activity&amp;view=list"[^>]*aria-current="true"/);
});

test('the four limits carry the team ceiling and the current use, in the user words', async () => {
  const body = await list();
  const bars = [...body.matchAll(/<progress[^>]*>/g)].map((m) => m[0]);
  assert.equal(bars.length, 4, `expected four bars, found ${bars.length}`);
  for (const bar of bars) assert.match(bar, /aria-label="/);
  assert.ok(bars.some((b) => /aria-label="Instances: 4 of 10"/.test(b)), `instances bar: ${bars.join(' | ')}`);
  assert.ok(bars.some((b) => /aria-label="vCPUs: 5 of 16"/.test(b)), `vcpu bar: ${bars.join(' | ')}`);
  assert.ok(bars.some((b) => /aria-label="Memory: 4096 of 8192 MiB"/.test(b)), `memory bar: ${bars.join(' | ')}`);
  assert.ok(bars.some((b) => /aria-label="Storage: 25 of 100 GiB"/.test(b)), `storage bar: ${bars.join(' | ')}`);
  assert.match(body, /<hosts-strip/, 'capacity is on the list, where the room left matters');
});

test('an org with nothing in it is told what to do, not just that it is empty', async () => {
  const other = await signInAs(app.handle, { id: 7301, login: 'newcomer' });
  const res = await app.handle(new Request('http://localhost/', asUser(other)));
  const body = await res.text();

  assert.equal(res.status, 200);
  assert.match(body, /No apps yet/);
  assert.match(body, /pilot deploy/, 'the empty state carries the command that ends it');
  assert.match(body, /Deploy an app/, 'and a link to the page that explains it');
});
