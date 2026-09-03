/**
 * The GitHub device flow.
 *
 * The device flow is the OAuth grant designed for a client that cannot keep a
 * secret, which is exactly what a CLI on someone's laptop is. So this ships a
 * `client_id` and NO client secret; a CLI that carried one would be handing
 * every user the fleet's credential and calling it configuration.
 *
 * `fetch` and `sleep` are injected because the interesting behaviour is
 * entirely in the poll loop's timing and error branches, and a test that has
 * to wait real seconds to exercise them is a test nobody runs.
 */

import { CliError } from './output.ts'

export const DEVICE_CODE_URL = 'https://github.com/login/device/code'
export const ACCESS_TOKEN_URL = 'https://github.com/login/oauth/access_token'

/**
 * The scopes the exchange needs to identify a person: who they are and which
 * verified email to attach the org to. Nothing that can write to a repo.
 */
export const DEFAULT_SCOPE = 'read:user user:email'

/**
 * The public client id of the pilots GitHub App.
 *
 * Empty until the App is registered (#33 owns it, alongside the exchange
 * route). It is read from the environment rather than compiled in as a
 * placeholder, because a wrong-but-present id fails inside GitHub's poll loop
 * with `incorrect_client_credentials`, and an operator reading that has no way
 * to tell a misconfigured fleet from a broken CLI.
 */
export function defaultClientId(env: NodeJS.ProcessEnv = process.env): string {
  return env.PILOT_GITHUB_CLIENT_ID ?? ''
}

export type FetchLike = typeof globalThis.fetch

export interface DeviceFlowOptions {
  clientId: string
  scope?: string
  fetch?: FetchLike
  /** Injected so the tests do not spend the interval they are asserting on. */
  sleep?: (ms: number) => Promise<void>
  /** Where the "open this URL" prompt goes. stderr by default; never stdout. */
  prompt?: (message: string) => void
  deviceCodeURL?: string
  accessTokenURL?: string
  /**
   * A hard ceiling on polls per device code. GitHub's own `expires_in` bounds
   * the loop in practice; this bounds it when a server answers `slow_down`
   * forever, which is what a client that ignores the new interval provokes.
   */
  maxPolls?: number
}

interface DeviceCodeResponse {
  device_code: string
  user_code: string
  verification_uri: string
  expires_in: number
  interval: number
}

interface AccessTokenResponse {
  access_token?: string
  error?: string
  error_description?: string
  interval?: number
}

/** Resolves with a GitHub access token, or throws a CliError naming why not. */
export async function deviceFlow(opts: DeviceFlowOptions): Promise<string> {
  const doFetch = opts.fetch ?? globalThis.fetch
  const sleep = opts.sleep ?? ((ms: number) => new Promise<void>((r) => setTimeout(r, ms)))
  const prompt = opts.prompt ?? ((m: string) => process.stderr.write(m + '\n'))
  const deviceCodeURL = opts.deviceCodeURL ?? DEVICE_CODE_URL
  const accessTokenURL = opts.accessTokenURL ?? ACCESS_TOKEN_URL
  const maxPolls = opts.maxPolls ?? 200

  if (!opts.clientId) {
    throw new CliError(
      'no GitHub client id: set PILOT_GITHUB_CLIENT_ID, or use `pilot login --token <key>`',
    )
  }

  // Two codes at most. A code that expires while someone walks to another
  // machine is ordinary and deserves a second one; a second expiry means
  // nobody is going to authorise this, and looping would keep a dead terminal
  // polling GitHub until it is closed.
  for (let attempt = 0; attempt < 2; attempt++) {
    const code = await requestDeviceCode(doFetch, deviceCodeURL, opts.clientId, opts.scope ?? DEFAULT_SCOPE)
    prompt(`Open ${code.verification_uri} and enter code ${code.user_code}`)

    let interval = Math.max(1, code.interval || 5)
    let expired = false

    for (let poll = 0; poll < maxPolls; poll++) {
      await sleep(interval * 1000)
      const body = await requestAccessToken(doFetch, accessTokenURL, opts.clientId, code.device_code)

      if (body.access_token) return body.access_token

      switch (body.error) {
        case 'authorization_pending':
          continue
        case 'slow_down':
          // GitHub sends the NEW, larger interval and expects it to be
          // adopted. Keeping the old one makes every subsequent poll answer
          // `slow_down` too, and the flow never completes.
          interval = body.interval ?? interval + 5
          continue
        case 'expired_token':
          expired = true
          break
        case 'access_denied':
          throw new CliError('authorization denied')
        default:
          throw new CliError(
            `github: ${body.error ?? 'unknown error'}` +
              (body.error_description ? `: ${body.error_description}` : ''),
          )
      }
      if (expired) break
    }

    if (!expired) {
      throw new CliError(`gave up after ${maxPolls} polls without an answer from github`)
    }
  }

  throw new CliError('the device code expired twice; run `pilot login` again')
}

async function requestDeviceCode(
  doFetch: FetchLike,
  url: string,
  clientId: string,
  scope: string,
): Promise<DeviceCodeResponse> {
  const res = await doFetch(url, {
    method: 'POST',
    headers: { accept: 'application/json', 'content-type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams({ client_id: clientId, scope }).toString(),
  })
  const text = await res.text()
  if (!res.ok) throw new CliError(`github device code request failed with ${res.status}: ${text}`)
  let parsed: DeviceCodeResponse & { error?: string; error_description?: string }
  try {
    parsed = JSON.parse(text) as typeof parsed
  } catch {
    throw new CliError(`github device code request returned non-JSON: ${text}`)
  }
  if (parsed.error) {
    // `device_flow_disabled` lands here: the App has "Enable Device Flow" off,
    // which is an operator mistake and never something a retry fixes.
    throw new CliError(
      `github: ${parsed.error}` + (parsed.error_description ? `: ${parsed.error_description}` : ''),
    )
  }
  if (!parsed.device_code || !parsed.user_code) {
    throw new CliError(`github device code response was missing fields: ${text}`)
  }
  return parsed
}

async function requestAccessToken(
  doFetch: FetchLike,
  url: string,
  clientId: string,
  deviceCode: string,
): Promise<AccessTokenResponse> {
  const res = await doFetch(url, {
    method: 'POST',
    headers: { accept: 'application/json', 'content-type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams({
      client_id: clientId,
      device_code: deviceCode,
      grant_type: 'urn:ietf:params:oauth:grant-type:device_code',
    }).toString(),
  })
  const text = await res.text()
  try {
    return JSON.parse(text) as AccessTokenResponse
  } catch {
    throw new CliError(`github token response was non-JSON (${res.status}): ${text}`)
  }
}

export interface ExchangeResult {
  api_key: string
  org_id: string
  scopes: string[]
}

/**
 * Trades a GitHub access token for a pilots API key at the dashboard.
 *
 * The one call in the CLI that talks to the dashboard, and it happens exactly
 * once per login. A non-200 is printed with its body and never retried in a
 * loop: the route is rate-limited at 10/min per IP, so a retry loop turns one
 * bad response into a lockout.
 */
export async function exchangeToken(
  githubToken: string,
  opts: { dashboardURL?: string; fetch?: FetchLike; env?: NodeJS.ProcessEnv } = {},
): Promise<ExchangeResult> {
  const env = opts.env ?? process.env
  const base = (opts.dashboardURL || env.PILOT_DASHBOARD_URL || 'https://pilots.run').replace(/\/+$/, '')
  const doFetch = opts.fetch ?? globalThis.fetch
  const res = await doFetch(`${base}/api/cli/exchange`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', accept: 'application/json' },
    body: JSON.stringify({ github_access_token: githubToken }),
  })
  const text = await res.text()
  if (!res.ok) {
    throw new CliError(`${base}/api/cli/exchange failed with ${res.status}: ${text}`)
  }
  let parsed: Partial<ExchangeResult>
  try {
    parsed = JSON.parse(text) as Partial<ExchangeResult>
  } catch {
    throw new CliError(`the exchange returned non-JSON: ${text}`)
  }
  if (!parsed.api_key || !parsed.org_id) {
    throw new CliError(`the exchange response was missing api_key or org_id: ${text}`)
  }
  return { api_key: parsed.api_key, org_id: parsed.org_id, scopes: parsed.scopes ?? [] }
}
