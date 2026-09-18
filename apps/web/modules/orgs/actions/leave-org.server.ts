'use server';
/**
 * Leave a team.
 *
 * Any member may, at any role, with two refusals:
 *
 *   - a PERSONAL team cannot be left. It is the account's own home, created on
 *     first sign-in and found again by its owner; an account with no team can
 *     see nothing and has no path back.
 *   - the LAST owner cannot leave. A team with no owner has nobody who can
 *     rename it, hand it over, delete it or change its plan, so it would hold
 *     its resources with nobody able to release them. Hand it over first.
 *
 * The acting-team cookie is deliberately not touched. It is never trusted on
 * its own: `currentOrg` re-reads the membership table on every request, so the
 * moment the row is gone the cookie selects nothing and the visitor falls back
 * to their personal team. Rewriting it here would be a second mechanism for
 * the same guarantee.
 */
import { and, eq } from 'drizzle-orm';
import { db } from '#db/connection.server.ts';
import { memberships } from '#db/schema.server.ts';
import { requireOrg } from '#modules/auth/session.server.ts';
import { normalizeRole } from '#modules/orgs/roles.ts';

export async function leaveOrg() {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };
  if (ctx.org.personal) {
    return { success: false, error: 'A personal team cannot be left.', status: 422 };
  }

  const rows = await db.select().from(memberships).where(eq(memberships.orgId, ctx.org.id)).all();
  const mine = rows.find((r) => r.userId === ctx.user.id);
  if (!mine) return { success: false, error: 'You are not a member of that team.', status: 403 };

  const owners = rows.filter((r) => normalizeRole(r.role) === 'owner');
  if (normalizeRole(mine.role) === 'owner' && owners.length === 1) {
    return {
      success: false,
      error: 'Hand the team to someone else first. A team with no owner cannot be changed or removed.',
      status: 422,
    };
  }

  await db
    .delete(memberships)
    .where(and(eq(memberships.userId, ctx.user.id), eq(memberships.orgId, ctx.org.id)));

  return { success: true, redirect: '/org?ok=left' };
}
