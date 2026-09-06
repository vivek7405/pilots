/**
 * Prompts that never echo.
 *
 * `node:readline` with a `Writable` that discards everything as its output:
 * readline owns raw mode and line editing, and every character it would have
 * echoed goes nowhere. The prompt text itself goes to stderr, which is where
 * the README puts every prompt so `--json` stdout stays parseable.
 *
 * One file so a later prompt adds itself here rather than starting a second
 * mechanism, and so the whole package has exactly one place that reads a
 * terminal.
 */

import { createInterface } from 'node:readline'
import { Writable, type Readable } from 'node:stream'

import { CliError, isJSONMode } from './output.ts'

/**
 * Reads one line from a TTY without echoing it.
 *
 * Refuses when stdin is not a terminal: a caller that can accept a piped
 * value has to read stdin itself, and `hint` is what the refusal names.
 */
export function promptSecret(message: string, hint: string): Promise<string> {
  if (!process.stdin.isTTY) {
    return Promise.reject(new CliError(`stdin is not a terminal, so there is nothing to prompt on; ${hint}`))
  }
  return new Promise((resolve, reject) => {
    process.stderr.write(message)
    const muted = new Writable({ write: (_chunk, _encoding, callback) => callback() })
    const rl = createInterface({ input: process.stdin, output: muted, terminal: true })
    let answered = false
    rl.on('SIGINT', () => {
      rl.close()
      process.stderr.write('\n')
      // 130 is what a shell reports for the same interruption (README, exit codes).
      process.exit(130)
    })
    rl.on('close', () => {
      if (!answered) reject(new CliError(`no value entered; ${hint}`))
    })
    rl.question('', (answer) => {
      answered = true
      rl.close()
      process.stderr.write('\n')
      resolve(answer)
    })
  })
}

/**
 * Whether a destructive command should stop and ask.
 *
 * A pure function so the whole truth table is testable without a terminal.
 * The rule is that a script and an agent are never blocked on a question: `-y`
 * says so outright, `--json` means the caller is a program, and either stream
 * not being a terminal means there is nobody there to answer.
 */
export function shouldAsk(state: {
  yes?: boolean
  json?: boolean
  stdinTTY?: boolean
  stderrTTY?: boolean
}): boolean {
  if (state.yes || state.json) return false
  return Boolean(state.stdinTTY) && Boolean(state.stderrTTY)
}

export interface ConfirmStreams {
  input?: Readable & { isTTY?: boolean }
  output?: NodeJS.WritableStream
}

/**
 * Asks a yes/no question on stderr.
 *
 * Anything but `y` or `yes` is a no, end of input included: the safe answer to
 * a destructive question that nobody answered is not to do it.
 */
export function confirm(question: string, streams: ConfirmStreams = {}): Promise<boolean> {
  const input = streams.input ?? process.stdin
  const output = streams.output ?? process.stderr
  return new Promise((resolve) => {
    const rl = createInterface({ input, output })
    let answered = false
    rl.on('close', () => {
      if (!answered) resolve(false)
    })
    rl.question(`${question} [y/N] `, (answer) => {
      answered = true
      rl.close()
      resolve(/^y(es)?$/i.test(answer.trim()))
    })
  })
}

/**
 * Confirms a destructive action, or throws.
 *
 * Returns without asking whenever `shouldAsk` says nobody is there to answer,
 * which is what keeps `-y`, `--json` and every non-interactive caller working
 * exactly as they did before the confirmation existed.
 */
export async function confirmOrExit(
  question: string,
  opts: { yes?: boolean } = {},
  streams: ConfirmStreams = {},
): Promise<void> {
  const input = streams.input ?? process.stdin
  if (
    !shouldAsk({
      yes: opts.yes,
      json: isJSONMode(),
      stdinTTY: Boolean(input.isTTY),
      stderrTTY: Boolean(process.stderr.isTTY),
    })
  ) {
    return
  }
  if (!(await confirm(question, streams))) throw new CliError('cancelled')
}
