/**
 * Rate limiting on the key-minting route.
 *
 * The property that actually matters is the second one. Behind the workload
 * router the socket peer is the router on EVERY request, so a limiter reading
 * the peer buckets every visitor together and the limit stops meaning
 * anything, while `X-RateLimit-Remaining` still counts down convincingly.
 * `trustProxy: true` is what makes each caller its own bucket.
 *
 * Its precondition is that the router strips an inbound `X-Forwarded-For`
 * before appending the peer. `apps/hostd/internal/router/router.go` does not
 * do that yet; README.md records it as a deploy prerequisite.
 *
 * Counterfactual: drop `trustProxy: true` from `app/api/keys/route.ts` and the
 * two runs below share one bucket, so the second assertion fails.
 */

import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';

let app: TestApp;
let cookie = '';

function mint(name: string, ip: string): Request {
  return new Request('http://localhost/api/keys', {
    method: 'POST',
    headers: { cookie, 'x-forwarded-for': ip, 'content-type': 'application/json' },
    body: JSON.stringify({ name, scopes: ['deploy'] }),
  });
}

before(async () => {
  app = await bootApp();
  cookie = await signInAs(app.handle, { id: 5150, login: 'limited' });
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

test('eleven mints from one forwarded IP inside a minute yield a 429', async () => {
  const ip = '192.0.2.77';
  const statuses: number[] = [];
  for (let i = 0; i < 11; i += 1) statuses.push((await app.handle(mint(`k${i}`, ip))).status);

  assert.equal(statuses.filter((s) => s === 200).length, 10, 'ten get through');
  assert.equal(statuses.at(-1), 429, 'the eleventh is refused');

  const refused = await app.handle(mint('again', ip));
  assert.equal(refused.status, 429);
  assert.ok(refused.headers.get('retry-after'), 'and it says when to come back');
});

test('eleven mints from eleven different forwarded IPs are not limited', async () => {
  const statuses: number[] = [];
  for (let i = 0; i < 11; i += 1) statuses.push((await app.handle(mint(`d${i}`, `198.51.100.${i}`))).status);

  assert.equal(
    statuses.some((s) => s === 429),
    false,
    'without trustProxy these would all share the router\'s own address as one bucket',
  );
});

test('the limit is per route group: a read is not spent by a refused mint', async () => {
  const ip = '192.0.2.88';
  for (let i = 0; i < 11; i += 1) await app.handle(mint(`x${i}`, ip));

  const read = await app.handle(
    new Request('http://localhost/api/keys', { headers: { cookie, 'x-forwarded-for': ip } }),
  );
  assert.equal(read.status, 200, 'the 120/min API budget is separate from the 10/min minting one');
});
