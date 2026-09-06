/**
 * prepack: put the skill where the published package serves it from.
 *
 * `skill/pilots/` is the source, and it is the copy a dev checkout reads.
 * `resources/pilots/` is what ships, and it carries a `corpus.json` naming the
 * version and the commit it came from, so a served page can be traced back to
 * the build that produced it.
 *
 * Two directories rather than publishing `skill/` directly, so the resolver
 * has one packaged path to look at and one source path, and a dev checkout
 * with a stale `resources/` still prefers what it just edited only when the
 * packaged copy is absent. `postpack` removes it, so the tree a developer
 * works in has exactly one copy.
 */
import { cpSync, existsSync, mkdirSync, rmSync, writeFileSync } from 'node:fs'
import { execSync } from 'node:child_process'
import { dirname, join } from 'node:path'
import { readFileSync } from 'node:fs'

const root = dirname(import.meta.dirname)
const source = join(root, 'skill', 'pilots')
const target = join(root, 'resources', 'pilots')

if (!existsSync(source)) {
  console.error(`copy-skill: ${source} is not there`)
  process.exit(1)
}

rmSync(join(root, 'resources'), { recursive: true, force: true })
mkdirSync(dirname(target), { recursive: true })
cpSync(source, target, { recursive: true })

const pkg = JSON.parse(readFileSync(join(root, 'package.json'), 'utf8'))
let sha = ''
try {
  sha = execSync('git rev-parse HEAD', { cwd: root, encoding: 'utf8' }).trim()
} catch {
  // A tarball built outside a checkout: the version still identifies it.
}
writeFileSync(
  join(root, 'resources', 'corpus.json'),
  JSON.stringify({ package: pkg.name, version: pkg.version, sha, copiedAt: new Date().toISOString() }, null, 2) + '\n',
)
