'use server';
/** The acting org's machines, for the SSR snapshot the live list starts from. */
import { listMachines as fleetList } from '#modules/fleet/client.server.ts';
import { requireOrg, signedOut } from '#modules/auth/session.server.ts';
import type { SignedOut } from '#modules/auth/session.server.ts';
import type { Machine } from '@pilots/sdk';

export async function listMachines(): Promise<Machine[] | SignedOut> {
  const ctx = await requireOrg();
  if (!ctx) return signedOut();
  return fleetList(ctx.org.id);
}
