'use server';
/** The acting org's services. */
import { listServices as fleetList } from '#modules/fleet/client.server.ts';
import { requireOrg, signedOut } from '#modules/auth/session.server.ts';
import type { SignedOut } from '#modules/auth/session.server.ts';
import type { Service } from '@pilots/sdk';

export async function listServices(): Promise<Service[] | SignedOut> {
  const ctx = await requireOrg();
  if (!ctx) return signedOut();
  return fleetList(ctx.org.id);
}
