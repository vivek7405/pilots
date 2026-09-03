'use server';
/** The org's machines, for the SSR snapshot the live list starts from. */
import { listMachines as fleetList } from '#modules/fleet/client.server.ts';
import type { Machine } from '@pilots/sdk';

export async function listMachines(orgId: string): Promise<Machine[]> {
  return fleetList(orgId);
}
