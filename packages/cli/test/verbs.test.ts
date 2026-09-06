/**
 * `open`, `logs`, the confirmation prompt and the global `-y`.
 *
 * `open` is tested through its injected spawn seam rather than a spawned
 * binary, because the assertion that matters most is that NOTHING was
 * spawned: a browser opening during a test run is the failure.
 */

import { strict as assert } from 'node:assert'
import { execFile } from 'node:child_process'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import type { ServerResponse } from 'node:http'
import { PassThrough } from 'node:stream'
import { after, test } from 'node:test'
import { promisify } from 'node:util'

import { createOpenCommand, openerFor } from '../src/commands/open.ts'
import { saveCredentials } from '../src/config.ts'
import { CliError, setJSONMode } from '../src/output.ts'
import { confirm, shouldAsk } from '../src/prompt.ts'
import { fakeMachine, fakeService, startFakeAPI } from './helpers/fake-api.ts'

const exec = promisify(execFile)
const BIN = join(import.meta.dirname, '..', 'bin', 'pilot.js')
const roots: string[] = []
after(() => {
  for (const dir of roots) rmSync(dir, { recursive: true, force: true })
})

function loggedIn(apiUrl: string): NodeJS.ProcessEnv {
  const dir = mkdtempSync(join(tmpdir(), 'pilot-verbs-'))
  roots.push(dir)
  const env = { XDG_CONFIG_HOME: dir }
  saveCredentials({ api_key: 'pilot_test_key', org_id: 'org_1', api_url: apiUrl }, env)
  return env
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

/** Runs one command object directly, so the spawn seam can be observed. */
async function runOpen(
  env: NodeJS.ProcessEnv,
  args: string[],
  spawned: string[][],
  emitted: string[] = [],
): Promise<void> {
  const previous = { ...process.env }
  Object.assign(process.env, env)
  try {
    const cmd = createOpenCommand(
      (file, argv) => spawned.push([file, ...argv]),
      (line) => emitted.push(line),
    )
    await cmd.parseAsync(['node', 'open', ...args])
  } finally {
    for (const key of Object.keys(process.env)) delete process.env[key]
    Object.assign(process.env, previous)
  }
}

// The opener is spawned with an argv array, never through a shell. `sprite`
// re-executes through $SHELL -c and hangs on every non-interactive call; this
// is the same mistake, one command over.
test('the platform opener is a binary and an argv array', () => {
  assert.deepEqual(openerFor('https://x', 'darwin'), { file: 'open', args: ['https://x'] })
  assert.deepEqual(openerFor('https://x', 'linux'), { file: 'xdg-open', args: ['https://x'] })
  // The empty string is start's title argument; without it the URL is read as
  // a window title and nothing opens.
  assert.deepEqual(openerFor('https://x', 'win32'), { file: 'cmd', args: ['/c', 'start', '', 'https://x'] })
})

test('open --print prints the URL and spawns nothing', async () => {
  const api = await startFakeAPI()
  api.services.push(fakeService({ id: 'svc_1', name: 'web', url: 'https://web.pilotrun.app' }))
  const env = loggedIn(api.url)
  try {
    const spawned: string[][] = []
    const emitted: string[] = []
    await runOpen(env, ['web', '--print'], spawned, emitted)
    assert.equal(spawned.length, 0, 'a browser was spawned by --print')
    assert.deepEqual(emitted, ['https://web.pilotrun.app'])
    // Also with no flag at all: stdout is a pipe here, so there is nothing to
    // open and the URL is the result.
    await runOpen(env, ['web'], spawned, emitted)
    assert.equal(spawned.length, 0, 'a browser was spawned with stdout piped')

    const printed = await pilot(env, ['open', 'web', '--print'])
    assert.equal(printed.code, 0, printed.stderr)
    assert.equal(printed.stdout, 'https://web.pilotrun.app\n')
  } finally {
    await api.close()
  }
})

// Services resolve first, so a promoted machine and its service share a name
// and the durable half wins. A name that is only a machine still resolves.
test('open falls back to a machine when no service has the name', async () => {
  const api = await startFakeAPI()
  api.machines.push(fakeMachine({ id: 'm_1', name: 'sandbox', url: 'https://sandbox.pilotrun.app' }))
  const env = loggedIn(api.url)
  try {
    const res = await pilot(env, ['--json', 'open', 'sandbox'])
    assert.equal(res.code, 0, res.stderr)
    assert.deepEqual(JSON.parse(res.stdout), { url: 'https://sandbox.pilotrun.app', opened: false })
  } finally {
    await api.close()
  }
})

test('open on a target with no URL exits 1 with the domain hint', async () => {
  const api = await startFakeAPI()
  api.services.push(fakeService({ id: 'svc_1', name: 'web', url: '' }))
  const env = loggedIn(api.url)
  try {
    const res = await pilot(env, ['open', 'web'])
    assert.equal(res.code, 1)
    assert.match(res.stderr, /web has no URL/)
    assert.match(res.stderr, /x-pilots\.domain/)
  } finally {
    await api.close()
  }
})

test('logs fans in every replica with a name prefix', async () => {
  const api = await startFakeAPI()
  api.services.push(fakeService({ id: 'svc_1', name: 'web' }))
  api.machines.push(
    fakeMachine({ id: 'm_1', name: 'web-1', service_id: 'svc_1' }),
    fakeMachine({ id: 'm_2', name: 'web-2', service_id: 'svc_1' }),
    fakeMachine({ id: 'm_3', name: 'other' }),
  )
  const plain = (body: string) => (_req: { path: string; body: string }, res: ServerResponse) => {
    res.writeHead(200, { 'content-type': 'text/plain' })
    res.end(body)
  }
  api.routes.set('GET /v1/machines/m_1/logs', plain('one\ntwo\n'))
  api.routes.set('GET /v1/machines/m_2/logs', plain('three\n'))
  const env = loggedIn(api.url)
  try {
    const human = await pilot(env, ['logs', 'web'])
    assert.equal(human.code, 0, human.stderr)
    assert.match(human.stdout, /^web-1 \| one$/m)
    assert.match(human.stdout, /^web-2 \| three$/m)
    // A machine in another service is not a replica of this one.
    assert.doesNotMatch(human.stdout, /other \|/)

    const asJSON = await pilot(env, ['--json', 'logs', 'web'])
    const parsed = JSON.parse(asJSON.stdout) as { service: string; machines: { name: string }[] }
    assert.equal(parsed.service, 'web')
    assert.deepEqual(parsed.machines.map((m) => m.name), ['web-1', 'web-2'])
  } finally {
    await api.close()
  }
})

test('logs on a service with no replicas exits 1 with the --replicas hint', async () => {
  const api = await startFakeAPI()
  api.services.push(fakeService({ id: 'svc_1', name: 'web' }))
  const env = loggedIn(api.url)
  try {
    const res = await pilot(env, ['logs', 'web'])
    assert.equal(res.code, 1)
    assert.match(res.stderr, /web has no replicas/)
    assert.match(res.stderr, /--replicas 1/)
  } finally {
    await api.close()
  }
})

// A script and an agent are never blocked on a question. Every one of these is
// a way of saying nobody is there to answer.
test('shouldAsk is false whenever nobody is there to answer', () => {
  assert.equal(shouldAsk({ stdinTTY: true, stderrTTY: true }), true)
  assert.equal(shouldAsk({ yes: true, stdinTTY: true, stderrTTY: true }), false)
  assert.equal(shouldAsk({ json: true, stdinTTY: true, stderrTTY: true }), false)
  assert.equal(shouldAsk({ stdinTTY: false, stderrTTY: true }), false)
  assert.equal(shouldAsk({ stdinTTY: true, stderrTTY: false }), false)
  assert.equal(shouldAsk({}), false)
})

// Anything but yes is a no, end of input included: the safe answer to a
// destructive question nobody answered is not to do it.
test('confirm reads y as yes and everything else as no', async () => {
  const ask = async (typed: string | null): Promise<boolean> => {
    const input = new PassThrough()
    const output = new PassThrough()
    output.resume()
    const promise = confirm('destroy web?', { input, output })
    if (typed === null) input.end()
    else input.end(typed)
    return promise
  }
  assert.equal(await ask('y\n'), true)
  assert.equal(await ask('yes\n'), true)
  assert.equal(await ask('n\n'), false)
  assert.equal(await ask('\n'), false)
  assert.equal(await ask(null), false, 'EOF is a no')
})

test('destroy and domains rm never ask off a TTY, and -y is accepted everywhere', async () => {
  const api = await startFakeAPI()
  api.machines.push(fakeMachine({ id: 'm_1', name: 'web' }))
  const env = loggedIn(api.url)
  try {
    // The child's stdin is a pipe, so the confirmation must not fire; a
    // prompt here would hang until the test timed out.
    const destroyed = await pilot(env, ['machines', 'destroy', 'web'])
    assert.equal(destroyed.code, 0, destroyed.stderr)
    assert.equal(api.all('DELETE', '/v1/machines/m_1').length, 1)

    const removed = await pilot(env, ['-y', 'domains', 'rm', 'x.example.com'])
    assert.equal(removed.code, 0, removed.stderr)

    const help = await pilot(env, ['--help'])
    assert.match(help.stdout, /-y, --yes/)
  } finally {
    await api.close()
  }
})

test('CliError from a cancelled confirmation is a plain refusal', () => {
  setJSONMode(false)
  const err = new CliError('cancelled')
  assert.equal(err.message, 'cancelled')
  assert.equal(err.hint, undefined)
})
