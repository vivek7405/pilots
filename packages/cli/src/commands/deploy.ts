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
import { BuildFailedError } from '@pilots/sdk'

import { clientFromEnv, loadCredentials, type GlobalOptions } from '../config.ts'
import { loadDotEnv } from '../env.ts'
import { CliError, isJSONMode, note, printJSON, printTable, setPlain } from '../output.ts'
import { BuildReporter } from '../progress.ts'
import { collect, parseKeyValues } from '../resolve.ts'
import { findComposeFile } from '../compose/find.ts'
import { executePlan } from '../compose/run.ts'
import { tarDirectory } from '../tar.ts'

/** hostd caps the plan body; catching it here names the file rather than a 413. */
const MAX_COMPOSE_BYTES = 1024 * 1024

export function createDeployCommand(): Command {
  return new Command('deploy')
    .argument(
      '[dir]',
      'the directory to deploy; when a compose file is used, build contexts resolve against ' +
        "the compose file's own directory, which is not this one when --file points elsewhere",
      '.',
    )
    .description('build and deploy a directory: a compose file, a Dockerfile, or neither')
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
        // What the host decided this directory was. Human prose, so it is
        // skipped under `--json` for the same reason every other line here is:
        // stderr carries the build's NDJSON and nothing else in that mode.
        if (!isJSONMode()) {
          for (const d of res.detected) {
            note(`${d.service}: ${d.source}${d.framework ? ` (${d.framework})` : ''} in ${d.dir}`)
          }
        }
        plan = res.plan
      }
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
          // Named for the path actually taken: a directory deployed with no
          // compose file has no compose file to put x-pilots.domain in, and
          // sending someone to edit one that does not exist is worse than
          // saying nothing.
          const setDomain = file
            ? `set x-pilots.domain in the compose file, or pilot domains add <host> --service ${s.name}`
            : `pilot domains add <host> --service ${s.name}`
          const url = s.url || `(no domain: ${setDomain})`
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
