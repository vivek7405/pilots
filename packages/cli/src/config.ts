/**
 * The credentials file, and the client built from it.
 *
 * Two rules here are load-bearing rather than tidy:
 *
 *   - **Nothing in this module touches the network.** The dashboard mints a
 *     key and is never consulted again. fly's tkdb outage (`docs/prior-art/
 *     fly-io.md` COPY 18) is the argument: a CLI that validates its cached
 *     credential against a service has made every command depend on that
 *     service being up, which is exactly the central dependency this platform
 *     exists without.
 *   - **The file is the lowest-precedence source.** `PILOT_API_KEY` and
 *     `PILOT_API_URL` win over it and `--api-url` wins over everything, so a
 *     CI job and a one-off against a staging fleet need no file at all.
 */

import { chmodSync, mkdirSync, readFileSync, renameSync, rmSync, statSync, writeFileSync } from 'node:fs'
import { homedir } from 'node:os'
import { dirname, join } from 'node:path'

import { PilotsClient } from '@pilots/sdk'
import type { ClientOptions } from '@pilots/sdk'

import { CliError } from './output.ts'

/** The SDK's own default, repeated here so `pilot login` can record it. */
export const DEFAULT_API_URL = 'https://api.pilotrun.app'

export interface Credentials {
  api_key: string
  api_url?: string
  org_id?: string
  /**
   * Per-app secret values a `secret://` reference in a compose file resolves
   * to on THIS machine. Plaintext, which is why the file is 0600: the
   * alternative is a keyring dependency per platform, and the value has to
   * reach the process in plaintext regardless.
   */
  secrets?: Record<string, Record<string, string>>
}

export function credentialsPath(env: NodeJS.ProcessEnv = process.env): string {
  const base = env.XDG_CONFIG_HOME || join(env.HOME || homedir(), '.config')
  return join(base, 'pilots', 'credentials')
}

/**
 * Reads the credentials file, or null when there is none.
 *
 * Refuses a file any other user can read. A leaked API key is a leaked fleet,
 * and a mode that drifted (a `cp` from a backup, a checkout of a dotfiles
 * repo) is silent until it is not.
 */
export function loadCredentials(env: NodeJS.ProcessEnv = process.env): Credentials | null {
  const path = credentialsPath(env)
  let raw: string
  try {
    const st = statSync(path)
    if ((st.mode & 0o077) !== 0) {
      throw new CliError(
        `${path} is readable by other users (mode ${(st.mode & 0o777).toString(8)}); ` +
          'run `chmod 600` on it or `pilot login` again',
      )
    }
    raw = readFileSync(path, 'utf8')
  } catch (err) {
    if (err instanceof CliError) throw err
    if ((err as NodeJS.ErrnoException).code === 'ENOENT') return null
    throw new CliError(`cannot read ${path}: ${(err as Error).message}`)
  }
  try {
    return JSON.parse(raw) as Credentials
  } catch {
    throw new CliError(`${path} is not valid JSON; run \`pilot login\` again`)
  }
}

/**
 * Writes the credentials file atomically at mode 0600.
 *
 * Written to a sibling temp name and renamed, so a crash mid-write leaves the
 * previous key in place rather than a truncated file that logs the user out.
 * The mode is set on the temp file BEFORE the rename: a chmod afterwards has a
 * window in which the key sits world-readable.
 */
export function saveCredentials(creds: Credentials, env: NodeJS.ProcessEnv = process.env): string {
  const path = credentialsPath(env)
  mkdirSync(dirname(path), { recursive: true, mode: 0o700 })
  chmodSync(dirname(path), 0o700)
  const tmp = `${path}.${process.pid}.tmp`
  writeFileSync(tmp, JSON.stringify(creds, null, 2) + '\n', { mode: 0o600 })
  chmodSync(tmp, 0o600)
  try {
    renameSync(tmp, path)
  } catch (err) {
    rmSync(tmp, { force: true })
    throw new CliError(`cannot write ${path}: ${(err as Error).message}`)
  }
  return path
}

export function clearCredentials(env: NodeJS.ProcessEnv = process.env): boolean {
  const path = credentialsPath(env)
  try {
    statSync(path)
  } catch {
    return false
  }
  rmSync(path, { force: true })
  return true
}

export interface GlobalOptions {
  json?: boolean
  apiUrl?: string
}

/** The API key, from the environment first and the file second. */
export function resolveApiKey(env: NodeJS.ProcessEnv = process.env): string | null {
  if (env.PILOT_API_KEY) return env.PILOT_API_KEY
  return loadCredentials(env)?.api_key ?? null
}

/** The fleet, from `--api-url`, then `PILOT_API_URL`, then the file, then the default. */
export function resolveApiUrl(opts: GlobalOptions = {}, env: NodeJS.ProcessEnv = process.env): string {
  return opts.apiUrl || env.PILOT_API_URL || loadCredentials(env)?.api_url || DEFAULT_API_URL
}

/**
 * Builds the SDK client. Makes no request: an unreachable fleet is discovered
 * by the command that needed it, naming the route it was calling.
 */
export function clientFromEnv(
  opts: GlobalOptions = {},
  env: NodeJS.ProcessEnv = process.env,
  clientOpts: ClientOptions = {},
): PilotsClient {
  const key = resolveApiKey(env)
  if (!key) {
    throw new CliError('no API key: run `pilot login` or set PILOT_API_KEY')
  }
  return new PilotsClient(key, { baseURL: resolveApiUrl(opts, env), ...clientOpts })
}
