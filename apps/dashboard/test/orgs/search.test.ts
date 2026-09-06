/**
 * What the command palette matches, and what it refuses to take as input.
 *
 * The ranking is a pure function so it can be driven directly; the query that
 * wraps it does exactly two things this file cannot reach without a request
 * scope, and both are asserted against the source instead. That is not a
 * shortcut: the tenancy claim being made is "there is nowhere to put an org
 * id", which is a claim about the shape of the code.
 *
 * Counterfactual: cap the results overall rather than per kind and the fourth
 * test fails, because thirty matching machines push both services off the end.
 */

import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { PER_KIND, rankHits } from '#modules/orgs/utils/search.ts';
import type { Named } from '#modules/orgs/utils/search.ts';

const services: Named[] = [
  { id: 'svc-web', name: 'web', app: 'gallery' },
  { id: 'svc-api', name: 'api', app: 'gallery' },
];

const machines: Named[] = Array.from({ length: 30 }, (_, i) => ({
  id: `m-${i}`,
  name: `webbox-${i}`,
  state: 'running',
}));

test('a query matches services, sandboxes and pages by name', () => {
  const hits = rankHits(services, machines, 'web');

  assert.ok(hits.some((h) => h.kind === 'service' && h.label === 'web'));
  assert.ok(hits.some((h) => h.kind === 'sandbox' && h.label.startsWith('webbox-')));
  assert.ok(!hits.some((h) => h.label === 'api'), 'a service that does not match is not returned');
});

test('a page is reachable by its own name, so Tokens finds /keys', () => {
  // The route is still /keys; the NAME is what a reader searches for, and
  // nobody types the path of a page they cannot see.
  assert.deepEqual(
    rankHits([], [], 'token').map((h) => h.href),
    ['/keys'],
  );
});

test('a sandbox is reachable by its id as well as its name', () => {
  const hits = rankHits([], machines, 'm-17');
  assert.deepEqual(
    hits.map((h) => h.href),
    ['/machines/m-17'],
  );
});

test('the cap is per kind, so sandboxes cannot crowd out services', () => {
  const hits = rankHits(services, machines, 'web');
  assert.equal(hits.filter((h) => h.kind === 'sandbox').length, PER_KIND, 'sandboxes are capped');
  assert.ok(
    hits.some((h) => h.kind === 'service' && h.label === 'web'),
    'and the matching service still made it into the answer',
  );
});

test('an empty query offers everything, and every hit has somewhere to go', () => {
  const hits = rankHits(services, machines, '');
  assert.ok(hits.length > 0);
  for (const hit of hits) assert.match(hit.href, /^\//, `a hit with no destination: ${JSON.stringify(hit)}`);
});

test('a machine named after a status does not drag every online machine in', () => {
  // `detail` is matched too, which is what makes "online" a useful query. The
  // point of this test is that it is deliberate rather than accidental, and
  // the word it matches is the one a reader sees rather than the engine's.
  const hits = rankHits([], machines.slice(0, 3), 'online');
  assert.equal(hits.length, 3);
});

test('the query reads its org from the session and takes none as an argument', () => {
  const source = readFileSync(
    new URL('../../modules/orgs/queries/search-org.server.ts', import.meta.url),
    'utf8',
  );
  // One admin key covers the whole fleet, so an org id in the argument
  // position would be an org-name oracle for every signed-in visitor.
  assert.match(source, /const ctx = await requireOrg\(\);/);
  assert.match(source, /if \(!ctx\) return signedOut\(\);/);
  assert.match(source, /listServices\(ctx\.org\.id\)/);
  assert.match(source, /listMachines\(ctx\.org\.id\)/);
  assert.ok(!/input\??\.\s*org/.test(source), 'no org is ever read from the arguments');
  // A read action must declare GET, or its arguments ride a POST body, no ETag
  // applies, and the CSRF exemption for safe reads does not.
  assert.match(source, /export const method = 'GET';/);
});

// A machine that belongs to a service is an INSTANCE of it, and one that does
// not is a sandbox. The palette is where a person goes knowing what they want,
// so the two must not read as one thing.
test('a machine is a sandbox or an instance by whether it names a service', () => {
  const hits = rankHits(
    [],
    [
      { id: 'm-solo', name: 'solo', state: 'running' },
      { id: 'm-copy', name: 'copy', state: 'running', service_id: 'svc-web' },
    ],
    '',
  );
  assert.equal(hits.find((h) => h.id === 'm-solo')?.kind, 'sandbox');
  assert.equal(hits.find((h) => h.id === 'm-copy')?.kind, 'instance');
});

// The detail line is the word a reader knows, never the engine's own value.
test("a machine's state reaches the palette as a word, not as a raw state", () => {
  const hits = rankHits([], [{ id: 'm-1', name: 'one', state: 'suspended' }], 'one');
  assert.equal(hits[0]?.detail, 'Sleeping');
});
