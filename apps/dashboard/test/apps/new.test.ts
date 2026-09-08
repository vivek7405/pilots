/**
 * /services/new and the create-from-repo action behind it.
 *
 * The page's plan renders need a GitHub App installation, which is a real
 * network read, so the App is stubbed the way the repository tests stub it.
 * With that in place every render the page can produce is driven through the
 * fake fleet, and the ACTION is driven for every refusal and the success path:
 * it re-plans through the fleet and does not trust the form, which is the
 * property worth pinning.
 */
import assert from 'node:assert/strict';
import { generateKeyPairSync } from 'node:crypto';
import { after, afterEach, before, test } from 'node:test';
import { submitForm } from '@webjsdev/server/testing';
import { PilotsError } from '@pilots/sdk';
import type { Service } from '@pilots/sdk';
import { asUser, bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';

let app: TestApp;
let cookie: string;
let org = '';
const realFetch = globalThis.fetch;

const TEST_PEM = generateKeyPairSync('rsa', {
  modulusLength: 2048,
  privateKeyEncoding: { type: 'pkcs8', format: 'pem' },
  publicKeyEncoding: { type: 'spki', format: 'pem' },
}).privateKey;

/** Stubs the App's installation listing. `null` means "not installed". */
function stubInstallations(installation: { id: number; login: string } | null): void {
  process.env.PILOT_GITHUB_APP_ID = '12345';
  process.env.PILOT_GITHUB_APP_KEY = TEST_PEM;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.startsWith('https://api.github.com/app/installations')) {
      const body = installation ? [{ id: installation.id, account: { login: installation.login }, suspended_at: null }] : [];
      return new Response(JSON.stringify(body), { headers: { 'content-type': 'application/json' } });
    }
    return new Response('{}', { status: 404 });
  }) as typeof globalThis.fetch;
}

const ONE_STEP = {
  plan: {
    app: 'shop',
    steps: [{ name: 'web', replicas: 1, vcpus: 1, mem_mib: 512, env: { NODE_ENV: 'production' }, health: { path: '/healthz' } }],
  },
  detected: [{ service: 'web', source: 'recipe', framework: 'webjs', dir: '.', port: 8080 }],
};

before(async () => {
  app = await bootApp();
  cookie = await signInAs(app.handle, { id: 7800, login: 'creator' });
  const { db } = await import('#db/connection.server.ts');
  org = (await db.query.orgs.findMany()).find((o) => o.slug === 'creator')!.id;
  app.fleet.data.services.push({ id: 'svc-new', name: 'web', org_id: org, app: 'shop', replicas: 1 } as unknown as Service);
});

afterEach(() => {
  globalThis.fetch = realFetch;
  delete process.env.PILOT_GITHUB_APP_ID;
  delete process.env.PILOT_GITHUB_APP_KEY;
  app.fleet.data.plan = null;
  app.fleet.data.planQueue.length = 0;
  app.fleet.data.planError = null;
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

async function page(query = ''): Promise<string> {
  const res = await app.handle(new Request(`http://localhost/services/new${query}`, asUser(cookie)));
  assert.equal(res.status, 200);
  return res.text();
}

const LOOK = '?repo=acme/shop&ref=main&app=shop';

test('the page is a look form first, and creates nothing by looking', async () => {
  app.fleet.calls.length = 0;
  const body = await page();
  assert.match(body, /New app/);
  assert.match(body, /<form method="get" action="\/services\/new"/, 'looking is a plain GET');
  assert.match(body, /Look inside/);
  assert.ok(!app.fleet.calls.some((c) => c.method === 'services.create'), 'nothing is created on the way in');
});

test('a value that is not owner/name is told so', async () => {
  const body = await page('?repo=not-a-repo');
  assert.match(body, /not a repository/i);
});

test('an owner without the App is pointed at installing it', async () => {
  // Its own owner: the installation lookup is memoised per owner, so a null
  // answer for `acme` here would be the answer every later test got too.
  stubInstallations(null);
  const body = await page('?repo=nope/shop&ref=main&app=shop');
  assert.match(body, /not installed on <strong>nope<\/strong>/);
  assert.match(body, /Install the pilots app on nope/);
});

test('a fleet with no GitHub App shows the CLI, and so does any other refusal', async () => {
  stubInstallations({ id: 1, login: 'acme' });
  app.fleet.data.planError = new PilotsError('no App on this fleet', { status: 503, code: 'not_configured', next: 'send a tar' });
  assert.match(await page(LOOK), /has no GitHub App/);

  app.fleet.data.planError = new PilotsError('the build gate is full', { status: 429, code: 'quota_exceeded', next: 'wait for a build to finish' });
  const body = await page(LOOK);
  assert.match(body, /the build gate is full/);
  assert.match(body, /wait for a build to finish/, 'the next step travels with the message');
  assert.match(body, /pilot deploy/);
});

test('a repository nothing recognises lists what was looked for and what is there', async () => {
  stubInstallations({ id: 1, login: 'acme' });
  app.fleet.data.planError = new PilotsError('unknown framework', {
    status: 400,
    code: 'unknown_framework',
    next: 'add a Dockerfile',
    details: { dir: '.', looked_for: ['package.json'], listing: ['README.md', 'main.rs'], rules: ['a package.json with a start script'] },
  });
  const body = await page(LOOK);
  assert.match(body, /could not tell how to build/);
  assert.match(body, /Add a Dockerfile, then try again/);
  assert.match(body, /a package.json with a start script/);
  assert.match(body, /main\.rs/);
});

test('several services, or one that needs storage, hand the visitor the CLI', async () => {
  stubInstallations({ id: 1, login: 'acme' });
  app.fleet.data.plan = { plan: { app: 'shop', steps: [{ name: 'web', replicas: 1 }, { name: 'db', replicas: 1 }] }, detected: [] };
  const many = await page(LOOK);
  assert.match(many, /2 services found/);
  assert.match(many, /pilot deploy/);
  assert.ok(!many.includes('name="name"'), 'no create form for a multi-service plan');

  app.fleet.data.plan = { plan: { app: 'shop', steps: [{ name: 'web', replicas: 1, volumes: [{ name: 'data' }] }] }, detected: [] };
  const storage = await page(LOOK);
  assert.match(storage, /needs storage/);
  assert.ok(!storage.includes('name="name"'));
});

test('one deployable service renders the confirmation and a bound Deploy form', async () => {
  stubInstallations({ id: 1, login: 'acme' });
  app.fleet.data.plan = ONE_STEP;
  const body = await page(LOOK);
  assert.match(body, /Ready to deploy/);
  assert.match(body, /found a webjs app in <strong>acme\/shop<\/strong>/);
  assert.match(body, /8080/);
  assert.match(body, /\/healthz/);
  assert.match(body, /NODE_ENV/);
  assert.match(body, /name="name" value="web"/, 'the name defaults to the step');
  assert.match(body, /name="domain" value="web"/, 'and so does the address');
  assert.match(body, />Deploy</);
});

async function create(fields: Record<string, string>) {
  return submitForm(
    app.handle,
    `/services/new${LOOK}`,
    { repo: 'acme/shop', ref: 'main', app: 'shop', name: 'web', domain: 'web', ...fields },
    { cookies: cookie, match: 'Deploy' },
  );
}

test('the action re-plans and refuses a multi-service repository', async () => {
  stubInstallations({ id: 1, login: 'acme' });
  app.fleet.calls.length = 0;
  // The page renders a one-step form; by submit time the repository plans as
  // two services. The action believes the fleet, not the form it was sent.
  const MULTI = { plan: { app: 'shop', steps: [{ name: 'web', replicas: 1 }, { name: 'db', replicas: 1 }] }, detected: [] };
  // Render (one step), act (two steps), re-render the page with the refusal.
  app.fleet.data.planQueue.push(ONE_STEP, MULTI, MULTI);
  const res = await create({});
  assert.equal(res.status, 422);
  assert.match(await res.text(), /deploys as 2 services/, 'the refusal is shown, not dropped');
  assert.equal(app.fleet.calls.filter((c) => c.method === 'planRepo').length, 3, 'planned to render, to act, and to re-render');
  assert.ok(!app.fleet.calls.some((c) => c.method === 'builds.createFromRepo'), 'no build was started');
});

test('a secret the plan asks for is required', async () => {
  stubInstallations({ id: 1, login: 'acme' });
  app.fleet.data.plan = { plan: { app: 'shop', steps: [{ name: 'web', replicas: 1, secret_refs: { DB_URL: 'secret://db' } }] }, detected: [] };
  const res = await create({});
  assert.equal(res.status, 422);
  assert.match(await res.text(), /Required/);
});

test('a one-step plan starts the build, creates the service as the org, and lands on the build', async () => {
  stubInstallations({ id: 1, login: 'acme' });
  app.fleet.calls.length = 0;
  app.fleet.data.plan = ONE_STEP;
  const res = await create({});
  assert.equal(res.status, 303);
  assert.equal(res.headers.get('location'), '/services/svc-new?tab=deployments&build=bld-fake&ok=building');

  assert.ok(app.fleet.calls.some((c) => c.method === 'as' && c.args[0] === org), 'acted as the visitor org');
  const build = app.fleet.calls.find((c) => c.method === 'builds.createFromRepo')!;
  assert.deepEqual(build.args, [{ repo: 'acme/shop', ref: 'main' }, { app: 'shop' }]);
  const created = app.fleet.calls.find((c) => c.method === 'services.create')!;
  assert.deepEqual(created.args[0], {
    name: 'web',
    app: 'shop',
    replicas: 1,
    repo: 'acme/shop',
    branch: 'main',
    autodeploy: true,
    health: { path: '/healthz' },
    env: { NODE_ENV: 'production' },
    domain: 'web',
  });

  const { db } = await import('#db/connection.server.ts');
  const builds = await db.query.builds.findMany();
  assert.ok(builds.some((b) => b.jobId === 'bld-fake' && b.serviceId === 'svc-new' && b.repo === 'acme/shop'), 'the build is recorded');
  const conns = await db.query.repoConnections.findMany();
  assert.ok(conns.some((c) => c.serviceId === 'svc-new' && c.repo === 'acme/shop' && c.autodeploy), 'the repository is connected');
});

// The ORDER of the two fleet calls, which is only observable when the second
// one fails. The build used to be started first, so a refused create -- a name
// already taken, a domain already claimed, a quota reached -- left a real
// build running on a host against a MaxBuilds slot, with no service to deliver
// to and no `builds` row to find it by, reported to the person as a 502.
//
// Counterfactual: move `builds.createFromRepo` back above `services.create`
// and this fails, while the success test above keeps passing because both
// calls happen there either way.
test('a refused create starts no build, so nothing is left running on a host', async () => {
  stubInstallations({ id: 1, login: 'acme' });
  app.fleet.calls.length = 0;
  app.fleet.data.plan = ONE_STEP;
  app.fleet.data.createServiceError = new PilotsError('a service named web already exists in shop', { status: 409, code: 'name_taken' });

  const res = await create({});
  assert.equal(res.status, 502);
  assert.ok(
    app.fleet.calls.some((c) => c.method === 'services.create'),
    'the create was attempted',
  );
  assert.ok(
    !app.fleet.calls.some((c) => c.method === 'builds.createFromRepo'),
    'and no build was started for a service that does not exist',
  );
});

// `app` is the only name here that can be DERIVED rather than typed, from the
// repo half of owner/name, so an unvalidated one reaches hostd as an app group
// nobody typed and nobody can address.
test('the app name is validated like the others', async () => {
  stubInstallations({ id: 1, login: 'acme' });
  app.fleet.calls.length = 0;
  app.fleet.data.plan = ONE_STEP;
  const res = await create({ app: 'Not An App' });
  assert.equal(res.status, 422);
  assert.ok(!app.fleet.calls.some((c) => c.method === 'services.create'), 'nothing was created');
  assert.ok(!app.fleet.calls.some((c) => c.method === 'builds.createFromRepo'), 'nothing was built');
});
