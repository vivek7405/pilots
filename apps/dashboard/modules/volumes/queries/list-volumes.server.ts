'use server';
/** The acting org's volumes. Read-only: volumes come from a deploy, not from here. */
import { listVolumes as fleetList } from '#modules/fleet/client.server.ts';
import { requireOrg, signedOut } from '#modules/auth/session.server.ts';
import type { SignedOut } from '#modules/auth/session.server.ts';
import type { Volume } from '@pilots/sdk';

export async function listVolumes(): Promise<Volume[] | SignedOut> {
  const ctx = await requireOrg();
  if (!ctx) return signedOut();
  return fleetList(ctx.org.id);
}
