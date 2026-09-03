/**
 * `pilot login`: the device flow's timing and its error branches.
 *
 * The poll loop is the whole risk surface here. Its timing is asserted through
 * an injected `sleep` that records rather than waits, and every GitHub error
 * code gets its own branch, because the difference between "keep polling",
 * "ask for a new code" and "stop" is invisible until the wrong one is chosen.
 */

import { strict as assert } from 'node:assert'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { after, test } from 'node:test'

import { loadCredentials, saveCredentials } from '../src/config.ts'
import { deviceFlow, exchangeToken } from '../src/github.ts'
import { CliError } from '../src/output.ts'
import { json, startServer, type FakeServer } from './helpers/server.ts'

const roots: string[] = []
after(() => {
  for (const dir of roots) rmSync(dir, { recursive: true, force: true })
})

function scratch(): NodeJS.ProcessEnv {
  const dir = mkdtempSync(join(tmpdir(), 'pilot-login-'))
  roots.push(dir)
  return { XDG_CONFIG_HOME: dir }
}

/**
 * A GitHub that answers the two device-flow endpoints from a script.
 *
 * `tokens` is consumed one entry per poll; the LAST entry repeats forever, so
 * a test that means "answers slow_down for ever" says exactly that.
 */
async function fakeGitHub(script: {
  deviceCodes?: unknown[]
  deviceCodeStatus?: number
  tokens: unknown[]
}): Promise<FakeServer & { polls: number; codesIssued: number }> {
  let polls = 0
  let codesIssued = 0
  const codes = script.deviceCodes ?? [
    { device_code: 'dc-1', user_code: 'ABCD-1234', verification_uri: 'https://github.com/login/device', expires_in: 900, interval: 5 },
  ]
  const server = await startServer((req, res) => {
    assert.equal(req.headers.accept, 'application/json', 'github needs Accept: application/json to answer JSON')
    assert.equal(req.headers['content-type'], 'application/x-www-form-urlencoded')
    const form = new URLSearchParams(req.body)
    if (req.path === '/login/device/code') {
      assert.ok(form.get('client_id'), 'the device code request carries the client id')
      assert.ok(form.get('scope'), 'the device code request carries the scope')
      assert.equal(form.get('client_secret'), null, 'a public client ships no secret')
      const body = codes[Math.min(codesIssued, codes.length - 1)]
      codesIssued++
      return json(res, script.deviceCodeStatus ?? 200, body)
    }
    if (req.path === '/login/oauth/access_token') {
      assert.equal(form.get('grant_type'), 'urn:ietf:params:oauth:grant-type:device_code')
      assert.ok(form.get('device_code'))
      assert.equal(form.get('client_secret'), null, 'a public client ships no secret')
      const body = script.tokens[Math.min(polls, script.tokens.length - 1)]
      polls++
      return json(res, 200, body)
    }
    return json(res, 404, { error: 'not found' })
  })
  // Defined rather than Object.assign'd: assign copies a getter's VALUE at
  // the moment of the call, which would freeze both counters at zero.
  return Object.defineProperties(server, {
    polls: { get: () => polls },
    codesIssued: { get: () => codesIssued },
  }) as FakeServer & { polls: number; codesIssued: number }
}

function flowOptions(gh: FakeServer, sleeps: number[], extra: Record<string, unknown> = {}) {
  return {
    clientId: 'Iv1.test',
    deviceCodeURL: `${gh.url}/login/device/code`,
    accessTokenURL: `${gh.url}/login/oauth/access_token`,
    sleep: async (ms: number) => {
      sleeps.push(ms)
    },
    prompt: () => {},
    maxPolls: 20,
    ...extra,
  }
}

test('slow_down adopts the interval GitHub sends, and the flow completes', async () => {
  const gh = await fakeGitHub({
    tokens: [
      { error: 'authorization_pending' },
      { error: 'slow_down', interval: 10 },
      { access_token: 'gho_token' },
    ],
  })
  const sleeps: number[] = []
  try {
    const token = await deviceFlow(flowOptions(gh, sleeps))
    assert.equal(token, 'gho_token')
    // The first two waits are the code's own interval; the third is the one
    // GitHub asked for. Keeping 5000 here is exactly the bug: GitHub answers
    // slow_down to every poll that arrives too soon, so the flow would never
    // finish.
    assert.deepEqual(sleeps, [5000, 5000, 10000])
  } finally {
    await gh.close()
  }
})

test('ignoring the new interval would never finish: the poll cap fires', async () => {
  // The counterfactual, with GitHub modelled the way GitHub actually behaves:
  // it answers slow_down to any poll that arrives less than its interval after
  // the previous one. The injected sleep advances a virtual clock the fake
  // reads, so "did the client slow down" is a real question here rather than a
  // stand-in for one.
  const clock = { now: 0 }
  let lastPoll = 0
  const gh = await startServer((req, res) => {
    if (req.path === '/login/device/code') {
      return json(res, 200, {
        device_code: 'dc-1', user_code: 'A', verification_uri: 'u', expires_in: 900, interval: 5,
      })
    }
    const gap = clock.now - lastPoll
    lastPoll = clock.now
    return json(res, 200, gap >= 10_000 ? { access_token: 'ok' } : { error: 'slow_down', interval: 10 })
  })

  try {
    const sleeps: number[] = []
    const tick = async (ms: number) => {
      clock.now += ms
      sleeps.push(ms)
    }
    const token = await deviceFlow({ ...flowOptions(gh, []), sleep: tick })
    assert.equal(token, 'ok')
    assert.deepEqual(sleeps, [5000, 10_000], 'the second wait is the interval GitHub asked for')

    // The same server, driven by a client that keeps its old interval. Modelled
    // by rewriting every slow_down back to the interval it already had, which
    // is what "ignored the field" means on the wire.
    clock.now = 0
    lastPoll = 0
    let polls = 0
    await assert.rejects(
      deviceFlow({
        ...flowOptions(gh, []),
        sleep: async (ms: number) => {
          clock.now += ms
          polls++
        },
        fetch: async (input: RequestInfo | URL, init?: RequestInit) => {
          const res = await fetch(input, init)
          const body = (await res.json()) as Record<string, unknown>
          if (body.error === 'slow_down') body.interval = 5
          return new Response(JSON.stringify(body), {
            status: res.status,
            headers: { 'content-type': 'application/json' },
          })
        },
      }),
      /gave up after 20 polls/,
    )
    assert.equal(polls, 20)
  } finally {
    await gh.close()
  }
})

test('an expired code is retried once, and a second expiry stops', async () => {
  const gh = await fakeGitHub({
    deviceCodes: [
      { device_code: 'dc-1', user_code: 'A', verification_uri: 'u', expires_in: 900, interval: 5 },
      { device_code: 'dc-2', user_code: 'B', verification_uri: 'u', expires_in: 900, interval: 5 },
    ],
    tokens: [{ error: 'expired_token' }, { access_token: 'gho_second' }],
  })
  const sleeps: number[] = []
  try {
    assert.equal(await deviceFlow(flowOptions(gh, sleeps)), 'gho_second')
    assert.equal(gh.codesIssued, 2, 'the expiry asked GitHub for a second code')
  } finally {
    await gh.close()
  }

  const always = await fakeGitHub({ tokens: [{ error: 'expired_token' }] })
  try {
    await assert.rejects(deviceFlow(flowOptions(always, [])), /expired twice/)
    assert.equal(always.codesIssued, 2, 'exactly two codes, never a third')
  } finally {
    await always.close()
  }
})

test('access_denied stops with "authorization denied"', async () => {
  const gh = await fakeGitHub({ tokens: [{ error: 'access_denied' }] })
  try {
    await assert.rejects(deviceFlow(flowOptions(gh, [])), (err: unknown) => {
      assert.ok(err instanceof CliError)
      assert.match(err.message, /authorization denied/)
      return true
    })
  } finally {
    await gh.close()
  }
})

test('device_flow_disabled is printed verbatim and never retried', async () => {
  // The App has "Enable Device Flow" off. It arrives on the device-code call,
  // not the poll, and no number of retries will change it.
  const gh = await fakeGitHub({
    deviceCodes: [{ error: 'device_flow_disabled', error_description: 'Device Flow has not been enabled' }],
    tokens: [],
  })
  try {
    await assert.rejects(deviceFlow(flowOptions(gh, [])), /device_flow_disabled: Device Flow has not been enabled/)
    assert.equal(gh.codesIssued, 1, 'no retry on an operator misconfiguration')
  } finally {
    await gh.close()
  }
})

test('an unknown poll error stops rather than looping', async () => {
  const gh = await fakeGitHub({ tokens: [{ error: 'incorrect_client_credentials' }] })
  try {
    await assert.rejects(deviceFlow(flowOptions(gh, [])), /incorrect_client_credentials/)
  } finally {
    await gh.close()
  }
})

test('with no client id, the flow names the headless path instead of calling GitHub', async () => {
  await assert.rejects(deviceFlow({ clientId: '', prompt: () => {} }), (err: unknown) => {
    assert.ok(err instanceof CliError)
    assert.match(err.message, /PILOT_GITHUB_CLIENT_ID/)
    assert.match(err.message, /pilot login --token/)
    return true
  })
})

test('the exchange posts github_access_token and returns the key', async () => {
  const dashboard = await startServer((req, res) => {
    assert.equal(req.path, '/api/cli/exchange')
    assert.equal(req.method, 'POST')
    assert.deepEqual(JSON.parse(req.body), { github_access_token: 'gho_token' })
    return json(res, 200, { api_key: 'pilot_live_1', org_id: 'org_7', scopes: ['deploy'] })
  })
  try {
    const result = await exchangeToken('gho_token', { dashboardURL: dashboard.url })
    assert.deepEqual(result, { api_key: 'pilot_live_1', org_id: 'org_7', scopes: ['deploy'] })

    const env = scratch()
    saveCredentials({ api_key: result.api_key, org_id: result.org_id, api_url: 'https://fleet' }, env)
    assert.deepEqual(loadCredentials(env), {
      api_key: 'pilot_live_1',
      org_id: 'org_7',
      api_url: 'https://fleet',
    })
  } finally {
    await dashboard.close()
  }
})

test('a rate-limited exchange prints the body once and does not retry', async () => {
  let calls = 0
  const dashboard = await startServer((_req, res) => {
    calls++
    return json(res, 429, { error: 'rate limited' })
  })
  try {
    await assert.rejects(
      exchangeToken('gho_token', { dashboardURL: dashboard.url }),
      /failed with 429: \{"error":"rate limited"\}/,
    )
    // The route allows 10/min per IP. A retry loop turns one bad answer into a
    // lockout, so there is exactly one call.
    assert.equal(calls, 1)
  } finally {
    await dashboard.close()
  }
})

test('`pilot login --token` writes the file and never calls GitHub', async () => {
  // The headless path, spawned as a real process so the assertion covers the
  // command wiring and not only the function it calls.
  const github = await startServer((_req, res) => json(res, 200, { error: 'should not be called' }))
  const dashboard = await startServer((_req, res) => json(res, 200, { error: 'should not be called' }))
  const env = scratch()
  try {
    const { execFile } = await import('node:child_process')
    const { promisify } = await import('node:util')
    const run = promisify(execFile)
    const BIN = join(import.meta.dirname, '..', 'bin', 'pilot.js')
    const { stdout } = await run(process.execPath, [BIN, '--json', 'login', '--token', 'pilot_headless', '--org', 'org_9'], {
      env: {
        ...env,
        PATH: process.env.PATH,
        PILOT_API_URL: 'https://fleet.example',
        PILOT_GITHUB_CLIENT_ID: 'Iv1.test',
        PILOT_DASHBOARD_URL: dashboard.url,
      },
    })
    assert.deepEqual(JSON.parse(stdout), { org_id: 'org_9', scopes: [], api_url: 'https://fleet.example' })
    assert.deepEqual(loadCredentials(env), {
      api_key: 'pilot_headless',
      api_url: 'https://fleet.example',
      org_id: 'org_9',
    })
    assert.equal(github.requests.length, 0, '--token is headless: no device flow')
    assert.equal(dashboard.requests.length, 0, '--token is headless: no exchange')
  } finally {
    await github.close()
    await dashboard.close()
  }
})

test('`pilot whoami` prints the key prefix and never the key', async () => {
  const env = scratch()
  saveCredentials({ api_key: 'pilot_secret_abcdefghijklmnop', org_id: 'org_3', api_url: 'https://f' }, env)
  const { execFile } = await import('node:child_process')
  const { promisify } = await import('node:util')
  const run = promisify(execFile)
  const BIN = join(import.meta.dirname, '..', 'bin', 'pilot.js')
  const { stdout } = await run(process.execPath, [BIN, '--json', 'whoami'], {
    env: { ...env, PATH: process.env.PATH },
  })
  const parsed = JSON.parse(stdout) as { key_prefix: string; org_id: string; api_url: string }
  assert.equal(parsed.org_id, 'org_3')
  assert.equal(parsed.api_url, 'https://f')
  assert.equal(parsed.key_prefix, 'pilot_secret')
  assert.equal(stdout.includes('abcdefghijklmnop'), false, 'the key itself never reaches stdout')
})
