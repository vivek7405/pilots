/**
 * `pilot login`: the device flow, the exchange, and the file.
 *
 * Three ways in, one file out. The interactive path is the device flow; the
 * headless paths are `--token` (which never touches GitHub) and the
 * `PILOT_API_KEY` environment variable (which never touches this command).
 */

import { Command } from 'commander'

import { PilotsClient } from '@pilots/sdk'

import {
  clearCredentials,
  credentialsPath,
  DEFAULT_API_URL,
  loadCredentials,
  resolveApiKeySource,
  resolveApiUrlSource,
  saveCredentials,
  type GlobalOptions,
} from '../config.ts'
import { defaultClientId, deviceFlow, exchangeToken } from '../github.ts'
import { promptSecret } from '../prompt.ts'
import { CliError, messageOf, note, printJSON, printTable } from '../output.ts'

/**
 * How long `whoami` waits for the fleet.
 *
 * Short on purpose. The command's job is to show configuration, and the
 * situation people run it in most is one where the fleet is not answering.
 */
const WHOAMI_TIMEOUT_MS = 3000

/** Injectable so a test can drive the prompt without a terminal. */
export type SecretPrompt = (message: string, hint: string) => Promise<string>

export function createLoginCommand(
  prompt: SecretPrompt = promptSecret,
  stdinIsTTY: () => boolean = () => Boolean(process.stdin.isTTY),
): Command {
  return new Command('login')
    .description('authenticate with GitHub and store a pilots API key')
    .option('--token <key>', 'skip GitHub and store this API key directly (headless)')
    .option('--org <id>', 'the org id to record alongside a --token key')
    .action(async function (this: Command) {
      const opts = this.optsWithGlobals() as GlobalOptions & { token?: string; org?: string }
      const apiUrl = opts.apiUrl || process.env.PILOT_API_URL || DEFAULT_API_URL

      if (opts.token) {
        // Deliberately not validated against anything. A `--token` login that
        // phoned home would fail on a fleet whose dashboard is down, which is
        // precisely the situation this flag exists for.
        const path = saveCredentials({
          api_key: opts.token,
          api_url: apiUrl,
          ...(opts.org ? { org_id: opts.org } : {}),
          ...(loadCredentials()?.secrets ? { secrets: loadCredentials()!.secrets } : {}),
        })
        if (opts.json) printJSON({ org_id: opts.org ?? null, scopes: [], api_url: apiUrl })
        else note(`API key stored in ${path}`)
        return
      }

      const clientId = defaultClientId()
      // No GitHub App configured, but somebody is sitting at a terminal: ask
      // for the key rather than telling them to rerun with a flag they would
      // then have to paste into their shell history.
      if (!clientId && stdinIsTTY()) {
        const typed = (await prompt('API key: ', 'or run pilot login --token <key>')).trim()
        if (!typed) {
          throw new CliError('no API key entered', { hint: 'run pilot login --token <key>' })
        }
        const path = saveCredentials({
          api_key: typed,
          api_url: apiUrl,
          ...(loadCredentials()?.secrets ? { secrets: loadCredentials()!.secrets } : {}),
        })
        if (opts.json) printJSON({ org_id: null, scopes: [], api_url: apiUrl })
        else note(`API key stored in ${path}`)
        return
      }
      const githubToken = await deviceFlow({ clientId })
      const result = await exchangeToken(githubToken)

      const existing = loadCredentials()
      const path = saveCredentials({
        api_key: result.api_key,
        api_url: apiUrl,
        org_id: result.org_id,
        ...(existing?.secrets ? { secrets: existing.secrets } : {}),
      })

      if (opts.json) {
        printJSON({ org_id: result.org_id, scopes: result.scopes, api_url: apiUrl })
      } else {
        note(`Logged in as ${result.org_id}`)
        note(`API key stored in ${path}`)
      }
    })
}

export function createLogoutCommand(): Command {
  return new Command('logout')
    .description('remove the stored credentials')
    .action(function (this: Command) {
      const opts = this.optsWithGlobals() as GlobalOptions
      const removed = clearCredentials()
      if (opts.json) printJSON({ removed, path: credentialsPath() })
      else note(removed ? `removed ${credentialsPath()}` : 'not logged in')
    })
}

/**
 * `pilot whoami`: which credentials every other command is using, and why.
 *
 * The precedence is real and it is invisible everywhere else: PILOT_API_KEY
 * beats the file, and --api-url beats PILOT_API_URL beats the file beats the
 * default. A file holding one key while the environment holds another is a
 * normal state to be in and an unreadable one to debug, so this command names
 * the source that won for every value it prints.
 *
 * The org comes from the fleet when the fleet answers, because the file
 * records only what login happened to store and says nothing at all about a
 * key that came from the environment.
 */
export function createWhoamiCommand(): Command {
  return new Command('whoami')
    .description('which key, fleet and org every other command uses, and where each came from')
    .action(async function (this: Command) {
      const opts = this.optsWithGlobals() as GlobalOptions
      const key = resolveApiKeySource()
      if (!key) throw new CliError('not logged in', { hint: 'run pilot login, or set PILOT_API_KEY' })
      const fleet = resolveApiUrlSource(opts)
      const file = loadCredentials()
      if (key.source === 'PILOT_API_KEY' && file?.api_key && file.api_key !== key.key) {
        note(`${credentialsPath()} also holds a key; PILOT_API_KEY wins`)
      }

      let org: string | null = file?.org_id ?? null
      let orgSource: string | null = org ? credentialsPath() : null
      let scopes: string[] | null = null
      try {
        const me = await new PilotsClient(key.key, {
          baseURL: fleet.url,
          timeoutMs: WHOAMI_TIMEOUT_MS,
        }).whoami()
        org = me.org_id || null
        orgSource = 'fleet'
        scopes = me.scopes
      } catch (err) {
        // Exit 0 anyway. A command that fails when the fleet is down is
        // useless in the one situation people reach for it.
        note(`the fleet at ${fleet.url} did not answer (${messageOf(err)}); org is from the file`)
      }

      // The prefix, never the key. `whoami` is the command someone runs while
      // screen-sharing to work out which fleet they are on.
      const prefix = key.key.slice(0, 12)
      if (opts.json) {
        printJSON({
          org_id: org,
          org_source: orgSource,
          api_url: fleet.url,
          api_url_source: fleet.source,
          key_prefix: prefix,
          key_source: key.source,
          scopes,
        })
        return
      }
      printTable([
        [
          'ORG',
          org ?? (orgSource === 'fleet' ? '(admin key, no org)' : '(unknown)'),
          orgSource ? `from ${orgSource}` : 'not recorded',
        ],
        ['FLEET', fleet.url, `from ${fleet.source}`],
        ['KEY', `${prefix}...`, `from ${key.source}`],
        ['SCOPES', scopes ? scopes.join(',') : '(fleet unreachable)', scopes ? 'from fleet' : ''],
      ])
    })
}
