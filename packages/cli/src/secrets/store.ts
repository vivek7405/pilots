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

import { envVarFor } from '../compose/secrets.ts'
import { loadCredentials, saveCredentials, type Credentials } from '../config.ts'
import { parseDotEnv } from '../env.ts'
import { CliError } from '../output.ts'

export interface SecretName {
  name: string
  /** The first 8 hex characters of SHA-256 of the value: enough to tell two machines agree, never enough to recover it. */
  digest: string
}

/**
 * Why the refusal, rather than writing a file anyway.
 *
 * `PILOT_API_KEY` authenticates a request and this command makes none: a
 * secret is written to the credentials file, never sent. Writing that file
 * from the variable would either persist an API key nobody asked to persist,
 * or leave a file with no `api_key` in it, which `whoami` dereferences.
 *
 * So the message names the two things that do work on a machine with no file:
 * `pilot login` on a laptop, and the environment variable on a CI runner,
 * where storing a secret in a file the job throws away buys nothing anyway.
 */
function notLoggedIn(varName: string): string {
  return (
    'no credentials file to store a secret in: run `pilot login` first. ' +
    'PILOT_API_KEY signs requests but a secret is stored in the file rather than sent, ' +
    `so on a machine with no file export ${varName} for the deploy instead`
  )
}

/**
 * Refuses a name no `secret://` reference could carry.
 *
 * Exported so a caller can check before it reads a value: a `set` that
 * validated afterwards would have the user type a whole secret at the prompt
 * and then throw it away.
 *
 * An allowed set rather than a banned one. `import` takes whatever key Node's
 * `.env` parser hands back, and that parser is happy to produce `A B` from a
 * line that lost its `=` and `"QK"` from a quoted key. Listing the characters
 * that a `secret://` reference can carry refuses both, where banning spaces
 * and `=` only refused the first. Every name in this repo's compose files and
 * docs is inside this set.
 */
const SECRET_NAME = /^[A-Za-z0-9_][A-Za-z0-9_.-]*$/

export function assertSecretName(name: string): void {
  if (!SECRET_NAME.test(name)) {
    throw new CliError(
      'a secret name is what secret://<name> carries in the compose file, so letters, digits, ' +
        `underscore, dot and dash; got ${JSON.stringify(name)}`,
    )
  }
}

export function setSecret(app: string, name: string, value: string, env: NodeJS.ProcessEnv = process.env): void {
  assertSecretName(name)
  const creds = requireCredentials(env, envVarFor(name))
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
  // The same rule `set` applies, because there is one answer to what a secret
  // name is. `parseEnv` will hand back `A B` for a line that lost its `=`, and
  // a name with a space in it is one no `secret://` reference can address and
  // one `ls` renders across two columns.
  for (const name of names) assertSecretName(name)
  const creds = requireCredentials(env, names.map(envVarFor).join(', '))
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

function requireCredentials(env: NodeJS.ProcessEnv, varName: string): Credentials {
  const creds = loadCredentials(env)
  if (!creds) throw new CliError(notLoggedIn(varName))
  return creds
}

/** The same merge `pilot add postgres` does: other apps and the key untouched. */
function withSecrets(creds: Credentials, app: string, pairs: Record<string, string>): Credentials {
  return {
    ...creds,
    secrets: { ...creds.secrets, [app]: { ...creds.secrets?.[app], ...pairs } },
  }
}
