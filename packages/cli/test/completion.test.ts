/**
 * The completion scripts.
 *
 * Generated from commander's own tree, so the assertion that matters is the
 * sweep: every top-level command the program registers appears in the script.
 * A command added to `main.ts` and missed by a hand-maintained list is exactly
 * what that sweep catches.
 */

import { strict as assert } from 'node:assert'
import { execFile } from 'node:child_process'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { after, test } from 'node:test'
import { promisify } from 'node:util'

import { generate, walk } from '../src/commands/completion.ts'
import { buildProgram } from '../src/main.ts'

const exec = promisify(execFile)
const roots: string[] = []
after(() => {
  for (const dir of roots) rmSync(dir, { recursive: true, force: true })
})

function scratchFile(name: string, content: string): string {
  const dir = mkdtempSync(join(tmpdir(), 'pilot-comp-'))
  roots.push(dir)
  const path = join(dir, name)
  writeFileSync(path, content)
  return path
}

test('the bash script parses and names every top-level command', async () => {
  const program = buildProgram()
  const script = generate(program, 'bash')
  const path = scratchFile('pilot.bash', script)
  // A generator that emits an unbalanced quote produces a script that breaks
  // the user's shell on the next login, silently until then.
  await exec('bash', ['-n', path])

  for (const cmd of program.commands) {
    assert.ok(script.includes(cmd.name()), `${cmd.name()} is missing from the bash script`)
  }
  assert.match(script, /complete -F _pilot pilot/)
})

test('zsh and fish are generated in their own shapes', () => {
  const program = buildProgram()
  const zsh = generate(program, 'zsh')
  assert.match(zsh, /^#compdef pilot/)
  assert.match(zsh, /compdef _pilot pilot/)

  const fish = generate(program, 'fish')
  for (const cmd of program.commands) {
    assert.ok(
      fish.includes(`-a '${cmd.name()}'`),
      `${cmd.name()} has no fish completion line`,
    )
  }
})

// Subcommands and aliases complete too: `pilot service ls` is the same command
// as `pilot services ls`, and a person who typed one expects the other to work.
test('the walk reaches subcommands and their aliases', () => {
  const paths = walk(buildProgram()).map((n) => n.path.join(' '))
  assert.ok(paths.includes('machines ls'))
  assert.ok(paths.includes('services ls'))
  assert.ok(paths.includes('service ls'), 'the services alias is not walked')
})

test('an unknown shell exits 1 and names the three', async () => {
  const BIN = join(import.meta.dirname, '..', 'bin', 'pilot.js')
  const res = await exec(process.execPath, [BIN, 'completion', 'elvish']).then(
    () => null,
    (err: { stderr?: string; code?: number }) => err,
  )
  assert.ok(res)
  assert.equal(res.code, 1)
  assert.match(res.stderr ?? '', /unknown shell elvish/)
  assert.match(res.stderr ?? '', /bash\|zsh\|fish/)
})

// The zsh script's own two landmines. `words` is the array zsh fills with the
// command line, so a `local words` blanks it and `words_` is not a variable at
// all; between them the path walk always came out empty and `pilot machines
// <TAB>` offered the top-level command list.
test('the zsh script reads zsh own words array and never shadows it', () => {
  const script = generate(buildProgram(), 'zsh')
  assert.doesNotMatch(script, /words_/, 'words_ is not a zsh variable')
  assert.doesNotMatch(script, /^\s*local .*\bwords\b/m, 'local words shadows the command-line array')
  assert.match(script, /word="\$\{words\[i\]\}"/)
  assert.match(script, /__pilot_words/)
})

// At the root the path is empty, and an UNQUOTED command substitution that
// prints nothing expands to zero arguments: fish would evaluate `test = ''`,
// which is not a valid test, so `pilot <TAB>` completed nothing at all.
test('the fish guard quotes the command substitution', () => {
  const script = generate(buildProgram(), 'fish')
  assert.match(script, /test "\(__fish_pilot_path\)" = /)
  assert.doesNotMatch(script, /test \(__fish_pilot_path\) = /)
})
