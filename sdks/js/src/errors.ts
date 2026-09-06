/**
 * The error model.
 *
 * Every non-2xx becomes a `PilotsError`, and the cases a caller has to branch
 * on -- a missing machine, a quota refusal, a compose file the planner cannot
 * express, a build that failed after the status line was already 200 -- become
 * subclasses carrying the fields needed to act, so nobody has to re-parse a
 * body string to find out what happened.
 */

import type {
  BuildLogLine,
  ComposeUnsupported,
  ComposePlanError as ComposePlanErrorBody,
  ComposeUnknownDetails,
  HealthGateDetails,
} from './types.ts'

export interface PilotsErrorInit {
  /** HTTP status, or 0 for an error raised before a request was made. */
  status?: number
  /** The response body, verbatim, for anything the fields did not capture. */
  body?: string
  /** The body's stable `code`. Empty when the server sent none. */
  code?: string
  /** The body's `next`: the one thing to do about it. */
  next?: string
  /** The body's `details`, typed per code. */
  details?: unknown
  cause?: unknown
}

/**
 * Base class for everything this SDK throws.
 *
 * `code`, `next` and `details` are on the base rather than only on the
 * subclasses, because a caller that does not branch still wants to print the
 * next step, and a code this SDK version has never heard of must still reach
 * it rather than be dropped on the way through.
 */
export class PilotsError extends Error {
  readonly status: number
  readonly body: string
  readonly code: string
  readonly next: string
  readonly details: unknown

  constructor(message: string, init: PilotsErrorInit = {}) {
    super(message, init.cause !== undefined ? { cause: init.cause } : undefined)
    this.name = 'PilotsError'
    this.status = init.status ?? 0
    this.body = init.body ?? ''
    this.code = init.code ?? ''
    this.next = init.next ?? ''
    this.details = init.details
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

/**
 * 422 `health_gate_failed`. The release built and started, and never answered
 * its health check inside the grace period.
 *
 * Matched by `code` and never by status alone: 422 is the shape of this one
 * answer, and a future 422 for something else must not be caught here.
 */
export class HealthGateError extends PilotsError {
  readonly details: HealthGateDetails

  constructor(message: string, details: HealthGateDetails, init: PilotsErrorInit = {}) {
    super(message, { status: 422, ...init, details })
    this.name = 'HealthGateError'
    this.details = details
  }
}

/**
 * 400 `unknown_framework`. The directory has no compose file, no Dockerfile
 * and no framework the platform recognises.
 *
 * `details` carries the listing, the manifests and the two Dockerfile rules,
 * which is enough to write one without reading the repository again.
 */
export class UnknownFrameworkError extends PilotsError {
  readonly details: ComposeUnknownDetails

  constructor(message: string, details: ComposeUnknownDetails, init: PilotsErrorInit = {}) {
    super(message, { status: 400, ...init, details })
    this.name = 'UnknownFrameworkError'
    this.details = details
  }
}
