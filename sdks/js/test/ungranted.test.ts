/**
 * A machine that has been granted nothing.
 */

import test from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { PilotsClient } from '../src/client.ts'
import { PilotsError } from '../src/errors.ts'

function withTokenFile<T>(contents: string | null, fn: (path: string) => T): T {
  const dir = mkdtempSync(join(tmpdir(), 'pilots-ungranted-'))
  const path = join(dir, 'token')
  if (contents !== null) writeFileSync(path, contents, { mode: 0o600 })
  const previous = process.env.PILOT_TOKEN_FILE
  process.env.PILOT_TOKEN_FILE = path
  try {
    return fn(path)
  } finally {
    if (previous === undefined) delete process.env.PILOT_TOKEN_FILE
    else process.env.PILOT_TOKEN_FILE = previous
  }
}

/**
 * An ungranted machine is refused with a reason, not with a silent 401.
 *
 * machineCredential() returns a credential whenever PILOT_TOKEN_FILE is SET --
 * it does not read the file -- so on a machine granted nothing the
 * constructor's `!this.broker` guard is satisfied and every request went out
 * with `Authorization: Bearer ` and came back 401 with nothing saying why.
 * That is word for word the failure the constructor comment says it exists to
 * prevent, one environment variable away from where it looks for it.
 *
 * Removing the check in credential() makes this return an empty string.
 */
test('an ungranted machine refuses with a reason rather than an empty bearer', () => {
  withTokenFile('', () => {
    const client = new PilotsClient('', { baseURL: 'https://host-1.example.com' })
    assert.throws(
      () => client.apiKey,
      (err: unknown) => {
        assert.ok(err instanceof PilotsError, `threw ${err}`)
        assert.match(String(err), /not been granted/)
        assert.match(String(err), /PILOT_TOKEN_FILE/, 'the refusal names the file to look in')
        return true
      },
    )
  })
})

/**
 * A token file that does not exist yet is the same case: the guest agent
 * writes it during boot, so a client built before that is not yet wrong, but a
 * request made before it is.
 */
test('a token file that has not been written yet refuses the same way', () => {
  withTokenFile(null, () => {
    const client = new PilotsClient('', { baseURL: 'https://host-1.example.com' })
    assert.throws(() => client.apiKey, PilotsError)
  })
})

/**
 * And a granted machine is unaffected, so the refusal is the empty token and
 * not the check itself.
 */
test('a granted machine still reads its token', () => {
  withTokenFile('pbt1.granted.token\n', () => {
    const client = new PilotsClient('', { baseURL: 'https://host-1.example.com' })
    assert.equal(client.apiKey, 'pbt1.granted.token')
  })
})

/**
 * Whitespace only is nothing. The agent writes the file with a trailing
 * newline, so a file holding just that is an ungranted machine rather than a
 * token of one character.
 */
test('a token file holding only whitespace is ungranted', () => {
  withTokenFile('\n  \n', () => {
    const client = new PilotsClient('', { baseURL: 'https://host-1.example.com' })
    assert.throws(() => client.apiKey, PilotsError)
  })
})
