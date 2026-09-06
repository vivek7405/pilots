/**
 * Finding the skill.
 *
 * The same pages are three things at once: files an agent reads off disk after
 * `pilot init`, MCP resources under `pilots-docs://`, and the `docs` tool's
 * answers. One copy on disk serves all three, so a fix to a page cannot land
 * in one surface and miss the other two.
 *
 * Resolution order, first hit wins:
 *
 *   1. `.agents/skills/pilots/` under the working directory, which is what
 *      `pilot init` writes. An app's own copy wins, because a team that edited
 *      it meant to.
 *   2. `<package>/skill/pilots/`, which is both the source in a dev checkout
 *      and what ships in the npm tarball.
 *
 * One packaged path, not two. A `prepack` hook used to copy the source to
 * `resources/` and the tarball shipped both, which is a corpus that can go
 * stale against itself; worse, a publish with `--ignore-scripts` skipped the
 * hook and shipped no skill at all, so `pilot init` and the `pilots-docs://`
 * resources would have been empty for everyone who installed it.
 *
 * A directory counts only when it holds a `SKILL.md`, so a half-created
 * `.agents` tree does not shadow the packaged copy.
 */

import { existsSync, readFileSync, readdirSync } from 'node:fs'
import { dirname, join } from 'node:path'

const packageRoot = dirname(dirname(import.meta.dirname))

export interface SkillPage {
  /** `SKILL.md`, or `references/deploy.md`. */
  name: string
  path: string
}

/** The skill root, or null when no copy is reachable. */
export function skillRoot(cwd: string = process.cwd()): string | null {
  const candidates = [
    join(cwd, '.agents', 'skills', 'pilots'),
    join(packageRoot, 'skill', 'pilots'),
  ]
  for (const dir of candidates) {
    if (existsSync(join(dir, 'SKILL.md'))) return dir
  }
  return null
}

/** Every page in the skill: `SKILL.md` first, then the references, sorted. */
export function skillPages(cwd: string = process.cwd()): SkillPage[] {
  const root = skillRoot(cwd)
  if (!root) return []
  const pages: SkillPage[] = [{ name: 'SKILL.md', path: join(root, 'SKILL.md') }]
  const refs = join(root, 'references')
  if (existsSync(refs)) {
    for (const file of readdirSync(refs).sort()) {
      if (file.endsWith('.md')) pages.push({ name: `references/${file}`, path: join(refs, file) })
    }
  }
  return pages
}

/** The topics `docs` accepts, derived from the pages rather than listed twice. */
export function topics(cwd: string = process.cwd()): string[] {
  return skillPages(cwd)
    .filter((p) => p.name.startsWith('references/'))
    .map((p) => p.name.slice('references/'.length, -'.md'.length))
}

/** One reference page's text, or null when there is no such topic. */
export function readTopic(topic: string, cwd: string = process.cwd()): string | null {
  const page = skillPages(cwd).find((p) => p.name === `references/${topic}.md`)
  return page ? readFileSync(page.path, 'utf8') : null
}

/**
 * Pages whose text matches a query, as `{topic, excerpt}`.
 *
 * A plain case-insensitive substring search over the page text. Not an index:
 * nine pages is not a search problem, and a ranking function here would be a
 * second thing to keep correct for no gain a reader would notice.
 */
export function searchTopics(query: string, cwd: string = process.cwd()): { topic: string; excerpt: string }[] {
  const needle = query.toLowerCase()
  const out: { topic: string; excerpt: string }[] = []
  for (const page of skillPages(cwd)) {
    if (!page.name.startsWith('references/')) continue
    const text = readFileSync(page.path, 'utf8')
    const at = text.toLowerCase().indexOf(needle)
    if (at < 0) continue
    out.push({
      topic: page.name.slice('references/'.length, -'.md'.length),
      excerpt: text.slice(Math.max(0, at - 120), at + 240).trim(),
    })
  }
  return out
}
