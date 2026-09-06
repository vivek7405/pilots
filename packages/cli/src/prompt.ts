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
import { Writable } from 'node:stream'

import { CliError } from './output.ts'

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
