/**
 * Usage for a period, and the two export links.
 *
 * The rows come from `usage_samples`, which the poller fills from every host's
 * own ledger. Reading the table rather than fanning out to the fleet is what
 * makes this page load in one query.
 */
import { html } from '@webjsdev/core';
import type { PageProps } from '@webjsdev/core';
import { orUnauthorized, requireOrg } from '#modules/auth/session.server.ts';
import { usageForOrg } from '#modules/usage/queries/usage-for-org.server.ts';
import type { UsageSample } from '#db/schema.server.ts';
import { toJson } from '#modules/usage/export.server.ts';
import { resolvePeriod } from '#modules/usage/period.ts';
import { buttonClass } from '#components/ui/button.ts';
import { cardClass, cardContentClass } from '#components/ui/card.ts';
import { inputClass } from '#components/ui/input.ts';
import { progressClass } from '#components/ui/progress.ts';
import { dataTable, emptyState, field, formRowClass, lede, pageHeading, sectionHeading } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';
import { listServicesWithStatus } from '#modules/services/queries/list-services-with-status.server.ts';
import { getQuota } from '#modules/fleet/queries/get-quota.server.ts';
import { isSignedOut } from '#modules/auth/session.server.ts';
import { NOUN } from '#lib/vocabulary.ts';
import type { Quota } from '#modules/fleet/types.ts';
import '#modules/usage/components/hosts-strip.ts';

export const metadata = { title: 'Usage' };

interface Bar {
  label: string;
  used: number;
  limit?: number;
  unit?: string;
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

export default async function UsagePage({ searchParams }: PageProps) {
  const ctx = (await requireOrg())!;
  const { since, until } = resolvePeriod(
    typeof searchParams.since === 'string' ? searchParams.since : null,
    typeof searchParams.until === 'string' ? searchParams.until : null,
  );
  const rows = orUnauthorized(await usageForOrg({ since, until }));
  const { totals } = toJson(rows);

  // Limits and capacity read the same fleet the apps list does. Each read
  // degrades on its own: a fleet that cannot answer costs the reader the
  // limits, not the whole page.
  const status = await listServicesWithStatus();
  const fleet = isSignedOut(status)
    ? { machines: [], hosts: [], volumes: [] }
    : { machines: status.machines, hosts: status.hosts, volumes: status.volumes };
  const quotaRead = await getQuota().catch((): Quota => ({}));
  const quota: Quota = isSignedOut(quotaRead) ? {} : quotaRead;
  const bars: Bar[] = [
    { label: NOUN.Instances, used: fleet.machines.length, limit: quota.max_machines },
    { label: 'vCPUs', used: sum(fleet.machines, (m) => m.vcpus), limit: quota.max_vcpus },
    { label: 'Memory', used: sum(fleet.machines, (m) => m.mem_mib), limit: quota.max_mem_mib, unit: 'MiB' },
    { label: NOUN.Storage, used: sum(fleet.volumes, (v) => v.size_gib), limit: quota.max_volume_gib, unit: 'GiB' },
  ];

  const day = (d: Date) => d.toISOString().slice(0, 10);
  const query = `org=${encodeURIComponent(ctx.org.id)}&since=${day(since)}&until=${day(until)}`;
  const number = (value: unknown) => Number(value).toLocaleString('en-US');

  return html`
    ${pageHeading('Usage')}
    ${lede('Metered where your code runs and collected here. This page being down does not stop the metering.')}

    <form method="GET" class=${cn(formRowClass(), 'mb-6')}>
      ${field({
        id: 'since',
        label: 'From',
        control: html`<input id="since" name="since" type="date" value=${day(since)} class=${inputClass()}>`,
      })}
      ${field({
        id: 'until',
        label: 'To',
        control: html`<input id="until" name="until" type="date" value=${day(until)} class=${inputClass()}>`,
      })}
      <button type="submit" class=${buttonClass()}>Apply</button>
      <a href=${`/api/usage?${query}&format=csv`} class=${cn(buttonClass({ variant: 'outline' }), 'no-underline')}
        >Download CSV</a
      >
      <a href=${`/api/usage?${query}`} class=${cn(buttonClass({ variant: 'ghost' }), 'no-underline')}>JSON</a>
    </form>

    <dl class="grid grid-cols-2 sm:grid-cols-4 gap-4 mb-8 m-0">
      ${[
        ['Machine seconds', totals.machine_seconds],
        ['vCPU seconds', totals.vcpu_seconds],
        ['MiB seconds', totals.mib_seconds],
        ['Volume GiB seconds', totals.volume_gib_seconds],
      ].map(
        ([label, value]) => html`
          <div class=${cardClass({ size: 'sm' })} data-slot="card" data-size="sm">
            <div class=${cardContentClass()}>
              <dt class="text-meta text-muted-foreground m-0">${label}</dt>
              <dd class="m-0 text-heading font-medium tabular-nums">${number(value)}</dd>
            </div>
          </div>
        `,
      )}
    </dl>

    ${sectionHeading('Usage over time', 'What this team ran, hour by hour, as pilots metered it for billing.')}
    <!-- One wrapper around BOTH branches, so the gap before the Fleet heading
         does not disappear when the period is empty. -->
    <div class="mb-8">
      ${rows.length === 0
        ? emptyState('Nothing recorded for this period. Usage is metered by the hour, so something started minutes ago has not been counted yet.')
        : dataTable<UsageSample>({
              caption: 'Usage samples for the selected period',
              rows,
              columns: [
                { header: 'Host', cellClass: 'font-mono', cell: (r) => r.hostId },
                {
                  header: 'Window',
                  cellClass: 'text-muted-foreground tabular-nums',
                  cell: (r) => r.windowStart.toISOString().slice(0, 16).replace('T', ' '),
                },
                { header: 'Machine s', align: 'right', cellClass: 'tabular-nums', cell: (r) => r.machineSeconds },
                { header: 'vCPU s', align: 'right', cellClass: 'tabular-nums', cell: (r) => r.vcpuSeconds },
                { header: 'MiB s', align: 'right', cellClass: 'tabular-nums', cell: (r) => r.mibSeconds },
                { header: 'Volume GiB s', align: 'right', cellClass: 'tabular-nums', cell: (r) => r.volumeGibSeconds },
              ],
            })}
    </div>

    <div class="mb-8">
      ${sectionHeading('Limits', 'What this team may run at once. A create that would cross a line is refused.')}
      <div class="grid gap-3 sm:grid-cols-2">${bars.map((bar) => quotaBar(bar))}</div>
    </div>

    <div>
      ${sectionHeading('Capacity', 'Where your services run right now, and how much room is left there.')}
      <hosts-strip .initial=${fleet.hosts}></hosts-strip>
    </div>
  `;
}
