'use server';

/**
 * The acting org's usage samples over a period, oldest first.
 *
 * The org comes from the session. This is an RPC endpoint, and a caller-chosen
 * org id would hand out any tenant's billing record; the `?org=` form lives in
 * `app/api/usage/route.ts`, behind a membership check.
 */
import { requireOrg, signedOut } from '#modules/auth/session.server.ts';
import type { SignedOut } from '#modules/auth/session.server.ts';
import { samplesForOrg } from '../samples.server.ts';
import type { UsageSample } from '#db/schema.server.ts';

export async function usageForOrg(input: { since: Date; until: Date }): Promise<UsageSample[] | SignedOut> {
  const ctx = await requireOrg();
  if (!ctx) return signedOut();
  return samplesForOrg(ctx.org.id, input.since, input.until);
}
