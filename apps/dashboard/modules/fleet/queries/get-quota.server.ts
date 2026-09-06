'use server';
/**
 * The acting org's ceilings.
 *
 * The org comes from the SESSION, never from an argument. This app holds one
 * admin key for the whole fleet, so a caller-supplied org id would let any
 * signed-in visitor read any org's quota.
 */
import { fleet } from '#modules/fleet/client.server.ts';
import { requireOrg, signedOut } from '#modules/auth/session.server.ts';
import type { SignedOut } from '#modules/auth/session.server.ts';
import type { Quota } from '#modules/fleet/types.ts';

export async function getQuota(): Promise<Quota | SignedOut> {
  const ctx = await requireOrg();
  if (!ctx) return signedOut();
  // A fleet that serves no quota route is not a reason to lose the page: the
  // bars simply do not render.
  return fleet.quotas.get(ctx.org.id).catch(() => ({}) as Quota);
}
