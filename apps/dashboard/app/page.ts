/**
 * The overview: what an org has and whether it is working.
 *
 * Signed in, this used to redirect straight to the machines list, which meant
 * the product's first screen was a table of rows with no state on them and no
 * sense of the shape above them. A pilots org has a shape: services grouped by
 * the app they resolve each other within, sandboxes that are not part of any
 * service, and four ceilings that decide when the next create fails.
 *
 * Every read here degrades on its own. A fleet that cannot answer for quotas
 * should cost the reader the quota bars, not the page.
 */
import { html } from '@webjsdev/core';
import { currentUser } from '#modules/auth/queries/current-user.server.ts';
import { listServicesWithStatus } from '#modules/services/queries/list-services-with-status.server.ts';
import { listVolumes } from '#modules/volumes/queries/list-volumes.server.ts';
import { getQuota } from '#modules/fleet/queries/get-quota.server.ts';
import { isSignedOut } from '#modules/auth/session.server.ts';
import { signInLink } from '#modules/auth/sign-in-link.ts';
import { serviceStatusLine } from '#modules/services/utils/ui/status-line.ts';
import { badgeClass } from '#components/ui/badge.ts';
import { buttonClass } from '#components/ui/button.ts';
import { cardClass } from '#components/ui/card.ts';
import { progressClass } from '#components/ui/progress.ts';
import { dataTable, emptyState, lede, pageHeading, sectionHeading } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';
import type { Machine, Release, Service } from '@pilots/sdk';
import type { Machine as BrowserMachine } from '#modules/machines/types.ts';
import type { Host as BrowserHost, Quota } from '#modules/fleet/types.ts';
import '#components/link-rows.ts';
import '#components/relative-time.ts';
import '#modules/machines/components/machine-list.ts';
import '#modules/usage/components/hosts-strip.ts';

export const metadata = { title: 'pilots' };

interface Bar {
  label: string;
  used: number;
  limit?: number;
  unit?: string;
}

export default async function Home() {
  const me = await currentUser();
  if (!me) {
    return html`
      <div class="max-w-md mx-auto py-24 flex flex-col items-center gap-6 text-center">
        <span class=${badgeClass({ variant: 'outline' })}>Firecracker microVMs</span>
        <h1 class="text-3xl font-semibold tracking-tight m-0">pilots</h1>
        <p class="text-muted-foreground m-0">Sandboxes and production services on one primitive.</p>
        ${signInLink()}
      </div>
    `;
  }

  const status = await listServicesWithStatus();
  const { services, machines, hosts, releases } = isSignedOut(status)
    ? { services: [] as Service[], machines: [] as Machine[], hosts: [], releases: {} as Record<string, Release[]> }
    : status;
  const volumesRead = await listVolumes().catch(() => []);
  const volumes = isSignedOut(volumesRead) ? [] : volumesRead;
  const quotaRead = await getQuota().catch((): Quota => ({}));
  const quota: Quota = isSignedOut(quotaRead) ? {} : quotaRead;

  const replicasOf = (id: string) => machines.filter((m) => m.service_id === id) as BrowserMachine[];
  const sandboxes = machines.filter((m) => !m.service_id) as BrowserMachine[];
  // Every machine the API returns counts against the ceiling: a destroyed one
  // is removed from the list rather than reported in a terminal state.
  const live = machines;

  // Ungrouped last: a service with no app is a service that has not been given
  // a place yet, and sorting it first would bury the ones that have.
  const groups = new Map<string, Service[]>();
  for (const service of services) {
    const key = service.app ?? '';
    groups.set(key, [...(groups.get(key) ?? []), service]);
  }
  const ordered = [...groups.entries()].sort(([a], [b]) => (a === '' ? 1 : b === '' ? -1 : a.localeCompare(b)));

  const bars: Bar[] = [
    { label: 'Machines', used: live.length, limit: quota.max_machines },
    { label: 'vCPUs', used: sum(live, (m) => m.vcpus), limit: quota.max_vcpus },
    { label: 'Memory', used: sum(live, (m) => m.mem_mib), limit: quota.max_mem_mib, unit: 'MiB' },
    { label: 'Volumes', used: sum(volumes, (v) => v.size_gib), limit: quota.max_volume_gib, unit: 'GiB' },
  ];

  return html`
    <div class="flex flex-wrap items-center gap-3">
      ${pageHeading(me.org.name)}
      <a href="/services/new" class=${cn(buttonClass({ size: 'sm' }), 'ml-auto')}>New service</a>
    </div>
    ${lede(
      html`Services and sandboxes in <strong>${me.org.slug}</strong>. A sandbox and a service are the same machine with
      different lifecycle knobs.`,
    )}

    ${sectionHeading('Services')}
    ${services.length === 0
      ? emptyState('No services yet. A service is created by a deploy from a repository with a compose file.', {
          command: 'pilot deploy',
          href: '/services/new',
          label: 'How to deploy',
        })
      : html`<div class="grid gap-4 mb-8">
          ${ordered.map(
            ([app, list]) => html`
              <div class=${cardClass()}>
                <h3 class="m-0 text-base font-medium">
                  ${app || 'Ungrouped'}
                  ${app
                    ? html`<span class="ml-2 font-normal text-xs text-muted-foreground"
                        >services here reach each other at &lt;name&gt;.internal</span
                      >`
                    : ''}
                </h3>
                <link-rows>
                  ${dataTable<Service>({
                    caption: `Services in ${app || 'no app group'}`,
                    rows: list,
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
              </div>
            `,
          )}
        </div>`}

    ${sectionHeading('Sandboxes')}
    <div class="mb-8">
      <machine-list
        .initial=${sandboxes}
        .hosts=${hosts as unknown as BrowserHost[]}
        sandboxes
      ></machine-list>
    </div>

    ${sectionHeading('Quota')}
    <div class="grid gap-3 sm:grid-cols-2 mb-8">
      ${bars.map((bar) => quotaBar(bar))}
    </div>

    ${sectionHeading('Fleet')}
    <hosts-strip .initial=${hosts}></hosts-strip>
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
      <div class="flex items-baseline justify-between text-sm">
        <span>${bar.label}</span>
        <span class="tabular-nums text-muted-foreground">
          ${bar.used}${bar.limit === undefined ? '' : html` / ${bar.limit}`}${bar.unit ? html` ${bar.unit}` : ''}
        </span>
      </div>
      ${bar.limit === undefined
        ? html`<p class="m-0 text-xs text-muted-foreground">No ceiling reported for this org.</p>`
        : html`<progress class=${progressClass()} value=${bar.used} max=${bar.limit} aria-label=${label}></progress>`}
    </div>
  `;
}
