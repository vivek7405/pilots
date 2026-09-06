/**
 * `pilot machines`: the one primitive, in eleven verbs.
 *
 * A sandbox and a production replica are the same object here; the only thing
 * that separates them is the lifecycle knobs, which is why there is no second
 * command tree for "sandboxes".
 */

import type { EventEmitter } from 'node:events'
import type { Readable, Writable } from 'node:stream'

import { Command } from 'commander'
import type { CreateMachineRequest, Machine, PilotsClient } from '@pilots/sdk'

import { clientFromEnv, type GlobalOptions } from '../config.ts'
import { CliError, isJSONMode, note, printJSON, printTable } from '../output.ts'
import { confirmOrExit } from '../prompt.ts'
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
        // Only destroy asks, and only when there is somebody to answer.
        // Nothing else here is irreversible.
        if (verb === 'destroy') {
          await confirmOrExit(`destroy ${found.name} (${found.id})?`, opts)
        }
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
 * The local end of a stream: the two standard streams, the signals around them
 * and the way out.
 *
 * Injectable because everything `pilot console` promises is made HERE and not
 * on the wire -- raw mode goes on and comes off again, a window change becomes
 * a resize, an interrupt still leaves a usable terminal behind -- and none of
 * it is observable from a spawned process whose stdin is a pipe. There is no
 * pseudo-terminal in Node without a native module, and this package adds no
 * dependency, so the seam is the terminal.
 */
export interface Terminal {
  stdin: Readable & { isTTY?: boolean; setRawMode?: (mode: boolean) => void }
  stdout: Writable & { isTTY?: boolean; columns?: number; rows?: number }
  signals: EventEmitter
  exit: (code: number) => void
}

export const processTerminal: Terminal = {
  stdin: process.stdin,
  stdout: process.stdout,
  signals: process,
  exit: (code) => process.exit(code),
}

/** The signals that end a session, with the code a shell reports for each. */
const TEARDOWN_SIGNALS: [NodeJS.Signals, number][] = [
  ['SIGINT', 130],
  ['SIGTERM', 143],
  ['SIGHUP', 129],
]

/**
 * Streams an exec, wiring frame 1 to stdout and frame 2 to stderr.
 *
 * `stdin` is opt-in. A guest process holding an open stdin it never reads
 * hangs, and the reference workload -- an agent session -- is exactly such a
 * process, so the default has to be off rather than convenient.
 *
 * `opts.tty` is the same stream in terminal mode, which is what `pilot console`
 * runs on: stdin is forced on, the local terminal goes into raw mode so every
 * keystroke reaches the shell as a byte rather than a line, a window change
 * becomes a resize, and nothing is expected on frame 2 because a PTY has one
 * device. Closing the socket kills the remote shell, so every exit path here
 * ends the session rather than leaving one behind.
 */
export async function execStream(
  client: PilotsClient,
  id: string,
  argv: string[],
  opts: Record<string, unknown>,
  term: Terminal = processTerminal,
): Promise<number> {
  const tty = Boolean(opts.tty)
  const wantsStdin = tty || Boolean(opts.stdin)
  const stream = client.machines.execStream(id, argv, {
    ...(opts.cwd ? { cwd: opts.cwd as string } : {}),
    ...(opts.env ? { env: parseKeyValues(opts.env as string[]) } : {}),
    ...(opts.user ? { user: opts.user as string } : {}),
    stdin: wantsStdin,
    ...(tty ? { tty: true, cols: colsOf(term), rows: rowsOf(term) } : {}),
  })
  stream.stdout.pipe(term.stdout)
  // Under a PTY frame 2 never arrives, so there is nothing to wire it to. A
  // byte on stderr there would mean the pipe path ran when a terminal was
  // asked for, and it is better seen than quietly forwarded.
  if (!tty) stream.stderr.pipe(process.stderr)

  // Raw mode is the one piece of global state this command owns, and a
  // terminal left in it is unusable afterwards: no echo, no line editing, no
  // ctrl-c. It is tracked rather than toggled blind so every exit path can put
  // it back exactly once.
  let raw = false
  const setRaw = (on: boolean) => {
    if (!tty || raw === on || typeof term.stdin.setRawMode !== 'function') return
    term.stdin.setRawMode(on)
    raw = on
  }

  const onStdin = (chunk: Buffer) => {
    try {
      stream.writeStdin(chunk)
    } catch (err) {
      // A keystroke can land between the shell exiting and this listener being
      // removed. Under a terminal that race is ordinary and a closed socket is
      // the answer to it; off a terminal a failed write is news.
      if (!tty) throw err
    }
  }
  // A terminal has no separate write end to close, so an EOF on the local one
  // is a hangup: end the session rather than send an EOT the shell may ignore.
  const onStdinEnd = () => (tty ? stream.kill() : stream.endStdin())
  if (wantsStdin) {
    setRaw(true)
    term.stdin.on('data', onStdin)
    term.stdin.on('end', onStdinEnd)
  }
  const onResize = () => {
    try {
      stream.resize(colsOf(term), rowsOf(term))
    } catch {
      // The window can change while the socket is on its way down. A resize
      // nobody can deliver is not a failure of the session that just ended.
    }
  }
  if (tty) term.signals.on('SIGWINCH', onResize)

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
  // A signal closes the socket, which cancels the guest's context and kills the
  // command; 128 plus the signal number is what a shell reports for the same
  // interruption. The exit skips the `finally` below, so raw mode comes off
  // here: this is the path where a terminal is most likely to be abandoned in
  // it, since the user is already reaching for the keyboard in frustration.
  const handlers: [NodeJS.Signals, () => void][] = []
  for (const [signal, code] of TEARDOWN_SIGNALS) {
    // Off a terminal only SIGINT is ours. Taking SIGTERM there would change
    // what `pilot machines exec` does under a `kill` for no gain.
    if (!tty && signal !== 'SIGINT') continue
    const handler = () => {
      setRaw(false)
      stream.kill()
      term.exit(code)
    }
    handlers.push([signal, handler])
    term.signals.once(signal, handler)
  }
  try {
    return await stream.wait()
  } catch (err) {
    // The stream's own "closed before exit" is true but useless here: the
    // caller asked for the deadline and deserves to be told it was hit.
    if (timedOut) throw new CliError(`timed out after ${timeoutMs}ms: the command was killed`)
    throw err
  } finally {
    // First, before anything below it can throw and strand the terminal.
    setRaw(false)
    if (timer) clearTimeout(timer)
    for (const [signal, handler] of handlers) term.signals.off(signal, handler)
    if (tty) term.signals.off('SIGWINCH', onResize)
    if (wantsStdin) {
      // Reading stdin keeps the handle referenced, so without this the CLI
      // outlives the command it ran: `pilot machines exec --stdin` would sit
      // there after the guest had already exited, waiting on a terminal
      // nobody is typing into.
      term.stdin.off('data', onStdin)
      term.stdin.off('end', onStdinEnd)
      term.stdin.pause()
    }
  }
}

/**
 * The window, with the protocol's own defaults when the terminal has no usable
 * one.
 *
 * A terminal that does not know its size reports 0, not undefined, and that is
 * not a rare shape: a pty whose window was never set, anything wrapped in
 * `script`, a serial console. hostd refuses a size outside 1..65535 by closing
 * the socket rather than clamping it, deliberately, so 0 on the wire is a
 * console that cannot connect at all. A size that is not a size means the
 * window is unknown, which is what the default is for.
 */
function windowOf(value: number | undefined, fallback: number): number {
  return typeof value === 'number' && Number.isInteger(value) && value >= 1 && value <= 65535
    ? value
    : fallback
}

function colsOf(term: Terminal): number {
  return windowOf(term.stdout.columns, 80)
}

function rowsOf(term: Terminal): number {
  return windowOf(term.stdout.rows, 24)
}

// Name first, id last, in every table here. The name is what a person types
// into every other command, and a 42-character id in column one pushes the URL
// off the right edge of a normal terminal.
function machineHeader(): string[] {
  return ['NAME', 'STATE', 'HOST', 'URL', 'ID']
}

function machineRow(m: Machine): string[] {
  return [m.name, m.state, m.host_id, m.custom_domain || m.url, m.id]
}

function pick(source: Record<string, unknown>, keys: string[]): Record<string, string> {
  const out: Record<string, string> = {}
  for (const key of keys) {
    const value = source[key]
    if (typeof value === 'string') out[key] = value
  }
  return out
}
