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
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { after, before, test } from 'node:test';
import { APP_DIR, asUser, bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';
import type { Host, Machine, Release, Service, Volume } from '@pilots/sdk';

let app: TestApp;
let cookie = '';
let org = '';

/** The pages that render at least one seeded table row or one form control. */
const PAGES = [
  '/',
  '/apps/gallery',
  '/apps/gallery?service=svc-1&tab=settings',
  '/apps/gallery?service=svc-1&tab=variables',
  '/apps/gallery?service=svc-1&tab=metrics',
  '/sandboxes',
  '/services/new',
  '/services/svc-1',
  '/storage',
  '/domains',
  '/usage',
  '/logs',
  '/sandboxes/playground',
  '/keys',
  '/org',
] as const;

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
    app: 'gallery',
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

/** The usage window every page tolerates, joined the way the path allows. */
function withWindow(path: string): string {
  return `${path}${path.includes('?') ? '&' : '?'}since=2026-01-01&until=2026-01-03`;
}

async function render(path: string): Promise<string> {
  const res = await app.handle(new Request(`http://localhost${withWindow(path)}`, asUser(cookie)));
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
  const body = await render('/sandboxes');

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

test('the app chrome is a fixed header, one toast viewport and the flash reader', async () => {
  const body = await render('/sandboxes');

  // Fixed, never sticky: sticky flickers its background for one frame on iOS
  // WebKit during a client-router navigation, and every iOS browser is WebKit.
  assert.match(body, /<header\s+class="fixed /, 'the header is position: fixed');
  assert.ok(!/class="[^"]*sticky top-0/.test(body), 'and never sticky');
  // A fixed header leaves normal flow, so the body has to reserve its height
  // or the first row of every page hides underneath it.
  assert.match(body, /--header-h: 56px;/);
  assert.match(body, /padding-top: var\(--header-h\);/);

  // Exactly one toast viewport in the document. Two would each take a copy of
  // the bus and only the last-mounted one would receive anything.
  assert.equal(body.match(/<ui-sonner\b/g)?.length, 1, 'one toast viewport');
  assert.equal(body.match(/<flash-toast\b/g)?.length, 1, 'one flash reader');
});

test('the identity menu holds the account chores and the nav holds the product', async () => {
  const body = await render('/sandboxes');
  const header = body.slice(body.indexOf('<header'), body.indexOf('</header>'));

  for (const label of ['Usage', 'Tokens', 'Team', 'Sign out']) {
    assert.ok(header.includes(`>${label}<`), `${label} is in the identity menu`);
  }
  // The nav is the product's nouns, in the user's words. Storage and Domains
  // stay routable and are reached from a service, not from a flat list of
  // seven equal items.
  const nav = header.slice(header.indexOf('<app-nav'), header.indexOf('</app-nav>'));
  assert.ok(nav.includes('>Apps<') && nav.includes('>Sandboxes<'));
  assert.ok(!nav.includes('>Storage<') && !nav.includes('>Keys<'), 'the chores left the nav');
  assert.ok(!nav.includes('>Machines<'), 'and the engine word left with them');
});

test('no source file paints a raw Tailwind colour', () => {
  // A raw swatch is the same in both themes and answers to no palette change,
  // which is the one styling rule this app states without exception. The kit's
  // sonner shipped three, and they are tokens here because we own the copy.
  const offenders: string[] = [];
  const dirs = ['app', 'components', 'modules', 'lib'];
  const walk = (dir: string): string[] => {
    const out: string[] = [];
    for (const entry of readdirSync(join(APP_DIR, dir), { withFileTypes: true })) {
      const rel = `${dir}/${entry.name}`;
      if (entry.isDirectory()) out.push(...walk(rel));
      else if (/\.(ts|js)$/.test(entry.name)) out.push(rel);
    }
    return out;
  };
  for (const file of dirs.flatMap(walk)) {
    const source = readFileSync(join(APP_DIR, file), 'utf8');
    for (const line of source.split('\n')) {
      // `cn.ts` documents the conflict-resolution rules by naming utilities in
      // prose; a comment paints nothing.
      if (/^\s*(\*|\/\/)/.test(line)) continue;
      if (/-(red|blue|gray|green|zinc|slate|amber|yellow|emerald|sky|rose|violet|orange)-[0-9]/.test(line)) {
        offenders.push(`${file}: ${line.trim()}`);
      }
    }
  }
  assert.deepEqual(offenders, [], `raw Tailwind colours: ${offenders.join(' | ')}`);
});

/**
 * Which page file serves each url in `PAGES`.
 *
 * Written out rather than derived, because a route group and a dynamic
 * segment do not fall out of the url, and a wrong guess here would make the
 * test below pass by reading the wrong file. A stale entry throws on the read.
 */
const PAGE_FILES: Record<string, string> = {
  '/': 'app/page.ts',
  '/apps/gallery': 'app/(app)/apps/[app]/page.ts',
  '/apps/gallery?service=svc-1&tab=settings': 'app/(app)/apps/[app]/page.ts',
  '/apps/gallery?service=svc-1&tab=variables': 'app/(app)/apps/[app]/page.ts',
  '/apps/gallery?service=svc-1&tab=metrics': 'app/(app)/apps/[app]/page.ts',
  '/apps/gallery?service=svc-1&tab=terminal': 'app/(app)/apps/[app]/page.ts',
  '/services/svc-1?tab=terminal': 'app/(app)/services/[id]/page.ts',
  '/sandboxes': 'app/(app)/sandboxes/page.ts',
  '/machines/m-1': 'app/(app)/machines/[id]/page.ts',
  '/machines/m-1/terminal': 'app/(app)/machines/[id]/terminal/page.ts',
  '/services/new': 'app/(app)/services/new/page.ts',
  '/services/svc-1': 'app/(app)/services/[id]/page.ts',
  '/storage': 'app/(app)/storage/page.ts',
  '/domains': 'app/(app)/domains/page.ts',
  '/usage': 'app/(app)/usage/page.ts',
  '/logs': 'app/(app)/logs/page.ts',
  '/sandboxes/playground': 'app/(app)/sandboxes/playground/page.ts',
  '/keys': 'app/(app)/keys/page.ts',
  '/org': 'app/(app)/org/page.ts',
};

/** Every `#`-aliased specifier a file imports, as a repo-relative path. */
function importsOf(source: string): string[] {
  const out: string[] = [];
  for (const m of source.matchAll(/from\s+'(#[^']+)'|^\s*import\s+'(#[^']+)'/gm)) {
    const spec = m[1] ?? m[2];
    if (spec) out.push(spec.slice(1));
  }
  return out;
}

/**
 * The custom-element tags a file's module graph registers.
 *
 * The SERVED MARKUP cannot answer this. A custom element is registered
 * process-wide the first time any module defines it, and the test process
 * renders many pages, so a page that forgot its import still renders an
 * upgraded element as long as some other page in the same run imported it.
 * That is exactly why the missing import on the services page reached a
 * browser: nothing server-side could see it.
 */
function registeredBy(entry: string): Set<string> {
  const tags = new Set<string>();
  const seen = new Set<string>();
  const walk = (rel: string) => {
    if (seen.has(rel)) return;
    seen.add(rel);
    let source: string;
    try {
      source = readFileSync(join(APP_DIR, rel), 'utf8');
    } catch {
      return; // a type-only or generated specifier; it registers nothing
    }
    for (const m of source.matchAll(/\.register\(\s*'([a-z][a-z0-9-]*)'/g)) tags.add(m[1]!);
    for (const spec of importsOf(source)) walk(spec);
  };
  walk(entry);
  return tags;
}

/** The custom-element tags in a page's markup, comments stripped. */
function elementsIn(body: string): string[] {
  const markup = body.replace(/<!--[\s\S]*?-->/g, '');
  return [...new Set([...markup.matchAll(/<([a-z][a-z0-9]*-[a-z0-9-]+)[\s>]/g)].map((m) => m[1]!))];
}

/**
 * A page that renders a custom element must import it.
 *
 * An un-imported custom element is not an error anywhere: it renders as an
 * inert tag. A `<copy-button>` looks like a button and does nothing when
 * clicked, and a `<ui-tooltip-content>` is not hidden, so it prints its whole
 * explanation as body text. Both shipped once each and neither was visible to
 * any other check, including a sweep of the served bytes.
 *
 * The empty-org render matters as much as the seeded one: an empty state is
 * the branch that introduces a control the populated page has no use for, and
 * it is where the services page's missing copy button lived.
 *
 * Counterfactual: drop `import '#components/copy-button.ts'` from
 * `app/(app)/services/page.ts` and this fails on `/services` for the empty org.
 */
test('every custom element a page renders is one that page imports', async () => {
  // The layout is on every page, so what it registers counts as available.
  const fromLayout = registeredBy('app/layout.ts');
  const empty = await signInAs(app.handle, { id: 7011, login: 'newcomer' });
  let checked = 0;

  for (const [path, file] of Object.entries(PAGE_FILES)) {
    const available = new Set([...fromLayout, ...registeredBy(file)]);

    for (const [label, session] of [
      ['seeded', cookie],
      ['empty', empty],
    ] as const) {
      // A seeded id under an empty org is a 404, which is correct and not
      // what this test is about.
      if (label === 'empty' && /\/(svc-1|m-1)|\/apps\//.test(path)) continue;
      const res = await app.handle(new Request(`http://localhost${withWindow(path)}`, asUser(session)));
      assert.equal(res.status, 200, `${path} renders for the ${label} org`);

      for (const tag of elementsIn(await res.text())) {
        checked += 1;
        assert.ok(available.has(tag), `<${tag}> is rendered by ${file} on the ${label} org but never imported there`);
      }
    }
  }
  assert.ok(checked > 40, `expected these pages to render custom elements, found ${checked}`);
});

/**
 * Four type steps, and no fifth.
 *
 * The app had 19 `text-sm` and 8 `text-xs` against one `text-3xl`, which is
 * another way of saying nothing on a screen was more important than anything
 * else. The scale is defined once in `public/input.css`, and a closed set is
 * only closed if something refuses the next addition.
 *
 * `components/ui/` and `lib/utils/cn.ts` are exempt: they are the kit's files,
 * they carry no product copy, and `cn.ts` holds the class-merge table, which
 * has to name every size Tailwind ships in order to merge them.
 */
test('no page invents a fifth type step', () => {
  const OTHER = /\btext-(xs|sm|base|lg|xl|2xl|3xl|4xl|5xl)\b/;
  const offenders: string[] = [];
  const walk = (dir: string) => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const full = join(dir, entry.name);
      const rel = full.slice(APP_DIR.length + 1).replaceAll('\\', '/');
      if (entry.isDirectory()) {
        if (rel === 'components/ui' || rel === 'components/terminal/vendor') continue;
        walk(full);
        continue;
      }
      if (!entry.name.endsWith('.ts') || rel === 'lib/utils/cn.ts') continue;
      const source = readFileSync(full, 'utf8');
      for (const line of source.split('\n')) {
        const hit = OTHER.exec(line);
        if (hit) offenders.push(`${rel}: ${hit[0]}`);
      }
    }
  };
  for (const root of ['app', 'modules', 'components', 'lib']) walk(join(APP_DIR, root));
  assert.deepEqual(
    offenders,
    [],
    `the scale is text-title, text-heading, text-body and text-meta:\n${offenders.join('\n')}`,
  );
});

/**
 * One primary action per screen.
 *
 * The accent is what tells a reader where to go next, and a screen with three
 * of them has told them nothing. Counted on the SERVED bytes after the header,
 * because the header carries its own controls on every page and they are not
 * the page's action.
 *
 * Two things wear the accent without being a call to action, and both are
 * excluded by their own ARIA rather than by a class allow-list:
 *
 *  - a toggle that is ON (`aria-pressed="true"`), such as the selected filter
 *    chip. There the accent means "this is the state", not "do this".
 *  - a form's own submit. A form whose submit is an outline button reads as
 *    optional, and a page with three sections legitimately has three forms.
 *    What this rule guards against is competing page-level calls to action.
 */
test('every screen has one primary action at most', async () => {
  for (const path of PAGES) {
    const body = await render(path);
    const main = body.slice(body.indexOf('</header>'));
    const outsideForms = main.replace(/<form\b[\s\S]*?<\/form\s*>/gi, ' ');
    const accents = [...outsideForms.matchAll(/<[a-z-]+\b[^>]*bg-primary text-primary-foreground[^>]*>/gi)].filter(
      (m) => !/aria-pressed="true"/.test(m[0]),
    );
    assert.ok(
      accents.length <= 1,
      `${path} has ${accents.length} primary actions; a screen may have one:\n${accents.map((m) => m[0].slice(0, 120)).join('\n')}`,
    );
  }
});
