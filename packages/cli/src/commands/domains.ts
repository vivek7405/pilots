/**
 * `pilot domains`: custom hostnames.
 *
 * `add` answers 201 when the CNAME already points at the fleet and 202 when it
 * does not yet. Both are success, and both print the target the customer's DNS
 * has to carry, because that is the only thing the user can act on.
 */

import { Command } from 'commander'

import { clientFromEnv, type GlobalOptions } from '../config.ts'
import { isJSONMode, note, printJSON, printTable } from '../output.ts'
import { resolveService } from '../resolve.ts'

export function createDomainsCommand(): Command {
  const domains = new Command('domains').alias('domain').description('attach custom hostnames to services')

  domains
    .command('add <hostname>')
    .description('point a hostname at a service')
    .requiredOption('--service <id|name>', 'the service to serve this hostname')
    .action(async function (this: Command, hostname: string) {
      const opts = this.optsWithGlobals() as GlobalOptions & { service: string }
      const client = clientFromEnv(opts)
      const service = await resolveService(client, opts.service)
      const result = await client.domains.add({ service_id: service.id, hostname })
      if (isJSONMode()) printJSON(result)
      else {
        note(
          result.verified
            ? `${result.hostname} is verified and serving`
            : `${result.hostname} is pending: point its CNAME at ${result.cname_target}`,
        )
      }
    })

  domains
    .command('ls')
    .alias('list')
    .description('list custom hostnames')
    .action(async function (this: Command) {
      const opts = this.optsWithGlobals() as GlobalOptions
      const client = clientFromEnv(opts)
      const list = await client.domains.list()
      if (isJSONMode()) printJSON(list)
      else
        printTable([
          ['HOSTNAME', 'SERVICE', 'VERIFIED', 'CNAME TARGET'],
          ...list.map((d) => [d.hostname, d.service_id, String(d.verified), d.cname_target]),
        ])
    })

  domains
    .command('rm <hostname>')
    .alias('remove')
    .description('detach a hostname')
    .action(async function (this: Command, hostname: string) {
      const opts = this.optsWithGlobals() as GlobalOptions
      const client = clientFromEnv(opts)
      await client.domains.remove(hostname)
      if (isJSONMode()) printJSON({ hostname, removed: true })
      else note(`removed ${hostname}`)
    })

  return domains
}
