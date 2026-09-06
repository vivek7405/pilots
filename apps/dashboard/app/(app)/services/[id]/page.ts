/**
 * One service, full width: the same panel the canvas opens in a slide-over,
 * at an address of its own for a bookmark or a service that belongs to no
 * app. The tab rides `?tab=` exactly as it does on the canvas.
 *
 * The doctor card renders above the panel when the service is not serving,
 * with the deploy action's own words when the failure is that fresh.
 */
import { html, notFound } from '@webjsdev/core';
import type { PageProps } from '@webjsdev/core';
import { orUnauthorized, requireOrg } from '#modules/auth/session.server.ts';
import { getService } from '#modules/services/queries/get-service.server.ts';
import { servicePanel } from '#modules/services/utils/ui/service-panel.ts';
import { tabOf } from '#modules/services/utils/tabs.ts';
import { serviceHealth } from '#modules/services/utils/health.ts';
import { doctorCard } from '#modules/services/utils/ui/doctor-card.ts';
import type { Machine as BrowserMachine } from '#modules/machines/types.ts';

export async function generateMetadata({ params }: PageProps) {
  return { title: `Service ${params.id}` };
}

export default async function ServicePage({ params, searchParams, actionData }: PageProps) {
  await requireOrg();
  const detail = orUnauthorized(await getService({ id: params.id }));
  if (!detail) throw notFound();
  const { service, releases, replicas } = detail;
  const health = serviceHealth(service, replicas as BrowserMachine[], releases);
  const errors = (actionData as { fieldErrors?: Record<string, string>; error?: string } | undefined) ?? {};
  const tab = tabOf(searchParams.tab);
  const instance = typeof searchParams.instance === 'string' ? searchParams.instance : undefined;

  return html`
    ${doctorCard({
      health,
      replicas: replicas as BrowserMachine[],
      serviceName: service.name,
      // The deploy action's own words when the failure is this fresh, rather
      // than a paraphrase of them.
      ...(errors.error ? { symptom: errors.error } : {}),
    })}
    <div class="-mx-5 sm:-mx-6">${servicePanel(detail, tab, { errors, instance })}</div>
  `;
}
