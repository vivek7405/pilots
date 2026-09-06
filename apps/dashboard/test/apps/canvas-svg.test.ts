/**
 * The two SVG drawings of a layout, as served bytes.
 *
 * What matters is geometry and naming: an arrow runs from the dependent's top
 * centre to the dependency's bottom centre so it points up; both drawings use
 * the layout's own viewBox so the thumbnail is the canvas at a smaller size;
 * the thumbnail names what it shows and the edge layer stays out of the
 * accessibility tree.
 *
 * Counterfactual: swap the line's endpoints and the first test fails on the
 * coordinates while every count still matches; drop the `aria-label` and the
 * thumbnail test fails on the name.
 */

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { renderToString } from '@webjsdev/core/server';
import { CARD, layoutApp } from '#modules/apps/utils/layout.ts';
import { edgesSvg, thumbnailSvg } from '#modules/apps/utils/ui/canvas-svg.ts';

const chain = layoutApp([
  { id: 'svc-web', name: 'web', dependsOn: ['api'] },
  { id: 'svc-api', name: 'api', dependsOn: ['db'] },
  { id: 'svc-db', name: 'db', dependsOn: [] },
]);

test('an edge is a dashed line from the dependent up to what it dials, with an arrowhead', async () => {
  const out = await renderToString(edgesSvg(chain));
  const lines = out.match(/<line[\s\S]*?<\/line>/g) ?? [];
  assert.equal(lines.length, 2, 'one line per edge');

  // web (layer 2) -> api (layer 1): from web's top centre to api's bottom centre.
  const web = chain.placed.find((p) => p.id === 'svc-web')!;
  const api = chain.placed.find((p) => p.id === 'svc-api')!;
  const webToApi = lines.find((l) => l.includes(`y1="${web.y}"`))!;
  assert.match(webToApi, new RegExp(`x1="${web.x + CARD.w / 2}"`));
  assert.match(webToApi, new RegExp(`x2="${api.x + CARD.w / 2}"`));
  assert.match(webToApi, new RegExp(`y2="${api.y + CARD.h}"`));
  assert.match(webToApi, /stroke-dasharray="/);
  assert.match(webToApi, /marker-end="url\(#canvas-arrow\)"/);
  assert.match(out, /<marker id="canvas-arrow"/);
});

test('the edge layer shares the layout viewBox, fills its stage and is hidden from assistive tech', async () => {
  const out = await renderToString(edgesSvg(chain));
  assert.match(out, new RegExp(`viewBox="0 0 ${chain.width} ${chain.height}"`));
  assert.match(out, /class="absolute inset-0 size-full stroke-border-strong"/);
  assert.match(out, /aria-hidden="true"/);
});

test('the thumbnail draws one card per service in the same viewBox and says what it shows', async () => {
  const out = await renderToString(thumbnailSvg(chain));
  assert.equal((out.match(/<rect /g) ?? []).length, 3);
  assert.match(out, new RegExp(`viewBox="0 0 ${chain.width} ${chain.height}"`));
  assert.match(out, /width="160"/);
  assert.match(out, /height="96"/);
  assert.match(out, /role="img"/);
  assert.match(out, /aria-label="3 services, 2 connections"/);
  assert.match(out, /class="fill-card stroke-border"/);
  // Each rectangle is where the canvas puts that card.
  const db = chain.placed.find((p) => p.id === 'svc-db')!;
  assert.match(out, new RegExp(`<rect x="${db.x}" y="${db.y}" width="${CARD.w}" height="${CARD.h}"`));
});

test('the thumbnail name reads correctly for one service and no connections', async () => {
  const one = layoutApp([{ id: 'svc-solo', name: 'solo', dependsOn: [] }]);
  const out = await renderToString(thumbnailSvg(one));
  assert.match(out, /aria-label="1 service, 0 connections"/);
  assert.ok(!out.includes('<line'), 'nothing to connect, so no line');
});

test('an edge whose endpoint is not placed draws nothing rather than a line to nowhere', async () => {
  const broken = { ...chain, edges: [...chain.edges, { from: 'svc-web', to: 'svc-ghost' }] };
  const out = await renderToString(edgesSvg(broken));
  assert.equal((out.match(/<line/g) ?? []).length, 2);
});
