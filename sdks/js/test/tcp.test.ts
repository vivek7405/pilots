/**
 * The TCP tunnel, driven frame by frame.
 *
 * What matters here is that a database driver can use it unmodified, so the
 * assertions are about the things a driver depends on: it is connected before
 * anybody writes, bytes arrive as bytes, a large write does not become one
 * oversized frame, and a closed tunnel ends the stream rather than erroring.
 */

import test from 'node:test'
import assert from 'node:assert/strict'

import { PilotsClient } from '../src/client.ts'
import type { WebSocketCtor } from '../src/stream.ts'
import { FakeWebSocket } from './fakes/websocket.ts'

const KEY = 'pilot_deadbeef'

function client(): PilotsClient {
  return new PilotsClient(KEY, {
    baseURL: 'https://host-1.example.com',
    WebSocket: FakeWebSocket as unknown as WebSocketCtor,
  })
}

test('the dial URL names the machine and port, with the key as a subprotocol', async () => {
  const pending = client().machines.tcp('m-1', 5432)
  const ws = FakeWebSocket.last!
  ws.open()
  await pending

  const url = new URL(ws.url)
  assert.equal(url.protocol, 'wss:')
  assert.equal(url.pathname, '/v1/machines/m-1/tcp/5432')
  assert.deepEqual(ws.protocols, [`authorization.bearer.${KEY}`])
})

// A driver handed a stream that is not yet connected writes its handshake into
// a queue and waits for a reply that cannot arrive until the queue drains. That
// is a hang with nothing in any log, so the promise waits for open.
test('the promise does not resolve until the socket is open', async () => {
  let opened = false
  const pending = client().machines.tcp('m-1', 5432).then(() => {
    assert.equal(opened, true, 'resolved before the socket opened')
  })
  const ws = FakeWebSocket.last!
  opened = true
  ws.open()
  await pending
})

test('bytes from the machine arrive as bytes', async () => {
  const pending = client().machines.tcp('m-1', 6379)
  const ws = FakeWebSocket.last!
  ws.open()
  const duplex = await pending

  const read = new Promise<Buffer>((resolve) => {
    duplex.once('data', (chunk: Buffer) => resolve(chunk))
  })
  ws.dispatchEvent(
    new MessageEvent('message', { data: new TextEncoder().encode('+PONG\r\n').buffer }),
  )
  assert.equal((await read).toString('utf8'), '+PONG\r\n')
})

// A hop reading with the WebSocket library's default 32 KiB message limit drops
// the whole connection on a larger frame, and a bulk insert writes large
// buffers. This is the assertion that keeps a big write from killing a query.
test('a large write is split into frames no larger than the ceiling', async () => {
  const pending = client().machines.tcp('m-1', 5432)
  const ws = FakeWebSocket.last!
  ws.open()
  const duplex = await pending

  const big = Buffer.alloc(200 * 1024, 0x41)
  await new Promise<void>((resolve, reject) => {
    duplex.write(big, (err) => (err ? reject(err) : resolve()))
  })

  assert.ok(ws.sent.length > 1, 'a 200 KiB write went out as one frame')
  for (const frame of ws.sent) {
    assert.ok(frame.length <= 32 * 1024, `a frame of ${frame.length} bytes exceeds the ceiling`)
  }
  const total = ws.sent.reduce((n, frame) => n + frame.length, 0)
  assert.equal(total, big.length, 'the split lost or duplicated bytes')
})

// A driver reading a reset reports a network error where the truth is that the
// query finished, so a close has to end the stream rather than destroy it.
test('a closed tunnel ends the stream rather than erroring', async () => {
  const pending = client().machines.tcp('m-1', 3306)
  const ws = FakeWebSocket.last!
  ws.open()
  const duplex = await pending

  let errored: Error | undefined
  duplex.on('error', (err: Error) => {
    errored = err
  })
  const ended = new Promise<void>((resolve) => duplex.once('end', () => resolve()))
  duplex.resume()
  ws.serverClose()

  await ended
  assert.equal(errored, undefined, `the close surfaced as an error: ${errored?.message}`)
})

// A socket that never opens must reject, not hang. A caller awaiting a promise
// that will never settle is the one failure with nothing to report at all.
test('a tunnel that closes before it opens rejects', async () => {
  const pending = client().machines.tcp('m-1', 5432)
  FakeWebSocket.last!.serverClose()
  await assert.rejects(pending, /closed before it opened/)
})
