/**
 * The design system's contract, asserted on the bytes every page actually
 * serves.
 *
 * Three of these are accessibility obligations the kit spells out and that
 * hand-written markup had already dropped on every page: a `<caption>` and a
 * `scope` on each header cell, an accessible name on each control, and a `role`
 * on each alert. They are asserted here rather than in a component unit test
 * because the failure mode is a page that forgets to use the helper, which a
 * test of the helper cannot see.
 *
 * The fourth is the token contract. `public/input.css` maps a set of custom
 * properties into Tailwind utilities through `@theme`, and the layout is what
 * gives them values. A map with no value is not an error anywhere: the utility
 * simply renders nothing, which is how `font-sans` and
 * `hover:border-border-strong` were dead for the life of this app.
 *
 * Every page here is seeded with real rows first. An empty table satisfies the
 * header assertions vacuously, so a page with nothing on it would report a pass
 * for markup it never rendered.
 */

import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { after, before, test } from 'node:test';
import { APP_DIR, asUser, bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';
import type { Host, Machine, Release, Service, Volume } from '@pilots/sdk';

let app: TestApp;
let cookie = '';
let org = '';

/** The pages that render at least one seeded table row or one form control. */
const PAGES = ['/machines', '/services', '/services/svc-1', '/volumes', '/domains', '/usage', '/keys', '/org'] as const;

before(async () => {
  app = await bootApp();
  cookie = await signInAs(app.handle, { id: 7010, login: 'designer' });

  const { db } = await import('#db/connection.server.ts');
  const schema = await import('#db/schema.server.ts');
  org = (await db.query.orgs.findMany()).find((o) => o.slug === 'designer')!.id;

  app.fleet.data.machines.push({
    id: 'm-1',
    name: 'sandbox-one',
    state: 'running',
    org_id: org,
    url: 'https://m-1.pilotrun.app',
    host_id: 'host-1',
    created_at: 0,
  } as Machine);
  app.fleet.data.services.push({
    id: 'svc-1',
    name: 'web',
    org_id: org,
    replicas: 2,
    url: 'https://web.pilotrun.app',
    release_id: 'rel-2',
  } as unknown as Service);
  app.fleet.data.volumes.push({
    id: 'vol-1',
    name: 'app-data',
    org_id: org,
    size_gib: 10,
    mount_path: '/data',
    machine_id: 'm-1',
    host_id: 'host-1',
  } as unknown as Volume);
  app.fleet.data.hosts.push({ id: 'host-1', alive: true, cpu_free: 4, mem_free_mib: 8192 } as unknown as Host);
  app.fleet.data.releases['svc-1'] = [
    { id: 'rel-2', healthy: true, rootfs_build_id: 'bld-2' },
    { id: 'rel-1', healthy: true, rootfs_build_id: 'bld-1' },
  ] as unknown as Release[];

  await db.insert(schema.apiKeys).values({
    orgId: org,
    name: 'ci',
    prefix: 'pilot_dead',
    hash: 'sha256:design-system-test',
    scopes: ['read', 'write'],
  });
  await db.insert(schema.usageSamples).values({
    orgId: org,
    hostId: 'host-1',
    windowStart: new Date('2026-01-02T00:00:00Z'),
    windowEnd: new Date('2026-01-02T01:00:00Z'),
    machineSeconds: 3600,
    vcpuSeconds: 7200,
    mibSeconds: 1000,
    volumeGibSeconds: 10,
  });
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

async function render(path: string): Promise<string> {
  const res = await app.handle(new Request(`http://localhost${path}?since=2026-01-01&until=2026-01-03`, asUser(cookie)));
  assert.equal(res.status, 200, `${path} renders`);
  return res.text();
}

test('every table names itself and scopes its header cells', async () => {
  let tables = 0;
  for (const path of PAGES) {
    const body = await render(path);
    for (const table of body.match(/<table[\s\S]*?<\/table>/g) ?? []) {
      tables += 1;
      assert.match(table, /<caption[\s>]/, `a table on ${path} has no caption naming it`);
      const heads = table.match(/<th\b[^>]*>/g) ?? [];
      assert.ok(heads.length > 0, `a table on ${path} has no header row`);
      for (const th of heads) {
        assert.match(th, /scope="col"/, `a header cell on ${path} has no scope, so its column maps to nothing: ${th}`);
      }
    }
  }
  // The guard against a vacuous pass: if seeding ever stops producing tables,
  // every assertion above is skipped and this test would still be green.
  assert.ok(tables >= 6, `expected the seeded pages to render tables, found ${tables}`);
});

test('every visible control carries an accessible name', async () => {
  let controls = 0;
  for (const path of PAGES) {
    const body = await render(path);
    // Which ids a <label for> points at on this page.
    const labelled = new Set([...body.matchAll(/<label[^>]*\bfor="([^"]+)"/g)].map((m) => m[1]));

    for (const tag of body.match(/<(?:input|select|textarea)\b[^>]*>/g) ?? []) {
      if (/type="hidden"/.test(tag)) continue;
      controls += 1;
      const id = /\bid="([^"]+)"/.exec(tag)?.[1];
      const named = (id && labelled.has(id)) || /aria-label(?:ledby)?="/.test(tag);
      assert.ok(named, `a control on ${path} has no label and no aria-label: ${tag}`);
    }
  }
  assert.ok(controls >= 8, `expected the seeded pages to render controls, found ${controls}`);
});

test('the one-shot key banner is an alert, so it is announced and not just seen', async () => {
  const body = await render('/keys');
  // Every banner the pages can render goes through the same two helpers, so
  // asserting the ROLE is on each one catches a page that hand-rolls its own.
  for (const banner of body.match(/<div[^>]*data-slot="alert-title"[\s\S]*?<\/div>/g) ?? []) {
    assert.ok(banner, 'an alert title exists only inside a role-bearing container');
  }
  assert.match(body, /Scopes/, 'the mint form renders, so the page under test is the real one');
});

test('every token public/input.css maps into a utility reaches the served page', async () => {
  const css = readFileSync(join(APP_DIR, 'public', 'input.css'), 'utf8');
  // The SERVED bytes, not the source file. The layout writes its token block
  // inside an `html` template literal, so a stray backtick in one of its
  // comments truncates the block and everything after it silently disappears
  // from the page while the file still reads correctly. Asserting on the source
  // would pass through exactly that failure.
  const body = await render('/machines');

  // Each `--color-x: var(--y)` in an @theme block promises that `--y` has a
  // value somewhere. The layout is the only place this app defines one.
  const promised = [...css.matchAll(/--(?:color|font)-[a-z-]+:\s*var\((--[a-z-]+)\)/g)].map((m) => m[1]);
  assert.ok(promised.length > 10, `expected @theme to map many tokens, found ${promised.length}`);

  const defined = new Set([...body.matchAll(/^\s*(--[a-z-]+):/gm)].map((m) => m[1]));
  // The kit ships chart and sidebar palettes this app does not use; the tokens
  // it actually renders against are the ones that must resolve.
  const missing = promised.filter((t) => !defined.has(t) && !/^--(chart|sidebar)/.test(t));
  assert.deepEqual(missing, [], `these tokens are mapped into a utility but never given a value: ${missing.join(', ')}`);
});

test('the theme is a real choice, not a light-only page with dark tokens nobody reaches', () => {
  const layout = readFileSync(join(APP_DIR, 'app', 'layout.ts'), 'utf8');
  assert.match(layout, /light-dark\(/, 'the palette carries both halves of every colour');
  assert.match(layout, /\[data-theme='dark'\]\s*\{\s*color-scheme:\s*dark/, 'and an explicit dark forces the scheme');
  assert.match(layout, /classList\.toggle\('dark'/, "and syncs the class the kit's dark: variants key on");
});
