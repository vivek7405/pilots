/**
 * A WebSocket server, in about a hundred lines.
 *
 * The exec-stream assertion has to be made against a real socket, because the
 * thing being asserted is a QUERY STRING on a handshake the SDK builds and the
 * MCP server sends from another process. There is no seam to inject a fake
 * through across a process boundary, so the wire is the only place to look.
 *
 * Hand-rolled rather than pulling `ws` in: the server half of the protocol
 * needed here is a SHA-1 of the client's key and one frame encoder.
 */

import { createHash } from 'node:crypto'
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http'
import type { AddressInfo } from 'node:net'
import type { Duplex } from 'node:stream'

const GUID = '258EAFA5-E914-47DA-95CA-C5AB0DC85B11'

export interface WSConnection {
  /** The full request URL, query string included. */
  url: string
  query: URLSearchParams
  protocols: string[]
  /** Sends one binary frame: an id byte followed by its payload. */
  frame: (id: number, payload?: Uint8Array | string) => void
  close: () => void
  /**
   * Text frames the CLIENT sent, in order.
   *
   * The terminal's resize control is a text frame rather than a binary one, so
   * a test that wants to assert a resize happened has nowhere else to look.
   */
  text: string[]
  /** Binary frames the CLIENT sent, the stream-id byte included. */
  binary: Buffer[]
  /** Resolves when the client's close frame arrives, or the socket ends. */
  closed: Promise<void>
}

export interface WSServer {
  /** An http:// URL; the SDK rewrites the scheme to ws:// itself. */
  url: string
  connections: WSConnection[]
  close: () => Promise<void>
}

/**
 * The exec stream shares an origin with the rest of the API, so this answers
 * ordinary requests too: a `machines.get` on the way to opening a stream has to
 * reach the same host.
 */
export async function startWSServer(
  onConnect: (conn: WSConnection) => void,
  http?: (req: IncomingMessage, res: ServerResponse) => void,
): Promise<WSServer> {
  const connections: WSConnection[] = []
  // Upgraded sockets are detached from the server, so `closeAllConnections`
  // does not see them and `close()` waits on them for ever. They are tracked
  // here and destroyed by hand.
  const upgraded = new Set<Duplex>()
  const server = createServer((req, res) => {
    if (http) return http(req, res)
    res.writeHead(426)
    res.end('upgrade required')
  })

  server.on('upgrade', (req: IncomingMessage, socket: Duplex) => {
    const key = req.headers['sec-websocket-key']
    const offered = String(req.headers['sec-websocket-protocol'] ?? '')
      .split(',')
      .map((p) => p.trim())
      .filter(Boolean)
    const accept = createHash('sha1').update(String(key) + GUID).digest('base64')

    const headers = [
      'HTTP/1.1 101 Switching Protocols',
      'Upgrade: websocket',
      'Connection: Upgrade',
      `Sec-WebSocket-Accept: ${accept}`,
      // A client that offered a subprotocol expects one back; the key rides in
      // that header, which is why one is always offered here.
      ...(offered.length > 0 ? [`Sec-WebSocket-Protocol: ${offered[0]}`] : []),
      '',
      '',
    ].join('\r\n')
    socket.write(headers)

    const url = new URL(req.url ?? '/', 'http://localhost')
    let closed = false
    let resolveClosed: () => void = () => {}
    const conn: WSConnection = {
      url: req.url ?? '/',
      query: url.searchParams,
      protocols: offered,
      text: [],
      binary: [],
      closed: new Promise<void>((resolve) => {
        resolveClosed = resolve
      }),
      frame: (id: number, payload: Uint8Array | string = new Uint8Array()) => {
        const bytes = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : Buffer.from(payload)
        const body = Buffer.concat([Buffer.from([id]), bytes])
        socket.write(encode(body))
      },
      close: () => {
        // 0x88 is a close frame; ending the socket without one makes the
        // client's decoder report a failure rather than a clean end.
        closed = true
        if (socket.writable) {
          socket.write(Buffer.from([0x88, 0x00]))
          socket.end()
        }
      },
    }
    upgraded.add(socket)
    socket.on('close', () => upgraded.delete(socket))
    // A client that closes and drops the connection in the same breath leaves
    // an ECONNRESET on this end. With no listener that is an unhandled error
    // event, which takes the whole test file down rather than failing one
    // assertion; the socket ending is what the test wanted either way.
    socket.on('error', () => {})
    connections.push(conn)

    // The close handshake has to be completed, not merely tolerated. A client
    // that sent a close frame waits for one back before releasing the socket,
    // so a server that only drains leaves the client's event loop alive and
    // its process never exits.
    //
    // Everything else is decoded rather than dropped, because a client frame is
    // an assertion target: `pilot console` sends its window size as a text
    // frame and its keystrokes as binary ones, and neither is visible from the
    // handshake.
    let pending = Buffer.alloc(0)
    socket.on('close', () => resolveClosed())
    socket.on('data', (chunk: Buffer) => {
      if (closed || chunk.length === 0) return
      pending = Buffer.concat([pending, chunk])
      for (;;) {
        const frame = decode(pending)
        if (!frame) return
        pending = pending.subarray(frame.size)
        if (frame.opcode === 0x8) {
          closed = true
          resolveClosed()
          // The server may already have closed first, in which case there is
          // nothing left to answer on.
          if (socket.writable) {
            socket.write(Buffer.from([0x88, 0x00]))
            socket.end()
          }
          return
        }
        if (frame.opcode === 0x1) conn.text.push(frame.payload.toString('utf8'))
        if (frame.opcode === 0x2) conn.binary.push(frame.payload)
      }
    })
    onConnect(conn)
  })

  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve))
  const { port } = server.address() as AddressInfo
  return {
    url: `http://127.0.0.1:${port}`,
    connections,
    close: () =>
      new Promise<void>((resolve) => {
        for (const socket of upgraded) socket.destroy()
        upgraded.clear()
        server.closeAllConnections()
        server.close(() => resolve())
      }),
  }
}

/**
 * One client frame, or null while it is still arriving.
 *
 * Client frames are always masked, and a chunk off the socket is not a frame:
 * two can land together and one can arrive in halves, which is why the caller
 * keeps the remainder rather than decoding each chunk on its own.
 */
function decode(buf: Buffer): { opcode: number; payload: Buffer; size: number } | null {
  if (buf.length < 2) return null
  const opcode = buf[0]! & 0x0f
  const masked = (buf[1]! & 0x80) !== 0
  let length = buf[1]! & 0x7f
  let offset = 2
  if (length === 126) {
    if (buf.length < 4) return null
    length = buf.readUInt16BE(2)
    offset = 4
  } else if (length === 127) {
    if (buf.length < 10) return null
    // A test never sends 4 GiB; reading the low half keeps this honest anyway.
    length = Number(buf.readBigUInt64BE(2))
    offset = 10
  }
  const key = masked ? buf.subarray(offset, offset + 4) : Buffer.alloc(0)
  offset += key.length
  if (buf.length < offset + length) return null
  const payload = Buffer.from(buf.subarray(offset, offset + length))
  if (masked) {
    for (let i = 0; i < payload.length; i++) payload[i] = payload[i]! ^ key[i % 4]!
  }
  return { opcode, payload, size: offset + length }
}

/** One unmasked binary frame. Server frames are never masked. */
function encode(payload: Buffer): Buffer {
  const first = Buffer.from([0x82])
  if (payload.length < 126) {
    return Buffer.concat([first, Buffer.from([payload.length]), payload])
  }
  const len = Buffer.alloc(3)
  len[0] = 126
  len.writeUInt16BE(payload.length, 1)
  return Buffer.concat([first, len, payload])
}
