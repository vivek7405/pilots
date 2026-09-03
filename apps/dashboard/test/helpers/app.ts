/**
 * One boot per test file: env, an in-memory database with the real migrations
 * applied, a fake fleet, and the framework's own request pipeline.
 *
 * Every layer drives `createRequestHandler().handle`, which runs middleware,
 * routing, SSR, form-dispatched actions, the action RPC endpoint, auth and
 * CSRF. That is the point: a route tested by importing its handler directly
 * would skip the rate limiter, the session gate and the segment middleware,
 * which is most of what these routes are.
 */

import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { createRequestHandler } from '@webjsdev/server';
import { getSetCookies } from '@webjsdev/server/testing';
import type { RouteHandlerContext } from '@webjsdev/core';
import { makeFakeFleet } from './fake-fleet.ts';
import type { FakeFleet } from './fake-fleet.ts';

export const APP_DIR = new URL('../../', import.meta.url).pathname.replace(/\/$/, '');

/** The env every boot needs. Set before anything imports `env.ts` or auth. */
export function setTestEnv(overrides: Record<string, string> = {}): void {
  const defaults: Record<string, string> = {
    NODE_ENV: 'test',
    DATABASE_URL: 'file::memory:',
    AUTH_SECRET: 'test-secret-at-least-thirty-two-characters-long',
    AUTH_GITHUB_ID: 'Iv1.testclientid',
    AUTH_GITHUB_SECRET: 'test-client-secret',
    PILOT_ADMIN_KEY: 'pilot_testadminkey',
    PILOT_API_URL: 'https://api.pilots.test',
    PILOT_USAGE_POLL: '0',
  };
  for (const [k, v] of Object.entries({ ...defaults, ...overrides })) process.env[k] = v;
}

/**
 * Apply every migration to the in-memory database.
 *
 * `webjs db migrate` runs in its own process against a FILE, so it can never
 * reach a `:memory:` database this process opened. Executing the same
 * committed SQL on the live handle is the only version of "migrated" that is
 * true here, and it is the same bytes production applies.
 */
export async function migrate(): Promise<void> {
  const { db } = await import('#db/connection.server.ts');
  const client = (db as unknown as { $client: { exec(sql: string): unknown } }).$client;
  const dir = join(APP_DIR, 'db', 'migrations');
  for (const name of readdirSync(dir).sort()) {
    const sql = readFileSync(join(dir, name, 'migration.sql'), 'utf8');
    for (const statement of sql.split('--> statement-breakpoint')) {
      const trimmed = statement.trim();
      if (trimmed) client.exec(trimmed);
    }
  }
}

/** The framework's own handle signature: it may answer synchronously. */
export type Handle = (req: Request) => Response | Promise<Response>;

export interface TestApp {
  handle: Handle;
  fleet: FakeFleet;
}

/**
 * Boot the app with a fake fleet installed.
 *
 * The fleet override is read by `modules/fleet/client.server.ts` BEFORE it
 * constructs a real client, so no test ever needs a reachable host and no test
 * can accidentally reach one.
 */
export async function bootApp(overrides: Record<string, string> = {}): Promise<TestApp> {
  setTestEnv(overrides);
  const fleet = makeFakeFleet();
  (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet = fleet;
  await migrate();
  const app = await createRequestHandler({ appDir: APP_DIR, dev: true });
  return { handle: app.handle, fleet };
}

/**
 * Sign in as a GitHub user by driving the REAL OAuth callback.
 *
 * There is no shortcut here on purpose: minting a cookie by hand would test a
 * cookie format rather than the sign-in path, and the `signIn` callback that
 * creates the user, org and membership rows only runs on this path.
 */
export async function signInAs(
  handle: TestApp['handle'],
  profile: { id: number | string; login: string; name?: string | null; email?: string | null; avatar_url?: string | null },
): Promise<string> {
  const start = await handle(new Request('http://localhost/api/auth/signin/github'));
  const location = start.headers.get('location');
  if (!location) throw new Error(`signin did not redirect: ${start.status}`);
  const state = new URL(location).searchParams.get('state');
  const stateCookie = getSetCookies(start).join('; ');

  const realFetch = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.startsWith('https://github.com/login/oauth/access_token')) {
      return new Response(JSON.stringify({ access_token: 'gho_test' }), {
        headers: { 'content-type': 'application/json' },
      });
    }
    if (url === 'https://api.github.com/user') {
      return new Response(JSON.stringify(profile), { headers: { 'content-type': 'application/json' } });
    }
    return new Response('{}', { status: 404 });
  }) as typeof globalThis.fetch;

  try {
    const cb = await handle(
      new Request(`http://localhost/api/auth/callback/github?code=abc&state=${state}`, {
        headers: { cookie: stateCookie },
      }),
    );
    const session = getSetCookies(cb).find((c) => c.startsWith('webjs.auth='));
    if (!session) throw new Error(`callback set no session cookie (status ${cb.status})`);
    return session.split(';')[0];
  } finally {
    globalThis.fetch = realFetch;
  }
}

/** A `RequestInit` carrying a session cookie and a forwarded client IP. */
export function asUser(cookie: string, init: RequestInit = {}, ip = '203.0.113.9'): RequestInit {
  return {
    ...init,
    headers: { ...(init.headers as Record<string, string>), cookie, 'x-forwarded-for': ip },
  };
}

/**
 * A `RouteHandlerContext` for calling a `WS` export directly.
 *
 * `params` is `Awaitable<T>` -- readable synchronously AND awaitable -- so a
 * plain object is not one. Building the real shape here keeps the test calling
 * the handler with exactly what the framework passes it.
 */
export function routeCtx(params: Record<string, string>): RouteHandlerContext {
  const awaitable = Object.assign({ ...params }, {
    then: <R>(onfulfilled?: ((value: Record<string, string>) => R | PromiseLike<R>) | null) =>
      Promise.resolve(params).then(onfulfilled),
  }) as RouteHandlerContext['params'];
  return { params: awaitable };
}

/** `Promise.withResolvers`, which the app's `lib` target predates. */
export function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}
