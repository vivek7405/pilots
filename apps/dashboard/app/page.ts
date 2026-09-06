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
 * A service with no app is listed below the grid under its own name. It is
 * not hidden, because it exists and costs money, and it is not promoted to a
 * card, because a card is a canvas and a canvas needs an app.
 *
 * Every read here degrades on its own. A fleet that cannot answer for quotas
 * should cost the reader the limits, not the page.
 */
import { html } from '@webjsdev/core';
import type { PageProps } from '@webjsdev/core';
import { currentUser } from '#modules/auth/queries/current-user.server.ts';
import { listServicesWithStatus } from '#modules/services/queries/list-services-with-status.server.ts';
import { getQuota } from '#modules/fleet/queries/get-quota.server.ts';
import { isSignedOut } from '#modules/auth/session.server.ts';
import { signInLink } from '#modules/auth/sign-in-link.ts';
import { serviceStatusLine } from '#modules/services/utils/ui/status-line.ts';
import { appTone, groupApps, sortApps, sortKey } from '#modules/apps/utils/apps.ts';
import type { AppGroup } from '#modules/apps/utils/apps.ts';
import { layoutApp } from '#modules/apps/utils/layout.ts';
import { thumbnailSvg } from '#modules/apps/utils/ui/canvas-svg.ts';
import { stageClass } from '#modules/apps/utils/ui/stage.ts';
import { toneDot } from '#modules/machines/utils/ui/state.ts';
import { NOUN } from '#lib/vocabulary.ts';
import { badgeClass } from '#components/ui/badge.ts';
import { buttonClass } from '#components/ui/button.ts';
import { cardClass } from '#components/ui/card.ts';
import { progressClass } from '#components/ui/progress.ts';
import { nativeSelectClass, nativeSelectIconClass, nativeSelectWrapperClass } from '#components/ui/native-select.ts';
import { cardBody, dataTable, emptyState, lede, pageHeading, sectionGap, sectionHeading } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';
import type { Machine, Release, Service, Volume } from '@pilots/sdk';
import type { Machine as BrowserMachine } from '#modules/machines/types.ts';
import type { Quota } from '#modules/fleet/types.ts';
import '#components/auto-submit.ts';
import '#components/copy-button.ts';
import '#components/link-rows.ts';
import '#components/list-filter.ts';
import '#components/relative-time.ts';
import '#modules/usage/components/hosts-strip.ts';

export const metadata = { title: 'pilots' };

interface Bar {
  label: string;
  used: number;
  limit?: number;
  unit?: string;
}

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
  const { services, machines, hosts, releases, volumes } = isSignedOut(status)
    ? {
        services: [] as Service[],
        machines: [] as Machine[],
        hosts: [],
        releases: {} as Record<string, Release[]>,
        volumes: [] as Volume[],
      }
    : status;
  const quotaRead = await getQuota().catch((): Quota => ({}));
  const quota: Quota = isSignedOut(quotaRead) ? {} : quotaRead;

  const sort = sortKey(searchParams.sort);
  const view = searchParams.view === 'list' ? 'list' : 'grid';
  const grouped = groupApps(services, machines as BrowserMachine[], releases);
  const apps = sortApps(grouped.apps, sort);
  const loose = grouped.loose;
  const replicasOf = (id: string) => machines.filter((m) => m.service_id === id) as BrowserMachine[];

  // Every machine the API returns counts against the ceiling: a destroyed one
  // is removed from the list rather than reported in a terminal state.
  const bars: Bar[] = [
    { label: NOUN.Instances, used: machines.length, limit: quota.max_machines },
    { label: 'vCPUs', used: sum(machines, (m) => m.vcpus), limit: quota.max_vcpus },
    { label: 'Memory', used: sum(machines, (m) => m.mem_mib), limit: quota.max_mem_mib, unit: 'MiB' },
    { label: NOUN.Storage, used: sum(volumes, (v) => v.size_gib), limit: quota.max_volume_gib, unit: 'GiB' },
  ];

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
          ? emptyState(
              'No apps yet. Deploy a repository and its services appear here as one app you can open, watch and change.',
              { command: 'pilot deploy', href: '/services/new', label: 'Deploy an app' },
            )
          : html`<div id="apps">
              ${view === 'grid'
                ? html`<link-rows>
                    <div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">${apps.map((app) => appCard(app))}</div>
                  </link-rows>`
                : html`<link-rows>
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
                        { header: NOUN.Services, cell: (app) => onlineLine(app) },
                        { header: 'Deployed', cell: (app) => deployedAt(app) },
                      ],
                    })}
                  </link-rows>`}
            </div>`}
      </section>

      ${loose.length > 0
        ? html`<section>
            ${sectionHeading(
              'Not in an app',
              'A service outside an app cannot be reached by name from other services. An app is set when the service is created, by the compose file or the name given at deploy.',
            )}
            <link-rows>
              ${dataTable<Service>({
                caption: 'Services that belong to no app',
                rows: loose,
                rowHref: (s) => `/services/${s.id}`,
                columns: [
                  {
                    header: 'Name',
                    cell: (s) => html`<a href=${`/services/${s.id}`} class="text-foreground">${s.name}</a>`,
                  },
                  {
                    header: 'Status',
                    cell: (s) => serviceStatusLine(s, replicasOf(s.id), releases[s.id] ?? []),
                  },
                  {
                    header: 'URL',
                    cell: (s) => (s.url ? html`<a href=${s.url} rel="noopener">${s.url}</a>` : ''),
                  },
                ],
              })}
            </link-rows>
          </section>`
        : ''}

      <section>
        ${sectionHeading('Limits', 'What this team may run at once. A create that would cross a line is refused.')}
        <div class="grid gap-3 sm:grid-cols-2">${bars.map((bar) => quotaBar(bar))}</div>
      </section>

      <section>
        ${sectionHeading('Capacity', 'Where your services run right now, and how much room is left there.')}
        <hosts-strip .initial=${hosts}></hosts-strip>
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

function onlineLine(app: AppGroup<Service>) {
  const total = app.services.length;
  return html`<span class="inline-flex items-center gap-1.5 whitespace-nowrap">
    ${toneDot(appTone(app))} ${app.online}/${total} ${total === 1 ? 'service' : 'services'} online
  </span>`;
}

function deployedAt(app: AppGroup<Service>) {
  return app.lastDeploy === undefined
    ? html`<span class="text-muted-foreground">never</span>`
    : html`<relative-time datetime=${String(app.lastDeploy)}></relative-time>`;
}

/**
 * One app. The name is the link and the whole card navigates through
 * `<link-rows>`, so a middle click and a copied address both still work and
 * a screen reader gets one link named after the app rather than the whole
 * card's text.
 */
function appCard(app: AppGroup<Service>) {
  const href = `/apps/${encodeURIComponent(app.name)}`;
  const layout = layoutApp(app.services.map((s) => ({ id: s.id, name: s.name, dependsOn: s.depends_on ?? [] })));
  return html`
    <div class=${cn(cardClass(), 'gap-0 py-0 cursor-pointer transition-colors hover:border-border-strong')} data-filter-row data-href=${href}>
      <div class=${cardBody()}>
        <h2 class="m-0 text-body font-semibold"><a href=${href} class="text-foreground no-underline">${app.name}</a></h2>
        <div class=${cn(stageClass(), 'mt-3 py-4')}>${thumbnailSvg(layout)}</div>
        <p class="m-0 mt-3 flex flex-wrap items-center gap-x-2 text-meta text-muted-foreground">
          ${onlineLine(app)}
          ${app.lastDeploy === undefined
            ? ''
            : html`<span aria-hidden="true">·</span
              ><span class="whitespace-nowrap">deployed ${deployedAt(app)}</span>`}
        </p>
      </div>
    </div>
  `;
}

function sum<T>(rows: T[], of: (row: T) => number | undefined): number {
  return rows.reduce((total, row) => total + (of(row) ?? 0), 0);
}

/**
 * One ceiling. The number is beside the bar rather than only in it, because a
 * bar is a proportion and what a reader needs before a create is the count.
 */
function quotaBar(bar: Bar) {
  const label = `${bar.label}: ${bar.used}${bar.limit === undefined ? '' : ` of ${bar.limit}`}${bar.unit ? ` ${bar.unit}` : ''}`;
  return html`
    <div class="grid gap-1">
      <div class="flex items-baseline justify-between text-body">
        <span>${bar.label}</span>
        <span class="tabular-nums text-muted-foreground">
          ${bar.used}${bar.limit === undefined ? '' : html` / ${bar.limit}`}${bar.unit ? html` ${bar.unit}` : ''}
        </span>
      </div>
      ${bar.limit === undefined
        ? html`<p class="m-0 text-meta text-muted-foreground">No ceiling reported for this team.</p>`
        : html`<progress class=${progressClass()} value=${bar.used} max=${bar.limit} aria-label=${label}></progress>`}
    </div>
  `;
}
