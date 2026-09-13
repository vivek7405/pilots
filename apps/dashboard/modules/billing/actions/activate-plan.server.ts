'use server';
/**
 * Put a team on a plan, and push what that plan buys to the fleet.
 *
 * The fleet write is the point of this action, not a side effect of it. A plan
 * stored here and never sent would be a number on a page that nothing
 * enforces, which is the same class of mistake as an expiry nobody reads. So
 * the quota goes first: if the fleet refuses it, no local row changes and the
 * team stays on the plan it was actually being held to.
 *
 * A PERSONAL team is always on the free plan and never gets a billing account.
 * It belongs to one person, it is created before anyone has chosen anything,
 * and an account row for it would be a row that exists only to say "free".
 *
 * No money moves. `paymentProvider()` returns the one implementation there is,
 * which makes no network call; `setup` returning a redirect is the seam a real
 * provider would use and is handled here so that adding one changes nothing
 * outside `provider.server.ts`.
 */
import { eq } from 'drizzle-orm';
import { db } from '#db/connection.server.ts';
import { billingAccounts, orgs } from '#db/schema.server.ts';
import { requireOrg } from '#modules/auth/session.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { canAdministerOrg } from '#modules/orgs/roles.ts';
import { isPlanId, PLANS, quotaWire } from '#modules/billing/plans.ts';
import type { PlanId } from '#modules/billing/plans.ts';
import { paymentProvider } from '#modules/billing/provider.server.ts';

export async function activatePlan(formData: FormData) {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };
  if (!canAdministerOrg(ctx.role)) {
    return { success: false, error: 'Only an owner can change the plan.', status: 403 };
  }

  const wanted = String(formData.get('plan') || '');
  if (!isPlanId(wanted)) return { success: false, fieldErrors: { plan: 'Choose a plan' } };
  const plan = PLANS[wanted as PlanId];

  if (ctx.org.personal) {
    return {
      success: false,
      error: 'A personal team is always on the free plan. Create a team to choose another.',
      status: 422,
    };
  }

  try {
    await fleet.quotas.put(ctx.org.id, quotaWire(ctx.org.id, plan.quota));
  } catch (err) {
    return { success: false, error: `The fleet refused: ${(err as Error).message}`, status: 502 };
  }

  const provider = paymentProvider();
  const now = new Date();
  const existingId = ctx.org.billingAccountId;
  const existing = existingId
    ? await db.select().from(billingAccounts).where(eq(billingAccounts.id, existingId)).get()
    : undefined;

  let account = existing;
  if (account) {
    await db
      .update(billingAccounts)
      .set({ plan: plan.id, status: 'active', provider: provider.id, updatedAt: now })
      .where(eq(billingAccounts.id, account.id));
    account = { ...account, plan: plan.id, status: 'active', provider: provider.id, updatedAt: now };
  } else {
    const [row] = await db
      .insert(billingAccounts)
      .values({ plan: plan.id, status: 'active', provider: provider.id })
      .returning();
    account = row;
    await db.update(orgs).set({ billingAccountId: row.id }).where(eq(orgs.id, ctx.org.id));
  }

  // A provider with somewhere to send the visitor gets to send them. The one
  // implementation there is returns null, so this is the seam and not a path.
  const next = await provider.setup(ctx.org);
  if (next) return { success: true, redirect: next.url };

  return { success: true, redirect: '/org?ok=plan-changed' };
}
