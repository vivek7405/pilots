/**
 * Static guards on `scripts/e2e.mjs`.
 *
 * The battery is the one file in this repository that CI never executes: it
 * exits early without `PILOTS_E2E=1`, and the full half needs a Firecracker
 * host. `node --check` parses it and nothing more, so a runtime error inside a
 * function that only runs on a real host reaches a host before it reaches
 * anybody.
 *
 * These are the cheap checks that would have caught the ones that got through.
 */

import { strict as assert } from 'node:assert'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { test } from 'node:test'

const BATTERY = join(import.meta.dirname, '..', '..', '..', 'scripts', 'e2e.mjs')
const source = readFileSync(BATTERY, 'utf8')

/**
 * A `let` declared in a `try` body is NOT in scope in that try's `finally`:
 * they are sibling blocks. Reading one from the cleanup throws a
 * `ReferenceError`, and because it happens in `finally` it REPLACES whatever
 * the battery was reporting, so a run that found a real failure reports a
 * scope error instead.
 *
 * Blocks are found by indentation, which is reliable here for the same reason
 * the SDK drift test relies on gofmt: this file's bodies are consistently
 * indented and its closing braces sit at a known column.
 */
test('no finally block reads a binding declared inside its own try', () => {
  const lines = source.split('\n')
  const problems: string[] = []

  for (let i = 0; i < lines.length; i++) {
    const opened = /^(\s*)try \{$/.exec(lines[i]!)
    if (!opened) continue
    const indent = opened[1]!

    // The try body runs to `} finally {` at the same indentation.
    let end = -1
    for (let j = i + 1; j < lines.length; j++) {
      if (lines[j] === `${indent}} finally {`) {
        end = j
        break
      }
      // A sibling block closed first: this try has no finally of its own.
      if (lines[j] === `${indent}}` || lines[j] === `${indent}} catch {`) break
    }
    if (end < 0) continue

    // Only the try's OWN body level. A binding inside a nested block or a
    // callback was never going to be visible in `finally` under any reading,
    // and matching one by name would flag every reused loop variable.
    const body = `${indent}  `
    const declared = new Set<string>()
    for (const line of lines.slice(i + 1, end)) {
      const decl = new RegExp(`^${body}(?:let|const|var)\\s+([A-Za-z_$][\\w$]*)`).exec(line)
      if (decl) declared.add(decl[1]!)
    }

    // The finally body runs to the closing brace at the try's indentation.
    let close = lines.length
    for (let j = end + 1; j < lines.length; j++) {
      if (lines[j] === `${indent}}`) {
        close = j
        break
      }
    }
    const cleanupLines = lines.slice(end + 1, close)
    // Comments stripped first: a cleanup that EXPLAINS what it leaves behind
    // mentions the names it is not touching, and matching prose would flag
    // every well-commented block.
    const cleanup = cleanupLines
      .map((line) => line.replace(/\/\/.*$/, ''))
      .join('\n')
      .replace(/\/\*[\s\S]*?\*\//g, '')
    // A name the cleanup declares itself, a `for (const id of ...)` most
    // often, shadows the try's and is not a read of it.
    const shadowed = new Set<string>()
    for (const line of cleanupLines) {
      for (const m of line.matchAll(/(?:let|const|var)\s+([A-Za-z_$][\w$]*)/g)) {
        shadowed.add(m[1]!)
      }
    }
    for (const name of declared) {
      if (shadowed.has(name)) continue
      if (new RegExp(`\\b${name}\\b`).test(cleanup)) {
        problems.push(`line ${end + 1}: finally reads ${name}, declared inside its try`)
      }
    }
  }

  assert.deepEqual(problems, [])
})
