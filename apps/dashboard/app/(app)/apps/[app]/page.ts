/**
 * One app's canvas: its services as cards, an arrow from each service up to
 * what it dials, and storage hanging off the service that mounts it. The
 * second step of the journey, and the picture the product was missing.
 *
 * The picture is drawn by the server from a pure layout, so it exists at
 * first paint and with scripting off; `<app-canvas>` only scales it to fit
 * and moves focus between cards on the arrow keys. Clicking a card puts the
 * service in the URL and the page renders its panel in a slide-over with the
 * canvas still behind it, so a reload, a bookmark and a no-script visitor
 * all see the same thing.
 *
 * The URL contract: `/apps/<app>?service=<id>&tab=<deployments|terminal|settings>`.
 */
import { html, notFound } from '@webjsdev/core';
import type { PageProps } from '@webjsdev/core';
import { orUnauthorized, requireOrg } from '#modules/auth/session.server.ts';
import { listServicesWithStatus } from '#modules/services/queries/list-services-with-status.server.ts';
import { getService } from '#modules/services/queries/get-service.server.ts';
import { servicePanel } from '#modules/services/utils/ui/service-panel.ts';
import { tabOf } from '#modules/services/utils/tabs.ts';
import { layoutApp } from '#modules/apps/utils/layout.ts';
import { edgesSvg } from '#modules/apps/utils/ui/canvas-svg.ts';
import { serviceCard } from '#modules/apps/utils/ui/service-card.ts';
import { stageClass } from '#modules/apps/utils/ui/stage.ts';
import { buttonClass } from '#components/ui/button.ts';
import { lede, pageHeading } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';
import type { Machine as BrowserMachine } from '#modules/machines/types.ts';
import '#modules/apps/components/app-canvas.ts';
import '#components/slide-over.ts';

export async function generateMetadata({ params }: PageProps) {
  return { title: params.app };
}

export default async function AppPage({ params, searchParams, actionData }: PageProps) {
  await requireOrg();
  const app = params.app;
  const { services, machines, volumes } = orUnauthorized(await listServicesWithStatus());
  const mine = services.filter((s) => s.app === app);
  if (mine.length === 0) throw notFound();

  const layout = layoutApp(mine.map((s) => ({ id: s.id, name: s.name, dependsOn: s.depends_on ?? [] })));
  const byId = new Map(mine.map((s) => [s.id, s] as const));
  const volumeOf = (id: string | undefined) => (id ? volumes.find((v) => v.id === id) : undefined);

  // A `?service=` outside this app is ignored rather than rendered: the
  // panel belongs to the canvas it sits on.
  const selectedId = String(searchParams.service ?? '');
  const selected = byId.has(selectedId) ? selectedId : '';
  const tab = tabOf(searchParams.tab);
  const detail = selected ? orUnauthorized(await getService({ id: selected })) : null;
  const errors = (actionData as { fieldErrors?: Record<string, string>; error?: string } | undefined) ?? {};
  const instance = typeof searchParams.instance === 'string' ? searchParams.instance : undefined;

  return html`
    <div class="flex flex-wrap items-center gap-3">
      ${pageHeading(app)}
      <a href=${`/services/new?app=${encodeURIComponent(app)}`} class=${cn(buttonClass({ size: 'sm' }), 'ml-auto')}
        >Add a service</a
      >
    </div>
    ${lede(
      html`Services in this app reach each other at <code class="font-mono">&lt;name&gt;.internal</code>. An arrow
      points at the service the other one dials.`,
    )}

    <app-canvas class="block">
      <div
        data-canvas-stage
        data-width=${String(layout.width)}
        data-height=${String(layout.height)}
        class=${cn(stageClass(), 'relative grid gap-4 p-4 sm:block sm:h-[var(--stage-h)] sm:w-[var(--stage-w)] sm:p-0')}
        style=${`--stage-w:${layout.width}px;--stage-h:${layout.height}px`}
      >
        <div class="pointer-events-none absolute inset-0 hidden sm:block" aria-hidden="true">${edgesSvg(layout)}</div>
        ${layout.placed.map((placed) => {
          const service = byId.get(placed.id)!;
          return serviceCard({
            app,
            service,
            placed,
            replicas: machines.filter((m) => m.service_id === service.id) as BrowserMachine[],
            volume: volumeOf(service.volume_id),
            selected: service.id === selected,
          });
        })}
      </div>
    </app-canvas>

    ${detail
      ? html`<slide-over back=${`/apps/${encodeURIComponent(app)}`}>
          ${servicePanel(detail, tab, { app, errors, instance })}
        </slide-over>`
      : ''}
  `;
}
