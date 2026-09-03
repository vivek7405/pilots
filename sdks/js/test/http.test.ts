/**
 * The transport, against a real server: what goes on the wire, and what a
 * failure becomes on the way back.
 */

import test from 'node:test'
import assert from 'node:assert/strict'

import { PilotsClient } from '../src/client.ts'
import {
  ComposePlanError,
  NotFoundError,
  PilotsError,
  QuotaExceededError,
} from '../src/errors.ts'
import { DEFAULT_BASE_URL, resolveBaseURL } from '../src/http.ts'
import { FakeHostd, json, machine, noContent } from './fakes/hostd.ts'

/** Starts a fake, hands it plus a client to the body, and always stops it. */
async function withFake(
  configure: (fake: FakeHostd) => void,
  body: (client: PilotsClient, fake: FakeHostd) => Promise<void>,
): Promise<void> {
  const fake = new FakeHostd()
  configure(fake)
  await fake.start()
  try {
    await body(new PilotsClient('pilot_deadbeef', { baseURL: fake.baseURL }), fake)
  } finally {
    await fake.stop()
  }
}

test('every call carries the bearer key, and a body is JSON', async () => {
  await withFake(
    (f) => f.on('POST /v1/machines', (_req, res) => json(res, 201, machine())),
    async (client, fake) => {
      const m = await client.machines.create({ name: 'demo' })
      assert.equal(m.url, 'https://demo.pilotrun.app')
      const req = fake.only
      assert.equal(req.headers.authorization, 'Bearer pilot_deadbeef')
      assert.equal(req.headers['content-type'], 'application/json')
      assert.equal(req.body, '{"name":"demo"}')
    },
  )
})

test('a 204 resolves undefined and sends no body', async () => {
  await withFake(
    (f) => f.on('POST /v1/machines/{id}/suspend', (_req, res) => noContent(res)),
    async (client, fake) => {
      assert.equal(await client.machines.suspend('m-1'), undefined)
      assert.equal(fake.only.path, '/v1/machines/m-1/suspend')
    },
  )
})

test('a 404 is a NotFoundError carrying hostd s message', async () => {
  await withFake(
    (f) => f.on('GET /v1/machines/{id}', (_req, res) => json(res, 404, { error: 'state: not found' })),
    async (client) => {
      const err = await client.machines.get('m-nope').then(
        () => null,
        (e: unknown) => e,
      )
      assert.ok(err instanceof NotFoundError, `got ${String(err)}`)
      assert.equal(err.status, 404)
      assert.equal(err.message, 'state: not found')
    },
  )
})

test('a 429 is a QuotaExceededError naming the quota, limit and used', async () => {
  await withFake(
    (f) =>
      f.on('POST /v1/machines', (_req, res) =>
        json(res, 429, { error: 'quota exceeded', quota: 'machines', limit: 20, used: 20 }),
      ),
    async (client) => {
      const err = await client.machines.create({ name: 'demo' }).then(
        () => null,
        (e: unknown) => e,
      )
      assert.ok(err instanceof QuotaExceededError, `got ${String(err)}`)
      assert.equal(err.quota, 'machines')
      assert.equal(err.limit, 20)
      assert.equal(err.used, 20)
      assert.equal(err.scope, undefined)
    },
  )
})

test('a build 429 carries the host scope', async () => {
  await withFake(
    (f) =>
      f.on('GET /v1/hosts', (_req, res) =>
        json(res, 429, { error: 'quota exceeded', quota: 'builds', limit: 4, used: 4, scope: 'host' }),
      ),
    async (client) => {
      const err = await client.hosts.list().then(
        () => null,
        (e: unknown) => e,
      )
      assert.ok(err instanceof QuotaExceededError)
      assert.equal(err.scope, 'host')
    },
  )
})

test('a 500 is a PilotsError whose message is the body s error', async () => {
  await withFake(
    (f) => f.on('GET /v1/hosts', (_req, res) => json(res, 500, { error: 'corrosion is unreachable' })),
    async (client) => {
      const err = await client.hosts.list().then(
        () => null,
        (e: unknown) => e,
      )
      assert.ok(err instanceof PilotsError)
      assert.ok(!(err instanceof NotFoundError))
      assert.equal(err.status, 500)
      assert.equal(err.message, 'corrosion is unreachable')
      assert.equal(err.body, '{"error":"corrosion is unreachable"}')
    },
  )
})

test('compose.plan sends JSON, never YAML, and a 400 lists what is unsupported', async () => {
  await withFake(
    (f) =>
      f.on('POST /v1/compose/plan', (_req, res) =>
        json(res, 400, {
          error: 'unsupported compose features',
          unsupported: [
            { service: 'web', key: 'privileged', message: 'moot in a microVM: the service already owns its kernel' },
          ],
        }),
      ),
    async (client, fake) => {
      const err = await client.compose
        .plan({ compose: 'services:\n  web:\n    image: nginx\n', env: { TAG: 'v1' } })
        .then(
          () => null,
          (e: unknown) => e,
        )
      assert.ok(err instanceof ComposePlanError, `got ${String(err)}`)
      assert.deepEqual(err.unsupported, [
        { service: 'web', key: 'privileged', message: 'moot in a microVM: the service already owns its kernel' },
      ])
      const req = fake.only
      assert.equal(req.headers['content-type'], 'application/json')
      assert.deepEqual(req.json, {
        compose: 'services:\n  web:\n    image: nginx\n',
        env: { TAG: 'v1' },
      })
    },
  )
})

test('services.patch sends PATCH with exactly the fields it was given', async () => {
  await withFake(
    (f) => f.on('PATCH /v1/services/{id}', (_req, res) => json(res, 200, { id: 'svc-1', name: 'web', replicas: 3 })),
    async (client, fake) => {
      const svc = await client.services.patch('svc-1', { replicas: 3 })
      assert.equal(svc.replicas, 3)
      const req = fake.only
      assert.equal(req.method, 'PATCH')
      assert.equal(req.path, '/v1/services/svc-1')
      assert.deepEqual(req.json, { replicas: 3 })
    },
  )
})

test('usage.get puts since and until in the query', async () => {
  await withFake(
    (f) => f.on('GET /v1/usage', (_req, res) => json(res, 200, { host_id: 'h1', since: 1, until: 2, orgs: {} })),
    async (client, fake) => {
      await client.usage.get({ since: 1, until: 2 })
      assert.equal(fake.only.query.get('since'), '1')
      assert.equal(fake.only.query.get('until'), '2')
    },
  )
})

test('apiKeys.list requires the org and sends it as a query parameter', async () => {
  await withFake(
    (f) => f.on('GET /v1/api-keys', (_req, res) => json(res, 200, [])),
    async (client, fake) => {
      await client.apiKeys.list('org_x')
      assert.equal(fake.only.query.get('org'), 'org_x')
    },
  )
})

test('trailing slashes on the base URL are stripped', () => {
  const client = new PilotsClient('pilot_k', { baseURL: 'https://api.example.com///' })
  assert.equal(client.baseURL, 'https://api.example.com')
})

test('an empty key throws before any request is made', () => {
  assert.throws(() => new PilotsClient(''), PilotsError)
})

test('PILOT_API_URL is honoured, and the default when it is unset', () => {
  const saved = process.env.PILOT_API_URL
  try {
    process.env.PILOT_API_URL = 'https://host-3.example.com/'
    assert.equal(resolveBaseURL(), 'https://host-3.example.com')
    // An explicit base URL still wins over the environment.
    assert.equal(resolveBaseURL('https://host-9.example.com'), 'https://host-9.example.com')
    delete process.env.PILOT_API_URL
    assert.equal(resolveBaseURL(), DEFAULT_BASE_URL)
  } finally {
    if (saved === undefined) delete process.env.PILOT_API_URL
    else process.env.PILOT_API_URL = saved
  }
})
