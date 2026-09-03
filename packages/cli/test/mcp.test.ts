/**
 * `pilot mcp`.
 *
 * Driven two ways, because the two things at stake need different lenses. The
 * MCP SDK's own client checks that the protocol is correct; a raw pipe checks
 * that stdout carries NOTHING BUT the protocol, which a well-behaved client
 * would hide by tolerating the noise.
 */

import { strict as assert } from 'node:assert'
import { spawn } from 'node:child_process'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { after, test } from 'node:test'

import { Client } from '@modelcontextprotocol/sdk/client/index.js'
import { StdioClientTransport } from '@modelcontextprotocol/sdk/client/stdio.js'

import { saveCredentials } from '../src/config.ts'
import { fakeMachine, startFakeAPI, type FakeAPI } from './helpers/fake-api.ts'
import { json } from './helpers/server.ts'
import { startWSServer } from './helpers/ws-server.ts'

const BIN = join(import.meta.dirname, '..', 'bin', 'pilot.js')
const NOISY = join(import.meta.dirname, 'helpers', 'noisy-import.mjs')
const roots: string[] = []

after(() => {
  for (const dir of roots) rmSync(dir, { recursive: true, force: true })
})

const TOOLS = [
  'build',
  'checkpoint',
  'create_machine',
  'deploy',
  'destroy_machine',
  'exec',
  'exec_stream',
  'generate_dockerfile',
  'list_machines',
  'logs',
  'promote',
  'restore',
  'status',
]

function serverEnv(apiUrl: string): NodeJS.ProcessEnv {
  const dir = mkdtempSync(join(tmpdir(), 'pilot-mcp-'))
  roots.push(dir)
  const env = { XDG_CONFIG_HOME: dir }
  saveCredentials({ api_key: 'pilot_test_key', org_id: 'org_1', api_url: apiUrl }, env)
  return {
    ...env,
    PATH: process.env.PATH,
    PILOT_API_URL: apiUrl,
    PILOT_API_KEY: 'pilot_test_key',
  }
}

async function connect(env: NodeJS.ProcessEnv): Promise<{ client: Client; close: () => Promise<void> }> {
  const transport = new StdioClientTransport({
    command: process.execPath,
    args: [BIN, 'mcp'],
    env: env as Record<string, string>,
    stderr: 'pipe',
  })
  const client = new Client({ name: 'test', version: '0' })
  await client.connect(transport)
  return { client, close: () => client.close() }
}

function textOf(result: unknown): string {
  const content = (result as { content: { type: string; text: string }[] }).content
  return content.map((c) => c.text).join('')
}

test('the server registers exactly the thirteen tools', async () => {
  const api = await startFakeAPI()
  const { client, close } = await connect(serverEnv(api.url))
  try {
    const { tools } = await client.listTools()
    assert.deepEqual(tools.map((t) => t.name).sort(), TOOLS)
    assert.equal(tools.length, 13)
    for (const tool of tools) {
      assert.ok(tool.description && tool.description.length > 20, `${tool.name} has no useful description`)
    }
    // The two rules an agent has to follow when it writes a Dockerfile itself
    // are in the descriptions of the tools that take one.
    for (const name of ['build', 'generate_dockerfile']) {
      const tool = tools.find((t) => t.name === name)!
      assert.match(tool.description!, /0\.0\.0\.0/)
      assert.match(tool.description!, /\$PORT/)
    }
  } finally {
    await close()
    await api.close()
  }
})

test('create_machine returns the machine and reaches the fleet', async () => {
  const api = await startFakeAPI()
  const { client, close } = await connect(serverEnv(api.url))
  try {
    const result = await client.callTool({ name: 'create_machine', arguments: { name: 'agent-box', app: 'demo' } })
    assert.equal(result.isError, undefined)
    const machine = JSON.parse(textOf(result)) as { name: string; url: string }
    assert.equal(machine.name, 'agent-box')
    assert.match(machine.url, /agent-box/)
  } finally {
    await close()
    await api.close()
  }
})

test('a 429 reaches the agent as the server body, unchanged', async () => {
  const api = await startFakeAPI()
  const body = '{"error":"quota exceeded","quota":"machines","limit":20,"used":20}'
  api.routes.set('POST /v1/machines', (_req, res) => {
    res.writeHead(429, { 'content-type': 'application/json' })
    res.end(body)
  })
  const { client, close } = await connect(serverEnv(api.url))
  try {
    const result = await client.callTool({ name: 'create_machine', arguments: {} })
    assert.equal(result.isError, true)
    // Byte for byte, the same string the CLI prints and the SDK carries. H8
    // compares the three.
    assert.equal(textOf(result), body)
  } finally {
    await close()
    await api.close()
  }
})

test('a non-zero exit from exec is a result, not a tool error', async () => {
  const api = await startFakeAPI()
  api.machines.push(fakeMachine({ id: 'm_1', name: 'box' }))
  api.routes.set('POST /v1/machines/m_1/exec', (_req, res) =>
    json(res, 200, { stdout: '', stderr: 'grep: no match\n', exit_code: 1 }))
  const { client, close } = await connect(serverEnv(api.url))
  try {
    const result = await client.callTool({ name: 'exec', arguments: { machine: 'box', cmd: 'grep x f' } })
    // A grep that found nothing is not a broken tool. Marking it one would
    // make the agent retry something that worked.
    assert.equal(result.isError, undefined)
    assert.equal((JSON.parse(textOf(result)) as { exit_code: number }).exit_code, 1)
  } finally {
    await close()
    await api.close()
  }
})

test('a failed build returns every NDJSON line verbatim', async () => {
  const api = await startFakeAPI()
  const lines = [
    '{"step":"FROM","line":"pulling python:3.12-slim","ts":1}',
    '{"step":"RUN","line":"ERROR: no matching distribution found for djengo","ts":2}',
    '{"error":"process \\"/bin/sh -c pip install -r requirements.txt\\" did not complete successfully: exit code: 1","ts":3}',
  ]
  api.routes.set('POST /v1/builds', (_req, res) => {
    res.writeHead(200, { 'content-type': 'application/x-ndjson', 'x-pilot-build-id': 'bld_1' })
    res.end(lines.join('\n') + '\n')
  })
  const dir = join(import.meta.dirname, 'fixtures', 'django-app')
  const { client, close } = await connect(serverEnv(api.url))
  try {
    const result = await client.callTool({ name: 'build', arguments: { dir } })
    assert.equal(result.isError, true)
    const got = textOf(result).split('\n')
    assert.equal(got.length, 3)
    // Each line parses on its own: this is the structure an agent reads to
    // find the failing step and patch the Dockerfile.
    for (const [i, line] of got.entries()) {
      assert.deepEqual(JSON.parse(line), JSON.parse(lines[i]!))
    }
    assert.match(got[2]!, /did not complete successfully/)
  } finally {
    await close()
    await api.close()
  }
})

test('a successful build returns the rootfs id', async () => {
  const api = await startFakeAPI()
  const dir = join(import.meta.dirname, 'fixtures', 'django-app')
  const { client, close } = await connect(serverEnv(api.url))
  try {
    const result = await client.callTool({
      name: 'build',
      arguments: { dir, dockerfile: 'FROM python:3.12-slim\nCMD ["true"]\n' },
    })
    assert.equal(result.isError, undefined)
    const parsed = JSON.parse(textOf(result)) as { rootfs_build_id: string; build_id: string }
    assert.equal(parsed.rootfs_build_id, 'rootfs_1')
    assert.equal(parsed.build_id, 'bld_1')
    // The Dockerfile passed as text is what got built, so an agent never has
    // to write to disk to try a fix.
    const tar = api.find('POST', '/v1/builds')!.raw.toString('latin1')
    assert.ok(tar.includes('FROM python:3.12-slim\nCMD ["true"]'))
  } finally {
    await close()
    await api.close()
  }
})

test('generate_dockerfile detects the bare Django app and answers with a recipe', async () => {
  const api = await startFakeAPI()
  const dir = join(import.meta.dirname, 'fixtures', 'django-app')
  const { client, close } = await connect(serverEnv(api.url))
  try {
    const result = await client.callTool({ name: 'generate_dockerfile', arguments: { dir } })
    assert.equal(result.isError, undefined)
    const recipe = JSON.parse(textOf(result)) as { framework: string; dockerfile: string; port: number }
    assert.equal(recipe.framework, 'django')
    assert.equal(recipe.port, 8000)
    assert.match(recipe.dockerfile, /--bind 0\.0\.0\.0:/)
    assert.match(recipe.dockerfile, /\$\{PORT/)
  } finally {
    await close()
    await api.close()
  }
})

test('an undetectable directory is a tool error listing what was looked for', async () => {
  const api = await startFakeAPI()
  const dir = mkdtempSync(join(tmpdir(), 'pilot-mcp-empty-'))
  roots.push(dir)
  const { client, close } = await connect(serverEnv(api.url))
  try {
    const result = await client.callTool({ name: 'generate_dockerfile', arguments: { dir } })
    assert.equal(result.isError, true)
    assert.match(textOf(result), /manage\.py/)
    assert.match(textOf(result), /go\.mod/)
  } finally {
    await close()
    await api.close()
  }
})

test('exec_stream sends stdin=false when nobody asked for stdin', async () => {
  const api = await startFakeAPI()
  api.machines.push(fakeMachine({ id: 'm_1', name: 'box' }))
  // hostd serves the exec stream on the same origin as the rest of the API, so
  // this one server answers both the machine lookup and the upgrade.
  const ws = await startWSServer(
    (conn) => {
      conn.frame(1, 'hello from the guest\n')
      conn.frame(2, 'a warning\n')
      conn.frame(3, new Uint8Array([0]))
    },
    (_req, res) => {
      res.writeHead(200, { 'content-type': 'application/json' })
      res.end(JSON.stringify(api.machines[0]))
    },
  )
  const { client, close } = await connect(serverEnv(ws.url))
  try {
    const result = await client.callTool({ name: 'exec_stream', arguments: { machine: 'm_1', cmd: 'echo hi' } })
    assert.equal(result.isError, undefined)
    const parsed = JSON.parse(textOf(result)) as { stdout: string; stderr: string; exit_code: number }
    assert.equal(parsed.stdout, 'hello from the guest\n')
    assert.equal(parsed.stderr, 'a warning\n')
    assert.equal(parsed.exit_code, 0)

    const conn = ws.connections[0]!
    // Explicit, never inferred. A guest process holding an open stdin it never
    // reads hangs, and that is the reference workload.
    assert.equal(conn.query.get('stdin'), 'false')
    assert.deepEqual(conn.query.getAll('cmd'), ['sh', '-c', 'echo hi'])
    assert.ok(conn.protocols.some((p) => p === 'authorization.bearer.pilot_test_key'))
  } finally {
    await close()
    await ws.close()
    await api.close()
  }
})

test('every byte on stdout is JSON-RPC, even with a dependency logging on a timer', async () => {
  const api = await startFakeAPI()
  api.machines.push(fakeMachine({ name: 'box' }))
  const env = serverEnv(api.url)
  const child = spawn(process.execPath, [BIN, 'mcp'], {
    env: { ...env, NODE_OPTIONS: `--import ${NOISY}` },
    stdio: ['pipe', 'pipe', 'pipe'],
  })
  let stdout = ''
  let stderr = ''
  child.stdout.on('data', (c: Buffer) => (stdout += c.toString()))
  child.stderr.on('data', (c: Buffer) => (stderr += c.toString()))

  const send = (msg: unknown) => child.stdin.write(JSON.stringify(msg) + '\n')
  try {
    send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocolVersion: '2025-06-18', capabilities: {}, clientInfo: { name: 't', version: '0' } } })
    send({ jsonrpc: '2.0', method: 'notifications/initialized' })
    send({ jsonrpc: '2.0', id: 2, method: 'tools/list' })
    send({ jsonrpc: '2.0', id: 3, method: 'tools/call', params: { name: 'list_machines', arguments: {} } })

    // Long enough for the noisy module's 400 ms timer to fire while the
    // process is up.
    const deadline = Date.now() + 6000
    while (Date.now() < deadline) {
      if (stdout.split('\n').filter(Boolean).length >= 3 && stderr.includes('a dependency printed this')) break
      await new Promise((r) => setTimeout(r, 50))
    }

    const lines = stdout.split('\n').filter((l) => l.trim().length > 0)
    assert.ok(lines.length >= 3, `expected three responses, got ${lines.length}: ${stdout}`)
    for (const line of lines) {
      const parsed = JSON.parse(line) as { jsonrpc?: string }
      assert.equal(parsed.jsonrpc, '2.0', `not a JSON-RPC frame: ${line}`)
    }
    // The counterfactual, and it is a real one: the noisy module calls
    // console.log AFTER the guard is installed. Without the guard that string
    // lands on stdout and the loop above fails on a parse error.
    assert.match(stderr, /a dependency printed this to stdout/)
    assert.equal(stdout.includes('a dependency printed this'), false)
  } finally {
    child.kill()
    await api.close()
  }
})

test('deploy creates a service, waits for the release and returns the URL', async () => {
  const api = await startFakeAPI()
  const { client, close } = await connect(serverEnv(api.url))
  try {
    const result = await client.callTool({
      name: 'deploy',
      arguments: {
        name: 'gate-django',
        build: 'rootfs_1',
        app: 'gate',
        port: 8000,
        health: { type: 'http', path: '/', grace: 30 },
      },
    })
    assert.equal(result.isError, undefined, textOf(result))
    const parsed = JSON.parse(textOf(result)) as { service_id: string; url: string; release_id: string }
    assert.ok(parsed.service_id)
    assert.ok(parsed.release_id)
    assert.match(parsed.url, /gate-django/)
    // `port` is not a field on the service row: it reaches the app the way
    // every recipe reads it, as PORT in the environment.
    const body = JSON.parse(api.find('POST', '/v1/services')!.body) as { env: Record<string, string> }
    assert.equal(body.env.PORT, '8000')
  } finally {
    await close()
    await api.close()
  }
})

test('restore is in place: the machine keeps its id and URL', async () => {
  const api = await startFakeAPI()
  const machine = fakeMachine({ id: 'm_keep', name: 'box' })
  api.machines.push(machine)
  api.routes.set('POST /v1/checkpoints/cp_1/restore', (_req, res) => json(res, 200, machine))
  const { client, close } = await connect(serverEnv(api.url))
  try {
    const result = await client.callTool({ name: 'restore', arguments: { checkpoint: 'cp_1' } })
    const restored = JSON.parse(textOf(result)) as { id: string; url: string }
    assert.equal(restored.id, 'm_keep')
    assert.equal(restored.url, machine.url)
  } finally {
    await close()
    await api.close()
  }
})

test('status without a machine reports hosts and a count by state', async () => {
  const api = await startFakeAPI()
  api.machines.push(fakeMachine({ state: 'running' }), fakeMachine({ state: 'suspended' }))
  const { client, close } = await connect(serverEnv(api.url))
  try {
    const result = await client.callTool({ name: 'status', arguments: {} })
    const parsed = JSON.parse(textOf(result)) as {
      hosts: unknown[]
      machines_by_state: Record<string, number>
    }
    assert.equal(parsed.hosts.length, 1)
    assert.deepEqual(parsed.machines_by_state, { running: 1, suspended: 1 })
  } finally {
    await close()
    await api.close()
  }
})
