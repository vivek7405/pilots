/**
 * `pilot volumes`: durable, per-write storage.
 *
 * A machine with a volume BOOTS rather than restoring, so a volume is a
 * deliberate trade rather than a default: it buys per-write durability and
 * costs the instant start. The help text says so, because the cost is not
 * visible from the command.
 */

import { Command } from 'commander'

import { clientFromEnv, type GlobalOptions } from '../config.ts'
import { isJSONMode, printJSON, printTable } from '../output.ts'

export function createVolumesCommand(): Command {
  const volumes = new Command('volumes').alias('volume').description('durable storage, per-write')

  volumes
    .command('create <name>')
    .description('create a volume (a machine holding one boots rather than restores)')
    .requiredOption('--size-gib <n>', 'size in GiB', Number)
    .option('--mount-path <path>', 'where the guest sees it', '/data')
    .action(async function (this: Command, name: string) {
      const opts = this.optsWithGlobals() as GlobalOptions & { sizeGib: number; mountPath: string }
      const client = clientFromEnv(opts)
      const volume = await client.volumes.create({
        name,
        size_gib: opts.sizeGib,
        mount_path: opts.mountPath,
      })
      if (isJSONMode()) printJSON(volume)
      else printTable([volumeHeader(), volumeRow(volume)])
    })

  volumes
    .command('ls')
    .alias('list')
    .description('list volumes')
    .action(async function (this: Command) {
      const opts = this.optsWithGlobals() as GlobalOptions
      const client = clientFromEnv(opts)
      const list = await client.volumes.list()
      if (isJSONMode()) printJSON(list)
      else printTable([volumeHeader(), ...list.map(volumeRow)])
    })

  return volumes
}

function volumeHeader(): string[] {
  return ['ID', 'NAME', 'GIB', 'MOUNT', 'MACHINE']
}

function volumeRow(v: { id: string; name: string; size_gib: number; mount_path: string; machine_id?: string }): string[] {
  return [v.id, v.name, String(v.size_gib), v.mount_path, v.machine_id ?? '']
}
