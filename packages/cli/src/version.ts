/**
 * The CLI's own version, read from `package.json` at startup.
 *
 * Read off disk rather than imported as a JSON module: an `import ... with
 * { type: 'json' }` makes Node print an ExperimentalWarning to stderr on some
 * releases, and stderr is a channel this CLI makes promises about (`pilot mcp`
 * routes every diagnostic there, and `--json` callers read it for errors).
 */

import { readFileSync } from 'node:fs'
import { join } from 'node:path'

function read(): string {
  try {
    const raw = readFileSync(join(import.meta.dirname, '..', 'package.json'), 'utf8')
    return (JSON.parse(raw) as { version?: string }).version ?? '0.0.0'
  } catch {
    return '0.0.0'
  }
}

export const VERSION: string = read()
