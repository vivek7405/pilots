/**
 * `@pilots/sdk/tanstack` -- pilots as a TanStack AI sandbox provider.
 *
 * ```ts
 * import { pilotsSandbox } from '@pilots/sdk/tanstack'
 *
 * const sandbox = pilotsSandbox({ apiKey: process.env.PILOT_API_KEY })
 * ```
 *
 * The contract is `@tanstack/ai-sandbox`'s `SandboxProvider` / `SandboxHandle`
 * pair. It is an OPTIONAL peer: the types below are structural copies rather
 * than imports, so the core package keeps its zero dependencies and a consumer
 * that never touches this entry point never needs the peer installed. A
 * provider object built here satisfies the real interface structurally, which
 * is what TypeScript checks at the call site.
 *
 * Three things about the mapping are worth knowing.
 *
 * - **A snapshot is a checkpoint, and a restore is IN PLACE.** `snapshot()`
 *   returns a checkpoint id; `restoreSnapshot()` puts that state back on the
 *   SAME machine, which keeps the URL. The contract's shape (restore returns a
 *   handle) is honoured by returning a handle to that same machine rather than
 *   a new one, because a machine created in a restore would mint a new URL.
 * - **`ports.connect()` returns the machine's permanent URL.** A pilots machine
 *   serves one HTTP port on an address that survives suspend, wake, restore and
 *   redeploy, so there is nothing to open and nothing to tear down.
 * - **`fork` is false.** Cloning a machine's disk into a second machine is
 *   ARCHITECTURE's post-parity backlog item, and a capability flag must say
 *   what is true today.
 */

import { PilotsClient } from './client.ts'
import type { ClientOptions } from './client.ts'
import { PilotsError } from './errors.ts'
import type { ExecStream } from './stream.ts'
import type { Machine } from './types.ts'

// ---------------------------------------------------------------------------
// The contract, mirrored structurally from @tanstack/ai-sandbox.
// ---------------------------------------------------------------------------

export interface SandboxCapabilities {
  fs: boolean
  exec: boolean
  env: boolean
  ports: boolean
  backgroundProcesses: boolean
  writableStdin: boolean
  killableProcesses: boolean
  snapshots: boolean
  networkPolicy: boolean
  durableFilesystem: boolean
  fork: boolean
}

export interface ExecResult {
  stdout: string
  stderr: string
  exitCode: number
}

export interface ProcessOptions {
  cwd?: string
  env?: Record<string, string>
  signal?: AbortSignal
}

export interface SpawnHandle {
  readonly pid: number
  readonly stdout: AsyncIterable<string>
  readonly stderr: AsyncIterable<string>
  readonly stdin: { write: (data: string) => Promise<void>; end: () => Promise<void> }
  wait: () => Promise<number>
  kill: (signal?: string | number) => Promise<void>
}

export interface SandboxChannel {
  url: string
  token?: string
  headers?: Record<string, string>
}

export interface SnapshotRef {
  id: string
  label?: string
}

export type SandboxFsStat =
  | { type: 'file'; mode: number; size: number }
  | { type: 'dir'; mode: number }
  | { type: 'symlink'; mode: number }
  | { type: 'other'; mode: number }

export interface SandboxHandle {
  readonly id: string
  readonly provider: string
  readonly workspaceRoot?: string
  readonly capabilities: SandboxCapabilities
  readonly fs: {
    read: (path: string) => Promise<string>
    readBytes: (path: string) => Promise<Uint8Array>
    write: (path: string, data: string | Uint8Array) => Promise<void>
    list: (path: string) => Promise<{ name: string; path: string; type: 'file' | 'dir' }[]>
    mkdir: (path: string) => Promise<void>
    remove: (path: string) => Promise<void>
    rename: (from: string, to: string) => Promise<void>
    exists: (path: string) => Promise<boolean>
    lstat?: (path: string) => Promise<SandboxFsStat | undefined>
  }
  readonly git: {
    clone: (input: { url: string; dir?: string; ref?: string; auth?: { username?: string; token: string }; depth?: number | 'full' }) => Promise<void>
    status: (dir?: string) => Promise<string>
    add: (paths: string[], dir?: string) => Promise<void>
    commit: (message: string, dir?: string) => Promise<void>
    push: (dir?: string) => Promise<void>
    pull: (dir?: string) => Promise<void>
    branch: (dir?: string) => Promise<string>
  }
  readonly process: {
    exec: (command: string, options?: ProcessOptions) => Promise<ExecResult>
    spawn: (command: string, options?: ProcessOptions) => Promise<SpawnHandle>
  }
  readonly ports: { connect: (port: number) => Promise<SandboxChannel> }
  readonly env: { set: (vars: Record<string, string>) => Promise<void> }
  snapshot?: (label?: string) => Promise<SnapshotRef>
  fork?: () => Promise<SandboxHandle>
  destroy: () => Promise<void>
}

export interface SandboxCreateInput {
  id?: string
  env?: Record<string, string>
  signal?: AbortSignal
  adapterName?: string
  [key: string]: unknown
}

export interface SandboxProvider {
  readonly name: string
  capabilities: () => SandboxCapabilities
  create: (input: SandboxCreateInput) => Promise<SandboxHandle>
  resume: (input: { id: string; signal?: AbortSignal }) => Promise<SandboxHandle | null>
  restoreSnapshot?: (input: { snapshotId: string; env?: Record<string, string>; signal?: AbortSignal }) => Promise<SandboxHandle>
  destroy: (input: { id: string; signal?: AbortSignal }) => Promise<void>
}

// ---------------------------------------------------------------------------
// The provider.
// ---------------------------------------------------------------------------

export interface PilotsSandboxConfig extends ClientOptions {
  /** Falls back to `PILOT_API_KEY`. */
  apiKey?: string
  /**
   * Where `/workspace` lands inside the machine. The guest image's home is
   * `/home/pilot`, so this is a directory under it by default.
   */
  workdir?: string
  /** vCPUs and memory for machines this provider creates. */
  vcpus?: number
  memMiB?: number
  /** Prefix for a generated machine name. */
  namePrefix?: string
}

export const DEFAULT_WORKDIR = '/home/pilot/workspace'

const CAPABILITIES: SandboxCapabilities = {
  fs: true,
  exec: true,
  env: true,
  ports: true,
  backgroundProcesses: true,
  writableStdin: true,
  killableProcesses: true,
  snapshots: true,
  networkPolicy: false,
  // A machine's disk survives suspend, wake and a host's death: S3 is the
  // truth and the local copy is a cache.
  durableFilesystem: true,
  // Post-parity backlog; see the note at the top of this file.
  fork: false,
}

function shellQuote(value: string): string {
  return `'${value.replace(/'/g, `'\\''`)}'`
}

/** A `SandboxHandle` over one machine. */
export class PilotsSandboxHandle implements SandboxHandle {
  readonly provider = 'pilots'
  readonly capabilities = CAPABILITIES
  readonly fs: SandboxHandle['fs']
  readonly git: SandboxHandle['git']
  readonly process: SandboxHandle['process']
  readonly ports: SandboxHandle['ports']
  readonly env: SandboxHandle['env']

  private readonly client: PilotsClient
  private machine: Machine
  private readonly workdir: string
  private readonly vars: Record<string, string>

  constructor(client: PilotsClient, machine: Machine, workdir: string, env: Record<string, string> = {}) {
    this.client = client
    this.machine = machine
    this.workdir = workdir
    this.vars = { ...env }

    this.fs = {
      read: async (path) => new TextDecoder().decode(await this.fs.readBytes(path)),
      readBytes: async (path) => {
        const res = await this.run(`base64 -w0 -- ${shellQuote(this.abs(path))}`)
        if (res.exitCode !== 0) throw new PilotsError(res.stderr.trim() || `cannot read ${path}`)
        return Uint8Array.from(atob(res.stdout.trim()), (c) => c.charCodeAt(0))
      },
      write: async (path, data) => {
        const bytes = typeof data === 'string' ? new TextEncoder().encode(data) : data
        // Chunked, not spread: `String.fromCharCode(...bytes)` passes one
        // argument per byte, which overflows the call stack somewhere past
        // 128 KB and dies with a RangeError naming neither the file nor the
        // size.
        let raw = ''
        for (let at = 0; at < bytes.length; at += 0x8000) {
          raw += String.fromCharCode(...bytes.subarray(at, at + 0x8000))
        }
        const encoded = btoa(raw)
        const target = shellQuote(this.abs(path))
        // The parent is QUOTED: unquoted, a path with a space makes two wrong
        // directories and the redirect then fails on the one it meant.
        const res = await this.run(
          `mkdir -p -- "$(dirname -- ${target})" && printf %s ${shellQuote(encoded)} | base64 -d > ${target}`,
        )
        if (res.exitCode !== 0) throw new PilotsError(res.stderr.trim() || `cannot write ${path}`)
      },
      list: async (path) => {
        const dir = this.abs(path)
        const res = await this.run(`ls -1A -- ${shellQuote(dir)}`)
        if (res.exitCode !== 0) throw new PilotsError(res.stderr.trim() || `cannot list ${path}`)
        const names = res.stdout.split('\n').filter(Boolean)
        const kinds = await this.run(
          names.length === 0
            ? 'true'
            : names.map((n) => `test -d ${shellQuote(`${dir}/${n}`)} && echo dir || echo file`).join('; '),
        )
        const types = kinds.stdout.split('\n').filter(Boolean)
        return names.map((name, i) => ({
          name,
          path: `${dir}/${name}`,
          type: (types[i] === 'dir' ? 'dir' : 'file') as 'file' | 'dir',
        }))
      },
      mkdir: async (path) => {
        // `cwd: '/'`, not the workdir: this is the call that CREATES the
        // workdir, and every other exec chdirs into it first. A mkdir that
        // chdirs into the directory it is about to make fails the fork, exit
        // 127 with no stderr, and the failure only surfaces on the next call.
        const res = await this.run(`mkdir -p -- ${shellQuote(this.abs(path))}`, { cwd: '/' })
        if (res.exitCode !== 0) throw new PilotsError(res.stderr.trim() || `cannot create ${path}`)
      },
      remove: async (path) => {
        await this.run(`rm -rf -- ${shellQuote(this.abs(path))}`)
      },
      rename: async (from, to) => {
        const res = await this.run(`mv -- ${shellQuote(this.abs(from))} ${shellQuote(this.abs(to))}`)
        if (res.exitCode !== 0) throw new PilotsError(res.stderr.trim() || `cannot rename ${from}`)
      },
      exists: async (path) => (await this.run(`test -e ${shellQuote(this.abs(path))}`)).exitCode === 0,
      // `stat -c` on the path itself, never through a symlink: the contract
      // says an implementation must not follow one.
      lstat: async (path) => {
        const res = await this.run(`stat -c '%F|%f|%s' -- ${shellQuote(this.abs(path))}`)
        if (res.exitCode !== 0) return undefined
        const [kind, hexMode, size] = res.stdout.trim().split('|')
        const mode = parseInt(hexMode ?? '0', 16)
        if (kind?.includes('directory')) return { type: 'dir', mode }
        if (kind?.includes('symbolic link')) return { type: 'symlink', mode }
        if (kind?.includes('regular')) return { type: 'file', mode, size: Number(size ?? 0) }
        return { type: 'other', mode }
      },
    }

    this.git = {
      clone: async ({ url, dir, ref, auth, depth }) => {
        const target = dir ? this.abs(dir) : this.workdir
        let remote = url
        if (auth?.token) {
          const user = auth.username || 'x-access-token'
          remote = url.replace(/^https:\/\//, `https://${user}:${auth.token}@`)
        }
        const flags = [
          depth !== 'full' && depth !== undefined ? `--depth ${Number(depth)}` : '',
          ref ? `--branch ${shellQuote(ref)}` : '',
        ]
          .filter(Boolean)
          .join(' ')
        const res = await this.run(`git clone ${flags} -- ${shellQuote(remote)} ${shellQuote(target)}`)
        if (res.exitCode !== 0) throw new PilotsError(res.stderr.trim() || 'git clone failed')
      },
      status: async (dir) => (await this.git_(dir, 'status --porcelain')).stdout,
      add: async (paths, dir) => {
        await this.git_(dir, `add -- ${paths.map(shellQuote).join(' ')}`)
      },
      commit: async (message, dir) => {
        await this.git_(dir, `commit -m ${shellQuote(message)}`)
      },
      push: async (dir) => {
        await this.git_(dir, 'push')
      },
      pull: async (dir) => {
        await this.git_(dir, 'pull')
      },
      branch: async (dir) => (await this.git_(dir, 'rev-parse --abbrev-ref HEAD')).stdout.trim(),
    }

    this.process = {
      exec: async (command, options) => this.run(command, options),
      spawn: async (command, options) => this.spawn(command, options),
    }

    this.ports = {
      // Nothing to open: the address is permanent and already serving.
      connect: async () => ({ url: this.machine.url }),
    }

    this.env = {
      set: async (vars) => {
        Object.assign(this.vars, vars)
      },
    }
  }

  get id(): string {
    return this.machine.name
  }

  /** The `m-…` id, for a caller that wants the typed client. */
  get machineId(): string {
    return this.machine.id
  }

  get workspaceRoot(): string {
    return this.workdir
  }

  /** The machine's permanent URL. */
  get url(): string {
    return this.machine.url
  }

  private abs(path: string): string {
    if (path === '/workspace') return this.workdir
    if (path.startsWith('/workspace/')) return this.workdir.replace(/\/$/, '') + path.slice('/workspace'.length)
    if (path.startsWith('/')) return path
    return `${this.workdir.replace(/\/$/, '')}/${path}`
  }

  private async run(command: string, options: ProcessOptions = {}): Promise<ExecResult> {
    const env = { ...this.vars, ...(options.env ?? {}) }
    const res = await this.client.machines.exec(this.machine.id, {
      cmd: command,
      cwd: options.cwd ? this.abs(options.cwd) : this.workdir,
      ...(Object.keys(env).length > 0 ? { env } : {}),
    })
    return { stdout: res.stdout, stderr: res.stderr, exitCode: res.exit_code }
  }

  private git_(dir: string | undefined, args: string): Promise<ExecResult> {
    return this.run(`git -C ${shellQuote(dir ? this.abs(dir) : this.workdir)} ${args}`)
  }

  private async spawn(command: string, options: ProcessOptions = {}): Promise<SpawnHandle> {
    const env = { ...this.vars, ...(options.env ?? {}) }
    const stream: ExecStream = this.client.machines.execStream(this.machine.id, ['sh', '-c', command], {
      cwd: options.cwd ? this.abs(options.cwd) : this.workdir,
      ...(Object.keys(env).length > 0 ? { env } : {}),
      stdin: true,
    })
    options.signal?.addEventListener('abort', () => stream.kill(), { once: true })
    const lines = async function* (readable: NodeJS.ReadableStream): AsyncIterable<string> {
      for await (const chunk of readable) yield chunk.toString()
    }
    return {
      // A pilots exec does not report the guest pid, and the contract needs a
      // number. 0 says "not addressable by pid"; kill() works regardless.
      pid: 0,
      stdout: lines(stream.stdout),
      stderr: lines(stream.stderr),
      stdin: {
        write: async (data) => stream.writeStdin(data),
        end: async () => stream.endStdin(),
      },
      wait: () => stream.wait(),
      kill: async () => stream.kill(),
    }
  }

  /** A checkpoint of the whole machine, memory included. */
  async snapshot(label?: string): Promise<SnapshotRef> {
    const checkpoint = await this.client.machines.checkpoint(this.machine.id, label ? { comment: label } : {})
    return { id: checkpoint.id, ...(label ? { label } : {}) }
  }

  /** Restores IN PLACE: the same machine, the same URL. Destructive. */
  async restoreSnapshot(snapshotId: string): Promise<void> {
    this.machine = await this.client.checkpoints.restore(snapshotId)
  }

  async destroy(): Promise<void> {
    await this.client.machines.destroy(this.machine.id)
  }
}

/**
 * The provider. `create` makes a machine (or adopts one whose name matches the
 * requested id), `resume` reattaches by name, `restoreSnapshot` puts a
 * checkpoint back on the machine that owns it, and `destroy` removes it.
 */
export function pilotsSandbox(config: PilotsSandboxConfig = {}): SandboxProvider {
  const apiKey = config.apiKey ?? (typeof process !== 'undefined' ? process.env?.PILOT_API_KEY : undefined) ?? ''
  const client = new PilotsClient(apiKey, config)
  const workdir = config.workdir ?? DEFAULT_WORKDIR
  const prefix = config.namePrefix ?? 'tanstack'

  const byName = async (name: string): Promise<Machine | null> => {
    for (const m of await client.machines.list()) if (m.name === name) return m
    return null
  }

  const handleFor = async (machine: Machine, env?: Record<string, string>): Promise<PilotsSandboxHandle> => {
    const handle = new PilotsSandboxHandle(client, machine, workdir, env)
    await handle.fs.mkdir('/workspace')
    return handle
  }

  return {
    name: 'pilots',
    capabilities: () => CAPABILITIES,

    async create(input) {
      // The contract asks a provider to honour a caller-chosen id where it
      // can. A pilots name IS addressable, and the URL is derived from it, so
      // it is honoured rather than ignored.
      const name = input.id ?? `${prefix}-${Math.random().toString(36).slice(2, 10)}`
      const existing = await byName(name)
      const machine =
        existing ??
        (await client.machines.create({
          name,
          ...(config.vcpus ? { vcpus: config.vcpus } : {}),
          ...(config.memMiB ? { mem_mib: config.memMiB } : {}),
          ...(input.env ? { env: input.env } : {}),
        }))
      return handleFor(machine, input.env)
    },

    async resume({ id }) {
      const machine = await byName(id)
      return machine ? handleFor(machine) : null
    },

    async restoreSnapshot({ snapshotId, env }) {
      // In place, so the machine the checkpoint belongs to is the machine
      // that comes back, with its URL intact.
      const machine = await client.checkpoints.restore(snapshotId)
      return handleFor(machine, env)
    },

    async destroy({ id }) {
      const machine = await byName(id)
      if (machine) await client.machines.destroy(machine.id)
    },
  }
}
