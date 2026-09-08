/**
 * The build log stream.
 *
 * `POST /v1/builds` answers 200 before the build starts, because a client
 * watching a ten-minute build needs the first step's output in the first
 * second. The consequence is that the status code cannot be the verdict: the
 * LAST NDJSON line is, and a line carrying `error` means the build failed
 * under a 200.
 */

import { BuildFailedError } from './errors.ts'
import { ndjson } from './http.ts'
import type { BuildLogLine } from './types.ts'

export class BuildStream {
  /** Also in the `X-Pilot-Build-Id` header, so a lost connection can reattach. */
  readonly buildId: string
  /** Every line seen so far, in order. */
  readonly lines: BuildLogLine[] = []

  private readonly source: AsyncGenerator<BuildLogLine, void, undefined>
  private readonly res: Response

  constructor(res: Response, buildId?: string) {
    this.buildId = buildId ?? res.headers.get('x-pilot-build-id') ?? ''
    this.res = res
    this.source = ndjson<BuildLogLine>(res)
  }

  /**
   * Stops reading and releases the response.
   *
   * For a caller that has seen what it needed and is walking away -- a page
   * whose reader navigated off a build log. Without it the generator is left
   * suspended holding an open body, and the socket is not returned until the
   * whole build finishes. A `result()` after this throws rather than hanging,
   * because a closed stream has no verdict to wait for and an interrupted
   * build must never read as a successful one.
   */
  async close(): Promise<void> {
    // The BODY first, not the generator. While a build is being followed the
    // generator is suspended inside `reader.read()`, and `return()` queues
    // behind that pending `next()`: it does not resolve until the next line
    // arrives, which on a quiet build step is minutes, and the upstream
    // connection stays open for all of it. Cancelling the body tears the read
    // out from under it, so the return below resolves at once.
    //
    // Guarded rather than conditional: a generator that took the reader's lock
    // makes this throw, and there is no way to ask a suspended one whether it
    // did. Either way the socket is released.
    await this.res.body?.cancel().catch(() => {})
    await this.source.return(undefined).catch(() => {})
  }

  /** `for await (const line of build)`. Consumes the stream; iterate once. */
  async *[Symbol.asyncIterator](): AsyncGenerator<BuildLogLine, void, undefined> {
    for await (const line of this.source) {
      this.lines.push(line)
      yield line
    }
  }

  /**
   * Drains the stream and returns the rootfs build id.
   *
   * Throws `BuildFailedError` when the last line carries `error`, and equally
   * when the stream ended with no verdict at all -- an interrupted build must
   * not read as a successful one.
   */
  async result(): Promise<string> {
    for await (const _line of this) {
      // Drained for its side effect: `lines` accumulates as they arrive.
    }
    const last = this.lines[this.lines.length - 1]
    if (last?.error) {
      throw new BuildFailedError(last.error, this.buildId, this.lines)
    }
    if (last?.result) return last.result
    throw new BuildFailedError(
      'the build stream ended without a verdict',
      this.buildId,
      this.lines,
    )
  }
}
