/**
 * The adapter, against a fake that records every request.
 *
 * Four of these assertions are non-negotiable: `createSprite` sends `{name}`
 * and nothing else, the sprite's `id` is the machine's name, a restore makes
 * exactly one request and creates nothing, and a spawn defaults `stdin` off.
 */

import test from 'node:test'
import assert from 'node:assert/strict'

import { PilotsError } from '../src/errors.ts'
import type { WebSocketCtor } from '../src/stream.ts'
import { SpritesClient, shellQuote } from '../src/sprites-compat.ts'
import type { ExecResult } from '../src/sprites-compat.ts'
import { FakeHostd, json, machine, noContent } from './fakes/hostd.ts'
import { FakeWebSocket } from './fakes/websocket.ts'

const M_ID = 'm-000000000000000000000001'

/** A fake wired with the routes the adapter uses, plus a client on it. */
async function withFake(
  body: (client: SpritesClient, fake: FakeHostd) => Promise<void>,
  configure: (fake: FakeHostd) => void = () => {},
): Promise<void> {
  const fake = new FakeHostd()
  fake
    .on('POST /v1/machines', (_req, res) => json(res, 201, machine()))
    .on('GET /v1/machines', (_req, res) => json(res, 200, [machine()]))
    .on('GET /v1/machines/{id}', (_req, res) => json(res, 200, machine()))
    .on('DELETE /v1/machines/{id}', (_req, res) => noContent(res))
    .on('POST /v1/machines/{id}/exec', (_req, res) =>
      json(res, 200, { stdout: 'out', stderr: 'err', exit_code: 3 }),
    )
    .on('POST /v1/machines/{id}/checkpoints', (_req, res) =>
      json(res, 201, { id: 'ck-1', machine_id: M_ID, seq: 1, durable: false, created_at: 1_756_000_500 }),
    )
    .on('GET /v1/machines/{id}/checkpoints', (_req, res) =>
      json(res, 200, [
        { id: 'ck-1', machine_id: M_ID, seq: 1, comment: 'msg-1', durable: true, created_at: 1_756_000_500 },
      ]),
    )
    .on('POST /v1/checkpoints/{id}/restore', (_req, res) => json(res, 200, machine()))
  configure(fake)
  await fake.start()
  try {
    await body(
      new SpritesClient('pilot_deadbeef', {
        baseURL: fake.baseURL,
        WebSocket: FakeWebSocket as unknown as WebSocketCtor,
      }),
      fake,
    )
  } finally {
    await fake.stop()
  }
}

test('createSprite sends {name} only and its id is the name', async () => {
  await withFake(async (client, fake) => {
    const sprite = await client.createSprite('demo')
    assert.equal(fake.only.body, '{"name":"demo"}')
    // The id a consumer persists is handed back as a path segment to a
    // name-keyed route, so it has to be the name.
    assert.equal(sprite.id, 'demo')
    assert.equal(sprite.name, 'demo')
    assert.equal(sprite.machineId, M_ID)
    assert.equal(sprite.url, 'https://demo.pilotrun.app')
    assert.equal(sprite.status, 'running')
  })
})

test('getSprite lists once, then answers from the cache', async () => {
  await withFake(async (client, fake) => {
    const first = await client.getSprite('demo')
    const second = await client.getSprite('demo')
    assert.equal(first.machineId, second.machineId)
    assert.equal(fake.requests.length, 1)
    assert.equal(fake.requests[0]!.path, '/v1/machines')
  })
})

test('a 404 on a cached machine evicts it and the next call re-lists', async () => {
  let execCalls = 0
  await withFake(
    async (client, fake) => {
      const sprite = await client.getSprite('demo')
      const result = await sprite.exec('true')
      assert.equal(result.exitCode, 3)
      assert.deepEqual(
        fake.requests.map((r) => `${r.method} ${r.path}`),
        [
          'GET /v1/machines',
          `POST /v1/machines/${M_ID}/exec`,
          'GET /v1/machines',
          `POST /v1/machines/${M_ID}/exec`,
        ],
      )
    },
    (fake) =>
      fake.on('POST /v1/machines/{id}/exec', (_req, res) => {
        execCalls += 1
        if (execCalls === 1) return json(res, 404, { error: 'state: not found' })
        json(res, 200, { stdout: 'out', stderr: 'err', exit_code: 3 })
      }),
  )
})

test('a machine id resolves when no name matches it', async () => {
  await withFake(async (client, fake) => {
    const sprite = await client.getSprite(M_ID)
    assert.equal(sprite.machineId, M_ID)
    assert.deepEqual(
      fake.requests.map((r) => r.path),
      ['/v1/machines', `/v1/machines/${M_ID}`],
    )
  })
})

test('execFile shell-quotes every argument and maps exit_code', async () => {
  await withFake(async (client, fake) => {
    const sprite = await client.getSprite('demo')
    const result: ExecResult = await sprite.execFile(
      'bash',
      ['-c', 'echo "hi there" > /tmp/x'],
      { cwd: '/home/sprite/app', env: { A: '1' }, timeout: 5000 },
    )
    assert.deepEqual(result, { stdout: 'out', stderr: 'err', exitCode: 3 })

    const exec = fake.requests[1]!
    assert.equal(exec.path, `/v1/machines/${M_ID}/exec`)
    assert.deepEqual(exec.json, {
      cmd: `'bash' '-c' 'echo "hi there" > /tmp/x'`,
      cwd: '/home/sprite/app',
      env: { A: '1' },
      timeout_ms: 5000,
    })
  })
})

test('a single quote survives the quoting', () => {
  assert.equal(shellQuote(['echo', "it's"]), `'echo' 'it'\\''s'`)
})

test('createCheckpoint returns a Response whose one NDJSON line carries id', async () => {
  await withFake(async (client, fake) => {
    const sprite = await client.getSprite('demo')
    const res = await sprite.createCheckpoint('msg-1')
    assert.ok(res instanceof Response)

    const text = await res.text()
    const lines = text.trim().split('\n').filter(Boolean)
    assert.equal(lines.length, 1)
    assert.equal(JSON.parse(lines[0]!).id, 'ck-1')
    assert.deepEqual(fake.requests[1]!.json, { comment: 'msg-1' })
  })
})

test('listCheckpoints hands back real Dates in milliseconds', async () => {
  await withFake(async (client) => {
    const sprite = await client.getSprite('demo')
    const [checkpoint] = await sprite.listCheckpoints()
    assert.ok(checkpoint!.createTime instanceof Date)
    assert.equal(checkpoint!.createTime.getTime(), 1_756_000_500 * 1000)
    assert.equal(checkpoint!.comment, 'msg-1')
  })
})

test('restoreCheckpoint makes exactly one request and creates nothing', async () => {
  await withFake(async (client, fake) => {
    // The lazy handle, so nothing has been resolved: a restore needs no
    // machine at all, and must not go looking for one.
    const res = await client.sprite('demo').restoreCheckpoint('ck-1')
    assert.ok(res instanceof Response)

    const req = fake.only
    assert.equal(req.method, 'POST')
    assert.equal(req.path, '/v1/checkpoints/ck-1/restore')

    const line = (await res.text()).trim()
    // The URL a consumer persisted at create time is the URL it gets back.
    assert.equal(JSON.parse(line).url, 'https://demo.pilotrun.app')
    assert.equal(JSON.parse(line).id, M_ID)
  })
})

test('setPublicUrl resolves without touching the network', async () => {
  await withFake(async (client, fake) => {
    await client.setPublicUrl('demo')
    assert.deepEqual(fake.requests, [])
  })
})

test('deleteSprite deletes by machine id, not by name', async () => {
  await withFake(async (client, fake) => {
    await client.deleteSprite('demo')
    assert.deepEqual(
      fake.requests.map((r) => `${r.method} ${r.path}`),
      ['GET /v1/machines', `DELETE /v1/machines/${M_ID}`],
    )
  })
})

test('spawn dials the machine exec stream with stdin off', async () => {
  await withFake(async (client) => {
    const sprite = await client.getSprite('demo')
    sprite.spawn('bash', ['-c', 'true'], { cwd: '/home/sprite/app', env: { A: '1' } })

    const url = new URL(FakeWebSocket.last!.url)
    // The machine route, not the sprites alias: the alias exists for a
    // consumer that builds its own URL, not for this SDK.
    assert.equal(url.pathname, `/v1/machines/${M_ID}/exec/stream`)
    assert.equal(url.searchParams.get('stdin'), 'false')
    assert.deepEqual(url.searchParams.getAll('cmd'), ['bash', '-c', 'true'])
    assert.equal(url.searchParams.get('dir'), '/home/sprite/app')
  })
})

test('spawn on an unresolved handle says how to resolve it', async () => {
  await withFake(async (client) => {
    assert.throws(() => client.sprite('demo').spawn('bash'), PilotsError)
  })
})

test('the client exposes baseURL, token and timeout', async () => {
  await withFake(async (client, fake) => {
    assert.equal(client.baseURL, fake.baseURL)
    assert.equal(client.token, 'pilot_deadbeef')
    assert.equal(client.timeout, 30_000)
    assert.equal(new SpritesClient('t', { baseURL: 'http://x', timeout: 300_000 }).timeout, 300_000)
  })
})
