'use server';
/**
 * Remove someone from the org.
 *
 * An owner cannot remove themselves. An org with no owner has nobody who can
 * invite, mint an admin key or restore the membership, so the account would be
 * locked out of its own resources with no path back.
 */
import { and, eq } from 'drizzle-orm';
import { db } from '#db/connection.server.ts';
import { memberships } from '#db/schema.server.ts';
import { requireOrg } from '#modules/auth/session.server.ts';

export async function removeMember(formData: FormData) {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };
  if (ctx.role !== 'owner') return { success: false, error: 'Only an owner can remove a member.', status: 403 };

  const userId = Number(formData.get('user'));
  if (!Number.isInteger(userId)) return { success: false, error: 'No such member.', status: 404 };
  if (userId === ctx.user.id) {
    return { success: false, error: 'An owner cannot remove themselves.', status: 422 };
  }

  const row = await db
    .select()
    .from(memberships)
    .where(and(eq(memberships.userId, userId), eq(memberships.orgId, ctx.org.id)))
    .get();
  if (!row) return { success: false, error: 'No such member.', status: 404 };

  await db.delete(memberships).where(eq(memberships.id, row.id));
  return { success: true, redirect: '/org' };
}
