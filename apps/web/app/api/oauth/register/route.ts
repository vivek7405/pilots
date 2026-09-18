/**
 * RFC 7591 dynamic client registration.
 *
 * Open registration, deliberately: an MCP client registers itself the first
 * time a user points it at this fleet, before any human has had a chance to
 * approve anything, and requiring a pre-shared client id would mean every
 * agent needs an operator. A registration grants NOTHING on its own. It
 * creates a name and an allowlist of redirect URIs; a token still needs a
 * signed-in human to approve it on the consent screen, and the key that comes
 * out belongs to the org they chose.
 *
 * That is why the interesting validation is on `redirect_uris`: an entry that
 * is not https, loopback http or a private-use scheme is dropped, and a
 * registration with nothing left is refused. An open redirector in this list
 * would be a way to have a code delivered somewhere the user never saw.
 */

import { jsonBody, readJson } from '#modules/http/guards.server.ts';
import { registerClient } from '#modules/oauth/oauth.server.ts';

const CORS = {
  'access-control-allow-origin': '*',
  'access-control-allow-methods': 'POST, OPTIONS',
  'access-control-allow-headers': 'content-type',
};

function error(code: string, description: string, status = 400): Response {
  return jsonBody({ error: code, error_description: description }, status, CORS);
}

export async function POST(req: Request): Promise<Response> {
  const body = await readJson(req);
  const uris = body.redirect_uris;
  if (!Array.isArray(uris) || uris.length === 0 || !uris.every((u) => typeof u === 'string')) {
    return error('invalid_redirect_uri', 'redirect_uris must be a non-empty array of strings');
  }

  const name = typeof body.client_name === 'string' ? body.client_name : 'An MCP client';
  const uri = typeof body.client_uri === 'string' ? body.client_uri : null;
  const client = await registerClient({ name, redirectUris: uris as string[], uri });
  if (!client) {
    return error(
      'invalid_redirect_uri',
      'every redirect_uri was rejected: use https, http on loopback, or a private-use scheme with a dot in it',
    );
  }

  // 201 with the client id, and NO secret: every client here is public and
  // proves itself with PKCE. `token_endpoint_auth_method: none` is what says
  // so in the client's own language.
  return jsonBody(
    {
      client_id: client.id,
      client_name: client.name,
      redirect_uris: client.redirectUris,
      token_endpoint_auth_method: 'none',
      grant_types: ['authorization_code'],
      response_types: ['code'],
      client_id_issued_at: Math.floor(Date.now() / 1000),
    },
    201,
    CORS,
  );
}

export function OPTIONS(): Response {
  return new Response(null, { status: 204, headers: CORS });
}
