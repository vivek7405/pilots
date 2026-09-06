/**
 * `composeAppName`: the client's copy of the rule hostd applies.
 *
 * The precedence is the thing under test, not the parsing. A client that
 * derived a different name from the same file would store a secret under a key
 * the deploy never reads, and the deploy would fail claiming the value was
 * never set.
 */

import { strict as assert } from 'node:assert'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { after, test } from 'node:test'

import { composeAppName } from '../src/compose/find.ts'
import { CliError } from '../src/output.ts'

const roots: string[] = []

after(() => {
  for (const dir of roots) rmSync(dir, { recursive: true, force: true })
})

/** A directory holding a compose file, and optionally a `.env` beside it. */
function bed(compose: string, dotenv?: string): string {
  const dir = mkdtempSync(join(tmpdir(), 'pilot-appname-'))
  roots.push(dir)
  const file = join(dir, 'compose.yaml')
  writeFileSync(file, compose)
  if (dotenv !== undefined) writeFileSync(join(dir, '.env'), dotenv)
  return file
}

test('COMPOSE_PROJECT_NAME in .env wins over x-pilots.app and name:', () => {
  // Counterfactual: swapping the first two checks returns `from-x-pilots`, and
  // the CLI then disagrees with the plan hostd returns for the same file.
  const file = bed('name: from-name\nx-pilots:\n  app: from-x-pilots\n', 'COMPOSE_PROJECT_NAME=from-env\n')
  assert.equal(composeAppName(file), 'from-env')
})

test('x-pilots.app wins over name: when there is no .env', () => {
  const file = bed('name: from-name\nx-pilots:\n  app: from-x-pilots\n')
  assert.equal(composeAppName(file), 'from-x-pilots')
})

test('name: is the last source', () => {
  assert.equal(composeAppName(bed('name: from-name\nservices:\n  web:\n    build: .\n')), 'from-name')
})

test('an empty value falls through to the next source', () => {
  // An empty string is not a name. Returning one would key the whole secret
  // store off `""` rather than refusing.
  assert.equal(composeAppName(bed('name: from-name\nx-pilots:\n  app: ""\n')), 'from-name')
  assert.equal(composeAppName(bed('name: from-name\n', 'COMPOSE_PROJECT_NAME=\n')), 'from-name')
})

test('a file with none of the three is refused naming all three', () => {
  const file = bed('services:\n  web:\n    build: .\n')
  assert.throws(
    () => composeAppName(file),
    (err: unknown) => {
      assert.ok(err instanceof CliError)
      assert.match(err.message, /top-level name:.*COMPOSE_PROJECT_NAME.*--app/)
      assert.ok(err.message.includes(file), 'the file that has no name is named')
      return true
    },
  )
})

test('an x-pilots block without an app falls through rather than throwing', () => {
  assert.equal(composeAppName(bed('name: from-name\nx-pilots:\n  other: 1\n')), 'from-name')
})
