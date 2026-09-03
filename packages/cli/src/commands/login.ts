/**
 * `pilot login`: the device flow, the exchange, and the file.
 *
 * Three ways in, one file out. The interactive path is the device flow; the
 * headless paths are `--token` (which never touches GitHub) and the
 * `PILOT_API_KEY` environment variable (which never touches this command).
 */

import { Command } from 'commander'

import {
  clearCredentials,
  credentialsPath,
  DEFAULT_API_URL,
  loadCredentials,
  saveCredentials,
  type GlobalOptions,
} from '../config.ts'
import { defaultClientId, deviceFlow, exchangeToken } from '../github.ts'
import { CliError, note, printJSON, printTable } from '../output.ts'

export function createLoginCommand(): Command {
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

export function createWhoamiCommand(): Command {
  return new Command('whoami')
    .description('print the stored org, fleet and key prefix (reads the file only)')
    .action(function (this: Command) {
      const opts = this.optsWithGlobals() as GlobalOptions
      const creds = loadCredentials()
      if (!creds) throw new CliError('not logged in: run `pilot login` or set PILOT_API_KEY')
      // The prefix, never the key. `whoami` is the command someone runs while
      // screen-sharing to work out which fleet they are on.
      const prefix = creds.api_key.slice(0, 12)
      const apiUrl = opts.apiUrl || process.env.PILOT_API_URL || creds.api_url || DEFAULT_API_URL
      if (opts.json) {
        printJSON({ org_id: creds.org_id ?? null, api_url: apiUrl, key_prefix: prefix })
      } else {
        printTable([
          ['ORG', creds.org_id ?? '(unknown)'],
          ['FLEET', apiUrl],
          ['KEY', `${prefix}...`],
        ])
      }
    })
}
