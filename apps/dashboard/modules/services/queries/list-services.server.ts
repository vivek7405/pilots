'use server';
/** The org's services. */
import { listServices as fleetList } from '#modules/fleet/client.server.ts';
import type { Service } from '@pilots/sdk';

export async function listServices(orgId: string): Promise<Service[]> {
  return fleetList(orgId);
}
