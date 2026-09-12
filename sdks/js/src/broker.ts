/**
 * Working from inside a machine, with no key.
 *
 * # Why this reads a file rather than calling the broker
 *
 * The guest agent already fetches this machine's token and keeps it fresh, and
 * it does that so every client does not have to. Four SDKs each running their
 * own refresh loop is four loops that have to be correct about backoff, about a
 * broker that says no, and about a token expiring mid-request, and three of
 * them would be wrong the first time somebody looked.
 *
 * So a client inside a machine reads the file the agent maintains: nothing to
 * refresh, nothing to schedule, and a token that is at most five minutes old
 * because something else keeps it that way.
 *
 * # Why it re-reads
 *
 * The token changes every five minutes and the file is on tmpfs, so a read is a
 * syscall against a page already in memory. Holding one for the life of the
 * process would mean sending a token that expired while it was held, which is a
 * 401 with no cause anybody can see.
 */

import { readFileSync } from 'node:fs'

/** Comfortably shorter than the agent's refresh, so a client never holds a
 * token the agent has already replaced. */
const TTL_MS = 30_000

/**
 * The machine's own token, read from the file the guest agent maintains.
 *
 * Returns undefined outside a machine and on an ungranted one. Both are
 * ordinary states rather than errors: a client with no credential gets 401 from
 * every route, which is the honest outcome of having none.
 */
export class BrokerCredential {
  private readonly path: string
  private token = ''
  private readAt = 0

  constructor(path: string) {
    this.path = path
  }

  value(): string {
    const now = Date.now()
    if (now - this.readAt < TTL_MS) return this.token
    this.readAt = now
    try {
      this.token = readFileSync(this.path, 'utf8').trim()
    } catch {
      // A missing file is the ordinary state of an ungranted machine. Nothing
      // is logged: a library warning every thirty seconds about a deliberate
      // decision is noise in somebody's logs for ever.
      this.token = ''
    }
    return this.token
  }
}

/**
 * A credential source for a process inside a machine, or undefined.
 *
 * undefined rather than an empty object, so the caller's check is "is there
 * one" and a client outside a machine carries nothing extra.
 */
export function machineCredential(): BrokerCredential | undefined {
  const path = process.env.PILOT_TOKEN_FILE
  return path ? new BrokerCredential(path) : undefined
}

/**
 * Whether this process runs in a pilots machine that has a broker to ask.
 *
 * For a caller deciding whether to require a key: inside a machine, an empty
 * key is the normal case rather than a configuration mistake.
 */
export function insideMachine(): boolean {
  return Boolean(process.env.PILOT_BROKER_URL)
}
