/**
 * The build log stream: it must yield as the bytes arrive, and it must never
 * read an interrupted build as a successful one.
 */

import test from 'node:test'
import assert from 'node:assert/strict'
import type { ServerResponse } from 'node:http'

import { PilotsClient } from '../src/client.ts'
import { BuildFailedError } from '../src/errors.ts'
import type { BuildLogLine } from '../src/types.ts'
import { FakeHostd } from './fakes/hostd.ts'

/** Writes NDJSON lines with a gap between them, flushing each. */
async function drip(res: ServerResponse, lines: unknown[], gapMs = 20): Promise<void> {
  res.writeHead(200, { 'content-type': 'application/x-ndjson', 'x-pilot-build-id': 'bld-1' })
  for (const line of lines) {
    res.write(JSON.stringify(line) + '\n')
    await new Promise((r) => setTimeout(r, gapMs))
  }
  res.end()
}

const accepted = { step: 'bld-1', stream: 'status', line: 'build accepted', ts: 1 }
const running = { step: 'FROM alpine', stream: 'stdout', line: 'pulling', ts: 2 }
const ok = { step: 'bld-1', stream: 'status', line: 'build succeeded', result: 'rootfs-xyz', ts: 3 }
const failed = { step: 'bld-1', stream: 'status', line: 'build failed', error: 'exit status 1', ts: 3 }

async function withFake(
  route: (res: ServerResponse) => Promise<void> | void,
  body: (client: PilotsClient, fake: FakeHostd) => Promise<void>,
): Promise<void> {
  const fake = new FakeHostd()
  fake.on('POST /v1/builds', (_req, res) => route(res))
  fake.on('GET /v1/builds/{id}/logs', (_req, res) => route(res))
  await fake.start()
  try {
    await body(new PilotsClient('pilot_k', { baseURL: fake.baseURL }), fake)
  } finally {
    await fake.stop()
  }
}

test('the build id comes from the header and the first line arrives early', async () => {
  await withFake(
    (res) => drip(res, [accepted, running, ok]),
    async (client) => {
      const build = await client.builds.create('a-tar')
      assert.equal(build.buildId, 'bld-1')

      const seen: BuildLogLine[] = []
      const times: number[] = []
      const started = Date.now()
      for await (const line of build) {
        seen.push(line)
        times.push(Date.now() - started)
      }

      assert.deepEqual(
        seen.map((l) => l.line),
        ['build accepted', 'pulling', 'build succeeded'],
      )
      // Buffering the whole body would make every line land at once, at the
      // end. A ten-minute build has to be watchable while it runs.
      assert.ok(times[0]! < times[2]! - 10, `lines arrived together: ${times.join(', ')}`)
    },
  )
})

test('result() returns the last line s result', async () => {
  await withFake(
    (res) => drip(res, [accepted, running, ok], 1),
    async (client) => {
      const build = await client.builds.create('a-tar')
      assert.equal(await build.result(), 'rootfs-xyz')
    },
  )
})

test('a failed build throws BuildFailedError carrying every line', async () => {
  await withFake(
    (res) => drip(res, [accepted, running, failed], 1),
    async (client) => {
      const build = await client.builds.create('a-tar')
      const err = await build.result().then(
        () => null,
        (e: unknown) => e,
      )
      assert.ok(err instanceof BuildFailedError, `got ${String(err)}`)
      assert.equal(err.buildId, 'bld-1')
      assert.equal(err.message, 'exit status 1')
      assert.equal(err.lines.length, 3)
    },
  )
})

test('a stream that ends with no verdict is a failure, not a success', async () => {
  await withFake(
    (res) => drip(res, [accepted, running], 1),
    async (client) => {
      const build = await client.builds.create('a-tar')
      const err = await build.result().then(
        () => null,
        (e: unknown) => e,
      )
      assert.ok(err instanceof BuildFailedError)
      assert.match(err.message, /without a verdict/)
    },
  )
})

test('the upload is sent as a tar, and logs asks to follow', async () => {
  await withFake(
    (res) => drip(res, [ok], 1),
    async (client, fake) => {
      await (await client.builds.create('a-tar')).result()
      assert.equal(fake.requests[0]!.headers['content-type'], 'application/x-tar')
      assert.equal(fake.requests[0]!.body, 'a-tar')

      const replay = await client.builds.logs('bld-1', { follow: true })
      assert.equal(replay.buildId, 'bld-1')
      await replay.result()
      assert.equal(fake.requests[1]!.query.get('follow'), '1')
    },
  )
})
