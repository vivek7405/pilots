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

import { styleText } from 'node:util'

import { PilotsError, QuotaExceededError, ComposePlanError, BuildFailedError } from '@pilots/sdk'

let jsonMode = false
let plain = false

/** Set once from the program's `preAction` hook, before any command runs. */
export function setJSONMode(on: boolean): void {
  jsonMode = on
}

export function isJSONMode(): boolean {
  return jsonMode
}

/**
 * Forces plain output: no colour, no line rewritten in place.
 *
 * Set by `--ci`, which exists because a build log read later is not a
 * terminal, and by anything else that wants the TTY rendering off while stderr
 * still happens to be one.
 */
export function setPlain(on: boolean): void {
  plain = on
}

export function isPlain(): boolean {
  return plain
}

/**
 * Colour for stderr prose, and nowhere else.
 *
 * Plain unless stderr is a terminal and nobody asked for plain, so a redirected
 * stderr, `--json`, `--ci` and `NO_COLOR` all get bytes a program can compare.
 * `validateStream` is off because the decision is made here, from the caller's
 * own `tty` argument, rather than from whichever stream styleText guesses at.
 */
export function paint(
  format: Parameters<typeof styleText>[0],
  text: string,
  tty: boolean = Boolean(process.stderr.isTTY),
): string {
  if (jsonMode || plain || !tty || process.env.NO_COLOR) return text
  return styleText(format, text, { validateStream: false })
}

/**
 * An error the CLI itself raised, as opposed to one the server returned.
 *
 * Carries no status and no body, so `fail` renders it as a plain sentence
 * rather than pretending the fleet said something it did not.
 */
export class CliError extends Error {
  /**
   * The next step, rendered as a second line under the message.
   *
   * Never part of the message itself. An error that reads "no API key: run
   * pilot login" gives a reader no structure to skim and a test nothing to
   * match; the two halves are separate so the rendering can put the fix where
   * the eye goes and a test can assert on it.
   */
  readonly hint: string | undefined

  constructor(message: string, opts: { hint?: string } = {}) {
    super(message)
    this.name = 'CliError'
    this.hint = opts.hint
  }
}

/**
 * The hint on any error, the CLI's own or one decorated at the boundary.
 *
 * Read through this accessor rather than the field, so a hint attached to an
 * SDK error (which has no `hint` in its type) renders the same way.
 */
export function hintOf(err: unknown): string | undefined {
  const h = (err as { hint?: unknown } | null | undefined)?.hint
  return typeof h === 'string' && h ? h : undefined
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
  const hint = hintOf(err)
  return hint ? `error: ${messageOf(err)}\n${paint('dim', '→')} ${hint}` : `error: ${messageOf(err)}`
}

export function messageOf(err: unknown): string {
  return err instanceof Error ? err.message : String(err)
}
