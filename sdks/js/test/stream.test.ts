/**
 * The exec-stream decoder, driven frame by frame.
 */

import test from 'node:test'
import assert from 'node:assert/strict'

import { PilotsClient } from '../src/client.ts'
import { PilotsError } from '../src/errors.ts'
import type { ExecStream, ExecStreamOptions, WebSocketCtor } from '../src/stream.ts'
import { collect, FakeWebSocket } from './fakes/websocket.ts'

const KEY = 'pilot_deadbeef'

/** Opens a stream on a fake socket and hands back both. */
function open(
  argv: string[],
  opts: ExecStreamOptions = {},
): { stream: ExecStream; ws: FakeWebSocket } {
  const client = new PilotsClient(KEY, {
    baseURL: 'https://host-1.example.com',
    WebSocket: FakeWebSocket as unknown as WebSocketCtor,
  })
  const stream = client.machines.execStream('m-1', argv, opts)
  const ws = FakeWebSocket.last!
  ws.open()
  return { stream, ws }
}

test('the dial URL carries sprites query names and the key as a subprotocol', () => {
  const { ws } = open(['bash', '-c', 'echo hi'], {
    cwd: '/home/sprite/app',
    env: { A: '1' },
  })
  const url = new URL(ws.url)

  assert.equal(url.protocol, 'wss:')
  assert.equal(url.pathname, '/v1/machines/m-1/exec/stream')
  assert.deepEqual(url.searchParams.getAll('cmd'), ['bash', '-c', 'echo hi'])
  assert.equal(url.searchParams.get('path'), 'bash')
  assert.equal(url.searchParams.get('dir'), '/home/sprite/app')
  assert.deepEqual(url.searchParams.getAll('env'), ['A=1'])
  assert.equal(url.searchParams.get('stdin'), 'false')
  assert.deepEqual(ws.protocols, [`authorization.bearer.${KEY}`])
  assert.equal(ws.binaryType, 'arraybuffer')
})

test('frames 1, 2 and 3 become stdout, stderr and an exit code', async () => {
  const { stream, ws } = open(['bash', '-c', 'true'])
  const exits: number[] = []
  stream.on('exit', (code: number) => exits.push(code))

  ws.frame(1, 'a')
  ws.frame(2, 'b')
  // Neither of these may reach the consumer.
  ws.empty()
  ws.frame(9, 'from a newer agent')
  ws.frame(3, new Uint8Array([7]))

  assert.equal(await stream.wait(), 7)
  assert.equal(stream.exitCode, 7)
  assert.deepEqual(exits, [7])
  // Ended, so a `for await` over either stream terminates.
  assert.equal(await collect(stream.stdout), 'a')
  assert.equal(await collect(stream.stderr), 'b')
  assert.equal(ws.closedWith, 1000)
})

test('output is complete when exit fires, and the exit path ends the streams', async () => {
  const { stream, ws } = open(['bash', '-c', 'true'])

  let bufferedAtExit = -1
  stream.on('exit', () => {
    bufferedAtExit = stream.stdout.readableLength
  })

  ws.frame(1, 'hello')
  ws.frame(3, new Uint8Array([0]))

  // The guest agent drains both pumps before writing the exit frame and
  // websocket frames are ordered, so nothing can still be in flight here.
  assert.equal(bufferedAtExit, 5)
  assert.equal(await stream.wait(), 0)

  // No 'close' event is ever dispatched by this fake, so a terminating read
  // proves the EOF came from the exit frame rather than from a dropped socket.
  assert.equal(await collect(stream.stdout), 'hello')
  assert.equal(await collect(stream.stderr), '')
})

test('a text exit frame after the binary one changes nothing', async () => {
  const { stream, ws } = open(['bash', '-c', 'true'])
  const exits: number[] = []
  stream.on('exit', (code: number) => exits.push(code))

  ws.frame(3, new Uint8Array([7]))
  ws.text(JSON.stringify({ type: 'exit', exit_code: 9 }))

  assert.equal(await stream.wait(), 7)
  assert.deepEqual(exits, [7])
})

test('a text exit frame alone is enough', async () => {
  const { stream, ws } = open(['bash', '-c', 'true'])
  ws.text(JSON.stringify({ type: 'exit', exit_code: 7 }))
  assert.equal(await stream.wait(), 7)
  assert.equal(stream.exitCode, 7)
})

test('a close with no exit frame is an error, never a silent zero', async () => {
  const { stream, ws } = open(['bash', '-c', 'true'])
  const errors: Error[] = []
  stream.on('error', (err: Error) => errors.push(err))

  ws.frame(1, 'partial')
  ws.serverClose()

  const err = await stream.wait().then(
    () => null,
    (e: unknown) => e,
  )
  assert.ok(err instanceof PilotsError, `got ${String(err)}`)
  assert.match(err.message, /closed before exit/)
  assert.equal(stream.exitCode, null)
  assert.equal(errors.length, 1)
  // Ended anyway, so a consumer's `for await` does not hang on a dropped socket.
  assert.equal(await collect(stream.stdout), 'partial')
})

test('a socket that never connects rejects wait()', async () => {
  const client = new PilotsClient(KEY, {
    baseURL: 'https://host-1.example.com',
    WebSocket: FakeWebSocket as unknown as WebSocketCtor,
  })
  const stream = client.machines.execStream('m-1', ['bash'])
  FakeWebSocket.last!.fail()
  const err = await stream.wait().then(
    () => null,
    (e: unknown) => e,
  )
  assert.ok(err instanceof PilotsError)
  assert.match(err.message, /could not connect/)
})

test('with stdin on, writeStdin sends frame 0 and endStdin sends frame 4', async () => {
  const { stream, ws } = open(['bash'], { stdin: true })
  assert.equal(new URL(ws.url).searchParams.get('stdin'), 'true')

  stream.writeStdin('x')
  stream.endStdin()

  assert.deepEqual(
    ws.sent.map((f) => Array.from(f)),
    [[0, 120], [4]],
  )
  ws.frame(3, new Uint8Array([0]))
  assert.equal(await stream.wait(), 0)
})

test('with stdin off, both stdin calls throw', () => {
  const { stream } = open(['bash'])
  assert.throws(() => stream.writeStdin('x'), PilotsError)
  assert.throws(() => stream.endStdin(), PilotsError)
})

test('kill closes the socket with 1000', () => {
  const { stream, ws } = open(['bash'])
  stream.kill()
  assert.equal(ws.closedWith, 1000)
})
