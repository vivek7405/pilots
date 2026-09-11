/**
 * The TanStack AI provider, against a fake that records every request.
 *
 * The assertions that matter: the caller's chosen id becomes the machine's
 * NAME (which is what makes the URL reconstructable), `/workspace` maps onto
 * the machine's workdir, a snapshot is a checkpoint and a restore is in place
 * so nothing new is created, and the capability flags say what is true today.
 */

import test from 'node:test'
import assert from 'node:assert/strict'

import { pilotsSandbox, DEFAULT_WORKDIR } from '../src/tanstack.ts'
import type { SandboxHandle } from '../src/tanstack.ts'
import { FakeHostd, json, machine, noContent } from './fakes/hostd.ts'

const M_ID = 'm-000000000000000000000001'

interface ExecCall {
  cmd: string
  cwd?: string
  env?: Record<string, string>
}

/** A fake wired with the routes the provider uses, and the execs it saw. */
async function withFake(
  body: (provider: ReturnType<typeof pilotsSandbox>, fake: FakeHostd, execs: ExecCall[]) => Promise<void>,
  opts: { exec?: (call: ExecCall) => { stdout?: string; stderr?: string; exit_code?: number }; machines?: unknown[] } = {},
): Promise<void> {
  const fake = new FakeHostd()
  const execs: ExecCall[] = []
  fake
    .on('POST /v1/machines', (_req, res) => json(res, 201, machine()))
    .on('GET /v1/machines', (_req, res) => json(res, 200, opts.machines ?? [machine()]))
    .on('DELETE /v1/machines/{id}', (_req, res) => noContent(res))
    .on('POST /v1/machines/{id}/exec', (req, res) => {
      const call = req.json as ExecCall
      execs.push(call)
      const answer = opts.exec?.(call) ?? {}
      json(res, 200, { stdout: answer.stdout ?? '', stderr: answer.stderr ?? '', exit_code: answer.exit_code ?? 0 })
    })
    .on('POST /v1/machines/{id}/checkpoints', (_req, res) =>
      json(res, 201, { id: 'ck-1', machine_id: M_ID, seq: 1, durable: false, created_at: 1_756_000_500 }),
    )
    .on('POST /v1/checkpoints/{id}/restore', (_req, res) => json(res, 200, machine()))
  await fake.start()
  try {
    await body(pilotsSandbox({ apiKey: 'pilot_deadbeef', baseURL: fake.baseURL }), fake, execs)
  } finally {
    await fake.stop()
  }
}

test('the provider reports what pilots can actually do', async () => {
  await withFake(async (provider) => {
    const caps = provider.capabilities()
    assert.equal(provider.name, 'pilots')
    assert.equal(caps.snapshots, true, 'checkpoints are snapshots')
    assert.equal(caps.durableFilesystem, true, 'the disk survives a host, let alone a restart')
    assert.equal(caps.writableStdin, true)
    assert.equal(caps.killableProcesses, true)
    // Cloning one machine's disk into another is post-parity backlog, and a
    // flag must say what is true today.
    assert.equal(caps.fork, false)
    assert.equal(caps.networkPolicy, false)
  })
})

test('create honours the caller id as the machine NAME, and maps /workspace', async () => {
  await withFake(async (provider, fake, execs) => {
    // No machine of that name exists yet, so one is created.
    const handle = await provider.create({ id: 'thread-42', env: { NODE_ENV: 'test' } })
    const created = fake.requests.find((r) => r.method === 'POST' && r.path === '/v1/machines')
    assert.ok(created, 'no machine was created')
    assert.deepEqual(created.json, { name: 'thread-42', env: { NODE_ENV: 'test' } })

    assert.equal(handle.provider, 'pilots')
    assert.equal(handle.id, 'demo', "the handle id is the machine's name")
    assert.equal(handle.workspaceRoot, DEFAULT_WORKDIR)
    // The workdir is created on the way up, under the real path.
    assert.match(execs[0]!.cmd, /mkdir -p -- '\/home\/pilot\/workspace'/)

    await handle.process.exec('pwd')
    assert.equal(execs.at(-1)!.cwd, DEFAULT_WORKDIR, 'a bare exec runs in the workspace')
    // The env the create carried rides on every command.
    assert.deepEqual(execs.at(-1)!.env, { NODE_ENV: 'test' })

    await handle.process.exec('pwd', { cwd: '/workspace/sub' })
    assert.equal(execs.at(-1)!.cwd, `${DEFAULT_WORKDIR}/sub`)
    await handle.process.exec('pwd', { cwd: '/etc' })
    assert.equal(execs.at(-1)!.cwd, '/etc', 'an absolute path outside /workspace is left alone')
  })
})

test('create adopts a machine whose name already matches the id', async () => {
  await withFake(async (provider, fake) => {
    await provider.create({ id: 'demo' })
    assert.equal(
      fake.requests.filter((r) => r.method === 'POST' && r.path === '/v1/machines').length,
      0,
      'an existing machine must be adopted rather than duplicated',
    )
  })
})

test('the filesystem round-trips through base64, and lstat does not follow links', async () => {
  const written: string[] = []
  await withFake(
    async (provider) => {
      const handle = await provider.create({ id: 'fs' })
      await handle.fs.write('/workspace/config.json', '{"debug": true}\n')
      assert.equal(written.at(-1), '{"debug": true}\n')

      assert.equal(await handle.fs.read('config.json'), 'file body')
      assert.deepEqual(await handle.fs.list('/workspace'), [
        { name: 'a.txt', path: `${DEFAULT_WORKDIR}/a.txt`, type: 'file' },
        { name: 'sub', path: `${DEFAULT_WORKDIR}/sub`, type: 'dir' },
      ])
      assert.equal(await handle.fs.exists('a.txt'), true)
      assert.deepEqual(await handle.fs.lstat('a.txt'), { type: 'file', mode: 0x81a4, size: 12 })
    },
    {
      exec: (call) => {
        if (call.cmd.includes('base64 -d >')) {
          const encoded = call.cmd.split("printf %s '")[1]!.split("'")[0]!
          written.push(Buffer.from(encoded, 'base64').toString())
          return {}
        }
        if (call.cmd.startsWith('base64 -w0')) return { stdout: Buffer.from('file body').toString('base64') }
        if (call.cmd.startsWith('ls -1A')) return { stdout: 'a.txt\nsub\n' }
        if (call.cmd.includes('test -d')) return { stdout: 'file\ndir\n' }
        if (call.cmd.startsWith('stat -c')) return { stdout: "regular file|81a4|12\n" }
        return {}
      },
    },
  )
})

test('git desugars to git -C in the workspace', async () => {
  await withFake(
    async (provider, _fake, execs) => {
      const handle = await provider.create({ id: 'g' })
      await handle.git.clone({ url: 'https://github.com/you/shop.git', ref: 'main', depth: 1, auth: { token: 't0ken' } })
      const clone = execs.at(-1)!.cmd
      assert.match(clone, /git clone --depth 1 --branch 'main'/)
      assert.match(clone, /x-access-token:t0ken@github\.com/, 'the token is woven into the remote')
      assert.equal(await handle.git.branch(), 'main')
      assert.match(execs.at(-1)!.cmd, new RegExp(`git -C '${DEFAULT_WORKDIR}' rev-parse --abbrev-ref HEAD`))
    },
    { exec: (call) => (call.cmd.includes('rev-parse') ? { stdout: 'main\n' } : {}) },
  )
})

test('a snapshot is a checkpoint and a restore keeps the same machine and URL', async () => {
  await withFake(async (provider, fake) => {
    const handle = (await provider.create({ id: 'snap' })) as SandboxHandle & {
      restoreSnapshot: (id: string) => Promise<void>
      url: string
    }
    const ref = await handle.snapshot!('pre-upgrade')
    assert.deepEqual(ref, { id: 'ck-1', label: 'pre-upgrade' })

    const createsBefore = fake.requests.filter((r) => r.method === 'POST' && r.path === '/v1/machines').length
    await handle.restoreSnapshot('ck-1')
    const restore = fake.requests.filter((r) => r.path === '/v1/checkpoints/ck-1/restore')
    assert.equal(restore.length, 1, 'a restore is exactly one request')
    assert.equal(
      fake.requests.filter((r) => r.method === 'POST' && r.path === '/v1/machines').length,
      createsBefore,
      'a restore must create no machine: that would mint a new URL',
    )
    assert.equal(handle.url, 'https://demo.pilotrun.app')

    // The provider-level restore reattaches to the same machine too.
    const restored = await provider.restoreSnapshot!({ snapshotId: 'ck-1' })
    assert.equal(restored.id, 'demo')
  })
})

test('ports.connect answers the permanent URL, opening nothing', async () => {
  await withFake(async (provider, fake) => {
    const handle = await provider.create({ id: 'p' })
    const before = fake.requests.length
    assert.deepEqual(await handle.ports.connect(8080), { url: 'https://demo.pilotrun.app' })
    assert.equal(fake.requests.length, before, 'connect made a request; the address is already serving')
  })
})

test('resume reattaches by name and answers null for a machine that is gone', async () => {
  await withFake(async (provider) => {
    const resumed = await provider.resume({ id: 'demo' })
    assert.ok(resumed)
    assert.equal(resumed.id, 'demo')
    assert.equal(await provider.resume({ id: 'no-such-machine' }), null)
  })
})

test('destroy removes the machine the id names', async () => {
  await withFake(async (provider, fake) => {
    await provider.destroy({ id: 'demo' })
    assert.ok(
      fake.requests.some((r) => r.method === 'DELETE' && r.path === `/v1/machines/${M_ID}`),
      'the delete did not reach the machine',
    )
    // A name nothing matches is not an error, and deletes nothing.
    const before = fake.requests.length
    await provider.destroy({ id: 'ghost' })
    assert.equal(fake.requests.filter((r) => r.method === 'DELETE').length, 1)
    assert.ok(fake.requests.length > before)
  })
})

test('env.set merges into every later command', async () => {
  await withFake(async (provider, _fake, execs) => {
    const handle = await provider.create({ id: 'e' })
    await handle.env.set({ TOKEN: 'abc' })
    await handle.process.exec('env')
    assert.deepEqual(execs.at(-1)!.env, { TOKEN: 'abc' })
    await handle.process.exec('env', { env: { TOKEN: 'override', EXTRA: '1' } })
    assert.deepEqual(execs.at(-1)!.env, { TOKEN: 'override', EXTRA: '1' })
  })
})
