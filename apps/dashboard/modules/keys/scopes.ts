/**
 * The scope ladder, browser-safe so the keys form and the route agree on it.
 *
 * `machines` is contained in `deploy`, which is contained in `admin`. A key is
 * stored with the scopes it was minted with; the containment is what hostd
 * applies when it authorises a request.
 */

export const SCOPES = ['machines', 'deploy', 'admin'] as const;
export type Scope = (typeof SCOPES)[number];

export function isScope(value: unknown): value is Scope {
  return typeof value === 'string' && (SCOPES as readonly string[]).includes(value);
}

/**
 * Validates a requested scope set.
 *
 * A `member` may not mint an `admin` key. An admin key can create and revoke
 * keys for any org on the fleet, so handing one to a non-owner would make org
 * membership meaningless in one step.
 */
export function validateScopes(raw: unknown, role: 'owner' | 'member'): { scopes: Scope[] } | { error: string } {
  if (!Array.isArray(raw) || raw.length === 0) return { error: 'Choose at least one scope' };
  if (!raw.every(isScope)) return { error: `Scopes must be drawn from ${SCOPES.join(', ')}` };
  const scopes = [...new Set(raw as Scope[])];
  if (role !== 'owner' && scopes.includes('admin')) {
    return { error: 'Only an org owner can mint an admin key' };
  }
  return { scopes };
}

/** The display prefix: `pilot_` plus the first 8 characters after it. */
export function prefixOf(plaintext: string): string {
  const body = plaintext.startsWith('pilot_') ? plaintext.slice('pilot_'.length) : plaintext;
  return `pilot_${body.slice(0, 8)}`;
}
