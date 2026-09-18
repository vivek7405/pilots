/**
 * The list of your apps: the first of the three steps a person takes
 * through this product (an app, its canvas, one service on it).
 *
 * One card per app, carrying its name, how many of its services are online,
 * when it last deployed, and a thumbnail of its own canvas in the positions
 * the canvas draws, so the list and the canvas read as one object at two
 * zoom levels. Search, sort and the grid or list toggle all work with
 * scripting off: the filter is an island over server-rendered rows, the sort
 * is a GET form, the toggle is two links.
 *
 * A service with no app is a card too, drawn as its own one-node canvas and
 * linking to the service, so this page is one grid of cards rather than a
 * grid and a leftover table. Limits and capacity live on Usage now, not here:
 * this page is the apps and nothing else.
 */
import { html } from '@webjsdev/core';
import type { PageProps } from '@webjsdev/core';
import { currentUser } from '#modules/auth/queries/current-user.server.ts';
import { listServicesWithStatus } from '#modules/services/queries/list-services-with-status.server.ts';
import { isSignedOut } from '#modules/auth/session.server.ts';
import { signInLink } from '#modules/auth/sign-in-link.ts';
import { serviceStatusLine } from '#modules/services/utils/ui/status-line.ts';
import type { HealthRelease } from '#modules/services/utils/health.ts';
import { groupApps, sortApps, sortKey } from '#modules/apps/utils/apps.ts';
import { onlineLine } from '#modules/apps/utils/ui/online-line.ts';
import type { AppGroup } from '#modules/apps/utils/apps.ts';
import { layoutApp } from '#modules/apps/utils/layout.ts';
import { thumbnailSvg } from '#modules/apps/utils/ui/canvas-svg.ts';
import { stageClass } from '#modules/apps/utils/ui/stage.ts';
import { NOUN } from '#lib/vocabulary.ts';
import { badgeClass } from '#components/ui/badge.ts';
import { buttonClass } from '#components/ui/button.ts';
import { cardClass } from '#components/ui/card.ts';
import { nativeSelectClass, nativeSelectIconClass, nativeSelectWrapperClass } from '#components/ui/native-select.ts';
import { cardBody, dataTable, lede, pageHeading, sectionEmpty, sectionGap, sectionHeading } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';
import type { Machine, Release, Service } from '@pilots/sdk';
import type { Machine as BrowserMachine } from '#modules/machines/types.ts';
import '#components/auto-submit.ts';
import '#components/copy-button.ts';
import '#components/link-rows.ts';
import '#components/list-filter.ts';
import '#components/relative-time.ts';
import '#modules/apps/components/live-status.ts';

export const metadata = { title: 'pilots' };


const SORTS: { value: 'activity' | 'created' | 'name'; label: string }[] = [
  { value: 'activity', label: 'Recent activity' },
  { value: 'created', label: 'Newest first' },
  { value: 'name', label: 'Name' },
];

export default async function Home({ searchParams }: PageProps) {
  const me = await currentUser();
  if (!me) {
    return html`
      <div class="max-w-md mx-auto py-24 flex flex-col items-center gap-6 text-center">
        <span class=${badgeClass({ variant: 'outline' })}>Firecracker microVMs</span>
        <h1 class="text-title font-semibold tracking-tight m-0">pilots</h1>
        <p class="text-muted-foreground m-0">Sandboxes and production services on one primitive.</p>
        ${signInLink()}
      </div>
    `;
  }

  const status = await listServicesWithStatus();
  const { services, machines, releases } = isSignedOut(status)
    ? {
        services: [] as Service[],
        machines: [] as Machine[],
        releases: {} as Record<string, Release[]>,
      }
    : status;
  const sort = sortKey(searchParams.sort);
  const view = searchParams.view === 'list' ? 'list' : 'grid';
  const grouped = groupApps(services, machines as BrowserMachine[], releases);
  const apps = sortApps(grouped.apps, sort);
  const loose = grouped.loose;
  const replicasOf = (id: string) => machines.filter((m) => m.service_id === id) as BrowserMachine[];


  const viewHref = (v: 'grid' | 'list') => `/?sort=${sort}&view=${v}`;

  return html`
    <div class="flex flex-wrap items-center gap-3">
      ${pageHeading(NOUN.Apps)}
      <a href="/services/new" class=${cn(buttonClass({ size: 'sm' }), 'ml-auto')}>New app</a>
    </div>
    ${lede(
      html`An app is a group of services that reach each other by name. Open one to see what talks to what, in
      <strong>${me.org.slug}</strong>.`,
    )}

    <div class=${sectionGap()}>
      <section>
        <div class="mb-4 flex flex-wrap items-center gap-x-4 gap-y-3">
          <list-filter for="apps" placeholder="Search apps"></list-filter>
          <span class="text-meta text-muted-foreground">${apps.length} ${apps.length === 1 ? 'app' : 'apps'}</span>
          <auto-submit class="contents">
            <form method="get" action="/" class="flex items-center gap-2">
              <input type="hidden" name="view" value=${view}>
              <label for="sort" class="text-meta text-muted-foreground">Sort by</label>
              <div class=${nativeSelectWrapperClass()}>
                <select id="sort" name="sort" data-size="sm" class=${nativeSelectClass()}>
                  ${SORTS.map((s) => html`<option value=${s.value} ?selected=${s.value === sort}>${s.label}</option>`)}
                </select>
                <svg class=${nativeSelectIconClass()} viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m6 9 6 6 6-6" /></svg>
              </div>
              <button type="submit" data-auto-submit-button class=${buttonClass({ variant: 'outline', size: 'sm' })}>Apply</button>
            </form>
          </auto-submit>
          <nav aria-label="View" class="ml-auto flex items-center gap-1">
            ${viewLink('grid', 'Grid', view, viewHref('grid'))} ${viewLink('list', 'List', view, viewHref('list'))}
          </nav>
        </div>

        ${apps.length === 0 && loose.length === 0
          ? sectionEmpty('No apps yet', {
              text: 'Deploy a repository, then it appears here as an app you can open, watch and change.',
              href: '/services/new',
            })
          : view === 'grid'
            ? html`<link-rows>
                <div id="apps" class="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
                  ${apps.map((app) => appCard(app, releases))} ${loose.map((s) => looseCard(s, replicasOf(s.id), releases[s.id] ?? []))}
                </div>
              </link-rows>`
            : html`<div id="apps"><link-rows>
                ${dataTable<AppGroup<Service>>({
                  caption: 'Your apps',
                  rows: apps,
                  rowHref: (app) => `/apps/${encodeURIComponent(app.name)}`,
                  columns: [
                    {
                      header: NOUN.App,
                      cell: (app) =>
                        html`<a href=${`/apps/${encodeURIComponent(app.name)}`} class="text-foreground font-medium"
                          >${app.name}</a
                        >`,
                    },
                    { header: NOUN.Services, cell: (app) => liveApp(app, releases) },
                    { header: 'Deployed', cell: (app) => deployedAt(app) },
                  ],
                })}
              </link-rows></div>`}
      </section>
    </div>
  `;
}

function viewLink(value: 'grid' | 'list', label: string, current: string, href: string) {
  const on = value === current;
  return html`<a
    href=${href}
    aria-current=${on ? 'true' : 'false'}
    class=${cn(buttonClass({ variant: on ? 'secondary' : 'ghost', size: 'sm' }), 'no-underline')}
    >${label}</a
  >`;
}

function deployedAt(app: AppGroup<Service>) {
  return app.lastDeploy === undefined
    ? html`<span class="text-muted-foreground">never</span>`
    : html`<relative-time datetime=${String(app.lastDeploy)}></relative-time>`;
}

/**
 * The same line the server just computed, wrapped in the element that keeps it
 * current. The slot's content is what a browser with no scripting shows, and
 * what every browser shows until the feed's first message.
 *
 * Only the services and their releases are handed over: the machines arrive on
 * the socket, once, for the whole page.
 */
function liveApp(app: AppGroup<Service>, releases: Record<string, HealthRelease[]>) {
  return html`<live-status
    kind="app"
    .services=${app.services}
    .releases=${Object.fromEntries(app.services.map((s) => [s.id, releases[s.id] ?? []]))}
    >${onlineLine(app)}</live-status
  >`;
}

function liveService(service: Service, replicas: BrowserMachine[], rels: HealthRelease[]) {
  return html`<live-status kind="service" .services=${[service]} .releases=${{ [service.id]: rels }}
    >${serviceStatusLine(service, replicas, rels)}</live-status
  >`;
}

/**
 * One app. The name is the link and the whole card navigates through
 * `<link-rows>`, so a middle click and a copied address both still work and
 * a screen reader gets one link named after the app rather than the whole
 * card's text.
 */
function appCard(app: AppGroup<Service>, releases: Record<string, HealthRelease[]>) {
  const href = `/apps/${encodeURIComponent(app.name)}`;
  const layout = layoutApp(app.services.map((s) => ({ id: s.id, name: s.name, dependsOn: s.depends_on ?? [] })));
  return html`
    <div class=${cn(cardClass(), 'gap-0 py-0 cursor-pointer transition-colors hover:border-border-strong')} data-filter-row data-href=${href}>
      <div class=${cardBody()}>
        <h2 class="m-0 text-body font-semibold"><a href=${href} class="text-foreground no-underline">${app.name}</a></h2>
        <div class=${cn(stageClass(), 'mt-3 py-4')}>${thumbnailSvg(layout)}</div>
        <p class="m-0 mt-3 flex flex-wrap items-center gap-x-2 text-meta text-muted-foreground">
          ${liveApp(app, releases)}
          ${app.lastDeploy === undefined
            ? ''
            : html`<span aria-hidden="true">·</span
              ><span class="whitespace-nowrap">deployed ${deployedAt(app)}</span>`}
        </p>
      </div>
    </div>
  `;
}

/**
 * A service that belongs to no app is still a card, so the apps page is one
 * grid of cards rather than a grid and a leftover table. It draws its own
 * one-node canvas and links to the service, since there is no app to open.
 */
function looseCard(service: Service, replicas: BrowserMachine[], rels: HealthRelease[]) {
  const href = `/services/${service.id}`;
  const layout = layoutApp([{ id: service.id, name: service.name, dependsOn: [] }]);
  return html`
    <div class=${cn(cardClass(), 'gap-0 py-0 cursor-pointer transition-colors hover:border-border-strong')} data-filter-row data-href=${href}>
      <div class=${cardBody()}>
        <h2 class="m-0 text-body font-semibold"><a href=${href} class="text-foreground no-underline">${service.name}</a></h2>
        <div class=${cn(stageClass(), 'mt-3 py-4')}>${thumbnailSvg(layout)}</div>
        <p class="m-0 mt-3 text-meta text-muted-foreground">${liveService(service, replicas, rels)}</p>
      </div>
    </div>
  `;
}

