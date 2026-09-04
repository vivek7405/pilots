/**
 * `pilot services`: the production face of the same primitive.
 *
 * `set` deliberately reads the service before it writes. `PATCH` REPLACES the
 * env and secret_env maps rather than merging into them, so a command that
 * sent only the pair the user typed would silently delete every other variable
 * the service had.
 */

import { Command } from 'commander'
import type { Service, UpdateServiceRequest } from '@pilots/sdk'

import { clientFromEnv, type GlobalOptions } from '../config.ts'
import { CliError, isJSONMode, note, printJSON, printTable } from '../output.ts'
import { collect, parseKeyValues, resolveService } from '../resolve.ts'

export function createServicesCommand(): Command {
  const services = new Command('services').alias('service').description('inspect and change services')

  services
    .command('ls')
    .alias('list')
    .description('list services')
    .option('--app <name>', 'only services in this app')
    .action(async function (this: Command) {
      const opts = this.optsWithGlobals() as GlobalOptions & { app?: string }
      const client = clientFromEnv(opts)
      const all = await client.services.list()
      const list = opts.app ? all.filter((s) => s.app === opts.app) : all
      if (isJSONMode()) printJSON(list)
      else printTable([serviceHeader(), ...list.map(serviceRow)])
    })

  services
    .command('info <service>')
    .description('show one service, by id or name')
    .action(async function (this: Command, service: string) {
      const opts = this.optsWithGlobals() as GlobalOptions
      const client = clientFromEnv(opts)
      const found = await resolveService(client, service)
      if (isJSONMode()) printJSON(found)
      else printTable([serviceHeader(), serviceRow(found)])
    })

  services
    .command('releases <service>')
    .description('list a service\'s releases, newest first')
    .action(async function (this: Command, service: string) {
      const opts = this.optsWithGlobals() as GlobalOptions
      const client = clientFromEnv(opts)
      const found = await resolveService(client, service)
      const releases = await client.services.releases(found.id)
      if (isJSONMode()) printJSON(releases)
      else
        printTable([
          ['ID', 'HEALTHY', 'ROOTFS'],
          ...releases.map((r) => [r.id, String(r.healthy), r.rootfs_build_id ?? '']),
        ])
    })

  services
    .command('rollback <service>')
    .description('return to the previous release')
    .action(async function (this: Command, service: string) {
      const opts = this.optsWithGlobals() as GlobalOptions
      const client = clientFromEnv(opts)
      const found = await resolveService(client, service)
      const release = await client.services.rollback(found.id)
      if (isJSONMode()) printJSON(release)
      else note(`rolled back to release ${release.id}`)
    })

  services
    .command('set <service>')
    .description('change replicas, env, secrets or the connected repo')
    .option('--replicas <n>', 'how many replicas the autoscaler reconciles to', Number)
    .option('--env <K=V>', 'set an environment variable (repeatable)', collect)
    .option('--unset-env <KEY>', 'remove an environment variable (repeatable)', collect)
    .option('--secret-env <K=V>', 'REPLACE the sealed environment with these pairs (repeatable)', collect)
    .option('--repo <owner/name>', 'the GitHub repo to deploy from')
    .option('--branch <name>', 'the branch to deploy')
    .option('--autodeploy [bool]', 'deploy on every push to the branch')
    .action(async function (this: Command, service: string) {
      const opts = this.optsWithGlobals() as GlobalOptions & Record<string, unknown>
      const client = clientFromEnv(opts)
      const found = await resolveService(client, service)

      const req: UpdateServiceRequest = {}
      if (opts.replicas !== undefined) req.replicas = opts.replicas as number
      if (opts.repo !== undefined) req.repo = opts.repo as string
      if (opts.branch !== undefined) req.branch = opts.branch as string
      // Every spelling of "no", not only the literal `false`: a bare
      // `--autodeploy` means on, and `--autodeploy no` meaning ON is a push
      // that ships to production the next time someone commits.
      if (opts.autodeploy !== undefined) {
        req.autodeploy = !/^(false|no|off|0)$/i.test(String(opts.autodeploy))
      }

      if (opts.env || opts.unsetEnv) {
        // Merged onto what the service already has, because PATCH replaces the
        // whole map. The current values are not on the Service object, so this
        // is the honest limit of the command: it can only merge what the
        // caller passes plus what the caller keeps.
        const current = (found as Service & { env?: Record<string, string> }).env ?? {}
        const next = { ...current, ...parseKeyValues(opts.env as string[] | undefined) }
        for (const key of (opts.unsetEnv as string[] | undefined) ?? []) delete next[key]
        req.env = next
      }
      if (opts.secretEnv) {
        // Never merged with anything read back: a sealed value is not readable,
        // so there is nothing to merge with. Passing --secret-env replaces the
        // sealed map, and the help text says so.
        req.secret_env = parseKeyValues(opts.secretEnv as string[])
      }
      if (Object.keys(req).length === 0) {
        throw new CliError('nothing to set')
      }

      const updated = await client.services.patch(found.id, req)
      if (isJSONMode()) printJSON(updated)
      else printTable([serviceHeader(), serviceRow(updated)])
    })

  return services
}

function serviceHeader(): string[] {
  return ['ID', 'NAME', 'APP', 'REPLICAS', 'RELEASE', 'URL']
}

function serviceRow(s: Service): string[] {
  return [
    s.id,
    s.name,
    s.app ?? '',
    String(s.replicas),
    s.release_id ?? '',
    s.custom_domain || s.url || '',
  ]
}
