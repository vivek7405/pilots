/**
 * `pilot deploy`: a directory to running services.
 *
 * The CLI decides nothing. With a compose file it posts the text and the
 * `.env` map to `POST /v1/compose/plan`; without one it posts a tar of the
 * whole directory to `POST /v1/plan` and lets the host say what the directory
 * is. Either way it executes what comes back.
 *
 * One parser and one detector, in Go, beside the daemon. A JavaScript copy of
 * either would be a second implementation of the same rule, and the two would
 * disagree on the day it mattered. It is also what makes a directory with no
 * compose file and no Dockerfile deployable at all: the dashboard and the
 * GitHub push path call the same route.
 */

import { basename, dirname, resolve } from 'node:path'
import { readFileSync } from 'node:fs'

import { Command } from 'commander'
import type { BuildLogLine, ComposeStep } from '@pilots/sdk'

import { clientFromEnv, loadCredentials, type GlobalOptions } from '../config.ts'
import { loadDotEnv } from '../env.ts'
import { CliError, isJSONMode, note, printJSON, printTable } from '../output.ts'
import { collect, parseKeyValues } from '../resolve.ts'
import { findComposeFile } from '../compose/find.ts'
import { executePlan } from '../compose/run.ts'
import { tarDirectory } from '../tar.ts'

/** hostd caps the plan body; catching it here names the file rather than a 413. */
const MAX_COMPOSE_BYTES = 1024 * 1024

export function createDeployCommand(): Command {
  return new Command('deploy')
    .argument('[dir]', 'the directory to deploy', '.')
    .description('build and deploy a directory: a compose file, a Dockerfile, or neither')
    .option('--app <name>', 'override the app name the plan derives')
    .option('--env <K=V>', 'add to the interpolation environment (repeatable)', collect)
    .option('--no-wait', 'return as soon as each deploy is accepted')
    .option('--file <path>', 'use this compose file instead of searching')
    .action(async function (this: Command, dirArg: string) {
      const opts = this.optsWithGlobals() as GlobalOptions & Record<string, unknown>
      const dir = resolve(dirArg)
      const file = opts.file ? resolve(dir, opts.file as string) : findComposeFile(dir)
      const client = clientFromEnv(opts)

      let plan
      let composeDir = dir
      if (file) {
        const text = readFileSync(file, 'utf8')
        if (Buffer.byteLength(text) > MAX_COMPOSE_BYTES) {
          throw new CliError(`${file} is larger than the 1 MiB the plan route accepts`)
        }
        composeDir = dirname(file)

        // The `.env` FILE, never `process.env`. A deploy has to be
        // reproducible from the checkout, and a plan interpolated from
        // whatever happened to be exported would build a different app on
        // every machine.
        const env = { ...loadDotEnv(composeDir), ...parseKeyValues(opts.env as string[] | undefined) }
        plan = await client.compose.plan({ compose: text, env })
      } else {
        // `--env` is the compose interpolation environment and there is no
        // compose file here, so it has nothing to interpolate. Refused rather
        // than dropped: an argument that is quietly ignored deploys something
        // other than what was asked for and says nothing about it.
        if ((opts.env as string[] | undefined)?.length) {
          throw new CliError(
            `--env is the interpolation environment for a compose file, and ${dir} has none; ` +
              'put the values in a compose file, or set them on the service with `pilot services update`',
          )
        }
        // No compose file: the host decides what this directory is, from the
        // same tar the build would upload. Its `.env` is inside the tar, so
        // the planner reads it there rather than being handed a map.
        const res = await client.plan(new Uint8Array(tarDirectory(dir)), { app: basename(dir) })
        for (const d of res.detected) {
          note(`${d.service}: ${d.source}${d.framework ? ` (${d.framework})` : ''} in ${d.dir}`)
        }
        plan = res.plan
      }
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
