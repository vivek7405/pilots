/**
 * A fake hostd: a real HTTP server answering the routes a test needs and
 * recording every request it saw.
 *
 * Real sockets rather than a stubbed `fetch`, because half of what these tests
 * assert is what actually goes on the wire -- the bearer header, the method,
 * the exact JSON body, and how many requests a call made.
 */

import { createServer } from 'node:http'
import type { IncomingMessage, Server, ServerResponse } from 'node:http'
import type { AddressInfo } from 'node:net'

export interface Recorded {
  method: string
  /** Path with the query string, as received. */
  url: string
  /** Path only. */
  path: string
  query: URLSearchParams
  headers: Record<string, string | string[] | undefined>
  /** The raw body; `json` is the parsed form when it parses. */
  body: string
  json: unknown
}

export type Route = (req: Recorded, res: ServerResponse) => void | Promise<void>

export class FakeHostd {
  readonly requests: Recorded[] = []
  private readonly routes = new Map<string, Route>()
  private server: Server | undefined
  private fallback: Route | undefined
  baseURL = ''

  /** `on('GET /v1/machines', handler)`, mirroring hostd's own route table. */
  on(pattern: string, route: Route): this {
    this.routes.set(pattern, route)
    return this
  }

  /** Answers anything with no registered route. Defaults to a 404 body. */
  otherwise(route: Route): this {
    this.fallback = route
    return this
  }

  async start(): Promise<this> {
    this.server = createServer((req, res) => void this.handle(req, res))
    await new Promise<void>((resolve) => this.server!.listen(0, '127.0.0.1', resolve))
    const { port } = this.server.address() as AddressInfo
    this.baseURL = `http://127.0.0.1:${port}`
    return this
  }

  async stop(): Promise<void> {
    if (!this.server) return
    await new Promise<void>((resolve, reject) =>
      this.server!.close((err) => (err ? reject(err) : resolve())),
    )
    this.server = undefined
  }

  /** The single request the test expected, or a failure if there were others. */
  get only(): Recorded {
    if (this.requests.length !== 1) {
      throw new Error(
        `expected exactly one request, saw ${this.requests.length}: ` +
          this.requests.map((r) => `${r.method} ${r.url}`).join(', '),
      )
    }
    return this.requests[0]!
  }

  private async handle(req: IncomingMessage, res: ServerResponse): Promise<void> {
    const chunks: Buffer[] = []
    for await (const chunk of req) chunks.push(chunk as Buffer)
    const body = Buffer.concat(chunks).toString('utf8')
    const url = new URL(req.url ?? '/', 'http://fake')
    let json: unknown
    try {
      json = body ? JSON.parse(body) : undefined
    } catch {
      json = undefined
    }

    const recorded: Recorded = {
      method: req.method ?? 'GET',
      url: req.url ?? '/',
      path: url.pathname,
      query: url.searchParams,
      headers: req.headers,
      body,
      json,
    }
    this.requests.push(recorded)

    const route =
      this.routes.get(`${recorded.method} ${recorded.path}`) ??
      this.matchPattern(recorded) ??
      this.fallback ??
      notFound
    await route(recorded, res)
  }

  /** Supports `{id}` wildcards, the same shape hostd's mux uses. */
  private matchPattern(req: Recorded): Route | undefined {
    for (const [pattern, route] of this.routes) {
      const [method, template] = pattern.split(' ')
      if (method !== req.method || !template) continue
      const want = template.split('/')
      const got = req.path.split('/')
      if (want.length !== got.length) continue
      if (want.every((seg, i) => seg.startsWith('{') || seg === got[i])) return route
    }
    return undefined
  }
}

const notFound: Route = (_req, res) => {
  json(res, 404, { error: 'state: not found' })
}

/** Writes a JSON response, as hostd's `writeJSON` does. */
export function json(res: ServerResponse, status: number, body: unknown): void {
  res.writeHead(status, { 'content-type': 'application/json' })
  res.end(JSON.stringify(body))
}

/** Writes a 204, as every lifecycle route does. */
export function noContent(res: ServerResponse): void {
  res.writeHead(204)
  res.end()
}

/** A machine as hostd would render it, with only the fields a test cares about. */
export function machine(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: 'm-000000000000000000000001',
    name: 'demo',
    host_id: 'h1',
    state: 'running',
    knobs: { auto_stop: 'suspend', auto_start: true, min_machines_running: 0, soft_limit: 20 },
    vcpus: 1,
    mem_mib: 512,
    url: 'https://demo.pilotrun.app',
    created_at: 1_756_000_000,
    last_activity: 1_756_000_100,
    ...over,
  }
}
