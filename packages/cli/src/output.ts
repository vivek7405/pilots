/**
 * The output contract, in one place because it is a promise to two audiences.
 *
 * A person reads a short table on stdout and a sentence on stderr. A program
 * -- the e2e battery, an agent, `#34`'s parity check -- passes `--json` and
 * reads the API's own response on stdout, byte for byte, with every diagnostic
 * on stderr. The two must never mix: a progress line on stdout breaks the
 * second audience silently, which is the same failure mode the MCP server
 * guards against, one channel over.
 */

import { PilotsError, QuotaExceededError, ComposePlanError, BuildFailedError } from '@pilots/sdk'

let jsonMode = false

/** Set once from the program's `preAction` hook, before any command runs. */
export function setJSONMode(on: boolean): void {
  jsonMode = on
}

export function isJSONMode(): boolean {
  return jsonMode
}

/**
 * An error the CLI itself raised, as opposed to one the server returned.
 *
 * Carries no status and no body, so `fail` renders it as a plain sentence
 * rather than pretending the fleet said something it did not.
 */
export class CliError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'CliError'
  }
}

/** stdout, two-space indented, exactly what a `--json` caller parses. */
export function printJSON(value: unknown): void {
  process.stdout.write(JSON.stringify(value, null, 2) + '\n')
}

/**
 * A column-aligned table on stdout, header row included by the caller.
 *
 * Two spaces between columns and no borders: the output is meant to survive
 * `grep` and `awk`, which a box-drawing frame does not.
 */
export function printTable(rows: string[][]): void {
  if (rows.length === 0) return
  const widths: number[] = []
  for (const row of rows) {
    row.forEach((cell, i) => {
      widths[i] = Math.max(widths[i] ?? 0, cell.length)
    })
  }
  for (const row of rows) {
    const line = row
      .map((cell, i) => (i === row.length - 1 ? cell : cell.padEnd(widths[i] ?? 0)))
      .join('  ')
      .trimEnd()
    process.stdout.write(line + '\n')
  }
}

/** stderr, never stdout: a note is not a result. */
export function note(message: string): void {
  process.stderr.write(message + '\n')
}

/**
 * Renders an error and exits 1.
 *
 * Under `--json` the server's body goes to stderr UNCHANGED. That is not a
 * stylistic choice: `#34` H8 compares the 429 body across the CLI, the SDK and
 * the MCP server byte for byte, and a re-serialisation here would make three
 * paths that agree look like three that do not.
 */
export function fail(err: unknown): never {
  process.stderr.write(renderError(err) + '\n')
  process.exit(1)
}

export function renderError(err: unknown): string {
  if (jsonMode) {
    if (err instanceof PilotsError && err.body) return err.body
    return JSON.stringify({ error: messageOf(err) })
  }

  if (err instanceof QuotaExceededError) {
    const scope = err.scope ? `, scope ${err.scope}` : ''
    return `error: ${err.message}: ${err.quota} (limit ${err.limit}, used ${err.used}${scope})`
  }
  if (err instanceof ComposePlanError) {
    // One line per rejected key: the whole point of the planner's loud refusal
    // is that the author sees every problem in one run, not the first one.
    const lines = err.unsupported.map((u) => `  ${u.service}.${u.key}: ${u.message}`)
    return [`error: ${err.error}`, ...lines].join('\n')
  }
  if (err instanceof BuildFailedError) {
    const last = err.lines[err.lines.length - 1]
    return `error: build ${err.buildId} failed: ${last?.error ?? err.message}`
  }
  return `error: ${messageOf(err)}`
}

function messageOf(err: unknown): string {
  return err instanceof Error ? err.message : String(err)
}
