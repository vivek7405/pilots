/**
 * The drift test.
 *
 * hostd's JSON tags are the contract (`internal/api/types.go:1-7`). This SDK
 * keeps a second copy of them, which is exactly the situation where a copy
 * rots silently: a field added to `Machine` in Go would simply never reach a
 * consumer, and nothing would be red.
 *
 * So the copy is checked against its source on every `npm test`: parse every
 * non-test Go file in the two packages that own wire shapes, parse
 * `src/types.ts`, and fail naming the struct and the tag when the two
 * disagree in either direction.
 */

import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, readdirSync, existsSync } from 'node:fs'
import { join } from 'node:path'

const repoRoot = join(import.meta.dirname, '..', '..', '..')
const apiDir = join(repoRoot, 'apps', 'hostd', 'internal', 'api')
// `internal/compose` arrives with #30. Until then there is nothing to walk;
// the day it lands, its structs are checked without touching this file.
const composeDir = join(repoRoot, 'apps', 'hostd', 'internal', 'compose')
const typesTS = join(import.meta.dirname, '..', 'src', 'types.ts')

/** Strips Go/TS line and block comments, so a comment cannot fake a field. */
function stripComments(src: string): string {
  return src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/\/\/.*$/gm, '')
}

interface GoStruct {
  name: string
  file: string
  tags: string[]
}

/**
 * Every `type X struct { ... }` in a directory, with its JSON tag names.
 *
 * gofmt puts the closing brace of a top-level struct at column 0, which is
 * what makes the block boundary reliable without a real parser.
 */
function parseGoStructs(dir: string, prefix = ''): GoStruct[] {
  const out: GoStruct[] = []
  for (const file of readdirSync(dir).sort()) {
    if (!file.endsWith('.go') || file.endsWith('_test.go')) continue
    const lines = stripComments(readFileSync(join(dir, file), 'utf8')).split('\n')
    for (let i = 0; i < lines.length; i++) {
      const open = /^type ([A-Za-z0-9_]+) struct \{/.exec(lines[i]!)
      if (!open) continue
      const tags: string[] = []
      for (i++; i < lines.length && lines[i] !== '}'; i++) {
        const tag = /json:"([^"]*)"/.exec(lines[i]!)
        if (!tag) continue
        const name = tag[1]!.split(',')[0]!
        if (name && name !== '-') tags.push(name)
      }
      // A struct with no tags is not a wire shape (api.Deps, for one).
      if (tags.length > 0) out.push({ name: prefix + open[1]!, file, tags })
    }
  }
  return out
}

/** Every `export interface X { ... }` in types.ts, with its property names. */
function parseTSInterfaces(file: string): Map<string, string[]> {
  const lines = stripComments(readFileSync(file, 'utf8')).split('\n')
  const out = new Map<string, string[]>()
  for (let i = 0; i < lines.length; i++) {
    const open = /^export interface ([A-Za-z0-9_]+) \{/.exec(lines[i]!)
    if (!open) continue
    const props: string[] = []
    for (i++; i < lines.length && lines[i] !== '}'; i++) {
      const prop = /^\s+([A-Za-z0-9_]+)\??\s*:/.exec(lines[i]!)
      if (prop) props.push(prop[1]!)
    }
    out.set(open[1]!, props)
  }
  return out
}

const goStructs = [
  ...parseGoStructs(apiDir),
  ...(existsSync(composeDir) ? parseGoStructs(composeDir, 'Compose') : []),
]
const tsInterfaces = parseTSInterfaces(typesTS)

test('the parsers found something to compare', () => {
  // A moved or renamed directory must fail loudly rather than pass vacuously
  // by finding nothing on either side.
  assert.ok(
    goStructs.length >= 20,
    `only ${goStructs.length} tagged structs found under ${apiDir}; the directory moved?`,
  )
  assert.ok(tsInterfaces.size >= 20, `only ${tsInterfaces.size} interfaces found in src/types.ts`)
})

test('src/types.ts mirrors every wire struct hostd serves', () => {
  const problems: string[] = []
  for (const s of goStructs) {
    const props = tsInterfaces.get(s.name)
    if (!props) {
      problems.push(`types.ts has no interface ${s.name} (hostd ${s.file}: ${s.tags.join(', ')})`)
      continue
    }
    const missing = s.tags.filter((t) => !props.includes(t))
    const extra = props.filter((p) => !s.tags.includes(p))
    if (missing.length || extra.length) {
      problems.push(
        `types.ts ${s.name} is missing [${missing.join(', ')}] / ` +
          `carries extra [${extra.join(', ')}] (hostd ${s.file})`,
      )
    }
  }
  assert.deepEqual(problems, [])
})

test('the frame constants match hostd', () => {
  const src = readFileSync(join(apiDir, 'types.go'), 'utf8')
  const ts = readFileSync(typesTS, 'utf8')
  const frames = [...src.matchAll(/^\s*(Frame[A-Za-z]+)\s+byte\s*=\s*(\d+)$/gm)]
  assert.ok(frames.length >= 3, 'no Frame constants found in hostd types.go')
  for (const [, name, value] of frames) {
    const mine = new RegExp(`^export const ${name} = (\\d+)$`, 'm').exec(ts)
    assert.ok(mine, `types.ts does not export ${name} (hostd types.go has it = ${value})`)
    assert.equal(mine[1], value, `types.ts ${name} is ${mine[1]}, hostd says ${value}`)
  }
})
