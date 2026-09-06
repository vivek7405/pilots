/**
 * The webjs fixtures.
 *
 * They are built for real by the agent gate, inside a guest with network
 * egress. A third-party browser import would make the importmap step contact
 * api.jspm.io during that build, which turns a deploy assertion into a test of
 * somebody else's uptime. `@webjsdev/server` short-circuits an empty install
 * set, so the rule is: nothing but framework, subpath and node: specifiers.
 */

import { strict as assert } from 'node:assert'
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join } from 'node:path'
import { test } from 'node:test'

const FIXTURES = join(import.meta.dirname, 'fixtures')

function walk(dir: string): string[] {
  const out: string[] = []
  for (const entry of readdirSync(dir)) {
    const path = join(dir, entry)
    if (statSync(path).isDirectory()) out.push(...walk(path))
    else if (path.endsWith('.ts')) out.push(path)
  }
  return out
}

const SPECIFIER = /(?:^|\n)\s*import\s[^'"]*from\s*['"]([^'"]+)['"]/g

for (const fixture of ['webjs-app', 'workspace-app']) {
  test(`${fixture} imports nothing that would be fetched at build time`, () => {
    for (const file of walk(join(FIXTURES, fixture))) {
      const text = readFileSync(file, 'utf8')
      for (const match of text.matchAll(SPECIFIER)) {
        const specifier = match[1]!
        assert.ok(
          specifier.startsWith('@webjsdev/') || specifier.startsWith('#') || specifier.startsWith('node:'),
          `${file} imports ${specifier}; the build would reach api.jspm.io for it`,
        )
      }
    }
  })
}

test('the workspace fixture declares its two members', () => {
  const pkg = JSON.parse(readFileSync(join(FIXTURES, 'workspace-app', 'package.json'), 'utf8')) as {
    workspaces: string[]
  }
  assert.deepEqual(pkg.workspaces, ['web', 'admin'])
})
