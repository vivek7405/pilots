/**
 * A client inside a machine, with no key.
 */

import test from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { PilotsClient } from '../src/client.ts'
import { PilotsError } from '../src/errors.ts'

function tokenFile(contents: string): string {
  const path = join(mkdtempSync(join(tmpdir(), 'pilots-broker-')), 'token')
  writeFileSync(path, contents, { mode: 0o600 })
  return path
}

test('a client with no key uses the token the agent maintains', () => {
  const path = tokenFile('pbt1.abc.def\n')
  const previous = process.env.PILOT_TOKEN_FILE
  process.env.PILOT_TOKEN_FILE = path
  try {
    const client = new PilotsClient('', { baseURL: 'https://host-1.example.com' })
    assert.equal(client.apiKey, 'pbt1.abc.def', 'the trailing newline survived')
  } finally {
    if (previous === undefined) delete process.env.PILOT_TOKEN_FILE
    else process.env.PILOT_TOKEN_FILE = previous
  }
})

// An explicit key is an explicit choice. Silently preferring the machine's own
// token would make a process that passed a key act as something else, and the
// call would succeed against the wrong identity.
test('an explicit key is never replaced by the machine token', () => {
  const path = tokenFile('pbt1.machine.token')
  const previous = process.env.PILOT_TOKEN_FILE
  process.env.PILOT_TOKEN_FILE = path
  try {
    const client = new PilotsClient('pilot_operator_key', { baseURL: 'https://host-1.example.com' })
    assert.equal(client.apiKey, 'pilot_operator_key')
  } finally {
    if (previous === undefined) delete process.env.PILOT_TOKEN_FILE
    else process.env.PILOT_TOKEN_FILE = previous
  }
})

// Outside a machine, no key is still a mistake worth catching before any
// request: it would otherwise fail once per call with a 401 saying nothing.
test('no key and no machine token is still refused at construction', () => {
  const previous = process.env.PILOT_TOKEN_FILE
  delete process.env.PILOT_TOKEN_FILE
  try {
    assert.throws(() => new PilotsClient(''), PilotsError)
  } finally {
    if (previous !== undefined) process.env.PILOT_TOKEN_FILE = previous
  }
})
