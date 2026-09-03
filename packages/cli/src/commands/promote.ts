/**
 * `pilot promote`: the sandbox-to-production step.
 *
 * The whole point of one primitive serving both faces is that this changes
 * lifecycle policy and nothing else. In particular the URL does not change: a
 * promote that minted a new URL would break every link to the thing being
 * promoted, which is the opposite of what promoting it is for.
 */

import { Command } from 'commander'
import type { PromoteRequest } from '@pilots/sdk'

import { clientFromEnv, type GlobalOptions } from '../config.ts'
import { isJSONMode, printJSON, printTable } from '../output.ts'
import { resolveMachine } from '../resolve.ts'

export function createPromoteCommand(): Command {
  return new Command('promote')
    .argument('<machine>', 'the machine to promote, by id or name')
    .description('turn a sandbox into a durable service, keeping its URL')
    .option('--custom-domain <hostname>', 'also serve this hostname')
    .option('--replicas <n>', 'how many replicas to run', Number)
    .option('--health-path <path>', 'an HTTP path the rollout gate polls')
    .action(async function (this: Command, machine: string) {
      const opts = this.optsWithGlobals() as GlobalOptions & Record<string, unknown>
      const client = clientFromEnv(opts)
      const found = await resolveMachine(client, machine)
      const req: PromoteRequest = {
        ...(opts.customDomain ? { custom_domain: opts.customDomain as string } : {}),
        ...(opts.replicas !== undefined ? { replicas: opts.replicas as number } : {}),
        ...(opts.healthPath ? { health: { type: 'http', path: opts.healthPath as string } } : {}),
      }
      const service = await client.machines.promote(found.id, req)
      if (isJSONMode()) printJSON(service)
      else
        printTable([
          ['ID', 'NAME', 'REPLICAS', 'URL'],
          [service.id, service.name, String(service.replicas), service.custom_domain || service.url || ''],
        ])
    })
}
