/**
 * The auth catch-all. It sits at the APP ROOT because `createAuth` hardcodes
 * `/api/auth/signin/*` and `/api/auth/callback/*`, so the GitHub App's callback
 * URL is `https://pilots.run/api/auth/callback/github` and nothing else will do.
 *
 * Sign in is a plain `<a href="/api/auth/signin/github">`; the GET handler turns
 * it into the OAuth redirect. Sign out is a `<form method="POST">` to
 * `/api/auth/signout`. Both work with JavaScript off.
 *
 * `?next=` on the sign-in link is where a visitor was going when the gate
 * bounced them. The framework's callback always lands on `/`, so the target
 * rides a short-lived cookie across the round trip to GitHub and is applied
 * here on the way back. The cookie is client-controlled like any other, so it
 * is checked against the same same-origin rule on BOTH sides: a value that
 * fails is dropped, never repaired.
 */
import { handlers } from '#modules/auth/auth.server.ts';
import { readCookie } from '#modules/auth/session.server.ts';
import { localPath } from '#lib/utils/local-path.ts';

export const NEXT_COOKIE = 'pilots_next';

const SIGNIN = '/api/auth/signin/github';
const CALLBACK = '/api/auth/callback/github';

export async function GET(req: Request): Promise<Response> {
  const url = new URL(req.url);
  const res = await handlers.GET(req);

  if (url.pathname === SIGNIN && res.status === 302) {
    const next = localPath(url.searchParams.get('next'), '');
    if (!next) return res;
    return withHeaders(res, (h) => h.append('set-cookie', nextCookie(next)));
  }

  if (url.pathname === CALLBACK && res.status === 302) {
    const carried = readCookie(req, NEXT_COOKIE);
    if (carried === null) return res;
    const next = localPath(carried, '');
    return withHeaders(res, (h) => {
      // Only the framework's own landing is overridden. A failed sign-in goes
      // to `pages.error` and keeps going there.
      if (next && h.get('location') === '/') h.set('location', next);
      h.append('set-cookie', clearNextCookie());
    });
  }

  return res;
}

export const POST = handlers.POST;

function nextCookie(next: string): string {
  // Scoped to the auth routes: nothing else reads it, and it expires before
  // the OAuth state cookie does.
  return `${NEXT_COOKIE}=${encodeURIComponent(next)}; Path=/api/auth; HttpOnly; SameSite=Lax; Max-Age=300`;
}

function clearNextCookie(): string {
  return `${NEXT_COOKIE}=; Path=/api/auth; HttpOnly; SameSite=Lax; Max-Age=0`;
}

/** A copy of the response with its headers edited; a `Response`'s own are immutable. */
function withHeaders(res: Response, edit: (headers: Headers) => void): Response {
  const headers = new Headers(res.headers);
  edit(headers);
  return new Response(res.body, { status: res.status, statusText: res.statusText, headers });
}
