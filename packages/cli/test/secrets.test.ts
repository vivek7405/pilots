/**
 * `secret://` resolution.
 *
 * The rule under test is that a resolved value only ever lands in `secret_env`.
 * `env` is stored in the clear on the service row; `secret_env` is sealed. A
 * value that took the wrong one of those two paths is a password in plaintext
 * in the fleet's replicated state, which nothing downstream would notice.
 */

import { strict as assert } from 'node:assert'
import { test } from 'node:test'

import { CliError } from '../src/output.ts'
import { collectRefs, envVarFor, resolveSecrets } from '../src/compose/secrets.ts'

const credentials = {
  api_key: 'k',
  secrets: { shop: { postgres_password: 'from-file', database_url: 'postgres://from-file' } },
}

test('a name resolves from the environment first, then the credentials file', () => {
  const refs = { POSTGRES_PASSWORD: 'postgres_password', DATABASE_URL: 'database_url' }
  assert.deepEqual(resolveSecrets(refs, { app: 'shop', env: {}, credentials }), {
    POSTGRES_PASSWORD: 'from-file',
    DATABASE_URL: 'postgres://from-file',
  })
  assert.deepEqual(
    resolveSecrets(refs, {
      app: 'shop',
      env: { PILOT_SECRET_POSTGRES_PASSWORD: 'from-env' },
      credentials,
    }),
    { POSTGRES_PASSWORD: 'from-env', DATABASE_URL: 'postgres://from-file' },
  )
})

test('the environment variable name is the secret name, upper-cased and prefixed', () => {
  assert.equal(envVarFor('postgres_password'), 'PILOT_SECRET_POSTGRES_PASSWORD')
  assert.equal(envVarFor('api-token'), 'PILOT_SECRET_API_TOKEN')
})

test('secrets are scoped to the app, so another app\'s values do not leak in', () => {
  assert.throws(
    () => resolveSecrets({ K: 'postgres_password' }, { app: 'blog', env: {}, credentials }),
    /no value for secret postgres_password/,
  )
})

test('every missing name is listed at once', () => {
  assert.throws(
    () =>
      resolveSecrets({ A: 'alpha', B: 'beta', C: 'postgres_password' }, {
        app: 'shop',
        env: {},
        credentials,
      }),
    (err: unknown) => {
      assert.ok(err instanceof CliError)
      // Both, in one message: reporting one at a time means one failed deploy
      // per secret.
      assert.match(err.message, /alpha, beta/)
      assert.match(err.message, /PILOT_SECRET_ALPHA, PILOT_SECRET_BETA/)
      assert.equal(err.message.includes('postgres_password'), false)
      return true
    },
  )
})

test('no refs resolves to an empty map rather than failing', () => {
  assert.deepEqual(resolveSecrets(undefined, { app: 'shop', env: {}, credentials }), {})
  assert.deepEqual(resolveSecrets({}, { app: 'shop', env: {}, credentials }), {})
})

test('collectRefs gathers every name in the plan, deduplicated and sorted', () => {
  assert.deepEqual(
    collectRefs([
      { secret_refs: { A: 'beta', B: 'alpha' } },
      { secret_refs: { C: 'alpha' } },
      {},
    ]),
    ['alpha', 'beta'],
  )
})
