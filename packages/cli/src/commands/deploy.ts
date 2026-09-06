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
import { BuildFailedError } from '@pilots/sdk'

import { clientFromEnv, loadCredentials, type GlobalOptions } from '../config.ts'
import { loadDotEnv } from '../env.ts'
import { CliError, isJSONMode, note, printJSON, printTable, setPlain } from '../output.ts'
import { BuildReporter } from '../progress.ts'
import { collect, parseKeyValues } from '../resolve.ts'
import { COMPOSE_NAMES, findComposeFile } from '../compose/find.ts'
import { executePlan } from '../compose/run.ts'

/** hostd caps the plan body; catching it here names the file rather than a 413. */
const MAX_COMPOSE_BYTES = 1024 * 1024

export function createDeployCommand(): Command {
  return new Command('deploy')
    .argument(
      '[dir]',
      "where to look for the compose file; build contexts resolve against the compose file's own " +
        'directory, which is not this one when --file points elsewhere',
      '.',
    )
    .description('build and deploy every service in a compose file')
    .option('--app <name>', 'override the app name the plan derives')
    .option('--env <K=V>', 'add to the interpolation environment (repeatable)', collect)
    .option('--no-wait', 'return as soon as each deploy is accepted')
    .option('-d, --detach', 'the same as --no-wait, under the name railway uses')
    .option('--file <path>', 'use this compose file instead of searching; relative to [dir]')
    .option('--verbose', 'stream every build log line instead of one line per stage')
    .option('-c, --ci', 'every log line, no colour, no live line; implied by a truthy CI variable')
    .action(async function (this: Command, dirArg: string) {
      const opts = this.optsWithGlobals() as GlobalOptions & Record<string, unknown>
      // railway's own equivalence: a truthy CI variable means --ci. A build log
      // read back later is not a terminal, so it gets neither colour nor a line
      // rewritten in place.
      const ci = Boolean(opts.ci) || /^(1|true|yes)$/i.test(process.env.CI ?? '')
      if (ci) setPlain(true)
      const verbose = Boolean(opts.verbose) || ci
      const wait = opts.wait !== false && !opts.detach
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
      if (!isJSONMode()) {
        note(`plan: ${plan.steps.length} services in app ${plan.app}`)
      }

      const reporter = new BuildReporter({
        verbose,
        tty: !ci && Boolean(process.stderr.isTTY),
      })
      let result
      try {
        result = await executePlan(client, plan, {
          dir: composeDir,
          credentials: loadCredentials(),
          wait,
          onBuildLine: (step, line) => reporter.line(step, line),
          onEvent: (step, message) => {
            reporter.clearLive()
            if (!isJSONMode()) note(`${step.name}  ${message}`)
          },
        })
      } catch (err) {
        reporter.clearLive()
        // The error names the build; only the buffered output says what the
        // failing step actually printed before it stopped. Which SERVICE that
        // was is the reporter's to know: the plan's first step is not the one
        // that was building when a later service failed.
        if (err instanceof BuildFailedError) reporter.failed(err)
        throw err
      }

      if (isJSONMode()) {
        printJSON(result)
        return
      }
      const header = wait ? ['SERVICE', 'URL'] : ['SERVICE', 'RELEASE', 'URL']
      printTable([
        header,
        ...result.services.map((s) => {
          // An empty column reads as a failed deploy. It is not: a service gets
          // a URL only when it has a domain, and the replicas answer at their
          // own machine URLs either way.
          const url =
            s.url ||
            `(no domain: set x-pilots.domain in the compose file, or pilot domains add <host> --service ${s.name})`
          return wait ? [s.name, url] : [s.name, s.release_id, url]
        }),
      ])
      if (!wait) {
        note('accepted, not waited for; pilot services releases <name> shows when a release is current')
      }
      if (result.services.some((s) => !s.url)) {
        note(`replicas answer at their own URLs meanwhile: pilot machines ls --app ${result.app}`)
      }
    })
}
