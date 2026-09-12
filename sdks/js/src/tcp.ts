/**
 * One TCP connection to a port inside a machine, as a Duplex.
 *
 * # Why a Duplex and not something of our own
 *
 * The callers are database drivers. `pg`, `mysql2`, `ioredis` and `mongodb`
 * all speak to a `net.Socket`, and a Duplex is what a `net.Socket` is from
 * their side. Handing them one means every driver works unmodified against a
 * machine on the fleet, which is the whole point: the alternative is four
 * transport shims that each have to stay correct.
 *
 * # Why the writes are chunked
 *
 * A hop reading with the WebSocket library's default 32 KiB message limit drops
 * the whole connection on a larger frame, and a bulk insert writes large
 * buffers. The Go SDK chunks for exactly this reason; so does this.
 */

import { Duplex } from 'node:stream'

import type { WebSocketCtor } from './stream.ts'

/** The frame ceiling, matching the Go SDK's stdin chunk. */
const CHUNK = 32 * 1024 - 1024

export interface TCPOptions {
  /** Overrides `globalThis.WebSocket`, for a runtime that has none. */
  WebSocket?: WebSocketCtor
}

/**
 * Opens the tunnel and resolves once the socket is open.
 *
 * It resolves on OPEN rather than returning immediately, because a driver
 * handed a stream that is not yet connected writes its handshake into a queue
 * and waits for a reply that cannot come until the queue drains. Waiting here
 * turns a class of silent hangs into one awaited promise.
 */
export function tcpStream(
  baseURL: string,
  apiKey: string,
  id: string,
  port: number,
  org?: string,
  opts: TCPOptions = {},
): Promise<Duplex> {
  const url = new URL(
    baseURL.replace(/^http/, 'ws') + `/v1/machines/${encodeURIComponent(id)}/tcp/${port}`,
  )
  // The org narrowing reaches this route too. Every other call goes through
  // `Http.url`, which applies it; this one builds its own URL, so without this
  // an admin client acting as one org would not be narrowed on the tunnel.
  if (org) url.searchParams.set('org', org)

  const Ctor = opts.WebSocket ?? (globalThis as { WebSocket?: WebSocketCtor }).WebSocket
  if (!Ctor) {
    throw new Error(
      'no WebSocket: pass one as options.WebSocket, or run on Node 22 or newer where it is global',
    )
  }
  // The key travels in the subprotocol because the browser WebSocket API has
  // no way to set a header. hostd accepts both, and the Go SDK uses the header.
  const ws = new Ctor(url.toString(), [`authorization.bearer.${apiKey}`]) as WebSocket
  // Buffers rather than Blobs, so a message handler can read bytes without an
  // await. A double that does not implement it is left alone.
  try {
    ws.binaryType = 'arraybuffer'
  } catch {
    // A socket that does not expose binaryType delivers whatever it delivers,
    // and the message handler below copes with both shapes.
  }

  return new Promise<Duplex>((resolve, reject) => {
    let settled = false
    const fail = (err: Error) => {
      if (settled) return
      settled = true
      reject(err)
    }

    const duplex = new Duplex({
      write(chunk: Buffer, _enc, cb) {
        try {
          for (let at = 0; at < chunk.length; at += CHUNK) {
            ws.send(chunk.subarray(at, Math.min(at + CHUNK, chunk.length)))
          }
          cb()
        } catch (err) {
          cb(err as Error)
        }
      },
      final(cb) {
        // Closed cleanly, so the guest's side sees an orderly end rather than
        // a reset. A driver reading a reset reports a network error where the
        // truth is that the query finished.
        try {
          ws.close(1000)
        } catch {
          // Already closing; nothing to do and nothing worth reporting.
        }
        cb()
      },
      read() {
        // Flow control is the socket's. Backpressure is handled by pausing in
        // the message handler below rather than by pulling here.
      },
    })

    // addEventListener rather than the on* properties, which is what the exec
    // stream uses: a caller injecting its own socket (the `ws` package, a test
    // double) only has to be an EventTarget, not a full WebSocket.
    ws.addEventListener('open', () => {
      settled = true
      resolve(duplex)
    })
    ws.addEventListener('message', (ev: MessageEvent) => {
      const data = ev.data
      if (typeof data === 'string') {
        duplex.push(Buffer.from(data, 'utf8'))
        return
      }
      duplex.push(Buffer.from(data as ArrayBuffer))
    })
    ws.addEventListener('error', () => {
      const err = new Error(`the tunnel to ${id}:${port} failed`)
      fail(err)
      duplex.destroy(err)
    })
    ws.addEventListener('close', () => {
      fail(new Error(`the tunnel to ${id}:${port} closed before it opened`))
      // A null push is end-of-stream, which is what a driver needs to see to
      // stop waiting. Destroying instead would surface as an error on a
      // connection that simply ended.
      duplex.push(null)
    })
  })
}
