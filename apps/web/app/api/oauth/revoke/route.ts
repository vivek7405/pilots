/**
 * RFC 7009 token revocation: a client handing back a token it no longer wants.
 *
 * The token is a pilots API key, so revoking it is the same tombstone the
 * tokens page writes, and it replicates to every host: within gossip latency
 * the key stops working fleet-wide, with no host having to be reachable for
 * the check.
 *
 * The endpoint answers 200 for a token it cannot find, as the RFC requires.
 * An error there would turn this into an oracle for guessing valid tokens.
 */

import { eq } from 'drizzle-orm';

import { db } from '#db/connection.server.ts';
import { apiKeys } from '#db/schema.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { jsonBody } from '#modules/http/guards.server.ts';

const CORS = {
  'access-control-allow-origin': '*',
  'access-control-allow-methods': 'POST, OPTIONS',
  'access-control-allow-headers': 'content-type, authorization',
};

/** sha256 hex, which is the form hostd keys a revocation by. */
async function sha256Hex(value: string): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(value));
  return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, '0')).join('');
}

export async function POST(req: Request): Promise<Response> {
  const type = req.headers.get('content-type') ?? '';
  const form = type.includes('application/json')
    ? new URLSearchParams(Object.entries((await req.json().catch(() => ({}))) as Record<string, string>))
    : new URLSearchParams(await req.text().catch(() => ''));

  const token = form.get('token');
  // 200 with no body either way: see the file comment.
  if (!token) return new Response(null, { status: 200, headers: CORS });

  const hash = await sha256Hex(token);
  try {
    await fleet.apiKeys.revoke(hash);
  } catch {
    // A key the fleet does not know is already as revoked as it can be.
  }
  await db.update(apiKeys).set({ revokedAt: new Date() }).where(eq(apiKeys.hash, hash));

  return new Response(null, { status: 200, headers: CORS });
}

export function OPTIONS(): Response {
  return new Response(null, { status: 204, headers: CORS });
}

export function GET(): Response {
  return jsonBody({ error: 'invalid_request', error_description: 'POST a token to revoke it' }, 405, CORS);
}
