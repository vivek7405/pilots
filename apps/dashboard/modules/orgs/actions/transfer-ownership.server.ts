'use server';
/**
 * Hand a team to one of its members.
 *
 * One transaction, both halves: the new owner is promoted and the old one is
 * demoted to `admin` in the same write. Doing it in two would leave a window
 * where the team has two owners or none, and a crash between them would leave
 * it that way for good.
 *
 * The old owner becomes an `admin` rather than a `member`: they were running
 * the team a moment ago, and dropping them to the role that can do nothing
 * about membership is a demotion the act does not imply. Leaving afterwards is
 * one click away and is theirs to choose.
 *
 * A PERSONAL team is never transferable. It is found by its `owner_id` on
 * every sign-in, so handing it over would make its original owner's next
 * sign-in create them a second personal team and leave their first one
 * unreachable from their own account.
 */
import { and, eq } from 'drizzle-orm';
import { db } from '#db/connection.server.ts';
import { memberships, orgs } from '#db/schema.server.ts';
import { requireOrg } from '#modules/auth/session.server.ts';
import { canAdministerOrg } from '#modules/orgs/roles.ts';

export async function transferOwnership(formData: FormData) {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };
  if (!canAdministerOrg(ctx.role)) {
    return { success: false, error: 'Only an owner can hand the team over.', status: 403 };
  }
  if (ctx.org.personal) {
    return { success: false, error: 'A personal team cannot be handed over.', status: 422 };
  }

  const userId = Number(formData.get('user'));
  if (!Number.isInteger(userId)) return { success: false, error: 'No such member.', status: 404 };
  if (userId === ctx.user.id) {
    return { success: false, error: 'You already own this team.', status: 422 };
  }

  const target = await db
    .select()
    .from(memberships)
    .where(and(eq(memberships.userId, userId), eq(memberships.orgId, ctx.org.id)))
    .get();
  if (!target) return { success: false, error: 'No such member.', status: 404 };

  const orgId = ctx.org.id;
  const from = ctx.user.id;
  await db.transaction((tx) => {
    tx.update(memberships).set({ role: 'owner' }).where(eq(memberships.id, target.id)).run();
    tx.update(memberships)
      .set({ role: 'admin' })
      .where(and(eq(memberships.userId, from), eq(memberships.orgId, orgId)))
      .run();
    // `orgs.owner_id` is what the personal-team lookup reads and what the team
    // list orders by; leaving it pointing at the previous owner would make the
    // two halves of "who owns this" disagree.
    tx.update(orgs).set({ ownerId: userId }).where(eq(orgs.id, orgId)).run();
  });

  return { success: true, redirect: '/org?ok=transferred' };
}
