/**
 * `pilot console`.
 *
 * Everything this command promises happens on the LOCAL end of the stream:
 * raw mode goes on and comes off again, a window change becomes a resize, a
 * keystroke becomes a frame. None of that is observable from a spawned
 * process, whose stdin is a pipe and never a terminal, and Node has no
 * pseudo-terminal without a native module this package will not add. So the
 * terminal is an injected seam and these cases drive the real command object
 * through it, the way the `open` cases drive its spawn seam.
 *
 * The two REFUSALS are spawned, because what they promise is the process's
 * own behaviour: exit 1, nothing on stdout, and no socket opened at all.
 *
 * The socket is real throughout. A resize is a text frame and a keystroke is a
 * binary one, so what is under test is what went over the wire rather than a
 * method that was called.
 */

import { strict as assert } from 'node:assert'
import { execFile } from 'node:child_process'
import { EventEmitter } from 'node:events'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { PassThrough } from 'node:stream'
import { after, test } from 'node:test'
import { promisify } from 'node:util'

import { createConsoleCommand } from '../src/commands/console.ts'
import type { Terminal } from '../src/commands/machines.ts'
import { saveCredentials } from '../src/config.ts'
import { fakeMachine, startFakeAPI } from './helpers/fake-api.ts'
import { startWSServer, type WSConnection, type WSServer } from './helpers/ws-server.ts'

const exec = promisify(execFile)
const BIN = join(import.meta.dirname, '..', 'bin', 'pilot.js')
const roots: string[] = []

after(() => {
  for (const dir of roots) rmSync(dir, { recursive: true, force: true })
})

function loggedIn(apiUrl: string): NodeJS.ProcessEnv {
  const dir = mkdtempSync(join(tmpdir(), 'pilot-console-'))
  roots.push(dir)
  const env = { XDG_CONFIG_HOME: dir }
  saveCredentials({ api_key: 'pilot_test_key', org_id: 'org_1', api_url: apiUrl }, env)
  return env
}

/** A fleet whose one machine answers an exec stream from a script. */
async function fleet(onConnect: (conn: WSConnection) => void = () => {}): Promise<WSServer> {
  const machine = fakeMachine({ id: 'm_1', name: 'box' })
  return await startWSServer(onConnect, (_req, res) => {
    res.writeHead(200, { 'content-type': 'application/json' })
    res.end(JSON.stringify(machine))
  })
}

interface Fake {
  term: Terminal
  /** Every `setRawMode` argument, in order. The whole assertion in three tests. */
  raw: boolean[]
  /**
   * Raw-mode changes AND exits, interleaved.
   *
   * The signal case needs the order and not just the set: a restore that runs
   * only in the command's `finally` looks identical in `raw`, and is wrong,
   * because the real `process.exit` never comes back and that `finally` never
   * runs.
   */
  events: string[]
  stdin: PassThrough
  stdout: PassThrough & { columns?: number; rows?: number }
  signals: EventEmitter
  printed: () => string
}

function fakeTerminal(columns = 100, rows = 30): Fake {
  const raw: boolean[] = []
  const events: string[] = []
  const signals = new EventEmitter()
  const stdin = new PassThrough()
  const stdout = Object.assign(new PassThrough(), { isTTY: true, columns, rows })
  const chunks: Buffer[] = []
  stdout.on('data', (chunk: Buffer) => chunks.push(chunk))
  const term: Terminal = {
    stdin: Object.assign(stdin, {
      isTTY: true,
      setRawMode: (on: boolean) => {
        raw.push(on)
        events.push(`raw ${on}`)
      },
    }),
    stdout,
    signals,
    // Never the real one: the signal path exits, and a test that took that
    // path for real would report as the whole file vanishing.
    exit: (code: number) => {
      events.push(`exit ${code}`)
    },
  }
  return { term, raw, events, stdin, stdout, signals, printed: () => Buffer.concat(chunks).toString() }
}

/**
 * Runs the command object against a fake terminal.
 *
 * `process.exitCode` is the command's channel for the remote status and is
 * global, so it is read and put back: a console that exited 7 would otherwise
 * be the exit code of the test run itself.
 */
async function runConsole(
  env: NodeJS.ProcessEnv,
  args: string[],
  term: Terminal,
): Promise<{ code: number | undefined; err: unknown }> {
  const previous = { ...process.env }
  const previousExit = process.exitCode
  Object.assign(process.env, env)
  try {
    await createConsoleCommand(term).parseAsync(['node', 'console', ...args])
    return { code: process.exitCode as number | undefined, err: null }
  } catch (err) {
    return { code: undefined, err }
  } finally {
    process.exitCode = previousExit
    for (const key of Object.keys(process.env)) delete process.env[key]
    Object.assign(process.env, previous)
  }
}

async function pilot(env: NodeJS.ProcessEnv, args: string[]) {
  try {
    const { stdout, stderr } = await exec(process.execPath, [BIN, ...args], {
      env: { ...env, PATH: process.env.PATH },
    })
    return { stdout, stderr, code: 0 }
  } catch (err) {
    const e = err as { stdout?: string; stderr?: string; code?: number }
    return { stdout: e.stdout ?? '', stderr: e.stderr ?? '', code: e.code ?? 1 }
  }
}

async function waitFor(what: string, ready: () => boolean, ms = 5000): Promise<void> {
  const deadline = Date.now() + ms
  while (!ready()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`)
    await new Promise((resolve) => setTimeout(resolve, 10))
  }
}

test('the handshake asks for a terminal, and carries the window this one has', async () => {
  const ws = await fleet((conn) => conn.frame(3, new Uint8Array([0])))
  const env = loggedIn(ws.url)
  try {
    const res = await runConsole(env, ['box'], fakeTerminal(100, 30).term)
    assert.equal(res.err, null)
    const conn = ws.connections[0]!
    // A login shell by default, so the guest's own profile has run before the
    // first prompt.
    assert.deepEqual(conn.query.getAll('cmd'), ['/bin/sh', '-l'])
    assert.equal(conn.query.get('tty'), 'true')
    // hostd answers tty=true with stdin=false with a 400, so the pair is never
    // left to contradict itself.
    assert.equal(conn.query.get('stdin'), 'true')
    // The window is sent at the handshake rather than after it: a shell that
    // draws its prompt before the first resize would draw it 80 columns wide.
    assert.equal(conn.query.get('cols'), '100')
    assert.equal(conn.query.get('rows'), '30')

    // Anything after -- replaces the shell.
    await runConsole(env, ['box', '--', 'bash', '-i'], fakeTerminal().term)
    assert.deepEqual(ws.connections[1]!.query.getAll('cmd'), ['bash', '-i'])
  } finally {
    await ws.close()
  }
})

test('a terminal that does not know its size sends the default, never a zero', async () => {
  // Found on a real pty: a window that was never set reports 0, not undefined,
  // and hostd refuses a size outside 1..65535 by CLOSING the socket. A zero on
  // the wire is a console that cannot connect at all.
  const ws = await fleet((conn) => conn.frame(3, new Uint8Array([0])))
  try {
    await runConsole(loggedIn(ws.url), ['box'], fakeTerminal(0, 0).term)
    assert.equal(ws.connections[0]!.query.get('cols'), '80')
    assert.equal(ws.connections[0]!.query.get('rows'), '24')
  } finally {
    await ws.close()
  }

  // And a window change to a size the protocol cannot carry says the same
  // thing rather than passing it on.
  const live = await fleet()
  const fake = fakeTerminal(100, 30)
  try {
    const pending = runConsole(loggedIn(live.url), ['box'], fake.term)
    await waitFor('the socket', () => live.connections.length > 0)
    const conn = live.connections[0]!
    fake.stdout.columns = 0
    fake.stdout.rows = 0
    fake.signals.emit('SIGWINCH')
    await waitFor('the resize', () => conn.text.length > 0)
    assert.deepEqual(JSON.parse(conn.text[0]!), { type: 'resize', cols: 80, rows: 24 })
    conn.frame(3, new Uint8Array([0]))
    await pending
  } finally {
    await live.close()
  }
})

test('raw mode goes on, comes off, and the exit code is the shell\'s', async () => {
  const ws = await fleet((conn) => {
    conn.frame(1, 'hello from the guest\n')
    conn.frame(3, new Uint8Array([7]))
  })
  const fake = fakeTerminal()
  try {
    const res = await runConsole(loggedIn(ws.url), ['box'], fake.term)
    assert.equal(res.err, null)
    // Exactly once each, in that order. A terminal left in raw mode has no
    // echo and no line editing, which is the worst thing this command could
    // leave behind.
    assert.deepEqual(fake.raw, [true, false])
    // `exit 7` in the shell exits 7 here, the same contract machines exec has.
    assert.equal(res.code, 7)
    assert.equal(fake.printed(), 'hello from the guest\n')
  } finally {
    await ws.close()
  }
})

test('the terminal is untouched until the socket is up', async () => {
  // Raw mode clears ISIG, so the moment it goes on there is no ctrl-c. Doing it
  // before the handshake hands the user an echo-less terminal with no way out
  // for however long the connect takes, which against a suspended machine is a
  // wake.
  const seen: number[] = []
  const fake = fakeTerminal()
  const ws = await fleet((conn) => {
    // The server has written its 101 and the client cannot have read it yet, so
    // this is the last moment that is definitely still "connecting".
    seen.push(fake.raw.length)
    conn.frame(3, new Uint8Array([0]))
  })
  try {
    await runConsole(loggedIn(ws.url), ['box'], fake.term)
    assert.deepEqual(seen, [0], 'the terminal went raw before the socket existed')
    assert.deepEqual(fake.raw, [true, false])
  } finally {
    await ws.close()
  }

  // And a stream that never connects never touches the terminal at all: this
  // fleet answers the machine lookup and then refuses the upgrade.
  const api = await startFakeAPI()
  api.machines.push(fakeMachine({ id: 'm_1', name: 'box' }))
  const never = fakeTerminal()
  try {
    const res = await runConsole(loggedIn(api.url), ['box'], never.term)
    assert.match(String((res.err as Error)?.message), /could not connect/)
    assert.deepEqual(never.raw, [], 'a session that never opened still changed the terminal')
  } finally {
    await api.close()
  }
})

test('a terminal that goes away ends the session instead of reporting a stream failure', async () => {
  // The controlling terminal vanishing without a signal reaching us first: an
  // ssh session dropped, an emulator closed. We close the socket ourselves, so
  // no exit frame arrives, and the stream is right to call that a failure when
  // it happens TO us. Here it did not.
  const ws = await fleet()
  const fake = fakeTerminal()
  try {
    const pending = runConsole(loggedIn(ws.url), ['box'], fake.term)
    await waitFor('the terminal to go raw', () => fake.raw.length > 0)
    fake.stdin.end()

    const res = await pending
    assert.equal(res.err, null, 'a deliberate hangup was reported as a stream failure')
    assert.equal(res.code, 0)
    // The shell goes with it: the guest cancels its context when the socket
    // closes, so a hangup leaves nothing running.
    await ws.connections[0]!.closed
    assert.deepEqual(fake.raw, [true, false])
  } finally {
    await ws.close()
  }
})

test('raw mode comes off when the stream fails, not only when it succeeds', async () => {
  // A socket that drops with no exit frame: nobody knows what the shell did.
  const ws = await fleet((conn) => {
    conn.frame(1, 'partial\n')
    conn.close()
  })
  const fake = fakeTerminal()
  try {
    const res = await runConsole(loggedIn(ws.url), ['box'], fake.term)
    assert.match(String((res.err as Error)?.message), /closed before exit/)
    // The error path is the one a `finally` exists for. Without it the user is
    // left staring at a dead terminal AND an error they cannot type past.
    assert.deepEqual(fake.raw, [true, false])
  } finally {
    await ws.close()
  }
})

test('a signal restores the terminal, kills the shell, and exits 128 plus the signal', async () => {
  // A guest that says nothing: the shell is sitting at its prompt, which is
  // exactly the state a user interrupts from.
  const ws = await fleet()
  const fake = fakeTerminal()
  try {
    const pending = runConsole(loggedIn(ws.url), ['box'], fake.term)
    await waitFor('the terminal to go raw', () => fake.raw.length > 0)
    await waitFor('the socket', () => ws.connections.length > 0)
    fake.signals.emit('SIGINT')

    const res = await pending
    // In that order, and the order is the assertion. The real `process.exit`
    // never returns, so a restore that happens only in the command's teardown
    // never happens at all on this path: the terminal the user gets back is
    // one with no echo and no line editing.
    assert.deepEqual(fake.events, ['raw true', 'raw false', 'exit 130'])
    // Closing the socket is what ends the remote shell: the guest cancels its
    // context on that path, so an interrupted console leaves nothing running.
    await ws.connections[0]!.closed
    assert.equal(res.code, undefined)
  } finally {
    await ws.close()
  }
})

test('a window change is forwarded as one resize with the new numbers', async () => {
  const ws = await fleet()
  const fake = fakeTerminal(100, 30)
  try {
    const pending = runConsole(loggedIn(ws.url), ['box'], fake.term)
    await waitFor('the socket', () => ws.connections.length > 0)
    const conn = ws.connections[0]!

    fake.stdout.columns = 120
    fake.stdout.rows = 40
    fake.signals.emit('SIGWINCH')
    await waitFor('the resize', () => conn.text.length > 0)

    // Cols then rows, which is the order the message uses and the opposite of
    // the order `stty size` prints them in.
    assert.deepEqual(JSON.parse(conn.text[0]!), { type: 'resize', cols: 120, rows: 40 })
    assert.equal(conn.text.length, 1, 'one window change, one resize')

    conn.frame(3, new Uint8Array([0]))
    assert.equal((await pending).code, 0)
  } finally {
    await ws.close()
  }
})

test('a keystroke reaches the guest as a stdin frame, byte for byte', async () => {
  const ws = await fleet()
  const fake = fakeTerminal()
  try {
    const pending = runConsole(loggedIn(ws.url), ['box'], fake.term)
    await waitFor('the socket', () => ws.connections.length > 0)
    const conn = ws.connections[0]!

    // Raw mode means a keystroke, not a line: ctrl-c is 0x03 and belongs to
    // the remote shell rather than to this process.
    fake.stdin.write(Buffer.from([0x03]))
    await waitFor('the keystroke', () => conn.binary.length > 0)
    assert.deepEqual([...conn.binary[0]!], [0, 0x03])

    conn.frame(3, new Uint8Array([0]))
    await pending
  } finally {
    await ws.close()
  }
})

test('off a terminal it names machines exec and never opens a socket', async () => {
  // Spawned, so stdin really is a pipe. This is the case an agent and a CI job
  // hit, and half-working there means a shell that draws nothing.
  const ws = await fleet()
  try {
    const res = await pilot(loggedIn(ws.url), ['console', 'box'])
    assert.equal(res.code, 1)
    assert.match(res.stderr, /console needs a terminal/)
    assert.match(res.stderr, /pilot machines exec/)
    assert.equal(ws.connections.length, 0, 'a socket was opened for a session nobody could type into')
  } finally {
    await ws.close()
  }
})

test('--json is refused with nothing on stdout and no socket opened', async () => {
  const ws = await fleet()
  try {
    const res = await pilot(loggedIn(ws.url), ['--json', 'console', 'box'])
    assert.equal(res.code, 1)
    // The byte-exact contract: under --json stdout carries the API's own
    // response and nothing else, so a command with no response puts nothing
    // there at all rather than an empty line a parser would choke on.
    assert.equal(res.stdout, '')
    assert.deepEqual(Object.keys(JSON.parse(res.stderr)), ['error'])
    assert.match(res.stderr, /no --json form/)
    assert.equal(ws.connections.length, 0)
  } finally {
    await ws.close()
  }
})
