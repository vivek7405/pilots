/**
 * The command surface, driven as a user drives it.
 *
 * Every case spawns `bin/pilot.js` against the fake hostd, so what is under
 * test is the whole path: argument parsing, the credentials file, the SDK call
 * and the rendering. A test that imported the action function directly would
 * pass with the command unwired.
 */

import { strict as assert } from 'node:assert'
import { execFile } from 'node:child_process'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { after, test } from 'node:test'
import { promisify } from 'node:util'

import { saveCredentials } from '../src/config.ts'
import { fakeMachine, startFakeAPI } from './helpers/fake-api.ts'
import { json, startServer } from './helpers/server.ts'

const exec = promisify(execFile)
const BIN = join(import.meta.dirname, '..', 'bin', 'pilot.js')
const roots: string[] = []

after(() => {
  for (const dir of roots) rmSync(dir, { recursive: true, force: true })
})

function loggedIn(apiUrl: string): NodeJS.ProcessEnv {
  const dir = mkdtempSync(join(tmpdir(), 'pilot-cmd-'))
  roots.push(dir)
  const env = { XDG_CONFIG_HOME: dir }
  saveCredentials({ api_key: 'pilot_test_key', org_id: 'org_1', api_url: apiUrl }, env)
  return env
}

interface RunResult {
  stdout: string
  stderr: string
  code: number
}

async function pilot(env: NodeJS.ProcessEnv, args: string[]): Promise<RunResult> {
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

test('machines create --json prints the response verbatim', async () => {
  const api = await startFakeAPI()
  const env = loggedIn(api.url)
  try {
    const res = await pilot(env, ['--json', 'machines', 'create', '--name', 'demo', '--app', 'shop', '--vcpus', '2'])
    assert.equal(res.code, 0, res.stderr)
    const machine = JSON.parse(res.stdout) as { name: string; url: string; app: string }
    assert.equal(machine.name, 'demo')
    assert.equal(machine.app, 'shop')
    // The exact invocation #34 H8 spawns: the body reaches hostd unmangled.
    const sent = JSON.parse(api.find('POST', '/v1/machines')!.body) as Record<string, unknown>
    assert.deepEqual(sent, { name: 'demo', app: 'shop', vcpus: 2 })
    assert.equal(api.find('POST', '/v1/machines')!.headers.authorization, 'Bearer pilot_test_key')
  } finally {
    await api.close()
  }
})

test('machines ls --app filters, and the human form is a table', async () => {
  const api = await startFakeAPI()
  api.machines.push(fakeMachine({ name: 'a', app: 'shop' }), fakeMachine({ name: 'b', app: 'blog' }))
  const env = loggedIn(api.url)
  try {
    const asJSON = await pilot(env, ['--json', 'machines', 'ls', '--app', 'shop'])
    assert.equal(asJSON.code, 0, asJSON.stderr)
    const list = JSON.parse(asJSON.stdout) as { name: string }[]
    assert.deepEqual(list.map((m) => m.name), ['a'])

    const human = await pilot(env, ['machines', 'ls'])
    assert.match(human.stdout, /^ID {2}/m)
    assert.match(human.stdout, /\ba\b/)
    assert.match(human.stdout, /\bb\b/)
  } finally {
    await api.close()
  }
})

test('a machine is addressable by name as well as id', async () => {
  const api = await startFakeAPI()
  api.machines.push(fakeMachine({ id: 'm_abc', name: 'web' }))
  const env = loggedIn(api.url)
  try {
    const res = await pilot(env, ['--json', 'machines', 'info', 'web'])
    assert.equal(res.code, 0, res.stderr)
    assert.equal((JSON.parse(res.stdout) as { id: string }).id, 'm_abc')
    // The id is tried first and the list is the fallback, so exactly one 404
    // precedes the list call.
    assert.ok(api.find('GET', '/v1/machines/web'), 'the id lookup happened first')
    assert.ok(api.all('GET', '/v1/machines').some((r) => r.path === '/v1/machines'))
  } finally {
    await api.close()
  }
})

test('a name that matches nothing names what was typed', async () => {
  const api = await startFakeAPI()
  const env = loggedIn(api.url)
  try {
    const res = await pilot(env, ['machines', 'info', 'ghost'])
    assert.equal(res.code, 1)
    assert.match(res.stderr, /no machine with id or name ghost/)
  } finally {
    await api.close()
  }
})

test('a 429 reaches --json byte for byte and the human form names quota, limit and used', async () => {
  const api = await startFakeAPI()
  const body = '{"error":"quota exceeded","quota":"machines","limit":20,"used":20}'
  api.routes.set('POST /v1/machines', (_req, res) => {
    res.writeHead(429, { 'content-type': 'application/json' })
    res.end(body)
  })
  const env = loggedIn(api.url)
  try {
    const asJSON = await pilot(env, ['--json', 'machines', 'create'])
    assert.equal(asJSON.code, 1)
    // Byte for byte: #34 H8 compares this against the SDK and MCP renderings.
    assert.equal(asJSON.stderr, body + '\n')
    assert.equal(asJSON.stdout, '')

    const human = await pilot(env, ['machines', 'create'])
    assert.equal(human.code, 1)
    assert.equal(human.stderr, 'error: quota exceeded: machines (limit 20, used 20)\n')
  } finally {
    await api.close()
  }
})

test('a build quota carries its host scope into the human form', async () => {
  const api = await startFakeAPI()
  api.routes.set('POST /v1/machines', (_req, res) => {
    res.writeHead(429, { 'content-type': 'application/json' })
    res.end('{"error":"quota exceeded","quota":"builds","limit":4,"used":4,"scope":"host"}')
  })
  const env = loggedIn(api.url)
  try {
    const res = await pilot(env, ['machines', 'create'])
    assert.equal(res.stderr, 'error: quota exceeded: builds (limit 4, used 4, scope host)\n')
  } finally {
    await api.close()
  }
})

test('status counts machines by state and lists hosts', async () => {
  const api = await startFakeAPI()
  api.machines.push(
    fakeMachine({ state: 'running' }),
    fakeMachine({ state: 'running' }),
    fakeMachine({ state: 'suspended' }),
  )
  const env = loggedIn(api.url)
  try {
    const res = await pilot(env, ['--json', 'status'])
    assert.equal(res.code, 0, res.stderr)
    const parsed = JSON.parse(res.stdout) as { machines_by_state: Record<string, number>; hosts: unknown[] }
    assert.equal(parsed.machines_by_state.running, 2)
    assert.equal(parsed.machines_by_state.suspended, 1)
    // Zero-valued states are present rather than absent: a script reading
    // `.stopped` gets a number instead of undefined.
    assert.equal(parsed.machines_by_state.stopped, 0)
    assert.equal(parsed.hosts.length, 1)
  } finally {
    await api.close()
  }
})

test('promote keeps the URL and prints the service', async () => {
  const api = await startFakeAPI()
  api.machines.push(fakeMachine({ id: 'm_1', name: 'sandbox' }))
  const env = loggedIn(api.url)
  try {
    const res = await pilot(env, ['--json', 'promote', 'sandbox', '--replicas', '3'])
    assert.equal(res.code, 0, res.stderr)
    assert.equal((JSON.parse(res.stdout) as { name: string }).name, 'sandbox')
    assert.deepEqual(JSON.parse(api.find('POST', '/v1/machines/m_1/promote')!.body), { replicas: 3 })
  } finally {
    await api.close()
  }
})

test('volumes create sends size_gib and the mount path', async () => {
  const api = await startFakeAPI()
  const env = loggedIn(api.url)
  try {
    const res = await pilot(env, ['--json', 'volumes', 'create', 'data', '--size-gib', '10'])
    assert.equal(res.code, 0, res.stderr)
    assert.deepEqual(JSON.parse(api.find('POST', '/v1/volumes')!.body), {
      name: 'data',
      size_gib: 10,
      mount_path: '/data',
    })
  } finally {
    await api.close()
  }
})

test('domains add names the CNAME target while verification is pending', async () => {
  const api = await startFakeAPI()
  api.services.push({ id: 'svc_1', name: 'web', replicas: 1, knobs: { auto_stop: 'off', auto_start: false, min_machines_running: 1, soft_limit: 20 }, autodeploy: false, created_at: 1 })
  const env = loggedIn(api.url)
  try {
    const res = await pilot(env, ['domains', 'add', 'shop.example.com', '--service', 'web'])
    assert.equal(res.code, 0, res.stderr)
    assert.match(res.stderr, /point its CNAME at fleet\.pilotrun\.app/)
  } finally {
    await api.close()
  }
})

test('an unreachable fleet names the route rather than the socket', async () => {
  const env = loggedIn('http://127.0.0.1:1')
  const res = await pilot(env, ['machines', 'ls'])
  assert.equal(res.code, 1)
  assert.match(res.stderr, /GET \/v1\/machines/)
})

test('no command validates the cached key: every one works with the dashboard down', async () => {
  // The rule from fly's tkdb outage: a CLI that checks its credential against
  // a service has made every command depend on that service. The dashboard
  // here is up ONLY so it can count, and the assertion is that it counted zero.
  const api = await startFakeAPI()
  api.machines.push(fakeMachine({ id: 'm_1', name: 'web' }))
  api.services.push({ id: 'svc_1', name: 'web', replicas: 1, knobs: { auto_stop: 'off', auto_start: false, min_machines_running: 1, soft_limit: 20 }, autodeploy: false, created_at: 1 })
  const dashboard = await startServer((_req, res) => json(res, 500, { error: 'the dashboard is down' }))
  const env = { ...loggedIn(api.url), PILOT_DASHBOARD_URL: dashboard.url }

  const surface: string[][] = [
    ['whoami'],
    ['status'],
    ['machines', 'ls'],
    ['machines', 'info', 'web'],
    ['machines', 'logs', 'web'],
    ['machines', 'checkpoint', 'web'],
    ['machines', 'suspend', 'web'],
    ['machines', 'wake', 'web'],
    ['services', 'ls'],
    ['services', 'info', 'web'],
    ['services', 'releases', 'web'],
    ['domains', 'ls'],
    ['volumes', 'ls'],
    ['promote', 'web'],
  ]
  try {
    for (const args of surface) {
      const res = await pilot(env, ['--json', ...args])
      assert.equal(res.code, 0, `pilot ${args.join(' ')} failed: ${res.stderr}`)
    }
    assert.equal(dashboard.requests.length, 0, 'a command reached the dashboard')
  } finally {
    await dashboard.close()
    await api.close()
  }
})

test('the type-stripping warning is filtered off stderr, and nothing else is', async () => {
  // stderr is a documented channel: `--json` promises it carries the server's
  // error body and nothing else. A note about the mechanism by which the CLI
  // runs would break that promise on any Node release that still prints one.
  const api = await startFakeAPI()
  // Answered slowly on purpose, so the warnings below fire while the process is
  // still up rather than after it has exited.
  api.routes.set('GET /v1/machines', (_req, res) => {
    setTimeout(() => {
      res.writeHead(200, { 'content-type': 'application/json' })
      res.end('[]')
    }, 400)
  })
  const env = loggedIn(api.url)
  const warner = join(import.meta.dirname, 'helpers', 'warn-import.mjs')
  try {
    const res = await pilot({ ...env, NODE_OPTIONS: `--import ${warner}` }, ['--json', 'machines', 'ls'])
    assert.equal(res.code, 0, res.stderr)
    assert.equal(res.stdout.trim(), '[]')
    assert.equal(
      res.stderr.includes('Type Stripping'),
      false,
      'the type-stripping warning reached stderr and would corrupt a --json error body',
    )
    // The filter is narrow: every other warning is re-emitted to the listeners
    // Node installed, so a real deprecation is not silently swallowed.
    assert.match(res.stderr, /something a user should actually see/)
  } finally {
    await api.close()
  }
})
