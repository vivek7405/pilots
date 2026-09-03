/**
 * `pilot add postgres`.
 *
 * The fragment is compared against a golden file rather than described in
 * assertions, because the thing that matters is the exact YAML: an
 * `archive_command` that lost a `%f`, an `archive_timeout` that became a
 * string, or a `command` that collapsed into one shell word all produce a
 * Postgres that starts and quietly archives nothing.
 */

import { strict as assert } from 'node:assert'
import { execFile } from 'node:child_process'
import { cpSync, existsSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { after, test } from 'node:test'
import { promisify } from 'node:util'

import { loadCredentials, saveCredentials } from '../src/config.ts'
import { databaseURL, generatePassword, postgresFragment } from '../src/compose/postgres.ts'

const exec = promisify(execFile)
const BIN = join(import.meta.dirname, '..', 'bin', 'pilot.js')
const FIXTURES = join(import.meta.dirname, 'fixtures', 'postgres')
const roots: string[] = []

after(() => {
  for (const dir of roots) rmSync(dir, { recursive: true, force: true })
})

interface Bed {
  dir: string
  env: NodeJS.ProcessEnv
}

function bed(): Bed {
  const dir = mkdtempSync(join(tmpdir(), 'pilot-pg-'))
  roots.push(dir)
  cpSync(join(FIXTURES, 'compose.yaml'), join(dir, 'compose.yaml'))
  const cfg = mkdtempSync(join(tmpdir(), 'pilot-pg-cfg-'))
  roots.push(cfg)
  const env = { XDG_CONFIG_HOME: cfg }
  saveCredentials({ api_key: 'pilot_k', org_id: 'org_1', api_url: 'https://fleet' }, env)
  return { dir, env }
}

async function add(b: Bed, args: string[] = []) {
  try {
    const { stdout, stderr } = await exec(process.execPath, [BIN, 'add', 'postgres', '--dir', b.dir, ...args], {
      env: { ...b.env, PATH: process.env.PATH },
    })
    return { stdout, stderr, code: 0 }
  } catch (err) {
    const e = err as { stdout?: string; stderr?: string; code?: number }
    return { stdout: e.stdout ?? '', stderr: e.stderr ?? '', code: e.code ?? 1 }
  }
}

test('the default fragment matches the golden file and keeps the comment', async () => {
  const b = bed()
  const res = await add(b)
  assert.equal(res.code, 0, res.stderr)
  const written = readFileSync(join(b.dir, 'compose.yaml'), 'utf8')
  assert.equal(written, readFileSync(join(FIXTURES, 'expected-wal-archive.yaml'), 'utf8'))
  assert.match(written, /^# Two replicas because the health check is slow\./, 'the file\'s comment survived')
})

test('the generated Dockerfile and init script are written, the script executable', async () => {
  const b = bed()
  await add(b)
  const dockerfile = readFileSync(join(b.dir, '.pilots/postgres/Dockerfile'), 'utf8')
  assert.equal(dockerfile, 'FROM postgres:17\nCOPY 10-base-backup.sh /docker-entrypoint-initdb.d/\n')
  const script = join(b.dir, '.pilots/postgres/10-base-backup.sh')
  assert.match(readFileSync(script, 'utf8'), /pg_basebackup -U postgres -D \/archive\/base -Ft -z -X none/)
  // Dropped into docker-entrypoint-initdb.d, where the entrypoint runs it.
  assert.equal(statSync(script).mode & 0o111, 0o111)
})

test('--durable-volume writes the volume variant and no generated files', async () => {
  const b = bed()
  const res = await add(b, ['--durable-volume'])
  assert.equal(res.code, 0, res.stderr)
  assert.equal(
    readFileSync(join(b.dir, 'compose.yaml'), 'utf8'),
    readFileSync(join(FIXTURES, 'expected-durable-volume.yaml'), 'utf8'),
  )
  assert.equal(existsSync(join(b.dir, '.pilots')), false, 'the volume mode needs no image of its own')
})

test('the mode is recorded on the line that sets it', async () => {
  const b = bed()
  await add(b)
  const written = readFileSync(join(b.dir, 'compose.yaml'), 'utf8')
  // The architecture asks for the mode in use to be stated. A file someone
  // opens in six months has to say which trade it took.
  assert.match(written, /durable_volume: false # data dir local, WAL shipped .* \(RPO 60s\)/)
})

test('a second run refuses rather than appending a second service', async () => {
  const b = bed()
  await add(b)
  const before = readFileSync(join(b.dir, 'compose.yaml'), 'utf8')
  const res = await add(b)
  assert.equal(res.code, 1)
  assert.match(res.stderr, /service postgres already exists/)
  assert.equal(readFileSync(join(b.dir, 'compose.yaml'), 'utf8'), before, 'the file is untouched')
})

test('the password is 32 base64url characters, printed once, and stored under the app', async () => {
  const b = bed()
  const res = await add(b)
  const match = res.stderr.match(/PILOT_SECRET_POSTGRES_PASSWORD=(\S+)/)
  assert.ok(match, res.stderr)
  const password = match[1]!
  assert.equal(password.length, 32)
  assert.match(password, /^[A-Za-z0-9_-]{32}$/)

  // Once. Every later mention in the output is inside the DATABASE_URL line,
  // which is the same secret, not a second disclosure of the password alone.
  const occurrences = res.stderr.split(password).length - 1
  assert.equal(occurrences, 2, 'the password and the URL that embeds it, and nothing more')

  const creds = loadCredentials(b.env)!
  const app = b.dir.split('/').pop()!
  assert.equal(creds.secrets?.[app]?.postgres_password, password)
  assert.equal(
    creds.secrets?.[app]?.database_url,
    `postgres://postgres:${password}@postgres.internal:5432/postgres`,
  )
})

test('--json prints the service, the mode and both secrets once', async () => {
  const b = bed()
  const { stdout } = await exec(process.execPath, [BIN, '--json', 'add', 'postgres', '--dir', b.dir, '--app', 'shop'], {
    env: { ...b.env, PATH: process.env.PATH },
  })
  const parsed = JSON.parse(stdout) as {
    service: string
    mode: string
    app: string
    secrets: { postgres_password: string; database_url: string }
  }
  assert.equal(parsed.service, 'postgres')
  assert.equal(parsed.mode, 'wal-archive')
  assert.equal(parsed.app, 'shop')
  assert.equal(loadCredentials(b.env)!.secrets!.shop!.postgres_password, parsed.secrets.postgres_password)
})

test('--name adds a differently named service, and the URL follows it', async () => {
  const b = bed()
  const res = await add(b, ['--name', 'analytics-db'])
  assert.equal(res.code, 0, res.stderr)
  assert.match(readFileSync(join(b.dir, 'compose.yaml'), 'utf8'), /^ {2}analytics-db:$/m)
  // `.internal` resolves the service name with no app prefix (#26).
  assert.match(res.stderr, /@analytics-db\.internal:5432\/postgres/)
})

test('no compose file names the four filenames', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'pilot-pg-empty-'))
  roots.push(dir)
  const cfg = mkdtempSync(join(tmpdir(), 'pilot-pg-cfg2-'))
  roots.push(cfg)
  const res = await add({ dir, env: { XDG_CONFIG_HOME: cfg } })
  assert.equal(res.code, 1)
  assert.match(res.stderr, /compose\.yaml, compose\.yml, docker-compose\.yml, docker-compose\.yaml/)
})

test('a docker-compose.yml is found when there is no compose.yaml', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'pilot-pg-legacy-'))
  roots.push(dir)
  const cfg = mkdtempSync(join(tmpdir(), 'pilot-pg-cfg3-'))
  roots.push(cfg)
  writeFileSync(join(dir, 'docker-compose.yml'), 'services:\n  web:\n    build: .\n')
  const res = await add({ dir, env: { XDG_CONFIG_HOME: cfg } })
  assert.equal(res.code, 0, res.stderr)
  assert.match(readFileSync(join(dir, 'docker-compose.yml'), 'utf8'), /^ {2}postgres:$/m)
})

test('the fragment shape is the two documented modes and nothing else', () => {
  const wal = postgresFragment()
  assert.equal(wal.mode, 'wal-archive')
  assert.deepEqual(Object.keys(wal.files), ['.pilots/postgres/Dockerfile', '.pilots/postgres/10-base-backup.sh'])
  assert.deepEqual(wal.volumes, { pgarchive: {} })

  const durable = postgresFragment({ durableVolume: true })
  assert.equal(durable.mode, 'durable-volume')
  assert.deepEqual(durable.files, {})
  assert.equal(durable.service.command, undefined, 'the volume mode needs no archive settings')

  assert.equal(generatePassword().length, 32)
  assert.equal(
    databaseURL('pw'),
    'postgres://postgres:pw@postgres.internal:5432/postgres',
  )
})
