/**
 * The authorize request: parsing it, refusing it, and the one-time ticket that
 * carries an approval from the consent screen to the approve endpoint.
 *
 * Two rules decide the error handling, and they are the ones that stop this
 * from becoming an open redirector:
 *
 *   - A request whose `client_id` or `redirect_uri` is wrong is shown to the
 *     USER as a page. It is NEVER redirected anywhere, because the only place
 *     to send it would be the unvalidated URI in the request.
 *   - Once both are known good, every later refusal goes back to the client as
 *     `?error=` on that verified URI, which is what the spec requires and what
 *     lets an agent report something better than a hang.
 */

import { createHmac, timingSafeEqual } from 'node:crypto';

import type { Scope } from '#modules/keys/scopes.ts';
import { isScope } from '#modules/keys/scopes.ts';
import { DEFAULT_PREFIX, findClient, isAllowedRedirectUri } from '#modules/oauth/oauth.server.ts';
import type { RegisteredClient } from '#modules/oauth/oauth.server.ts';

export interface AuthorizeRequest {
  client: RegisteredClient;
  redirectUri: string;
  state: string | null;
  scopes: Scope[];
  codeChallenge: string;
  resource: string | null;
}

/** A refusal the USER sees, because there is nowhere safe to send it. */
export interface UserFacingError {
  kind: 'user';
  title: string;
  detail: string;
}

/** A refusal the CLIENT sees, on its own verified redirect URI. */
export interface ClientFacingError {
  kind: 'client';
  redirectUri: string;
  code: string;
  description: string;
  state: string | null;
}

export type AuthorizeFailure = UserFacingError | ClientFacingError;

export async function parseAuthorize(params: URLSearchParams): Promise<AuthorizeRequest | AuthorizeFailure> {
  const clientId = params.get('client_id') ?? '';
  const client = clientId ? await findClient(clientId) : null;
  if (!client) {
    return {
      kind: 'user',
      title: 'Unknown application',
      detail: 'No application is registered with that client_id. Ask it to register again, then retry.',
    };
  }

  const redirectUri = params.get('redirect_uri') ?? '';
  // Exact match against what the client registered, with no prefix rule: a
  // partial match is how a code ends up at a path its owner never chose.
  if (!redirectUri || !client.redirectUris.includes(redirectUri) || !isAllowedRedirectUri(redirectUri)) {
    return {
      kind: 'user',
      title: 'That redirect is not registered',
      detail: `${client.name} asked to be sent to an address it did not register. Nothing was approved.`,
    };
  }

  const state = params.get('state');
  const fail = (code: string, description: string): ClientFacingError => ({
    kind: 'client',
    redirectUri,
    code,
    description,
    state,
  });

  if ((params.get('response_type') ?? '') !== 'code') {
    return fail('unsupported_response_type', 'only the authorization code flow is supported');
  }
  const method = params.get('code_challenge_method') ?? '';
  const challenge = params.get('code_challenge') ?? '';
  if (!challenge) return fail('invalid_request', 'code_challenge is required: every client here is public');
  if (method !== 'S256') return fail('invalid_request', 'code_challenge_method must be S256; plain is not accepted');

  const asked = (params.get('scope') ?? '').split(/[\s+]+/).filter(Boolean);
  const scopes = asked.filter(isScope);
  if (asked.length > 0 && scopes.length !== asked.length) {
    return fail('invalid_scope', 'scopes must be drawn from machines, deploy, admin');
  }

  return {
    client,
    redirectUri,
    state,
    // Nothing asked for means the least a client can do anything with.
    scopes: scopes.length > 0 ? [...new Set(scopes)] : (['machines'] as Scope[]),
    codeChallenge: challenge,
    resource: params.get('resource'),
  };
}

/** The client-facing refusal, as the URL to send the browser to. */
export function errorRedirect(err: ClientFacingError): string {
  const url = new URL(err.redirectUri);
  url.searchParams.set('error', err.code);
  url.searchParams.set('error_description', err.description);
  if (err.state) url.searchParams.set('state', err.state);
  return url.toString();
}

/**
 * The consent ticket.
 *
 * The consent form posts to a `route.ts`, which the framework's action CSRF
 * check does not cover, so the form carries this instead: an HMAC over the
 * exact request that was SHOWN, bound to the signed-in user and stamped with a
 * time. The approve endpoint re-derives it, so a form a third-party page
 * forged has no valid ticket, and a ticket for one user cannot be replayed by
 * another. It expires with the screen it was rendered on.
 *
 * The secret is `AUTH_SECRET`, which the app already requires and already uses
 * to sign the session cookie.
 */
export const TICKET_TTL_MS = 10 * 60_000;

interface TicketPayload {
  userId: number;
  clientId: string;
  redirectUri: string;
  scopes: Scope[];
  codeChallenge: string;
  state: string | null;
  resource: string | null;
  issuedAt: number;
}

function secret(): string {
  const value = process.env.AUTH_SECRET?.trim();
  if (!value) throw new Error('AUTH_SECRET is required to sign a consent ticket');
  return value;
}

function payloadOf(req: AuthorizeRequest, userId: number, issuedAt: number): TicketPayload {
  return {
    userId,
    clientId: req.client.id,
    redirectUri: req.redirectUri,
    scopes: req.scopes,
    codeChallenge: req.codeChallenge,
    state: req.state,
    resource: req.resource,
    issuedAt,
  };
}

export function signTicket(req: AuthorizeRequest, userId: number, now = Date.now()): string {
  const body = Buffer.from(JSON.stringify(payloadOf(req, userId, now))).toString('base64url');
  const mac = createHmac('sha256', secret()).update(body).digest('base64url');
  return `${body}.${mac}`;
}

/** The ticket's payload, or null when it is forged, altered or stale. */
export function verifyTicket(ticket: string, userId: number, now = Date.now()): TicketPayload | null {
  const [body, mac] = ticket.split('.');
  if (!body || !mac) return null;
  const expected = createHmac('sha256', secret()).update(body).digest('base64url');
  const a = Buffer.from(mac);
  const b = Buffer.from(expected);
  if (a.length !== b.length || !timingSafeEqual(a, b)) return null;

  let payload: TicketPayload;
  try {
    payload = JSON.parse(Buffer.from(body, 'base64url').toString()) as TicketPayload;
  } catch {
    return null;
  }
  // Bound to the human who was shown the screen, not merely to a signature.
  if (payload.userId !== userId) return null;
  if (!Number.isFinite(payload.issuedAt) || now - payload.issuedAt > TICKET_TTL_MS) return null;
  return payload;
}

/**
 * The restriction a consent screen offers by default.
 *
 * A prefix is the difference between "this agent can operate my fleet" and
 * "this agent can operate the machines it made", and an agent that names its
 * own machines loses nothing by having one.
 */
export function defaultPrefixFor(scopes: Scope[]): string {
  return scopes.includes('admin') ? '' : DEFAULT_PREFIX;
}
