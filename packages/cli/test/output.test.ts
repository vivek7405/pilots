/**
 * The output contract's own unit tests.
 *
 * Everything here is about what reaches which channel, in which bytes. The
 * command-level tests in `commands.test.ts` prove the same rules end to end
 * through a spawned binary; these prove the helpers those rules rest on,
 * including the states a terminal cannot be put into from a test.
 */

import { strict as assert } from 'node:assert'
import { test } from 'node:test'

import { PilotsError } from '@pilots/sdk'

import { CliError, hintOf, isPlain, paint, renderError, setJSONMode, setPlain } from '../src/output.ts'

function withOutputState(fn: () => void): void {
  const wasPlain = isPlain()
  const noColor = process.env.NO_COLOR
  try {
    fn()
  } finally {
    setJSONMode(false)
    setPlain(wasPlain)
    if (noColor === undefined) delete process.env.NO_COLOR
    else process.env.NO_COLOR = noColor
  }
}

// A helper that consulted only isTTY would colour under --json, which is the
// one place a stray escape byte breaks a contract rather than a screen.
test('paint is plain off a TTY, and under --json, --ci or NO_COLOR', () => {
  withOutputState(() => {
    assert.equal(paint('red', 'x', false), 'x')
    assert.ok(paint('red', 'x', true).includes('\x1b[31m'))

    setJSONMode(true)
    assert.equal(paint('red', 'x', true), 'x')
    setJSONMode(false)

    setPlain(true)
    assert.equal(paint('red', 'x', true), 'x')
    setPlain(false)

    process.env.NO_COLOR = '1'
    assert.equal(paint('red', 'x', true), 'x')
  })
})

test('hintOf reads a hint off any error and ignores an empty one', () => {
  assert.equal(hintOf(new CliError('m', { hint: 'do the thing' })), 'do the thing')
  assert.equal(hintOf(new CliError('m')), undefined)
  assert.equal(hintOf(Object.assign(new Error('m'), { hint: 'attached later' })), 'attached later')
  assert.equal(hintOf(Object.assign(new Error('m'), { hint: '' })), undefined)
  assert.equal(hintOf(Object.assign(new Error('m'), { hint: 42 })), undefined)
  assert.equal(hintOf(undefined), undefined)
})

// The hint is a second line, not a longer sentence. Folding it into the
// message gives a reader nothing to skim and a test nothing to match.
test('a hint renders on its own line under the message, and never under --json', () => {
  withOutputState(() => {
    const err = new CliError('no API key', { hint: 'run pilot login, or set PILOT_API_KEY' })
    assert.equal(renderError(err), 'error: no API key\n→ run pilot login, or set PILOT_API_KEY')
    assert.equal(renderError(new CliError('no API key')), 'error: no API key')

    setJSONMode(true)
    assert.equal(renderError(err), '{"error":"no API key"}')
  })
})

// The merge seam between the server's `next` and the CLI's `hint`. Both are
// rendered on the same fall-through and both are a "-> do this" line, so the
// thing worth pinning is that exactly one appears, whichever sources exist.
test('exactly one next-step line renders, whichever source supplied it', () => {
  withOutputState(() => {
    const arrows = (text: string): number => text.split('\n').filter((l) => l.startsWith('→')).length

    // Server only: hostd answers a 401 with its own next step.
    const fromServer = new PilotsError('unauthorized', {
      status: 401,
      next: 'pass an API key: pilot login, or set PILOT_API_KEY',
    })
    assert.equal(arrows(renderError(fromServer)), 1)
    assert.match(renderError(fromServer), /→ pass an API key/)

    // Client only: a CliError never carries a server next.
    assert.equal(arrows(renderError(new CliError('no API key', { hint: 'run pilot login' }))), 1)

    // Both: the client's wins, because it says the same thing with the fleet
    // and the key source in it. Printing both is the same sentence twice.
    const both = new PilotsError('unauthorized', {
      status: 401,
      next: 'pass an API key: pilot login, or set PILOT_API_KEY',
    })
    ;(both as { hint?: string }).hint = 'http://f rejected the key from PILOT_API_KEY; run pilot login'
    assert.equal(arrows(renderError(both)), 1, 'a 401 with both sources printed two arrow lines')
    assert.match(renderError(both), /rejected the key from PILOT_API_KEY/)
    assert.doesNotMatch(renderError(both), /pass an API key/)

    // Neither: no arrow line at all.
    assert.equal(arrows(renderError(new PilotsError('boom', { status: 500 }))), 0)
  })
})
