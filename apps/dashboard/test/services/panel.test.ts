/**
 * The service panel at its own address, `/services/<id>?tab=`.
 *
 * The same fragment the canvas opens in a slide-over renders here full
 * width, so what is asserted is the panel's own contract: a tab is a URL and
 * only that tab renders; every form carries where it returns to and refuses
 * a return that leaves the site; the one lifecycle knob the deploy form
 * offers reaches the engine as a knob and only when ticked; and the terminal
 * is a shell on a running instance or an honest empty state.
 *
 * Counterfactuals: drop the `back` guard and the refusal test redirects off
 * site; drop the `keep_awake` mapping and the knobs test sees none; render
 * every tab at once and the "only the selected tab" assertion fails.
 */

import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { submitForm } from '@webjsdev/server/testing';
import { asUser, bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';
import type { Machine, Release, Service } from '@pilots/sdk';

let app: TestApp;
let cookie = '';
let org = '';

const NOW_SEC = Math.floor(Date.now() / 1000);

before(async () => {
  app = await bootApp();
  cookie = await signInAs(app.handle, { id: 7500, login: 'panelist' });
  const { db } = await import('#db/connection.server.ts');
  org = (await db.query.orgs.findMany()).find((o) => o.slug === 'panelist')!.id;

  app.fleet.data.services.push({
    id: 'svc-web',
    name: 'web',
    org_id: org,
    replicas: 1,
    url: 'https://web.example',
    release_id: 'rel-w',
    knobs: {},
    autodeploy: false,
    created_at: 1,
  } as unknown as Service);
  app.fleet.data.releases['svc-web'] = [
    { id: 'rel-w', service_id: 'svc-web', healthy: true, created_at: NOW_SEC - 3600, rootfs_build_id: 'bld-w' },
    { id: 'rel-w0', service_id: 'svc-web', healthy: true, created_at: NOW_SEC - 86_400, rootfs_build_id: 'bld-w0' },
  ] as Release[];
  app.fleet.data.machines.push({
    id: 'm-web',
    name: 'web-1',
    state: 'running',
    org_id: org,
    host_id: 'h-1',
    service_id: 'svc-web',
    release_id: 'rel-w',
  } as unknown as Machine);
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

async function page(path: string): Promise<{ status: number; body: string }> {
  const res = await app.handle(new Request(`http://localhost${path}`, asUser(cookie)));
  return { status: res.status, body: await res.text() };
}

test('the default tab is Deployments, showing what is live and what can be rolled back to', async () => {
  const { status, body } = await page('/services/svc-web');
  assert.equal(status, 200);
  assert.match(body, /aria-label="Service sections"/);
  assert.match(body, /href="\/services\/svc-web\?tab=deployments"[^>]*aria-current="page"/);
  assert.match(body, /data-current-deployment/);
  assert.match(body, /rel-w/);
  assert.equal((body.match(/Roll back to this/g) ?? []).length, 1, 'the older healthy deployment is the one target');
  const deployForm = body.slice(body.lastIndexOf('<form', body.indexOf('name="release"')), body.indexOf('</form>', body.indexOf('name="release"')));
  assert.match(deployForm, /name="back" value="\/services\/svc-web\?tab=deployments"/, 'the deploy form carries its return');
  assert.ok(!body.includes('<slide-over'), 'full width, no panel chrome');
  assert.ok(!body.includes('aria-label="Close"'), 'nothing to close');
});

test('?tab=settings renders the Instances form and only that tab', async () => {
  const { body } = await page('/services/svc-web?tab=settings');
  assert.match(body, /href="\/services\/svc-web\?tab=settings"[^>]*aria-current="page"/);
  assert.match(body, /name="replicas"/);
  assert.match(body, /Domains/);
  assert.ok(!body.includes('data-current-deployment'), 'only the selected tab renders');
});

test('a form returns to the tab it was on', async () => {
  app.fleet.calls.length = 0;
  const res = await submitForm(
    app.handle,
    '/services/svc-web?tab=settings',
    { service: 'svc-web', replicas: '2', back: '/services/svc-web?tab=settings' },
    { cookies: cookie, match: 'Save' },
  );
  assert.equal(res.status, 303);
  assert.equal(res.headers.get('location'), '/services/svc-web?tab=settings&ok=saved');
  assert.deepEqual(app.fleet.calls.find((c) => c.method === 'services.patch')!.args, ['svc-web', { replicas: 2 }]);
});

test('a back that leaves the site is refused and the service page is used instead', async () => {
  const res = await submitForm(
    app.handle,
    '/services/svc-web?tab=settings',
    { service: 'svc-web', replicas: '1', back: 'https://evil.example/' },
    { cookies: cookie, match: 'Save' },
  );
  assert.equal(res.status, 303);
  assert.equal(res.headers.get('location'), '/services/svc-web?ok=saved');
});

test('the keep-awake box becomes the one knob the deploy carries, and nothing when unticked', async () => {
  app.fleet.calls.length = 0;
  let res = await submitForm(
    app.handle,
    '/services/svc-web?tab=deployments',
    { service: 'svc-web', release: 'rel-w0', keep_awake: 'on' },
    { cookies: cookie, match: 'Deploy' },
  );
  assert.equal(res.status, 303);
  assert.deepEqual(app.fleet.calls.find((c) => c.method === 'services.deploy')!.args, [
    'svc-web',
    { release: 'rel-w0', knobs: { min_machines_running: 1 } },
  ]);

  app.fleet.calls.length = 0;
  res = await submitForm(
    app.handle,
    '/services/svc-web?tab=deployments',
    { service: 'svc-web', release: 'rel-w0' },
    { cookies: cookie, match: 'Deploy' },
  );
  assert.equal(res.status, 303);
  assert.deepEqual(app.fleet.calls.find((c) => c.method === 'services.deploy')!.args, ['svc-web', { release: 'rel-w0' }]);
});

test('the Terminal tab is a shell on a running instance, with a full-screen link', async () => {
  const { body } = await page('/services/svc-web?tab=terminal');
  assert.match(body, /<machine-terminal[^>]*machine-id="m-web"/);
  assert.match(body, /href="\/machines\/m-web\/terminal"/);
  assert.match(body, /pilot console web-1/, 'with scripting off the CLI is named');
});

test('a service with nothing running gets an empty Terminal tab pointing at Deployments', async () => {
  app.fleet.data.machines.find((m) => m.id === 'm-web')!.state = 'suspended';
  try {
    const { body } = await page('/services/svc-web?tab=terminal');
    assert.ok(!body.includes('<machine-terminal'));
    assert.match(body, /No instance is running right now/);
    assert.match(body, /href="\/services\/svc-web\?tab=deployments"/);
  } finally {
    app.fleet.data.machines.find((m) => m.id === 'm-web')!.state = 'running';
  }
});

test('an unknown tab falls back to Deployments rather than an empty panel', async () => {
  const { body } = await page('/services/svc-web?tab=nonsense');
  assert.match(body, /data-current-deployment/);
});

/**
 * The `2/1` case, end to end.
 *
 * A real fleet had `website` asking for one instance, one machine on its
 * current release and one still on the release before it. The page counted
 * both and read `2/1 instances online`. The engine never counted the second:
 * `replicasOf` pairs service id with release id, so its autoscaler cannot see
 * a leftover and will never retire one.
 *
 * Both halves are asserted here because they pull in opposite directions: the
 * VERDICT must ignore the leftover, and the instance list must still show it,
 * or a machine holding a URL and burning quota would be named on no page at
 * all. The `N/M instances online` count is asserted where it is rendered, on
 * the apps page (`test/apps/list.test.ts`).
 */
test('a machine left on an older release is not counted, but is still named', async () => {
  app.fleet.data.machines.push({
    id: 'm-web-old',
    name: 'web-0',
    state: 'suspended',
    org_id: org,
    host_id: 'h-1',
    service_id: 'svc-web',
    release_id: 'rel-w0',
  } as unknown as Machine);

  try {
    // The leftover is suspended, not failed, so nothing should change here --
    // the assertion that matters is the one below it, with a failed leftover.
    const { body } = await page('/services/svc-web');
    assert.ok(!/Failing/.test(body), 'a service serving its current release is not failing');

    // Deployments lists instances INSIDE the current release's card, under its
    // deploy stamp and image id, so it answers "what is running this release".
    // The leftover answers a different question and does not belong there.
    //
    // Asserted on the rendered LINK, not on the name: `detail` is serialised
    // into the page for hydration, so every attached machine's name appears in
    // that payload whatever the markup shows. `href="/machines/<id>"` is only
    // written by a list that actually drew the row.
    const deployments = await page('/services/svc-web?tab=deployments');
    assert.match(deployments.body, /href="\/machines\/m-web"/, 'the current release names the instance running it');
    assert.ok(
      !/href="\/machines\/m-web-old"/.test(deployments.body),
      'and not the one left on the release before it, which is not running this deployment',
    );

    // Metrics is the one place that shows everything attached, marked, so a
    // machine holding a URL and burning quota is named on some page.
    const metrics = await page('/services/svc-web?tab=metrics');
    assert.match(metrics.body, /href="\/machines\/m-web-old"/, 'the leftover is still listed among the instances');
    assert.match(metrics.body, /On an older release/, 'and it is marked as what it is');
    assert.match(metrics.body, /web-1/, 'beside the instance that is serving');
  } finally {
    app.fleet.data.machines = app.fleet.data.machines.filter((m) => m.id !== 'm-web-old');
  }
});
