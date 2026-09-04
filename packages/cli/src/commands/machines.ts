/**
 * `pilot machines`: the one primitive, in eleven verbs.
 *
 * A sandbox and a production replica are the same object here; the only thing
 * that separates them is the lifecycle knobs, which is why there is no second
 * command tree for "sandboxes".
 */

import { Command } from 'commander'
import type { CreateMachineRequest, Machine, PilotsClient } from '@pilots/sdk'

import { clientFromEnv, type GlobalOptions } from '../config.ts'
import { CliError, isJSONMode, note, printJSON, printTable } from '../output.ts'
import { collect, parseKeyValues, resolveMachine } from '../resolve.ts'

export function createMachinesCommand(): Command {
  const machines = new Command('machines').alias('machine').description('create and drive machines')

  machines
    .command('create')
    .description('create a machine (a restore from a template, not a boot)')
    .option('--name <name>', 'a stable name; the URL is derived from it and never changes')
    .option('--image <ref>', 'a rootfs build id from `pilot deploy` or the build tool')
    .option('--template <name>', 'a golden template to restore from')
    .option('--checkpoint <id>', 'restore this checkpoint into the new machine')
    .option('--vcpus <n>', 'vCPUs', Number)
    .option('--mem-mib <n>', 'memory in MiB', Number)
    .option('--app <name>', 'the app this machine belongs to')
    .option('--cmd <command>', 'the start command, overriding the image')
    .option('--env <K=V>', 'an environment variable (repeatable)', collect)
    .option('--volume <id>', 'attach this volume')
    .action(async function (this: Command) {
      const opts = this.optsWithGlobals() as GlobalOptions & Record<string, unknown>
      const client = clientFromEnv(opts)
      const req: CreateMachineRequest = {
        ...pick(opts, ['name', 'image', 'template', 'checkpoint', 'app', 'cmd']),
        ...(opts.vcpus !== undefined ? { vcpus: opts.vcpus as number } : {}),
        ...(opts.memMib !== undefined ? { mem_mib: opts.memMib as number } : {}),
        ...(opts.volume !== undefined ? { volume: opts.volume as string } : {}),
        ...(opts.env ? { env: parseKeyValues(opts.env as string[]) } : {}),
      }
      const machine = await client.machines.create(req)
      if (isJSONMode()) printJSON(machine)
      else printTable([machineHeader(), machineRow(machine)])
    })

  machines
    .command('ls')
    .alias('list')
    .description('list machines')
    .option('--app <name>', 'only machines in this app')
    .action(async function (this: Command) {
      const opts = this.optsWithGlobals() as GlobalOptions & { app?: string }
      const client = clientFromEnv(opts)
      // Filtered here rather than on the wire: `GET /v1/machines` takes no app
      // parameter, and inventing a query string the server ignores would read
      // as a filter that works.
      const all = await client.machines.list()
      const list = opts.app ? all.filter((m) => m.app === opts.app) : all
      if (isJSONMode()) printJSON(list)
      else printTable([machineHeader(), ...list.map(machineRow)])
    })

  machines
    .command('info <machine>')
    .description('show one machine, by id or name')
    .action(async function (this: Command, machine: string) {
      const opts = this.optsWithGlobals() as GlobalOptions
      const client = clientFromEnv(opts)
      const found = await resolveMachine(client, machine)
      if (isJSONMode()) printJSON(found)
      else printTable([machineHeader(), machineRow(found)])
    })

  machines
    .command('exec <machine>')
    .description('run a command; everything after -- is the argv')
    .option('--cwd <dir>', 'working directory in the guest')
    .option('--env <K=V>', 'an environment variable (repeatable)', collect)
    .option('--user <name>', 'run as this user')
    .option('--stdin', 'forward this terminal\'s stdin to the command', false)
    .option('--timeout-ms <n>', 'give up after this long', Number)
    .allowExcessArguments(true)
    .action(async function (this: Command, machine: string) {
      const opts = this.optsWithGlobals() as GlobalOptions & Record<string, unknown>
      const argv = this.args.slice(1)
      if (argv.length === 0) throw new CliError('nothing to run: pilot machines exec <machine> -- <argv...>')
      const client = clientFromEnv(opts)
      const found = await resolveMachine(client, machine)
      process.exitCode = await execStream(client, found.id, argv, opts)
    })

  machines
    .command('logs <machine>')
    .description('print the console log')
    .option('-f, --follow', 'stream new lines as they arrive')
    .action(async function (this: Command, machine: string) {
      const opts = this.optsWithGlobals() as GlobalOptions & { follow?: boolean }
      const client = clientFromEnv(opts)
      const found = await resolveMachine(client, machine)
      if (opts.follow) {
        for await (const line of client.machines.followLogs(found.id)) {
          process.stdout.write(line + '\n')
        }
        return
      }
      const text = await client.machines.logs(found.id)
      if (isJSONMode()) printJSON({ machine: found.id, logs: text })
      else process.stdout.write(text.endsWith('\n') || text === '' ? text : text + '\n')
    })

  machines
    .command('checkpoint <machine>')
    .description('capture a checkpoint of a running machine')
    .option('--comment <text>', 'a note stored with the checkpoint')
    .action(async function (this: Command, machine: string) {
      const opts = this.optsWithGlobals() as GlobalOptions & { comment?: string }
      const client = clientFromEnv(opts)
      const found = await resolveMachine(client, machine)
      const cp = await client.machines.checkpoint(found.id, opts.comment ? { comment: opts.comment } : {})
      if (isJSONMode()) printJSON(cp)
      else printTable([['ID', 'SEQ', 'DURABLE'], [cp.id, String(cp.seq), String(cp.durable)]])
    })

  machines
    .command('checkpoints <machine>')
    .description('list a machine\'s checkpoints')
    .action(async function (this: Command, machine: string) {
      const opts = this.optsWithGlobals() as GlobalOptions
      const client = clientFromEnv(opts)
      const found = await resolveMachine(client, machine)
      const list = await client.machines.listCheckpoints(found.id)
      if (isJSONMode()) printJSON(list)
      else printTable([['ID', 'SEQ', 'DURABLE'], ...list.map((c) => [c.id, String(c.seq), String(c.durable)])])
    })

  machines
    .command('restore <checkpoint>')
    .description('restore a checkpoint IN PLACE: same machine, same id, same URL')
    .action(async function (this: Command, checkpoint: string) {
      const opts = this.optsWithGlobals() as GlobalOptions
      const client = clientFromEnv(opts)
      const machine = await client.checkpoints.restore(checkpoint)
      if (isJSONMode()) printJSON(machine)
      else printTable([machineHeader(), machineRow(machine)])
    })

  for (const verb of ['suspend', 'wake', 'start', 'stop', 'destroy'] as const) {
    machines
      .command(`${verb} <machine>`)
      .description(descriptionFor(verb))
      .action(async function (this: Command, machine: string) {
        const opts = this.optsWithGlobals() as GlobalOptions
        const client = clientFromEnv(opts)
        const found = await resolveMachine(client, machine)
        // `start` and `stop` answer 501 at HEAD. The server's own error is
        // what the user sees: a CLI that hid it behind "not supported yet"
        // would keep saying so for a week after the route landed.
        await client.machines[verb](found.id)
        if (isJSONMode()) printJSON({ machine: found.id, [verb]: true })
        else note(`${verb} ${found.id}`)
      })
  }

  machines
    .command('volume <machine>')
    .description('the volume drive Firecracker actually has, not the one hostd meant to set')
    .action(async function (this: Command, machine: string) {
      const opts = this.optsWithGlobals() as GlobalOptions
      const client = clientFromEnv(opts)
      const found = await resolveMachine(client, machine)
      const vol = await client.machines.volume(found.id)
      if (isJSONMode()) printJSON(vol)
      else printTable([
        ['VOLUME', 'MOUNT', 'DEVICE', 'CACHE'],
        [vol.volume_id, vol.mount_path, vol.device, vol.cache_type],
      ])
    })

  return machines
}

function descriptionFor(verb: string): string {
  switch (verb) {
    case 'suspend':
      return 'snapshot and free the machine; the URL wakes it again'
    case 'wake':
      return 'restore a suspended machine'
    case 'start':
      return 'start a stopped machine (boots; no snapshot involved)'
    case 'stop':
      return 'stop without snapshotting'
    default:
      return 'destroy the machine and its snapshots'
  }
}

/**
 * Streams an exec, wiring frame 1 to stdout and frame 2 to stderr.
 *
 * `stdin` is opt-in. A guest process holding an open stdin it never reads
 * hangs, and the reference workload -- an agent session -- is exactly such a
 * process, so the default has to be off rather than convenient.
 */
async function execStream(
  client: PilotsClient,
  id: string,
  argv: string[],
  opts: Record<string, unknown>,
): Promise<number> {
  const stream = client.machines.execStream(id, argv, {
    ...(opts.cwd ? { cwd: opts.cwd as string } : {}),
    ...(opts.env ? { env: parseKeyValues(opts.env as string[]) } : {}),
    ...(opts.user ? { user: opts.user as string } : {}),
    stdin: Boolean(opts.stdin),
  })
  stream.stdout.pipe(process.stdout)
  stream.stderr.pipe(process.stderr)

  const onStdin = (chunk: Buffer) => stream.writeStdin(chunk)
  const onStdinEnd = () => stream.endStdin()
  if (opts.stdin) {
    process.stdin.on('data', onStdin)
    process.stdin.on('end', onStdinEnd)
  }
  // The deadline is enforced here rather than on the wire: the streaming exec
  // takes no timeout, unlike the buffered one. Closing the socket cancels the
  // guest's context, which is the same thing SIGINT does below.
  const timeoutMs = Number(opts.timeoutMs)
  let timedOut = false
  const timer =
    Number.isFinite(timeoutMs) && timeoutMs > 0
      ? setTimeout(() => {
          timedOut = true
          stream.kill()
        }, timeoutMs)
      : undefined
  // SIGINT closes the socket, which cancels the guest's context and kills the
  // command; 130 is what a shell reports for the same interruption.
  const onSigint = () => {
    stream.kill()
    process.exit(130)
  }
  process.once('SIGINT', onSigint)
  try {
    return await stream.wait()
  } catch (err) {
    // The stream's own "closed before exit" is true but useless here: the
    // caller asked for the deadline and deserves to be told it was hit.
    if (timedOut) throw new CliError(`timed out after ${timeoutMs}ms: the command was killed`)
    throw err
  } finally {
    if (timer) clearTimeout(timer)
    process.off('SIGINT', onSigint)
    if (opts.stdin) {
      // Reading stdin keeps the handle referenced, so without this the CLI
      // outlives the command it ran: `pilot machines exec --stdin` would sit
      // there after the guest had already exited, waiting on a terminal
      // nobody is typing into.
      process.stdin.off('data', onStdin)
      process.stdin.off('end', onStdinEnd)
      process.stdin.pause()
    }
  }
}

function machineHeader(): string[] {
  return ['ID', 'NAME', 'STATE', 'HOST', 'URL']
}

function machineRow(m: Machine): string[] {
  return [m.id, m.name, m.state, m.host_id, m.custom_domain || m.url]
}

function pick(source: Record<string, unknown>, keys: string[]): Record<string, string> {
  const out: Record<string, string> = {}
  for (const key of keys) {
    const value = source[key]
    if (typeof value === 'string') out[key] = value
  }
  return out
}
