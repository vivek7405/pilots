/**
 * `/api/cli/**`: a tighter limit and no session.
 *
 * `pilot login` calls the exchange route with a GitHub access token, so there
 * is no cookie to gate on. Ten a minute per IP is generous for a human running
 * `pilot login` and mean for anything spraying stolen tokens at the check
 * endpoint.
 *
 * Per-segment middleware runs outermost first, so `app/api/middleware.ts` has
 * already applied the 120/min API limit before this one is reached.
 */

import { rateLimit } from '@webjsdev/server';

const limited = rateLimit({ window: '1m', max: 10, trustProxy: true, key: 'cli:' });

export default async function cliMiddleware(req: Request, next: () => Promise<Response>): Promise<Response> {
  return limited(req, next);
}
