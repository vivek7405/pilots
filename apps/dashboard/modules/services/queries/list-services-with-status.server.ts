'use server';
/**
 * The services, their replicas and their releases, in one read.
 *
 * Three surfaces need exactly this shape (the overview, the services list, and
 * a service's own header), and the reason it is ONE query rather than three
 * awaits at each call site is that a status line assembled from a service read
 * at one moment and machines read at another can say `3/3 healthy` about a
 * release that has since been replaced.
 *
 * The releases go out in parallel: one call per service, which is what the API
 * offers. A service whose releases the engine cannot serve degrades to an
 * empty list rather than taking the page down.
 */
import { listMachines, listServices, listVolumes, fleet } from '#modules/fleet/client.server.ts';
import { requireOrg, signedOut } from '#modules/auth/session.server.ts';
import type { SignedOut } from '#modules/auth/session.server.ts';
import type { Host, Machine, Release, Service, Volume } from '@pilots/sdk';

export interface FleetStatus {
  services: Service[];
  machines: Machine[];
  hosts: Host[];
  /** The org's volumes, so a canvas can hang storage off the service that mounts it. */
  volumes: Volume[];
  /** Keyed by service id. A service with no releases has an empty array. */
  releases: Record<string, Release[]>;
}

export async function listServicesWithStatus(): Promise<FleetStatus | SignedOut> {
  const ctx = await requireOrg();
  if (!ctx) return signedOut();

  const [services, machines, hosts, volumes] = await Promise.all([
    listServices(ctx.org.id).catch(() => [] as Service[]),
    listMachines(ctx.org.id).catch(() => [] as Machine[]),
    fleet.hosts.list().catch(() => [] as Host[]),
    listVolumes(ctx.org.id).catch(() => [] as Volume[]),
  ]);

  const lists = await Promise.all(
    services.map((s) => fleet.services.releases(s.id).catch(() => [] as Release[])),
  );
  const releases: Record<string, Release[]> = {};
  services.forEach((s, i) => {
    releases[s.id] = lists[i] ?? [];
  });

  return { services, machines, hosts, volumes, releases };
}
