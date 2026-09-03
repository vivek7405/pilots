'use server';
/** The org's volumes. Read-only: volumes come from a deploy, not from here. */
import { listVolumes as fleetList } from '#modules/fleet/client.server.ts';
import type { Volume } from '@pilots/sdk';

export async function listVolumes(orgId: string): Promise<Volume[]> {
  return fleetList(orgId);
}
