/**
 * `pilot deploy`: a compose file to running services.
 *
 * The CLI does NO interpolation. It posts the file's text and the `.env` map
 * to `POST /v1/compose/plan` and executes what comes back. One compose parser,
 * in Go, beside the daemon: a JavaScript one here would be a second
 * implementation of a specification, and the two would disagree on the day it
 * mattered.
 */

import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'

import { Command } from 'commander'
import type { BuildLogLine, ComposeStep } from '@pilots/sdk'

import { clientFromEnv, loadCredentials, type GlobalOptions } from '../config.ts'
import { loadDotEnv } from '../env.ts'
import { CliError, isJSONMode, note, printJSON, printTable } from '../output.ts'
import { collect, parseKeyValues } from '../resolve.ts'
import { COMPOSE_NAMES, findComposeFile } from '../compose/find.ts'
import { executePlan } from '../compose/run.ts'

/** hostd caps the plan body; catching it here names the file rather than a 413. */
const MAX_COMPOSE_BYTES = 1024 * 1024

export function createDeployCommand(): Command {
  return new Command('deploy')
    .argument('[dir]', 'the directory holding the compose file', '.')
    .description('build and deploy every service in a compose file')
    .option('--app <name>', 'override the app name the plan derives')
    .option('--env <K=V>', 'add to the interpolation environment (repeatable)', collect)
    .option('--no-wait', 'return as soon as each deploy is accepted')
    .option('--file <path>', 'use this compose file instead of searching')
    .action(async function (this: Command, dirArg: string) {
      const opts = this.optsWithGlobals() as GlobalOptions & Record<string, unknown>
      const dir = resolve(dirArg)
      const file = opts.file ? resolve(dir, opts.file as string) : findComposeFile(dir)
      if (!file) {
        throw new CliError(`no compose file in ${dir}: looked for ${COMPOSE_NAMES.join(', ')}`)
      }

      const text = readFileSync(file, 'utf8')
      if (Buffer.byteLength(text) > MAX_COMPOSE_BYTES) {
        throw new CliError(`${file} is larger than the 1 MiB the plan route accepts`)
      }
      const composeDir = dirname(file)

      // The `.env` FILE, never `process.env`. A deploy has to be reproducible
      // from the checkout, and a plan interpolated from whatever happened to
      // be exported would build a different app on every machine.
      const env = { ...loadDotEnv(composeDir), ...parseKeyValues(opts.env as string[] | undefined) }

      const client = clientFromEnv(opts)
      const plan = await client.compose.plan({ compose: text, env })
      if (opts.app) plan.app = opts.app as string

      const result = await executePlan(client, plan, {
        dir: composeDir,
        credentials: loadCredentials(),
        wait: opts.wait !== false,
        onBuildLine: (step, line) => printBuildLine(step, line),
      })

      if (isJSONMode()) printJSON(result)
      else printTable([['SERVICE', 'URL'], ...result.services.map((s) => [s.name, s.url])])
    })
}

/**
 * Build output goes to stderr in both modes.
 *
 * Under `--json` stdout carries one object, the result, so a caller can parse
 * it without stripping a log first; the NDJSON is forwarded to stderr
 * unchanged so an agent reading a failure still has every line.
 */
function printBuildLine(step: ComposeStep, line: BuildLogLine): void {
  if (isJSONMode()) {
    process.stderr.write(JSON.stringify(line) + '\n')
    return
  }
  const text = line.error ?? line.line ?? ''
  if (!text) return
  note(`${step.name}${line.step ? ` ${line.step}` : ''} | ${text.replace(/\n$/, '')}`)
}
