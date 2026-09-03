'use server';
/**
 * The org's custom domains, with the service each points at.
 *
 * `domains.list()` is fleet-wide because this app's key is an admin key, so it
 * is narrowed here against the org's own services rather than trusted as it
 * arrives.
 */
import { fleet, listServices } from '#modules/fleet/client.server.ts';
import type { DomainResponse, Service } from '@pilots/sdk';

export interface DomainRow {
  domain: DomainResponse;
  service: Service | undefined;
}

export async function listDomains(orgId: string): Promise<DomainRow[]> {
  const services = await listServices(orgId);
  const mine = new Map(services.map((s) => [s.id, s]));
  const domains = await fleet.domains.list().catch(() => [] as DomainResponse[]);
  return domains.filter((d) => mine.has(d.service_id)).map((d) => ({ domain: d, service: mine.get(d.service_id) }));
}
