/**
 * The transport: one bearer-authenticated `fetch`, one status-to-error map,
 * and an NDJSON line iterator that yields as the bytes arrive.
 *
 * Nothing here retries, pools or rate-limits. A caller who wants any of those
 * passes their own `fetch` -- adding them inside the SDK would be a dependency
 * bought for convenience.
 */

import {
  ComposePlanError,
  NotFoundError,
  PilotsError,
  QuotaExceededError,
} from './errors.ts'
import type { ComposePlanError as ComposePlanErrorBody, QuotaExceededResponse } from './types.ts'

export type FetchLike = typeof globalThis.fetch

export interface HttpOptions {
  baseURL?: string
  fetch?: FetchLike
  /** Applies to JSON calls only. Streams are never given a deadline. */
  timeoutMs?: number
}

export interface RequestInit_ {
  /** JSON-encoded into the body. */
  body?: unknown
  /** A pre-encoded body (a tar, a stream). Takes precedence over `body`. */
  raw?: BodyInit
  contentType?: string
  accept?: string
  query?: Record<string, string | number | boolean | undefined>
  signal?: AbortSignal
  /** null disables the client deadline; used for builds and log follows. */
  timeoutMs?: number | null
}

export const DEFAULT_BASE_URL = 'https://api.pilotrun.app'
export const DEFAULT_TIMEOUT_MS = 30_000

/** Reads the base URL a client was given, the environment, then the default. */
export function resolveBaseURL(explicit?: string): string {
  const env = typeof process !== 'undefined' ? process.env?.PILOT_API_URL : undefined
  return (explicit || env || DEFAULT_BASE_URL).replace(/\/+$/, '')
}

export class Http {
  readonly baseURL: string
  readonly apiKey: string
  readonly timeoutMs: number
  private readonly fetchImpl: FetchLike

  constructor(apiKey: string, opts: HttpOptions = {}) {
    if (!apiKey) {
      // Before any request: a client built with no key would otherwise fail
      // once per call with a 401 that says nothing about the cause.
      throw new PilotsError('an API key is required: new PilotsClient(process.env.PILOT_API_KEY)')
    }
    this.apiKey = apiKey
    this.baseURL = resolveBaseURL(opts.baseURL)
    this.timeoutMs = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS
    this.fetchImpl = opts.fetch ?? globalThis.fetch
  }

  url(path: string, query?: RequestInit_['query']): URL {
    const url = new URL(this.baseURL + path)
    for (const [key, value] of Object.entries(query ?? {})) {
      if (value !== undefined) url.searchParams.set(key, String(value))
    }
    return url
  }

  /** Performs the request and throws on any non-2xx. */
  async send(method: string, path: string, init: RequestInit_ = {}): Promise<Response> {
    const headers: Record<string, string> = { authorization: `Bearer ${this.apiKey}` }
    if (init.accept) headers.accept = init.accept

    let body: BodyInit | undefined
    if (init.raw !== undefined) {
      body = init.raw
      if (init.contentType) headers['content-type'] = init.contentType
    } else if (init.body !== undefined) {
      body = JSON.stringify(init.body)
      headers['content-type'] = init.contentType ?? 'application/json'
    }

    const signals: AbortSignal[] = []
    if (init.signal) signals.push(init.signal)
    const deadline = init.timeoutMs === null ? null : (init.timeoutMs ?? this.timeoutMs)
    if (deadline !== null && deadline > 0) signals.push(AbortSignal.timeout(deadline))

    const request: RequestInit & { duplex?: string } = {
      method,
      headers,
      ...(body !== undefined ? { body } : {}),
      ...(signals.length ? { signal: signals.length === 1 ? signals[0] : AbortSignal.any(signals) } : {}),
    }
    // Node refuses a streaming request body without it, and a build context is
    // the one body this SDK streams.
    if (body !== undefined && typeof body === 'object' && Symbol.asyncIterator in Object(body)) {
      request.duplex = 'half'
    }

    let res: Response
    try {
      res = await this.fetchImpl(this.url(path, init.query), request)
    } catch (cause) {
      throw new PilotsError(`${method} ${path}: ${(cause as Error).message}`, { cause })
    }
    if (!res.ok) throw await toError(res, method, path)
    return res
  }

  async json<T>(method: string, path: string, init: RequestInit_ = {}): Promise<T> {
    const res = await this.send(method, path, { accept: 'application/json', ...init })
    return (await res.json()) as T
  }

  async text(method: string, path: string, init: RequestInit_ = {}): Promise<string> {
    const res = await this.send(method, path, init)
    return await res.text()
  }

  /** For a 204: drains the body so the connection can be reused. */
  async none(method: string, path: string, init: RequestInit_ = {}): Promise<void> {
    const res = await this.send(method, path, init)
    await res.arrayBuffer()
  }
}

/** Maps a failed response onto the narrowest error class that fits it. */
async function toError(res: Response, method: string, path: string): Promise<PilotsError> {
  const body = await res.text().catch(() => '')
  let parsed: unknown
  try {
    parsed = JSON.parse(body)
  } catch {
    parsed = undefined
  }
  const record = (parsed ?? {}) as Record<string, unknown>
  const message = typeof record.error === 'string' && record.error ? record.error : `${method} ${path} failed with ${res.status}`

  if (res.status === 404) return new NotFoundError(message, { body })
  if (res.status === 429 && typeof record.quota === 'string') {
    const q = record as unknown as QuotaExceededResponse
    return new QuotaExceededError(
      message,
      { quota: q.quota, limit: q.limit, used: q.used, ...(q.scope !== undefined ? { scope: q.scope } : {}) },
      { body },
    )
  }
  if (res.status === 400 && Array.isArray(record.unsupported)) {
    return new ComposePlanError(parsed as ComposePlanErrorBody, { body })
  }
  return new PilotsError(message, { status: res.status, body })
}

/**
 * Yields each NDJSON line as it arrives.
 *
 * Never buffers the whole body: a client watching a ten-minute build needs the
 * first step's output in the first second, which is the entire reason hostd
 * streams it.
 */
export async function* ndjson<T>(res: Response): AsyncGenerator<T, void, undefined> {
  for await (const line of textLines(res)) yield JSON.parse(line) as T
}

/** Yields each non-empty line of a text body as it arrives. */
export async function* textLines(res: Response): AsyncGenerator<string, void, undefined> {
  if (!res.body) return
  const reader = res.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  try {
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      buffer += decoder.decode(value, { stream: true })
      let nl: number
      while ((nl = buffer.indexOf('\n')) >= 0) {
        const line = buffer.slice(0, nl).trim()
        buffer = buffer.slice(nl + 1)
        if (line) yield line
      }
    }
    buffer += decoder.decode()
    const last = buffer.trim()
    if (last) yield last
  } finally {
    reader.releaseLock()
  }
}
