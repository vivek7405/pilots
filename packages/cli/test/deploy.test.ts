/**
 * `pilot deploy`: the compose executor, driven end to end against a fake fleet.
 *
 * The plan comes from hostd, so what is under test here is the WALK: the order
 * the primitives are created in, which body each call carries, and where the
 * executor stops when something fails. Order is the part that cannot be
 * checked by reading the code, because every step in isolation looks right.
 */

import { strict as assert } from 'node:assert'
import { execFile } from 'node:child_process'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { after, test } from 'node:test'
import { promisify } from 'node:util'

import { saveCredentials } from '../src/config.ts'
import { startFakeAPI, unknownFrameworkBody, type FakeAPI } from './helpers/fake-api.ts'
import { json } from './helpers/server.ts'

const exec = promisify(execFile)
const BIN = join(import.meta.dirname, '..', 'bin', 'pilot.js')
const APP_DIR = join(import.meta.dirname, 'fixtures', 'compose-app')
const roots: string[] = []

after(() => {
  for (const dir of roots) rmSync(dir, { recursive: true, force: true })
})

function loggedIn(apiUrl: string, secrets?: Record<string, Record<string, string>>): NodeJS.ProcessEnv {
  const dir = mkdtempSync(join(tmpdir(), 'pilot-deploy-'))
  roots.push(dir)
  const env = { XDG_CONFIG_HOME: dir }
  saveCredentials(
    { api_key: 'pilot_test_key', org_id: 'org_1', api_url: apiUrl, ...(secrets ? { secrets } : {}) },
    env,
  )
  return env
}

/** The plan hostd would return for the fixture, in Kahn order. */
function plan() {
  return {
    app: 'shop',
    steps: [
      {
        name: 'postgres',
        dockerfile: 'FROM postgres:17\n',
        replicas: 1,
        vcpus: 1,
        mem_mib: 512,
        volumes: [{ name: 'pgarchive', size_gib: 10, mount_path: '/archive' }],
        knobs: { auto_stop: 'off', auto_start: false, min_machines_running: 1, soft_limit: 20 },
      },
      {
        name: 'web',
        build: { context: './web', dockerfile: 'Dockerfile' },
        replicas: 2,
        vcpus: 1,
        mem_mib: 1024,
        env: { DEPLOY_ENV: 'staging' },
        secret_refs: { DATABASE_URL: 'database_url' },
        pre_deploy: 'python manage.py migrate --noinput',
        health: { type: 'http', path: '/', grace: 30 },
      },
      {
        name: 'worker',
        build: { context: './worker' },
        replicas: 1,
        vcpus: 1,
        mem_mib: 512,
        depends_on: ['postgres'],
      },
    ],
  }
}

async function pilot(env: NodeJS.ProcessEnv, args: string[], cwd = APP_DIR) {
  try {
    const { stdout, stderr } = await exec(process.execPath, [BIN, ...args], {
      cwd,
      env: { ...env, PATH: process.env.PATH },
      maxBuffer: 8 * 1024 * 1024,
    })
    return { stdout, stderr, code: 0 }
  } catch (err) {
    const e = err as { stdout?: string; stderr?: string; code?: number }
    return { stdout: e.stdout ?? '', stderr: e.stderr ?? '', code: e.code ?? 1 }
  }
}

function withPlan(api: FakeAPI, body: unknown, status = 200): void {
  api.routes.set('POST /v1/compose/plan', (_req, res) => json(res, status, body))
}

test('a three-service plan is executed in order, one primitive at a time', async () => {
  const api = await startFakeAPI()
  withPlan(api, plan())
  const env = loggedIn(api.url, { shop: { database_url: 'postgres://postgres:pw@postgres.internal:5432/postgres' } })
  try {
    const res = await pilot(env, ['--json', 'deploy'])
    assert.equal(res.code, 0, res.stderr)

    const result = JSON.parse(res.stdout) as { app: string; services: { name: string; url: string }[] }
    assert.equal(result.app, 'shop')
    assert.deepEqual(result.services.map((s) => s.name), ['postgres', 'web', 'worker'])

    // The walk, as a sequence. Reorder the executor and this is what fails;
    // every individual call still looks correct on its own.
    const order = api.requests
      .filter((r) => r.path !== '/v1/compose/plan')
      .map((r) => `${r.method} ${r.path.replace(/\/(svc|m|vol|bld)_[^/]+/g, '/{id}')}`)
    const firstService = order.indexOf('POST /v1/services')
    assert.ok(order.indexOf('POST /v1/builds') < order.indexOf('POST /v1/volumes'), 'build before volume')
    assert.ok(order.indexOf('POST /v1/volumes') < firstService, 'volume before the service')
    assert.ok(firstService < order.indexOf('POST /v1/services/{id}/deploy'), 'service before its deploy')

    assert.equal(api.all('POST', '/v1/builds').length, 3, 'one build per step, the stock image included')
    assert.equal(api.all('POST', '/v1/volumes').length, 1)
    assert.deepEqual(JSON.parse(api.find('POST', '/v1/volumes')!.body), {
      name: 'shop-pgarchive',
      size_gib: 10,
      mount_path: '/archive',
    })

    // The created volume's id reaches the service that declared it, and only
    // that one. Without this the volume exists, is billed, and is mounted by
    // nothing -- which is what the CLI used to warn about.
    const created = api.all('POST', '/v1/services').map((r) => JSON.parse(r.body) as Record<string, unknown>)
    const byName = new Map(created.map((c) => [c.name as string, c]))
    assert.equal(byName.get('postgres')?.volume, 'vol_1', 'the postgres create names the volume it made')
    assert.equal('volume' in (byName.get('web') ?? {}), false, 'web declares none and sends none')
    assert.equal('volume' in (byName.get('worker') ?? {}), false, 'worker declares none and sends none')
    assert.doesNotMatch(res.stderr, /cannot mount a volume yet/)
  } finally {
    await api.close()
  }
})

test('a stock image step is a one-file build context, not a special path', async () => {
  const api = await startFakeAPI()
  withPlan(api, plan())
  const env = loggedIn(api.url, { shop: { database_url: 'x' } })
  try {
    assert.equal((await pilot(env, ['--json', 'deploy'])).code, 0)
    const tar = api.all('POST', '/v1/builds')[0]!.raw
    assert.equal(api.all('POST', '/v1/builds')[0]!.headers['content-type'], 'application/x-tar')
    assert.match(tar.subarray(0, 512).toString('utf8'), /^Dockerfile\0/)
    assert.match(tar.subarray(512, 1024).toString('utf8'), /^FROM postgres:17/)
  } finally {
    await api.close()
  }
})

test('a resolved secret lands in secret_env and never in env', async () => {
  const api = await startFakeAPI()
  withPlan(api, plan())
  const env = loggedIn(api.url, { shop: { database_url: 'postgres://secret-value' } })
  try {
    assert.equal((await pilot(env, ['--json', 'deploy'])).code, 0)
    const web = api.all('POST', '/v1/services').map((r) => JSON.parse(r.body) as Record<string, unknown>)
      .find((b) => b.name === 'web')!
    assert.deepEqual(web.secret_env, { DATABASE_URL: 'postgres://secret-value' })
    assert.deepEqual(web.env, { DEPLOY_ENV: 'staging' })
    // The whole point: a sealed value in the clear on the service row would be
    // a password in the fleet's replicated state, and nothing would complain.
    assert.equal(JSON.stringify(web.env).includes('secret-value'), false)
  } finally {
    await api.close()
  }
})

test('the environment variable wins over the credentials file', async () => {
  const api = await startFakeAPI()
  withPlan(api, plan())
  const env = { ...loggedIn(api.url, { shop: { database_url: 'from-file' } }), PILOT_SECRET_DATABASE_URL: 'from-env' }
  try {
    assert.equal((await pilot(env, ['--json', 'deploy'])).code, 0)
    const web = api.all('POST', '/v1/services').map((r) => JSON.parse(r.body) as Record<string, unknown>)
      .find((b) => b.name === 'web')!
    assert.deepEqual(web.secret_env, { DATABASE_URL: 'from-env' })
  } finally {
    await api.close()
  }
})

test('an unresolved secret stops before any request is made', async () => {
  const api = await startFakeAPI()
  withPlan(api, plan())
  const env = loggedIn(api.url)
  try {
    const res = await pilot(env, ['deploy'])
    assert.equal(res.code, 1)
    assert.match(res.stderr, /no value for secret database_url/)
    assert.match(res.stderr, /PILOT_SECRET_DATABASE_URL/)
    // Nothing was built. A deploy that spends four minutes on a build and then
    // stops has already left half an app in the fleet.
    assert.equal(api.all('POST', '/v1/builds').length, 0)
    assert.equal(api.all('POST', '/v1/services').length, 0)
  } finally {
    await api.close()
  }
})

test('pre_deploy runs as a one-shot machine that is destroyed before the next service', async () => {
  const api = await startFakeAPI()
  withPlan(api, plan())
  const env = loggedIn(api.url, { shop: { database_url: 'x' } })
  try {
    assert.equal((await pilot(env, ['--json', 'deploy'])).code, 0)

    const created = api.find('POST', '/v1/machines')!
    const body = JSON.parse(created.body) as Record<string, unknown>
    assert.match(String(body.name), /^shop-web-predeploy-\d+$/)
    // Built from web's own rootfs, so the migration runs the code being
    // deployed rather than whatever is currently live.
    assert.equal(body.image, 'rootfs_2')
    assert.deepEqual(body.knobs, { auto_stop: 'off', auto_start: false })
    assert.deepEqual(body.secret_env, { DATABASE_URL: 'x' })

    const order = api.requests.map((r) => `${r.method} ${r.path}`)
    const destroyIndex = order.findIndex((o) => o.startsWith('DELETE /v1/machines/'))
    const workerService = api.requests.findIndex(
      (r) => r.method === 'POST' && r.path === '/v1/services' && (JSON.parse(r.body) as { name: string }).name === 'worker',
    )
    assert.ok(destroyIndex >= 0, 'the one-shot machine is destroyed')
    assert.ok(destroyIndex < workerService, 'destroyed before the next service is created')

    const execCall = api.requests.find((r) => r.method === 'POST' && r.path.endsWith('/exec'))!
    const parsed = JSON.parse(execCall.body) as Record<string, unknown>
    assert.equal(parsed.cmd, 'python manage.py migrate --noinput')
    assert.equal(parsed.cwd, '/app')
  } finally {
    await api.close()
  }
})

test('a failing pre_deploy stops the deploy and still destroys the machine', async () => {
  const api = await startFakeAPI()
  withPlan(api, plan())
  api.routes.set('POST /v1/machines', (_req, res) => json(res, 201, {
    id: 'm_pre', name: 'shop-web-predeploy-1', host_id: 'host-a', state: 'running',
    knobs: { auto_stop: 'off', auto_start: false, min_machines_running: 0, soft_limit: 20 },
    vcpus: 1, mem_mib: 512, url: 'https://x', created_at: 1, last_activity: 1,
  }))
  api.routes.set('POST /v1/machines/m_pre/exec', (_req, res) =>
    json(res, 200, { stdout: 'applying 0001_initial\n', stderr: 'relation already exists\n', exit_code: 1 }))
  const env = loggedIn(api.url, { shop: { database_url: 'x' } })
  try {
    const res = await pilot(env, ['deploy'])
    assert.equal(res.code, 1)
    assert.match(res.stderr, /pre_deploy exited 1/)
    assert.match(res.stderr, /relation already exists/)
    assert.ok(api.find('DELETE', '/v1/machines/m_pre'), 'the machine is destroyed even on failure')
    // `worker` never starts: the plan is ordered, and a broken migration is
    // not a reason to deploy the rest of the app on top of it.
    assert.equal(
      api.all('POST', '/v1/services').some((r) => (JSON.parse(r.body) as { name: string }).name === 'worker'),
      false,
    )
  } finally {
    await api.close()
  }
})

test('the compose file\'s knobs travel on the deploy, not on the create', async () => {
  const api = await startFakeAPI()
  withPlan(api, plan())
  const env = loggedIn(api.url, { shop: { database_url: 'x' } })
  try {
    assert.equal((await pilot(env, ['--json', 'deploy'])).code, 0)

    // A service row has nowhere to keep knobs, so the create drops them on the
    // floor. The deploy is what creates the replicas they apply to.
    const created = api.all('POST', '/v1/services').map((r) => JSON.parse(r.body) as Record<string, unknown>)
      .find((b) => b.name === 'postgres')!
    assert.equal('knobs' in created, false, 'knobs on the create are discarded by hostd')

    const deploys = api.requests
      .filter((r) => r.method === 'POST' && r.path.endsWith('/deploy'))
      .map((r) => JSON.parse(r.body) as Record<string, unknown>)
    const withKnobs = deploys.filter((b) => 'knobs' in b)
    assert.equal(withKnobs.length, 1, 'only the step that declared knobs sends them')
    assert.deepEqual(withKnobs[0].knobs, {
      auto_stop: 'off', auto_start: false, min_machines_running: 1, soft_limit: 20,
    })
  } finally {
    await api.close()
  }
})

test('the second run patches instead of creating, with no knobs in the body', async () => {
  const api = await startFakeAPI()
  withPlan(api, plan())
  const env = loggedIn(api.url, { shop: { database_url: 'x' } })
  try {
    assert.equal((await pilot(env, ['--json', 'deploy'])).code, 0)
    const before = api.requests.length

    const second = await pilot(env, ['--json', 'deploy'])
    assert.equal(second.code, 0, second.stderr)
    const patches = api.requests.slice(before).filter((r) => r.method === 'PATCH')
    assert.equal(patches.length, 3, 'one PATCH per service on the second run')
    for (const patch of patches) {
      const body = JSON.parse(patch.body) as Record<string, unknown>
      // #30 Decision 8: the PATCH body is a 400 with `knobs`. They travel on
      // the deploy instead, which is where the replicas they apply to are made.
      assert.equal('knobs' in body, false, 'knobs never go on the PATCH')
      // Create-only, for the same reason: the server refuses it as an
      // unknown field, and a volume swap is a data migration anyway.
      assert.equal('volume' in body, false, 'the volume never goes on the PATCH')
      assert.ok('replicas' in body)
    }
    assert.equal(
      api.requests.slice(before).some((r) => r.method === 'POST' && r.path === '/v1/services'),
      false,
      'nothing is created twice',
    )
  } finally {
    await api.close()
  }
})

test('a failed build stops before any service call and prints the line verbatim', async () => {
  const api = await startFakeAPI()
  withPlan(api, plan())
  const failing = '{"step":"RUN","error":"process \\"/bin/sh -c pip install\\" did not complete successfully: exit code: 1","ts":3}'
  api.routes.set('POST /v1/builds', (_req, res) => {
    res.writeHead(200, { 'content-type': 'application/x-ndjson', 'x-pilot-build-id': 'bld_x' })
    res.write('{"step":"FROM","line":"pulling base","ts":1}\n')
    res.end(failing + '\n')
  })
  const env = loggedIn(api.url, { shop: { database_url: 'x' } })
  try {
    const res = await pilot(env, ['--json', 'deploy'])
    assert.equal(res.code, 1)
    // The failing NDJSON line reaches stderr unchanged, which is the loop the
    // structured log exists for: an agent reads it and patches the Dockerfile.
    assert.ok(res.stderr.includes(failing), `stderr did not carry the line: ${res.stderr}`)
    assert.equal(api.all('POST', '/v1/services').length, 0, 'no service is touched after a failed build')
  } finally {
    await api.close()
  }
})

test('the plan body is {compose, env} from the .env file and nothing from process.env', async () => {
  const api = await startFakeAPI()
  withPlan(api, plan())
  const env = { ...loggedIn(api.url, { shop: { database_url: 'x' } }), PILOT_TEST_LEAK: 'should-not-travel' }
  try {
    assert.equal((await pilot(env, ['--json', 'deploy'])).code, 0)
    const body = JSON.parse(api.find('POST', '/v1/compose/plan')!.body) as {
      compose: string
      env: Record<string, string>
    }
    assert.match(body.compose, /^# The shop app/)
    // The .env file's map exactly. A plan interpolated from the ambient
    // environment builds a different app on every machine.
    assert.deepEqual(body.env, { DEPLOY_ENV: 'staging', REGION: 'eu' })
    assert.equal(JSON.stringify(body).includes('should-not-travel'), false)
  } finally {
    await api.close()
  }
})

test('--env adds to the interpolation environment for a one-off', async () => {
  const api = await startFakeAPI()
  withPlan(api, plan())
  const env = loggedIn(api.url, { shop: { database_url: 'x' } })
  try {
    assert.equal((await pilot(env, ['--json', 'deploy', '--env', 'REGION=us', '--env', 'TAG=v2'])).code, 0)
    const body = JSON.parse(api.find('POST', '/v1/compose/plan')!.body) as { env: Record<string, string> }
    assert.deepEqual(body.env, { DEPLOY_ENV: 'staging', REGION: 'us', TAG: 'v2' })
  } finally {
    await api.close()
  }
})

test('a PlanError prints one line per rejected key and exits 1', async () => {
  const api = await startFakeAPI()
  withPlan(
    api,
    {
      error: 'compose file has unsupported keys',
      unsupported: [
        { service: 'web', key: 'deploy.placement', message: 'placement is decided by the fleet' },
        { service: 'worker', key: 'depends_on.condition', message: 'service_completed_successfully is not supported; use x-pilots.pre_deploy' },
      ],
    },
    400,
  )
  const env = loggedIn(api.url)
  try {
    const res = await pilot(env, ['deploy'])
    assert.equal(res.code, 1)
    assert.match(res.stderr, /web\.deploy\.placement: placement is decided by the fleet/)
    assert.match(res.stderr, /worker\.depends_on\.condition: .*pre_deploy/)
    assert.equal(api.all('POST', '/v1/builds').length, 0)
  } finally {
    await api.close()
  }
})

// A directory with no compose file is no longer an error the CLI raises: it
// is a question for the host, which is what makes a repo with only a
// Dockerfile, or with neither, deployable at all. What the CLI owns is the
// tar it sends and the plan it executes.
test('a directory with no compose file is planned by the host and deployed', async () => {
  const api = await startFakeAPI()
  const dir = join(import.meta.dirname, 'fixtures', 'webjs-app')
  const env = loggedIn(api.url)
  try {
    const res = await pilot(env, ['--json', 'deploy'], dir)
    assert.equal(res.code, 0, res.stderr)

    // The whole directory went up as a tar. The host reads the files; the
    // CLI does not look at them at all.
    const planned = api.find('POST', '/v1/plan')
    assert.ok(planned, 'the CLI never asked the host what the directory was')
    const posted = planned.raw.toString('latin1')
    assert.ok(posted.includes('package.json'), 'the tar is missing package.json')
    assert.ok(posted.includes('app/page.ts'), 'the tar is missing the app directory')

    // The plan carried a build context AND generated Dockerfile text, so the
    // build has to upload the text as the context's own Dockerfile.
    const built = api.find('POST', '/v1/builds')!.raw.toString('latin1')
    assert.ok(built.includes('ENV PORT=8080'), "the plan's Dockerfile did not reach the build")

    // And the health the plan named reached the service, so the gate polls
    // the readiness path rather than the default.
    const created = JSON.parse(api.find('POST', '/v1/services')!.body) as {
      health?: { path?: string }
    }
    assert.equal(created.health?.path, '/__webjs/ready')
  } finally {
    await api.close()
  }
})

// The refusal is printed with the server's own next step under it, because
// that line is the whole difference between "it did not work" and "here is
// what to do".
test('a directory the host cannot place prints the refusal and its next step', async () => {
  const api = await startFakeAPI()
  const empty = mkdtempSync(join(tmpdir(), 'pilot-empty-'))
  roots.push(empty)
  api.routes.set('POST /v1/plan', (_req, res) => json(res, 400, unknownFrameworkBody()))
  const env = loggedIn(api.url)
  try {
    const res = await pilot(env, ['deploy'], empty)
    assert.equal(res.code, 1)
    assert.match(res.stderr, /no framework was detected/)
    assert.match(res.stderr, /\u2192 add a Dockerfile/)
  } finally {
    await api.close()
  }
})

test('a service declaring two volumes is refused before anything is built', async () => {
  const api = await startFakeAPI()
  const twoVolumes = plan()
  twoVolumes.steps[0]!.volumes = [
    { name: 'pgdata', size_gib: 10, mount_path: '/var/lib/postgresql/data' },
    { name: 'pgarchive', size_gib: 10, mount_path: '/archive' },
  ]
  withPlan(api, twoVolumes)
  const env = loggedIn(api.url, { shop: { database_url: 'x' } })
  try {
    const res = await pilot(env, ['--json', 'deploy'])
    assert.notEqual(res.code, 0)
    assert.match(res.stdout + res.stderr, /postgres/)
    assert.match(res.stdout + res.stderr, /mounts one/)
    // Before the build, so a compose file that asks for two does not spend
    // minutes on an image first.
    assert.equal(api.all('POST', '/v1/builds').length, 0, 'nothing was built')
  } finally {
    await api.close()
  }
})

test('the second run refuses a volume swap rather than sending one', async () => {
  const api = await startFakeAPI()
  withPlan(api, plan())
  const env = loggedIn(api.url, { shop: { database_url: 'x' } })
  try {
    assert.equal((await pilot(env, ['--json', 'deploy'])).code, 0)

    // The same app, with the volume renamed: a different volume, and the
    // platform copies nothing between two of them.
    const renamed = plan()
    renamed.steps[0]!.volumes = [{ name: 'pgdata', size_gib: 10, mount_path: '/archive' }]
    withPlan(api, renamed)
    const before = api.requests.length

    const second = await pilot(env, ['--json', 'deploy'])
    assert.notEqual(second.code, 0)
    const said = second.stdout + second.stderr
    assert.match(said, /postgres/)
    assert.match(said, /created/)
    assert.equal(
      api.requests.slice(before).some((r) => r.method === 'PATCH'),
      false,
      'no PATCH is sent for a swap the server would refuse anyway',
    )
    // Refused for free. ensureVolumes creates the name it cannot find, so a
    // refusal after it would leave a brand-new, empty, billed volume behind
    // and would have paid for a full image first.
    const after = api.requests.slice(before)
    assert.equal(
      after.filter((r) => r.method === 'POST' && r.path === '/v1/volumes').length,
      0,
      'the renamed volume is never created',
    )
    assert.equal(
      after.filter((r) => r.method === 'POST' && r.path === '/v1/builds').length,
      0,
      'nothing is built for a deploy that is refused',
    )
  } finally {
    await api.close()
  }
})

/** The body of one file in a ustar archive, by name. */
function tarEntry(tar: Buffer, want: string): string | undefined {
  for (let off = 0; off + 512 <= tar.length; ) {
    const header = tar.subarray(off, off + 512)
    const name = header.subarray(0, 100).toString('utf8').replace(/\0.*$/, '')
    if (name === '') return undefined
    const size = parseInt(header.subarray(124, 136).toString('utf8').replace(/\0.*$/, '').trim() || '0', 8)
    const body = tar.subarray(off + 512, off + 512 + size)
    if (name === want) return body.toString('utf8')
    off += 512 + Math.ceil(size / 512) * 512
  }
  return undefined
}

/**
 * The compose file's `command:`, `working_dir:` and `user:` reach the guest.
 *
 * They reach it as Dockerfile instructions appended to the context's own
 * Dockerfile, because hostd reads that file's final stage to learn what the
 * image starts. Before this, the plan rendered the command into a `cmd` field
 * that nothing downstream read: the build succeeded, the machine came up, and
 * it ran the image's own CMD forever.
 */
test("a build step's overrides are appended to the context's Dockerfile", async () => {
  const api = await startFakeAPI()
  const withAppend = plan()
  const web = withAppend.steps[1] as Record<string, unknown>
  web.dockerfile_append = 'WORKDIR "/app/gallery"\nCMD ["bun","/app/node_modules/.bin/webjs","start"]\n'
  withPlan(api, withAppend)
  const env = loggedIn(api.url, { shop: { database_url: 'x' } })
  try {
    assert.equal((await pilot(env, ['--json', 'deploy'])).code, 0)
    // The second build is web's; the first is the stock postgres image.
    const dockerfile = tarEntry(api.all('POST', '/v1/builds')[1]!.raw, 'Dockerfile')
    assert.equal(
      dockerfile,
      'FROM python:3.12-slim\nWORKDIR "/app/gallery"\nCMD ["bun","/app/node_modules/.bin/webjs","start"]\n',
    )
  } finally {
    await api.close()
  }
})

/** A step with nothing to override uploads the context's Dockerfile untouched. */
test('a build step with no overrides sends the context Dockerfile as it is', async () => {
  const api = await startFakeAPI()
  withPlan(api, plan())
  const env = loggedIn(api.url, { shop: { database_url: 'x' } })
  try {
    assert.equal((await pilot(env, ['--json', 'deploy'])).code, 0)
    assert.equal(tarEntry(api.all('POST', '/v1/builds')[1]!.raw, 'Dockerfile'), 'FROM python:3.12-slim\n')
  } finally {
    await api.close()
  }
})

// `--env` is the interpolation environment for a compose file. On a directory
// that has none it has nothing to interpolate, and a flag that is parsed and
// then ignored deploys something other than what was asked for while
// reporting success.
test('--env on a directory with no compose file is refused, not dropped', async () => {
  const api = await startFakeAPI()
  const dir = join(import.meta.dirname, 'fixtures', 'webjs-app')
  const env = loggedIn(api.url)
  try {
    const res = await pilot(env, ['deploy', '--env', 'DATABASE_URL=postgres://x'], dir)
    assert.equal(res.code, 1)
    assert.match(res.stderr, /--env is the interpolation environment/)
    assert.equal(api.all('POST', '/v1/plan').length, 0, 'it planned before refusing')
    assert.equal(api.all('POST', '/v1/builds').length, 0, 'it built before refusing')
  } finally {
    await api.close()
  }
})
