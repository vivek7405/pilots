/**
 * `parseDotEnv`: the one `.env` parser, pinned shape by shape.
 *
 * The parser is Node's own, so this table is not testing Node. It is the
 * contract `pilot secrets import` and `pilot deploy`'s interpolation file both
 * promise, written down so a runtime upgrade that changed a quoting rule fires
 * a test rather than a wrongly stored secret.
 *
 * Counterfactual: replacing `parseEnv` with a hand-rolled split-and-unquote
 * loop fails the `export`, inline-comment, padded-value, multi-line and
 * backslash-escape rows.
 */

import { strict as assert } from 'node:assert'
import { test } from 'node:test'

import { parseDotEnv } from '../src/env.ts'

const rows: [label: string, input: string, expected: Record<string, string>][] = [
  ['a comment and a blank line hold nothing', '# comment\n\n', {}],
  ['an unquoted value', 'PLAIN=one\n', { PLAIN: 'one' }],
  ['= inside a value belongs to the value', 'EQUALS=a=b=c\n', { EQUALS: 'a=b=c' }],
  ['double quotes are stripped', 'DQ="double quoted"\n', { DQ: 'double quoted' }],
  ['single quotes are stripped', "SQ='single quoted'\n", { SQ: 'single quoted' }],
  ['backticks are stripped', 'BT=`backtick`\n', { BT: 'backtick' }],
  ['a leading export is not part of the key', 'export EXPORTED=yes\n', { EXPORTED: 'yes' }],
  ['an inline comment after an unquoted value is dropped', 'INLINE=value # trailing\n', { INLINE: 'value' }],
  ['a # inside quotes is part of the value', 'DQ_HASH="has # inside"\n', { DQ_HASH: 'has # inside' }],
  ['an unquoted value is trimmed', 'SPACES=  padded  \n', { SPACES: 'padded' }],
  ['a key with no value is the empty string', 'EMPTY=\n', { EMPTY: '' }],
  ['a line with no = is dropped', 'NOEQ\n', {}],
  ['a quoted value spans lines', 'MULTI="line1\nline2"\n', { MULTI: 'line1\nline2' }],
  ['spaces around = are not part of the key', 'KEY_WITH_SPACE = spaced\n', { KEY_WITH_SPACE: 'spaced' }],
  ['a \\n escape inside double quotes is a newline', 'DQ_ESC="a\\nb"\n', { DQ_ESC: 'a\nb' }],
]

for (const [label, input, expected] of rows) {
  test(`parseDotEnv: ${label}`, () => {
    assert.deepEqual(parseDotEnv(input), expected)
  })
}

test('the whole table parses as one file, which is what an import reads', () => {
  const text = rows.map(([, input]) => input).join('')
  const expected = Object.assign({}, ...rows.map(([, , out]) => out)) as Record<string, string>
  assert.deepEqual(parseDotEnv(text), expected)
})

test('an empty file is an empty map rather than a throw', () => {
  assert.deepEqual(parseDotEnv(''), {})
})
