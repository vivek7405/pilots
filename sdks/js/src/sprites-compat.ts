/**
 * `@pilots/sdk/sprites-compat` -- the sprites-shaped face of the pilots API.
 *
 * It exists so the reference customer moves providers by changing one import
 * line. The surface is exactly what a sprites client consumes and nothing
 * more: no sessions, no `/control` multiplex, no filesystem API.
 *
 * Two rules decide the shapes here.
 *
 *   - A sprite's `id` is the machine's NAME. A sprites consumer persists the
 *     id and then hands it back as a path segment, and the alias that serves
 *     those paths resolves names. `machineId` carries the `m-...` id for
 *     anyone who wants the raw client.
 *   - A restore is IN PLACE. `restoreCheckpoint` performs exactly one request,
 *     `POST /v1/checkpoints/{id}/restore`, and never creates a machine: a
 *     machine created in a restore would get a new URL, and a URL is permanent.
 */

import { PilotsClient } from './client.ts'
import { NotFoundError, PilotsError } from './errors.ts'
import { resolveBaseURL } from './http.ts'
import type { ExecStream, ExecStreamOptions, WebSocketCtor } from './stream.ts'
import type { Machine } from './types.ts'

/** What a sprites consumer destructures from an exec. */
export interface ExecResult {
  stdout: string
  stderr: string
  exitCode: number
}

export interface SpriteInfo {
  id: string
  name: string
  url: string
  status: string
}

export interface Checkpoint {
  id: string
  createTime: Date
  comment?: string
}

export interface ExecOptions {
  cwd?: string
  env?: Record<string, string>
  /** Milliseconds, as sprites spells it. Sent as `timeout_ms`. */
  timeout?: number
}

export interface SpritesClientOptions {
  baseURL?: string
  /** Milliseconds. Default 30000. */
  timeout?: number
  WebSocket?: WebSocketCtor
}

const DEFAULT_TIMEOUT = 30_000
/** A machine id as the manager mints them. */
const MACHINE_ID = /^m-[0-9a-f]{24}$/

export class SpritesClient {
  readonly baseURL: string
  readonly token: string
  readonly timeout: number
  /** The underlying typed client, for anything this surface does not cover. */
  readonly pilots: PilotsClient

  private readonly byName = new Map<string, Machine>()

  constructor(token: string, opts: SpritesClientOptions = {}) {
    this.token = token
    this.baseURL = resolveBaseURL(opts.baseURL)
    this.timeout = opts.timeout ?? DEFAULT_TIMEOUT
    this.pilots = new PilotsClient(token, {
      baseURL: this.baseURL,
      timeoutMs: this.timeout,
      ...(opts.WebSocket ? { WebSocket: opts.WebSocket } : {}),
    })
  }

  /** A handle. No network until something is asked of it. */
  sprite(name: string): Sprite {
    return new Sprite(name, this)
  }

  /**
   * Creates a machine. The body is `{name}` and nothing else: the router
   * already proxies every machine's URL to the guest's port 8080, which is
   * the port a dev server binds, so there is no port to declare.
   */
  async createSprite(name: string): Promise<Sprite> {
    const machine = await this.pilots.machines.create({ name })
    this.byName.set(machine.name, machine)
    return new Sprite(machine.name, this, machine)
  }

  async getSprite(name: string): Promise<Sprite> {
    const machine = await this.resolve(name)
    return new Sprite(machine.name, this, machine)
  }

  async deleteSprite(name: string): Promise<void> {
    const machine = await this.resolve(name)
    this.byName.delete(machine.name)
    await this.pilots.machines.destroy(machine.id)
  }

  /**
   * A no-op. A workload's URL is public by default here, so there is nothing
   * to switch on; it stays on the surface because a sprites consumer calls it.
   */
  async setPublicUrl(_name: string): Promise<void> {
    return
  }

  /** Drops a cached machine, so the next resolve goes back to the fleet. */
  forget(name: string): void {
    this.byName.delete(name)
  }

  /**
   * Name to machine. Names are fleet-unique, so a list filtered on the name
   * is the lookup; an argument that matches no name but looks like a machine
   * id is tried as one, which is what makes an id work wherever a name does.
   */
  async resolve(name: string): Promise<Machine> {
    const cached = this.byName.get(name)
    if (cached) return cached

    const machines = await this.pilots.machines.list()
    for (const machine of machines) this.byName.set(machine.name, machine)
    const found = this.byName.get(name)
    if (found) return found

    if (MACHINE_ID.test(name)) {
      const machine = await this.pilots.machines.get(name)
      this.byName.set(machine.name, machine)
      return machine
    }
    throw new NotFoundError(`no sprite named ${name}`)
  }
}

export class Sprite {
  readonly name: string

  private readonly client: SpritesClient
  private machine: Machine | undefined

  constructor(name: string, client: SpritesClient, machine?: Machine) {
    this.name = name
    this.client = client
    this.machine = machine
  }

  /** The machine's NAME, which is what a sprites consumer persists. */
  get id(): string {
    return this.name
  }

  /** `Machine.url` verbatim. Permanent: it survives every lifecycle step. */
  get url(): string {
    return this.machine?.url ?? ''
  }

  get status(): string {
    return this.machine?.state ?? ''
  }

  /** `m-<24 hex>`, for calls made through the raw `PilotsClient`. */
  get machineId(): string {
    return this.machine?.id ?? ''
  }

  toInfo(): SpriteInfo {
    return { id: this.id, name: this.name, url: this.url, status: this.status }
  }

  /** Runs `[file, ...args]`, each argument shell-quoted, under the guest's bash. */
  execFile(file: string, args: string[] = [], opts: ExecOptions = {}): Promise<ExecResult> {
    return this.exec(shellQuote([file, ...args]), opts)
  }

  async exec(command: string, opts: ExecOptions = {}): Promise<ExecResult> {
    const res = await this.withMachine((machine) =>
      this.client.pilots.machines.exec(machine.id, {
        cmd: command,
        ...(opts.cwd !== undefined ? { cwd: opts.cwd } : {}),
        ...(opts.env !== undefined ? { env: opts.env } : {}),
        ...(opts.timeout !== undefined ? { timeout_ms: opts.timeout } : {}),
      }),
    )
    return { stdout: res.stdout, stderr: res.stderr, exitCode: res.exit_code }
  }

  /**
   * Streams a command. `stdin` stays off unless asked for.
   *
   * Synchronous, as sprites' is, which means the machine has to be known
   * already: use `createSprite` or `getSprite` rather than the lazy `sprite`.
   */
  spawn(command: string, args: string[] = [], opts: ExecStreamOptions = {}): ExecStream {
    if (!this.machine) {
      throw new PilotsError(
        `spawn needs a resolved sprite: await client.getSprite('${this.name}') first`,
      )
    }
    return this.client.pilots.machines.execStream(this.machine.id, [command, ...args], opts)
  }

  /**
   * Returns a `Response` whose body is one NDJSON line, the checkpoint's JSON.
   * A sprites consumer reads `.text()` and scans the lines backwards for `id`.
   */
  async createCheckpoint(comment?: string): Promise<Response> {
    const checkpoint = await this.withMachine((machine) =>
      this.client.pilots.machines.checkpoint(machine.id, comment !== undefined ? { comment } : {}),
    )
    return ndjsonResponse(checkpoint)
  }

  async listCheckpoints(): Promise<Checkpoint[]> {
    const list = await this.withMachine((machine) =>
      this.client.pilots.machines.listCheckpoints(machine.id),
    )
    return list.map((c) => ({
      id: c.id,
      createTime: new Date(c.created_at * 1000),
      ...(c.comment !== undefined ? { comment: c.comment } : {}),
    }))
  }

  /**
   * Restores IN PLACE. Exactly one request, and no machine is created: the
   * same machine keeps its id, its URL and its agent token.
   */
  async restoreCheckpoint(id: string): Promise<Response> {
    const machine = await this.client.pilots.checkpoints.restore(id)
    this.machine = machine
    return ndjsonResponse(machine)
  }

  destroy(): Promise<void> {
    return this.client.deleteSprite(this.name)
  }

  /**
   * Resolves the machine, runs the call, and on a 404 forgets what it knew and
   * tries once more -- a cached machine that was destroyed and recreated
   * elsewhere would otherwise poison every later call.
   */
  private async withMachine<T>(fn: (machine: Machine) => Promise<T>): Promise<T> {
    const machine = this.machine ?? (await this.client.resolve(this.name))
    this.machine = machine
    try {
      return await fn(machine)
    } catch (err) {
      if (!(err instanceof NotFoundError)) throw err
      this.client.forget(this.name)
      this.machine = undefined
      const fresh = await this.client.resolve(this.name)
      this.machine = fresh
      return await fn(fresh)
    }
  }
}

/** Per-argument POSIX quoting, so the quoted form re-yields the original. */
export function shellQuote(args: string[]): string {
  return args.map((a) => `'${a.replace(/'/g, "'\\''")}'`).join(' ')
}

function ndjsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body) + '\n', {
    status: 200,
    headers: { 'content-type': 'application/x-ndjson' },
  })
}
