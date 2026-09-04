/**
 * Finding the compose file.
 *
 * The order is uncloud's and docker compose's: `compose.yaml` first, because
 * that is the name the spec settled on, and the `docker-compose` names last
 * for the trees that predate it. First hit wins rather than "merge them all",
 * so a leftover `docker-compose.yml` beside a current `compose.yaml` cannot
 * silently deploy the wrong thing.
 */

import { existsSync } from 'node:fs'
import { join } from 'node:path'

export const COMPOSE_NAMES = ['compose.yaml', 'compose.yml', 'docker-compose.yml', 'docker-compose.yaml'] as const

export function findComposeFile(dir: string): string | null {
  for (const name of COMPOSE_NAMES) {
    const path = join(dir, name)
    if (existsSync(path)) return path
  }
  return null
}
