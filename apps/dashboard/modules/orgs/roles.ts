/**
 * What each role on a team may do.
 *
 * Browser-safe, so the page that hides a control and the action that refuses
 * it agree on one rule. A hidden button is a courtesy; the action is the
 * control, and both read these predicates.
 *
 * Three roles, not two:
 *
 *   - `owner` is the account the team belongs to. Only an owner may rename it,
 *     hand it to someone else, delete it, change the plan, or mint a token
 *     that can administer every team on the fleet.
 *   - `admin` runs the team day to day: add and remove people, mint tokens
 *     that deploy. It deliberately stops short of the five things above,
 *     because each of them either ends the team or grants power over teams
 *     this one has nothing to do with.
 *   - `member` uses what the team owns and changes nothing about the team.
 *
 * A row carrying a role this file does not know reads as `member`. An
 * unrecognised value must never widen a permission, and the membership table
 * is plain text with no check constraint behind it.
 */

export const ROLES = ['owner', 'admin', 'member'] as const;
export type Role = (typeof ROLES)[number];

export function isRole(value: unknown): value is Role {
  return typeof value === 'string' && (ROLES as readonly string[]).includes(value);
}

/** A stored role, narrowed. Anything unrecognised is the least of them. */
export function normalizeRole(value: unknown): Role {
  return isRole(value) ? value : 'member';
}

/** Add and remove people. */
export function canManageMembers(role: Role): boolean {
  return role === 'owner' || role === 'admin';
}

/**
 * Rename, hand over, delete, change the plan, mint an `admin`-scoped token.
 * The owner alone.
 *
 * There is deliberately no `canMintKeys`: every member of a team may mint a
 * token for it, which is what makes the CLI usable by the people doing the
 * work. The restriction that matters is the `admin` SCOPE, which is power over
 * every team on the fleet rather than over this one, and `validateScopes`
 * gates it on this predicate.
 */
export function canAdministerOrg(role: Role): boolean {
  return role === 'owner';
}

/** How a role is written on screen. */
export function roleLabel(role: Role): string {
  return role === 'owner' ? 'Owner' : role === 'admin' ? 'Admin' : 'Member';
}
