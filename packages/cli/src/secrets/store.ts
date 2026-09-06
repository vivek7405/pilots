/**
 * The secret store: `credentials.secrets[app][name]` on this machine.
 *
 * Three operations and nothing else, with no terminal in the file: the
 * `pilot secrets` command renders these, and the MCP tools call the same three
 * functions, so a value has exactly one write path to audit.
 *
 * Every write goes through `saveCredentials`, which is the atomic 0600 write
 * and rewrites the whole file, so each operation merges into what is there.
 * A `set` that forgot the merge would log the user out.
 */

import { createHash } from 'node:crypto'
import { readFileSync } from 'node:fs'

import { loadCredentials, saveCredentials, type Credentials } from '../config.ts'
import { parseDotEnv } from '../env.ts'
import { CliError } from '../output.ts'

export interface SecretName {
  name: string
  /** The first 8 hex characters of SHA-256 of the value: enough to tell two machines agree, never enough to recover it. */
  digest: string
}

const NOT_LOGGED_IN = 'not logged in: run `pilot login` first; secrets are stored beside the API key'

export function setSecret(app: string, name: string, value: string, env: NodeJS.ProcessEnv = process.env): void {
  if (name === '' || /\s|=/.test(name)) {
    throw new CliError(`a secret name is what secret://<name> carries in the compose file; got ${JSON.stringify(name)}`)
  }
  const creds = requireCredentials(env)
  saveCredentials(withSecrets(creds, app, { [name]: value }), env)
}

/** Every pair in a `.env` file (or stdin when `file` is `-`), merged in. Returns the names, sorted. */
export function importSecrets(app: string, file: string, env: NodeJS.ProcessEnv = process.env): string[] {
  let text: string
  try {
    text = readFileSync(file === '-' ? 0 : file, 'utf8')
  } catch (err) {
    throw new CliError(`cannot read ${file}: ${(err as Error).message}`)
  }
  const pairs = parseDotEnv(text)
  const names = Object.keys(pairs).sort()
  if (names.length === 0) throw new CliError(`${file === '-' ? 'stdin' : file} holds no KEY=value lines`)
  const creds = requireCredentials(env)
  saveCredentials(withSecrets(creds, app, pairs), env)
  return names
}

/** Names and digests, sorted by name. Never a value. */
export function listSecretNames(app: string, env: NodeJS.ProcessEnv = process.env): SecretName[] {
  const stored = loadCredentials(env)?.secrets?.[app] ?? {}
  return Object.keys(stored)
    .sort()
    .map((name) => ({ name, digest: digestOf(stored[name]!) }))
}

export function digestOf(value: string): string {
  return createHash('sha256').update(value).digest('hex').slice(0, 8)
}

function requireCredentials(env: NodeJS.ProcessEnv): Credentials {
  const creds = loadCredentials(env)
  if (!creds) throw new CliError(NOT_LOGGED_IN)
  return creds
}

/** The same merge `pilot add postgres` does: other apps and the key untouched. */
function withSecrets(creds: Credentials, app: string, pairs: Record<string, string>): Credentials {
  return {
    ...creds,
    secrets: { ...creds.secrets, [app]: { ...creds.secrets?.[app], ...pairs } },
  }
}
