/**
 * `pilot secrets set`, `import` and `ls`.
 *
 * Every case spawns `bin/pilot.js`, because the thing under test is the whole
 * path a user drives: argument parsing, the app derivation, the atomic 0600
 * write and what reaches the two output streams. A test that imported the
 * action would pass with the command unwired.
 *
 * The assertion running through all of it is that a value never appears on
 * stdout or stderr. That is the point of the design, and it is the one failure
 * a reviewer cannot see by reading a diff.
 */

import { strict as assert } from 'node:assert'
import { execFile, execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { after, test } from 'node:test'
import { promisify } from 'node:util'

import { credentialsPath, loadCredentials, saveCredentials } from '../src/config.ts'
import { promptSecret } from '../src/prompt.ts'
import { digestOf, setSecret } from '../src/secrets/store.ts'
import { startFakeAPI, type FakeAPI } from './helpers/fake-api.ts'
import { json } from './helpers/server.ts'

const exec = promisify(execFile)
const BIN = join(import.meta.dirname, '..', 'bin', 'pilot.js')
const roots: string[] = []

after(() => {
  for (const dir of roots) rmSync(dir, { recursive: true, force: true })
})

interface Bed {
  /** The project directory, holding the compose file. */
  dir: string
  env: NodeJS.ProcessEnv
}

function tmp(prefix: string): string {
  const dir = mkdtempSync(join(tmpdir(), prefix))
  roots.push(dir)
  return dir
}

/**
 * A logged-in config dir plus a project whose compose file says `name: shop`,
 * and one other app's secret already stored so every write can be checked for
 * clobbering.
 */
function bed(compose = 'name: shop\nservices:\n  web:\n    build: .\n', dotenv?: string): Bed {
  const dir = tmp('pilot-secrets-')
  writeFileSync(join(dir, 'compose.yaml'), compose)
  if (dotenv !== undefined) writeFileSync(join(dir, '.env'), dotenv)
  const env = { XDG_CONFIG_HOME: tmp('pilot-secrets-cfg-') }
  saveCredentials(
    {
      api_key: 'pilot_k',
      org_id: 'org_1',
      api_url: 'https://fleet',
      secrets: { blog: { other: 'keep-me' } },
    },
    env,
  )
  return { dir, env }
}

interface RunResult {
  stdout: string
  stderr: string
  code: number
}

async function pilot(b: Bed, args: string[], extra: NodeJS.ProcessEnv = {}): Promise<RunResult> {
  try {
    const { stdout, stderr } = await exec(process.execPath, [BIN, ...args], {
      cwd: b.dir,
      env: { ...b.env, ...extra, PATH: process.env.PATH },
    })
    return { stdout, stderr, code: 0 }
  } catch (err) {
    const e = err as { stdout?: string; stderr?: string; code?: number }
    return { stdout: e.stdout ?? '', stderr: e.stderr ?? '', code: e.code ?? 1 }
  }
}

/** `pilot` with something on stdin, which is how a script sets a value. */
function pilotWithStdin(b: Bed, args: string[], input: string): RunResult {
  try {
    const stdout = execFileSync(process.execPath, [BIN, ...args], {
      cwd: b.dir,
      env: { ...b.env, PATH: process.env.PATH },
      input,
      encoding: 'utf8',
      stdio: ['pipe', 'pipe', 'pipe'],
    })
    return { stdout, stderr: '', code: 0 }
  } catch (err) {
    const e = err as { stdout?: string; stderr?: string; status?: number }
    return { stdout: e.stdout ?? '', stderr: e.stderr ?? '', code: e.status ?? 1 }
  }
}

test('set stores under the compose file\'s app and leaves everything else intact', async () => {
  const b = bed()
  const res = await pilot(b, ['secrets', 'set', 'auth_secret', 's3cret'])
  assert.equal(res.code, 0, res.stderr)

  const creds = loadCredentials(b.env)!
  assert.equal(creds.secrets?.shop?.auth_secret, 's3cret')
  // Counterfactual: a `saveCredentials({ secrets: ... })` without the spread
  // drops the API key and the other app, which logs the user out on the next
  // command and loses a secret nobody was touching.
  assert.equal(creds.secrets?.blog?.other, 'keep-me')
  assert.equal(creds.api_key, 'pilot_k')
  assert.equal(creds.org_id, 'org_1')
})

test('--app wins over the compose file', async () => {
  const b = bed()
  assert.equal((await pilot(b, ['secrets', 'set', 'k', 'v', '--app', 'other'])).code, 0)
  const creds = loadCredentials(b.env)!
  assert.equal(creds.secrets?.other?.k, 'v')
  assert.equal(creds.secrets?.shop, undefined)
})

test('COMPOSE_PROJECT_NAME in .env wins over name:', async () => {
  const b = bed('name: shop\n', 'COMPOSE_PROJECT_NAME=from-env\n')
  assert.equal((await pilot(b, ['secrets', 'set', 'k', 'v'])).code, 0)
  assert.equal(loadCredentials(b.env)!.secrets?.['from-env']?.k, 'v')
})

test('x-pilots.app wins over name:', async () => {
  const b = bed('name: shop\nx-pilots:\n  app: from-x\n')
  assert.equal((await pilot(b, ['secrets', 'set', 'k', 'v'])).code, 0)
  assert.equal(loadCredentials(b.env)!.secrets?.['from-x']?.k, 'v')
})

test('--env COMPOSE_PROJECT_NAME matches what deploy --env would derive', async () => {
  const b = bed('name: shop\n')
  assert.equal((await pilot(b, ['secrets', 'set', 'k', 'v', '--env', 'COMPOSE_PROJECT_NAME=prod'])).code, 0)
  // Counterfactual: without the flag this lands under `shop`, and a
  // `pilot deploy --env COMPOSE_PROJECT_NAME=prod` then fails saying the
  // secret was never set while `ls` shows it sitting there.
  assert.equal(loadCredentials(b.env)!.secrets?.prod?.k, 'v')
  assert.equal(loadCredentials(b.env)!.secrets?.shop, undefined)
})

test('--env wins over the .env file, the way deploy merges them', async () => {
  const b = bed('name: shop\n', 'COMPOSE_PROJECT_NAME=from-file\n')
  assert.equal((await pilot(b, ['secrets', 'set', 'k', 'v', '--env', 'COMPOSE_PROJECT_NAME=from-flag'])).code, 0)
  assert.equal(loadCredentials(b.env)!.secrets?.['from-flag']?.k, 'v')
})

test('--file takes the app from another compose file, resolved against --dir', async () => {
  const b = bed('name: shop\n')
  writeFileSync(join(b.dir, 'prod.compose.yaml'), 'name: shop-prod\n')
  assert.equal((await pilot(b, ['secrets', 'set', 'k', 'v', '--file', 'prod.compose.yaml'])).code, 0)
  // Counterfactual: ignoring --file reads compose.yaml and stores under
  // `shop`, which is a different app from the one that deploy --file builds.
  assert.equal(loadCredentials(b.env)!.secrets?.['shop-prod']?.k, 'v')
  assert.equal(loadCredentials(b.env)!.secrets?.shop, undefined)
})

test('--app still wins over --env and --file', async () => {
  const b = bed('name: shop\n')
  writeFileSync(join(b.dir, 'prod.compose.yaml'), 'name: shop-prod\n')
  const res = await pilot(b, [
    'secrets', 'ls', '--json',
    '--app', 'explicit', '--env', 'COMPOSE_PROJECT_NAME=prod', '--file', 'prod.compose.yaml',
  ])
  assert.equal(res.code, 0, res.stderr)
  assert.equal((JSON.parse(res.stdout) as { app: string }).app, 'explicit')
})

test('a compose file with no app name is refused naming the three sources', async () => {
  const b = bed('services:\n  web:\n    build: .\n')
  const res = await pilot(b, ['secrets', 'set', 'k', 'v'])
  assert.equal(res.code, 1)
  assert.match(res.stderr, /top-level name:.*COMPOSE_PROJECT_NAME.*--app/)
  assert.equal(loadCredentials(b.env)!.secrets?.shop, undefined)
})

test('no compose file at all names --app and the four filenames', async () => {
  const b = bed()
  rmSync(join(b.dir, 'compose.yaml'))
  const res = await pilot(b, ['secrets', 'ls'])
  assert.equal(res.code, 1)
  assert.match(res.stderr, /pass --app/)
  assert.match(res.stderr, /compose\.yaml, compose\.yml, docker-compose\.yml, docker-compose\.yaml/)
})

test('no value is echoed by set or import', async () => {
  const b = bed()
  const set = await pilot(b, ['secrets', 'set', 'auth_secret', 's3cret'])
  // Counterfactual: a `note()` that included the value for confirmation, which
  // is the obvious convenience, fails here and puts the secret in scrollback.
  assert.equal(set.stdout, '')
  assert.equal(set.stderr.includes('s3cret'), false, set.stderr)
  assert.match(set.stderr, /stored auth_secret for shop/)

  const file = join(b.dir, 'secrets.env')
  writeFileSync(file, 'TOKEN=tok-value\n')
  const imp = await pilot(b, ['secrets', 'import', file])
  assert.equal(imp.stdout, '')
  assert.equal(imp.stderr.includes('tok-value'), false, imp.stderr)
})

test('the credentials file keeps mode 0600 and leaves no temp file', async () => {
  const b = bed()
  assert.equal((await pilot(b, ['secrets', 'set', 'k', 'v'])).code, 0)
  const path = credentialsPath(b.env)
  // Counterfactual: a direct `writeFileSync` without the mode, or a crash
  // between the temp write and the rename, fails one of these two.
  assert.equal(statSync(path).mode & 0o777, 0o600)
  assert.deepEqual(readdirSync(dirname(path)), ['credentials'])
})

test('a write that fails partway leaves the previous file intact', () => {
  // In-process rather than spawned, because the sibling temp name carries the
  // writing process's pid and only this process knows its own. Taking that
  // name with a directory is the closest reachable stand-in for the disk
  // filling up or the machine losing power mid-write.
  const b = bed()
  setSecret('shop', 'first', 'one', b.env)
  const path = credentialsPath(b.env)
  const before = readFileSync(path, 'utf8')

  const tmpName = `${path}.${process.pid}.tmp`
  mkdirSync(tmpName)
  try {
    assert.throws(() => setSecret('shop', 'second', 'two', b.env))
  } finally {
    rmSync(tmpName, { recursive: true, force: true })
  }

  // Counterfactual: a store that wrote the file in place would have truncated
  // it before failing, taking the API key and `first` with it and logging the
  // user out.
  assert.equal(readFileSync(path, 'utf8'), before)
  const creds = loadCredentials(b.env)!
  assert.equal(creds.api_key, 'pilot_k')
  assert.equal(creds.secrets?.shop?.first, 'one')
  assert.equal(creds.secrets?.shop?.second, undefined)
})

test('set with no value reads stdin when stdin is not a terminal', () => {
  const b = bed()
  const res = pilotWithStdin(b, ['secrets', 'set', 'auth_secret'], 'from-stdin\n')
  assert.equal(res.code, 0, res.stderr)
  // The trailing newline is `echo`'s, not the secret's, and it is stripped
  // once. Counterfactual: keeping it makes every piped value wrong by one byte
  // and the failure only shows up in the deployed process.
  assert.equal(loadCredentials(b.env)!.secrets?.shop?.auth_secret, 'from-stdin')
})

test('set with no value and empty stdin refuses, naming every way to supply one', () => {
  const b = bed()
  const res = pilotWithStdin(b, ['secrets', 'set', 'auth_secret'], '')
  assert.equal(res.code, 1)
  assert.match(res.stderr, /no value on stdin for auth_secret/)
  assert.match(res.stderr, /PILOT_SECRET_AUTH_SECRET/)
  assert.equal(loadCredentials(b.env)!.secrets?.shop, undefined)
})

test('import stores every pair in the file and reports names only', async () => {
  const b = bed()
  const file = join(b.dir, 'app.env')
  writeFileSync(file, '# three secrets\nA=one\nB=two\nC=three\n')
  const res = await pilot(b, ['secrets', 'import', file])
  assert.equal(res.code, 0, res.stderr)
  assert.match(res.stderr, /stored 3 secrets for shop: A, B, C/)
  assert.equal(res.stdout, '')
  for (const value of ['one', 'two', 'three']) {
    assert.equal(res.stderr.includes(value), false, `${value} reached stderr`)
  }
  const stored = loadCredentials(b.env)!.secrets!.shop!
  assert.deepEqual(stored, { A: 'one', B: 'two', C: 'three' })
})

test('import - reads the same pairs from stdin', () => {
  const b = bed()
  const res = pilotWithStdin(b, ['secrets', 'import', '-'], 'A=one\nB=two\nC=three\n')
  assert.equal(res.code, 0, res.stderr)
  assert.deepEqual(loadCredentials(b.env)!.secrets!.shop!, { A: 'one', B: 'two', C: 'three' })
})

test('import merges rather than replacing the app\'s other secrets', async () => {
  const b = bed()
  assert.equal((await pilot(b, ['secrets', 'set', 'kept', 'yes'])).code, 0)
  const file = join(b.dir, 'more.env')
  writeFileSync(file, 'ADDED=also\n')
  assert.equal((await pilot(b, ['secrets', 'import', file])).code, 0)
  assert.deepEqual(loadCredentials(b.env)!.secrets!.shop!, { kept: 'yes', ADDED: 'also' })
})

test('import of a file with no pairs is refused and changes nothing', async () => {
  const b = bed()
  const file = join(b.dir, 'empty.env')
  writeFileSync(file, '# only a comment\n\n')
  const res = await pilot(b, ['secrets', 'import', file])
  assert.equal(res.code, 1)
  assert.match(res.stderr, /holds no KEY=value lines/)
  assert.equal(loadCredentials(b.env)!.secrets?.shop, undefined)
})

test('import of a missing file names the path', async () => {
  const b = bed()
  const res = await pilot(b, ['secrets', 'import', join(b.dir, 'nope.env')])
  assert.equal(res.code, 1)
  assert.match(res.stderr, /cannot read .*nope\.env/)
})

test('ls prints names and digests, never values', async () => {
  const b = bed()
  await pilot(b, ['secrets', 'set', 'zeta', 'z-value'])
  await pilot(b, ['secrets', 'set', 'alpha', 'a-value'])

  const res = await pilot(b, ['secrets', 'ls'])
  assert.equal(res.code, 0, res.stderr)
  const lines = res.stdout.trimEnd().split('\n')
  assert.match(lines[0]!, /^NAME\s+DIGEST$/)
  // Sorted, so two runs of the command are diffable.
  assert.deepEqual(
    lines.slice(1).map((l) => l.split(/\s+/)),
    [['alpha', digestOf('a-value')], ['zeta', digestOf('z-value')]],
  )
  for (const digest of [digestOf('a-value'), digestOf('z-value')]) {
    assert.match(digest, /^[0-9a-f]{8}$/)
  }
  // Counterfactual: printing the value instead of the digest is the obvious
  // implementation of `ls`, and it puts every secret in the scrollback of
  // anyone who runs it.
  assert.equal(res.stdout.includes('a-value'), false)
  assert.equal(res.stdout.includes('z-value'), false)
})

test('ls --json is the documented shape', async () => {
  const b = bed()
  await pilot(b, ['secrets', 'set', 'alpha', 'a-value'])
  const res = await pilot(b, ['--json', 'secrets', 'ls'])
  assert.equal(res.code, 0, res.stderr)
  assert.deepEqual(JSON.parse(res.stdout), {
    app: 'shop',
    secrets: [{ name: 'alpha', digest: digestOf('a-value') }],
  })
})

test('ls for an app with no secrets prints the header and nothing else', async () => {
  const b = bed()
  const res = await pilot(b, ['secrets', 'ls', '--app', 'nothing'])
  // Empty is a result, not an error.
  assert.equal(res.code, 0, res.stderr)
  assert.equal(res.stdout, 'NAME  DIGEST\n')
})

test('set and import refuse when not logged in, naming pilot login', async () => {
  const b = bed()
  b.env = { XDG_CONFIG_HOME: tmp('pilot-secrets-nologin-') }
  const file = join(b.dir, 'x.env')
  writeFileSync(file, 'A=one\n')

  for (const args of [['secrets', 'set', 'k', 'v'], ['secrets', 'import', file]]) {
    const res = await pilot(b, args)
    assert.equal(res.code, 1, res.stdout)
    assert.match(res.stderr, /pilot login/)
  }
  // Nothing was created: a secrets-only file has no API key, and the next
  // command would crash dereferencing it.
  assert.equal(existsSync(credentialsPath(b.env)), false)
})

test('an empty or whitespace name is refused', async () => {
  const b = bed()
  for (const name of ['', 'a b', 'a=b']) {
    const res = await pilot(b, ['secrets', 'set', name, 'v'])
    assert.equal(res.code, 1, `${JSON.stringify(name)} was accepted`)
    assert.match(res.stderr, /secret:\/\/<name>/)
  }
  assert.equal(loadCredentials(b.env)!.secrets?.shop, undefined)
})

test('a bad name is refused before the value is read', () => {
  // Counterfactual: validating inside the store, after `readValue`, makes the
  // user type the whole secret at the prompt (or hands the piped one to a
  // process that discards it) and only then refuses.
  const b = bed()
  const res = pilotWithStdin(b, ['secrets', 'set', 'a b'], 'never-read\n')
  assert.equal(res.code, 1)
  assert.match(res.stderr, /secret:\/\/<name>/)
  assert.equal(res.stderr.includes('never-read'), false)
  assert.equal(loadCredentials(b.env)!.secrets?.shop, undefined)
})

test('import refuses a key `set` would refuse, and stores nothing', async () => {
  // `parseEnv` hands back `A B` for a line whose key holds a space. A name
  // like that is one no secret:// reference can address and one `ls` renders
  // across two columns, so it is refused by the same rule `set` applies.
  const b = bed()
  const file = join(b.dir, 'bad.env')
  writeFileSync(file, 'GOOD=one\nA B=two\n')
  const res = await pilot(b, ['secrets', 'import', file])
  assert.equal(res.code, 1)
  assert.match(res.stderr, /secret:\/\/<name>/)
  assert.equal(res.stderr.includes('two'), false)
  assert.equal(loadCredentials(b.env)!.secrets?.shop, undefined)
})

test('promptSecret refuses without a TTY', async () => {
  // Under `node --test` stdin is never a terminal, so this is the branch a
  // script hits. The TTY branch is verified by hand on a pty; see the PR.
  await assert.rejects(promptSecret('Value: ', 'the-hint'), /not a terminal.*the-hint/)
})

/** The plan hostd returns for a compose file with three `secret://` values. */
function threeSecretPlan() {
  return {
    app: 'shop',
    steps: [
      {
        name: 'web',
        dockerfile: 'FROM scratch\n',
        replicas: 1,
        vcpus: 1,
        mem_mib: 512,
        env: { DEPLOY_ENV: 'staging' },
        secret_refs: {
          AUTH_SECRET: 'auth_secret',
          SESSION_SECRET: 'session_secret',
          FILE_URL_SECRET: 'file_url_secret',
        },
      },
    ],
  }
}

test('a deploy resolves three secrets stored by import, with no PILOT_SECRET_* set', async () => {
  const api: FakeAPI = await startFakeAPI()
  api.routes.set('POST /v1/compose/plan', (_req, res) => json(res, 200, threeSecretPlan()))
  const b = bed()
  saveCredentials({ api_key: 'pilot_k', org_id: 'org_1', api_url: api.url }, b.env)
  writeFileSync(join(b.dir, 'Dockerfile'), 'FROM scratch\n')
  writeFileSync(
    join(b.dir, 'prod.env'),
    'auth_secret=a-value\nsession_secret=s-value\nfile_url_secret=f-value\n',
  )
  try {
    assert.equal((await pilot(b, ['secrets', 'import', join(b.dir, 'prod.env')])).code, 0)

    // The environment is clean. This is the whole point of the issue: the
    // three values used to have to be on the deploy command line.
    const res = await pilot(b, ['--json', 'deploy'])
    assert.equal(res.code, 0, res.stderr)

    const body = JSON.parse(api.find('POST', '/v1/services')!.body) as Record<string, unknown>
    assert.deepEqual(body.secret_env, {
      AUTH_SECRET: 'a-value',
      SESSION_SECRET: 's-value',
      FILE_URL_SECRET: 'f-value',
    })
    // Counterfactual: a resolver that left them in `env` would store three
    // secrets unsealed on the replicated service row.
    assert.deepEqual(body.env, { DEPLOY_ENV: 'staging' })
  } finally {
    await api.close()
  }
})
