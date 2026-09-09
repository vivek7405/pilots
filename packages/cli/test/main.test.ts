/**
 * The entry point runs with no build step.
 *
 * This is the acceptance criterion in executable form: `bin/pilot.js` is spawned
 * as a child process with no flags, which is how a user's shell runs it, so a
 * `.ts` file that needs a loader, a transpile or an `--experimental-` flag fails
 * here rather than on someone's machine.
 */

import { strict as assert } from 'node:assert'
import { execFile, spawn } from 'node:child_process'
import { mkdtemp, rm, symlink } from 'node:fs/promises'
import { tmpdir } from 'node:os'
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

test('the bin runs under the name it is installed as, not only as pilot.js', async () => {
  // Every other case here spawns `bin/pilot.js` by path, which is the one way
  // a user never runs it: `npm install -g` links `<prefix>/bin/pilot` to this
  // file and Node leaves argv[1] as that symlink. A CLI that decided whether
  // to run by matching argv[1] against `pilot.js` therefore exited 0 with no
  // output for every global install, with the whole battery green.
  const dir = await mkdtemp(join(tmpdir(), 'pilot-bin-'))
  try {
    const link = join(dir, 'pilot')
    await symlink(BIN, link)

    const { stdout } = await run(process.execPath, [link, '--version'])
    assert.match(stdout.trim(), /^\d+\.\d+\.\d+$/)
  } finally {
    await rm(dir, { recursive: true, force: true })
  }
})

test('a reader that closes stdout early ends the run quietly, with 141', async () => {
  // `completion bash` is the vehicle on purpose. It writes once and returns,
  // so the `error` event Node delivers on the NEXT tick still gets to run.
  // `--version` and `--help` are not vehicles: commander calls process.exit
  // synchronously after its write, the process is gone before the event
  // fires, and a case built on either passes against an entry point with no
  // listener at all. Destroying the read end before the child has started
  // Node puts the first write on a closed pipe, which takes the pipe buffer
  // and the size of the output out of the test.
  const child = spawn(process.execPath, [BIN, 'completion', 'bash'], {
    stdio: ['ignore', 'pipe', 'pipe'],
  })
  child.stdout.destroy()
  let stderr = ''
  child.stderr.on('data', (chunk) => (stderr += chunk))
  const code = await new Promise((resolve) => child.on('close', resolve))

  assert.equal(code, 141, `stderr was: ${stderr.slice(0, 400)}`)
  assert.equal(stderr, '')
})
