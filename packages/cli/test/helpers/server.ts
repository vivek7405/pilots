/**
 * A one-function HTTP server for the tests.
 *
 * Real sockets rather than a stubbed `fetch`, because half of what these tests
 * assert IS the wire: the `Accept` header GitHub needs to answer JSON, the
 * form encoding of the device-code body, the `Content-Type` on a tar upload.
 * A stub that takes a URL and returns an object cannot fail on any of those.
 */

import { createServer, type IncomingMessage, type ServerResponse } from 'node:http'
import type { AddressInfo } from 'node:net'

export interface RecordedRequest {
  method: string
  path: string
  headers: Record<string, string | string[] | undefined>
  body: string
}

export interface FakeServer {
  url: string
  requests: RecordedRequest[]
  close: () => Promise<void>
}

export type Handler = (
  req: IncomingMessage & { path: string; body: string },
  res: ServerResponse,
) => unknown

export async function startServer(handler: Handler): Promise<FakeServer> {
  const requests: RecordedRequest[] = []
  const server = createServer((req, res) => {
    const chunks: Buffer[] = []
    req.on('data', (c: Buffer) => chunks.push(c))
    req.on('end', () => {
      const body = Buffer.concat(chunks).toString('utf8')
      const path = (req.url ?? '/').split('?')[0]!
      requests.push({ method: req.method ?? 'GET', path: req.url ?? '/', headers: req.headers, body })
      void Promise.resolve(
        handler(Object.assign(req, { path, body }), res),
      ).catch((err: Error) => {
        if (!res.headersSent) res.writeHead(500, { 'content-type': 'application/json' })
        res.end(JSON.stringify({ error: err.message }))
      })
    })
  })
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve))
  const { port } = server.address() as AddressInfo
  return {
    url: `http://127.0.0.1:${port}`,
    requests,
    close: () =>
      new Promise<void>((resolve) => {
        server.closeAllConnections()
        server.close(() => resolve())
      }),
  }
}

export function json(res: ServerResponse, status: number, body: unknown): void {
  const text = typeof body === 'string' ? body : JSON.stringify(body)
  res.writeHead(status, { 'content-type': 'application/json' })
  res.end(text)
}
