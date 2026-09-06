'use server';
/**
 * Everything the service detail page renders, in one read.
 *
 * Releases and previews come back alongside the service rather than as three
 * separate awaits in the page, so a page that renders them together cannot get
 * a service from one moment and its releases from another.
 *
 * A release route the engine does not serve yet answers 404, so `releases`
 * degrades to an empty list instead of taking the page down with it.
 *
 * The org the service is checked against is the SESSION's, so a caller cannot
 * pass the org id that would make the tenancy check pass.
 */
import { eq } from 'drizzle-orm';
import { db } from '#db/connection.server.ts';
import { repoConnections } from '#db/schema.server.ts';
import { fleet, listMachines } from '#modules/fleet/client.server.ts';
import { assertOwned } from '#modules/fleet/org-filter.server.ts';
import { requireOrg, signedOut } from '#modules/auth/session.server.ts';
import type { SignedOut } from '#modules/auth/session.server.ts';
import { githubAppConfigured } from '#modules/github/app-jwt.server.ts';
import { installUrl } from '#modules/github/installations.server.ts';
import type { RepoConnection } from '#db/schema.server.ts';
import type { DomainResponse, Host, Machine, Release, Service } from '@pilots/sdk';

export interface ServiceDetail {
  service: Service;
  releases: Release[];
  previews: Machine[];
  repo: RepoConnection | null;
  /** The machines running this service, which is what its health is about. */
  replicas: Machine[];
  /** Every host, so a replica's resume tier can be named without a second read. */
  hosts: Host[];
  /** The custom domains pointing at this service, for its Settings tab. */
  domains: DomainResponse[];
  /**
   * Whether this fleet has a GitHub App, and where to install it. Read here
   * because the panel that renders it is a pure fragment and may not touch
   * the environment itself.
   */
  github: { configured: boolean; installUrl: string };
}

export async function getService(input: { id: string }): Promise<ServiceDetail | null | SignedOut> {
  const ctx = await requireOrg();
  if (!ctx) return signedOut();
  let service: Service;
  try {
    service = await fleet.services.get(input.id);
  } catch {
    return null;
  }
  if (!assertOwned(ctx.org.id, service)) return null;

  const releases = await fleet.services.releases(input.id).catch(() => [] as Release[]);
  const machines = await listMachines(ctx.org.id).catch(() => [] as Machine[]);
  // A PR preview is named `pr-<number>-<app>`, which is the engine's own
  // convention; there is no separate previews route to ask.
  const app = service.app ?? service.name;
  const previews = machines.filter((m) => (m.name ?? '').startsWith('pr-') && (m.name ?? '').endsWith(`-${app}`));
  const replicas = machines.filter((m) => m.service_id === service.id);
  const hosts = await fleet.hosts.list().catch(() => [] as Host[]);
  const repo = (await db.select().from(repoConnections).where(eq(repoConnections.serviceId, input.id)).get()) ?? null;
  const domains = (await fleet.domains.list().catch(() => [] as DomainResponse[])).filter(
    (d) => d.service_id === service.id,
  );

  return {
    service,
    releases,
    previews,
    repo,
    replicas,
    hosts,
    domains,
    github: { configured: githubAppConfigured(), installUrl: installUrl() },
  };
}
