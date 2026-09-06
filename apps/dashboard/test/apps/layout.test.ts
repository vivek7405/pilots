/**
 * The canvas layout, as pure arithmetic.
 *
 * The rules are the feature: what a service dials sits above it, siblings
 * read left to right by name, a dependent sits under its dependencies, and
 * the same input always draws the same picture. Each rule is asserted on its
 * own so a regression names the rule it broke.
 *
 * Counterfactuals, each run once against a real revert: layer every node at
 * 0 and the first test fails; drop the barycenter and the fourth fails while
 * the name-order test still passes; stop breaking cycles and the cycle test
 * hangs the process rather than failing.
 */

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { CARD, layoutApp } from '#modules/apps/utils/layout.ts';
import type { LayoutNode } from '#modules/apps/utils/layout.ts';

const node = (name: string, ...dependsOn: string[]): LayoutNode => ({ id: `svc-${name}`, name, dependsOn });

const layerOf = (out: ReturnType<typeof layoutApp>, id: string) => out.placed.find((p) => p.id === id)?.layer;
const xOf = (out: ReturnType<typeof layoutApp>, id: string) => out.placed.find((p) => p.id === id)?.x;

test('web -> api -> db lands on layers 2, 1, 0, with what is dialled above the dialler', () => {
  const out = layoutApp([node('web', 'api'), node('api', 'db'), node('db')]);
  assert.equal(layerOf(out, 'svc-web'), 2);
  assert.equal(layerOf(out, 'svc-api'), 1);
  assert.equal(layerOf(out, 'svc-db'), 0);
  // Layer 0 is the top row, so the arrow from web points up through api to db.
  assert.equal(out.placed.find((p) => p.id === 'svc-db')!.y, 0);
  assert.equal(out.placed.find((p) => p.id === 'svc-web')!.y, 2 * CARD.dy);
  assert.deepEqual(out.edges, [
    { from: 'svc-api', to: 'svc-db' },
    { from: 'svc-web', to: 'svc-api' },
  ]);
});

test('a service that dials two things sits below the deeper one, not the shallower', () => {
  // web dials both db (layer 0) and api (layer 1), so it is layer 2, not 1.
  const out = layoutApp([node('web', 'db', 'api'), node('api', 'db'), node('db')]);
  assert.equal(layerOf(out, 'svc-web'), 2);
});

test('siblings on one layer read left to right by name', () => {
  const out = layoutApp([node('zeta'), node('alpha'), node('mid')]);
  assert.deepEqual(
    out.placed.map((p) => [p.name, p.x]),
    [
      ['alpha', 0],
      ['mid', CARD.dx],
      ['zeta', 2 * CARD.dx],
    ],
  );
  assert.equal(out.width, 2 * CARD.dx + CARD.w);
  assert.equal(out.height, CARD.h);
});

test('a dependent lands under what it dials, before its name is considered', () => {
  // Layer 0 by name: a (x=0), b, c (x=2dx). On layer 1, y dials c and z dials
  // a. By name y would come first; by barycenter z sits left, under a.
  const out = layoutApp([node('a'), node('b'), node('c'), node('y', 'c'), node('z', 'a')]);
  assert.equal(xOf(out, 'svc-z'), 0, 'z sits under a');
  assert.equal(xOf(out, 'svc-y'), CARD.dx, 'y comes after z, though y sorts first by name');
});

test('an equal barycenter falls back to name order', () => {
  const out = layoutApp([node('db'), node('worker', 'db'), node('api', 'db')]);
  assert.equal(xOf(out, 'svc-api'), 0);
  assert.equal(xOf(out, 'svc-worker'), CARD.dx);
});

test('a cycle terminates, and the edge whose source sorts later is the one dropped', () => {
  const out = layoutApp([node('web', 'api'), node('api', 'web')]);
  // "web" sorts after "api", so web's edge is the one dropped and api -> web
  // survives: web ends up above, api below it.
  assert.deepEqual(out.edges, [{ from: 'svc-api', to: 'svc-web' }]);
  assert.equal(layerOf(out, 'svc-web'), 0);
  assert.equal(layerOf(out, 'svc-api'), 1);
});

test('a longer cycle loses exactly one edge and every other edge survives', () => {
  // a -> b -> c -> a. The source that sorts last is c, so c -> a goes.
  const out = layoutApp([node('a', 'b'), node('b', 'c'), node('c', 'a')]);
  assert.deepEqual(out.edges, [
    { from: 'svc-a', to: 'svc-b' },
    { from: 'svc-b', to: 'svc-c' },
  ]);
  assert.equal(layerOf(out, 'svc-c'), 0);
  assert.equal(layerOf(out, 'svc-a'), 2);
});

test('an unknown target and a self-reference are not edges', () => {
  const out = layoutApp([node('web', 'redis', 'web'), node('db')]);
  assert.deepEqual(out.edges, []);
  assert.equal(layerOf(out, 'svc-web'), 0);
  assert.equal(layerOf(out, 'svc-db'), 0);
});

test('a repeated dependency is one edge', () => {
  const out = layoutApp([node('web', 'db', 'db'), node('db')]);
  assert.deepEqual(out.edges, [{ from: 'svc-web', to: 'svc-db' }]);
});

test('the output does not depend on the order the services arrive in', () => {
  const nodes = [node('web', 'api', 'db'), node('api', 'db'), node('db'), node('worker', 'db'), node('cron', 'api')];
  const first = layoutApp(nodes);
  const second = layoutApp([...nodes].reverse());
  const third = layoutApp([nodes[2]!, nodes[4]!, nodes[0]!, nodes[3]!, nodes[1]!]);
  assert.deepEqual(second, first);
  assert.deepEqual(third, first);
});

test('placement is in reading order: top row first, left to right', () => {
  const out = layoutApp([node('web', 'api'), node('api', 'db'), node('db'), node('cache')]);
  assert.deepEqual(
    out.placed.map((p) => p.name),
    ['cache', 'db', 'api', 'web'],
  );
});

test('no services is an empty picture, not a crash', () => {
  assert.deepEqual(layoutApp([]), { placed: [], edges: [], width: 0, height: 0 });
});
