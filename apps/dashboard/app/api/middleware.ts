/**
 * Everything under `/api/`: rate limit first, then the session gate.
 *
 * `trustProxy: true` is required here and is not optional. The pilots router
 * fronts this app, so the socket peer is the router on every request; without
 * it a single shared proxy buckets every visitor together and the limit stops
 * meaning anything. Its precondition is that the router STRIPS an inbound
 * `X-Forwarded-For` before appending the peer -- see README.md, which records
 * that this is not yet true in `apps/hostd/internal/router/router.go`.
 *
 * No `clientIpHeader`: that option is for a CDN that sets and overwrites its
 * own header. The pilots router is the edge and `pilots.run` is DNS-only, so
 * the default leftmost-entry chain is exactly right once the router owns the
 * header.
 *
 * Two subtrees skip the session gate and keep the rate limit: `/api/auth/**`
 * is how a visitor BECOMES authenticated, and `/api/cli/**` authenticates with
 * a GitHub token instead of a cookie.
 */

import { rateLimit } from '@webjsdev/server';
import { auth } from '#modules/auth/auth.server.ts';

const limited = rateLimit({ window: '1m', max: 120, trustProxy: true, key: 'api:' });

/** Paths that authenticate themselves rather than through the session cookie. */
const PUBLIC_PREFIXES = ['/api/auth/', '/api/cli/'];

export default async function apiMiddleware(req: Request, next: () => Promise<Response>): Promise<Response> {
  return limited(req, async () => {
    const { pathname } = new URL(req.url);
    if (PUBLIC_PREFIXES.some((p) => pathname.startsWith(p))) return next();

    const session = await auth(req);
    if (!session?.user) return new Response('unauthorized', { status: 401 });
    return next();
  });
}
