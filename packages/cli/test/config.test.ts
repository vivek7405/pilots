/**
 * The credentials file: where it lives, how it is written, and what wins.
 *
 * Every test runs under a temp `XDG_CONFIG_HOME` and passes its own `env`
 * object, so none of them can read or write the developer's real key.
 */

import { strict as assert } from 'node:assert'
import { chmodSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { after, test } from 'node:test'

import {
  clientFromEnv,
  clearCredentials,
  credentialsPath,
  loadCredentials,
  resolveApiUrl,
  saveCredentials,
} from '../src/config.ts'
import { CliError } from '../src/output.ts'

const roots: string[] = []

function scratch(): NodeJS.ProcessEnv {
  const dir = mkdtempSync(join(tmpdir(), 'pilot-cfg-'))
  roots.push(dir)
  return { XDG_CONFIG_HOME: dir }
}

after(() => {
  for (const dir of roots) rmSync(dir, { recursive: true, force: true })
})

test('credentialsPath honours XDG_CONFIG_HOME, then HOME', () => {
  assert.equal(credentialsPath({ XDG_CONFIG_HOME: '/x' }), '/x/pilots/credentials')
  assert.equal(credentialsPath({ HOME: '/home/h' }), '/home/h/.config/pilots/credentials')
})

test('saveCredentials creates the directory 0700 and the file 0600', () => {
  const env = scratch()
  const path = saveCredentials({ api_key: 'pilot_abc', org_id: 'org_1', api_url: 'https://f' }, env)
  assert.equal(statSync(path).mode & 0o777, 0o600)
  assert.equal(statSync(join(env.XDG_CONFIG_HOME!, 'pilots')).mode & 0o777, 0o700)
  assert.deepEqual(loadCredentials(env), {
    api_key: 'pilot_abc',
    org_id: 'org_1',
    api_url: 'https://f',
  })
  // No temp file is left behind by a successful write.
  assert.equal(readFileSync(path, 'utf8').endsWith('\n'), true)
})

test('loadCredentials refuses a file other users can read', () => {
  const env = scratch()
  const path = saveCredentials({ api_key: 'pilot_abc' }, env)
  // The counterfactual for the 0600 write: widen the mode and the refusal
  // fires. Without the check, this key would be readable by every user on a
  // shared box and nothing would say so.
  chmodSync(path, 0o644)
  assert.throws(() => loadCredentials(env), (err: unknown) => {
    assert.ok(err instanceof CliError)
    assert.match(err.message, /readable by other users/)
    assert.match(err.message, /pilots\/credentials/)
    return true
  })
})

test('loadCredentials returns null with no file and rejects invalid JSON', () => {
  const env = scratch()
  assert.equal(loadCredentials(env), null)
  saveCredentials({ api_key: 'k' }, env)
  const path = credentialsPath(env)
  writeFileSync(path, 'not json', { mode: 0o600 })
  assert.throws(() => loadCredentials(env), /not valid JSON/)
})

test('PILOT_API_KEY wins over the file', () => {
  const env = scratch()
  saveCredentials({ api_key: 'from_file' }, env)
  assert.equal(clientFromEnv({}, env).apiKey, 'from_file')
  assert.equal(clientFromEnv({}, { ...env, PILOT_API_KEY: 'from_env' }).apiKey, 'from_env')
})

test('--api-url wins over PILOT_API_URL, which wins over the file', () => {
  const env = scratch()
  saveCredentials({ api_key: 'k', api_url: 'https://file' }, env)
  assert.equal(resolveApiUrl({}, env), 'https://file')
  assert.equal(resolveApiUrl({}, { ...env, PILOT_API_URL: 'https://env' }), 'https://env')
  assert.equal(
    resolveApiUrl({ apiUrl: 'https://flag' }, { ...env, PILOT_API_URL: 'https://env' }),
    'https://flag',
  )
})

test('with no key from any source, clientFromEnv names `pilot login`', () => {
  const env = scratch()
  assert.throws(() => clientFromEnv({}, env), (err: unknown) => {
    assert.ok(err instanceof CliError)
    assert.match(err.message, /pilot login/)
    assert.match(err.message, /PILOT_API_KEY/)
    return true
  })
})

test('the default fleet is used when nothing names one', () => {
  const env = scratch()
  assert.equal(resolveApiUrl({}, env), 'https://api.pilotrun.app')
})

test('clearCredentials removes the file and reports whether there was one', () => {
  const env = scratch()
  assert.equal(clearCredentials(env), false)
  saveCredentials({ api_key: 'k' }, env)
  assert.equal(clearCredentials(env), true)
  assert.equal(loadCredentials(env), null)
})
