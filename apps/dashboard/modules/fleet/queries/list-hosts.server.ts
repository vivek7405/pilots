'use server';
/**
 * Every host in the fleet.
 *
 * Liveness and CPU vendor belong to no tenant, so this is not narrowed by org.
 * It is what turns `suspended` into `resumes warm` or `will cold-boot` on a
 * page that shows one machine and has no other reason to read the fleet.
 */
import { fleet } from '#modules/fleet/client.server.ts';
import { requireOrg, signedOut } from '#modules/auth/session.server.ts';
import type { SignedOut } from '#modules/auth/session.server.ts';
import type { Host } from '@pilots/sdk';

export async function fleetHosts(): Promise<Host[] | SignedOut> {
  const ctx = await requireOrg();
  if (!ctx) return signedOut();
  // A fleet that cannot answer costs the page its resume-tier wording, not the
  // page: `resumeTier` reports cold for a machine whose owner it cannot see,
  // which is the safe direction to be wrong in.
  return fleet.hosts.list().catch(() => [] as Host[]);
}
