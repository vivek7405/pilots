/**
 * `pilot machines exec`.
 *
 * Three promises, all of them made on the wire rather than in the code: frame 1
 * reaches stdout and frame 2 reaches stderr as separate channels, the process
 * exits with the guest's status, and the handshake says `stdin=false` unless
 * the caller asked otherwise.
 *
 * The last one is the reason this test drives a real socket. A guest process
 * holding an open stdin it never reads hangs, and the reference workload -- an
 * agent session -- is exactly such a process, so the default has to be off and
 * has to be sent EXPLICITLY rather than left for the server to infer.
 */

import { strict as assert } from 'node:assert'
import { execFile } from 'node:child_process'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { after, test } from 'node:test'
import { promisify } from 'node:util'

import { saveCredentials } from '../src/config.ts'
import { fakeMachine } from './helpers/fake-api.ts'
import { startWSServer, type WSConnection } from './helpers/ws-server.ts'

const exec = promisify(execFile)
const BIN = join(import.meta.dirname, '..', 'bin', 'pilot.js')
const roots: string[] = []

after(() => {
  for (const dir of roots) rmSync(dir, { recursive: true, force: true })
})

function loggedIn(apiUrl: string): NodeJS.ProcessEnv {
  const dir = mkdtempSync(join(tmpdir(), 'pilot-exec-'))
  roots.push(dir)
  const env = { XDG_CONFIG_HOME: dir }
  saveCredentials({ api_key: 'pilot_test_key', org_id: 'org_1', api_url: apiUrl }, env)
  return { ...env, PATH: process.env.PATH }
}

/** A fleet whose one machine answers an exec stream from a script. */
async function fleet(onConnect: (conn: WSConnection) => void) {
  const machine = fakeMachine({ id: 'm_1', name: 'box' })
  return await startWSServer(onConnect, (_req, res) => {
    res.writeHead(200, { 'content-type': 'application/json' })
    res.end(JSON.stringify(machine))
  })
}

async function pilot(env: NodeJS.ProcessEnv, args: string[]) {
  try {
    const { stdout, stderr } = await exec(process.execPath, [BIN, ...args], { env })
    return { stdout, stderr, code: 0 }
  } catch (err) {
    const e = err as { stdout?: string; stderr?: string; code?: number }
    return { stdout: e.stdout ?? '', stderr: e.stderr ?? '', code: e.code ?? 1 }
  }
}

test('stdout and stderr stay separate channels, and the exit code is the guest\'s', async () => {
  const ws = await fleet((conn) => {
    conn.frame(1, 'to stdout\n')
    conn.frame(2, 'to stderr\n')
    conn.frame(3, new Uint8Array([7]))
  })
  try {
    const res = await pilot(loggedIn(ws.url), ['machines', 'exec', 'box', '--', 'sh', '-c', 'exit 7'])
    // Merging the two would make `pilot machines exec ... > out.txt` collect a
    // guest's diagnostics as if they were its output.
    assert.equal(res.stdout, 'to stdout\n')
    assert.equal(res.stderr, 'to stderr\n')
    // The remote status, not the CLI's own. A shell script chaining execs
    // depends on this being the guest's answer.
    assert.equal(res.code, 7)
  } finally {
    await ws.close()
  }
})

test('the argv after -- reaches the guest, one query parameter per word', async () => {
  const ws = await fleet((conn) => conn.frame(3, new Uint8Array([0])))
  try {
    const res = await pilot(loggedIn(ws.url), [
      'machines', 'exec', 'box', '--cwd', '/app', '--env', 'A=1', '--env', 'B=two words',
      '--', 'sh', '-c', 'echo hello world',
    ])
    assert.equal(res.code, 0, res.stderr)
    const conn = ws.connections[0]!
    assert.deepEqual(conn.query.getAll('cmd'), ['sh', '-c', 'echo hello world'])
    assert.equal(conn.query.get('dir'), '/app')
    assert.deepEqual(conn.query.getAll('env'), ['A=1', 'B=two words'])
  } finally {
    await ws.close()
  }
})

test('stdin is false unless --stdin, and always stated', async () => {
  const ws = await fleet((conn) => conn.frame(3, new Uint8Array([0])))
  try {
    await pilot(loggedIn(ws.url), ['machines', 'exec', 'box', '--', 'true'])
    // Present, not merely falsy-by-absence. The default is the thing most
    // likely to be wrong by omission, so it is always on the wire.
    assert.equal(ws.connections[0]!.query.get('stdin'), 'false')

    await pilot(loggedIn(ws.url), ['machines', 'exec', 'box', '--stdin', '--', 'cat'])
    assert.equal(ws.connections[1]!.query.get('stdin'), 'true')
  } finally {
    await ws.close()
  }
})

test('the API key rides the subprotocol, never a query parameter', async () => {
  const ws = await fleet((conn) => conn.frame(3, new Uint8Array([0])))
  try {
    await pilot(loggedIn(ws.url), ['machines', 'exec', 'box', '--', 'true'])
    const conn = ws.connections[0]!
    assert.ok(conn.protocols.includes('authorization.bearer.pilot_test_key'))
    // A key in the query string lands in every access log between here and the
    // host. Browsers cannot set handshake headers, so the subprotocol is where
    // it goes.
    assert.equal(conn.url.includes('pilot_test_key'), false)
  } finally {
    await ws.close()
  }
})

test('a stream that closes with no exit frame is an error, never a silent 0', async () => {
  const ws = await fleet((conn) => {
    conn.frame(1, 'partial output\n')
    conn.close()
  })
  try {
    const res = await pilot(loggedIn(ws.url), ['machines', 'exec', 'box', '--', 'true'])
    // Nobody knows what the command did. Reporting success here is how a
    // failed migration looks like a successful one.
    assert.equal(res.code, 1)
    assert.match(res.stderr, /closed before exit/)
  } finally {
    await ws.close()
  }
})

test('exec with nothing to run says so instead of opening a socket', async () => {
  const ws = await fleet(() => {})
  try {
    const res = await pilot(loggedIn(ws.url), ['machines', 'exec', 'box'])
    assert.equal(res.code, 1)
    assert.match(res.stderr, /nothing to run/)
    assert.equal(ws.connections.length, 0)
  } finally {
    await ws.close()
  }
})
