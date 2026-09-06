/**
 * The playground: one click from nothing to a shell in a fresh sandbox, and
 * the two actions around it. The sandbox is created AS the visitor's org and
 * a foreign one is a 404, never a 403.
 */
import assert from 'node:assert/strict';
import { before, test } from 'node:test';
import { submitForm } from '@webjsdev/server/testing';
import { asUser, bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';
import type { Machine } from '@pilots/sdk';

let app: TestApp;
let cookie: string;
let org = '';

before(async () => {
  app = await bootApp();
  cookie = await signInAs(app.handle, { id: 7900, login: 'player' });
  const { db } = await import('#db/connection.server.ts');
  org = (await db.query.orgs.findMany()).find((o) => o.slug === 'player')!.id;
  app.fleet.data.machines.push(
    ...([
      { id: 'm-play', name: 'calm-box-1a2b', state: 'running', org_id: org, url: 'http://calm-box-1a2b.pilots.localhost:8080' },
      { id: 'm-play2', name: 'warm-box-9z8y', state: 'running', org_id: org },
      { id: 'm-theirs', name: 'other', state: 'running', org_id: 'someone-else' },
    ] as unknown as Machine[]),
  );
});

async function page(query = ''): Promise<Response> {
  return app.handle(new Request(`http://localhost/sandboxes/playground${query}`, asUser(cookie)));
}

test('with nothing selected it is one button and the CLI equivalent', async () => {
  const res = await page();
  assert.equal(res.status, 200);
  const body = await res.text();
  assert.match(body, /Open a sandbox/);
  assert.match(body, /pilot console/);
  assert.ok(!body.includes('<machine-terminal'), 'no terminal until there is a sandbox');
});

test('a selected sandbox is a bar over a terminal, with its URL, a new one and a remove', async () => {
  const res = await page('?m=m-play');
  assert.equal(res.status, 200);
  const body = await res.text();
  assert.match(body, /calm-box-1a2b/);
  assert.match(body, /href="http:\/\/calm-box-1a2b\.pilots\.localhost:8080"/, 'the URL is a link');
  assert.match(body, /<machine-terminal[^>]*machine-id="m-play"/);
  assert.match(body, /New sandbox/);
  assert.match(body, /Remove calm-box-1a2b\?/, 'removing confirms');
  assert.match(body, /<noscript>/, 'scripting off is told what to do instead');
});

test("another org's sandbox is a 404, the same as one that never existed", async () => {
  assert.equal((await page('?m=m-theirs')).status, 404);
  assert.equal((await page('?m=m-nope')).status, 404);
});

test('opening a sandbox creates one as the org and lands on it', async () => {
  app.fleet.calls.length = 0;
  const res = await submitForm(app.handle, '/sandboxes/playground', {}, { cookies: cookie, match: 'Open a sandbox' });
  assert.equal(res.status, 303);
  assert.match(res.headers.get('location') ?? '', /^\/sandboxes\/playground\?m=.+&ok=created$/);
  assert.ok(app.fleet.calls.some((c) => c.method === 'as' && c.args[0] === org), 'created as the visitor org');
  assert.ok(app.fleet.calls.some((c) => c.method === 'machines.create'), 'a machine was created');
});

test('removing a sandbox destroys it and returns to the empty playground', async () => {
  app.fleet.calls.length = 0;
  const res = await submitForm(
    app.handle,
    '/sandboxes/playground?m=m-play',
    { machine: 'm-play' },
    { cookies: cookie, match: 'Remove' },
  );
  assert.equal(res.status, 303);
  assert.equal(res.headers.get('location'), '/sandboxes/playground?ok=destroyed');
  assert.deepEqual(app.fleet.calls.find((c) => c.method === 'machines.destroy')!.args, ['m-play']);
});

test("removing another org's sandbox is refused as not found", async () => {
  app.fleet.calls.length = 0;
  // Rendered from a sandbox that still exists; the one above was removed.
  const res = await submitForm(
    app.handle,
    '/sandboxes/playground?m=m-play2',
    { machine: 'm-theirs' },
    { cookies: cookie, match: 'Remove' },
  );
  assert.equal(res.status, 404);
  assert.ok(!app.fleet.calls.some((c) => c.method === 'machines.destroy'), 'nothing was destroyed');
});
