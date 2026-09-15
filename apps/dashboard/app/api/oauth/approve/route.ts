/**
 * The consent screen's target: a signed ticket in, an authorization code out.
 *
 * This is the one endpoint in the flow that a signed-in browser can be made to
 * POST to from somewhere else, so it is the one that needs its own CSRF
 * defence. A `route.ts` is not covered by the framework's action CSRF check,
 * so the form carries a ticket instead: an HMAC over the exact request that
 * was SHOWN, bound to the signed-in user id and stamped with a time. A forged
 * form has no valid ticket; a ticket lifted from one user's page is refused
 * for another; an old one expires.
 *
 * The parameters that decide what the token can do -- client, redirect URI,
 * scopes, PKCE challenge -- come from the TICKET and never from the form, so a
 * tampered field changes nothing. Only the three the human actually chose (the
 * org, the prefix, the cap and the lifetime) are read from the form, and the
 * org is checked against their memberships before it is used.
 */

import { requireUser, roleOn } from '#modules/auth/session.server.ts';
import { canAdministerOrg } from '#modules/orgs/roles.ts';
import { issueCode } from '#modules/oauth/oauth.server.ts';
import { verifyTicket } from '#modules/oauth/authorize.server.ts';

/** A redirect the browser follows, whatever method it arrived with. */
function seeOther(location: string): Response {
  return new Response(null, { status: 303, headers: { location, 'cache-control': 'no-store' } });
}

function refuse(status: number, message: string): Response {
  return new Response(message, { status, headers: { 'content-type': 'text/plain; charset=utf-8' } });
}

export async function POST(req: Request): Promise<Response> {
  const user = await requireUser(req);
  if (!user) return refuse(401, 'sign in and try again');

  const form = await req.formData().catch(() => null);
  if (!form) return refuse(400, 'expected a form submission');

  const ticket = String(form.get('ticket') ?? '');
  const grant = ticket ? verifyTicket(ticket, user.id) : null;
  if (!grant) {
    // Deliberately vague and deliberately terminal: a bad ticket is either a
    // forgery or a stale screen, and neither should be repaired for the
    // caller. Nothing is redirected anywhere.
    return refuse(400, 'that consent form is no longer valid; start the authorization again');
  }

  const target = new URL(grant.redirectUri);
  if (grant.state) target.searchParams.set('state', grant.state);

  if (String(form.get('decision') ?? '') !== 'approve') {
    target.searchParams.set('error', 'access_denied');
    target.searchParams.set('error_description', 'the user declined');
    return seeOther(target.toString());
  }

  // The org is the one field that grants access rather than restricting it,
  // so it is the one field checked against the database rather than trusted.
  const orgId = String(form.get('org') ?? '');
  const role = orgId ? await roleOn(user.id, orgId) : null;
  if (!role) return refuse(403, 'you are not a member of that team');
  // Minting an admin key is an owner's act, the same rule the tokens form
  // applies, read from the same predicate so the two cannot drift apart.
  if (grant.scopes.includes('admin') && !canAdministerOrg(role)) {
    target.searchParams.set('error', 'access_denied');
    target.searchParams.set('error_description', 'only an org owner can grant an admin token');
    return seeOther(target.toString());
  }

  const prefix = String(form.get('prefix') ?? '').trim().slice(0, 40) || null;
  const maxRaw = Number(form.get('max_machines'));
  const maxMachines = Number.isFinite(maxRaw) && maxRaw > 0 ? Math.min(Math.floor(maxRaw), 1000) : null;
  const hours = Number(form.get('lifetime'));
  const keyExpiresAt = Number.isFinite(hours) && hours > 0 ? new Date(Date.now() + hours * 3_600_000) : null;

  const code = await issueCode({
    clientId: grant.clientId,
    userId: user.id,
    orgId,
    scopes: grant.scopes,
    redirectUri: grant.redirectUri,
    codeChallenge: grant.codeChallenge,
    resource: grant.resource,
    namePrefix: prefix,
    maxMachines,
    keyExpiresAt,
  });

  target.searchParams.set('code', code);
  return seeOther(target.toString());
}
