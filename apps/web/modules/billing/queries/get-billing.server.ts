'use server';
/**
 * What the team's billing card reads.
 *
 * The team comes from the SESSION, never from an argument. Every export of a
 * `'use server'` file is an RPC endpoint the browser can post to, so a
 * caller-supplied team id would let any signed-in visitor read any other
 * team's plan.
 *
 * A team with no account row is not an error and not an empty card: it is a
 * team on the free plan, which is the answer, and a personal team is always
 * that by design.
 */
import { eq } from 'drizzle-orm';
import { db } from '#db/connection.server.ts';
import { billingAccounts } from '#db/schema.server.ts';
import { requireOrg, signedOut } from '#modules/auth/session.server.ts';
import type { SignedOut } from '#modules/auth/session.server.ts';
import { planOf } from '#modules/billing/plans.ts';
import type { Plan } from '#modules/billing/plans.ts';
import { paymentProvider } from '#modules/billing/provider.server.ts';
import type { Status } from '#modules/billing/provider.server.ts';

export interface BillingView {
  plan: Plan;
  /** Null while nobody has activated a plan, which is the free plan. */
  provider: string | null;
  status: Status;
  /** When the plan last changed, or null when it never has. */
  since: Date | null;
}

export async function getBilling(): Promise<BillingView | SignedOut> {
  const ctx = await requireOrg();
  if (!ctx) return signedOut();

  const account = ctx.org.billingAccountId
    ? ((await db
        .select()
        .from(billingAccounts)
        .where(eq(billingAccounts.id, ctx.org.billingAccountId))
        .get()) ?? null)
    : null;

  return {
    plan: planOf(account?.plan),
    provider: account?.provider ?? null,
    status: await paymentProvider().status(account),
    since: account?.updatedAt ?? null,
  };
}
