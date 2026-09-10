/**
 * The RFC 8414 authorization-server metadata.
 *
 * It is served here rather than at `/.well-known/oauth-authorization-server`
 * because the router does not route a dot-directory; `package.json`'s
 * `webjs.redirects` points both well-known paths at this route with a 307,
 * which every client follows on a GET. The document names this origin as the
 * issuer either way, so what a client validates is unchanged.
 *
 * Unauthenticated and CORS-open on purpose: a client reads it BEFORE it has
 * anything to present, and a browser-based one reads it cross-origin.
 */

import { jsonBody } from '#modules/http/guards.server.ts';
import { authorizationServerMetadata } from '#modules/oauth/oauth.server.ts';
import { publicOrigin } from '#modules/oauth/origin.server.ts';

const CORS = {
  'access-control-allow-origin': '*',
  'access-control-allow-methods': 'GET, OPTIONS',
  'access-control-allow-headers': 'content-type, mcp-protocol-version',
  'cache-control': 'public, max-age=300',
};

export function GET(req: Request): Response {
  return jsonBody(authorizationServerMetadata(publicOrigin(req)), 200, CORS);
}

export function OPTIONS(): Response {
  return new Response(null, { status: 204, headers: CORS });
}
