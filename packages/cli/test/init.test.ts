/**
 * `pilot init` and `pilot skill install`.
 *
 * Both are idempotent and neither is destructive, and that is what these
 * assert. An `init` that duplicated its stanza or took away another MCP
 * server's config entry would be a command people run once and then avoid,
 * which is the same as one that does not exist.
 */

import { strict as assert } from 'node:assert'
import { execFile } from 'node:child_process'
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { after, test } from 'node:test'

import { promisify } from 'node:util'

import { skillPages, skillRoot } from '../src/mcp/skill.ts'

const exec = promisify(execFile)
const BIN = join(import.meta.dirname, '..', 'bin', 'pilot.js')
const roots: string[] = []

after(() => {
  for (const dir of roots) rmSync(dir, { recursive: true, force: true })
})

function scratch(prefix: string): string {
  const dir = mkdtempSync(join(tmpdir(), prefix))
  roots.push(dir)
  return dir
}

async function pilot(args: string[], cwd: string): Promise<{ code: number; stdout: string; stderr: string }> {
  try {
    const { stdout, stderr } = await exec(process.execPath, [BIN, ...args], { cwd })
    return { code: 0, stdout, stderr }
  } catch (err) {
    const e = err as { stdout?: string; stderr?: string; code?: number }
    return { code: e.code ?? 1, stdout: e.stdout ?? '', stderr: e.stderr ?? '' }
  }
}

test('init writes the skill, both MCP configs and the AGENTS.md stanza', async () => {
  const dir = scratch('pilot-init-')
  const res = await pilot(['--json', 'init'], dir)
  assert.equal(res.code, 0, res.stderr)

  assert.ok(existsSync(join(dir, '.agents', 'skills', 'pilots', 'SKILL.md')))
  assert.ok(existsSync(join(dir, '.agents', 'skills', 'pilots', 'references', 'deploy.md')))

  for (const path of ['.claude.json', join('.cursor', 'mcp.json')]) {
    const config = JSON.parse(readFileSync(join(dir, path), 'utf8')) as {
      mcpServers: { pilots: { command: string; args: string[] } }
    }
    assert.equal(config.mcpServers.pilots.command, 'pilot')
    assert.deepEqual(config.mcpServers.pilots.args, ['mcp'])
  }

  const agents = readFileSync(join(dir, 'AGENTS.md'), 'utf8')
  assert.match(agents, /## Deploying with pilots/)
  assert.match(agents, /unknown_framework/)
})

test('a second init changes nothing and says so', async () => {
  const dir = scratch('pilot-init-twice-')
  await pilot(['init'], dir)
  const before = readFileSync(join(dir, 'AGENTS.md'), 'utf8')

  const res = await pilot(['--json', 'init'], dir)
  assert.equal(res.code, 0, res.stderr)
  const report = JSON.parse(res.stdout) as { written: string[]; already_present: string[] }
  assert.deepEqual(report.written, [])
  assert.equal(report.already_present.length, 4)

  // The stanza is not duplicated, which is the failure a naive append gives.
  assert.equal(readFileSync(join(dir, 'AGENTS.md'), 'utf8'), before)
})

test('init keeps another MCP server and the rest of AGENTS.md', async () => {
  const dir = scratch('pilot-init-merge-')
  writeFileSync(
    join(dir, '.claude.json'),
    JSON.stringify({ mcpServers: { other: { command: 'other-server' } }, somethingElse: 1 }),
  )
  writeFileSync(join(dir, 'AGENTS.md'), '# House rules\n\nRun the tests.\n')

  await pilot(['init'], dir)

  const config = JSON.parse(readFileSync(join(dir, '.claude.json'), 'utf8')) as {
    mcpServers: Record<string, unknown>
    somethingElse: number
  }
  // The file belongs to the user. Rewriting it would take away servers
  // nobody asked us to touch.
  assert.ok(config.mcpServers.other, "init removed another server's entry")
  assert.equal(config.somethingElse, 1)
  assert.ok(config.mcpServers.pilots)

  const agents = readFileSync(join(dir, 'AGENTS.md'), 'utf8')
  assert.match(agents, /# House rules/)
  assert.match(agents, /## Deploying with pilots/)
})

test('init refuses an MCP config that does not parse rather than guessing', async () => {
  const dir = scratch('pilot-init-broken-')
  writeFileSync(join(dir, '.claude.json'), '{ not json')

  const res = await pilot(['init'], dir)
  assert.equal(res.code, 1)
  assert.match(res.stderr, /not valid JSON/)
})

test('skill install refuses a real directory rather than deleting it', async () => {
  const home = scratch('pilot-home-')
  const target = join(home, '.claude', 'skills', 'pilots')
  mkdirSync(target, { recursive: true })
  writeFileSync(join(target, 'SKILL.md'), '# somebody edited this\n')

  const { stderr, code } = await (async () => {
    try {
      const { stdout, stderr } = await exec(process.execPath, [BIN, 'skill', 'install'], {
        cwd: home,
        env: { ...process.env, HOME: home },
      })
      return { code: 0, stdout, stderr }
    } catch (err) {
      const e = err as { stderr?: string; code?: number }
      return { code: e.code ?? 1, stderr: e.stderr ?? '' }
    }
  })()

  assert.equal(code, 1)
  assert.match(stderr, /is a directory, not a link/)
  // Still there, with its edits.
  assert.match(readFileSync(join(target, 'SKILL.md'), 'utf8'), /somebody edited this/)
})

// What `npm pack` would ship: the skill, exactly once, with no lifecycle hook
// standing between the source and the tarball.
//
// The published package used to list both `skill` and `resources` in `files`,
// so the corpus went out twice and the two could drift; and because
// `resources` only existed if `prepack` ran, a publish with `--ignore-scripts`
// shipped no skill at all, which breaks `pilot init` and every
// `pilots-docs://` resource for everyone who installed it.
test('the package ships exactly one copy of the skill, with no pack hooks', () => {
  const pkg = JSON.parse(
    readFileSync(join(import.meta.dirname, '..', 'package.json'), 'utf8'),
  ) as { files: string[]; scripts: Record<string, string> }

  assert.ok(pkg.files.includes('skill'), 'the skill is not in files')
  assert.ok(!pkg.files.includes('resources'), 'the skill ships twice')
  for (const hook of ['prepack', 'postpack', 'prepare']) {
    assert.equal(pkg.scripts[hook], undefined, `${hook} would decide whether the skill ships`)
  }
})

test('the skill resolves from the package with no working-directory copy', () => {
  // Resolved from a directory that has no `.agents` tree, which is what a
  // globally installed CLI sees on a machine that never ran `pilot init`.
  const root = skillRoot(mkdtempSync(join(tmpdir(), 'pilot-nocwd-')))
  assert.ok(root, 'the packaged skill is unreachable')
  assert.ok(existsSync(join(root!, 'SKILL.md')))
  assert.equal(skillPages(root!).length >= 10, true)
})
