/**
 * The build context, as a POSIX ustar archive built in memory.
 *
 * Hand-rolled rather than shelled out to `tar(1)` or pulled from npm, the same
 * call `scripts/e2e.mjs:589` makes: the archive format is 512-byte headers and
 * an octal checksum, and a dependency here would be one bought for
 * convenience against a spec that has not moved since 1988.
 *
 * Three details are not decoration. Executable bits are carried from `stat`,
 * because `mke2fs -d` reads mode straight out of the tar header and a build
 * whose entrypoint script lost `+x` fails at boot rather than at build time.
 * Symlinks are emitted as links rather than followed, because following them
 * turns one `node_modules/.bin` tree into a copy of every target. And names
 * over 100 bytes use the `prefix` field, because silently truncating a path is
 * a file that vanishes from the image with no error anywhere.
 */

import { lstatSync, readdirSync, readFileSync, readlinkSync } from 'node:fs'
import { join, posix, relative, sep } from 'node:path'
import { matchesGlob } from 'node:path'

import { CliError } from './output.ts'

const BLOCK = 512

export interface IgnoreRule {
  pattern: string
  negate: boolean
}

/** Parses `.dockerignore` text into rules, in file order. */
export function parseDockerignore(text: string): IgnoreRule[] {
  const rules: IgnoreRule[] = []
  for (const raw of text.split('\n')) {
    const line = raw.trim()
    if (line === '' || line.startsWith('#')) continue
    const negate = line.startsWith('!')
    // A trailing slash means "this directory"; the matcher works on paths with
    // no trailing slash, so it is stripped rather than special-cased.
    const pattern = (negate ? line.slice(1) : line).replace(/^\.\//, '').replace(/\/+$/, '')
    if (pattern === '') continue
    rules.push({ pattern, negate })
  }
  return rules
}

/**
 * Whether a path is excluded. Last matching rule wins, and a rule that matches
 * any ANCESTOR of the path matches the path: excluding `node_modules` has to
 * exclude everything under it, which is what `.dockerignore` means and what a
 * plain glob against the full path does not do.
 */
export function isIgnored(relPath: string, rules: IgnoreRule[]): boolean {
  const parts = relPath.split('/')
  let excluded = false
  for (const rule of rules) {
    let matched = matchesGlob(relPath, rule.pattern)
    for (let i = 1; !matched && i < parts.length; i++) {
      matched = matchesGlob(parts.slice(0, i).join('/'), rule.pattern)
    }
    if (matched) excluded = !rule.negate
  }
  return excluded
}

export interface TarEntry {
  /** Path inside the archive, always POSIX-separated and never absolute. */
  name: string
  mode: number
  /** '0' file, '2' symlink, '5' directory. */
  type: '0' | '2' | '5'
  body?: Buffer
  linkTarget?: string
}

export interface TarOptions {
  /** Extra files layered on top of the directory, e.g. an agent's Dockerfile. */
  extraFiles?: Record<string, string>
  /** Ignore rules, read from `<dir>/.dockerignore` when not supplied. */
  rules?: IgnoreRule[]
}

/** Tars a directory into a build context. */
export function tarDirectory(dir: string, opts: TarOptions = {}): Buffer {
  const rules = opts.rules ?? readIgnoreRules(dir)
  const entries: TarEntry[] = []
  const extras = new Set(Object.keys(opts.extraFiles ?? {}))
  walk(dir, dir, rules, entries, extras)
  for (const [name, content] of Object.entries(opts.extraFiles ?? {})) {
    entries.push({ name, mode: 0o644, type: '0', body: Buffer.from(content, 'utf8') })
  }
  return writeTar(entries)
}

export function readIgnoreRules(dir: string): IgnoreRule[] {
  try {
    return parseDockerignore(readFileSync(join(dir, '.dockerignore'), 'utf8'))
  } catch {
    return []
  }
}

function walk(root: string, dir: string, rules: IgnoreRule[], out: TarEntry[], extras: Set<string>): void {
  let names: string[]
  try {
    names = readdirSync(dir).sort()
  } catch (err) {
    throw new CliError(`cannot read ${dir}: ${(err as Error).message}`)
  }
  for (const name of names) {
    const full = join(dir, name)
    const rel = relative(root, full).split(sep).join(posix.sep)
    // `.git` is skipped unconditionally: it is never part of a build context
    // and it is usually the largest thing in the tree.
    if (rel === '.git' || rel.startsWith('.git/')) continue
    // An extra file wins over whatever is on disk under the same name.
    if (extras.has(rel)) continue

    const st = lstatSync(full)
    const ignored = isIgnored(rel, rules)

    if (st.isDirectory()) {
      // A directory entry is emitted even when empty, so an image that expects
      // a mount point finds one.
      if (!ignored) out.push({ name: rel + '/', mode: st.mode & 0o777, type: '5' })
      if (ignored && !mightBeReincluded(rel, rules)) continue
      walk(root, full, rules, out, extras)
      continue
    }
    if (ignored) continue
    if (st.isSymbolicLink()) {
      out.push({ name: rel, mode: st.mode & 0o777, type: '2', linkTarget: readlinkSync(full) })
      continue
    }
    if (!st.isFile()) continue
    out.push({ name: rel, mode: st.mode & 0o777, type: '0', body: readFileSync(full) })
  }
}

/**
 * Whether descending into an excluded directory can still find something.
 *
 * Pruning is what keeps a `node_modules` out of the walk, not only out of the
 * archive. It is only safe when no `!` rule could match inside, so a rule
 * beginning with a glob, or one whose literal head points into this directory,
 * blocks the prune.
 */
function mightBeReincluded(relDir: string, rules: IgnoreRule[]): boolean {
  return rules.some((rule) => {
    if (!rule.negate) return false
    const head = rule.pattern.split('/')[0] ?? ''
    if (head.includes('*') || head.includes('?')) return true
    return rule.pattern === relDir || rule.pattern.startsWith(relDir + '/')
  })
}

/** Serialises entries into a ustar stream with the two-block end marker. */
export function writeTar(entries: TarEntry[]): Buffer {
  const blocks: Buffer[] = []
  for (const entry of entries) {
    const body = entry.type === '0' ? (entry.body ?? Buffer.alloc(0)) : Buffer.alloc(0)
    blocks.push(header(entry, body.length))
    if (body.length > 0) {
      blocks.push(body, Buffer.alloc((BLOCK - (body.length % BLOCK)) % BLOCK))
    }
  }
  blocks.push(Buffer.alloc(BLOCK * 2))
  return Buffer.concat(blocks)
}

function header(entry: TarEntry, size: number): Buffer {
  const { name, prefix } = splitName(entry.name)
  const buf = Buffer.alloc(BLOCK)
  buf.write(name, 0, 100, 'utf8')
  buf.write(octal(entry.mode & 0o7777, 7), 100, 8, 'utf8')
  buf.write(octal(0, 7), 108, 8, 'utf8')
  buf.write(octal(0, 7), 116, 8, 'utf8')
  buf.write(size.toString(8).padStart(11, '0') + ' ', 124, 12, 'utf8')
  // A fixed mtime rather than the file's. The build cache is keyed on content,
  // and a timestamp would make two identical trees hash differently.
  buf.write('00000000000 ', 136, 12, 'utf8')
  buf.write('        ', 148, 8, 'utf8')
  buf.write(entry.type, 156, 1, 'utf8')
  if (entry.linkTarget) buf.write(entry.linkTarget, 157, 100, 'utf8')
  buf.write('ustar\0' + '00', 257, 8, 'utf8')
  buf.write('root', 265, 32, 'utf8')
  buf.write('root', 297, 32, 'utf8')
  if (prefix) buf.write(prefix, 345, 155, 'utf8')

  let sum = 0
  for (const byte of buf) sum += byte
  buf.write(sum.toString(8).padStart(6, '0') + '\0 ', 148, 8, 'utf8')
  return buf
}

function octal(value: number, digits: number): string {
  return value.toString(8).padStart(digits, '0') + ' '
}

/**
 * Splits a long path across ustar's `name` and `prefix` fields.
 *
 * The split has to fall on a `/`: the reader rejoins them with one, so a split
 * mid-segment produces a different path than the one that went in.
 */
export function splitName(full: string): { name: string; prefix: string } {
  if (Buffer.byteLength(full) <= 100) return { name: full, prefix: '' }
  const parts = full.split('/')
  for (let i = 1; i < parts.length; i++) {
    const prefix = parts.slice(0, i).join('/')
    const name = parts.slice(i).join('/')
    if (Buffer.byteLength(name) <= 100 && Buffer.byteLength(prefix) <= 155) {
      return { name, prefix }
    }
  }
  throw new CliError(`path too long for a ustar archive (max 255 bytes): ${full}`)
}
