/**
 * The app canvas at `/apps/<app>` and the panel it opens.
 *
 * What is asserted is the contract the journey rests on: the picture is in
 * the served bytes (cards and arrows, drawn by the server); `?service=` and
 * `?tab=` carry the selection so a reload restores it; a service of another
 * app is not a panel on this canvas; and every form in the panel returns to
 * the canvas rather than throwing the reader off it.
 *
 * Counterfactuals: drop the `back` hidden field and the redirect test lands
 * on `/services/svc-web`; filter `?service=` against nothing and the
 * foreign-service test renders a panel it must not; layer every card at 0
 * and the position assertions fail.
 */

import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { submitForm } from '@webjsdev/server/testing';
import { asUser, bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';
import type { Machine, Release, Service, Volume } from '@pilots/sdk';

let app: TestApp;
let cookie = '';
let org = '';

const NOW_SEC = Math.floor(Date.now() / 1000);

before(async () => {
  app = await bootApp();
  cookie = await signInAs(app.handle, { id: 7400, login: 'canvasser' });
  const { db } = await import('#db/connection.server.ts');
  org = (await db.query.orgs.findMany()).find((o) => o.slug === 'canvasser')!.id;

  app.fleet.data.services.push(
    ...([
      { id: 'svc-web', name: 'web', org_id: org, app: 'gallery', replicas: 1, url: 'https://web.example', release_id: 'rel-w', depends_on: ['api'], knobs: {}, autodeploy: false, created_at: 1 },
      { id: 'svc-api', name: 'api', org_id: org, app: 'gallery', replicas: 1, url: 'https://api.example', release_id: 'rel-a', volume_id: 'vol-1', knobs: {}, autodeploy: false, created_at: 2 },
      { id: 'svc-other', name: 'other', org_id: org, app: 'shop', replicas: 1, knobs: {}, autodeploy: false, created_at: 3 },
      // Their own app, so the gallery canvas keeps the layout its own
      // assertions pin. No url of its own: created private, or from before
      // addresses were minted. The instance still has one, and the card has to
      // say whose it is showing.
      { id: 'svc-quiet', name: 'quiet', org_id: org, app: 'quiet-app', replicas: 1, release_id: 'rel-q', knobs: {}, autodeploy: false, created_at: 4 },
      // Nothing at all: no address and no instance carrying one.
      { id: 'svc-blank', name: 'blank', org_id: org, app: 'quiet-app', replicas: 0, knobs: {}, autodeploy: false, created_at: 5 },
    ] as unknown as Service[]),
  );
  app.fleet.data.releases['svc-web'] = [
    { id: 'rel-w', service_id: 'svc-web', healthy: true, created_at: NOW_SEC - 3600, rootfs_build_id: 'bld-w' },
    { id: 'rel-w0', service_id: 'svc-web', healthy: true, created_at: NOW_SEC - 86_400, rootfs_build_id: 'bld-w0' },
  ] as Release[];
  app.fleet.data.releases['svc-api'] = [{ id: 'rel-a', service_id: 'svc-api', healthy: true, created_at: NOW_SEC - 7200 }] as Release[];
  app.fleet.data.machines.push(
    ...([
      { id: 'm-web', name: 'web-1', state: 'running', org_id: org, host_id: 'h-1', service_id: 'svc-web', release_id: 'rel-w' },
      { id: 'm-api', name: 'api-1', state: 'running', org_id: org, host_id: 'h-1', service_id: 'svc-api', release_id: 'rel-a', volume_id: 'vol-1' },
      { id: 'm-quiet', name: 'quiet-1', state: 'running', org_id: org, host_id: 'h-1', service_id: 'svc-quiet', release_id: 'rel-q', url: 'https://quiet-1.example' },
    ] as unknown as Machine[]),
  );
  app.fleet.data.volumes.push({ id: 'vol-1', name: 'app-data', org_id: org, size_gib: 10, mount_path: '/data', machine_id: 'm-api' } as unknown as Volume);
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

async function page(path: string): Promise<{ status: number; body: string }> {
  const res = await app.handle(new Request(`http://localhost${path}`, asUser(cookie)));
  return { status: res.status, body: await res.text() };
}

// A service with no address of its own falls back to its instance's, which the
// next deploy replaces. Showing that address unlabelled is what made a moving
// URL look like the app's own, so the label is the fix and the assertion.
test('a service with no address shows its instance address, labelled as the instance', async () => {
  const { body } = await page('/apps/quiet-app');
  assert.match(body, /instance quiet-1\.example/, 'the instance address is labelled');
  assert.match(body, /It changes on the next deploy/, 'and says what that costs');
  // A service with its own address is never labelled: it does not move.
  const gallery = await page('/apps/gallery');
  assert.doesNotMatch(gallery.body, /instance web\.example/);
});

test('a service with no address and no instance says so plainly', async () => {
  const { body } = await page('/apps/quiet-app');
  assert.match(body, /No URL yet/);
});

test('the canvas is drawn by the server: one card per service, one arrow per edge, storage on the mounting card', async () => {
  const { status, body } = await page('/apps/gallery');
  assert.equal(status, 200);
  assert.equal((body.match(/data-canvas-card/g) ?? []).length, 2, 'two cards');
  assert.equal((body.match(/<line/g) ?? []).length, 1, 'one arrow, from web up to api');
  assert.match(body, /<app-canvas/);
  assert.match(body, /data-canvas-stage[^>]*data-width="240"[^>]*data-height="336"/, 'two rows, one column');

  // api dials nothing, so it is the top row; web sits under it.
  assert.match(body, /href="\/apps\/gallery\?service=svc-api"[\s\S]*?style="left:0px;top:0px"/);
  assert.match(body, /href="\/apps\/gallery\?service=svc-web"[\s\S]*?style="left:0px;top:216px"/);

  const apiCard = body.slice(body.indexOf('service=svc-api'), body.indexOf('</a>', body.indexOf('service=svc-api')));
  assert.match(apiCard, /app-data/, 'the volume hangs off the service that mounts it');
  assert.match(apiCard, /10 GB/);
  assert.match(apiCard, /Online/);
  assert.ok(!body.includes('<slide-over'), 'no selection, no panel');
});

test('?service= opens the panel in a slide-over and marks the card', async () => {
  const { body } = await page('/apps/gallery?service=svc-web');
  assert.match(body, /<slide-over[^>]*back="\/apps\/gallery"/);
  const webCard = body.slice(body.indexOf('service=svc-web"'), body.indexOf('</a>', body.indexOf('service=svc-web"')));
  assert.match(webCard, /aria-current="true"/);
  assert.match(webCard, /ring-primary/);
  const apiCard = body.slice(body.indexOf('service=svc-api"'), body.indexOf('</a>', body.indexOf('service=svc-api"')));
  assert.match(apiCard, /aria-current="false"/);

  // The Deployments tab is the default and shows what is live.
  assert.match(body, /aria-label="Service sections"/);
  assert.match(body, /href="\/apps\/gallery\?service=svc-web&amp;tab=deployments"[^>]*aria-current="page"/);
  assert.match(body, /data-current-deployment/);
  assert.match(body, /rel-w/);
  assert.match(body, /Roll back to this/, 'the older healthy deployment is a roll-back target');
  assert.match(body, /aria-label="Close"[^>]*|href="\/apps\/gallery"[^>]*aria-label="Close"/, 'close is a link');
});

test('&tab=settings renders the Instances form, and its forms return to the canvas', async () => {
  const { body } = await page('/apps/gallery?service=svc-web&tab=settings');
  assert.match(body, /href="\/apps\/gallery\?service=svc-web&amp;tab=settings"[^>]*aria-current="page"/);
  assert.match(body, /name="replicas"/);
  // The Instances form itself, not any form on the page, carries the return.
  const instancesForm = body.slice(body.lastIndexOf('<form', body.indexOf('name="replicas"')), body.indexOf('</form>', body.indexOf('name="replicas"')));
  assert.match(instancesForm, /name="back" value="\/apps\/gallery\?service=svc-web&amp;tab=settings"/);
  assert.ok(!body.includes('data-current-deployment'), 'only the selected tab renders');

  app.fleet.calls.length = 0;
  const res = await submitForm(
    app.handle,
    '/apps/gallery?service=svc-web&tab=settings',
    { service: 'svc-web', replicas: '2', back: '/apps/gallery?service=svc-web&tab=settings' },
    { cookies: cookie, match: 'Save' },
  );
  assert.equal(res.status, 303);
  assert.equal(res.headers.get('location'), '/apps/gallery?service=svc-web&tab=settings&ok=saved');
  assert.deepEqual(app.fleet.calls.find((c) => c.method === 'services.patch')!.args, ['svc-web', { replicas: 2 }]);
});

test('the Terminal tab is a shell on a running instance, with a full-screen link', async () => {
  const { body } = await page('/apps/gallery?service=svc-web&tab=terminal');
  assert.match(body, /<machine-terminal[^>]*machine-id="m-web"/);
  assert.match(body, /href="\/machines\/m-web\/terminal"/);
  assert.match(body, /pilot console web-1/, 'with scripting off the CLI is named');
});

test('a service with nothing running gets an empty Terminal tab pointing at Deployments', async () => {
  app.fleet.data.machines.find((m) => m.id === 'm-web')!.state = 'suspended';
  try {
    const { body } = await page('/apps/gallery?service=svc-web&tab=terminal');
    assert.ok(!body.includes('<machine-terminal'));
    assert.match(body, /No instance is running right now/);
    assert.match(body, /href="\/apps\/gallery\?service=svc-web&amp;tab=deployments"/);
  } finally {
    app.fleet.data.machines.find((m) => m.id === 'm-web')!.state = 'running';
  }
});

test('a service of another app is not a panel on this canvas', async () => {
  const { status, body } = await page('/apps/gallery?service=svc-other');
  assert.equal(status, 200);
  assert.ok(!body.includes('<slide-over'));
  assert.equal((body.match(/data-canvas-card/g) ?? []).length, 2, 'the canvas still draws its own services');
});

test('an app with no services is a 404', async () => {
  assert.equal((await page('/apps/nope')).status, 404);
});

test("another org's app is a 404, not a canvas of someone else's services", async () => {
  const other = await signInAs(app.handle, { id: 7401, login: 'stranger' });
  const res = await app.handle(new Request('http://localhost/apps/gallery', asUser(other)));
  assert.equal(res.status, 404);
});
