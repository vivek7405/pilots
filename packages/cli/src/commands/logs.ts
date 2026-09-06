/**
 * `pilot logs <service>`: every replica's output, fanned in.
 *
 * `pilot machines logs` already exists and stays as it is. The gap this fills
 * is that a service is what a person deployed, and it has more than one
 * replica, so reading its logs meant listing machines by hand first.
 *
 * Every line is prefixed with the replica's name, which is the only thing that
 * makes an interleaved stream readable.
 */

import { Command } from 'commander'
import type { Machine, PilotsClient } from '@pilots/sdk'

import { clientFromEnv, type GlobalOptions } from '../config.ts'
import { CliError, isJSONMode, printJSON } from '../output.ts'
import { resolveService } from '../resolve.ts'

export function createLogsCommand(): Command {
  return new Command('logs')
    .argument('<service>', 'a service, by id or name')
    .description("every replica's logs, each line prefixed with the replica name")
    .option('-f, --follow', 'stream new lines as they arrive')
    .action(async function (this: Command, service: string) {
      const opts = this.optsWithGlobals() as GlobalOptions & { follow?: boolean }
      const client = clientFromEnv(opts)
      const found = await resolveService(client, service)
      // Every state, not just running: a replica that crashed is exactly the
      // one whose logs are worth reading.
      const replicas = (await client.machines.list()).filter((m) => m.service_id === found.id)
      if (replicas.length === 0) {
        throw new CliError(`${found.name} has no replicas`, {
          hint: `pilot services info ${found.name} shows the replica count; pilot services set ${found.name} --replicas 1`,
        })
      }

      if (opts.follow) {
        await follow(client, found.name, replicas)
        return
      }
      const collected = await Promise.all(
        replicas.map(async (m) => ({ machine: m.id, name: m.name, logs: await client.machines.logs(m.id) })),
      )
      if (isJSONMode()) {
        printJSON({ service: found.name, machines: collected })
        return
      }
      for (const entry of collected) {
        for (const line of entry.logs.split('\n')) {
          if (line) process.stdout.write(`${entry.name} | ${line}\n`)
        }
      }
    })
}

/**
 * Follows every replica at once.
 *
 * Under `--json` each line is its own object, which is the shape an agent can
 * consume incrementally; one array at the end would mean waiting for a stream
 * that by definition does not end.
 */
async function follow(client: PilotsClient, service: string, replicas: Machine[]): Promise<void> {
  await Promise.all(
    replicas.map(async (m) => {
      for await (const line of client.machines.followLogs(m.id)) {
        if (!line) continue
        if (isJSONMode()) {
          process.stdout.write(JSON.stringify({ service, machine: m.id, name: m.name, line }) + '\n')
        } else {
          process.stdout.write(`${m.name} | ${line}\n`)
        }
      }
    }),
  )
}
