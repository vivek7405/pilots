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
import { fakeService, startFakeAPI, unknownFrameworkBody, type FakeAPI } from './helpers/fake-api.ts'
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

/** A build stream with one BuildKit vertex that takes 12.3s, then the host phases. */
function stagedBuild(api: FakeAPI, extra = ''): void {
  api.routes.set('POST /v1/builds', (_req, res) => {
    res.writeHead(200, { 'content-type': 'application/x-ndjson', 'x-pilot-build-id': 'bld_x' })
    res.write('{"step":"[stage-1 1/2] RUN npm ci","stream":"stdout","line":"added 12 packages","ts":1000}\n')
    if (extra) res.write(extra + '\n')
    res.write('{"step":"[stage-1 1/2] RUN npm ci","stream":"status","line":"done","ts":13300}\n')
    res.write('{"step":"bld_x","stream":"status","line":"packing rootfs","ts":13400}\n')
    res.write('{"step":"bld_x","stream":"status","line":"build succeeded","ts":14000}\n')
    res.end('{"result":"bld_x","ts":14000}\n')
  })
}

// Counterfactual: printing every line puts the npm output on stderr, which is
// the firehose this replaces.
test('the quiet default prints one line per stage with elapsed time', async () => {
  const api = await startFakeAPI()
  withPlan(api, plan())
  stagedBuild(api)
  const env = loggedIn(api.url, { shop: { database_url: 'x' } })
  try {
    const res = await pilot(env, ['deploy'])
    assert.equal(res.code, 0, res.stderr)
    assert.match(res.stderr, /web {2}\[stage-1 1\/2\] RUN npm ci {2}done 12\.3s/)
    assert.doesNotMatch(res.stderr, /added 12 packages/)
    assert.match(res.stderr, /web {2}packing rootfs/)
    assert.match(res.stderr, /^plan: 3 services in app shop$/m)
  } finally {
    await api.close()
  }
})

test('--verbose streams every line, and --ci implies it without colour', async () => {
  const api = await startFakeAPI()
  withPlan(api, plan())
  stagedBuild(api)
  const env = loggedIn(api.url, { shop: { database_url: 'x' } })
  try {
    const verbose = await pilot(env, ['deploy', '--verbose'])
    assert.equal(verbose.code, 0, verbose.stderr)
    assert.ok(verbose.stderr.includes('added 12 packages'))

    const ci = await pilot({ ...env, FORCE_COLOR: '1' }, ['deploy', '--ci'])
    assert.equal(ci.code, 0, ci.stderr)
    assert.ok(ci.stderr.includes('added 12 packages'))
    // eslint-disable-next-line no-control-regex
    assert.doesNotMatch(ci.stderr, /\x1b\[/)

    // railway's own equivalence, and the reason it exists: a build log read
    // later is not a terminal.
    const viaEnv = await pilot({ ...env, CI: 'true' }, ['deploy'])
    assert.equal(viaEnv.code, 0, viaEnv.stderr)
    assert.ok(viaEnv.stderr.includes('added 12 packages'))
  } finally {
    await api.close()
  }
})

// Quiet mode hides a step's output right up until that step is the one that
// failed, which is the only moment anybody wants it.
test('a failed build prints the failing step output before the error', async () => {
  const api = await startFakeAPI()
  withPlan(api, plan())
  api.routes.set('POST /v1/builds', (_req, res) => {
    res.writeHead(200, { 'content-type': 'application/x-ndjson', 'x-pilot-build-id': 'bld_x' })
    res.write('{"step":"[stage-1 1/2] RUN pip install","stream":"stdout","line":"ERROR: no matching distribution","ts":1000}\n')
    res.end('{"step":"[stage-1 1/2] RUN pip install","error":"exit code: 1","ts":2000}\n')
  })
  const env = loggedIn(api.url, { shop: { database_url: 'x' } })
  try {
    const res = await pilot(env, ['deploy'])
    assert.equal(res.code, 1)
    assert.ok(res.stderr.includes('ERROR: no matching distribution'), res.stderr)
    const output = res.stderr.indexOf('ERROR: no matching distribution')
    const error = res.stderr.indexOf('error: build bld_x failed')
    assert.ok(output < error, 'the output comes before the error line')
  } finally {
    await api.close()
  }
})

// Counterfactual: computing wait from opts.wait alone ignores --detach, and
// the run polls GET /v1/services/{id} anyway.
test('--detach returns after the deploy call and prints the release id', async () => {
  const api = await startFakeAPI()
  withPlan(api, plan())
  stagedBuild(api)
  const env = loggedIn(api.url, { shop: { database_url: 'x' } })
  try {
    const res = await pilot(env, ['deploy', '--detach'])
    assert.equal(res.code, 0, res.stderr)
    const header = res.stdout.split('\n')[0]!.trim().split(/ {2,}/)
    assert.deepEqual(header, ['SERVICE', 'RELEASE', 'URL'])
    assert.match(res.stderr, /accepted, not waited for/)

    // One GET per service, the read that fills in the URL. Waiting adds the
    // readiness poll on top of it, so the count is what separates the modes.
    const gets = (reqs: typeof api.requests): number =>
      reqs.filter((r) => r.method === 'GET' && /^\/v1\/services\/svc_[^/]+$/.test(r.path)).length
    assert.equal(gets(api.requests), 3, 'three services, one URL read each, no poll')

    // --no-wait is the same flag under its older name.
    const waiting = await startFakeAPI()
    withPlan(waiting, plan())
    stagedBuild(waiting)
    const waitingEnv = loggedIn(waiting.url, { shop: { database_url: 'x' } })
    try {
      assert.equal((await pilot(waitingEnv, ['deploy'])).code, 0)
      assert.ok(gets(waiting.requests) > 3, 'waiting polls for the release on top of the URL read')
    } finally {
      await waiting.close()
    }
  } finally {
    await api.close()
  }
})

// A person reads an empty column as a failed deploy. Both replicas were up the
// whole time; a service just has no URL until it has a domain.
test('an empty URL says why, and the JSON keeps the empty string', async () => {
  const api = await startFakeAPI()
  // One service, so nothing here depends on the volume echo the default route
  // does; the point is the URL column, not the walk.
  withPlan(api, { app: 'shop', steps: [{ name: 'web', build: { context: './web' }, replicas: 1, vcpus: 1, mem_mib: 512 }] })
  stagedBuild(api)
  api.routes.set('POST /v1/services', (req, res) => {
    const body = JSON.parse(req.body || '{}') as { name?: string; app?: string }
    const svc = fakeService({ name: body.name ?? 'x', ...(body.app ? { app: body.app } : {}), url: '' })
    api.services.push(svc)
    return json(res, 201, svc)
  })
  const env = loggedIn(api.url)
  try {
    const human = await pilot(env, ['deploy'])
    assert.equal(human.code, 0, human.stderr)
    assert.match(human.stdout, /\(private: peers reach it at web\.internal\)/)
    assert.match(human.stderr, /pilot machines ls --app shop/)

    const asJSON = await pilot(env, ['--json', 'deploy'])
    assert.equal(asJSON.code, 0, asJSON.stderr)
    const result = JSON.parse(asJSON.stdout) as { services: { url: string }[] }
    assert.equal(result.services[0]!.url, '', 'the explanation is human-only')
  } finally {
    await api.close()
  }
})

// The [dir] argument names the directory searched for the compose file, but
// every build context resolves against the compose FILE's directory, so
// `--file infra/compose.yaml` builds from infra/ and not from [dir].
test('deploy --help states the build context rule', async () => {
  const res = await pilot({}, ['deploy', '--help'])
  assert.match(res.stdout, /compose file's own directory/)
  assert.match(res.stdout, /-d, --detach/)
  assert.match(res.stdout, /-c, --ci/)
})

// The timeout said only that it timed out. Where to look next is the whole
// question at that moment, and it is a CLI-raised error so the hint is ours.
test('the readiness timeout carries a hint naming what to run next', async () => {
  const { PilotsClient } = await import('@pilots/sdk')
  const { executePlan } = await import('../src/compose/run.ts')
  const { CliError } = await import('../src/output.ts')

  const api = await startFakeAPI()
  stagedBuild(api)
  // A service whose release_id never becomes the one just deployed.
  api.routes.set('POST /v1/services/svc_stuck/deploy', (_req, res) =>
    json(res, 201, { id: 'rel_new', service_id: 'svc_stuck', healthy: false, created_at: 1 }),
  )
  api.routes.set('GET /v1/services/svc_stuck', (_req, res) =>
    json(res, 200, fakeService({ id: 'svc_stuck', name: 'web', release_id: 'rel_old' })),
  )
  api.routes.set('POST /v1/services', (_req, res) =>
    json(res, 201, fakeService({ id: 'svc_stuck', name: 'web', release_id: 'rel_old' })),
  )
  try {
    const client = new PilotsClient('pilot_test_key', { baseURL: api.url })
    let clock = 0
    await assert.rejects(
      executePlan(
        client,
        { app: 'shop', steps: [{ name: 'web', build: { context: './web' }, replicas: 1, vcpus: 1, mem_mib: 512 }] },
        {
          dir: APP_DIR,
          waitTimeoutMs: 5000,
          sleep: () => Promise.resolve(),
          now: () => (clock += 4000),
        },
      ),
      (err: unknown) => {
        assert.ok(err instanceof CliError)
        assert.match(err.message, /did not become current within 5s/)
        assert.ok(err.hint)
        assert.match(err.hint, /pilot services releases/)
        assert.match(err.hint, /pilot logs/)
        return true
      },
    )
  } finally {
    await api.close()
  }
})

/**
 * The same vertex name in more than one service, with each build's timestamps
 * where they really are: absolute wall-clock milliseconds, so the second
 * build's stream starts long after the first one's.
 */
function sharedVertexBuilds(api: FakeAPI): void {
  let call = 0
  api.routes.set('POST /v1/builds', (_req, res) => {
    // Every service here is built from the same Dockerfile, so BuildKit names
    // the vertex identically in all three streams -- the ordinary case for an
    // app whose services share a base image.
    const base = call++ * 100_000
    res.writeHead(200, { 'content-type': 'application/x-ndjson', 'x-pilot-build-id': 'bld_x' })
    res.write(`{"step":"[stage-1 1/2] RUN npm ci","stream":"stdout","line":"added 12 packages","ts":${base + 1000}}\n`)
    res.write(`{"step":"[stage-1 1/2] RUN npm ci","stream":"status","line":"done","ts":${base + 13300}}\n`)
    res.end(`{"result":"bld_x","ts":${base + 14000}}\n`)
  })
}

// Counterfactual: keying the elapsed-time map by the vertex name alone makes
// every service after the first time its stage from the FIRST service's first
// line, so a 12.3s stage is reported as 112.3s and then 212.3s.
test('each service times its own stages, even when the vertex names collide', async () => {
  const api = await startFakeAPI()
  withPlan(api, plan())
  sharedVertexBuilds(api)
  const env = loggedIn(api.url, { shop: { database_url: 'x' } })
  try {
    const res = await pilot(env, ['deploy'])
    assert.equal(res.code, 0, res.stderr)
    for (const service of ['postgres', 'web', 'worker']) {
      assert.match(
        res.stderr,
        new RegExp(`^${service} {2}\\[stage-1 1/2\\] RUN npm ci {2}done 12\\.3s$`, 'm'),
        `${service} did not time its own build`,
      )
    }
  } finally {
    await api.close()
  }
})

// Counterfactual: labelling the replay with the plan's first step names a
// service that built fine, and reading the buffer by vertex name alone replays
// that service's output instead of the one that actually failed.
test('a failure in a later service is labelled and replayed as that service', async () => {
  const api = await startFakeAPI()
  withPlan(api, plan())
  let call = 0
  api.routes.set('POST /v1/builds', (_req, res) => {
    res.writeHead(200, { 'content-type': 'application/x-ndjson', 'x-pilot-build-id': 'bld_x' })
    if (call++ === 0) {
      res.write('{"step":"[stage-1 1/2] RUN npm ci","stream":"stdout","line":"postgres output","ts":1000}\n')
      res.write('{"step":"[stage-1 1/2] RUN npm ci","stream":"status","line":"done","ts":2000}\n')
      return res.end('{"result":"bld_x","ts":2000}\n')
    }
    res.write('{"step":"[stage-1 1/2] RUN npm ci","stream":"stdout","line":"ERROR: no matching distribution","ts":101000}\n')
    res.end('{"step":"[stage-1 1/2] RUN npm ci","error":"exit code: 1","ts":102000}\n')
  })
  const env = loggedIn(api.url, { shop: { database_url: 'x' } })
  try {
    const res = await pilot(env, ['deploy'])
    assert.equal(res.code, 1)
    assert.match(res.stderr, /^web {2}ERROR: no matching distribution$/m)
    assert.doesNotMatch(res.stderr, /^postgres {2}ERROR: no matching distribution$/m)
    // The step that succeeded is not replayed alongside the one that failed.
    assert.doesNotMatch(res.stderr, /^web {2}postgres output$/m)
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

// The two halves of this PR had to meet here: the host-planned path is a
// deploy like any other, so it gets the staged output rather than the
// firehose, and its stderr under --json stays NDJSON only.
test('a host-planned directory gets the quiet output, and --json keeps stderr clean', async () => {
  const api = await startFakeAPI()
  stagedBuild(api)
  const dir = join(import.meta.dirname, 'fixtures', 'webjs-app')
  const env = loggedIn(api.url)
  try {
    const human = await pilot(env, ['deploy'], dir)
    assert.equal(human.code, 0, human.stderr)
    // One line per stage with its own elapsed time, not every npm line.
    assert.match(human.stderr, /\[stage-1 1\/2\] RUN npm ci {2}done 12\.3s/)
    assert.doesNotMatch(human.stderr, /added 12 packages/)
    // The host's own detection still reaches a person.
    assert.match(human.stderr, /in \./)

    const asJSON = await pilot(env, ['--json', 'deploy'], dir)
    assert.equal(asJSON.code, 0, asJSON.stderr)
    // Counterfactual: leaving the detection lines unguarded puts prose on the
    // one stderr that promises NDJSON and nothing else.
    for (const line of asJSON.stderr.split('\n').filter(Boolean)) {
      assert.doesNotThrow(() => JSON.parse(line), `stderr carried prose under --json: ${line}`)
    }
  } finally {
    await api.close()
  }
})

// Without a compose file there is no compose file to put x-pilots.domain in,
// so the empty-URL explanation names only the command that actually applies.
test('the empty-URL advice matches the path the deploy took', async () => {
  const api = await startFakeAPI()
  stagedBuild(api)
  api.routes.set('POST /v1/services', (req, res) => {
    const body = JSON.parse(req.body || '{}') as { name?: string; app?: string }
    const svc = fakeService({ name: body.name ?? 'x', ...(body.app ? { app: body.app } : {}), url: '' })
    api.services.push(svc)
    return json(res, 201, svc)
  })
  const dir = join(import.meta.dirname, 'fixtures', 'webjs-app')
  const env = loggedIn(api.url)
  try {
    const res = await pilot(env, ['deploy'], dir)
    assert.equal(res.code, 0, res.stderr)
    assert.match(res.stdout, /\(private: peers reach it at web\.internal\)/)
    assert.doesNotMatch(res.stdout, /x-pilots\.domain/)
  } finally {
    await api.close()
  }
})

// A database has nothing to serve on 8080, so a compose file can ask for no
// address. The flag has to survive the whole walk from the plan to the create,
// or the file says one thing and the service does another.
test('x-pilots.private reaches the create request', async () => {
  const api = await startFakeAPI()
  const p = plan()
  ;(p.steps[0] as Record<string, unknown>).private = true
  withPlan(api, p)
  const env = loggedIn(api.url, { shop: { database_url: 'x' } })
  try {
    const res = await pilot(env, ['--json', 'deploy'])
    assert.equal(res.code, 0, res.stderr)

    const created = api.all('POST', '/v1/services').map((r) => JSON.parse(r.body) as Record<string, unknown>)
    const byName = new Map(created.map((c) => [c.name as string, c]))
    assert.equal(byName.get('postgres')?.private, true, 'the private step asked for no address')
    assert.equal('domain' in (byName.get('postgres') ?? {}), false, 'and named no address')
    // Every other service is untouched: private is per service, not per file.
    assert.equal('private' in (byName.get('web') ?? {}), false)
    assert.equal('private' in (byName.get('worker') ?? {}), false)
  } finally {
    await api.close()
  }
})
