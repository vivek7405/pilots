/**
 * Limits and capacity live on Usage now, not on the apps page. They read the
 * same fleet the apps list does and each degrades on its own.
 */
import assert from 'node:assert/strict';
import { before, test } from 'node:test';
import { asUser, bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';

let app: TestApp;
let cookie: string;
before(async () => {
  app = await bootApp();
  cookie = await signInAs(app.handle, { id: 7401, login: 'usage-pilot' });
});

test('the usage page carries the four limit bars and the capacity strip', async () => {
  const res = await app.handle(new Request('http://localhost/usage', asUser(cookie)));
  assert.equal(res.status, 200);
  const body = await res.text();
  assert.ok(body.includes('>Limits<'), 'a Limits section');
  assert.ok(body.includes('>Capacity<'), 'a Capacity section');
  const bars = [...body.matchAll(/<progress[^>]*>/g)];
  assert.equal(bars.length, 4, `expected four quota bars, found ${bars.length}`);
  assert.match(body, /<hosts-strip/, 'capacity is the fleet strip');
});
