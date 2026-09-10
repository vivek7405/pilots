/**
 * The OAuth 2.1 authorization server, for MCP clients.
 *
 * The shape is the one the MCP specification expects and the one every modern
 * client already implements: protected-resource metadata on the API host
 * (hostd serves that), authorization-server metadata here, dynamic client
 * registration, an authorize endpoint gated by the ordinary dashboard session,
 * and a token endpoint that exchanges a PKCE-bound code.
 *
 * ONE decision shapes everything else: **the access token IS a pilots API
 * key**. There is no second credential type, no JWT to verify, no
 * introspection endpoint, and nothing on the data plane that has to learn a
 * new format. hostd authenticates the token exactly as it authenticates the
 * key `pilot login` writes to disk, from its own local replica, so a token
 * keeps working with this dashboard down -- which is the whole point of rule 1.
 *
 * The consequences, stated plainly:
 *
 *   - **No refresh tokens.** A key does not expire unless the consent screen
 *     gave it a lifetime, so there is nothing to refresh. A client that wants
 *     a new one runs the flow again; a user who wants it gone revokes it on
 *     the tokens page, and the revocation replicates to every host.
 *   - **PKCE is required, S256 only.** Every client here is public: a desktop
 *     agent or a CLI cannot hold a secret. `plain` is refused rather than
 *     downgraded to.
 *   - **Codes are single-use, short-lived and stored as hashes.** A database
 *     that leaks hands nobody a usable credential.
 */

import { and, eq, isNull, lt } from 'drizzle-orm';

import { db } from '#db/connection.server.ts';
import { oauthClients, oauthCodes } from '#db/schema.server.ts';
import { SCOPES } from '#modules/keys/scopes.ts';
import type { Scope } from '#modules/keys/scopes.ts';

/** How long an authorization code is good for. RFC 6749 says "short"; this is short. */
export const CODE_TTL_MS = 60_000;

/** The scopes a client may ask for, in OAuth spelling. */
export const OAUTH_SCOPES = SCOPES;

/** The prefix a restricted token's machines must carry, when one is chosen. */
export const DEFAULT_PREFIX = 'mcp-';

export interface RegisteredClient {
  id: string;
  name: string;
  redirectUris: string[];
  uri: string | null;
}

export interface CodeRestrictions {
  namePrefix?: string | null;
  maxMachines?: number | null;
  keyExpiresAt?: Date | null;
}

export interface IssuedCode extends CodeRestrictions {
  clientId: string;
  userId: number;
  orgId: string;
  scopes: Scope[];
  redirectUri: string;
  codeChallenge: string;
  resource?: string | null;
}

/** URL-safe random, from the platform CSPRNG. */
export function randomToken(bytes = 32): string {
  const raw = crypto.getRandomValues(new Uint8Array(bytes));
  return btoa(String.fromCharCode(...raw)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

/** Base64url of the sha256 of `value`: what a PKCE challenge is, and how a code is stored. */
export async function sha256Base64Url(value: string): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(value));
  return btoa(String.fromCharCode(...new Uint8Array(digest)))
    .replace(/\+/g, '-')
    .replace(/\//g, '_')
    .replace(/=+$/, '');
}

/**
 * A redirect URI this server will send a code to.
 *
 * The rules are OAuth 2.1's, and they are the ones that stop an open
 * redirector from becoming a token thief: an exact string match against what
 * the client registered, and only `https`, `http` on loopback (which is how a
 * CLI receives a code), or a private-use scheme (how a desktop app does).
 */
export function isAllowedRedirectUri(raw: string): boolean {
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    return false;
  }
  if (url.hash) return false;
  if (url.protocol === 'https:') return true;
  if (url.protocol === 'http:') return url.hostname === '127.0.0.1' || url.hostname === '[::1]' || url.hostname === 'localhost';
  // A private-use scheme, e.g. `com.example.app:/callback`. It must have a
  // dot, which is what distinguishes it from a scheme anyone could claim.
  return /^[a-z][a-z0-9+.-]*:$/.test(url.protocol) && url.protocol.includes('.');
}

/** Registers a client. Returns null when nothing it sent is usable. */
export async function registerClient(input: {
  name: string;
  redirectUris: string[];
  uri?: string | null;
}): Promise<RegisteredClient | null> {
  const redirectUris = [...new Set(input.redirectUris.filter(isAllowedRedirectUri))].slice(0, 10);
  if (redirectUris.length === 0) return null;
  const name = input.name.trim().slice(0, 120) || 'An MCP client';
  const [row] = await db
    .insert(oauthClients)
    .values({ name, redirectUris, uri: input.uri?.slice(0, 300) ?? null, dynamic: true })
    .returning();
  if (!row) return null;
  return { id: row.id, name: row.name, redirectUris: row.redirectUris, uri: row.uri };
}

export async function findClient(clientId: string): Promise<RegisteredClient | null> {
  const row = await db.query.oauthClients.findFirst({ where: { id: clientId } });
  return row ? { id: row.id, name: row.name, redirectUris: row.redirectUris, uri: row.uri } : null;
}

/**
 * Issues a code for an approved authorization and returns the plaintext,
 * which is the only time it exists outside the client's own memory.
 */
export async function issueCode(grant: IssuedCode): Promise<string> {
  const code = randomToken();
  await db.insert(oauthCodes).values({
    codeHash: await sha256Base64Url(code),
    clientId: grant.clientId,
    userId: grant.userId,
    orgId: grant.orgId,
    scopes: grant.scopes,
    redirectUri: grant.redirectUri,
    codeChallenge: grant.codeChallenge,
    resource: grant.resource ?? null,
    namePrefix: grant.namePrefix ?? null,
    maxMachines: grant.maxMachines ?? null,
    keyExpiresAt: grant.keyExpiresAt ?? null,
    expiresAt: new Date(Date.now() + CODE_TTL_MS),
  });
  return code;
}

export type ConsumeFailure =
  | 'invalid_grant'
  | 'expired'
  | 'already_used'
  | 'client_mismatch'
  | 'redirect_mismatch'
  | 'pkce_mismatch';

export interface ConsumedCode {
  clientId: string;
  userId: number;
  orgId: string;
  scopes: Scope[];
  resource: string | null;
  namePrefix: string | null;
  maxMachines: number | null;
  keyExpiresAt: Date | null;
}

/**
 * Exchanges a code, exactly once.
 *
 * The row is marked used BEFORE anything is minted, and the update is
 * conditional on it still being unused, so two token requests racing on one
 * code produce one key and one `already_used`. A replayed code is not merely
 * refused: it is the signal that the code leaked, and the single-use rule is
 * what limits the damage to nothing.
 */
export async function consumeCode(input: {
  code: string;
  clientId: string;
  redirectUri: string;
  codeVerifier: string;
}): Promise<{ ok: true; grant: ConsumedCode } | { ok: false; reason: ConsumeFailure }> {
  const codeHash = await sha256Base64Url(input.code);
  const row = await db.query.oauthCodes.findFirst({ where: { codeHash } });
  if (!row) return { ok: false, reason: 'invalid_grant' };
  if (row.usedAt) return { ok: false, reason: 'already_used' };
  if (row.expiresAt.getTime() <= Date.now()) return { ok: false, reason: 'expired' };
  if (row.clientId !== input.clientId) return { ok: false, reason: 'client_mismatch' };
  if (row.redirectUri !== input.redirectUri) return { ok: false, reason: 'redirect_mismatch' };
  if ((await sha256Base64Url(input.codeVerifier)) !== row.codeChallenge) {
    return { ok: false, reason: 'pkce_mismatch' };
  }

  const claimed = await db
    .update(oauthCodes)
    .set({ usedAt: new Date() })
    .where(and(eq(oauthCodes.id, row.id), isNull(oauthCodes.usedAt)))
    .returning();
  if (claimed.length === 0) return { ok: false, reason: 'already_used' };

  return {
    ok: true,
    grant: {
      clientId: row.clientId,
      userId: row.userId,
      orgId: row.orgId,
      scopes: row.scopes as Scope[],
      resource: row.resource,
      namePrefix: row.namePrefix,
      maxMachines: row.maxMachines,
      keyExpiresAt: row.keyExpiresAt,
    },
  };
}

/** Drops codes that expired more than an hour ago. Cheap, and keeps the table small. */
export async function pruneCodes(): Promise<void> {
  await db.delete(oauthCodes).where(lt(oauthCodes.expiresAt, new Date(Date.now() - 3_600_000)));
}

/**
 * The RFC 8414 metadata document.
 *
 * `issuer` is this origin, so a client that derives the metadata URL from the
 * issuer lands back here. Everything else is a URL on this same origin, which
 * is why a self-hosted fleet needs no configuration for any of it.
 */
export function authorizationServerMetadata(origin: string): Record<string, unknown> {
  return {
    issuer: origin,
    authorization_endpoint: `${origin}/oauth/authorize`,
    token_endpoint: `${origin}/api/oauth/token`,
    registration_endpoint: `${origin}/api/oauth/register`,
    revocation_endpoint: `${origin}/api/oauth/revoke`,
    scopes_supported: [...OAUTH_SCOPES],
    response_types_supported: ['code'],
    grant_types_supported: ['authorization_code'],
    // Every client here is public and proves itself with PKCE.
    token_endpoint_auth_methods_supported: ['none'],
    // S256 only: `plain` is not a proof of anything.
    code_challenge_methods_supported: ['S256'],
    service_documentation: 'https://pilots.run/agents',
  };
}
