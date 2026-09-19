/**
 * The marketing pages owe nothing to the product they share a process with.
 *
 * `pilots.run/` and `pilots.run/dashboard` are one app, so the homepage now
 * boots alongside a database, a session layer and a fleet client. It must use
 * none of them. A signed-out visitor on a fleet whose GitHub App is not
 * configured, whose fleet API is down, or whose database is locked still gets
 * the homepage, and gets it without paying for a query or a fleet round trip
 * on the way (wake time is the product).
 *
 * Two halves. The request half proves it for the pages as they render today.
 * The import half proves it for the code path a render does not take: a
 * marketing module that imports the session is one `await currentUser()`
 * away from putting the homepage behind the database.
 *
 * Counterfactual: add `import { currentUser } from
 * '#modules/auth/queries/current-user.server.ts'` to `app/(site)/layout.ts`
 * and the import half fails naming that line; call it, and with the fleet
 * listing orgs the request half fails too.
 */
import { test, before } from 'node:test';
import assert from 'node:assert/strict';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { bootApp } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';

const ROOT = join(import.meta.dirname, '..', '..');
const PAGES = ['/', '/agents', '/architecture', '/architecture/internals', '/deploy', '/sandboxes', '/brand'];

/** The product modules a marketing file may import. Pure helpers, no I/O. */
const ALLOWED = new Set(['#lib/utils/cn.ts', '#lib/utils/dom.ts']);

let app: TestApp;
before(async () => {
  app = await bootApp();
});

test('every marketing page renders signed out, in the marketing shell, without touching the fleet', async () => {
  for (const path of PAGES) {
    const before = app.fleet.calls.length;
    const res = await app.handle(new Request(`http://localhost${path}`));
    assert.equal(res.status, 200, `${path} renders for a visitor with no session`);
    const body = await res.text();
    assert.match(body, /\/public\/site\.css/, `${path} loads the marketing stylesheet`);
    assert.doesNotMatch(body, /\/public\/tailwind\.css/, `${path} does not load the product's`);
    assert.equal(app.fleet.calls.length, before, `${path} made a fleet call: ${JSON.stringify(app.fleet.calls.slice(before))}`);
  }
});

test('the product loads its own stylesheet and not the marketing one', async () => {
  const res = await app.handle(new Request('http://localhost/login'));
  assert.equal(res.status, 200);
  const body = await res.text();
  assert.match(body, /\/public\/tailwind\.css/);
  assert.doesNotMatch(body, /\/public\/site\.css/);
});

test('an address nothing matches gets the marketing 404 inside its shell', async () => {
  const res = await app.handle(new Request('http://localhost/no-such-page'));
  assert.equal(res.status, 404);
  const body = await res.text();
  assert.match(body, /this address does not resolve/);
  assert.match(body, /\/public\/site\.css/, 'styled, not a bare fragment: there is no root layout to wrap it');
});

test('the marketing 404 is a routed page, so its components are shipped', async () => {
  // The root not-found renders with no route and therefore no module scripts:
  // the theme toggle is drawn and does nothing. The catch-all makes it routed.
  const body = await (await app.handle(new Request('http://localhost/no/such/page'))).text();
  assert.match(body, /this address does not resolve/);
  assert.match(body, /site\/components\/theme-toggle\.ts/, 'the layout\'s island is loaded on the 404 too');
});

test('an address nothing matches under /dashboard gets the product 404, not the marketing one', async () => {
  const res = await app.handle(new Request('http://localhost/dashboard/no-such-page'));
  assert.equal(res.status, 404);
  const body = await res.text();
  assert.match(body, /Back to your apps/);
  assert.match(body, /\/public\/tailwind\.css/);
  assert.doesNotMatch(body, /\/public\/site\.css/);
});

test('robots.txt keeps crawlers out of the product in every group', async () => {
  const body = await (await app.handle(new Request('http://localhost/robots.txt'))).text();
  const groups = body.split(/\n\n/).filter((g) => g.startsWith('User-agent:'));
  assert.ok(groups.length > 5, 'the wildcard and the named agents');
  for (const group of groups) {
    for (const path of ['/dashboard', '/login', '/oauth', '/api']) {
      assert.match(group, new RegExp(`^Disallow: ${path}$`, 'm'), `${group.split('\n')[0]} is kept out of ${path}`);
    }
    assert.match(group, /^Allow: \/$/m);
  }
});

test('the links between the two shells are full page loads', async () => {
  const home = await (await app.handle(new Request('http://localhost/'))).text();
  const tag = home.match(/<a\b[^>]*href="\/dashboard"[^>]*>/);
  assert.ok(tag, 'the marketing header links the product');
  // A soft navigation keeps the outgoing shell's stylesheet.
  assert.match(tag[0], /\bdata-no-router\b/);
});

function sources(dir: string, out: string[] = []): string[] {
  for (const name of readdirSync(dir)) {
    const full = join(dir, name);
    if (statSync(full).isDirectory()) sources(full, out);
    else if (/\.(ts|js)$/.test(name)) out.push(full);
  }
  return out;
}

function productImports(text: string): string[] {
  const found: string[] = [];
  for (const m of text.matchAll(/(?:from|import)\s*\(?\s*['"](#[^'"]+)['"]/g)) {
    const spec = m[1];
    if (spec.startsWith('#site/') || ALLOWED.has(spec)) continue;
    found.push(spec);
  }
  return found;
}

test('no marketing module imports the product', () => {
  const files = [...sources(join(ROOT, 'app', '(site)')), ...sources(join(ROOT, 'site')), join(ROOT, 'app', 'not-found.ts')];
  assert.ok(files.length > 20, 'the marketing half was found');
  const offenders: string[] = [];
  for (const file of files) {
    for (const spec of productImports(readFileSync(file, 'utf8'))) offenders.push(`${relative(ROOT, file)} imports ${spec}`);
  }
  assert.deepEqual(offenders, [], 'a marketing file reaches into the product (session, database or fleet client)');
});

test('the import check fires on a session import and not on the allowed helpers', () => {
  assert.deepEqual(productImports("import { currentUser } from '#modules/auth/queries/current-user.server.ts';"), ['#modules/auth/queries/current-user.server.ts']);
  assert.deepEqual(productImports("const { db } = await import('#db/connection.server.ts');"), ['#db/connection.server.ts']);
  assert.deepEqual(productImports("import { cn } from '#lib/utils/cn.ts';\nimport { NAV } from '#site/lib/links.ts';"), []);
});
