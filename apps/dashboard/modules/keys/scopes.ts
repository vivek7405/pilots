/**
 * The scope ladder, browser-safe so the keys form and the route agree on it.
 *
 * `machines` is contained in `deploy`, which is contained in `admin`. A key is
 * stored with the scopes it was minted with; the containment is what hostd
 * applies when it authorises a request.
 */

import { canAdministerOrg } from '#modules/orgs/roles.ts';
import type { Role } from '#modules/orgs/roles.ts';

export const SCOPES = ['machines', 'deploy', 'admin'] as const;
export type Scope = (typeof SCOPES)[number];

export function isScope(value: unknown): value is Scope {
  return typeof value === 'string' && (SCOPES as readonly string[]).includes(value);
}

/**
 * Validates a requested scope set.
 *
 * Only an OWNER may mint an `admin` key -- not the team's own admin role, and
 * not a member. An `admin`-scoped key can create and revoke keys for any org
 * on the fleet, so it is power over teams this one has nothing to do with:
 * handing it to anyone but the owner would make team membership meaningless in
 * one step, and handing it to the `admin` role would make that role a rename
 * of owner rather than a narrower one.
 *
 * Every other scope is open to every member. That is deliberate: a token is
 * how the CLI and the SDKs authenticate, and a team whose members cannot mint
 * one has members who cannot work.
 */
export function validateScopes(raw: unknown, role: Role): { scopes: Scope[] } | { error: string } {
  if (!Array.isArray(raw) || raw.length === 0) return { error: 'Choose at least one scope' };
  if (!raw.every(isScope)) return { error: `Scopes must be drawn from ${SCOPES.join(', ')}` };
  const scopes = [...new Set(raw as Scope[])];
  if (!canAdministerOrg(role) && scopes.includes('admin')) {
    return { error: 'Only an org owner can mint an admin key' };
  }
  return { scopes };
}

/** The display prefix: `pilot_` plus the first 8 characters after it. */
export function prefixOf(plaintext: string): string {
  const body = plaintext.startsWith('pilot_') ? plaintext.slice('pilot_'.length) : plaintext;
  return `pilot_${body.slice(0, 8)}`;
}
