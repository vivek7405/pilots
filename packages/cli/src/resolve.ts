/**
 * Turning what a person typed into an id.
 *
 * Every machine route is keyed by id, but nobody remembers an id. A name is
 * what the URL carries and what `pilot machines ls` prints, so every command
 * that takes a machine takes either.
 */

import type { Machine, PilotsClient, Service } from '@pilots/sdk'
import { NotFoundError } from '@pilots/sdk'

import { CliError } from './output.ts'

/**
 * Resolves a machine by id, falling back to a name.
 *
 * The id is tried first and the list second, because the id is exact and the
 * list is a scan. A 404 on the id lookup is not an error yet: with tenancy on,
 * a foreign id is a 404 too, and the name path is what a user with a name in
 * their hand deserves before being told it does not exist.
 */
export async function resolveMachine(client: PilotsClient, idOrName: string): Promise<Machine> {
  try {
    return await client.machines.get(idOrName)
  } catch (err) {
    if (!(err instanceof NotFoundError)) throw err
  }
  const machines = await client.machines.list()
  const named = machines.filter((m) => m.name === idOrName)
  if (named.length === 1) return named[0]!
  if (named.length > 1) {
    throw new CliError(
      `${named.length} machines are named ${idOrName}; use an id: ${named.map((m) => m.id).join(', ')}`,
    )
  }
  throw new CliError(`no machine with id or name ${idOrName}`)
}

/** The same, for services. */
export async function resolveService(client: PilotsClient, idOrName: string): Promise<Service> {
  try {
    return await client.services.get(idOrName)
  } catch (err) {
    if (!(err instanceof NotFoundError)) throw err
  }
  const services = await client.services.list()
  const named = services.filter((s) => s.name === idOrName)
  if (named.length === 1) return named[0]!
  if (named.length > 1) {
    throw new CliError(
      `${named.length} services are named ${idOrName}; use an id: ${named.map((s) => s.id).join(', ')}`,
    )
  }
  throw new CliError(`no service with id or name ${idOrName}`)
}

/**
 * Parses repeated `--env K=V` flags.
 *
 * A value containing `=` is kept whole (only the first separator splits), so a
 * `DATABASE_URL` with a query string survives.
 */
export function parseKeyValues(pairs: string[] | undefined): Record<string, string> {
  const out: Record<string, string> = {}
  for (const pair of pairs ?? []) {
    const eq = pair.indexOf('=')
    if (eq <= 0) throw new CliError(`expected KEY=VALUE, got ${pair}`)
    out[pair.slice(0, eq)] = pair.slice(eq + 1)
  }
  return out
}

/** Commander's collector for a repeatable option. */
export function collect(value: string, previous: string[] = []): string[] {
  return previous.concat([value])
}
