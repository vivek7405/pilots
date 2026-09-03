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
    const conn: WSConnection = {
      url: req.url ?? '/',
      query: url.searchParams,
      protocols: offered,
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
    connections.push(conn)

    // The close handshake has to be completed, not merely tolerated. A client
    // that sent a close frame waits for one back before releasing the socket,
    // so a server that only drains leaves the client's event loop alive and
    // its process never exits. Nothing else the client sends is asserted on
    // here, so the rest is drained.
    socket.on('data', (chunk: Buffer) => {
      if (closed || chunk.length === 0) return
      const opcode = chunk[0]! & 0x0f
      if (opcode === 0x8) {
        closed = true
        // The server may already have closed first, in which case there is
        // nothing left to answer on.
        if (socket.writable) {
          socket.write(Buffer.from([0x88, 0x00]))
          socket.end()
        }
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
