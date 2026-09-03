/**
 * The `.env` file.
 *
 * Parsed by `node:util`'s built-in `parseEnv`, which is the same parser Node
 * uses for `--env-file`. A `dotenv` dependency would be a second copy of a
 * contract the runtime already implements, and the two would drift on exactly
 * the quoting edge cases that matter.
 */

import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { parseEnv } from 'node:util'

/**
 * Reads `<dir>/.env`, or an empty map when there is none.
 *
 * Only the file. `process.env` never reaches the plan call: a deploy has to be
 * reproducible from the checkout, and a plan that interpolated whatever
 * happened to be exported would produce a different service on every machine.
 */
export function loadDotEnv(dir: string, filename = '.env'): Record<string, string> {
  let text: string
  try {
    text = readFileSync(join(dir, filename), 'utf8')
  } catch {
    return {}
  }
  const parsed = parseEnv(text) as Record<string, string | undefined>
  const out: Record<string, string> = {}
  for (const [key, value] of Object.entries(parsed)) {
    if (typeof value === 'string') out[key] = value
  }
  return out
}
