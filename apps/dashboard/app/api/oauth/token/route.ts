/**
 * The token endpoint: an authorization code in, a pilots API key out.
 *
 * The key IS the access token. hostd verifies it from its own local replica
 * exactly as it verifies the key `pilot login` writes to disk, so a token
 * issued here keeps working with this dashboard down, and nothing on the data
 * plane learns a second credential format.
 *
 * `expires_in` is absent unless the consent screen chose a lifetime, and there
 * is never a refresh token: a key that does not expire has nothing to refresh,
 * and a caller who wants it gone revokes it on the tokens page, which
 * replicates to every host.
 */

import { db } from '#db/connection.server.ts';
import { apiKeys } from '#db/schema.server.ts';
import { jsonBody } from '#modules/http/guards.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { prefixOf } from '#modules/keys/scopes.ts';
import { consumeCode, findClient } from '#modules/oauth/oauth.server.ts';

const CORS = {
  'access-control-allow-origin': '*',
  'access-control-allow-methods': 'POST, OPTIONS',
  'access-control-allow-headers': 'content-type, authorization',
};

function error(code: string, description: string, status = 400): Response {
  return jsonBody({ error: code, error_description: description }, status, CORS);
}

/** The token endpoint takes form encoding, as RFC 6749 requires. */
async function readForm(req: Request): Promise<URLSearchParams> {
  const type = req.headers.get('content-type') ?? '';
  if (type.includes('application/json')) {
    const body = (await req.json().catch(() => ({}))) as Record<string, unknown>;
    const params = new URLSearchParams();
    for (const [k, v] of Object.entries(body)) if (typeof v === 'string') params.set(k, v);
    return params;
  }
  return new URLSearchParams(await req.text().catch(() => ''));
}

export async function POST(req: Request): Promise<Response> {
  const form = await readForm(req);

  if (form.get('grant_type') !== 'authorization_code') {
    // No refresh_token grant, and no client_credentials: see the file comment.
    return error('unsupported_grant_type', 'only authorization_code is supported');
  }
  const code = form.get('code');
  const clientId = form.get('client_id');
  const redirectUri = form.get('redirect_uri');
  const verifier = form.get('code_verifier');
  if (!code || !clientId || !redirectUri || !verifier) {
    return error('invalid_request', 'code, client_id, redirect_uri and code_verifier are all required');
  }
  if (!(await findClient(clientId))) return error('invalid_client', 'unknown client_id', 401);

  const consumed = await consumeCode({ code, clientId, redirectUri, codeVerifier: verifier });
  if (!consumed.ok) {
    // One code, one answer. A replay says the code leaked, and single use is
    // what makes that worth nothing.
    const detail: Record<string, string> = {
      invalid_grant: 'no such code',
      expired: 'the code expired; start the flow again',
      already_used: 'the code was already exchanged',
      client_mismatch: 'the code was issued to another client',
      redirect_mismatch: 'redirect_uri does not match the one the code was issued for',
      pkce_mismatch: 'code_verifier does not match the challenge',
    };
    return error('invalid_grant', detail[consumed.reason] ?? 'the code cannot be exchanged');
  }
  const grant = consumed.grant;

  let created;
  try {
    created = await fleet.apiKeys.create({
      org_id: grant.orgId,
      scopes: grant.scopes,
      ...(grant.namePrefix ? { name_prefix: grant.namePrefix } : {}),
      ...(grant.maxMachines ? { max_machines: grant.maxMachines } : {}),
      ...(grant.keyExpiresAt ? { expires_at: Math.floor(grant.keyExpiresAt.getTime() / 1000) } : {}),
    });
  } catch (err) {
    return error('server_error', `the fleet refused: ${(err as Error).message}`, 502);
  }
  if (!created.key) return error('server_error', 'the fleet returned no key', 502);

  const client = await findClient(clientId);
  await db.insert(apiKeys).values({
    orgId: grant.orgId,
    name: `oauth ${client?.name ?? clientId}`.slice(0, 100),
    prefix: prefixOf(created.key),
    hash: created.hash,
    scopes: grant.scopes,
    createdBy: grant.userId,
    clientId,
  });

  const lifetime = grant.keyExpiresAt ? Math.floor((grant.keyExpiresAt.getTime() - Date.now()) / 1000) : null;
  return jsonBody(
    {
      access_token: created.key,
      token_type: 'Bearer',
      scope: grant.scopes.join(' '),
      ...(lifetime && lifetime > 0 ? { expires_in: lifetime } : {}),
    },
    200,
    CORS,
  );
}

export function OPTIONS(): Response {
  return new Response(null, { status: 204, headers: CORS });
}
