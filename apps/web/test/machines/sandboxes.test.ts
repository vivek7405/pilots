/**
 * The sandboxes list, and the one button on it.
 *
 * The button CREATES. It used to be a link to a separate playground page
 * whose own button, wearing the same words, did the creating -- so the label
 * named an action the control did not perform and a sandbox cost two clicks
 * that looked like one. That page is gone; this is what replaced it, and
 * these assertions came with it rather than being retired.
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
      { id: 'm-theirs', name: 'other', state: 'running', org_id: 'someone-else' },
    ] as unknown as Machine[]),
  );
});

test('the list offers one button, and it says what it does', async () => {
  const res = await app.handle(new Request('http://localhost/sandboxes', asUser(cookie)));
  assert.equal(res.status, 200);
  const body = await res.text();
  assert.match(body, /Create a sandbox/);
  // It SUBMITS rather than navigating, which is the whole point of the change
  // and the reason the label is now honest. Asserted as two independent facts
  // rather than one regex spanning both: the button's class attribute alone
  // runs to several hundred characters, so any rule about the distance
  // between the tags is a rule about Tailwind, not about the markup.
  assert.match(body, />Create a sandbox<\/button>/, 'it is a button');
  assert.ok(!/<a[^>]*>\s*Create a sandbox/.test(body), 'not a link dressed as one');
  assert.match(body, /<form[^>]+action=/, 'the button sits in a posting form');
  assert.ok(!body.includes('/sandboxes/playground'), 'the playground is gone, not merely unlinked');
});

test('the button creates as the visitor org and lands on the sandbox', async () => {
  app.fleet.calls.length = 0;
  const res = await submitForm(app.handle, '/sandboxes', {}, { cookies: cookie, match: 'Create a sandbox' });
  assert.equal(res.status, 303);
  // The sandbox's own page, where the terminal is -- not back to the list.
  assert.match(res.headers.get('location') ?? '', /^\/machines\/m-[a-z0-9-]+\?ok=created$/);
  assert.ok(app.fleet.calls.some((c) => c.method === 'as' && c.args[0] === org), 'created as the visitor org');
  assert.ok(app.fleet.calls.some((c) => c.method === 'machines.create'), 'a machine was created');
});

test('a signed-out visitor is not offered one', async () => {
  const res = await app.handle(new Request('http://localhost/sandboxes'));
  assert.ok(res.status === 302 || res.status === 401, `got ${res.status}`);
});

test('the retired playground page is a 404, not a redirect', async () => {
  // A stale bookmark should say the page is gone rather than quietly land
  // somewhere else, which is how a removed surface goes on looking alive.
  const res = await app.handle(new Request('http://localhost/sandboxes/playground', asUser(cookie)));
  assert.equal(res.status, 404);
});
