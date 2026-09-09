/**
 * The build log: the NDJSON route, the Deployments tab that follows it, and
 * the verdict the element reads off the stream.
 *
 * Ownership is this app's own `builds` row, so a job this org did not start is
 * a 404 rather than a 403: a job id must leak nothing about existing.
 *
 * The element READS the verdict; it no longer decides it. The build carries
 * the deploy it is for, so hostd cuts the release and puts its id on the last
 * line. Counterfactual: put the POST back in `<build-log>` and the two
 * `verdictOf` tests below still pass, which is why the one that matters --
 * exactly one release when nobody is watching -- lives in `scripts/e2e.mjs`
 * against a real host.
 */
import assert from 'node:assert/strict';
import { before, test } from 'node:test';
import { asUser, bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';
import type { Service } from '@pilots/sdk';
import { verdictOf, failureText } from '#modules/services/components/build-log.ts';
import { awaitsRelease } from '#modules/services/utils/build-follow.ts';

let app: TestApp;
let cookie: string;
let org = '';

before(async () => {
  app = await bootApp();
  cookie = await signInAs(app.handle, { id: 7810, login: 'builder' });
  const { db } = await import('#db/connection.server.ts');
  const { builds } = await import('#db/schema.server.ts');
  org = (await db.query.orgs.findMany()).find((o) => o.slug === 'builder')!.id;
  app.fleet.data.services.push({ id: 'svc-b', name: 'web', org_id: org, app: 'shop', replicas: 1 } as unknown as Service);
  await db
    .insert(builds)
    .values({ orgId: org, serviceId: 'svc-b', jobId: 'bld-mine', repo: 'acme/shop', ref: 'main', startedBy: 1 })
    .run();
  await db
    .insert(builds)
    .values({ orgId: 'someone-else', serviceId: 'svc-x', jobId: 'bld-theirs', repo: 'x/y', ref: 'main', startedBy: 2 })
    .run();
});

test('a job the org did not start is a 404, not a 403', async () => {
  const res = await app.handle(new Request('http://localhost/api/builds/bld-theirs/logs', asUser(cookie)));
  assert.equal(res.status, 404);
  const none = await app.handle(new Request('http://localhost/api/builds/bld-nope/logs', asUser(cookie)));
  assert.equal(none.status, 404);
});

test('an owned job streams its lines as NDJSON, following when asked', async () => {
  app.fleet.calls.length = 0;
  app.fleet.data.buildLines.push({ step: 'fetch', line: 'cloning', ts: 1 }, { result: 'rootfs-123', ts: 2 });
  const res = await app.handle(new Request('http://localhost/api/builds/bld-mine/logs?follow=1', asUser(cookie)));
  assert.equal(res.status, 200);
  assert.match(res.headers.get('content-type') ?? '', /x-ndjson/);
  const lines = (await res.text()).trim().split('\n').map((l) => JSON.parse(l));
  assert.deepEqual(lines, [
    { step: 'fetch', line: 'cloning', ts: 1 },
    { result: 'rootfs-123', ts: 2 },
  ]);
  assert.deepEqual(app.fleet.calls.find((c) => c.method === 'builds.logs')!.args, ['bld-mine', { follow: true }]);
  assert.ok(app.fleet.calls.some((c) => c.method === 'as' && c.args[0] === org), 'read as the visitor org');
});

test('the Deployments tab follows a build it owns and lists the rest', async () => {
  const following = await app.handle(
    new Request('http://localhost/services/svc-b?tab=deployments&build=bld-mine', asUser(cookie)),
  );
  const body = await following.text();
  assert.match(body, /Building/);
  assert.match(body, /acme\/shop@main/);
  assert.match(body, /<build-log[^>]*build-id="bld-mine"[^>]*service-id="svc-b"[^>]*autodeploy/, 'the element follows and deploys');
  assert.match(body, /the Deploy form below takes it/, 'the scripting-off path is stated');

  const listed = await app.handle(new Request('http://localhost/services/svc-b?tab=deployments', asUser(cookie)));
  const list = await listed.text();
  assert.match(list, />Builds</);
  assert.match(list, /href="\/api\/builds\/bld-mine\/logs"/, 'the raw log link');
  assert.ok(!list.includes('bld-theirs'), 'another org\'s build never appears');
  assert.ok(!list.includes('<build-log'), 'nothing is followed unless asked');
});

test('the release on the last line is the verdict, and the image alone is not', () => {
  // The image existing says the BUILD worked. Only `release` says a
  // deployment was cut, and it is cut by the host: this element navigates to
  // it rather than posting anything, so a second tab on the same build lands
  // on the same deployment instead of rolling it out again.
  assert.deepEqual(verdictOf({ result: 'img-1' }), { kind: 'built', text: 'img-1' });
  assert.deepEqual(verdictOf({ result: 'img-1', release: 'rel-7' }), { kind: 'deployed', text: 'rel-7' });
  assert.equal(verdictOf({ step: 'FROM alpine', line: 'pulling' }), null);
});

test('a refused deploy keeps the engine s words, never a bare status', () => {
  // The regression this guards: "The image was built but the deploy was
  // refused (422)." A health gate names the instance whose console says why,
  // and that sentence threw it away.
  const gate = {
    error: 'the health gate never passed',
    code: 'health_gate_failed',
    next: "read the replica's console: pilot machines logs m_9",
  };
  const verdict = verdictOf(gate)!;
  assert.equal(verdict.kind, 'failed');
  assert.match(verdict.text, /health gate never passed/);
  assert.match(verdict.text, /health_gate_failed/);
  assert.match(verdict.text, /pilot machines logs m_9/);
  // A build that failed on its own has no `next`, and must not grow an empty
  // one or trailing whitespace.
  assert.equal(failureText({ error: 'exit status 1', code: 'build_failed' }), 'exit status 1 (build_failed)');
});

test('a build older than the running deployment is a log to read, not one to wait on', () => {
  // Every historical build carries a Follow link, and a finished build's log
  // REPLAYS its terminal `deployed` line. Acted on, that bounces the reader
  // out of the log they opened, announcing a deployment from days ago -- and
  // the log they asked for cannot be read at all.
  const build = { createdAt: new Date(10_000_000) };
  assert.equal(awaitsRelease(build, { created_at: 20_000 }), false, 'the deployment is newer than the build');
  assert.equal(awaitsRelease(build, { created_at: 9_000 }), true, 'the build is newer, so its release is still to come');
  assert.equal(awaitsRelease(build, undefined), true, 'a service with no deployment yet is the first-deploy case');
  assert.equal(awaitsRelease(undefined, { created_at: 20_000 }), false, 'nothing is being followed');
});

test('the tab waits on a fresh build and only shows the log of an old one', async () => {
  const { db } = await import('#db/connection.server.ts');
  const { builds } = await import('#db/schema.server.ts');

  await db
    .insert(builds)
    .values({ orgId: org, serviceId: 'svc-old', jobId: 'bld-old', repo: 'acme/shop', ref: 'main', startedBy: 1 })
    .run();
  const now = Math.floor(Date.now() / 1000);
  app.fleet.data.services.push({
    id: 'svc-old',
    name: 'old',
    org_id: org,
    app: 'shop',
    replicas: 1,
    release_id: 'rel-now',
  } as unknown as Service);
  // The deployment running now was cut AFTER that build row, which is the
  // shape of every build in the history list.
  app.fleet.data.releases['svc-old'] = [
    { id: 'rel-now', service_id: 'svc-old', healthy: true, created_at: now + 600, rootfs_build_id: 'img-1' },
  ] as never;

  const res = await app.handle(
    new Request('http://localhost/services/svc-old?tab=deployments&build=bld-old', asUser(cookie)),
  );
  const body = await res.text();
  const element = body.slice(body.indexOf('<build-log'), body.indexOf('</build-log>'));
  assert.ok(element.includes('build-id="bld-old"'), 'the log of the build asked for is still rendered');
  assert.ok(!/\bautodeploy\b/.test(element), 'an old build is not waited on');
  assert.ok(element.includes('release-id="rel-now"'), 'the element is told which deployment the page already shows');
  assert.match(body, /older than the deployment running now/, 'the heading says what this log is');
});

test('the deployment is readable without the build log, which is what the fallback needs', async () => {
  // A build's log lives on the ONE host that ran it, and this app reaches the
  // fleet at a hostname every host answers -- so a 404 there says "not on
  // this host", not "nothing was deployed". `<build-log>` therefore falls
  // back to the service, whose release row is replicated to every host. This
  // is that contract: the id the element compares against `release-id`, from
  // a route that never touches the build.
  const res = await app.handle(new Request('http://localhost/api/services/svc-old', asUser(cookie)));
  assert.equal(res.status, 200);
  const service = (await res.json()) as { release_id?: string };
  assert.equal(service.release_id, 'rel-now');

  // And it stays scoped: another org's service is a 404 here too, so the
  // fallback cannot be pointed at a deployment that is not the visitor's.
  const foreign = await app.handle(new Request('http://localhost/api/services/svc-x', asUser(cookie)));
  assert.equal(foreign.status, 404);
});

test('a build someone else owns is not followed even when named', async () => {
  const res = await app.handle(
    new Request('http://localhost/services/svc-b?tab=deployments&build=bld-theirs', asUser(cookie)),
  );
  const body = await res.text();
  assert.ok(!body.includes('<build-log'), 'a foreign job id is ignored, not followed');
});
