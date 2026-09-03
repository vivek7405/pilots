/**
 * The streaming exec.
 *
 * The wire format is sprites' byte protocol, so an existing sprites client
 * drops in unchanged: every binary frame's first byte is the stream id --
 * 1 stdout, 2 stderr, 3 exit with the code in `payload[0]` -- and the two the
 * client sends are 0 for a stdin chunk and 4 for stdin EOF.
 *
 * Two behaviours here are load-bearing rather than stylistic:
 *
 *   - `stdin` defaults to FALSE. A process holding an open stdin it never
 *     reads hangs, and Claude Code is exactly such a process: the reference
 *     workload's whole streaming path depends on this default.
 *   - a close with no exit frame is an ERROR, never a silent code 0. The guest
 *     agent drains both output pumps before it writes the exit frame and
 *     websocket frames are ordered, so an exit frame means the output that
 *     preceded it has already arrived. A socket that dropped instead means
 *     nobody knows what the command did.
 */

import { EventEmitter } from 'node:events'
import { PassThrough } from 'node:stream'

import { PilotsError } from './errors.ts'
import { FrameExit, FrameStderr, FrameStdin, FrameStdinEOF, FrameStdout } from './types.ts'

export type WebSocketCtor = new (url: string | URL, protocols?: string | string[]) => WebSocket

export interface ExecStreamOptions {
  cwd?: string
  env?: Record<string, string>
  user?: string
  /** Off by default; see the note above before turning it on. */
  stdin?: boolean
  /** Overrides `globalThis.WebSocket`. The seam the tests dial through. */
  WebSocket?: WebSocketCtor
}

/**
 * Builds the exec-stream URL, with the query names sprites uses.
 *
 * `path` is argv[0] repeated: hostd ignores it, but sprites clients send it,
 * and the compatibility surface is the query rather than only the frames.
 */
export function buildExecURL(
  baseURL: string,
  path: string,
  argv: string[],
  opts: ExecStreamOptions = {},
): URL {
  const url = new URL(baseURL.replace(/^http/, 'ws') + path)
  for (const arg of argv) url.searchParams.append('cmd', arg)
  if (argv.length > 0) url.searchParams.set('path', argv[0]!)
  if (opts.cwd) url.searchParams.set('dir', opts.cwd)
  for (const [key, value] of Object.entries(opts.env ?? {})) {
    url.searchParams.append('env', `${key}=${value}`)
  }
  if (opts.user) url.searchParams.set('user', opts.user)
  // Always present, never inferred: the default is the thing most likely to be
  // wrong by omission.
  url.searchParams.set('stdin', opts.stdin ? 'true' : 'false')
  return url
}

export interface ExecStreamInit {
  stdin: boolean
  WebSocket?: WebSocketCtor
}

/**
 * A running command.
 *
 * `stdout` and `stderr` are PassThrough streams. A WebSocket cannot be paused,
 * so those buffers are the only boundary: a stream nobody reads grows without
 * limit. For output nobody intends to read, use the buffered `machines.exec`.
 */
export class ExecStream extends EventEmitter {
  readonly stdout = new PassThrough()
  readonly stderr = new PassThrough()

  private ws: WebSocket
  private code: number | null = null
  private exited = false
  private settled = false
  private opened = false
  private readonly stdinEnabled: boolean
  private readonly pending: Uint8Array[] = []
  private readonly done: Promise<number>
  private resolveDone!: (code: number) => void
  private rejectDone!: (err: Error) => void

  constructor(url: URL, apiKey: string, init: ExecStreamInit) {
    super()
    this.stdinEnabled = init.stdin
    this.done = new Promise<number>((resolve, reject) => {
      this.resolveDone = resolve
      this.rejectDone = reject
    })
    // Nobody is obliged to call wait(); without this a failed stream would be
    // an unhandled rejection that takes the process down.
    this.done.catch(() => {})

    const WS = init.WebSocket ?? (globalThis as { WebSocket?: WebSocketCtor }).WebSocket
    if (!WS) {
      throw new PilotsError(
        'execStream needs a global WebSocket: Node 22+, Bun, Deno or a browser',
      )
    }

    // The key travels as a subprotocol rather than a header: browsers cannot
    // set handshake headers, and one code path is simpler to get right than
    // two. hostd accepts either form.
    this.ws = new WS(url, [`authorization.bearer.${apiKey}`])
    this.ws.binaryType = 'arraybuffer'
    this.ws.addEventListener('open', () => {
      this.opened = true
      for (const frame of this.pending.splice(0)) this.ws.send(frame)
      this.emit('open')
    })
    this.ws.addEventListener('message', (event: MessageEvent) => this.onMessage(event.data))
    this.ws.addEventListener('error', () => {
      this.fail(new PilotsError(this.opened ? 'exec stream failed' : 'exec stream could not connect'))
    })
    this.ws.addEventListener('close', () => {
      // Ends the streams even when no exit frame arrived, so a consumer's
      // `for await` terminates instead of hanging.
      this.endStreams()
      if (!this.exited) this.fail(new PilotsError('stream closed before exit'))
      this.emit('close')
    })
  }

  /** The exit code, or null until the exit frame arrives. */
  get exitCode(): number | null {
    return this.code
  }

  /** Resolves with the exit code; rejects on a failure or a close before exit. */
  wait(): Promise<number> {
    return this.done
  }

  /** Sends one stdin chunk (frame 0). Throws unless the stream opted into stdin. */
  writeStdin(chunk: Uint8Array | string): void {
    if (!this.stdinEnabled) {
      throw new PilotsError('this stream was opened with stdin: false')
    }
    const bytes = typeof chunk === 'string' ? new TextEncoder().encode(chunk) : chunk
    const frame = new Uint8Array(bytes.length + 1)
    frame[0] = FrameStdin
    frame.set(bytes, 1)
    this.send(frame)
  }

  /** Closes the process's stdin (frame 4). */
  endStdin(): void {
    if (!this.stdinEnabled) {
      throw new PilotsError('this stream was opened with stdin: false')
    }
    this.send(new Uint8Array([FrameStdinEOF]))
  }

  /** Closes the socket. The agent's context cancel kills the process. */
  kill(): void {
    this.ws.close(1000)
  }

  private send(frame: Uint8Array): void {
    if (this.opened) this.ws.send(frame)
    else this.pending.push(frame)
  }

  private onMessage(data: unknown): void {
    if (typeof data === 'string') return this.onText(data)
    const bytes =
      data instanceof ArrayBuffer
        ? new Uint8Array(data)
        : ArrayBuffer.isView(data as ArrayBufferView)
          ? new Uint8Array(
              (data as ArrayBufferView).buffer,
              (data as ArrayBufferView).byteOffset,
              (data as ArrayBufferView).byteLength,
            )
          : null
    if (!bytes || bytes.length === 0) return

    const payload = bytes.subarray(1)
    switch (bytes[0]) {
      case FrameStdout:
        // Dropped rather than pushed once the exit frame has ended the
        // streams: push() after EOF throws inside the socket's message
        // listener, which is an uncaught exception that takes the process
        // down. A frame that arrives after the verdict is a server that
        // reordered, not a reason to kill the caller.
        if (!this.exited) this.stdout.push(payload)
        return
      case FrameStderr:
        if (!this.exited) this.stderr.push(payload)
        return
      case FrameExit:
        this.finish(payload.length > 0 ? payload[0]! : 0)
        return
      default:
        // An id this version does not know is not a reason to fail: the
        // protocol is allowed to grow ids a newer agent sends.
        return
    }
  }

  /**
   * hostd sends a text `{"type":"exit","exit_code":n}` alongside the binary
   * exit frame. It is a fallback, not a second source of truth: whichever
   * arrives first decides, and the other is ignored.
   */
  private onText(data: string): void {
    let parsed: { type?: string; exit_code?: number }
    try {
      parsed = JSON.parse(data) as { type?: string; exit_code?: number }
    } catch {
      return
    }
    if (parsed.type === 'exit' && !this.exited) this.finish(parsed.exit_code ?? 0)
  }

  /** Ends both streams BEFORE announcing the exit, so no output can follow it. */
  private finish(code: number): void {
    if (this.exited) return
    this.exited = true
    this.code = code
    this.endStreams()
    this.emit('exit', code)
    if (!this.settled) {
      this.settled = true
      this.resolveDone(code)
    }
    this.ws.close(1000)
  }

  private endStreams(): void {
    if (this.stdout.readable) this.stdout.push(null)
    if (this.stderr.readable) this.stderr.push(null)
  }

  private fail(err: Error): void {
    if (this.settled) return
    this.settled = true
    this.rejectDone(err)
    // Emitting 'error' with no listener would throw; wait() is the other way
    // to learn about this, and a caller may legitimately use only that one.
    if (this.listenerCount('error') > 0) this.emit('error', err)
  }
}
