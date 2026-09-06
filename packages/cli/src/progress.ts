/**
 * The build stream, rendered for a person.
 *
 * A webjs Dockerfile is 29 stages and each one's `npm install` is dozens of
 * lines, so two services put several hundred lines on the screen and the thing
 * a reader wants, which stage is running and how long it took, is buried in
 * them. The default here is one line per stage; `--verbose` is the old firehose
 * and `--json` is untouched NDJSON.
 *
 * Nothing in this file writes to stdout. Under `--json` stdout carries one
 * object, the result, and every log line goes to stderr unchanged so an agent
 * reading a failure still has all of them.
 */

import type { BuildFailedError, BuildLogLine, ComposeStep } from '@pilots/sdk'

import { isJSONMode, note, paint } from './output.ts'

/** At most this many buffered lines are replayed when a step fails. */
const FAILURE_TAIL = 200

export interface ReporterOptions {
  /** Every line, the way it streamed. */
  verbose: boolean
  /** Whether the running step may be shown on a line that is later erased. */
  tty: boolean
}

export class BuildReporter {
  private readonly opts: ReporterOptions
  /** step name to the timestamp of the first line seen for it. */
  private readonly first = new Map<string, number>()
  /** step name to its buffered stdout and stderr, kept for a failure. */
  private readonly output = new Map<string, string[]>()
  /** The step currently shown on the live line, empty when there is none. */
  private live = ''

  constructor(opts: ReporterOptions) {
    this.opts = opts
  }

  line(step: ComposeStep, l: BuildLogLine): void {
    if (isJSONMode()) {
      process.stderr.write(JSON.stringify(l) + '\n')
      return
    }
    if (this.opts.verbose) {
      const text = l.error ?? l.line ?? ''
      if (text) note(`${step.name}${l.step ? ` ${l.step}` : ''} | ${text.replace(/\n$/, '')}`)
      return
    }

    const name = l.step ?? ''
    if (name && !this.first.has(name)) this.first.set(name, l.ts)

    if (l.stream === 'status') {
      this.status(step.name, name, l)
      return
    }
    // Not printed, but kept: on a failure the only thing worth seeing is the
    // output of the step that failed, and by then it has already streamed past.
    const text = l.error ?? l.line ?? ''
    if (!text) return
    const buffered = this.output.get(name) ?? []
    buffered.push(text.replace(/\n$/, ''))
    if (buffered.length > FAILURE_TAIL) buffered.splice(0, buffered.length - FAILURE_TAIL)
    this.output.set(name, buffered)
    this.showLive(step.name, name)
  }

  /**
   * Prints the failing step's own output, then returns.
   *
   * The error itself is rendered by `fail()`, so this adds the context that
   * error cannot carry: what the step actually printed before it stopped.
   */
  failed(step: ComposeStep, err: BuildFailedError): void {
    if (isJSONMode() || this.opts.verbose) return
    this.clearLive()
    const failing = err.lines.at(-1)?.step ?? ''
    const buffered = this.output.get(failing)
    if (!buffered?.length) return
    note(paint('dim', `${step.name}  ${failing}`))
    for (const text of buffered) note(`${step.name}  ${text}`)
  }

  /** Erases the live line, if any. Called before anything else prints. */
  clearLive(): void {
    if (!this.live) return
    process.stderr.write('\r\x1b[2K')
    this.live = ''
  }

  private status(service: string, name: string, l: BuildLogLine): void {
    this.clearLive()
    const text = l.error ?? l.line ?? ''
    if (!text) return
    // A host phase names the build id as its step, so there is no vertex to
    // time and the phase name is the whole line.
    const started = this.first.get(name)
    const elapsed = started !== undefined && l.ts > started ? ` ${((l.ts - started) / 1000).toFixed(1)}s` : ''
    if (text === 'cached') {
      note(`${service}  ${name}  ${paint('dim', 'cached')}`)
      return
    }
    if (text === 'done') {
      note(`${service}  ${name}  done${elapsed}`)
      return
    }
    note(`${service}  ${text}`)
  }

  /**
   * Shows the running step on one line that is erased when it completes.
   *
   * Off a TTY nothing is written: a build log read later is not a terminal, and
   * a carriage return in it is a line that reads as garbage.
   */
  private showLive(service: string, name: string): void {
    if (!this.opts.tty || !name || this.live === name) return
    this.clearLive()
    process.stderr.write(`${service}  ${name} …`)
    this.live = name
  }
}
