/**
 * The entry point runs with no build step.
 *
 * This is the acceptance criterion in executable form: `bin/pilot.js` is spawned
 * as a child process with no flags, which is how a user's shell runs it, so a
 * `.ts` file that needs a loader, a transpile or an `--experimental-` flag fails
 * here rather than on someone's machine.
 */

import { strict as assert } from 'node:assert'
import { execFile } from 'node:child_process'
import { join } from 'node:path'
import { test } from 'node:test'
import { promisify } from 'node:util'

const run = promisify(execFile)
const BIN = join(import.meta.dirname, '..', 'bin', 'pilot.js')

test('--version prints the package version with no flags and no build', async () => {
  const { stdout } = await run(process.execPath, [BIN, '--version'])
  assert.match(stdout.trim(), /^\d+\.\d+\.\d+$/)
})

test('--help names the program', async () => {
  const { stdout } = await run(process.execPath, [BIN, '--help'])
  assert.match(stdout, /Usage: pilot/)
})

test('an unknown command exits 1 with the error on stderr', async () => {
  await assert.rejects(
    run(process.execPath, [BIN, 'no-such-command']),
    (err: NodeJS.ErrnoException & { code?: number; stderr?: string }) => {
      assert.equal(err.code, 1)
      assert.match(String(err.stderr), /no-such-command/)
      return true
    },
  )
})
