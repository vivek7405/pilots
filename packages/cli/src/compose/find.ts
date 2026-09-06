/**
 * Finding the compose file.
 *
 * The order is uncloud's and docker compose's: `compose.yaml` first, because
 * that is the name the spec settled on, and the `docker-compose` names last
 * for the trees that predate it. First hit wins rather than "merge them all",
 * so a leftover `docker-compose.yml` beside a current `compose.yaml` cannot
 * silently deploy the wrong thing.
 */

import { existsSync, readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'

import { parse } from 'yaml'

import { loadDotEnv } from '../env.ts'
import { CliError } from '../output.ts'

export const COMPOSE_NAMES = ['compose.yaml', 'compose.yml', 'docker-compose.yml', 'docker-compose.yaml'] as const

export function findComposeFile(dir: string): string | null {
  for (const name of COMPOSE_NAMES) {
    const path = join(dir, name)
    if (existsSync(path)) return path
  }
  return null
}

/**
 * The app a compose file belongs to, by the rule hostd applies to the same
 * file (`internal/compose/plan.go` `appName`): `COMPOSE_PROJECT_NAME` from
 * the directory's `.env`, then top-level `x-pilots.app`, then top-level
 * `name:`. Client-side because `pilot secrets` and `pilot add` run with no
 * fleet in reach, and a name that differed from the plan's would store a
 * value under a key the deploy never reads.
 *
 * `extraEnv` is `--env`, which hostd sees because `deploy` merges it over the
 * `.env` file into the `env` it posts. A caller that left it out would derive
 * a different name from the same project for `deploy --env
 * COMPOSE_PROJECT_NAME=prod`, which is the bug this function exists to close.
 */
export function composeAppName(file: string, extraEnv: Record<string, string> = {}): string {
  const env = { ...loadDotEnv(dirname(file)), ...extraEnv }
  if (env.COMPOSE_PROJECT_NAME) return env.COMPOSE_PROJECT_NAME
  const doc = parse(readFileSync(file, 'utf8')) as Record<string, unknown> | null
  const xPilots = doc?.['x-pilots']
  if (xPilots && typeof xPilots === 'object') {
    const app = (xPilots as Record<string, unknown>).app
    if (typeof app === 'string' && app !== '') return app
  }
  if (typeof doc?.name === 'string' && doc.name !== '') return doc.name
  throw new CliError(
    `${file} has no app name: add a top-level name:, set COMPOSE_PROJECT_NAME in .env, or pass --app`,
  )
}
