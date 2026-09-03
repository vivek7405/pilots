/**
 * The error model.
 *
 * Every non-2xx becomes a `PilotsError`, and the cases a caller has to branch
 * on -- a missing machine, a quota refusal, a compose file the planner cannot
 * express, a build that failed after the status line was already 200 -- become
 * subclasses carrying the fields needed to act, so nobody has to re-parse a
 * body string to find out what happened.
 */

import type { BuildLogLine, ComposeUnsupported, ComposePlanError as ComposePlanErrorBody } from './types.ts'

export interface PilotsErrorInit {
  /** HTTP status, or 0 for an error raised before a request was made. */
  status?: number
  /** The response body, verbatim, for anything the fields did not capture. */
  body?: string
  cause?: unknown
}

/** Base class for everything this SDK throws. */
export class PilotsError extends Error {
  readonly status: number
  readonly body: string

  constructor(message: string, init: PilotsErrorInit = {}) {
    super(message, init.cause !== undefined ? { cause: init.cause } : undefined)
    this.name = 'PilotsError'
    this.status = init.status ?? 0
    this.body = init.body ?? ''
  }
}

/** 404. The machine, checkpoint, service, volume or build does not exist. */
export class NotFoundError extends PilotsError {
  constructor(message: string, init: PilotsErrorInit = {}) {
    super(message, { status: 404, ...init })
    this.name = 'NotFoundError'
  }
}

/**
 * 429. The org (or, for builds, the host) is at its ceiling.
 *
 * `quota` names which one, so a caller can raise the right limit rather than
 * guess from a sentence.
 */
export class QuotaExceededError extends PilotsError {
  readonly quota: string
  readonly limit: number
  readonly used: number
  /** "host" when the ceiling is the host's rather than the org's. */
  readonly scope?: string

  constructor(
    message: string,
    fields: { quota: string; limit: number; used: number; scope?: string },
    init: PilotsErrorInit = {},
  ) {
    super(message, { status: 429, ...init })
    this.name = 'QuotaExceededError'
    this.quota = fields.quota
    this.limit = fields.limit
    this.used = fields.used
    if (fields.scope !== undefined) this.scope = fields.scope
  }
}

/**
 * 400 from `POST /v1/compose/plan` listing what the planner will not accept.
 *
 * Structurally the `compose.PlanError` wire shape (`ComposePlanError` in
 * types.ts, which the drift test checks); the class is what `@pilots/sdk`
 * exports under that name, because a caller catches it rather than decoding
 * it by hand.
 */
export class ComposePlanError extends PilotsError implements ComposePlanErrorBody {
  readonly error: string
  readonly unsupported: ComposeUnsupported[]

  constructor(body: ComposePlanErrorBody, init: PilotsErrorInit = {}) {
    const detail = body.unsupported.map((u) => `${u.service}.${u.key}: ${u.message}`).join('; ')
    super(detail ? `${body.error}: ${detail}` : body.error, { status: 400, ...init })
    this.name = 'ComposePlanError'
    this.error = body.error
    this.unsupported = body.unsupported
  }
}

/**
 * A build that failed.
 *
 * The status code is 200: hostd decides it before the build's outcome is
 * known, so a client can watch a ten-minute build instead of waiting for it
 * (`internal/api/builds.go`). The LAST NDJSON line is the verdict, and this is
 * what a `result()` raises when that line carries `error`. It keeps every line
 * so an agent can read the failing step and patch the Dockerfile.
 */
export class BuildFailedError extends PilotsError {
  readonly buildId: string
  readonly lines: BuildLogLine[]

  constructor(message: string, buildId: string, lines: BuildLogLine[]) {
    super(message, { status: 200 })
    this.name = 'BuildFailedError'
    this.buildId = buildId
    this.lines = lines
  }
}
